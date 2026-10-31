package vde

import (
	"os"
	"testing"
	"time"
)

// The test streams are x264's, 128x96, 30 frames each (testdata/*.h264; *.md5 is ffmpeg's framemd5 of
// each frame as shown, for vdeprobe on the device):
//
//	main_b:       Main, CAVLC, 2 B-frames, 3 references, a keyframe every 12
//	main_pyramid: Main, CAVLC, 3 B-frames with B references (pyramid), 4 references
//	base_slices:  Baseline, 3 slices a picture, 2 references

// fakeEngine decodes nothing: it records what it was asked.
type fakeEngine struct {
	t      *testing.T
	queued int
	calls  []fakeCall
}

type fakeCall struct {
	buf    int
	ts     uint64
	params le
	slices int
}

func (e *fakeEngine) queuePicture(i int, fds [3]int) error { e.queued = i; return nil }

func (e *fakeEngine) decode(bitstream []byte, sps, pps, params []byte, ts uint64, _ time.Duration) error {
	if len(sps) != spsSize || len(pps) != ppsSize || len(params) != paramsSize {
		e.t.Fatalf("controls of %d, %d, %d bytes", len(sps), len(pps), len(params))
	}
	n := 0
	for i := 0; i+3 <= len(bitstream); i++ {
		if bitstream[i] == 0 && bitstream[i+1] == 0 && bitstream[i+2] == 1 {
			n++
		}
	}
	e.calls = append(e.calls, fakeCall{buf: e.queued, ts: ts, params: append(le(nil), params...), slices: n})
	return nil
}

func (e *fakeEngine) format() Format { return Format{} }
func (e *fakeEngine) close()         {}

// run decodes a test stream with the fake engine, and gives back the order count of each picture as
// it was shown, and the engine.
func run(t *testing.T, name string) ([]int32, *fakeEngine, *Decoder) {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".h264")
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for _, n := range splitNALs(data) {
		if n[0]&0x1f == nalSPS {
			first = n
			break
		}
	}
	s, err := ParseSPS(first)
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEngine{t: t}
	var shown []int32
	var d *Decoder
	pocOf := map[uint64]int32{}
	d = newDecoder(s, e, func(buf int) {
		// The picture shown is the last one decoded into buf.
		for i := len(e.calls) - 1; i >= 0; i-- {
			if e.calls[i].buf == buf {
				shown = append(shown, pocOf[e.calls[i].ts])
				break
			}
		}
		d.Release(buf)
	}, nil)
	n := int(s.MaxNumRefFrames) + d.dpb.reorder() + 2
	d.setBuffers(make([][3]int, n))
	// Feed in odd-sized pieces, as a pipe would.
	for i := 0; i < len(data); i += 777 {
		if _, err := d.Write(data[i:min(i+777, len(data))]); err != nil {
			t.Fatal(err)
		}
		for _, c := range e.calls {
			pocOf[c.ts] = int32(c.params.u32(516))
		}
	}
	if err := d.End(); err != nil {
		t.Fatal(err)
	}
	return shown, e, d
}

func splitNALs(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && b[end-1] == 0 {
					end--
				}
				out = append(out, b[start:end])
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 {
		out = append(out, b[start:])
	}
	return out
}

func TestStreamsShowEveryPictureInOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		slices  int
		bframes bool
	}{{"main_b", 1, true}, {"main_pyramid", 1, true}, {"base_slices", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			shown, e, d := run(t, tc.name)
			if len(e.calls) != 30 || d.Pictures != 30 {
				t.Fatalf("%d pictures decoded, want 30", len(e.calls))
			}
			if len(shown) != 30 {
				t.Fatalf("%d pictures shown, want 30", len(shown))
			}
			// Each keyframe period starts its order count again; within one it only goes up.
			reordered := false
			for i := 1; i < len(shown); i++ {
				if shown[i] != 0 && shown[i] <= shown[i-1] {
					t.Errorf("shown out of order at %d: %v", i, shown)
					break
				}
			}
			for i, c := range e.calls {
				if c.slices != tc.slices {
					t.Fatalf("picture %d went with %d slices, want %d", i, c.slices, tc.slices)
				}
				flags := c.params.u32(556)
				nrefs := 0
				for k := 0; k < dpbEntries; k++ {
					if c.params.u32(32*k+28)&dpbFlagActive != 0 {
						nrefs++
					}
				}
				switch {
				case flags&decodeFlagIDR != 0:
				case flags&decodeFlagBFrame != 0:
					reordered = true
					if nrefs < 2 {
						t.Errorf("B picture %d with %d references", i, nrefs)
					}
				case flags&decodeFlagPFrame != 0:
					if nrefs < 1 {
						t.Errorf("P picture %d with no reference", i)
					}
				}
				if nrefs > 4 {
					t.Errorf("picture %d with %d references", i, nrefs)
				}
			}
			if reordered != tc.bframes {
				t.Errorf("B pictures: %v, want %v", reordered, tc.bframes)
			}
		})
	}
}

func TestParseSPS(t *testing.T) {
	data, err := os.ReadFile("testdata/main_b.h264")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range splitNALs(data) {
		if n[0]&0x1f != nalSPS {
			continue
		}
		s, err := ParseSPS(n)
		if err != nil {
			t.Fatal(err)
		}
		if s.ProfileIDC != 77 || s.Width() != 128 || s.Height() != 96 || s.MaxNumRefFrames != 3 || !s.FrameMbsOnly {
			t.Errorf("SPS %+v", s)
		}
		if !s.Restricted || s.MaxNumReorderFrames != 1 {
			t.Errorf("reorder %v %d, want 1", s.Restricted, s.MaxNumReorderFrames)
		}
		return
	}
	t.Fatal("no SPS")
}

func TestCrop(t *testing.T) {
	s := &SPS{WidthMbs: 79, HeightMapUnits: 67, FrameMbsOnly: true, Cropped: true, Crop: [4]uint32{0, 0, 0, 4}}
	if x0, y0, x1, y1 := s.Visible(); x0 != 0 || y0 != 0 || x1 != 1280 || y1 != 1080 {
		t.Errorf("visible %d,%d-%d,%d, want 0,0-1280,1080", x0, y0, x1, y1)
	}
}

// A stream that waits on a buffer the screen still holds gets it once it is released, and starved is
// asked meanwhile.
func TestWaitsForABuffer(t *testing.T) {
	s := &SPS{MaxNumRefFrames: 1, FrameMbsOnly: true}
	d := newDecoder(s, &fakeEngine{t: t}, func(int) {}, nil)
	d.setBuffers(make([][3]int, 2))
	d.held[0], d.held[1] = true, true
	asked := make(chan struct{}, 1)
	d.starved = func() {
		select {
		case asked <- struct{}{}:
		default:
		}
		d.Release(1)
	}
	got, err := d.free()
	if err != nil || got != 1 {
		t.Fatalf("free = %d, %v; want 1", got, err)
	}
	select {
	case <-asked:
	default:
		t.Error("starved was not asked")
	}
	d.Stop()
	d.held[1] = true
	if _, err := d.free(); err == nil {
		t.Error("a stopped decoder handed out a buffer")
	}
}
