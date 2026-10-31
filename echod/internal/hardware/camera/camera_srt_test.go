//go:build !dot && !spot && !panel

package camera

import (
	"image/png"
	"os"
	"testing"
)

// tegraWord is how the Tegra 3's VI stores a 10-bit pixel in a 16-bit word.
func tegraWord(v uint16) uint16 { return (v>>3)<<8 | (v&7)<<4 | v>>6 }

// A VI frame repacks into the MediaTek layout pixel for pixel; shifted, each line starts a pixel in and
// its last pixel repeats the one two before it.
func TestTegraFramesRepackToPacked10Bit(t *testing.T) {
	const w, h = 8, 2
	src := make([]byte, w*h*2)
	want := make([]uint16, w*h)
	for i := range want {
		want[i] = uint16(i*131+7) % 1024
		v := tegraWord(want[i])
		src[2*i], src[2*i+1] = byte(v), byte(v>>8)
	}
	dst := make([]byte, w*10/8*h)
	got := make([]uint16, w)
	repackTegra(dst, src, w, h, false)
	for y := range h {
		unpackLine(dst[y*w*10/8:(y+1)*w*10/8], got)
		for x := range w {
			if got[x] != want[y*w+x] {
				t.Fatalf("pixel %d,%d: %d, want %d", x, y, got[x], want[y*w+x])
			}
		}
	}
	repackTegra(dst, src, w, h, true)
	unpackLine(dst[:w*10/8], got)
	for x := range w - 1 {
		if got[x] != want[x+1] {
			t.Fatalf("shifted pixel %d: %d, want %d", x, got[x], want[x+1])
		}
	}
	if got[w-1] != want[w-2] {
		t.Fatalf("the last shifted pixel is %d, want %d", got[w-1], want[w-2])
	}
}

// TECHO5_SRT_RAW=frame.raw (one 1280x720 16-bit frame off /dev/video1, both flips on, 'RG10') develops
// it through the shared code into TECHO5_SRT_PNG, to look at.
func TestDevelopASurfaceRTFrame(t *testing.T) {
	path := os.Getenv("TECHO5_SRT_RAW")
	if path == "" {
		t.Skip("no TECHO5_SRT_RAW")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := [7]any{sensor, sensorW, sensorH, bytesPerLine, frameBytes, Width, Height}
	defer func() {
		sensor, sensorW, sensorH, bytesPerLine, frameBytes = saved[0].(sensorSpec), saved[1].(int), saved[2].(int), saved[3].(int), saved[4].(int)
		Width, Height = saved[5].(int), saved[6].(int)
	}()
	sensor, sensorW, sensorH, Width, Height = srtSensor, 1280, 720, 640, 360
	bytesPerLine, frameBytes = sensorW*10/8, sensorW*10/8*sensorH
	packed := make([]byte, frameBytes)
	repackTegra(packed, raw, sensorW, sensorH, false)
	img, _ := convert(packed)
	f, err := os.Create(os.Getenv("TECHO5_SRT_PNG"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// The Surface RT's camera is off until its file says otherwise; off any Surface RT it is never there.
func TestTheSurfaceRTCameraIsOffByDefault(t *testing.T) {
	saved := srtEnabledPath
	defer func() { srtEnabledPath = saved }()
	srtEnabledPath = t.TempDir() + "/camera-enabled"
	if srtAvailable() {
		t.Fatal("available with no file")
	}
	if _, err := openSRT(); err == nil {
		t.Fatal("opened with no file")
	}
}
