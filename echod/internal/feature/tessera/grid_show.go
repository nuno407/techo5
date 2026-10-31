//go:build !dot && !spot && !panel

package tessera

import "github.com/HuskerMinion/techo5/echod/internal/layout"

// screenGrid is the grid of this Show's screen, known from its board. The Surface RT's 1366 by 768 is
// laid out as any screen of that size would be: five by three, as the Skylight's.
func screenGrid() grid {
	if layout.SurfaceRT() {
		return gridForSize(1366, 768)
	}
	return gridFor(layout.Show8(), layout.Skylight())
}
