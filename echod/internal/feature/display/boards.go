//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// The boards: Work (GitHub, Slack and Jira, feature/workfeed; render_work.go) and Sky (the aircraft and
// the rain around home, feature/skyview; render_sky.go), each a page of its own over the whole screen.
// A swipe in from the left edge of the clock brings them up when there is no Home Assistant dashboard
// there to bring up, and so does a swipe up from the bottom edge when there is no deck; the tabs along
// the top turn between the two, the × puts them away, and so does "go home" or the Dashboard returns
// time without a touch. While up they take every finger, as the deck does: on Work a tap opens an item's
// card and a swipe up or down scrolls the column under it, on Sky a finger drags the map, a double tap
// zooms in where it lands and the buttons at the right zoom and go back home.

const (
	boardWork = "work"
	boardSky  = "sky"

	// doubleTap is how soon a second tap has to come, and how near, to be a double tap on the map.
	doubleTap     = 350 * time.Millisecond
	doubleTapNear = 40
)

// boardState is the boards between frames: which is up, and what a finger has done on it.
type boardState struct {
	up       string // boardWork, boardSky or ""
	last     string // the one up last: what the edge swipes open
	onScreen bool   // drawn in the last frame, rather than under something else
	touched  time.Time

	// Work: the item whose card is open, and each column's scroll in rows.
	detail string
	scroll [3]int

	// Sky: the finger dragging the map, the last tap (for a double tap), the aircraft picked.
	dragAt   image.Point
	dragging bool
	tapAt    image.Point
	tapTime  time.Time
	selected string
	follow   bool // touch's follow mode asked for, for the map's drags
}

// boardView is what a frame of the boards shows (boardScene fills it in).
type boardView struct {
	name     string // "" when no board is up
	work     workfeed.View
	sky      skyview.View
	detail   string
	scroll   [3]int
	selected string
}

// boardZone is a place on the board a tap means something: a tab, the ×, an item, a map button, an
// aircraft.
type boardZone struct {
	r    image.Rectangle
	kind string // "tab", "close", "item", "zoomin", "zoomout", "home", "plane", "card"
	id   string // the tab's board, the item's or the aircraft's id
	col  int    // the item's column
}

// boardHits are the zones of the board last drawn, read by the touch goroutine.
var boardHits struct {
	sync.Mutex
	zones []boardZone
}

func setBoardZones(z []boardZone) {
	boardHits.Lock()
	boardHits.zones = z
	boardHits.Unlock()
}

// boardZoneAt is the zone under p, the last drawn (topmost) first.
func boardZoneAt(p image.Point) (boardZone, bool) {
	boardHits.Lock()
	defer boardHits.Unlock()
	for i := len(boardHits.zones) - 1; i >= 0; i-- {
		if p.In(boardHits.zones[i].r) {
			return boardHits.zones[i], true
		}
	}
	return boardZone{}, false
}

// boardsListening is the boards' data waking the screen while a board is on it, once.
var boardsListening sync.Once

// boardsOffered is whether this screen has the boards: the Surface RT, which they were made for, or a
// Show whose owner has written either board's settings. Elsewhere the edge swipes stay as they were.
func boardsOffered() bool {
	if layout.SurfaceRT() {
		return true
	}
	for _, p := range []string{workfeed.SettingsPath(), skyview.SettingsPath()} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// openBoard puts a board up: the one named, or for "" the one up last (Work the first time). It
// reports false on a screen that does not offer them (boardsOffered).
func (d *Display) openBoard(name string) bool {
	if !boardsOffered() {
		return false
	}
	boardsListening.Do(func() {
		redraw := func(struct{}) {
			if d.boardShowing() {
				d.wake()
			}
		}
		workfeed.Get().Changed.Listen(redraw)
		skyview.Get().Changed.Listen(redraw)
	})
	d.mu.Lock()
	if name == "" {
		name = d.boards.last
	}
	if name != boardSky {
		name = boardWork
	}
	d.boards.up, d.boards.last, d.boards.onScreen, d.boards.touched = name, name, true, time.Now()
	d.boards.detail, d.boards.dragging = "", false
	d.closeAlert()
	d.calUntil, d.weatherUntil = time.Time{}, time.Time{}
	d.sheet, d.dash, d.drawer, d.deckUp = false, false, false, false
	d.mu.Unlock()
	slog.Info("board up", "board", name)
	d.wake()
	return true
}

func (d *Display) closeBoard() {
	d.mu.Lock()
	d.boards.up, d.boards.detail, d.boards.dragging = "", "", false
	d.mu.Unlock()
	slog.Info("board closed")
	d.wake()
}

// boardShowing is whether a board is on the screen now: up, and drawn in the last frame.
func (d *Display) boardShowing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boards.up != "" && d.boards.onScreen && pageIdle(d.view.Phase)
}

