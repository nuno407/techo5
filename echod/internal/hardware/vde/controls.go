package vde

// The stateless H.264 controls (linux/v4l2-controls.h) as the kernel reads them: struct
// v4l2_ctrl_h264_sps, _pps and _decode_params, packed by hand.

const (
	spsFlagDeltaPOCAlwaysZero = 0x04
	spsFlagGapsAllowed        = 0x08
	spsFlagFrameMbsOnly       = 0x10
	spsFlagMBAFF              = 0x20
	spsFlagDirect8x8          = 0x40

	ppsFlagCABAC                = 0x01
	ppsFlagBottomFieldPOC       = 0x02
	ppsFlagWeightedPred         = 0x04
	ppsFlagDeblockingControl    = 0x08
	ppsFlagConstrainedIntraPred = 0x10
	ppsFlagRedundantPicCnt      = 0x20
	ppsFlagTransform8x8         = 0x40

	dpbFlagValid    = 0x01
	dpbFlagActive   = 0x02
	dpbFlagLongTerm = 0x04

	frameRef = 3 // V4L2_H264_FRAME_REF: both fields

	decodeFlagIDR    = 0x01
	decodeFlagPFrame = 0x08
	decodeFlagBFrame = 0x10
)

func spsControl(s *SPS) le {
	b := heap(spsSize)
	b[0] = s.ProfileIDC
	b[1] = s.ConstraintFlags
	b[2] = s.LevelIDC
	b[3] = uint8(s.ID)
	b[4] = uint8(s.ChromaFormatIDC)
	b[5] = uint8(s.BitDepthLumaMinus8)
	b[6] = uint8(s.BitDepthChromaMinus8)
	b[7] = uint8(s.Log2MaxFrameNumMinus4)
	b[8] = uint8(s.POCType)
	b[9] = uint8(s.Log2MaxPOCLsbMinus4)
	b[10] = uint8(s.MaxNumRefFrames)
	b[11] = uint8(len(s.OffsetForRefFrame))
	for i, o := range s.OffsetForRefFrame {
		b.put32(12+4*i, uint32(o))
	}
	b.put32(1032, uint32(s.OffsetForNonRefPic))
	b.put32(1036, uint32(s.OffsetForTopToBottomField))
	b.put16(1040, uint16(s.WidthMbs))
	b.put16(1042, uint16(s.HeightMapUnits))
	var f uint32
	if s.DeltaPOCAlwaysZero {
		f |= spsFlagDeltaPOCAlwaysZero
	}
	if s.GapsAllowed {
		f |= spsFlagGapsAllowed
	}
	if s.FrameMbsOnly {
		f |= spsFlagFrameMbsOnly
	}
	if s.MBAFF {
		f |= spsFlagMBAFF
	}
	if s.Direct8x8Inference {
		f |= spsFlagDirect8x8
	}
	b.put32(1044, f)
	return b
}

func ppsControl(p *PPS) le {
	b := heap(ppsSize)
	b[0] = uint8(p.ID)
	b[1] = uint8(p.SPSID)
	b[2] = uint8(p.NumSliceGroupsMinus1)
	b[3] = uint8(p.NumRefIdxL0Minus1)
	b[4] = uint8(p.L1Minus1)
	b[5] = uint8(p.WeightedBipredIDC)
	b[6] = uint8(int8(p.PicInitQPMinus26))
	b[7] = uint8(int8(p.PicInitQSMinus26))
	b[8] = uint8(int8(p.ChromaQPIndexOffset))
	b[9] = uint8(int8(p.SecondChromaQPIndexOffset))
	var f uint16
	for _, x := range []struct {
		on   bool
		flag uint16
	}{
		{p.EntropyCABAC, ppsFlagCABAC}, {p.BottomFieldPOCPresent, ppsFlagBottomFieldPOC},
		{p.WeightedPred, ppsFlagWeightedPred}, {p.DeblockingControlPresent, ppsFlagDeblockingControl},
		{p.ConstrainedIntraPred, ppsFlagConstrainedIntraPred}, {p.RedundantPicCntPresent, ppsFlagRedundantPicCnt},
		{p.Transform8x8, ppsFlagTransform8x8},
	} {
		if x.on {
			f |= x.flag
		}
	}
	b.put16(10, f)
	return b
}

// paramsControl is decode_params for the picture whose first slice is h, its order count in cur, with
// the references refs.
func paramsControl(h *Slice, cur *picture, refs []*picture, frameNum func(*picture) int) le {
	b := heap(paramsSize)
	for i, p := range refs {
		if i >= dpbEntries {
			break
		}
		at := 32 * i
		b.put64(at, p.ts)
		fn := p.frameNum
		pic := frameNum(p)
		flags := uint32(dpbFlagValid | dpbFlagActive)
		if p.long {
			fn, pic = p.longIdx, p.longIdx
			flags |= dpbFlagLongTerm
		}
		b.put32(at+8, uint32(pic))
		b.put16(at+12, uint16(fn))
		b[at+14] = frameRef
		b.put32(at+20, uint32(p.top))
		b.put32(at+24, uint32(p.bot))
		b.put32(at+28, flags)
	}
	b.put16(512, uint16(h.NalRefIDC))
	b.put16(514, uint16(h.FrameNum))
	b.put32(516, uint32(cur.top))
	b.put32(520, uint32(cur.bot))
	b.put16(524, uint16(h.IDRPicID))
	b.put16(526, uint16(h.POCLsb))
	b.put32(528, uint32(h.DeltaPOCBottom))
	b.put32(532, uint32(h.DeltaPOC[0]))
	b.put32(536, uint32(h.DeltaPOC[1]))
	b.put32(540, h.DecRefPicMarkingBits)
	b.put32(544, h.POCBits)
	var f uint32
	if h.IDR() {
		f |= decodeFlagIDR
	}
	switch h.Type {
	case sliceP, sliceSP:
		f |= decodeFlagPFrame
	case sliceB:
		f |= decodeFlagBFrame
	}
	b.put32(556, f)
	return b
}
