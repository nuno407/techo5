//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
)

// The Sky board: the map and the rain (composed by feature/skyview), and over them home, the aircraft
// with their trails and labels, the zoom and home buttons at the right, the picked aircraft's card at
// the left, Windy's wind when there is a key, and the credits along the foot. The aircraft are colored
// by height, as on most flight radars: low ones warm, high ones cool, those on the ground gray.

const (
	skyButton = 52 // the map buttons' size
	skyPlane  = 13 // half an aircraft's length
)

// Heights' colors: on the ground, under 10,000 ft, under 25,000 ft, above.
var (
	skyGround = color.RGBA{0x8a, 0x8a, 0x8a, 0xff}
	skyLow    = color.RGBA{0xf2, 0x9b, 0x38, 0xff}
	skyMid    = color.RGBA{0xf2, 0xd5, 0x4a, 0xff}
	skyHigh   = color.RGBA{0x7f, 0xd6, 0xf0, 0xff}
	skyHalo   = color.RGBA{0x10, 0x10, 0x14, 0xff}
)

func skyColor(a skyview.Aircraft) color.RGBA {
	switch {
	case a.Ground:
		return skyGround
	case a.AltFt < 10000:
		return skyLow
	case a.AltFt < 25000:
		return skyMid
	}
	return skyHigh
}

// altText is a height as pilots say it: feet low down, flight levels above 18,000.
func altText(a skyview.Aircraft) string {
	switch {
	case a.Ground:
		return "GND"
	case a.AltFt >= 18000:
		return fmt.Sprintf("FL%03d", (a.AltFt+50)/100)
	}
	return fmt.Sprintf("%d ft", (a.AltFt+50)/100*100)
}