// boardScene fills in the board, when one is up and nothing that comes before it has the screen.
func (d *Display) boardScene(s *scene, now time.Time) {
	over := s.showSheet || s.showDrawer || s.showCamera || s.showWifi || s.showAlert || s.showCalendar ||
		s.showWeather || s.showRadar || s.bt.Pairing || s.showDeck || s.redClock
	d.mu.Lock()
	if d.boards.up != "" && dashForgotten(d.boards.touched) {
		d.boards.up = ""
		slog.Info("board closed: not touched for a while")
	}
	up := d.boards.up != "" && s.phase == "idle" && !over
	d.boards.onScreen = up
	name := ""
	if up {
		name = d.boards.up
	}
	v := boardView{name: name, detail: d.boards.detail, scroll: d.boards.scroll, selected: d.boards.selected}
	wantFollow := name == boardSky
	was := d.boards.follow
	d.boards.follow = wantFollow
	d.mu.Unlock()

	// The map wants every finger as it moves. Asked every frame while it is up: the dashboard, going
	// away under it in the frame the board came up, turns follow mode off once as it goes.
	if wantFollow || was {
		touch.Get().SetFollow(wantFollow)
	}
	switch name {
	case boardWork:
		workfeed.Get().Looked()
		v.work = workfeed.Get().View()
	case boardSky:
		if d.r != nil {
			skyview.Get().Looked(d.r.w, d.r.h)
			v.sky = skyview.Get().View(d.r.w, d.r.h)
		}
	}
	s.board = v
}

// boardFrameKey is what a frame of the Work board shows, or "" when the frame is not just that board:
// two frames with the same key look the same, and the second is not drawn (frame's deckDrawn). The Sky
// board moves every second, so it is always drawn.
func boardFrameKey(s scene, ringing, calling bool) string {
	if s.board.name != boardWork || s.showVideo || s.showVideoAsk || s.showVolume || ringing || calling || s.setupAsking ||
		s.showAnnouncement || s.showReminder || s.showNotification || s.popup != nil || s.pin.open || s.announceRecording ||
		s.updateActive || s.cue != "" {
		return ""
	}
	// The ages on the items change by the minute.
	return fmt.Sprintf("board|%+v|%s|%v|%s|%v", s.board.work, s.board.detail, s.board.scroll, s.now.Format("15:04"),
		[5]any{walnut, amber, cream, dim, drawnFont()})
}

// boardNext is how long until a board's next frame: the aircraft glide along their tracks a second at a
// time; Work changes when its data does, which wakes the screen itself.
func boardNext(s scene) time.Duration {
	if s.board.name == boardSky {
		return time.Second
	}
	return time.Until(s.now.Truncate(idleFrame).Add(idleFrame))
}

// boardGesture is a finger on a board.
func (d *Display) boardGesture(g touch.Gesture) {
	if d.r == nil {
		return
	}
	d.mu.Lock()
	d.boards.touched = time.Now()
	name := d.boards.up
	d.mu.Unlock()
	at := image.Pt(g.X, g.Y)

	if g.Kind == touch.Tap {
		if z, ok := boardZoneAt(at); ok {
			switch z.kind {
			case "tab":
				if z.id != name {
					d.openBoard(z.id)
				}
				return
			case "close":
				d.closeBoard()
				return
			}
		}
	}
	if name == boardSky {
		d.skyGesture(g, at)
		return
	}
	d.workGesture(g, at)
}

