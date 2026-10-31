//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
)

// The boards' common top bar, and the Work board: a column for each of GitHub, Slack and Jira, its
// counts along its head (those waiting on the owner in the accent), then its items, those waiting on
// the owner first with a bar of the accent beside them. A source with no token is a column that says
// what to add and where; one that failed says why, over what it had before. A tap on an item opens its
// card: the whole message or status, and the address to open it at.

// Sizes, in the Show 5's pixels as the rest of the package's are.
const (
	boardBarH = 60 // the top bar
	boardIn   = 14 // the page's margin
	boardGap  = 12 // between the columns
	workRowH  = 50 // an item
	workHeadH = 46 // a column's name
)

// workRows is how far each column could scroll in the frame last drawn, in rows: the most rows hidden
// under its foot.
var workRows struct {
	sync.Mutex
	max [3]int
}

func workScrollMax(col int) int {
	workRows.Lock()
	defer workRows.Unlock()
	if col < 0 || col >= len(workRows.max) {
		return 0
	}
	return workRows.max[col]
}

// workColumns are the three columns' boxes on a screen w wide, from top down.
func (p *paint) workColumns(top, h int) [3]image.Rectangle {
	m, gap := p.s(boardIn), p.s(boardGap)
	cw := (p.w - 2*m - 2*gap) / 3
	var out [3]image.Rectangle
	for i := range out {
		x := m + i*(cw+gap)
		out[i] = image.Rect(x, top, x+cw, h-m)
	}
	return out
}

// workColumnAt is the column under x on a screen w wide.
func workColumnAt(w, x int) int { return min(max(x*3/max(w, 1), 0), 2) }

// boardPage draws the board up.
func (r *renderer) boardPage(s scene) {
	var zones []boardZone
	if s.board.name == boardSky {
		zones = r.skyPage(s)
	} else {
		zones = r.workPage(s)
	}
	setBoardZones(zones)
}

// boardBar is the top bar: the Work and Sky tabs at the left, what the board says about itself beside
// them, the time and the × at the right. On the map (shaded) it is a dark band the map shows through.
func (r *renderer) boardBar(s scene, summary string, hot bool, shaded bool) []boardZone {
	fc := r.faces()
	barH := r.s(boardBarH)
	if shaded {
		washOver(r.dst, image.Rect(0, 0, r.w, barH), color.RGBA{0, 0, 0, 160})
	}
	var zones []boardZone

	// Work | Sky, the board up raised, as the drawer's tabs are.
	seg := image.Rect(r.s(boardIn), r.s(8), r.s(boardIn)+r.s(240), barH-r.s(8))
	segRad := float64(seg.Dy()) / 2
	r.roundFill(seg, segRad, surface(1), surface(1))
	r.roundStroke(seg, segRad, r.sf(1), ember)
	half := seg.Dx() / 2
	inset := r.s(4)
	for i, tab := range []struct{ id, label string }{{boardWork, "Work"}, {boardSky, "Sky"}} {
		b := image.Rect(seg.Min.X+i*half+inset, seg.Min.Y+inset, seg.Min.X+(i+1)*half-inset, seg.Max.Y-inset)
		fg, face := lerp(dim, cream, 0.3), fc.nav
		if tab.id == s.board.name {
			rad := float64(b.Dy()) / 2
			r.roundFill(b, rad, shift(amber, 14), shift(amber, -14))
			r.roundHighlight(b, rad)
			fg, face = onAccent(), fc.navBold
		}
		r.text(face, tab.label, b.Min.X+(b.Dx()-r.width(face, tab.label))/2, b.Min.Y+b.Dy()/2+r.s(9), fg)
		zones = append(zones, boardZone{r: image.Rect(seg.Min.X+i*half, 0, seg.Min.X+(i+1)*half, barH+r.s(6)), kind: "tab", id: tab.id})
	}

	// The ×, round, at the right.
	side := barH - r.s(16)
	c := image.Rect(r.w-r.s(boardIn)-side, r.s(8), r.w-r.s(boardIn), r.s(8)+side)
	r.roundFill(c, float64(side)/2, surface(7), surface(5))
	r.roundHighlight(c, float64(side)/2)
	cx, cy := float64(c.Min.X)+float64(c.Dx())/2, float64(c.Min.Y)+float64(c.Dy())/2
	arm, pen := r.sf(7), r.sf(26)/10
	r.aaLine(cx-arm, cy-arm, cx+arm, cy+arm, pen, cream)
	r.aaLine(cx-arm, cy+arm, cx+arm, cy-arm, pen, cream)
	zones = append(zones, boardZone{r: c.Inset(-r.s(10)), kind: "close"})

	// The time, left of the ×, and the summary between the tabs and it.
	t := clockText(s.now)
	tx := c.Min.X - r.s(18) - r.width(r.small, t)
	base := barH/2 + r.s(10)
	r.text(r.small, t, tx, base, dim)
	col := cream
	if hot {
		col = amber
	}
	room := tx - r.s(24) - (seg.Max.X + r.s(20))
	r.text(fc.value, r.fit(fc.value, summary, room), seg.Max.X+r.s(20), base, col)
	return zones
}

