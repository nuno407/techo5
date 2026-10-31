package skyview

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/HuskerMinion/techo5/echod/internal/component"
	"github.com/HuskerMinion/techo5/echod/internal/feature/home"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hook"
)

func init() {
	component.Register(component.Network, Get(), component.Order(91))
}

const (
	// activeFor is how long after the board was last on the screen the aircraft and the radar are still
	// fetched: a glance at the clock and back finds them current.
	activeFor = time.Minute
	homeRetry = 30 * time.Second
	// fr24Rest is how long a Flightradar24 token that was refused is left before it is tried again.
	fr24Rest = 30 * time.Minute
	// changedEvery coalesces the tiles' arrivals into one redraw.
	changedEvery = 120 * time.Millisecond
)

// ground is drawn where no tile is yet: a dark map.
var ground = color.RGBA{0x1a, 0x1c, 0x20, 0xff}

// Feature is the Sky board's data, and the map's place and zoom.
type Feature struct {
	// Changed fires when there is something new to draw. Listeners must not block.
	Changed hook.Hook[struct{}]

	tiles *tileStore

	mu     sync.Mutex
	looked time.Time
	poke   chan struct{}
	emit   *time.Timer

	// The map: its middle in world pixels at zoom, the size it is drawn at, and whether it has been put
	// on home yet.
	zoom    int
	cx, cy  float64
	w, h    int
	placed  bool
	homeLat float64
	homeLon float64
	homeOK  bool
	homeErr string
	homeAt  time.Time // asked last

	planes     map[string]Aircraft
	source     string // the name of where the aircraft shown came from, for the credit
	sourceNote string
	planesDue  time.Time
	planesBusy bool
	fr24Off    time.Time // a refused token is left alone until then

	radar     radarFrame
	radarNote string
	radarDue  time.Time

	wind     *Wind
	windNote string
	windDue  time.Time

	// The map and the rain composed for the view last drawn, and what it showed.
	composed *image.RGBA
	key      string
	gen      uint64 // bumped when a tile arrives
}

var (
	once   sync.Once
	shared *Feature
)

// Get is the Sky board's data.
func Get() *Feature {
	once.Do(func() {
		f := &Feature{poke: make(chan struct{}, 1), zoom: defaultZoom}
		f.tiles = newTileStore(filepath.Join(settingsDir, "tiles"), f.tileArrived)
		shared = f
	})
	return shared
}

func (f *Feature) Name() string { return "skyview" }

// Looked is the board on the screen, w by h: the fetching runs while it is, and a minute after.
func (f *Feature) Looked(w, h int) {
	f.mu.Lock()
	was := f.looked
	f.looked, f.w, f.h = time.Now(), w, h
	f.mu.Unlock()
	if time.Since(was) > activeFor {
		f.wake()
	}
}

func (f *Feature) wake() {
	select {
	case f.poke <- struct{}{}:
	default:
	}
}

func (f *Feature) active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Since(f.looked) < activeFor
}

