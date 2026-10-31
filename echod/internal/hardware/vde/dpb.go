package vde

import "sort"

// The decoded picture buffer, as the stateless API leaves it to the caller (ITU-T H.264 8.2.1, 8.2.5
// and C.4): each picture's order count, which pictures stay references, and when each is shown. The
// VDE gets the references with every picture (decode_params' DPB) and builds its lists from them.

// picture is a decoded picture the buffer still holds: a reference, or one not shown yet.
type picture struct {
	buf      int    // the capture buffer it is in; -1 for a frame a gap left out
	ts       uint64 // its capture buffer's timestamp, which the references name it by
	frameNum int
	top, bot int32 // picture order counts
	long     bool  // a long-term reference
	ref      bool  // a reference at all
	longIdx  int   // LongTermFrameIdx
	waiting  bool  // not output yet
	idr      bool
}

// poc is the picture's order count as a frame.
func (p *picture) poc() int32 { return min(p.top, p.bot) }

// dpb is the decoder's side of the picture buffer.
type dpb struct {
	sps  *SPS
	pics []*picture

	prevMsb, prevLsb   int32 // picture order type 0, of the previous reference picture
	prevFrameNumOffset int
	prevFrameNum       int
	prevRefFrameNum    int
	maxLongIdx         int // MaxLongTermFrameIdx; -1 for none
	frameNumOffset     int // the current picture's
	msb                int32
}

func newDPB() *dpb { return &dpb{maxLongIdx: -1} }

// reorder is how many pictures may wait to be shown after a later one is decoded.
func (d *dpb) reorder() int {
	s := d.sps
	if s.Restricted {
		return int(s.MaxNumReorderFrames)
	}
	if s.ProfileIDC == 66 || (s.ProfileIDC == 77 && s.ConstraintFlags&0x80 != 0) {
		return 0 // Baseline: no B slices, shown in the order decoded
	}
	return dpbFrames(s)
}

// dpbFrames is how many frames the stream's level lets the buffer hold (A.3.1, MaxDpbMbs).
func dpbFrames(s *SPS) int {
	mbs := 0
	switch s.LevelIDC {
	case 9, 10:
		mbs = 396
	case 11:
		if s.ConstraintFlags&0x10 != 0 && s.ProfileIDC != 100 {
			mbs = 396 // level 1b
		} else {
			mbs = 900
		}
	case 12, 13, 20:
		mbs = 2376
	case 21:
		mbs = 4752
	case 22, 30:
		mbs = 8100
	case 31:
		mbs = 18000
	case 32:
		mbs = 20480
	case 40, 41:
		mbs = 32768
	case 42:
		mbs = 34816
	case 50:
		mbs = 110400
	default:
		mbs = 184320
	}
	frame := int(s.WidthMbs+1) * int(s.HeightMapUnits+1) * frameMul(s)
	return max(1, min(mbs/max(frame, 1), 16))
}

// refs are the references, for decode_params.
func (d *dpb) refs() []*picture {
	var out []*picture
	for _, p := range d.pics {
		if p.ref && p.buf >= 0 {
			out = append(out, p)
		}
	}
	return out
}

// picNum is a short-term reference's PicNum while frame frameNum is decoded (8.2.4.1).
func (d *dpb) picNum(p *picture, frameNum int) int {
	if p.frameNum > frameNum {
		return p.frameNum - d.sps.MaxFrameNum()
	}
	return p.frameNum
}

