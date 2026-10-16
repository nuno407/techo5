//go:build !dot && !spot

package lenscover

import "github.com/HuskerMinion/techo5/echod/internal/layout"

// hasShutter says whether this board has a physical cover over the lens with a switch behind it: the
// Echo Show 8, both generations, and the 2nd gen Show 5 on the mainline kernel option. On the 2nd gen
// Show 8 (athena) and on that Show 5 (GPIO 142 in custom-kernel/cronos's device tree) it is gpio-keys'
// SW_CAMERA_LENS_COVER, read on both units 2026-10-16 on the 7.2.11 kernels: 1 with the slider across
// the lens, 0 open, following every move.
//
// This has to be a board question, not a capability question. On their vendor kernels both Show 5
// generations expose
// SW_CAMERA_LENS_COVER on their gpio-keys node exactly as the Show 8 does — measured on a 2nd gen
// unit 2026-09-22, caps.sw=200, which is bit 9 — while having no shutter at all. The switch read
// false on that unit, so nothing was blocked, but reading the capability bitmap alone would publish
// a cover sensor for hardware that has none, and a unit whose pin read the other way would refuse
// the camera with nothing in the log but "the lens cover is closed".
func hasShutter() bool { return layout.Crown() || layout.Athena() || layout.CronosMainline() }