func (r *renderer) workPage(s scene) []boardZone {
	v := s.board.work
	summary := "Nothing waiting on you"
	switch {
	case v.Hot == 1:
		summary = "1 thing waiting on you"
	case v.Hot > 1:
		summary = fmt.Sprintf("%d things waiting on you", v.Hot)
	}
	if v.Problem != "" {
		summary = v.Problem
	}
	zones := r.boardBar(s, summary, v.Hot > 0 || v.Problem != "", false)

	// The calendar's strip across the top, the columns under it.
	top, zones := r.agendaStrip(s, v.Calendar, r.s(boardBarH)+r.s(4), zones)
	cols := r.workColumns(top, r.h)
	var most [3]int
	for i, fd := range v.Feeds {
		if i >= len(cols) {
			break
		}
		zones, most[i] = r.workColumn(s, fd, cols[i], s.board.scroll[i], i, zones)
	}
	workRows.Lock()
	workRows.max = most
	workRows.Unlock()

	if s.board.detail != "" {
		if fd, it, ok := findItem(v, s.board.detail); ok {
			r.workCard(s, fd, it)
		}
	}
	return zones
}

// findItem is the item with the id, and its source.
func findItem(v workfeed.View, id string) (workfeed.Feed, workfeed.Item, bool) {
	for _, e := range v.Calendar.Events {
		if e.ID == id {
			return v.Calendar, eventItem(e), true
		}
	}
	for _, fd := range v.Feeds {
		for _, it := range fd.Items {
			if it.ID == id {
				return fd, it, true
			}
		}
	}
	return workfeed.Feed{}, workfeed.Item{}, false
}

// workColumn draws one source in box and returns the zones with its items added, and how many rows
// lie below its foot at the top of its scroll.
func (r *renderer) workColumn(s scene, fd workfeed.Feed, box image.Rectangle, scroll, col int, zones []boardZone) ([]boardZone, int) {
	fc := r.faces()
	nameF, chipF := r.textFace(true, 24), r.textFace(false, 15)
	titleF, metaF, kindF := r.textFace(false, 19), r.textFace(false, 14), r.textFace(true, 13)
	rad := r.sf(18)
	r.roundShadow(box, rad, r.sf(14), r.s(4), shadowAlpha()*0.8)
	r.roundFill(box, rad, surface(3), surface(2))
	r.roundHighlight(box, rad)
	pad := r.s(18)
	x0, x1 := box.Min.X+pad, box.Max.X-pad

	// The name, and how fresh it is.
	head := box.Min.Y + r.s(32)
	r.text(nameF, fd.Name, x0, head, cream)
	var state string
	switch {
	case fd.Demo:
		state = "demo"
	case fd.State == workfeed.Ready:
		state = "updated " + ago(s.now, fd.Fetched)
	case fd.State == workfeed.Loading:
		state = "fetching…"
	}
	if state != "" {
		r.rightText(metaF, state, x1, head, dim)
	}

	// Its foot: a demo's word, or the last poll's problem over what it had.
	foot := box.Max.Y - r.s(10)
	listBottom := box.Max.Y - r.s(8)
	note, noteCol := "", dim
	switch {
	case fd.Demo:
		note = "Made-up items · tokens go in work.json"
	case fd.State == workfeed.Ready && fd.Problem != "":
		note, noteCol = fd.Problem, amber
	}
	// The foot is kept for the note and for how many items are below it.
	listBottom = foot - r.s(20)
	footLine := func(more string) {
		room := x1 - x0
		if more != "" {
			r.rightText(metaF, more, x1, foot, dim)
			room -= r.width(metaF, more) + r.s(12)
		}
		if note != "" {
			r.text(metaF, r.fit(metaF, note, room), x0, foot, noteCol)
		}
	}

	switch fd.State {
	case workfeed.Off, workfeed.Loading, workfeed.Failed:
		y := box.Min.Y + r.s(workHeadH) + r.s(60)
		title, text, tcol := "Not set up", fd.Setup, cream
		switch fd.State {
		case workfeed.Loading:
			title, text = "Fetching…", "The first answer is on its way."
		case workfeed.Failed:
			title, text, tcol = "Couldn't fetch", fd.Problem, danger
		}
		r.text(fc.label, title, x0, y, tcol)
		y += r.s(40)
		for _, line := range r.wrapLines(fc.sub, text, x1-x0) {
			r.text(fc.sub, line, x0, y, dim)
			y += r.s(28)
		}
		return zones, 0
	}

	// The counts, as chips, wrapping to a second line when they must.
	cy := box.Min.Y + r.s(workHeadH) + r.s(4)
	cx := x0
	chipH := r.s(24)
	for _, c := range fd.Counts {
		label := fmt.Sprintf("%s %d", c.Label, c.N)
		w := r.width(chipF, label) + r.s(18)
		if cx+w > x1 && cx > x0 {
			cx, cy = x0, cy+chipH+r.s(6)
		}
		b := image.Rect(cx, cy, cx+w, cy+chipH)
		bg, fg := surface(5), dim
		if c.Hot && c.N > 0 {
			bg, fg = amber, onAccent()
		} else if c.N > 0 {
			fg = cream
		}
		r.roundFill(b, float64(chipH)/2, bg, bg)
		r.text(chipF, label, b.Min.X+r.s(9), b.Min.Y+chipH/2+r.s(5), fg)
		cx += w + r.s(6)
	}
	top := cy + chipH + r.s(10)
	r.rule(x0, x1, top-r.s(5), 1)

	if len(fd.Items) == 0 {
		r.text(fc.sub, "Nothing here right now", x0, top+r.s(34), dim)
		footLine("")
		return zones, 0
	}
	rowH := r.s(workRowH)
	fits := max((listBottom-top)/rowH, 1)
	scroll = min(scroll, max(len(fd.Items)-fits, 0))
	for i := scroll; i < len(fd.Items) && i < scroll+fits; i++ {
		it := fd.Items[i]
		y := top + (i-scroll)*rowH
		row := image.Rect(box.Min.X+r.s(6), y, box.Max.X-r.s(6), y+rowH)
		if it.Hot {
			bar := image.Rect(box.Min.X+r.s(6), y+r.s(10), box.Min.X+r.s(10), y+rowH-r.s(10))
			r.roundFill(bar, r.sf(2), amber, amber)
		}
		kind := strings.ToUpper(it.Kind)
		kcol := dim
		if it.Hot {
			kcol = amber
		}
		r.text(kindF, kind, x0, y+r.s(19), kcol)
		age := ago(s.now, it.At)
		r.rightText(metaF, age, x1, y+r.s(19), dim)
		r.text(titleF, r.fit(titleF, it.Title, x1-x0), x0, y+r.s(41), cream)
		meta := it.Where
		if it.Who != "" {
			meta += " · " + it.Who
		}
		kw := r.width(kindF, kind) + r.s(8)
		r.text(metaF, r.fit(metaF, meta, x1-x0-kw-r.width(metaF, age)-r.s(12)), x0+kw, y+r.s(19), lerp(dim, cream, 0.35))
		zones = append(zones, boardZone{r: row, kind: "item", id: it.ID, col: col})
	}
	if hidden := len(fd.Items) - scroll - fits; hidden > 0 {
		footLine(fmt.Sprintf("%d more ↓", hidden))
	} else {
		footLine("")
	}
	return zones, max(len(fd.Items)-fits, 0)
}