// Run keeps the tiles' workers, and fetches what is due while the board is up.
func (f *Feature) Run(ctx context.Context) error {
	go f.tiles.prune()
	go f.tiles.run(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if f.active() {
			f.tick(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-f.poke:
		}
	}
}

// tick fetches whatever is due: home's place until it is known, the aircraft, the radar's frame and
// Windy's forecast, each in the background so a slow one does not hold the others.
func (f *Feature) tick(ctx context.Context) {
	s, _ := current()
	now := time.Now()
	f.mu.Lock()
	needHome := !f.homeOK && now.Sub(f.homeAt) > homeRetry
	if needHome {
		f.homeAt = now
	}
	f.mu.Unlock()
	if needHome || (s.Lat != 0 || s.Lon != 0) {
		f.findHome(s)
	}

	f.mu.Lock()
	lat, lon := f.centerLatLonLocked()
	planesDue := f.homeOK && !f.planesBusy && !now.Before(f.planesDue) && s.source() != SourceOff
	if planesDue {
		f.planesBusy = true
	}
	radarDue := s.radarOn() && !now.Before(f.radarDue)
	if radarDue {
		f.radarDue = now.Add(radarEvery)
	}
	windDue := f.homeOK && s.WindyKey != "" && !now.Before(f.windDue)
	if windDue {
		f.windDue = now.Add(windyEvery)
	}
	homeLat, homeLon := f.homeLat, f.homeLon
	radius := f.radiusLocked(s)
	f.mu.Unlock()

	if s.source() == SourceOff {
		f.mu.Lock()
		f.planes, f.source, f.sourceNote = nil, "", ""
		f.mu.Unlock()
	}
	if s.WindyKey == "" {
		f.mu.Lock()
		f.wind, f.windNote = nil, ""
		f.mu.Unlock()
	}
	if planesDue {
		go f.fetchPlanes(ctx, s, lat, lon, radius)
	}
	if radarDue {
		go func() {
			fr, err := fetchRadar(ctx)
			f.mu.Lock()
			if err != nil {
				if f.radarNote == "" {
					slog.Info("sky: radar", "err", err)
				}
				f.radarNote = "Radar: " + err.Error()
				f.radarDue = time.Now().Add(time.Minute)
			} else {
				f.radarNote = ""
				if fr.layer != f.radar.layer {
					f.radar = fr
				}
			}
			keep := f.radar.layer
			f.mu.Unlock()
			if keep != "" {
				f.tiles.forgetRadar(keep)
			}
			f.changed()
		}()
	}
	if windDue {
		go func() {
			w, err := fetchWindy(ctx, s.WindyKey, homeLat, homeLon, time.Now())
			f.mu.Lock()
			if err != nil {
				if f.windNote == "" {
					slog.Info("sky: windy", "err", err)
				}
				f.windNote = err.Error()
				f.windDue = time.Now().Add(5 * time.Minute)
			} else {
				f.wind, f.windNote = &w, ""
			}
			f.mu.Unlock()
			f.changed()
		}()
	}
}

// findHome is where the map opens: sky.json's place, or home's.
func (f *Feature) findHome(s Settings) {
	lat, lon, err := s.Lat, s.Lon, error(nil)
	if lat == 0 && lon == 0 {
		lat, lon, err = home.Location()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.homeErr = err.Error()
		return
	}
	moved := f.homeOK && (lat != f.homeLat || lon != f.homeLon)
	f.homeLat, f.homeLon, f.homeOK, f.homeErr = lat, lon, true, ""
	if !f.placed || moved {
		f.zoom = s.zoom()
		f.cx, f.cy = worldPixel(lat, lon, f.zoom)
		f.placed = true
		f.planesDue = time.Time{}
	}
}

// fetchPlanes asks the source for the aircraft around lat, lon and keeps them.
func (f *Feature) fetchPlanes(ctx context.Context, s Settings, lat, lon, radius float64) {
	src := s.source()
	f.mu.Lock()
	if src == SourceFR24 && (s.FR24Token == "" || time.Now().Before(f.fr24Off)) {
		src = SourceADSBLol // no token, or one just refused: the free network meanwhile
	}
	f.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var (
		got   []Aircraft
		err   error
		name  string
		every time.Duration
		note  string
	)
	switch src {
	case SourceFR24:
		name, every = "Flightradar24", s.fr24Every()
		got, err = fetchFR24(cctx, s.FR24Token, lat, lon, radius)
		if errors.Is(err, errRefused) {
			slog.Info("sky: Flightradar24 refused the token; adsb.lol meanwhile", "err", err)
			f.mu.Lock()
			f.fr24Off = time.Now().Add(fr24Rest)
			f.mu.Unlock()
			note = "Flightradar24 refused the token: showing adsb.lol"
			name, every = "adsb.lol", adsbLolEvery
			got, err = fetchADSBLol(cctx, lat, lon, radius)
		}
	case SourceDump1090:
		name, every = "your receiver", dump1090Every
		got, err = fetchDump1090(cctx, s.Dump1090)
	case SourceDemo:
		name, every = "demo", demoEvery
		f.mu.Lock()
		hl, ho := f.homeLat, f.homeLon
		f.mu.Unlock()
		got = demoAircraft(hl, ho, time.Now())
	default:
		name, every = "adsb.lol", adsbLolEvery
		got, err = fetchADSBLol(cctx, lat, lon, radius)
	}
	if s.source() == SourceFR24 && src != SourceFR24 && note == "" {
		note = "Flightradar24 needs fr24_token: showing adsb.lol"
		if s.FR24Token != "" {
			note = "Flightradar24 refused the token: showing adsb.lol"
		}
	}
	f.mu.Lock()
	f.planesBusy = false
	if err != nil {
		if f.sourceNote == "" || f.sourceNote[0] != '!' {
			slog.Info("sky: aircraft", "source", name, "err", err)
		}
		// Marked with a ! so the log has it once; the board shows it without.
		f.sourceNote = "!" + name + ": " + err.Error()
		f.planesDue = time.Now().Add(max(every, 20*time.Second))
	} else {
		if f.source != name {
			f.planes = nil // a different source's ids are not this one's
		}
		f.planes = merge(f.planes, got, time.Now())
		f.source, f.sourceNote = name, note
		f.planesDue = time.Now().Add(every)
	}
	f.mu.Unlock()
	f.changed()
}

// changed is something new to draw, coalesced with what else arrives in the next moment.
func (f *Feature) changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emit != nil {
		return
	}
	f.emit = time.AfterFunc(changedEvery, func() {
		f.mu.Lock()
		f.emit = nil
		f.mu.Unlock()
		f.Changed.Emit(struct{}{})
	})
}

