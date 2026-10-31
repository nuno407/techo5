//go:build !dot && !spot

package screen

import "github.com/HuskerMinion/techo5/echod/internal/layout"

// rotated: the Echo Show 5's panel is portrait and the device sits landscape. The Skylight's
// 1920×1080 panel and the Surface RT's 1366×768 one are landscape already and are drawn as they are.
var rotated = !layout.Skylight() && !layout.SurfaceRT()
