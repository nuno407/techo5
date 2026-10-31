package skyview

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // a map server may answer in either
	_ "image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The tiles: fetched by two workers, newest asked first (a pan's latest view before the ones it left),
// kept decoded in memory up to a count and, for the map, on disk. Decoding is the costly part on the
// Cortex-A9 (a 256 pixel PNG is a few milliseconds), so a tile is decoded once and drawn from memory
// every frame after; the disk is what a restart or a pan back over known ground reads.

const (
	tileSize = 256

	memTiles   = 120 // decoded map tiles kept: a 1366x768 view is 24 to 35 of them
	memRadar   = 90  // decoded radar tiles, as fetched and as enlarged for the zoom in view
	queueMax   = 80  // tiles waiting to be fetched; the oldest asked are dropped past it
	workers    = 2   // tiles fetched at once: OSM's policy asks for no more than two
	diskFor    = 14 * 24 * time.Hour
	diskTiles  = 6000 // map tiles kept on disk, about 90 MB of PNG
	retryAfter = 2 * time.Minute

	userAgent = "TECHO5 (https://github.com/HuskerMinion/techo5)"
)

// tileKey is one tile of a layer.
type tileKey struct {
	layer   string
	z, x, y int
}

// Map styles (Settings.Map): OpenStreetMap's own tiles, darkened here for a screen in a dark room
// (darken), or as they come. Both are the same tiles, fetched and kept once.
const (
	mapDark = "dark"
	mapOSM  = "osm"
)

// osmTiles is where the map's tiles are; a test points it at its own server.
var osmTiles = "https://tile.openstreetmap.org/%d/%d/%d.png"

// tileURL is where a tile of a fetched layer comes from: a map style, or a radar frame ("rv" and the
// frame's address).
func tileURL(k tileKey) string {
	switch {
	case k.layer == mapDark || k.layer == mapOSM:
		return fmt.Sprintf(osmTiles, k.z, k.x, k.y)
	case strings.HasPrefix(k.layer, "rv:"):
		// Scheme 2 with smoothing and snow, the colors RainViewer's own map shows.
		return fmt.Sprintf("%s/%d/%d/%d/%d/2/1_1.png", strings.TrimPrefix(k.layer, "rv:"), tileSize, k.z, k.x, k.y)
	}
	return ""
}

// kept is whether a layer's tiles are kept on disk: the map's, not the radar's, which is new every ten
// minutes.
func kept(layer string) bool { return layer == mapDark || layer == mapOSM }

// lru is decoded tiles, the least lately drawn going first.
type lru struct {
	max int
	ll  *list.List
	m   map[tileKey]*list.Element
}

type lruEntry struct {
	k   tileKey
	img *image.RGBA
}

func newLRU(n int) *lru { return &lru{max: n, ll: list.New(), m: map[tileKey]*list.Element{}} }

func (c *lru) get(k tileKey) (*image.RGBA, bool) {
	e, ok := c.m[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*lruEntry).img, true
}

func (c *lru) put(k tileKey, img *image.RGBA) {
	if e, ok := c.m[k]; ok {
		e.Value.(*lruEntry).img = img
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&lruEntry{k, img})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.m, old.Value.(*lruEntry).k)
	}
}

// dropLayer forgets every tile of the layers starting with prefix but keep: an old radar frame's.
func (c *lru) dropLayer(prefix, keep string) {
	for k, e := range c.m {
		if !strings.HasPrefix(k.layer, prefix) || k.layer == keep {
			continue
		}
		c.ll.Remove(e)
		delete(c.m, k)
	}
}

// tileStore is the cache and the queue in front of the network.
type tileStore struct {
	mu      sync.Mutex
	mem     *lru
	radar   *lru
	queued  map[tileKey]bool // waiting or being fetched
	queue   []tileKey        // newest last
	failed  map[tileKey]time.Time
	wake    chan struct{}
	arrived func() // a tile came: the board is drawn again
	dir     string // where map tiles are kept; "" keeps none
}

func newTileStore(dir string, arrived func()) *tileStore {
	return &tileStore{
		mem: newLRU(memTiles), radar: newLRU(memRadar), queued: map[tileKey]bool{},
		failed: map[tileKey]time.Time{}, wake: make(chan struct{}, 1), arrived: arrived, dir: dir,
	}
}

func (t *tileStore) cache(layer string) *lru {
	if kept(layer) {
		return t.mem
	}
	return t.radar
}

// tile is a tile if it is in memory, and otherwise asks for it and says no: the frame draws what it has,
// and the tile's arrival draws the next.
func (t *tileStore) tile(k tileKey) (*image.RGBA, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if img, ok := t.cache(k.layer).get(k); ok {
		return img, true
	}
	t.askLocked(k)
	return nil, false
}

// peek is a tile if it is in memory, without asking for it: for the coarser tiles drawn under one that
// is on its way.
func (t *tileStore) peek(k tileKey) (*image.RGBA, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cache(k.layer).get(k)
}

// keep puts a tile made here (a radar tile enlarged) in memory.
func (t *tileStore) keep(k tileKey, img *image.RGBA) {
	t.mu.Lock()
	t.cache(k.layer).put(k, img)
	t.mu.Unlock()
}