func (f *Feature) tileArrived() {
	f.mu.Lock()
	f.gen++
	f.mu.Unlock()
	f.changed()
}

// worldPixel is a place on the Web Mercator world at a zoom, in pixels.
func worldPixel(lat, lon float64, zoom int) (x, y float64) {
	n := float64(tileSize) * math.Pow(2, float64(zoom))
	x = (lon + 180) / 360 * n
	lat = math.Max(math.Min(lat, 85.05), -85.05)
	phi := lat * math.Pi / 180
	y = (1 - math.Log(math.Tan(phi)+1/math.Cos(phi))/math.Pi) / 2 * n
	return x, y
}

// latLon is worldPixel turned back.
func latLon(x, y float64, zoom int) (lat, lon float64) {
	n := float64(tileSize) * math.Pow(2, float64(zoom))
	lon = x/n*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*y/n))) * 180 / math.Pi
	return lat, lon
}

func (f *Feature) centerLatLonLocked() (lat, lon float64) {
	lat, lon = latLon(f.cx, f.cy, f.zoom)
	// Rounded, so a small pan does not ask for a new circle each time.
	return math.Round(lat*20) / 20, math.Round(lon*20) / 20
}

// radiusLocked is how far around the map's middle the aircraft are asked for: the view's half
// diagonal, or the setting when that is further, within the sources' limit.
func (f *Feature) radiusLocked(s Settings) float64 {
	lat, _ := latLon(f.cx, f.cy, f.zoom)
	mPerPx := 156543.03 * math.Cos(lat*math.Pi/180) / math.Pow(2, float64(f.zoom))
	half := math.Hypot(float64(f.w), float64(f.h)) / 2 * mPerPx / 1000
	return min(max(half, s.radius()), maxRadius)
}

// Pan moves the map by a finger's dx, dy: the ground follows the finger.
func (f *Feature) Pan(dx, dy int) {
	f.mu.Lock()
	f.cx -= float64(dx)
	f.cy -= float64(dy)
	f.clampLocked()
	f.mu.Unlock()
}

// ZoomAt zooms in (by > 0) or out a step, the ground under p (a point on the view) staying under it.
func (f *Feature) ZoomAt(by int, p image.Point) {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := min(max(f.zoom+by, minZoom), maxZoom)
	if z == f.zoom {
		return
	}
	k := math.Pow(2, float64(z-f.zoom))
	ox, oy := float64(p.X)-float64(f.w)/2, float64(p.Y)-float64(f.h)/2
	wx, wy := f.cx+ox, f.cy+oy
	f.cx, f.cy, f.zoom = wx*k-ox, wy*k-oy, z
	f.clampLocked()
	f.planesDue = time.Time{} // a wider view wants a wider circle
}

// Home puts the map back on home, at the zoom it opens at.
func (f *Feature) Home() {
	s, _ := current()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.homeOK {
		return
	}
	f.zoom = s.zoom()
	f.cx, f.cy = worldPixel(f.homeLat, f.homeLon, f.zoom)
	f.planesDue = time.Time{}
}

