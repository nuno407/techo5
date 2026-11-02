//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// The boards: pages of their own over the whole screen, each fed by a feature of its own. Work (GitHub,
// Slack, Jira and the Outlook calendar, feature/workfeed; board_work.go, render_work.go) and Sky (the
// aircraft and the rain around home, feature/skyview; board_sky.go, render_sky.go) are the two there
// are. Each registers itself here (registerBoard) with what the screen asks of it: what a frame of it
// shows, how it is drawn, what a finger on it does, how often it moves. The rest of this file knows
// none of them by name: the tabs along the top, the ×, the edge swipes, the idle screen and its Home
// Assistant select are the same for every board, so a third board is a file pair of its own.
//
// A board is on the screen one of two ways:
//
//   - Opened by hand: a swipe in from the left edge of the clock brings up the board shown last (the
//     idle board, or the first, the first time) when there is no Home Assistant dashboard there to
//     bring up, and so does a swipe up from the bottom edge when there is no deck. The tabs turn between
//     the boards, the × puts them away, and so does "go home" or the Dashboard returns time without a
//     touch.
//   - As the idle screen (Idle screen in Home Assistant, the config's screen.idle_board): the board
//     stands in for the clock whenever the clock would be the page, as the Dashboard's Show when idle
//     does, and it is there again after a restart. Music playing, a dashboard opened or asked for, and
//     everything else that takes the screen come first; the board is back once they are done. The × or
//     the left edge's swipe put it away for a while (dashAway), the clock showing meanwhile, and a
//     board turned to from it by a tab goes back to it after the Dashboard returns time.
//
// While up a board takes every finger, as the deck does; what it does with them is its own.

// boardKind is one board, as it registered itself.
type boardKind struct {
	id    string // what the config and the voice assistant's pages call it ("work")
	label string // its tab, and its choice in Idle screen
	rank  int    // its place among the tabs, the lowest first

	// listen has the board's feature call changed whenever what the board shows has changed.
	listen func(changed func())
	// fill puts what a frame of the board shows in v.data, on the frame's goroutine: asking the feature
	// for it is also how the feature knows somebody is looking.
	fill func(d *Display, v *boardView)
	// draw draws the board over the whole screen and returns where a tap means something.
	draw func(r *renderer, s scene) []boardZone
	// gesture is a finger on the board, past the tabs and the × (boardGesture).
	gesture func(d *Display, g touch.Gesture, at image.Point)
	// key is what a frame of it shows, two frames with the same key looking the same, so the second is
	// not drawn (boardFrameKey); nil for a board that moves, drawn every frame.
	key func(s scene) string
	// next is how long until its next frame; nil for the clock's pace.
	next func(s scene) time.Duration
	// follow is a board that wants every finger as it moves (touch's follow mode): a map to drag.
	follow bool
}

// boardKinds are the boards registered, in their ranks' order.
var boardKinds []boardKind

// registerBoard adds a board, from its own file's init.
func registerBoard(b boardKind) {
	boardKinds = append(boardKinds, b)
	slices.SortStableFunc(boardKinds, func(x, y boardKind) int { return x.rank - y.rank })
}

// boardByID is the board registered under id.
func boardByID(id string) (boardKind, bool) {
	for _, b := range boardKinds {
		if id != "" && b.id == id {
			return b, true
		}
	}
	return boardKind{}, false
}

// boardAfter is the board step tabs on from id, around the end.
func boardAfter(id string, step int) string {
	n := len(boardKinds)
	if n == 0 {
		return ""
	}
	i := slices.IndexFunc(boardKinds, func(b boardKind) bool { return b.id == id })
	return boardKinds[((i+step)%n+n)%n].id
}