func (t *tileStore) askLocked(k tileKey) {
	if t.queued[k] || tileURL(k) == "" {
		return
	}
	if at, ok := t.failed[k]; ok && time.Since(at) < retryAfter {
		return
	}
	t.queued[k] = true
	t.queue = append(t.queue, k)
	if len(t.queue) > queueMax {
		delete(t.queued, t.queue[0])
		t.queue = t.queue[1:]
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// forgetRadar drops the tiles of every radar frame but the one now shown, and their place in the queue.
func (t *tileStore) forgetRadar(keep string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.radar.dropLayer("rv:", keep)
	t.radar.dropLayer("xrv:", "x"+keep)
	q := t.queue[:0]
	for _, k := range t.queue {
		if strings.HasPrefix(k.layer, "rv:") && k.layer != keep {
			delete(t.queued, k)
			continue
		}
		q = append(q, k)
	}
	t.queue = q
}

// run is the workers, until ctx ends.
func (t *tileStore) run(ctx context.Context) {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k, ok := t.next()
				if !ok {
					select {
					case <-ctx.Done():
						return
					case <-t.wake:
					}
					continue
				}
				img, err := t.load(ctx, k)
				t.mu.Lock()
				delete(t.queued, k)
				if err != nil {
					t.failed[k] = time.Now()
				} else {
					delete(t.failed, k)
					t.cache(k.layer).put(k, img)
				}
				t.mu.Unlock()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Debug("sky: tile", "layer", k.layer, "z", k.z, "x", k.x, "y", k.y, "err", err)
					continue
				}
				t.arrived()
			}
		}()
	}
	wg.Wait()
}

// next is the newest tile asked for, taken off the queue.
func (t *tileStore) next() (tileKey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) == 0 {
		return tileKey{}, false
	}
	k := t.queue[len(t.queue)-1]
	t.queue = t.queue[:len(t.queue)-1]
	return k, true
}

// path is where a map tile is kept: as it came, whichever style it is drawn in.
func (t *tileStore) path(k tileKey) string {
	return filepath.Join(t.dir, mapOSM, fmt.Sprint(k.z), fmt.Sprint(k.x), fmt.Sprintf("%d.png", k.y))
}

// load is a tile from disk when it is kept there and fresh, otherwise from the network (and then kept).
// A kept tile past its two weeks is still drawn when the network cannot be reached.
func (t *tileStore) load(ctx context.Context, k tileKey) (*image.RGBA, error) {
	var stale []byte
	if kept(k.layer) && t.dir != "" {
		if fi, err := os.Stat(t.path(k)); err == nil {
			b, err := os.ReadFile(t.path(k))
			if err == nil && time.Since(fi.ModTime()) < diskFor {
				if img, err := decode(b); err == nil {
					return styled(k.layer, img), nil
				}
			}
			stale = b
		}
	}
	b, err := fetch(ctx, tileURL(k), 1<<20)
	if err != nil {
		if stale != nil {
			img, err := decode(stale)
			if err != nil {
				return nil, err
			}
			return styled(k.layer, img), nil
		}
		return nil, err
	}
	img, err := decode(b)
	if err != nil {
		return nil, err
	}
	if kept(k.layer) && t.dir != "" {
		save(t.path(k), b)
	}
	return styled(k.layer, img), nil
}

// decode is a tile as RGBA, whatever it was sent as.
func decode(b []byte) (*image.RGBA, error) {
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if rgba, ok := src.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba, nil
	}
	out := image.NewRGBA(image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy()))
	draw.Draw(out, out.Rect, src, src.Bounds().Min, draw.Src)
	return out, nil
}

// save writes a tile through a temporary file, so a crash leaves no half tile.
func save(path string, b []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, path)
}

// prune keeps the disk under diskTiles map tiles, the oldest going first. Once at start.
func (t *tileStore) prune() {
	if t.dir == "" {
		return
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	_ = filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".tmp") {
			_ = os.Remove(p)
			return nil
		}
		if fi, err := d.Info(); err == nil {
			files = append(files, file{p, fi.ModTime()})
		}
		return nil
	})
	if len(files) <= diskTiles {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files[:len(files)-diskTiles] {
		_ = os.Remove(f.path)
	}
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// fetch is a GET with TECHO5's User-Agent, read up to limit bytes.
func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return send(req, limit)
}

// send is a request whose answer has to be 200, read up to limit bytes.
func send(req *http.Request, limit int64) ([]byte, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		return nil, fmt.Errorf("longer than %d bytes", limit)
	}
	return b, err
}

// statusErr is an answer that was not 200.
type statusErr struct{ code int }

func (e statusErr) Error() string { return fmt.Sprintf("%d %s", e.code, http.StatusText(e.code)) }

// styled is a map tile in its style: the dark one darkened, the rest as they came.
func styled(layer string, img *image.RGBA) *image.RGBA {
	if layer == mapDark {
		darken(img)
	}
	return img
}

// darken turns a light map dark in place, keeping its colors: each color inverted and turned half
// round the hue circle (what a browser's invert and hue-rotate do), so light land goes near black, water
// stays blue and roads stay their colors but dim, then laid on a warm floor at darkK of the light. Done
// once, as the tile is decoded.
func darken(img *image.RGBA) {
	// CSS's hue-rotate(180deg) matrix.
	m := [9]float64{
		-0.574, 1.430, 0.144,
		0.426, 0.430, 0.144,
		0.426, 1.430, -0.856,
	}
	const darkK, floor = 0.78, 14
	clamp := func(v float64) uint8 { return uint8(min(max(v, 0), 255)) }
	p := img.Pix
	for i := 0; i+3 < len(p); i += 4 {
		r, g, b := 255-float64(p[i]), 255-float64(p[i+1]), 255-float64(p[i+2])
		p[i] = clamp(floor + darkK*(m[0]*r+m[1]*g+m[2]*b))
		p[i+1] = clamp(floor*0.9 + darkK*(m[3]*r+m[4]*g+m[5]*b))
		p[i+2] = clamp(floor*0.8 + darkK*(m[6]*r+m[7]*g+m[8]*b))
	}
}