// clampLocked keeps the map on the world: round the dateline, and not past the poles.
func (f *Feature) clampLocked() {
	n := float64(tileSize) * math.Pow(2, float64(f.zoom))
	f.cx = math.Mod(math.Mod(f.cx, n)+n, n)
	half := float64(f.h) / 2
	if n > float64(f.h) {
		f.cy = math.Min(math.Max(f.cy, half), n-half)
	} else {
		f.cy = n / 2
	}
}

// View is what the board draws over the map.
type View struct {
	Zoom     int
	X0, Y0   float64 // the view's top left, in world pixels at Zoom
	W, H     int
	HomeOK   bool
	HomeLat  float64
	HomeLon  float64
	Waiting  string // why there is no map yet: home's place not known
	Aircraft []Aircraft
	Source   string // where the aircraft come from
	Note     string // about the source, the radar or the settings
	RadarOn  bool
	RadarAt  time.Time
	Wind     *Wind
	WindNote string
	WindSet  bool
	Credit   string
}

// Pixel is where a place falls on the view, the world's copy nearest the view's middle.
func (v View) Pixel(lat, lon float64) (x, y float64) {
	wx, wy := worldPixel(lat, lon, v.Zoom)
	n := float64(tileSize) * math.Pow(2, float64(v.Zoom))
	d := wx - v.X0 - float64(v.W)/2
	d -= n * math.Round(d/n)
	return d + float64(v.W)/2, wy - v.Y0
}

// KmPerPixel is the map's scale at its middle.
func (v View) KmPerPixel() float64 {
	lat, _ := latLon(v.X0+float64(v.W)/2, v.Y0+float64(v.H)/2, v.Zoom)
	return 156.54303 * math.Cos(lat*math.Pi/180) / math.Pow(2, float64(v.Zoom))
}

// View is the board as it stands, for a view w by h; the aircraft highest first, so the low ones near
// home are drawn over them.
func (f *Feature) View(w, h int) View {
	s, problem := current()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.w, f.h = w, h
	v := View{Zoom: f.zoom, X0: f.cx - float64(w)/2, Y0: f.cy - float64(h)/2, W: w, H: h,
		HomeOK: f.homeOK, HomeLat: f.homeLat, HomeLon: f.homeLon, Source: f.source,
		RadarOn: s.radarOn() && f.radar.layer != "", RadarAt: f.radar.at,
		Wind: f.wind, WindNote: f.windNote, WindSet: s.WindyKey != ""}
	if !f.homeOK {
		v.Waiting = "Waiting for home's place"
		if f.homeErr != "" {
			v.Waiting = "Home's place: " + f.homeErr
		}
	}
	for _, a := range f.planes {
		v.Aircraft = append(v.Aircraft, a)
	}
	sort.Slice(v.Aircraft, func(i, j int) bool {
		if v.Aircraft[i].AltFt != v.Aircraft[j].AltFt {
			return v.Aircraft[i].AltFt > v.Aircraft[j].AltFt
		}
		return v.Aircraft[i].ID < v.Aircraft[j].ID
	})
	note := f.sourceNote
	if len(note) > 0 && note[0] == '!' {
		note = note[1:]
	}
	for _, n := range []string{problem, f.radarNote} {
		if n != "" {
			if note != "" {
				note += " · "
			}
			note += n
		}
	}
	v.Note = note
	v.Credit = credit(s, f.source, v.RadarOn, f.wind != nil)
	return v
}

// credit is the board's line of credits, as each source asks.
func credit(s Settings, source string, radar, windy bool) string {
	c := "© OpenStreetMap contributors"
	if radar {
		c += " · Radar RainViewer"
	}
	switch source {
	case "adsb.lol":
		c += " · Aircraft adsb.lol (ODbL)"
	case "Flightradar24":
		c += " · Aircraft Flightradar24"
	}
	if windy {
		c += " · Wind Windy.com"
	}
	return c
}

func (s Settings) style() string {
	if s.Map == mapOSM {
		return mapOSM
	}
	return mapDark
}

