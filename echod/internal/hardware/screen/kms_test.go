//go:build !dot && !panel

package screen

import (
	"encoding/binary"
	"image"
	"testing"
)

// The ioctl numbers are built from the structs' sizes as the kernel has them on 32-bit ARM; these are
// libdrm's own values for DRM_IOCTL_MODE_*.
func TestDRMIoctlNumbers(t *testing.T) {
	for _, c := range []struct {
		name string
		nr   uintptr
		size int
		want uintptr
	}{
		{"GETRESOURCES", drmModeGetRes, 64, 0xc04064a0},
		{"SETCRTC", drmModeSetCrtc, 104, 0xc06864a2},
		{"GETCONNECTOR", drmModeGetConn, 80, 0xc05064a7},
		{"GETENCODER", drmModeGetEncoder, 20, 0xc01464a6},
		{"ADDFB", drmModeAddFB, 28, 0xc01c64ae},
		{"PAGE_FLIP", drmModePageFlip, 24, 0xc01864b0},
		{"CREATE_DUMB", drmModeCreateDumb, 32, 0xc02064b2},
		{"MAP_DUMB", drmModeMapDumb, 16, 0xc01064b3},
		{"GETPLANE", drmModeGetPlane, 32, 0xc02064b6},
		{"SETPLANE", drmModeSetPlane, 48, 0xc03064b7},
		{"ADDFB2", drmModeAddFB2, 104, 0xc06864b8},
		{"PRIME_FD_TO_HANDLE", drmPrimeFDToHndl, 12, 0xc00c642e},
	} {
		if got := drmIoc(c.nr, c.size); got != c.want {
			t.Errorf("%s: %#x, want %#x", c.name, got, c.want)
		}
	}
}

// modeinfo packs a struct drm_mode_modeinfo.
func modeinfo(w, h, refresh int, typ uint32, name string) []byte {
	b := make([]byte, modeInfoSize)
	binary.LittleEndian.PutUint16(b[4:], uint16(w))
	binary.LittleEndian.PutUint16(b[14:], uint16(h))
	binary.LittleEndian.PutUint32(b[24:], uint32(refresh))
	binary.LittleEndian.PutUint32(b[32:], typ)
	copy(b[36:], name)
	return b
}

func TestModeIsTheConnectorsPreferred(t *testing.T) {
	modes := []kmsMode{
		parseMode(modeinfo(1024, 768, 60, 0, "1024x768")),
		parseMode(modeinfo(1366, 768, 60, drmModeTypePreferred, "1366x768")),
	}
	m, ok := pickMode(modes)
	if !ok || m.w != 1366 || m.h != 768 || m.refresh != 60 || m.name != "1366x768" {
		t.Fatalf("picked %+v", m)
	}
	// None preferred: the first, which the kernel lists as the native one.
	m, _ = pickMode(modes[:1])
	if m.w != 1024 {
		t.Fatalf("picked %dx%d", m.w, m.h)
	}
	if _, ok := pickMode(nil); ok {
		t.Fatal("a mode from none")
	}
	// The raw struct goes back to the kernel as it came.
	if string(modes[1].raw[36:44]) != "1366x768" {
		t.Fatal("the raw mode was not kept")
	}
}

// event packs a DRM event of type typ, 32 bytes as a vblank or flip event is.
func event(typ uint32) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, typ)
	binary.LittleEndian.PutUint32(b[4:], 32)
	return b
}

func TestAFlipIsDoneOnItsEvent(t *testing.T) {
	const vblank = 1
	if flipDone(event(vblank)) {
		t.Error("a vblank event taken for a flip")
	}
	if !flipDone(append(event(vblank), event(drmEventFlipComplete)...)) {
		t.Error("a flip after a vblank event missed")
	}
	if flipDone([]byte{2, 0, 0}) {
		t.Error("a short read taken for a flip")
	}
	bad := event(vblank)
	binary.LittleEndian.PutUint32(bad[4:], 4) // a length under the header's
	if flipDone(append(bad, event(drmEventFlipComplete)...)) {
		t.Error("read on past a broken event")
	}
}

// On KMS a frame goes to the page not on show, and each page's shadow is what that page holds: two
// frames that differ only in one row each write just that row.
func TestKMSPagesAlternateAndWriteOnlyChangedRows(t *testing.T) {
	const w, h = 64, 8
	d := &Device{pages: 2, panelW: w, panelH: h, line: w * 4, shift: [4]uint{16, 8, 0, 24}}
	d.pageBytes = d.line * h
	pages := [][]byte{make([]byte, d.pageBytes), make([]byte, d.pageBytes)}
	d.shadow = [][]byte{make([]byte, d.pageBytes), make([]byte, d.pageBytes)}
	d.row = make([]byte, w*4)
	d.page = 1
	d.mem = append(pages[0], pages[1]...)
	d.canvas = newCanvas(w, h)

	// Frame 1 onto page 0: row 3 red.
	paint(d, 3, 255)
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 0 {
		t.Fatalf("frame 1 on page %d", d.page)
	}
	if got := d.mem[3*d.line+2]; got != 255 {
		t.Fatalf("page 0 row 3 red is %d", got)
	}
	// Frame 2 onto page 1: page 1 is zeros, so row 3 is written there too.
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 1 || d.mem[d.pageBytes+3*d.line+2] != 255 {
		t.Fatalf("frame 2: page %d, row 3 red %d", d.page, d.mem[d.pageBytes+3*d.line+2])
	}
	// Scribble on page 0 behind the shadow's back: a row the frame does not change is not rewritten.
	d.mem[5*d.line] = 9
	paint(d, 4, 200)
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 0 || d.mem[4*d.line+2] != 200 {
		t.Fatalf("frame 3: page %d, row 4 red %d", d.page, d.mem[4*d.line+2])
	}
	if d.mem[5*d.line] != 9 {
		t.Fatal("an unchanged row was written")
	}
}

