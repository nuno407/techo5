//go:build !dot && !panel

package screen

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
)

// A video on KMS's planes (the Surface RT): the decoder's YUV 4:2:0 pictures go as they are onto an
// overlay plane, which the display controller scales into place and turns into RGB as it scans out,
// so the cores neither scale nor convert a pixel. The pages under it (the primary plane) show black,
// the bars; the controls go on the cursor plane, which tegra-drm's Tegra 3 has as a full window over
// the other two, with the canvas's alpha.
//
// Tegra 3's display controller stacks its windows A (the primary, the pages), B (the overlay) and C
// (the cursor) in that order, which is the order wanted here.

const (
	drmSetClientCap   = 0x0d
	drmModeGetProp    = 0xaa
	drmModeObjGetProp = 0xb9

	drmClientCapUniversalPlanes = 2
	drmModeObjectPlane          = 0xeeeeeeee

	planeOverlay = 0
	planeCursor  = 2

	fourccYUV420   = 0x32315559 // 'YU12'
	fourccARGB8888 = 0x34325241 // 'AR24'
)

// planeKind is a plane the CRTC can use: its type and the formats it takes.
type planeKind struct {
	id      uint32
	typ     uint64
	formats []uint32
}

// yuvBuf is a dumb buffer holding one 4:2:0 picture: Y, then U, then V, pitch for Y and half of it for U
// and V.
type yuvBuf struct {
	kmsBuf
	w, h, pitch int
}

// kmsVideo is the video's planes and buffers.
type kmsVideo struct {
	overlay, cursor uint32
	yuv             []yuvBuf
	next            int  // the YUV buffer to fill next
	on              bool // the overlay shows the video
	ctl             kmsBuf
	ctlRect         image.Rectangle // where the controls' plane is, empty while it is off
	ctlShadow       []byte
	ctlPitch        int
	blackPage       bool // the pages show black under the video
}

// findVideoPlanes picks the overlay and the cursor plane of the CRTC, with universal planes on so the
// cursor plane is listed and each plane's type can be read.
func (k *kmsOut) findVideoPlanes() {
	c := make(args, 16)
	c.put64(0, drmClientCapUniversalPlanes)
	c.put64(8, 1)
	if drmIoctl(k.fd(), drmSetClientCap, c) != nil {
		return
	}
	pr := make(args, 16)
	if drmIoctl(k.fd(), drmModeGetPlaneRs, pr) != nil || pr.u32(8) == 0 {
		return
	}
	ids := make([]uint32, pr.u32(8))
	pr = make(args, 16)
	pr.ptr(0, ids)
	pr.put32(8, uint32(len(ids)))
	if drmIoctl(k.fd(), drmModeGetPlaneRs, pr) != nil {
		return
	}
	ci := k.crtcIndex()
	var planes []planeKind
	for _, id := range ids[:min(int(pr.u32(8)), len(ids))] {
		p := make(args, 32)
		p.put32(0, id)
		if drmIoctl(k.fd(), drmModeGetPlane, p) != nil {
			continue
		}
		if ci < 0 || p.u32(12)&(1<<ci) == 0 {
			continue
		}
		formats := make([]uint32, p.u32(20))
		if len(formats) > 0 {
			p.ptr(24, formats)
			if drmIoctl(k.fd(), drmModeGetPlane, p) != nil {
				continue
			}
		}
		typ, ok := k.planeType(id)
		if !ok {
			continue
		}
		planes = append(planes, planeKind{id: id, typ: typ, formats: formats})
	}
	v := &kmsVideo{}
	v.overlay, v.cursor = pickVideoPlanes(planes)
	if v.overlay != 0 {
		k.video = v
	}
}

// pickVideoPlanes is the overlay that takes YUV 4:2:0 and the cursor plane that takes ARGB.
func pickVideoPlanes(planes []planeKind) (overlay, cursor uint32) {
	has := func(p planeKind, f uint32) bool {
		for _, g := range p.formats {
			if g == f {
				return true
			}
		}
		return false
	}
	for _, p := range planes {
		switch {
		case p.typ == planeOverlay && overlay == 0 && has(p, fourccYUV420):
			overlay = p.id
		case p.typ == planeCursor && cursor == 0 && has(p, fourccARGB8888):
			cursor = p.id
		}
	}
	return overlay, cursor
}

