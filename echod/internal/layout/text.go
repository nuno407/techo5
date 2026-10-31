package layout

// DefaultTextSize is how large the screen draws its words until the owner picks a text size, in percent
// of the size its layout gives them. Every board draws them at that size but the Surface RT: the layout
// scales with the panel's width, which on its 10.6" 1366x768 panel makes words half again as tall as the
// Show 8 draws them, and 80% brings them near the Show 8's on the glass while the boxes keep filling the
// screen.
func DefaultTextSize() int {
	if SurfaceRT() {
		return 80
	}
	return 100
}
