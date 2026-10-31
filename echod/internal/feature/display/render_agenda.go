//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
)

// The Work board's calendar: a strip across the top, over the columns. At its left the meeting under way
// (in the accent, with when it ends) or how long the owner is free; then the next meeting and how soon
// it starts; then the rest of the day as chips, and how many there are tomorrow. A tap on a meeting opens
// its card, as an item's does. Not set up, the strip is a line saying what to add; waiting for a sign-in,
// it is the code and where to enter it, large enough to read from across the room.

const (
	agendaH     = 84 // the strip, with meetings or a sign-in
	agendaSlimH = 40 // the strip with only a line to say
)

// agendaStrip draws the calendar from top and returns where the columns start, and the zones with the
// meetings added.
func (r *renderer) agendaStrip(s scene, cal workfeed.Feed, top int, zones []boardZone) (int, []boardZone) {
	m := r.s(boardIn)
	h := r.s(agendaH)
	slim := cal.State == workfeed.Off || cal.State == workfeed.Loading || cal.State == workfeed.Failed
	if slim {
		h = r.s(agendaSlimH)
	}
	box := image.Rect(m, top, r.w-m, top+h)
	rad := r.sf(16)
	r.roundShadow(box, rad, r.sf(12), r.s(3), shadowAlpha()*0.7)
	r.roundFill(box, rad, surface(3), surface(2))
	r.roundHighlight(box, rad)
	pad := r.s(16)
	x0, x1 := box.Min.X+pad, box.Max.X-pad
	nameF, lineF, smallF := r.textFace(true, 17), r.textFace(false, 16), r.textFace(false, 13)
	next := box.Max.Y + r.s(8)

	if slim {
		y := box.Min.Y + h/2 + r.s(6)
		r.text(nameF, "Calendar", x0, y, cream)
		x := x0 + r.width(nameF, "Calendar") + r.s(14)
		line, col := cal.Setup, dim
		switch cal.State {
		case workfeed.Off:
			line = "Not set up · " + cal.Setup
		case workfeed.Loading:
			line = "Fetching today's meetings…"
		case workfeed.Failed:
			line, col = "Couldn't fetch · "+cal.Problem, danger
		}
		r.text(lineF, r.fit(lineF, line, x1-x), x, y, col)
		return next, zones
	}

	if cal.State == workfeed.SigningIn {
		si := cal.SignIn
		if si == nil || si.Code == "" {
			r.text(nameF, "Outlook", x0, box.Min.Y+r.s(34), cream)
			r.text(lineF, "Asking Microsoft for a sign-in code…", x0, box.Min.Y+r.s(62), dim)
			return next, zones
		}
		where := strings.TrimPrefix(strings.TrimPrefix(si.URL, "https://"), "http://")
		r.text(nameF, "Sign in to Outlook", x0, box.Min.Y+r.s(30), cream)
		r.text(lineF, "On your phone, open "+where+" and enter", x0, box.Min.Y+r.s(56), dim)
		left := fmt.Sprintf("this code works for %d more minutes", max(int(si.Expires.Sub(s.now).Minutes()), 0))
		r.text(smallF, left, x0, box.Min.Y+r.s(76), dim)
		codeF := r.textFace(true, 40)
		r.rightText(codeF, si.Code, x1, box.Min.Y+h/2+r.s(14), amber)
		return next, zones
	}

	cur, upcoming := workfeed.NowAndNext(cal.Events, s.now)
	third := (x1 - x0) / 3
	block := func(x, w int, label string, labelHot bool, e *workfeed.Event, idle string) {
		col := dim
		if labelHot {
			col = amber
		}
		kindF := r.textFace(true, 13)
		r.text(kindF, r.fit(kindF, label, w-r.s(12)), x, box.Min.Y+r.s(24), col)
		if e == nil {
			r.text(lineF, r.fit(lineF, idle, w-r.s(12)), x, box.Min.Y+r.s(50), lerp(dim, cream, 0.4))
			return
		}
		r.text(nameF, r.fit(nameF, e.Title, w-r.s(12)), x, box.Min.Y+r.s(50), cream)
		r.text(smallF, r.fit(smallF, eventWhere(*e), w-r.s(12)), x, box.Min.Y+r.s(72), lerp(dim, cream, 0.35))
		zones = append(zones, boardZone{r: image.Rect(x-r.s(6), box.Min.Y, x+w-r.s(6), box.Max.Y), kind: "item", id: e.ID})
	}

	// Now: the meeting under way, or the time free until the next.
	switch {
	case cur != nil:
		bar := image.Rect(box.Min.X+r.s(6), box.Min.Y+r.s(12), box.Min.X+r.s(10), box.Max.Y-r.s(12))
		r.roundFill(bar, r.sf(2), amber, amber)
		block(x0, third, "NOW · until "+clockText(cur.End), true, cur, "")
	case upcoming != nil && sameDay(upcoming.Start, s.now):
		block(x0, third, "NOW", false, nil, "Free until "+clockText(upcoming.Start))
	default:
		block(x0, third, "NOW", false, nil, "No more meetings today")
	}
	// Next, and how soon.
	if upcoming != nil {
		label := "NEXT · " + untilText(s.now, upcoming.Start) + " · " + clockHM(upcoming.Start) + "–" + clockText(upcoming.End)
		if !sameDay(upcoming.Start, s.now) {
			label = "NEXT · " + upcoming.Start.Format("Mon") + " " + clockText(upcoming.Start)
		}
		block(x0+third, third, label, upcoming.Start.Sub(s.now) <= 10*time.Minute, upcoming, "")
	} else {
		block(x0+third, third, "NEXT", false, nil, "Nothing on the calendar")
	}
	for _, sx := range []int{x0 + third - r.s(14), x0 + 2*third - r.s(14)} {
		r.aaLine(float64(sx), float64(box.Min.Y+r.s(14)), float64(sx), float64(box.Max.Y-r.s(14)), r.sf(1), ember)
	}

	// The rest of today as chips; the all-day ones and tomorrow's count beside the heading.
	x := x0 + 2*third
	r.text(r.textFace(true, 13), "LATER", x, box.Min.Y+r.s(24), dim)
	chipF := r.textFace(false, 14)
	cy, chipH := box.Min.Y+r.s(31), r.s(20)
	cx := x
	tomorrow := 0
	var later []workfeed.Event
	var allDay []string
	for _, e := range cal.Events {
		switch {
		case e.AllDay && !e.End.After(s.now):
		case e.AllDay && sameDay(e.Start, s.now):
			allDay = append(allDay, e.Title)
		case !sameDay(e.Start, s.now) && e.Start.After(s.now) && sameDay(e.Start, s.now.AddDate(0, 0, 1)):
			tomorrow++
		case sameDay(e.Start, s.now) && e.Start.After(s.now) && (upcoming == nil || e.ID != upcoming.ID):
			later = append(later, e)
		}
	}
	shown := 0
	for _, e := range later {
		label := clockHM(e.Start) + " " + e.Title
		w := min(r.width(chipF, label)+r.s(16), x1-x)
		if cx+w > x1 && cx > x {
			cx, cy = x, cy+chipH+r.s(4)
		}
		if cy+chipH > box.Max.Y-r.s(6) {
			break
		}
		b := image.Rect(cx, cy, cx+w, cy+chipH)
		bg, fg := surface(5), cream
		if e.Free || e.Tentative {
			fg = dim
		}
		r.roundFill(b, float64(chipH)/2, bg, bg)
		r.text(chipF, r.fit(chipF, label, w-r.s(16)), b.Min.X+r.s(8), b.Min.Y+chipH/2+r.s(5), fg)
		zones = append(zones, boardZone{r: b, kind: "item", id: e.ID})
		cx += w + r.s(6)
		shown++
	}
	if shown == 0 && len(later) == 0 {
		r.text(lineF, "Nothing else today", x, box.Min.Y+r.s(50), lerp(dim, cream, 0.4))
	}
	var right []string
	if len(allDay) > 0 {
		right = append(right, "all day: "+strings.Join(allDay, ", "))
	}
	switch {
	case tomorrow == 1:
		right = append(right, "tomorrow 1 meeting")
	case tomorrow > 1:
		right = append(right, fmt.Sprintf("tomorrow %d meetings", tomorrow))
	}
	col := dim
	switch {
	case cal.Demo:
		right = append(right, "made up")
	case cal.Problem != "":
		right, col = append(right, cal.Problem), amber
	}
	if len(right) > 0 {
		t := r.fit(smallF, strings.Join(right, " · "), x1-x-r.s(70))
		r.rightText(smallF, t, x1, box.Min.Y+r.s(24), col)
	}
	return next, zones
}