// start is a new picture, its first slice h: its order count, the frames a gap in frame_num left
// out, and the pictures to show before it (an IDR or a memory_management_control_operation 5 shows
// everything still waiting). cur is filled in with the order count.
func (d *dpb) start(h *Slice, cur *picture) (shown []*picture) {
	s := d.sps
	maxFrameNum := s.MaxFrameNum()
	cur.frameNum = int(h.FrameNum)
	cur.idr = h.IDR()
	if h.IDR() || h.hasMMCO5() {
		shown = d.flush(h.IDR() && h.NoOutputOfPriorPics)
	}
	if h.IDR() {
		for _, p := range d.pics {
			p.ref, p.long = false, false
		}
		d.prune()
		d.prevRefFrameNum = 0
	} else if fn := int(h.FrameNum); fn != d.prevRefFrameNum && fn != (d.prevRefFrameNum+1)%maxFrameNum {
		// Frames missing from frame_num (lost, or left out where the stream allows it): each takes a
		// place as a short-term reference the sliding window moves on from (8.2.5.2), with no picture.
		for n := (d.prevRefFrameNum + 1) % maxFrameNum; n != fn; n = (n + 1) % maxFrameNum {
			d.slide(n)
			d.pics = append(d.pics, &picture{buf: -1, frameNum: n, ref: true})
			d.prevRefFrameNum = n
			if len(d.pics) > 64 {
				break
			}
		}
	}

	// 8.2.1: picture order count.
	ref := h.NalRefIDC != 0
	if !h.IDR() {
		if d.prevFrameNum > int(h.FrameNum) {
			d.frameNumOffset = d.prevFrameNumOffset + maxFrameNum
		} else {
			d.frameNumOffset = d.prevFrameNumOffset
		}
	} else {
		d.frameNumOffset = 0
	}
	switch s.POCType {
	case 0:
		prevMsb, prevLsb := d.prevMsb, d.prevLsb
		if h.IDR() {
			prevMsb, prevLsb = 0, 0
		}
		maxLsb := int32(1) << (s.Log2MaxPOCLsbMinus4 + 4)
		lsb := int32(h.POCLsb)
		msb := prevMsb
		switch {
		case lsb < prevLsb && prevLsb-lsb >= maxLsb/2:
			msb = prevMsb + maxLsb
		case lsb > prevLsb && lsb-prevLsb > maxLsb/2:
			msb = prevMsb - maxLsb
		}
		d.msb = msb
		cur.top = msb + lsb
		cur.bot = cur.top + h.DeltaPOCBottom
	case 1:
		n := len(s.OffsetForRefFrame)
		abs := 0
		if n != 0 {
			abs = d.frameNumOffset + int(h.FrameNum)
		}
		if !ref && abs > 0 {
			abs--
		}
		var expected int32
		if abs > 0 {
			var cycle int32
			for _, o := range s.OffsetForRefFrame {
				cycle += o
			}
			cnt, in := (abs-1)/n, (abs-1)%n
			expected = int32(cnt) * cycle
			for i := 0; i <= in; i++ {
				expected += s.OffsetForRefFrame[i]
			}
		}
		if !ref {
			expected += s.OffsetForNonRefPic
		}
		cur.top = expected + h.DeltaPOC[0]
		cur.bot = cur.top + s.OffsetForTopToBottomField + h.DeltaPOC[1]
	case 2:
		var t int32
		switch {
		case h.IDR():
			t = 0
		case !ref:
			t = 2*int32(d.frameNumOffset+int(h.FrameNum)) - 1
		default:
			t = 2 * int32(d.frameNumOffset+int(h.FrameNum))
		}
		cur.top, cur.bot = t, t
	}
	return shown
}

// finish takes picture cur, decoded from slice h, into the buffer: the reference marking (8.2.5), then
// whatever has waited long enough to be shown, in order.
func (d *dpb) finish(h *Slice, cur *picture) (shown []*picture) {
	mmco5 := h.hasMMCO5()
	if h.NalRefIDC != 0 {
		cur.ref = true
		switch {
		case h.IDR():
			if h.LongTermReference {
				cur.long, cur.longIdx, d.maxLongIdx = true, 0, 0
			} else {
				d.maxLongIdx = -1
			}
		case h.Adaptive:
			d.adaptive(h, cur)
		default:
			d.slide(cur.frameNum)
		}
		// More references than the stream says it keeps: a damaged stream; the oldest go.
		for d.countRefs() >= max(int(d.sps.MaxNumRefFrames), 1) && d.dropOldestShort(cur.frameNum) {
		}
	}
	if mmco5 {
		// After memory_management_control_operation 5 the picture counts as frame 0, its order from 0.
		t := cur.poc()
		cur.top -= t
		cur.bot -= t
		cur.frameNum = 0
	}

	// The order count state the next picture starts from.
	if h.NalRefIDC != 0 {
		if mmco5 {
			d.prevMsb, d.prevLsb = 0, cur.top
		} else {
			d.prevMsb, d.prevLsb = d.msb, int32(h.POCLsb)
		}
		d.prevRefFrameNum = cur.frameNum
	}
	if mmco5 {
		d.prevFrameNumOffset, d.prevFrameNum = 0, 0
	} else {
		d.prevFrameNumOffset, d.prevFrameNum = d.frameNumOffset, int(h.FrameNum)
	}

	cur.waiting = true
	d.pics = append(d.pics, cur)
	reorder := d.reorder()
	for d.countWaiting() > reorder {
		shown = append(shown, d.bump())
	}
	d.prune()
	return shown
}

