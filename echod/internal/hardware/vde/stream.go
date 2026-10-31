// Package vde decodes H.264 on the Tegra 3's video decoder engine (tegra-vde, the kernel's V4L2
// stateless decoder) straight into buffers someone else owns: the screen's, which it then scans out
// as they are, so no core copies or converts a pixel of the picture. The bitstream comes in as Annex B;
// the parameter sets, the slice headers' start, the picture order and the reference marking are
// worked out here (syntax.go, dpb.go), since a stateless decoder leaves them to its caller, and the
// pictures come out in the order they are shown.
//
// What the VDE takes: Baseline and Main, CAVLC only, progressive frames, no explicit weighted
// prediction, every slice of a picture of one type (surface_rt docs/hw/media.md).
package vde

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"sync"
	"time"
)

// pictureTimeout is how long the VDE may take over one picture: a 1080p one takes about 25 ms.
const pictureTimeout = time.Second

// mostBufferBytes is as much as the picture buffers may take in all, past what the stream needs.
const mostBufferBytes = 48 << 20

// spareBuffers is how many pictures past the references and the reordering there are buffers for:
// the one being decoded, the one on the panel and the one going up, and a few waiting their turn.
const spareBuffers = 6

// MakeBuffers makes n picture buffers of format f and gives back each one's three dmabufs (Y, U, V),
// which stay open until the decoder is closed.
type MakeBuffers func(n int, f Format) ([][3]int, error)

// Decoder is one stream on the VDE.
type Decoder struct {
	dev     engine
	dpb     *dpb
	sps     map[uint32]*SPS
	pps     map[uint32]*PPS
	first   *SPS
	crop    image.Rectangle
	bufs    [][3]int
	ts      uint64
	pending []byte // what Write has not split into NAL units yet

	// The picture being gathered: its slices, Annex B, and its first slice's header.
	au      []byte
	head    *Slice
	headSPS *SPS
	headPPS *PPS

	out func(buf int)

	mu      sync.Mutex
	cond    *sync.Cond
	held    []bool // shown and not released yet
	closed  bool
	starved func() // called while no buffer is free, about every 50 ms

	Pictures, Damaged int
}

// Open opens the VDE for the stream whose first SPS is sps (a NAL unit), with picture buffers from
// mk; out is called with each picture as it is due to be shown, in order, and the picture's buffer is
// the caller's until Release. starved is called now and then while the decoder waits for a buffer:
// it may Release one it no longer needs (a picture that would be dropped anyway).
func Open(sps []byte, mk MakeBuffers, out func(buf int), starved func()) (*Decoder, error) {
	s, err := ParseSPS(sps)
	if err != nil {
		return nil, err
	}
	if err := supported(s); err != nil {
		return nil, err
	}
	video, media, err := Find()
	if err != nil {
		return nil, err
	}
	dev, err := openDevice(video, media, s.Width(), s.Height(), 1)
	if err != nil {
		return nil, err
	}
	f := dev.fmt
	d := newDecoder(s, dev, out, starved)
	n := int(s.MaxNumRefFrames) + d.dpb.reorder() + spareBuffers
	if per := f.Size[0] + f.Size[1] + f.Size[2]; per > 0 {
		n = min(n, max(mostBufferBytes/per, int(s.MaxNumRefFrames)+d.dpb.reorder()+3))
	}
	bufs, err := mk(n, f)
	if err != nil {
		dev.close()
		return nil, err
	}
	if err := dev.useBuffers(bufs); err != nil {
		dev.close()
		return nil, err
	}
	d.setBuffers(bufs)
	return d, nil
}

// engine is what decodes: the VDE (device), or a test's stand-in.
type engine interface {
	queuePicture(i int, fds [3]int) error
	decode(bitstream []byte, sps, pps, params []byte, ts uint64, timeout time.Duration) error
	format() Format
	close()
}

func (d *device) format() Format { return d.fmt }

func newDecoder(s *SPS, dev engine, out func(int), starved func()) *Decoder {
	d := &Decoder{dev: dev, dpb: newDPB(), sps: map[uint32]*SPS{s.ID: s}, pps: map[uint32]*PPS{}, first: s, out: out, starved: starved}
	d.cond = sync.NewCond(&d.mu)
	d.dpb.sps = s
	x0, y0, x1, y1 := s.Visible()
	d.crop = image.Rect(x0, y0, x1, y1)
	return d
}

func (d *Decoder) setBuffers(bufs [][3]int) {
	d.bufs = bufs
	d.held = make([]bool, len(bufs))
}

// supported is why the VDE cannot decode a stream of this SPS, nil if it can.
func supported(s *SPS) error {
	switch {
	case s.ProfileIDC != 66 && s.ProfileIDC != 77:
		return fmt.Errorf("vde: H.264 profile %d (the VDE takes Baseline and Main)", s.ProfileIDC)
	case !s.FrameMbsOnly:
		return errors.New("vde: interlaced H.264")
	case s.ChromaFormatIDC != 1:
		return errors.New("vde: not 4:2:0")
	case s.Width() > 2032 || s.Height() > 2032:
		return errors.New("vde: a picture larger than the VDE takes")
	}
	return nil
}

// Format is what the pictures are written as.
func (d *Decoder) Format() Format { return d.dev.format() }

// Crop is the part of each picture to show.
func (d *Decoder) Crop() image.Rectangle { return d.crop }

// Buffers is how many picture buffers there are.
func (d *Decoder) Buffers() int { return len(d.bufs) }