// boardState is the boards between frames: which is up, and what a finger has done on it.
type boardState struct {
	up    string // the board opened by hand, or ""
	last  string // the one shown last: what the edge swipes open
	shown string // the board drawn in the last frame, rather than under something else; "" for none
	// awayUntil is the idle board put away by hand: the clock is the page in its place until then.
	awayUntil time.Time
	touched   time.Time

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
	data     any    // the board's own, what its fill put there for its draw
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
// Show whose owner has written either board's settings. Elsewhere the edge swipes stay as they were,
// and Home Assistant has no Idle screen to choose.
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

// idleBoard is the board that is the idle screen, or "" for the clock: a board this build does not
// have, or any on a screen that does not offer them, is the clock.
func idleBoard() string {
	id := config.Get().Screen.IdleBoard
	if _, ok := boardByID(id); !ok || !boardsOffered() {
		return ""
	}
	return id
}

// openBoard puts a board up: the one named, or for "" the one shown last (the idle board, or the first,
// the first time). It reports false on a screen that does not offer them (boardsOffered).
func (d *Display) openBoard(name string) bool {
	if !boardsOffered() || len(boardKinds) == 0 {
		return false
	}
	idle := idleBoard()
	d.mu.Lock()
	if name == "" {
		name = d.boards.last
	}
	if name == "" {
		name = idle
	}
	if _, ok := boardByID(name); !ok {
		name = boardKinds[0].id
	}
	d.boards.up, d.boards.last, d.boards.touched, d.boards.awayUntil = name, name, time.Now(), time.Time{}
	d.boards.detail, d.boards.dragging = "", false
	d.closeAlert()
	d.calUntil, d.weatherUntil = time.Time{}, time.Time{}
	d.sheet, d.dash, d.drawer, d.deckUp = false, false, false, false
	d.mu.Unlock()
	slog.Info("board up", "board", name)
	d.wake()
	return true
}

// closeBoard puts the board away: back to the clock, and when a board is the idle screen, the clock for
// a while (dashAway) before the board comes back.
func (d *Display) closeBoard() {
	idle := idleBoard()
	d.mu.Lock()
	d.boards.up, d.boards.detail, d.boards.dragging = "", "", false
	if idle != "" {
		d.boards.awayUntil = time.Now().Add(dashAway)
	}
	d.mu.Unlock()
	slog.Info("board closed")
	d.wake()
}

// boardShowing is whether a board is on the screen now: drawn in the last frame, and the device idle.
func (d *Display) boardShowing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boards.shown != "" && pageIdle(d.view.Phase)
}

// boardScene fills in the board, when one is up or is the idle screen and nothing that comes before it
// has the screen.
func (d *Display) boardScene(s *scene, now time.Time) {
	over := s.showSheet || s.showDrawer || s.showCamera || s.showWifi || s.showAlert || s.showCalendar ||
		s.showWeather || s.showRadar || s.bt.Pairing || s.showDeck || s.redClock
	idle := idleBoard()
	d.mu.Lock()
	if d.boards.up != "" && dashForgotten(d.boards.touched) {
		d.boards.up = ""
		slog.Info("board closed: not touched for a while")
	}
	name := d.boards.up
	// The idle board stands in for the clock: so not over the music, which the clock gives way to, nor
	// over a dashboard opened by hand or asked for by Home Assistant.
	asIdle := name == "" && idle != "" && !d.dash && !s.nowPlaying && !now.Before(d.boards.awayUntil)
	if asIdle {
		name = idle
	}
	if s.phase != "idle" || over {
		name = ""
	}
	if asIdle && name != "" && name != d.boards.shown {
		slog.Info("board up: the idle screen", "board", name)
	}
	d.boards.shown = name
	if name != "" {
		d.boards.last = name
	}
	v := boardView{name: name, detail: d.boards.detail, scroll: d.boards.scroll, selected: d.boards.selected}
	b, _ := boardByID(name)
	wantFollow := b.follow
	was := d.boards.follow
	d.boards.follow = wantFollow
	d.mu.Unlock()

	// A map wants every finger as it moves. Asked every frame while it is up: the dashboard, going away
	// under it in the frame the board came up, turns follow mode off once as it goes.
	if wantFollow || was {
		touch.Get().SetFollow(wantFollow)
	}
	if name != "" {
		boardsListening.Do(func() {
			redraw := func() {
				if d.boardShowing() {
					d.wake()
				}
			}
			for _, k := range boardKinds {
				if k.listen != nil {
					k.listen(redraw)
				}
			}
		})
		if b.fill != nil {
			b.fill(d, &v)
		}
	}
	s.board = v
}