// Draw puts the map and the rain for the view onto dst (the view's size, at its origin). The two are
// composed once for a place, a zoom and the tiles in hand, and copied from there while nothing changes:
// a frame that only moves the aircraft is one copy.
func (f *Feature) Draw(dst *image.RGBA) {
	s, _ := current()
	w, h := dst.Rect.Dx(), dst.Rect.Dy()
	f.mu.Lock()
	x0, y0 := int(math.Floor(f.cx-float64(w)/2)), int(math.Floor(f.cy-float64(h)/2))
	z, gen := f.zoom, f.gen
	radar := ""
	if s.radarOn() {
		radar = f.radar.layer
	}
	key := fmt.Sprintf("%d %d %d %dx%d %d %s %s", x0, y0, z, w, h, gen, s.style(), radar)
	if f.composed != nil && f.key == key {
		draw.Draw(dst, dst.Rect, f.composed, image.Point{}, draw.Src)
		f.mu.Unlock()
		return
	}
	if f.composed == nil || f.composed.Rect.Size() != dst.Rect.Size() {
		f.composed = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	out := f.composed
	f.mu.Unlock()

	f.compose(out, s.style(), radar, z, x0, y0)

	f.mu.Lock()
	f.key = key
	f.mu.Unlock()
	draw.Draw(dst, dst.Rect, out, image.Point{}, draw.Src)
}

// compose draws the tiles of the view whose top left is world pixel x0, y0 at zoom z.
func (f *Feature) compose(out *image.RGBA, style, radar string, z, x0, y0 int) {
	w, h := out.Rect.Dx(), out.Rect.Dy()
	n := 1 << z
	for ty := floorDiv(y0, tileSize); ty*tileSize < y0+h; ty++ {
		for tx := floorDiv(x0, tileSize); tx*tileSize < x0+w; tx++ {
			at := image.Pt(tx*tileSize-x0, ty*tileSize-y0)
			r := image.Rectangle{Min: at, Max: at.Add(image.Pt(tileSize, tileSize))}
			if ty < 0 || ty >= n {
				draw.Draw(out, r, image.NewUniform(ground), image.Point{}, draw.Src)
				continue
			}
			x := ((tx % n) + n) % n
			if img, ok := f.tiles.tile(tileKey{style, z, x, ty}); ok {
				draw.Draw(out, r, img, image.Point{}, draw.Src)
			} else {
				f.coarser(out, r, style, z, x, ty)
			}
			if radar != "" {
				if img := f.radarTile(radar, z, x, ty); img != nil {
					draw.Draw(out, r, img, image.Point{}, draw.Over)
				}
			}
		}
	}
}

// coarser fills a tile's place from a coarser tile already in memory, enlarged, while the tile itself
// is on its way; the ground where there is none.
func (f *Feature) coarser(out *image.RGBA, r image.Rectangle, style string, z, x, y int) {
	for dz := 1; dz <= 3 && z-dz >= 0; dz++ {
		img, ok := f.tiles.peek(tileKey{style, z - dz, x >> dz, y >> dz})
		if !ok {
			continue
		}
		part := tileSize >> dz
		sx, sy := (x&(1<<dz-1))*part, (y&(1<<dz-1))*part
		xdraw.NearestNeighbor.Scale(out, r, img, image.Rect(sx, sy, sx+part, sy+part), draw.Src, nil)
		return
	}
	draw.Draw(out, r, image.NewUniform(ground), image.Point{}, draw.Src)
}

// radarTile is the rain over tile x, y at zoom z, see-through to radarAlpha: RainViewer's own tile, or
// at zooms past radarTop the part of its tile there enlarged, made once and kept.
func (f *Feature) radarTile(layer string, z, x, y int) *image.RGBA {
	made := tileKey{"x" + layer, z, x, y}
	if img, ok := f.tiles.peek(made); ok {
		return img
	}
	rz := min(z, radarTop)
	dz := z - rz
	src, ok := f.tiles.tile(tileKey{layer, rz, x >> dz, y >> dz})
	if !ok {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, tileSize, tileSize))
	if dz == 0 {
		copy(out.Pix, src.Pix)
	} else {
		part := tileSize >> dz
		sx, sy := (x&(1<<dz-1))*part, (y&(1<<dz-1))*part
		xdraw.ApproxBiLinear.Scale(out, out.Rect, src, image.Rect(sx, sy, sx+part, sy+part), draw.Src, nil)
	}
	// Premultiplied: every channel takes the same fraction.
	for i := range out.Pix {
		out.Pix[i] = uint8(uint16(out.Pix[i]) * radarAlpha / 255)
	}
	f.tiles.keep(made, out)
	return out
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
