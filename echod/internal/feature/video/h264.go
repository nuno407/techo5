//go:build !dot

package video

import (
	"bytes"
	"errors"
	"fmt"
)

// What the Surface RT's hardware decoder (tegra-vde) takes, read from the stream's own parameter sets
// before it is handed one: H.264 Baseline or Main, CAVLC only (no CABAC, so no High profile and none
// of the usual Main either), no slice groups, and no explicit weighted prediction, on which the VDE
// times out a few frames in (surface_rt docs/hw/media.md). Anything else is decoded on the cores.

// h264Check is the verdict on a stream's first SPS and PPS.
type h264Check struct {
	sps, pps bool
	profile  int
	err      error // why the hardware cannot decode it; nil when it can
}

// done is whether both parameter sets have been seen, or the stream already ruled out.
func (c *h264Check) done() bool { return c.err != nil || (c.sps && c.pps) }

// feed reads the NAL units in an Annex B stream's start, b, the parameter sets among them.
func (c *h264Check) feed(b []byte) {
	for _, nal := range nalUnits(b) {
		if c.done() || len(nal) < 2 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7:
			if c.sps {
				continue
			}
			c.sps, c.profile = true, int(nal[1])
			if c.profile != 66 && c.profile != 77 {
				c.err = fmt.Errorf("H.264 profile %d (the hardware takes Baseline and Main)", c.profile)
			}
		case 8:
			if c.pps {
				continue
			}
			c.pps = true
			c.err = checkPPS(unescape(nal[1:]))
		}
	}
}

// nalUnits splits an Annex B stream at its start codes. The last unit may be cut short.
func nalUnits(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		if start >= 0 {
			end := i
			if end > start && b[end-1] == 0 {
				end--
			}
			out = append(out, b[start:end])
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
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

// checkPPS reads a picture parameter set as far as weighted_bipred_idc.
func checkPPS(b []byte) error {
	r := bitReader{b: b}
	r.ue() // pic_parameter_set_id
	r.ue() // seq_parameter_set_id
	cabac := r.u(1)
	r.u(1) // bottom_field_pic_order_in_frame_present_flag
	groups := r.ue()
	if r.err != nil {
		return errors.New("an unreadable H.264 PPS")
	}
	if cabac == 1 {
		return errors.New("H.264 with CABAC (the hardware takes CAVLC only)")
	}
	if groups > 0 {
		return errors.New("H.264 with slice groups")
	}
	r.ue() // num_ref_idx_l0_default_active_minus1
	r.ue() // num_ref_idx_l1_default_active_minus1
	weighted := r.u(1)
	bipred := r.u(2)
	if r.err != nil {
		return errors.New("an unreadable H.264 PPS")
	}
	if weighted == 1 || bipred == 1 {
		return errors.New("H.264 with explicit weighted prediction (the hardware times out on it)")
	}
	return nil
}

// bitReader reads an RBSP's bits, most significant first; past the end it sets err.
type bitReader struct {
	b   []byte
	pos int
	err error
}

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			r.err = errors.New("short")
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

// ue is an Exp-Golomb code.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.err != nil || zeros > 31 {
			r.err = errors.New("bad exp-golomb")
			return 0
		}
		zeros++
	}
	return 1<<zeros - 1 + r.u(zeros)
}