func (r *renderer) skyPage(s scene) []boardZone {
	v, _ := s.board.data.(skyview.View)
	fc := r.faces()
	if !v.HomeOK {
		zones := r.boardBar(s, "Sky", false, false)
		y := r.h / 2
		for _, line := range r.wrapLines(fc.value, v.Waiting, r.w-4*r.s(boardIn)) {
			r.text(fc.value, line, (r.w-r.width(fc.value, line))/2, y, dim)
			y += r.s(34)
		}
		return zones
	}
	skyview.Get().Draw(r.dst)

	// Home: a ring in the accent with a dark edge, as on the rain map.
	hx, hy := v.Pixel(v.HomeLat, v.HomeLon)
	hp := image.Pt(int(hx), int(hy))
	r.ring(hp, r.s(10), skyHalo)
	r.ring(hp, r.s(8), amber)
	r.ring(hp, r.s(7), amber)

	// The trails first, then the aircraft over every trail, then the labels where they fit.
	k := r.sk()
	type placed struct {
		a    skyview.Aircraft
		x, y float64
	}
	var shown []placed
	bounds := r.dst.Rect.Inset(-r.s(30))
	for _, a := range v.Aircraft {
		lat, lon := a.Position(s.now)
		x, y := v.Pixel(lat, lon)
		if !image.Pt(int(x), int(y)).In(bounds) {
			continue
		}
		shown = append(shown, placed{a, x, y})
		c := skyColor(a)
		trail := lerp(skyHalo, c, 0.45)
		px, py := x, y
		for i := len(a.Trail) - 1; i >= 0; i-- {
			tx, ty := v.Pixel(a.Trail[i][0], a.Trail[i][1])
			if math.Hypot(tx-px, ty-py) > float64(r.w) {
				break // the far side of the dateline: no line across the world
			}
			r.aaLine(px, py, tx, ty, 1.6*k, trail)
			px, py = tx, ty
		}
	}
	var zones []boardZone
	for _, p := range shown {
		sel := p.a.ID == s.board.selected
		size := float64(r.s(skyPlane))
		if p.a.Ground {
			size *= 0.7
		}
		if sel {
			size *= 1.35
			r.ring(image.Pt(int(p.x), int(p.y)), int(size)+r.s(6), cream)
			r.ring(image.Pt(int(p.x), int(p.y)), int(size)+r.s(6)+1, cream)
		}
		r.plane(p.x, p.y, p.a.Track, size, skyColor(p.a))
		hit := r.s(26)
		zones = append(zones, boardZone{r: image.Rect(int(p.x)-hit, int(p.y)-hit, int(p.x)+hit, int(p.y)+hit), kind: "plane", id: p.a.ID})
	}
	// Labels: the picked one always, then from the highest down where they do not cover one already
	// written (the lowest, near home, are the likeliest to crowd, and the ground's are left unsaid).
	callF, altF, small := r.textFace(true, 13), r.textFace(false, 12), r.textFace(false, 12)
	var taken []image.Rectangle
	label := func(p placed, force bool) {
		name := p.a.Callsign
		if name == "" {
			name = strings.ToUpper(p.a.Reg)
		}
		if name == "" {
			name = strings.ToUpper(p.a.ID)
		}
		line2 := altText(p.a)
		w := max(r.width(callF, name), r.width(altF, line2))
		x, y := int(p.x)+r.s(14), int(p.y)-r.s(2)
		box := image.Rect(x-r.s(3), y-r.s(14), x+w+r.s(3), y+r.s(17))
		if !force {
			for _, t := range taken {
				if box.Overlaps(t) {
					return
				}
			}
		}
		taken = append(taken, box)
		washOver(r.dst, box, color.RGBA{0, 0, 0, 130})
		r.text(callF, name, x, y, cream)
		r.text(altF, line2, x, y+r.s(14), skyColor(p.a))
	}
	var picked *placed
	for i := range shown {
		if shown[i].a.ID == s.board.selected {
			picked = &shown[i]
			label(shown[i], true)
		}
	}
	for _, p := range shown {
		if !p.a.Ground && p.a.ID != s.board.selected {
			label(p, false)
		}
	}

	// The bar: how many aircraft, from where, and the rain's time.
	flying := 0
	for _, p := range shown {
		if !p.a.Ground {
			flying++
		}
	}
	summary := fmt.Sprintf("%d aircraft in view", flying)
	if v.Source != "" {
		summary += " · " + v.Source
	}
	if v.RadarOn {
		summary += " · rain " + clockHM(v.RadarAt.Local())
	}
	zones = append(zones, r.boardBar(s, summary, false, true)...)

	// Windy's wind, under the bar at the right, when there is a key.
	if v.WindSet {
		text := "Wind: " + v.WindNote
		if v.Wind != nil {
			w := v.Wind
			text = fmt.Sprintf("Wind %.0f km/h from %s · gusts %.0f", w.SpeedKmh, compass(w.From), w.GustKmh)
			if w.RainMm >= 0.1 {
				text += fmt.Sprintf(" · rain %.1f mm", w.RainMm)
			}
		}
		wf := r.textFace(false, 15)
		tw := r.width(wf, text)
		b := image.Rect(r.w-r.s(boardIn)-tw-r.s(20), r.s(boardBarH)+r.s(8), r.w-r.s(boardIn), r.s(boardBarH)+r.s(36))
		washOver(r.dst, b, color.RGBA{0, 0, 0, 150})
		r.text(wf, text, b.Min.X+r.s(10), b.Max.Y-r.s(9), cream)
	}

	// The buttons: zoom in, zoom out, home, down the right edge above the foot.
	side := r.s(skyButton)
	x1 := r.w - r.s(boardIn)
	y := r.h - r.s(40) - 3*side - 2*r.s(10)
	for _, b := range []struct{ kind, label string }{{"zoomin", "+"}, {"zoomout", "−"}, {"home", "⌂"}} {
		box := image.Rect(x1-side, y, x1, y+side)
		r.roundShadow(box, float64(side)/2, r.sf(8), r.s(2), shadowAlpha())
		r.roundFill(box, float64(side)/2, surface(6), surface(4))
		r.roundHighlight(box, float64(side)/2)
		r.skyButtonMark(box, b.kind)
		zones = append(zones, boardZone{r: box.Inset(-r.s(6)), kind: b.kind})
		y += side + r.s(10)
	}

	// The foot: the scale, the credits, and a note about the sources over them.
	foot := image.Rect(0, r.h-r.s(22), r.w, r.h)
	washOver(r.dst, foot, color.RGBA{0, 0, 0, 160})
	scale := r.scaleBar(v, foot)
	r.text(small, r.fit(small, v.Credit, r.w-scale-r.s(boardIn)*2), scale+r.s(16), r.h-r.s(8), dim)
	if v.Note != "" {
		note := r.fit(small, v.Note, r.w-2*r.s(boardIn)-side-r.s(20))
		b := image.Rect(0, r.h-r.s(48), r.s(boardIn)+r.width(small, note)+r.s(12), r.h-r.s(22))
		washOver(r.dst, b, color.RGBA{0, 0, 0, 160})
		r.text(small, note, r.s(boardIn), r.h-r.s(30), amber)
	}

	if picked != nil {
		zones = append(zones, r.planeCard(s, v, picked.a))
	}
	return zones
}