// eventWhere is a meeting's time and place in a line: 11:00–11:45 · Teams · Room 2.14.
func eventWhere(e workfeed.Event) string {
	parts := []string{clockHM(e.Start) + "–" + clockText(e.End)}
	if e.Online {
		parts = append(parts, "Teams")
	}
	if e.Location != "" && !strings.Contains(e.Location, "Microsoft Teams") {
		parts = append(parts, e.Location)
	}
	if e.Tentative {
		parts = append(parts, "tentative")
	}
	return strings.Join(parts, " · ")
}

// eventItem is a meeting as the item card shows one.
func eventItem(e workfeed.Event) workfeed.Item {
	body := eventWhere(e)
	if e.AllDay {
		body = "All day"
	}
	if e.Organizer != "" {
		body += " · organized by " + e.Organizer
	}
	link := e.JoinURL
	if link == "" {
		link = e.URL
	}
	return workfeed.Item{ID: e.ID, Kind: "Meeting", Title: e.Title, Where: e.Location, Who: e.Organizer, At: e.Start, Body: body, URL: link}
}

// untilText is how soon t is from now: in 5 min, in 2 h 10 min, now.
func untilText(now, t time.Time) string {
	d := t.Sub(now).Round(time.Minute)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %d min", int(d/time.Minute))
	case d%time.Hour == 0:
		return fmt.Sprintf("in %d h", int(d/time.Hour))
	}
	return fmt.Sprintf("in %d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