// planeType reads a plane's "type" property.
func (k *kmsOut) planeType(id uint32) (uint64, bool) {
	g := make(args, 32)
	g.put32(20, id)
	g.put32(24, drmModeObjectPlane)
	if drmIoctl(k.fd(), drmModeObjGetProp, g) != nil || g.u32(16) == 0 {
		return 0, false
	}
	n := g.u32(16)
	props := make([]uint32, n)
	values := make([]uint32, n*2)
	g = make(args, 32)
	g.ptr(0, props)
	g.ptr(8, values)
	g.put32(16, n)
	g.put32(20, id)
	g.put32(24, drmModeObjectPlane)
	if drmIoctl(k.fd(), drmModeObjGetProp, g) != nil {
		return 0, false
	}
	for i := range min(int(g.u32(16)), int(n)) {
		p := make(args, 64)
		p.put32(16, props[i])
		if drmIoctl(k.fd(), drmModeGetProp, p) != nil {
			continue
		}
		name := p[24:56]
		if string(name[:5]) == "type\x00" {
			return uint64(values[2*i]) | uint64(values[2*i+1])<<32, true
		}
	}
	return 0, false
}

// newYUV makes a dumb buffer for a w×h 4:2:0 picture and its framebuffer. The pitch is rounded to 128
// so the chroma's, half of it, keeps the 64-byte alignment the display controller wants.
func (k *kmsOut) newYUV(w, h int) (yuvBuf, error) {
	pitch := (w + 127) &^ 127
	ch := (h + 1) / 2
	cr := make(args, 32)
	cr.put32(0, uint32(h+ch))
	cr.put32(4, uint32(pitch))
	cr.put32(8, 8)
	if err := drmIoctl(k.fd(), drmModeCreateDumb, cr); err != nil {
		return yuvBuf{}, fmt.Errorf("CREATE_DUMB: %w", err)
	}
	b := yuvBuf{kmsBuf: kmsBuf{handle: cr.u32(16)}, w: w, h: h, pitch: int(cr.u32(20))}
	size := int(cr.u64(24))
	if b.pitch%128 != 0 || b.pitch*(h+ch) > size {
		k.freeBuf(b.kmsBuf)
		return yuvBuf{}, fmt.Errorf("a YUV buffer with pitch %d", b.pitch)
	}
	fb := make(args, 104)
	fb.put32(4, uint32(w))
	fb.put32(8, uint32(h))
	fb.put32(12, fourccYUV420)
	for i, off := range []int{0, b.pitch * h, b.pitch*h + b.pitch/2*ch} {
		fb.put32(20+4*i, b.handle)
		p := b.pitch
		if i > 0 {
			p = b.pitch / 2
		}
		fb.put32(36+4*i, uint32(p))
		fb.put32(52+4*i, uint32(off))
	}
	if err := drmIoctl(k.fd(), drmModeAddFB2, fb); err != nil {
		k.freeBuf(b.kmsBuf)
		return yuvBuf{}, fmt.Errorf("ADDFB2: %w", err)
	}
	b.fb = fb.u32(0)
	if err := k.mapBuf(&b.kmsBuf, size); err != nil {
		k.freeBuf(b.kmsBuf)
		return yuvBuf{}, err
	}
	return b, nil
}

// setPlane shows fb on plane, its src rectangle (whole pixels) scaled to dst; fb 0 turns the plane off.
// Through the atomic helpers this waits for the vblank it lands on.
func (k *kmsOut) setPlane(plane, fb uint32, dst, src image.Rectangle) error {
	a := make(args, 48)
	a.put32(0, plane)
	if fb != 0 {
		a.put32(4, k.crtc)
		a.put32(8, fb)
		a.put32(16, uint32(int32(dst.Min.X)))
		a.put32(20, uint32(int32(dst.Min.Y)))
		a.put32(24, uint32(dst.Dx()))
		a.put32(28, uint32(dst.Dy()))
		a.put32(32, uint32(src.Min.X)<<16)
		a.put32(36, uint32(src.Min.Y)<<16)
		a.put32(40, uint32(src.Dy())<<16)
		a.put32(44, uint32(src.Dx())<<16)
	}
	return drmIoctl(k.fd(), drmModeSetPlane, a)
}