// plane is an aircraft seen from above, nose along track: a fuselage, swept wings and a tailplane, in
// c over a dark edge so it reads over the map and the rain alike.
func (r *renderer) plane(x, y, track, size float64, c color.RGBA) {
	th := track * math.Pi / 180
	fx, fy := math.Sin(th), -math.Cos(th) // forward, on the screen
	sx, sy := -fy, fx                     // to the right
	pt := func(f, s float64) (float64, float64) { return x + (fx*f+sx*s)*size, y + (fy*f+sy*s)*size }
	k := r.sk()
	strokes := [][4]float64{}
	add := func(f0, s0, f1, s1 float64) {
		ax, ay := pt(f0, s0)
		bx, by := pt(f1, s1)
		strokes = append(strokes, [4]float64{ax, ay, bx, by})
	}
	add(1, 0, -0.85, 0)       // fuselage
	add(0.15, 0, -0.25, 0.95) // wings, swept back
	add(0.15, 0, -0.25, -0.95)
	add(-0.7, 0, -0.95, 0.4) // tailplane
	add(-0.7, 0, -0.95, -0.4)
	for _, l := range strokes {
		r.aaLine(l[0], l[1], l[2], l[3], 4.6*k, skyHalo)
	}
	for _, l := range strokes {
		r.aaLine(l[0], l[1], l[2], l[3], 2.4*k, c)
	}
}

// skyButtonMark draws a map button's sign: a plus, a minus, or a house.
func (r *renderer) skyButtonMark(b image.Rectangle, kind string) {
	cx, cy := float64(b.Min.X)+float64(b.Dx())/2, float64(b.Min.Y)+float64(b.Dy())/2
	arm, pen := r.sf(10), r.sf(3)
	switch kind {
	case "zoomin":
		r.aaLine(cx-arm, cy, cx+arm, cy, pen, cream)
		r.aaLine(cx, cy-arm, cx, cy+arm, pen, cream)
	case "zoomout":
		r.aaLine(cx-arm, cy, cx+arm, cy, pen, cream)
	case "home":
		a := arm
		pen = r.sf(2)
		r.aaLine(cx-a, cy-a*0.1, cx, cy-a, pen, cream) // the roof
		r.aaLine(cx, cy-a, cx+a, cy-a*0.1, pen, cream)
		r.aaLine(cx-a*0.7, cy-a*0.3, cx-a*0.7, cy+a*0.8, pen, cream) // the walls and the floor
		r.aaLine(cx+a*0.7, cy-a*0.3, cx+a*0.7, cy+a*0.8, pen, cream)
		r.aaLine(cx-a*0.7, cy+a*0.8, cx+a*0.7, cy+a*0.8, pen, cream)
	}
}