// workCard is an item's card over the board: the whole of it, and where to open it.
func (r *renderer) workCard(s scene, fd workfeed.Feed, it workfeed.Item) {
	fc := r.faces()
	r.dimAll(0.6)
	w := min(r.s(720), r.w-2*r.s(boardIn))
	pad := r.s(28)
	inner := w - 2*pad
	title := r.wrapLines(fc.labelBold, it.Title, inner)
	if len(title) > 3 {
		title = title[:3]
	}
	body := r.wrapLines(fc.value, it.Body, inner)
	if len(body) > 8 {
		body = append(body[:8], "…")
	}
	small := r.textFace(false, 15)
	link := r.wrapLines(small, it.URL, inner)
	if len(link) > 2 {
		link = link[:2]
	}
	h := pad + len(title)*r.s(38) + r.s(40) + len(body)*r.s(32) + r.s(18) + len(link)*r.s(22) + r.s(30) + pad
	box := image.Rect((r.w-w)/2, (r.h-h)/2, (r.w+w)/2, (r.h+h)/2)
	rad := r.cardRad()
	r.roundShadow(box, rad, r.sf(26), r.s(8), shadowAlpha()*1.2)
	r.roundFill(box, rad, surface(4), surface(3))
	r.roundHighlight(box, rad)

	y := box.Min.Y + pad + r.s(26)
	for _, line := range title {
		r.text(fc.labelBold, line, box.Min.X+pad, y, cream)
		y += r.s(38)
	}
	meta := fd.Name + " · " + it.Kind
	if it.Where != "" {
		meta += " · " + it.Where
	}
	if it.Who != "" {
		meta += " · " + it.Who
	}
	meta += " · " + ago(s.now, it.At)
	col := dim
	if it.Hot {
		col = amber
	}
	r.text(fc.sub, r.fit(fc.sub, meta, inner), box.Min.X+pad, y, col)
	y += r.s(40)
	for _, line := range body {
		r.text(fc.value, line, box.Min.X+pad, y, cream)
		y += r.s(32)
	}
	y += r.s(18)
	for _, line := range link {
		r.text(small, line, box.Min.X+pad, y, lerp(dim, amber, 0.5))
		y += r.s(22)
	}
	r.text(small, "Tap anywhere to close", box.Min.X+pad, box.Max.Y-pad, dim)
}

// ago is how long before now t was, in a few letters: now, 5m, 3h, 2d.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
