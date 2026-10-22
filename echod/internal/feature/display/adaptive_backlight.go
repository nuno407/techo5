//go:build !dot && !spot

package display

import (
	"log/slog"
	"sync"

	esphome "github.com/ygelfand/go-esphome-device"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/gpu"
)

// Adaptive backlight: on dark content the backlight goes down and the pixels are drawn brighter by as
// much, so the screen looks the same for less light (screen's lift). Off by default. Offered only where
// techo5-gpu can drive the panel (liftOffered): elsewhere there is nothing to draw the pixels brighter.

// liftOffered is whether the GPU helper is there to draw the panel, looked for once. A variable for
// tests.
var liftOffered = sync.OnceValue(gpu.Available)

func adaptiveBacklightSwitch(d *Display) *esphome.Switch {
	s := &esphome.Switch{
		Base: esphome.Base{
			ObjectID: "screen_adaptive_backlight",
			Name:     "Adaptive backlight",
			Icon:     "mdi:brightness-auto",
			Category: esphome.CategoryConfig,
		},
	}
	s.OnCommand = d.setAdaptiveBacklight
	return s
}

// setAdaptiveBacklight saves the setting, tells Home Assistant and has the next frame take it up.
func (d *Display) setAdaptiveBacklight(on bool) {
	if err := config.Set().Screen().AdaptiveBacklight(on); err != nil {
		slog.Error("saving a setting failed", "setting", "screen_adaptive_backlight", "err", err)
		on = config.Get().Screen.AdaptiveBacklight
	} else {
		slog.Info("setting changed", "setting", "screen_adaptive_backlight", "using", on)
	}
	if d.liftSw != nil {
		d.liftSw.Set(on)
	}
	d.wake()
}
