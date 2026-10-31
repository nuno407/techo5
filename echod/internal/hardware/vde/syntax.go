package vde

import (
	"bytes"
	"errors"
	"fmt"
)

// The H.264 syntax the decoder needs read in software (ITU-T H.264 7.3): the parameter sets, as the
// kernel's controls carry them, and each picture's slice header as far as dec_ref_pic_marking, for the
// picture order and the reference marking (dpb.go). The VDE reads the slices themselves.

// NAL unit types.
const (
	nalSlice    = 1
	nalIDR      = 5
	nalSEI      = 6
	nalSPS      = 7
	nalPPS      = 8
	nalAUD      = 9
	nalEndSeq   = 10
	nalEndStrm  = 11
	nalFiller   = 12
	nalSPSExt   = 13
	nalPrefix   = 14
	nalSubSPS   = 15
	nalSliceExt = 20
)

// Slice types, modulo 5.
const (
	sliceP  = 0
	sliceB  = 1
	sliceI  = 2
	sliceSP = 3
	sliceSI = 4
)

// SPS is a sequence parameter set, the fields the VDE and the reference marking use.
type SPS struct {
	ProfileIDC, ConstraintFlags, LevelIDC    uint8
	ID                                       uint32
	ChromaFormatIDC                          uint32
	BitDepthLumaMinus8, BitDepthChromaMinus8 uint32
	Log2MaxFrameNumMinus4                    uint32
	POCType                                  uint32
	Log2MaxPOCLsbMinus4                      uint32
	DeltaPOCAlwaysZero                       bool
	OffsetForNonRefPic                       int32
	OffsetForTopToBottomField                int32
	OffsetForRefFrame                        []int32
	MaxNumRefFrames                          uint32
	GapsAllowed                              bool
	WidthMbs, HeightMapUnits                 uint32 // minus one, as coded
	FrameMbsOnly, MBAFF                      bool
	Direct8x8Inference                       bool
	Crop                                     [4]uint32 // left, right, top, bottom, in the stream's crop units
	Cropped                                  bool

	// From the VUI's bitstream restriction, when there is one.
	Restricted           bool
	MaxNumReorderFrames  uint32
	MaxDecFrameBuffering uint32
}

// MaxFrameNum is 2^(log2_max_frame_num).
func (s *SPS) MaxFrameNum() int { return 1 << (s.Log2MaxFrameNumMinus4 + 4) }

// Width and Height are the coded picture's, whole macroblocks.
func (s *SPS) Width() int  { return int(s.WidthMbs+1) * 16 }
func (s *SPS) Height() int { return int(s.HeightMapUnits+1) * 16 * frameMul(s) }

func frameMul(s *SPS) int {
	if s.FrameMbsOnly {
		return 1
	}
	return 2
}

// Visible is the picture's cropped rectangle, as x0, y0, x1, y1 in the coded picture (4:2:0).
func (s *SPS) Visible() (x0, y0, x1, y1 int) {
	w, h := s.Width(), s.Height()
	if !s.Cropped {
		return 0, 0, w, h
	}
	cx, cy := 2, 2*frameMul(s) // 4:2:0
	x0, x1 = int(s.Crop[0])*cx, w-int(s.Crop[1])*cx
	y0, y1 = int(s.Crop[2])*cy, h-int(s.Crop[3])*cy
	if x0 >= x1 || y0 >= y1 {
		return 0, 0, w, h
	}
	return x0, y0, x1, y1
}

// PPS is a picture parameter set.
type PPS struct {
	ID, SPSID                   uint32
	EntropyCABAC                bool
	BottomFieldPOCPresent       bool
	NumSliceGroupsMinus1        uint32
	NumRefIdxL0Minus1, L1Minus1 uint32
	WeightedPred                bool
	WeightedBipredIDC           uint32
	PicInitQPMinus26            int32
	PicInitQSMinus26            int32
	ChromaQPIndexOffset         int32
	SecondChromaQPIndexOffset   int32
	DeblockingControlPresent    bool
	ConstrainedIntraPred        bool
	RedundantPicCntPresent      bool
	Transform8x8                bool
}

// mmco is one memory_management_control_operation and its arguments.
type mmco struct {
	op                uint32
	diffPicNumsMinus1 uint32
	longTermPicNum    uint32
	longTermFrameIdx  uint32
	maxLongTermIdxP1  uint32
}

