//go:build !dot && !spot

package display

import (
	"image"
	"math"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
)

// The Sky board, as boards.go knows it: feature/skyview's View for a frame, render_sky.go's page, and
// what a finger on it does. The aircraft glide along their tracks, so it is drawn a second at a time,
// and the map wants every finger as it moves, to be dragged.

const (
	boardSky = "sky"

	// doubleTap is how soon a second tap has to come, and how near, to be a double tap on the map.
	doubleTap     = 350 * time.Millisecond
	doubleTapNear = 40
)

func init() {
	registerBoard(boardKind{
		id: boardSky, label: "Sky", rank: 20,
		listen: func(changed func()) { skyview.Get().Changed.Listen(func(struct{}) { changed() }) },
		fill: func(d *Display, v *boardView) {
			if d.r != nil {
				skyview.Get().Looked(d.r.w, d.r.h)
				v.data = skyview.Get().View(d.r.w, d.r.h)
			}
		},
		draw:    func(r *renderer, s scene) []boardZone { return r.skyPage(s) },
		gesture: (*Display).skyGesture,
		next:    func(scene) time.Duration { return time.Second },
		follow:  true,
	})
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
