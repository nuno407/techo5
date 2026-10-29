//go:build !dot

package display

import (
	"image"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/feature/video"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/screen"
)

// A row of a board's own is tapped through its handler, and only its own.
func TestABoardsRowTaps(t *testing.T) {
	saved := rowTaps
	t.Cleanup(func() { rowTaps = saved })
	var tapped []string
	rowTaps = []func(*Display, string, part, int) bool{
		func(_ *Display, id string, _ part, _ int) bool {
			tapped = append(tapped, "first:"+id)
			return id == "mine"
		},
		func(_ *Display, id string, _ part, _ int) bool { tapped = append(tapped, "second:"+id); return false },
	}
	d := &Display{}
	if !d.boardRowTap("mine", 0, 0) || len(tapped) != 1 {
		t.Fatalf("tapped %v", tapped)
	}
	if d.boardRowTap("other", 0, 0) || len(tapped) != 3 {
		t.Fatalf("tapped %v", tapped)
	}
}

// A frame in a decoder's own form goes to its presenter; panel pixels, and any frame with no presenter
// registered, are presented as pixels.
func TestVideoFramesGoToTheirPresenter(t *testing.T) {
	saved := presentYUV
	t.Cleanup(func() { presentYUV = saved })
	var presented *video.Frame
	presentYUV = func(_ *screen.Device, fr *video.Frame, _ image.Rectangle) error { presented = fr; return nil }
	fr := &video.Frame{YUV: &video.YUVFrames{W: 2, H: 2}}
	if err := presentFrame(nil, fr, image.Rectangle{}); err != nil || presented != fr {
		t.Fatalf("%v, presented %v", err, presented)
	}
}
