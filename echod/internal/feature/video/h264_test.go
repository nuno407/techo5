//go:build !dot

package video

import (
	"strings"
	"testing"
)

// bitWriter packs RBSP bits for the tests.
type bitWriter struct {
	b []byte
	n int
}

func (w *bitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>i&1 == 1 {
			w.b[len(w.b)-1] |= 0x80 >> (w.n % 8)
		}
		w.n++
	}
}

func (w *bitWriter) ue(v uint32) {
	v++
	n := 0
	for v>>n > 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v)
}

// pps is a PPS NAL unit with the fields checkPPS reads.
func pps(cabac, groups, weighted, bipred uint32) []byte {
	w := &bitWriter{}
	w.ue(0)
	w.ue(0)
	w.u(1, cabac)
	w.u(1, 0)
	w.ue(groups)
	w.ue(2)
	w.ue(0)
	w.u(1, weighted)
	w.u(2, bipred)
	w.u(1, 1) // rbsp stop bit, roughly
	return append([]byte{0x68}, w.b...)
}

func sps(profile byte) []byte { return []byte{0x67, profile, 0, 31, 0xff} }

func annexB(nals ...[]byte) []byte {
	var b []byte
	for i, n := range nals {
		if i%2 == 0 {
			b = append(b, 0, 0, 0, 1)
		} else {
			b = append(b, 0, 0, 1)
		}
		b = append(b, n...)
	}
	return b
}

func TestTheVDETakesBaselineAndCAVLCMain(t *testing.T) {
	for _, c := range []struct {
		name   string
		stream []byte
		want   string // "" for taken
	}{
		{"baseline", annexB(sps(66), pps(0, 0, 0, 0)), ""},
		{"main CAVLC, implicit B weights", annexB(sps(77), pps(0, 0, 0, 2)), ""},
		{"main CABAC", annexB(sps(77), pps(1, 0, 0, 0)), "CABAC"},
		{"high", annexB(sps(100), pps(0, 0, 0, 0)), "profile 100"},
		{"weighted P", annexB(sps(77), pps(0, 0, 1, 0)), "weighted"},
		{"explicit B weights", annexB(sps(77), pps(0, 0, 0, 1)), "weighted"},
		{"slice groups", annexB(sps(66), pps(0, 1, 0, 0)), "slice groups"},
	} {
		var ch h264Check
		ch.feed(c.stream)
		if !ch.done() {
			t.Errorf("%s: not decided", c.name)
			continue
		}
		switch {
		case c.want == "" && ch.err != nil:
			t.Errorf("%s: refused: %v", c.name, ch.err)
		case c.want != "" && (ch.err == nil || !strings.Contains(ch.err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, ch.err, c.want)
		}
	}
}

// The parameter sets can come in pieces, after other NAL units, and only the first of each counts.
func TestTheCheckWaitsForBothParameterSets(t *testing.T) {
	var ch h264Check
	stream := annexB([]byte{0x09, 0xf0}, sps(66), pps(0, 0, 0, 0), sps(100))
	ch.feed(stream[:12])
	if ch.done() {
		t.Fatal("decided on the SPS alone")
	}
	ch.feed(stream)
	if !ch.done() || ch.err != nil {
		t.Fatalf("done %v err %v", ch.done(), ch.err)
	}
}

func TestEmulationPreventionIsRemoved(t *testing.T) {
	got := unescape([]byte{1, 0, 0, 3, 0, 0, 0, 3, 1, 0, 0, 3})
	want := []byte{1, 0, 0, 0, 0, 0, 1, 0, 0}
	if string(got) != string(want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

func TestHWDecodeArgsCopyThePicture(t *testing.T) {
	a := strings.Join(HWDecodeArgs(Decoding{URL: "http://x/v.mp4", Info: Info{VideoIndex: 1, AudioIndex: 0}}), " ")
	for _, want := range []string{"-map 0:v:1", "-c:v copy -bsf:v h264_mp4toannexb -f h264 pipe:3", "-map 0:a:0", "pipe:1"} {
		if !strings.Contains(a, want) {
			t.Errorf("%q has no %q", a, want)
		}
	}
	if strings.Contains(a, "-vf") {
		t.Error("the picture is filtered")
	}
}

func TestExactRateKeepsSixty(t *testing.T) {
	if r := ExactRate(59.94); r != (Rate{60000, 1001}) {
		t.Errorf("59.94: %v", r)
	}
	if r := ExactRate(60); r != (Rate{60, 1}) {
		t.Errorf("60: %v", r)
	}
	if r := ExactRate(0); r != (Rate{30, 1}) {
		t.Errorf("none: %v", r)
	}
}

func TestI420Size(t *testing.T) {
	if n := i420Size(1280, 720); n != 1280*720*3/2 {
		t.Errorf("720p: %d", n)
	}
	// Odd chroma widths round up to 4 bytes a row, as GStreamer lays them out.
	if n := i420Size(854, 480); n != 856*480+2*428*240 {
		t.Errorf("854x480: %d", n)
	}
}

func TestGStreamerGetsMoreFilesOnly(t *testing.T) {
	got := strings.Join(gstLimits(), " ")
	if !strings.Contains(got, "-n 512") || !strings.Contains(got, "-f 0") || strings.Contains(got, "-n 64") {
		t.Errorf("limits %q", got)
	}
}