// Write takes more of the Annex B stream.
func (d *Decoder) Write(p []byte) (int, error) {
	d.pending = append(d.pending, p...)
	b := d.pending
	start := bytes.Index(b, startCode)
	if start < 0 {
		// Nothing to split on yet: keep the last two bytes, a start code's beginning.
		if len(b) > 2 {
			d.pending = append(d.pending[:0], b[len(b)-2:]...)
		}
		return len(p), nil
	}
	for {
		next := bytes.Index(b[start+3:], startCode)
		if next < 0 {
			break
		}
		end := start + 3 + next
		nal := b[start+3 : end]
		for len(nal) > 0 && nal[len(nal)-1] == 0 { // a four-byte start code's leading zero, trailing zeros
			nal = nal[:len(nal)-1]
		}
		if err := d.nal(nal); err != nil {
			return len(p), err
		}
		start = end
	}
	d.pending = append(d.pending[:0], b[start:]...)
	return len(p), nil
}

var startCode = []byte{0, 0, 1}

// End decodes what is left of the stream and shows every picture still waiting.
func (d *Decoder) End() error {
	if len(d.pending) > 3 {
		nal := d.pending[3:]
		d.pending = nil
		if err := d.nal(nal); err != nil {
			return err
		}
	}
	if err := d.picture(); err != nil {
		return err
	}
	for _, p := range d.dpb.drain() {
		d.show(p)
	}
	return nil
}

// nal takes one NAL unit.
func (d *Decoder) nal(nal []byte) error {
	if len(nal) == 0 {
		return nil
	}
	switch typ := nal[0] & 0x1f; typ {
	case nalSlice, nalIDR:
		h, sps, pps, err := ParseSlice(nal, d.spsByID, d.ppsByID)
		if err != nil {
			return err
		}
		if h.RedundantPicCnt > 0 {
			return nil // the primary picture is there too
		}
		if h.FirstMb == 0 && d.head != nil {
			if err := d.picture(); err != nil {
				return err
			}
		}
		if d.head == nil {
			if h.FieldPic {
				return errors.New("vde: a field picture")
			}
			d.head, d.headSPS, d.headPPS = h, sps, pps
		}
		d.au = append(d.au, 0, 0, 1)
		d.au = append(d.au, nal...)
	case nalSPS:
		if err := d.picture(); err != nil {
			return err
		}
		s, err := ParseSPS(nal)
		if err != nil {
			return err
		}
		if s.Width() != d.first.Width() || s.Height() != d.first.Height() {
			return errors.New("vde: the stream changed its picture size")
		}
		if err := supported(s); err != nil {
			return err
		}
		d.sps[s.ID] = s
	case nalPPS:
		if err := d.picture(); err != nil {
			return err
		}
		p, err := ParsePPS(nal)
		if err != nil {
			return err
		}
		if p.EntropyCABAC {
			return errors.New("vde: H.264 with CABAC (the VDE takes CAVLC only)")
		}
		d.pps[p.ID] = p
	case nalAUD, nalEndSeq, nalEndStrm, nalSEI:
		return d.picture()
	}
	return nil
}

func (d *Decoder) spsByID(id uint32) *SPS { return d.sps[id] }
func (d *Decoder) ppsByID(id uint32) *PPS { return d.pps[id] }

// picture decodes the picture gathered so far, if there is one.
func (d *Decoder) picture() error {
	h, sps, pps, au := d.head, d.headSPS, d.headPPS, d.au
	d.head, d.au = nil, d.au[:0]
	if h == nil {
		return nil
	}
	d.dpb.sps = sps
	cur := &picture{}
	for _, p := range d.dpb.start(h, cur) {
		d.show(p)
	}
	buf, err := d.free()
	if err != nil {
		return err
	}
	d.ts += 1000
	cur.buf, cur.ts = buf, d.ts
	refs := d.dpb.refs()
	params := paramsControl(h, cur, refs, func(p *picture) int { return d.dpb.picNum(p, int(h.FrameNum)) })
	if err := d.dev.queuePicture(buf, d.bufs[buf]); err != nil {
		return fmt.Errorf("vde: QBUF picture: %w", err)
	}
	err = d.dev.decode(au, spsControl(sps), ppsControl(pps), params, cur.ts, pictureTimeout)
	switch {
	case errors.Is(err, errPicture):
		d.Damaged++ // kept as it came out: the pictures after it refer to it
	case err != nil:
		return err
	}
	d.Pictures++
	for _, p := range d.dpb.finish(h, cur) {
		d.show(p)
	}
	return nil
}

// show hands a picture out to be shown.
func (d *Decoder) show(p *picture) {
	if p.buf < 0 {
		return
	}
	d.mu.Lock()
	d.held[p.buf] = true
	d.mu.Unlock()
	d.out(p.buf)
}

// free is a buffer the next picture can go into, waiting for one if all are taken.
func (d *Decoder) free() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if d.closed {
			return 0, errClosed
		}
		for i := range d.bufs {
			if !d.held[i] && !d.dpb.holds(i) {
				return i, nil
			}
		}
		t := time.AfterFunc(50*time.Millisecond, func() {
			d.mu.Lock()
			d.cond.Broadcast()
			d.mu.Unlock()
		})
		d.cond.Wait()
		t.Stop()
		if d.starved != nil && !d.closed {
			d.mu.Unlock()
			d.starved()
			d.mu.Lock()
		}
	}
}

var errClosed = errors.New("vde: closed")

// Release gives a shown picture's buffer back.
func (d *Decoder) Release(buf int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if buf >= 0 && buf < len(d.held) {
		d.held[buf] = false
	}
	d.cond.Broadcast()
}

// Stop wakes a Write waiting for a buffer, which then fails: for ending a stream from elsewhere.
func (d *Decoder) Stop() {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// Close lets the VDE go. The picture buffers are the caller's to free, after.
func (d *Decoder) Close() {
	d.Stop()
	d.dev.close()
}