// scaleBar draws a scale of a round distance at the foot's left and returns where it ends.
func (r *renderer) scaleBar(v skyview.View, foot image.Rectangle) int {
	km := v.KmPerPixel()
	if km <= 0 {
		return r.s(boardIn)
	}
	want := km * float64(r.s(110))
	step := 1.0
	for _, n := range []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000} {
		if n <= want {
			step = n
		}
	}
	px := int(step / km)
	x0 := r.s(boardIn)
	y := float64(foot.Min.Y + foot.Dy()/2 + r.s(2))
	pen := r.sf(2)
	r.aaLine(float64(x0), y, float64(x0+px), y, pen, cream)
	r.aaLine(float64(x0), y-r.sf(5), float64(x0), y+r.sf(1), pen, cream)
	r.aaLine(float64(x0+px), y-r.sf(5), float64(x0+px), y+r.sf(1), pen, cream)
	label := fmt.Sprintf("%.0f km", step)
	f := r.textFace(false, 12)
	r.text(f, label, x0+px+r.s(8), foot.Max.Y-r.s(8), cream)
	return x0 + px + r.s(8) + r.width(f, label)
}

// planeCard is the picked aircraft's card, at the left above the foot: who it is, and how it flies.
func (r *renderer) planeCard(s scene, v skyview.View, a skyview.Aircraft) boardZone {
	nameF, lineF := r.textFace(true, 20), r.textFace(false, 15)
	lines := []string{}
	what := strings.TrimSpace(strings.Join([]string{a.Type, a.Reg}, "  "))
	if what != "" {
		lines = append(lines, what)
	}
	if a.From != "" || a.To != "" {
		lines = append(lines, fmt.Sprintf("%s → %s", or(a.From, "?"), or(a.To, "?")))
	}
	climb := ""
	switch {
	case a.VRate > 300:
		climb = fmt.Sprintf("  ↑ %d ft/min", a.VRate)
	case a.VRate < -300:
		climb = fmt.Sprintf("  ↓ %d ft/min", -a.VRate)
	}
	lines = append(lines, altText(a)+climb)
	lines = append(lines, fmt.Sprintf("%.0f kt · heading %03.0f°", a.SpeedKt, a.Track))
	lat, lon := a.Position(s.now)
	lines = append(lines, fmt.Sprintf("%.0f km from home", haversineKm(v.HomeLat, v.HomeLon, lat, lon)))
	if a.Squawk != "" {
		lines = append(lines, "squawk "+a.Squawk)
	}
	name := or(a.Callsign, strings.ToUpper(a.ID))
	w := r.width(nameF, name)
	for _, l := range lines {
		w = max(w, r.width(lineF, l))
	}
	pad := r.s(14)
	h := pad*2 + r.s(26) + len(lines)*r.s(21)
	box := image.Rect(r.s(boardIn), r.h-r.s(56)-h, r.s(boardIn)+w+2*pad, r.h-r.s(56))
	rad := r.sf(16)
	r.roundShadow(box, rad, r.sf(14), r.s(4), shadowAlpha())
	r.roundFill(box, rad, surface(4), surface(3))
	r.roundHighlight(box, rad)
	y := box.Min.Y + pad + r.s(18)
	r.text(nameF, name, box.Min.X+pad, y, skyColor(a))
	y += r.s(27)
	for _, l := range lines {
		r.text(lineF, l, box.Min.X+pad, y, cream)
		y += r.s(21)
	}
	return boardZone{r: box, kind: "card"}
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// compass is a bearing as one of the sixteen points.
func compass(deg float64) string {
	points := []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}
	return points[int(math.Round(math.Mod(deg+360, 360)/22.5))%16]
}

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 6371 * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