// Slice is the start of a slice header.
type Slice struct {
	NalType, NalRefIDC uint8
	FirstMb            uint32
	Type               uint32 // modulo 5
	PPSID              uint32
	FrameNum           uint32
	FieldPic, Bottom   bool
	IDRPicID           uint32
	POCLsb             uint32
	DeltaPOCBottom     int32
	DeltaPOC           [2]int32
	RedundantPicCnt    uint32

	NoOutputOfPriorPics bool
	LongTermReference   bool
	Adaptive            bool
	MMCO                []mmco

	DecRefPicMarkingBits uint32
	POCBits              uint32
}

// IDR is whether the slice is of an IDR picture.
func (s *Slice) IDR() bool { return s.NalType == nalIDR }

// hasMMCO5 is whether the slice's marking resets everything (memory_management_control_operation 5).
func (s *Slice) hasMMCO5() bool {
	for _, m := range s.MMCO {
		if m.op == 5 {
			return true
		}
	}
	return false
}

// bitReader reads an RBSP's bits, most significant first; past the end it sets err.
type bitReader struct {
	b   []byte
	pos int
	err error
}

var errShort = errors.New("vde: H.264 syntax runs past its end")

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			r.err = errShort
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

// ue is an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.err != nil {
			return 0
		}
		zeros++
		if zeros > 31 {
			r.err = errors.New("vde: a bad Exp-Golomb code")
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return 1<<zeros - 1 + r.u(zeros)
}

// se is a signed Exp-Golomb code.
func (r *bitReader) se() int32 {
	k := r.ue()
	if k&1 == 1 {
		return int32((k + 1) / 2)
	}
	return -int32(k / 2)
}

// unescape takes the emulation prevention bytes out of a NAL unit's payload.
func unescape(b []byte) []byte {
	if !bytes.Contains(b, []byte{0, 0, 3}) {
		return b
	}
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, v := range b {
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, v)
	}
	return out
}

