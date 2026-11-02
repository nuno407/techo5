//go:build !dot && !spot

package display

import (
	"fmt"
	"image"

	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
)

// The Work board, as boards.go knows it: feature/workfeed's View for a frame, render_work.go's page,
// and what a finger on it does. It is still between changes, so a frame the same as the last is not
// drawn again; the ages on its items change by the minute, which its key has.

const boardWork = "work"

func init() {
	registerBoard(boardKind{
		id: boardWork, label: "Work", rank: 10,
		listen: func(changed func()) { workfeed.Get().Changed.Listen(func(struct{}) { changed() }) },
		fill: func(_ *Display, v *boardView) {
			workfeed.Get().Looked()
			v.data = workfeed.Get().View()
		},
		draw:    func(r *renderer, s scene) []boardZone { return r.workPage(s) },
		gesture: (*Display).workGesture,
		key: func(s scene) string {
			return fmt.Sprintf("%+v|%s", workView(s), s.now.Format("15:04"))
		},
	})
}

// workView is the Work board's View in the frame, the empty one in a frame that is not Work's.
func workView(s scene) workfeed.View {
	v, _ := s.board.data.(workfeed.View)
	return v
}

// workGesture: a tap on an item opens its card, a tap on an open card closes it, a swipe up or down
// scrolls the column it starts in, and a swipe left turns to the next board (from the left edge, it
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
		d.openBoard(boardAfter(boardWork, +1))
	case touch.SwipeRight:
		if g.X < d.r.drawerEdge() {
			d.closeBoard()
		}
	}
}