// workGesture: a tap on an item opens its card, a tap on an open card closes it, a swipe up or down
// scrolls the column it starts in, and a swipe across turns to the Sky board (from the left edge, it
// puts the board away, as it opened it).
func (d *Display) workGesture(g touch.Gesture, at image.Point) {
	switch g.Kind {
	case touch.Tap:
		d.mu.Lock()
		open := d.boards.detail != ""
		d.mu.Unlock()
		if open {
			d.mu.Lock()
			d.boards.detail = ""
			d.mu.Unlock()
			return
		}
		if z, ok := boardZoneAt(at); ok && z.kind == "item" {
			d.mu.Lock()
			d.boards.detail = z.id
			d.mu.Unlock()
		}
	case touch.SwipeUp, touch.SwipeDown:
		// A row a notch: a finger moving up brings the rows below into view.
		col := workColumnAt(d.r.w, g.X)
		by := 1
		if g.Kind == touch.SwipeDown {
			by = -1
		}
		d.mu.Lock()
		d.boards.scroll[col] = min(max(d.boards.scroll[col]+by, 0), workScrollMax(col))
		d.mu.Unlock()
	case touch.SwipeLeft:
		d.openBoard(boardSky)
	case touch.SwipeRight:
		if g.X < d.r.drawerEdge() {
			d.closeBoard()
		}
	}
}

// skyGesture: a finger moving drags the map (follow mode reports it as Hold, Drag and Release); a tap
// on a button does what it says, on an aircraft picks it (or lets it go), and two taps in a moment
// zoom in where they land.
func (d *Display) skyGesture(g touch.Gesture, at image.Point) {
	sky := skyview.Get()
	switch g.Kind {
	case touch.Hold:
		d.mu.Lock()
		d.boards.dragAt, d.boards.dragging = at, true
		d.mu.Unlock()
	case touch.Drag:
		d.mu.Lock()
		from, dragging := d.boards.dragAt, d.boards.dragging
		d.boards.dragAt = at
		d.mu.Unlock()
		if dragging {
			sky.Pan(at.X-from.X, at.Y-from.Y)
			d.wake()
		}
	case touch.Release:
		d.mu.Lock()
		if d.boards.dragging {
			sky.Pan(at.X-d.boards.dragAt.X, at.Y-d.boards.dragAt.Y)
		}
		d.boards.dragging = false
		d.mu.Unlock()
	case touch.Tap:
		z, hit := boardZoneAt(at)
		if hit {
			switch z.kind {
			case "zoomin":
				sky.ZoomAt(+1, image.Pt(d.r.w/2, d.r.h/2))
				return
			case "zoomout":
				sky.ZoomAt(-1, image.Pt(d.r.w/2, d.r.h/2))
				return
			case "home":
				sky.Home()
				return
			case "card":
				d.mu.Lock()
				d.boards.selected = ""
				d.mu.Unlock()
				return
			}
		}
		d.mu.Lock()
		double := time.Since(d.boards.tapTime) < doubleTap &&
			math.Hypot(float64(at.X-d.boards.tapAt.X), float64(at.Y-d.boards.tapAt.Y)) < float64(d.r.s(doubleTapNear))
		d.boards.tapAt, d.boards.tapTime = at, time.Now()
		if double {
			d.boards.tapTime = time.Time{}
		}
		switch {
		case double:
		case hit && z.kind == "plane":
			if d.boards.selected == z.id {
				d.boards.selected = ""
			} else {
				d.boards.selected = z.id
			}
		default:
			d.boards.selected = ""
		}
		d.mu.Unlock()
		if double {
			sky.ZoomAt(+1, at)
		}
	}
}