// adaptive applies the slice's memory management operations to the references, cur being decoded.
func (d *dpb) adaptive(h *Slice, cur *picture) {
	currPicNum := cur.frameNum
	for _, m := range h.MMCO {
		switch m.op {
		case 1:
			x := currPicNum - int(m.diffPicNumsMinus1+1)
			for _, p := range d.pics {
				if p.ref && !p.long && d.picNum(p, cur.frameNum) == x {
					p.ref = false
				}
			}
		case 2:
			for _, p := range d.pics {
				if p.ref && p.long && p.longIdx == int(m.longTermPicNum) {
					p.ref, p.long = false, false
				}
			}
		case 3:
			x := currPicNum - int(m.diffPicNumsMinus1+1)
			idx := int(m.longTermFrameIdx)
			for _, p := range d.pics {
				if p.ref && p.long && p.longIdx == idx {
					p.ref, p.long = false, false
				}
			}
			for _, p := range d.pics {
				if p.ref && !p.long && d.picNum(p, cur.frameNum) == x {
					p.long, p.longIdx = true, idx
				}
			}
		case 4:
			d.maxLongIdx = int(m.maxLongTermIdxP1) - 1
			for _, p := range d.pics {
				if p.ref && p.long && p.longIdx > d.maxLongIdx {
					p.ref, p.long = false, false
				}
			}
		case 5:
			for _, p := range d.pics {
				p.ref, p.long = false, false
			}
			d.maxLongIdx = -1
		case 6:
			idx := int(m.longTermFrameIdx)
			for _, p := range d.pics {
				if p.ref && p.long && p.longIdx == idx {
					p.ref, p.long = false, false
				}
			}
			cur.long, cur.longIdx = true, idx
		}
	}
}

// slide is the sliding window before frame frameNum is added: with the references full, the oldest
// short-term one goes.
func (d *dpb) slide(frameNum int) {
	for d.countRefs() >= max(int(d.sps.MaxNumRefFrames), 1) {
		if !d.dropOldestShort(frameNum) {
			return
		}
	}
}

// dropOldestShort unmarks the short-term reference with the lowest FrameNumWrap; false if there is none.
func (d *dpb) dropOldestShort(frameNum int) bool {
	var oldest *picture
	for _, p := range d.pics {
		if p.ref && !p.long && (oldest == nil || d.picNum(p, frameNum) < d.picNum(oldest, frameNum)) {
			oldest = p
		}
	}
	if oldest == nil {
		return false
	}
	oldest.ref = false
	d.prune()
	return true
}

func (d *dpb) countRefs() int {
	n := 0
	for _, p := range d.pics {
		if p.ref {
			n++
		}
	}
	return n
}

func (d *dpb) countWaiting() int {
	n := 0
	for _, p := range d.pics {
		if p.waiting {
			n++
		}
	}
	return n
}

// bump shows the waiting picture with the lowest order count.
func (d *dpb) bump() *picture {
	var first *picture
	for _, p := range d.pics {
		if p.waiting && (first == nil || p.poc() < first.poc()) {
			first = p
		}
	}
	first.waiting = false
	return first
}

// flush shows every waiting picture, in order; drop is a stream asking for them not to be shown.
func (d *dpb) flush(drop bool) []*picture {
	var out []*picture
	for _, p := range d.pics {
		if p.waiting {
			p.waiting = false
			if !drop {
				out = append(out, p)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].poc() < out[j].poc() })
	d.prune()
	return out
}

// prune lets go of the pictures neither referenced nor waiting.
func (d *dpb) prune() {
	keep := d.pics[:0]
	for _, p := range d.pics {
		if p.ref || p.waiting {
			keep = append(keep, p)
		}
	}
	for i := len(keep); i < len(d.pics); i++ {
		d.pics[i] = nil
	}
	d.pics = keep
}

// holds is whether capture buffer buf is still the buffer's: a reference, or not shown yet.
func (d *dpb) holds(buf int) bool {
	for _, p := range d.pics {
		if p.buf == buf {
			return true
		}
	}
	return false
}

// drain shows everything still waiting, at the end of the stream.
func (d *dpb) drain() []*picture { return d.flush(false) }