// I420Size is the bytes of a w×h I420 picture as GStreamer lays one out with no padding of its own:
// rows of Y rounded to 4 bytes, of U and V to 4 bytes of half the width, rounded up.
func I420Size(w, h int) int {
	ys, cs := (w+3)&^3, ((w+1)/2+3)&^3
	return ys*h + 2*cs*((h+1)/2)
}

// CanPresentYUV is whether PresentYUV puts pictures on a plane: KMS with an overlay that takes 4:2:0.
func (d *Device) CanPresentYUV() bool {
	return d.kms != nil && d.kms.video != nil && !rotated
}

// PresentYUV shows a w×h I420 picture scaled into dst on the panel, the canvas's rectangle over drawn
// over it (the video's controls), and black around it. The picture is copied as it is into a buffer of
// the overlay plane; the display controller scales and converts it.
func (d *Device) PresentYUV(pix []byte, w, h int, dst, over image.Rectangle) error {
	if err := d.settle(); err != nil {
		return err
	}
	if !d.CanPresentYUV() {
		return errors.New("screen: no video plane")
	}
	if w <= 0 || h <= 0 || w&1 != 0 || h&1 != 0 || len(pix) < I420Size(w, h) {
		return fmt.Errorf("screen: a %dx%d picture of %d bytes", w, h, len(pix))
	}
	k, v := d.kms, d.kms.video
	dst = dst.Intersect(image.Rect(0, 0, d.panelW, d.panelH))
	if dst.Empty() {
		return errors.New("screen: the picture is off the panel")
	}
	if !v.blackPage {
		// Black on the pages, once: the bars around the picture.
		if err := d.blackPages(); err != nil {
			return err
		}
		v.blackPage = true
	}
	if len(v.yuv) == 0 || v.yuv[0].w != w || v.yuv[0].h != h {
		d.freeYUV()
		for range 2 {
			b, err := k.newYUV(w, h)
			if err != nil {
				d.freeYUV()
				return fmt.Errorf("screen: %w", err)
			}
			v.yuv = append(v.yuv, b)
		}
		slog.Info("screen: video on the overlay plane", "picture", image.Pt(w, h), "shown", dst, "pitch", v.yuv[0].pitch)
	}
	b := v.yuv[v.next]
	copyI420(b.mem, b.pitch, pix, w, h)
	if err := k.setPlane(v.overlay, b.fb, dst, image.Rect(0, 0, w, h)); err != nil {
		return fmt.Errorf("screen: SETPLANE: %w", err)
	}
	v.on = true
	v.next = (v.next + 1) % len(v.yuv)
	return d.controls(over)
}

// copyI420 copies a GStreamer I420 picture into a buffer with pitch for Y and pitch/2 for U and V.
func copyI420(dst []byte, pitch int, src []byte, w, h int) {
	ys, cs := (w+3)&^3, ((w+1)/2+3)&^3
	cw, ch := (w+1)/2, (h+1)/2
	for y := range h {
		copy(dst[y*pitch:y*pitch+w], src[y*ys:y*ys+w])
	}
	so, do := ys*h, pitch*h
	for range 2 {
		for y := range ch {
			copy(dst[do+y*pitch/2:do+y*pitch/2+cw], src[so+y*cs:so+y*cs+cw])
		}
		so += cs * ch
		do += pitch / 2 * ch
	}
}

// blackPages paints black on the page not on show and flips to it.
func (d *Device) blackPages() error {
	next := (d.page + 1) % d.pages
	dst, err := d.pageFor(next)
	if err != nil {
		return err
	}
	row := d.panelW * 4
	if len(d.row) < row {
		d.row = make([]byte, row)
	}
	black := d.row[:row]
	for i := range black {
		black[i] = 0
	}
	for i := 3; i < row; i += 4 {
		black[i] = 0xff
	}
	d.markStale(next)
	for y := range d.panelH {
		copy(dst[y*d.line:y*d.line+row], black)
	}
	return d.pan(next)
}