// ParseSPS reads a sequence parameter set NAL unit (with its header byte).
func ParseSPS(nal []byte) (*SPS, error) {
	if len(nal) < 4 || nal[0]&0x1f != nalSPS {
		return nil, errors.New("vde: not an SPS")
	}
	r := &bitReader{b: unescape(nal[1:])}
	s := &SPS{}
	s.ProfileIDC = uint8(r.u(8))
	s.ConstraintFlags = uint8(r.u(8))
	s.LevelIDC = uint8(r.u(8))
	s.ID = r.ue()
	s.ChromaFormatIDC = 1
	switch s.ProfileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		s.ChromaFormatIDC = r.ue()
		if s.ChromaFormatIDC == 3 {
			r.u(1) // separate_colour_plane_flag
		}
		s.BitDepthLumaMinus8 = r.ue()
		s.BitDepthChromaMinus8 = r.ue()
		r.u(1)        // qpprime_y_zero_transform_bypass_flag
		if r.flag() { // seq_scaling_matrix_present_flag
			n := 8
			if s.ChromaFormatIDC == 3 {
				n = 12
			}
			for i := range n {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	s.Log2MaxFrameNumMinus4 = r.ue()
	s.POCType = r.ue()
	switch s.POCType {
	case 0:
		s.Log2MaxPOCLsbMinus4 = r.ue()
	case 1:
		s.DeltaPOCAlwaysZero = r.flag()
		s.OffsetForNonRefPic = r.se()
		s.OffsetForTopToBottomField = r.se()
		n := r.ue()
		if n > 255 {
			return nil, errors.New("vde: an SPS with too long a picture order cycle")
		}
		s.OffsetForRefFrame = make([]int32, n)
		for i := range s.OffsetForRefFrame {
			s.OffsetForRefFrame[i] = r.se()
		}
	case 2:
	default:
		return nil, fmt.Errorf("vde: pic_order_cnt_type %d", s.POCType)
	}
	s.MaxNumRefFrames = r.ue()
	s.GapsAllowed = r.flag()
	s.WidthMbs = r.ue()
	s.HeightMapUnits = r.ue()
	s.FrameMbsOnly = r.flag()
	if !s.FrameMbsOnly {
		s.MBAFF = r.flag()
	}
	s.Direct8x8Inference = r.flag()
	if r.flag() {
		s.Cropped = true
		for i := range s.Crop {
			s.Crop[i] = r.ue()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if s.Log2MaxFrameNumMinus4 > 12 || s.Log2MaxPOCLsbMinus4 > 12 || s.MaxNumRefFrames > 16 ||
		s.WidthMbs > 255 || s.HeightMapUnits > 255 {
		return nil, errors.New("vde: an SPS out of range")
	}
	if r.flag() { // vui_parameters_present_flag
		parseVUI(r, s)
	}
	return s, nil
}

func skipScalingList(r *bitReader, size int) {
	last, next := int32(8), int32(8)
	for range size {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// parseVUI reads the VUI for its bitstream restriction: how many pictures may wait to be shown. A VUI
// cut short leaves the restriction unknown.
func parseVUI(r *bitReader, s *SPS) {
	if r.flag() { // aspect_ratio_info_present_flag
		if r.u(8) == 255 {
			r.u(16)
			r.u(16)
		}
	}
	if r.flag() { // overscan_info_present_flag
		r.u(1)
	}
	if r.flag() { // video_signal_type_present_flag
		r.u(4)
		if r.flag() {
			r.u(24)
		}
	}
	if r.flag() { // chroma_loc_info_present_flag
		r.ue()
		r.ue()
	}
	if r.flag() { // timing_info_present_flag
		r.u(32)
		r.u(32)
		r.u(1)
	}
	nal := r.flag()
	if nal {
		skipHRD(r)
	}
	vcl := r.flag()
	if vcl {
		skipHRD(r)
	}
	if nal || vcl {
		r.u(1) // low_delay_hrd_flag
	}
	r.u(1)        // pic_struct_present_flag
	if r.flag() { // bitstream_restriction_flag
		r.u(1)
		r.ue()
		r.ue()
		r.ue()
		r.ue()
		reorder := r.ue()
		dec := r.ue()
		if r.err == nil && reorder <= 16 && dec <= 16 {
			s.Restricted, s.MaxNumReorderFrames, s.MaxDecFrameBuffering = true, reorder, dec
		}
	}
}

func skipHRD(r *bitReader) {
	n := r.ue()
	r.u(4)
	r.u(4)
	for i := uint32(0); i <= n && i < 32; i++ {
		r.ue()
		r.ue()
		r.u(1)
	}
	r.u(5)
	r.u(5)
	r.u(5)
	r.u(5)
}

// ParsePPS reads a picture parameter set NAL unit; sps finds the SPS it names, for its profile.
func ParsePPS(nal []byte) (*PPS, error) {
	if len(nal) < 2 || nal[0]&0x1f != nalPPS {
		return nil, errors.New("vde: not a PPS")
	}
	b := unescape(nal[1:])
	r := &bitReader{b: b}
	p := &PPS{}
	p.ID = r.ue()
	p.SPSID = r.ue()
	p.EntropyCABAC = r.flag()
	p.BottomFieldPOCPresent = r.flag()
	p.NumSliceGroupsMinus1 = r.ue()
	if p.NumSliceGroupsMinus1 > 0 {
		return nil, errors.New("vde: a PPS with slice groups")
	}
	p.NumRefIdxL0Minus1 = r.ue()
	p.L1Minus1 = r.ue()
	p.WeightedPred = r.flag()
	p.WeightedBipredIDC = r.u(2)
	p.PicInitQPMinus26 = r.se()
	p.PicInitQSMinus26 = r.se()
	p.ChromaQPIndexOffset = r.se()
	p.DeblockingControlPresent = r.flag()
	p.ConstrainedIntraPred = r.flag()
	p.RedundantPicCntPresent = r.flag()
	p.SecondChromaQPIndexOffset = p.ChromaQPIndexOffset
	if r.err != nil {
		return nil, r.err
	}
	if p.ID > 255 || p.SPSID > 31 || p.NumRefIdxL0Minus1 > 31 || p.L1Minus1 > 31 {
		return nil, errors.New("vde: a PPS out of range")
	}
	if moreRBSP(r) {
		p.Transform8x8 = r.flag()
		// pic_scaling_matrix and second_chroma_qp_index_offset: High profile only, which the VDE does
		// not take; left as they are.
	}
	return p, nil
}

// moreRBSP is whether there is syntax left before the RBSP's stop bit.
func moreRBSP(r *bitReader) bool {
	last := len(r.b) - 1
	for last >= 0 && r.b[last] == 0 {
		last--
	}
	if last < 0 {
		return false
	}
	stop := last*8 + 7
	for r.b[last]>>(uint(7-stop&7))&1 == 0 {
		stop--
	}
	return r.pos < stop
}

// sliceHeaderBytes is how much of a slice NAL unit is unescaped to read its header: the header ends
// long before this in any stream the VDE takes (no weight tables).
const sliceHeaderBytes = 512

// ParseSlice reads a slice header up to and including dec_ref_pic_marking. sps and pps look up the
// parameter sets by id.
func ParseSlice(nal []byte, spsFor func(uint32) *SPS, ppsFor func(uint32) *PPS) (*Slice, *SPS, *PPS, error) {
	if len(nal) < 2 {
		return nil, nil, nil, errShort
	}
	h := &Slice{NalType: nal[0] & 0x1f, NalRefIDC: nal[0] >> 5 & 3}
	if h.NalType != nalSlice && h.NalType != nalIDR {
		return nil, nil, nil, errors.New("vde: not a slice")
	}
	b := nal[1:]
	if len(b) > sliceHeaderBytes {
		b = b[:sliceHeaderBytes]
	}
	r := &bitReader{b: unescape(b)}
	h.FirstMb = r.ue()
	h.Type = r.ue() % 5
	h.PPSID = r.ue()
	if r.err != nil {
		return nil, nil, nil, r.err
	}
	pps := ppsFor(h.PPSID)
	if pps == nil {
		return nil, nil, nil, fmt.Errorf("vde: a slice of PPS %d, which has not come", h.PPSID)
	}
	sps := spsFor(pps.SPSID)
	if sps == nil {
		return nil, nil, nil, fmt.Errorf("vde: a slice of SPS %d, which has not come", pps.SPSID)
	}
	h.FrameNum = r.u(int(sps.Log2MaxFrameNumMinus4 + 4))
	if !sps.FrameMbsOnly {
		h.FieldPic = r.flag()
		if h.FieldPic {
			h.Bottom = r.flag()
		}
	}
	if h.IDR() {
		h.IDRPicID = r.ue()
	}
	pocStart := r.pos
	switch sps.POCType {
	case 0:
		h.POCLsb = r.u(int(sps.Log2MaxPOCLsbMinus4 + 4))
		if pps.BottomFieldPOCPresent && !h.FieldPic {
			h.DeltaPOCBottom = r.se()
		}
	case 1:
		if !sps.DeltaPOCAlwaysZero {
			h.DeltaPOC[0] = r.se()
			if pps.BottomFieldPOCPresent && !h.FieldPic {
				h.DeltaPOC[1] = r.se()
			}
		}
	}
	h.POCBits = uint32(r.pos - pocStart)
	if pps.RedundantPicCntPresent {
		h.RedundantPicCnt = r.ue()
	}
	if h.Type == sliceB {
		r.u(1) // direct_spatial_mv_pred_flag
	}
	if h.Type == sliceP || h.Type == sliceSP || h.Type == sliceB {
		if r.flag() { // num_ref_idx_active_override_flag
			r.ue()
			if h.Type == sliceB {
				r.ue()
			}
		}
	}
	// ref_pic_list_modification
	if h.Type != sliceI && h.Type != sliceSI {
		lists := 1
		if h.Type == sliceB {
			lists = 2
		}
		for range lists {
			if !r.flag() {
				continue
			}
			for n := 0; ; n++ {
				idc := r.ue()
				if idc == 3 || r.err != nil {
					break
				}
				if idc > 5 || n > 64 {
					return nil, nil, nil, errors.New("vde: a bad reference list modification")
				}
				r.ue()
			}
		}
	}
	if (pps.WeightedPred && (h.Type == sliceP || h.Type == sliceSP)) || (pps.WeightedBipredIDC == 1 && h.Type == sliceB) {
		return nil, nil, nil, errors.New("vde: explicit weighted prediction")
	}
	if h.NalRefIDC != 0 {
		start := r.pos
		if h.IDR() {
			h.NoOutputOfPriorPics = r.flag()
			h.LongTermReference = r.flag()
		} else if h.Adaptive = r.flag(); h.Adaptive {
			for n := 0; ; n++ {
				m := mmco{op: r.ue()}
				if m.op == 0 || r.err != nil {
					break
				}
				if m.op > 6 || n > 64 {
					return nil, nil, nil, errors.New("vde: a bad memory management operation")
				}
				if m.op == 1 || m.op == 3 {
					m.diffPicNumsMinus1 = r.ue()
				}
				if m.op == 2 {
					m.longTermPicNum = r.ue()
				}
				if m.op == 3 || m.op == 6 {
					m.longTermFrameIdx = r.ue()
				}
				if m.op == 4 {
					m.maxLongTermIdxP1 = r.ue()
				}
				h.MMCO = append(h.MMCO, m)
			}
		}
		h.DecRefPicMarkingBits = uint32(r.pos - start)
	}
	if r.err != nil {
		return nil, nil, nil, r.err
	}
	return h, sps, pps, nil
}