// newCanvas is the canvas for a w×h panel: turned where the build's panel is (rotated).
func newCanvas(w, h int) *image.RGBA {
	if rotated {
		return image.NewRGBA(image.Rect(0, 0, h, w))
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

// paint makes what lands on panel row y opaque, red r: a canvas column where the panel is turned.
func paint(d *Device, y int, r uint8) {
	c := d.canvas
	if rotated {
		for cy := range c.Rect.Dy() {
			i := cy*c.Stride + y*4
			c.Pix[i], c.Pix[i+3] = r, 255
		}
		return
	}
	for x := range c.Rect.Dx() {
		i := y*c.Stride + x*4
		c.Pix[i], c.Pix[i+3] = r, 255
	}
}

func TestTheVideoGoesOnTheYUVOverlayAndTheControlsOnTheCursor(t *testing.T) {
	planes := []planeKind{
		{id: 1, typ: 1, formats: []uint32{fourccARGB8888, fourccYUV420}}, // the primary
		{id: 2, typ: planeOverlay, formats: []uint32{fourccARGB8888}},    // no YUV
		{id: 3, typ: planeOverlay, formats: []uint32{fourccARGB8888, fourccYUV420}},
		{id: 4, typ: planeCursor, formats: []uint32{fourccARGB8888, fourccYUV420}},
	}
	if o, c := pickVideoPlanes(planes); o != 3 || c != 4 {
		t.Fatalf("overlay %d cursor %d", o, c)
	}
	if o, _ := pickVideoPlanes(planes[:2]); o != 0 {
		t.Fatalf("an overlay without YUV picked: %d", o)
	}
}

// A GStreamer I420 picture lands in the plane's buffer row by row: Y at pitch, U and V at half of it.
func TestI420IsCopiedAtThePlanesPitch(t *testing.T) {
	const w, h, pitch = 6, 4, 128
	src := make([]byte, I420Size(w, h))
	for i := range src {
		src[i] = byte(i)
	}
	dst := make([]byte, pitch*h*3/2)
	copyI420(dst, pitch, src, w, h)
	ys, cs := 8, 4 // 6 rounded to 4 bytes; 3 rounded to 4
	for y := range h {
		for x := range w {
			if dst[y*pitch+x] != src[y*ys+x] {
				t.Fatalf("Y %d,%d", x, y)
			}
		}
	}
	for p := range 2 {
		for y := range h / 2 {
			for x := range w / 2 {
				d := pitch*h + p*pitch/2*h/2 + y*pitch/2 + x
				s := ys*h + p*cs*h/2 + y*cs + x
				if dst[d] != src[s] {
					t.Fatalf("plane %d %d,%d: %d, want %d", p+1, x, y, dst[d], src[s])
				}
			}
		}
	}
}

// On KMS with XBGR pages a row goes as it is, and only a row whose checksum differs from what that
// page holds is written: each page keeps its own.
func TestKMSRowSumsWriteOnlyChangedRowsAsTheyAre(t *testing.T) {
	if rotated {
		t.Skip("the Surface RT's panel is not turned")
	}
	const w, h = 64, 80
	d := &Device{pages: 2, panelW: w, panelH: h, line: w*4 + 32, shift: [4]uint{0, 8, 16, 24}, rowSum: true}
	d.pageBytes = d.line * h
	d.page = 1
	d.mem = make([]byte, 2*d.pageBytes)
	d.canvas = newCanvas(w, h)

	paint(d, 3, 255)
	paint(d, 70, 7) // in the other core's half
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 0 || d.mem[3*d.line] != 255 || d.mem[3*d.line+3] != 255 || d.mem[70*d.line] != 7 {
		t.Fatalf("frame 1: page %d, row 3 %v", d.page, d.mem[3*d.line:3*d.line+4])
	}
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 1 || d.mem[d.pageBytes+3*d.line] != 255 || d.mem[d.pageBytes+70*d.line] != 7 {
		t.Fatal("frame 2 did not write page 1's first frame whole")
	}
	// Behind the sums' back: a row the frame does not change is not rewritten, one it does is.
	d.mem[5*d.line] = 9
	d.mem[71*d.line] = 9
	paint(d, 4, 200)
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.page != 0 || d.mem[4*d.line] != 200 || d.mem[5*d.line] != 9 || d.mem[71*d.line] != 9 {
		t.Fatalf("frame 3: page %d, row 4 %d, row 5 %d", d.page, d.mem[4*d.line], d.mem[5*d.line])
	}
	// A frame painted over a page (a video's) makes the next present write it whole.
	d.markStale(1)
	d.mem[d.pageBytes+6*d.line] = 9
	if err := d.present(d.canvas); err != nil {
		t.Fatal(err)
	}
	if d.mem[d.pageBytes+6*d.line] != 0 {
		t.Fatal("a stale page was not written whole")
	}
}