// controls puts the canvas's rectangle over on the cursor plane, with its alpha, or takes it off.
func (d *Device) controls(over image.Rectangle) error {
	k, v := d.kms, d.kms.video
	over = over.Intersect(d.canvas.Rect).Intersect(image.Rect(0, 0, d.panelW, d.panelH))
	if over.Empty() || v.cursor == 0 {
		if !v.ctlRect.Empty() {
			v.ctlRect = image.Rectangle{}
			return k.setPlane(v.cursor, 0, image.Rectangle{}, image.Rectangle{})
		}
		return nil
	}
	if v.ctl.mem == nil {
		b, err := k.newBuf(d.panelW, d.panelH)
		if err != nil {
			return fmt.Errorf("screen: the controls' buffer: %w", err)
		}
		// newBuf's framebuffer is XRGB; the controls need their alpha.
		k.rmFB(b.fb)
		fb, err := k.addFB2(b.handle, d.panelW, d.panelH, k.pitch, fourccARGB8888)
		if err != nil {
			k.freeBuf(b)
			return fmt.Errorf("screen: the controls' framebuffer: %w", err)
		}
		b.fb = fb
		v.ctl, v.ctlPitch = b, k.pitch
		v.ctlShadow = make([]byte, k.pitch*d.panelH)
	}
	// The rows of the rectangle, premultiplied RGBA to BGRA, written where they changed.
	c := d.canvas
	n := over.Dx() * 4
	if len(d.row) < n {
		d.row = make([]byte, d.panelW*4)
	}
	row := d.row[:n]
	for y := over.Min.Y; y < over.Max.Y; y++ {
		src := c.Pix[y*c.Stride+over.Min.X*4 : y*c.Stride+over.Max.X*4]
		for i := 0; i < n; i += 4 {
			row[i], row[i+1], row[i+2], row[i+3] = src[i+2], src[i+1], src[i], src[i+3]
		}
		at := y*v.ctlPitch + over.Min.X*4
		if string(v.ctlShadow[at:at+n]) == string(row) {
			continue
		}
		copy(v.ctlShadow[at:at+n], row)
		copy(v.ctl.mem[at:at+n], row)
	}
	if over == v.ctlRect {
		return nil
	}
	v.ctlRect = over
	if err := k.setPlane(v.cursor, v.ctl.fb, over, over); err != nil {
		v.ctlRect = image.Rectangle{}
		return fmt.Errorf("screen: the controls' plane: %w", err)
	}
	return nil
}

// leaveVideo takes the video's planes off, before the pages show anything else.
func (d *Device) leaveVideo() {
	if d.kms == nil || d.kms.video == nil {
		return
	}
	k, v := d.kms, d.kms.video
	if v.on {
		_ = k.setPlane(v.overlay, 0, image.Rectangle{}, image.Rectangle{})
		v.on = false
	}
	if !v.ctlRect.Empty() {
		_ = k.setPlane(v.cursor, 0, image.Rectangle{}, image.Rectangle{})
		v.ctlRect = image.Rectangle{}
	}
	v.blackPage = false
	d.freeYUV()
}

func (d *Device) freeYUV() {
	v := d.kms.video
	if v.on {
		_ = d.kms.setPlane(v.overlay, 0, image.Rectangle{}, image.Rectangle{})
		v.on = false
	}
	for _, b := range v.yuv {
		d.kms.freeBuf(b.kmsBuf)
	}
	v.yuv, v.next = nil, 0
}

func (k *kmsOut) rmFB(fb uint32) {
	a := make(args, 4)
	a.put32(0, fb)
	_ = drmIoctl(k.fd(), drmModeRmFB, a)
}

// addFB2 is a single-plane framebuffer of format on a buffer.
func (k *kmsOut) addFB2(handle uint32, w, h, pitch int, format uint32) (uint32, error) {
	fb := make(args, 104)
	fb.put32(4, uint32(w))
	fb.put32(8, uint32(h))
	fb.put32(12, format)
	fb.put32(20, handle)
	fb.put32(36, uint32(pitch))
	if err := drmIoctl(k.fd(), drmModeAddFB2, fb); err != nil {
		return 0, err
	}
	return fb.u32(0), nil
}
