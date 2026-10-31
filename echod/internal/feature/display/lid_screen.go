//go:build !dot && !spot

package display

import (
	"log/slog"
	"sync"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/swinput"
)

// The cover over the screen (the Surface RT's Type Cover, hardware/swinput): closed, the screen goes out
// and nothing is drawn; opened, it comes back as it was. The kernel's backlight is put out by the
// image's own srt-lid-blank as well; this stops the frames behind it.

var lidState struct {
	once sync.Once
	dark bool // the screen was put out by the cover
}

// lidTick puts the screen out while the cover is closed and back on when it opens, if it was the cover
// that put it out. It says whether it changed the screen.
func (d *Display) lidTick(on bool) bool {
	lidState.once.Do(func() {
		swinput.Lid.Changed.Listen(func(bool) { d.wake() })
	})
	if !swinput.Lid.Here() {
		return false
	}
	switch closed := swinput.Lid.On(); {
	case closed && on:
		slog.Info("screen: the cover closed, off")
		lidState.dark = true
		d.apply(false, d.ceilingOrDefault(), false)
		return true
	case !closed && lidState.dark:
		lidState.dark = false
		if !on {
			slog.Info("screen: the cover opened, on again")
			d.apply(true, d.ceilingOrDefault(), false)
			return true
		}
	}
	return false
}