// boardPage draws the board up.
func (r *renderer) boardPage(s scene) {
	b, ok := boardByID(s.board.name)
	if !ok || b.draw == nil {
		setBoardZones(nil)
		return
	}
	setBoardZones(b.draw(r, s))
}

// boardFrameKey is what a frame of a board shows, or "" when the frame is not just that board, or the
// board moves: two frames with the same key look the same, and the second is not drawn (frame's
// deckDrawn).
func boardFrameKey(s scene, ringing, calling bool) string {
	b, ok := boardByID(s.board.name)
	if !ok || b.key == nil || s.showVideo || s.showVideoAsk || s.showVolume || ringing || calling || s.setupAsking ||
		s.showAnnouncement || s.showReminder || s.showNotification || s.popup != nil || s.pin.open || s.announceRecording ||
		s.updateActive || s.cue != "" {
		return ""
	}
	return fmt.Sprintf("board|%s|%s|%s|%v|%v", b.id, b.key(s), s.board.detail, s.board.scroll,
		[5]any{walnut, amber, cream, dim, drawnFont()})
}

// boardNext is how long until a board's next frame: its own pace, or the clock's. A board's data
// changing wakes the screen itself (boardKind.listen).
func boardNext(s scene) time.Duration {
	if b, ok := boardByID(s.board.name); ok && b.next != nil {
		return b.next(s)
	}
	return time.Until(s.now.Truncate(idleFrame).Add(idleFrame))
}

// boardGesture is a finger on a board: the tabs and the × are every board's, the rest the board's own.
func (d *Display) boardGesture(g touch.Gesture) {
	if d.r == nil {
		return
	}
	d.mu.Lock()
	d.boards.touched = time.Now()
	name := d.boards.shown
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
	if b, ok := boardByID(name); ok && b.gesture != nil {
		b.gesture(d, g, at)
	}
}

// idleBoardOptions are Idle screen's choices: the clock, then the boards.
func idleBoardOptions() []string {
	out := []string{"Clock"}
	for _, b := range boardKinds {
		out = append(out, b.label)
	}
	return out
}

// idleBoardIndex is the idle screen's place among idleBoardOptions.
func idleBoardIndex() int {
	id := idleBoard()
	return slices.IndexFunc(boardKinds, func(b boardKind) bool { return b.id == id }) + 1
}

// setIdleBoard saves the idle screen, i of idleBoardOptions, and shows it at once.
func (d *Display) setIdleBoard(i int) {
	if i < 0 || i > len(boardKinds) {
		return
	}
	id := ""
	if i > 0 {
		id = boardKinds[i-1].id
	}
	if err := config.Set().Screen().IdleBoard(id); err != nil {
		slog.Error("saving the idle screen failed", "err", err)
		return
	}
	d.mu.Lock()
	d.boards.awayUntil = time.Time{}
	if id != "" && d.boards.up == id {
		d.boards.up = "" // opened by hand, it is there for good now
	}
	d.mu.Unlock()
	if d.idleBoardSel != nil {
		d.idleBoardSel.Set(idleBoardOptions()[i])
	}
	slog.Info("setting changed", "setting", "screen_idle_board", "using", idleBoardOptions()[i])
	d.wake()
}

// idleBoardSelect is Idle screen in Home Assistant: the clock, or one of the boards in its place while
// nothing else is on the screen.
func idleBoardSelect(d *Display) *esphome.Select {
	s := &esphome.Select{
		Base: esphome.Base{
			ObjectID: "screen_idle_board",
			Name:     "Idle screen",
			Icon:     "mdi:monitor-dashboard",
			Category: esphome.CategoryConfig,
		},
		Options: idleBoardOptions(),
	}
	s.OnCommand = func(v string) {
		if i := slices.Index(idleBoardOptions(), v); i >= 0 {
			d.setIdleBoard(i)
		}
	}
	return s
}
