//go:build !dot && !panel

package screen

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
	"syscall"

	"golang.org/x/sys/unix"
)

// The hardware decoder's pictures, shown where it wrote them: buffers of the card's, made here and
// handed to the decoder as dmabufs (hardware/vde imports them as its capture buffers), each picture
// put on the overlay plane as it is. Neither a core nor the decoder's own buffers copy a pixel of it,
// and none goes through the cache: the decoder writes with DMA and the display controller reads.
//
// One set of buffers at a time, for one video, made by the video's goroutine while the frame loop
// may be showing the last one's pictures: picMu keeps the two apart. A set the video is done with is
// let go once none of it is on the panel (an RMFB of the framebuffer on show would turn the plane off).

const (
	drmPrimeHandleToFD = 0x2d
	drmCloexecRDWR     = unix.O_CLOEXEC | unix.O_RDWR
)

// picBuf is one picture: Y, U and V in three dumb buffers, one framebuffer over them.
type picBuf struct {
	fb      uint32
	handles [3]uint32
	fds     [3]int
	pitch   [3]int
	size    [3]int
	mem     [3][]byte // mapped when asked for: a screenshot, or pictures read into them
}

// picSet is the buffers of one video.
type picSet struct {
	gen  int
	w, h int
	bufs []picBuf
	done bool // the video has let them go
}

// NewPictureBufs makes n buffers for w×h 4:2:0 pictures with these pitches and plane sizes, and gives
// back the dmabufs of each (Y, U, V) and the set's number, which PresentPicture names it by. A set
// made before goes.
func (d *Device) NewPictureBufs(n, w, h int, pitch, size [3]int) (gen int, fds [][3]int, err error) {
	if !d.CanPresentYUV() {
		return 0, nil, errors.New("screen: no video plane")
	}
	k := d.kms
	k.picMu.Lock()
	defer k.picMu.Unlock()
	k.dropPictures(true)
	k.picGen++
	set := &picSet{gen: k.picGen, w: w, h: h}
	for range n {
		b, err := k.newPicBuf(w, h, pitch, size)
		if err != nil {
			k.freePicSet(set)
			return 0, nil, fmt.Errorf("screen: a picture buffer: %w", err)
		}
		set.bufs = append(set.bufs, b)
		fds = append(fds, b.fds)
	}
	k.pics = set
	slog.Info("screen: decoder pictures on the overlay plane", "buffers", n, "picture", image.Pt(w, h), "pitch", pitch[0])
	return set.gen, fds, nil
}

// ReleasePictureBufs is the video done with set gen: it goes now, or once none of it is on the panel.
func (d *Device) ReleasePictureBufs(gen int) {
	if d.kms == nil {
		return
	}
	k := d.kms
	k.picMu.Lock()
	defer k.picMu.Unlock()
	if k.pics == nil || k.pics.gen != gen {
		return
	}
	k.pics.done = true
	if k.video == nil || !k.video.on || k.video.showGen != gen {
		k.dropPictures(false)
	}
}

// dropPictures frees the set of pictures, off the plane first if one is on it; force frees a set the
// video still uses too. Wants picMu.
func (k *kmsOut) dropPictures(force bool) {
	set := k.pics
	if set == nil || (!set.done && !force) {
		return
	}
	if v := k.video; v != nil && v.on && v.showGen == set.gen {
		_ = k.setPlane(v.overlay, 0, image.Rectangle{}, image.Rectangle{})
		v.on, v.showGen = false, 0
	}
	k.freePicSet(set)
	k.pics = nil
}

func (k *kmsOut) freePicSet(set *picSet) {
	for _, b := range set.bufs {
		k.freePicBuf(b)
	}
	set.bufs = nil
}

func (k *kmsOut) newPicBuf(w, h int, pitch, size [3]int) (picBuf, error) {
	b := picBuf{pitch: pitch, size: size, fds: [3]int{-1, -1, -1}}
	for i := range 3 {
		if pitch[i] <= 0 || size[i] < pitch[i] || pitch[i]%8 != 0 {
			k.freePicBuf(b)
			return picBuf{}, fmt.Errorf("plane %d: pitch %d, %d bytes", i, pitch[i], size[i])
		}
		rows := (size[i] + pitch[i] - 1) / pitch[i]
		cr := make(args, 32)
		cr.put32(0, uint32(rows))
		cr.put32(4, uint32(pitch[i]))
		cr.put32(8, 8)
		if err := drmIoctl(k.fd(), drmModeCreateDumb, cr); err != nil {
			k.freePicBuf(b)
			return picBuf{}, fmt.Errorf("CREATE_DUMB: %w", err)
		}
		b.handles[i] = cr.u32(16)
		if int(cr.u32(20)) != pitch[i] {
			k.freePicBuf(b)
			return picBuf{}, fmt.Errorf("a dumb buffer with pitch %d, want %d", cr.u32(20), pitch[i])
		}
		b.size[i] = int(cr.u64(24))
		p := make(args, 12)
		p.put32(0, b.handles[i])
		p.put32(4, drmCloexecRDWR)
		if err := drmIoctl(k.fd(), drmPrimeHandleToFD, p); err != nil {
			k.freePicBuf(b)
			return picBuf{}, fmt.Errorf("PRIME_HANDLE_TO_FD: %w", err)
		}
		b.fds[i] = int(int32(p.u32(8)))
	}
	fb := make(args, 104)
	fb.put32(4, uint32(w))
	fb.put32(8, uint32(h))
	fb.put32(12, fourccYUV420)
	for i := range 3 {
		fb.put32(20+4*i, b.handles[i])
		fb.put32(36+4*i, uint32(pitch[i]))
	}
	if err := drmIoctl(k.fd(), drmModeAddFB2, fb); err != nil {
		k.freePicBuf(b)
		return picBuf{}, fmt.Errorf("ADDFB2: %w", err)
	}
	b.fb = fb.u32(0)
	return b, nil
}

func (k *kmsOut) freePicBuf(b picBuf) {
	if b.fb != 0 {
		k.rmFB(b.fb)
	}
	for i := range 3 {
		if b.mem[i] != nil {
			_ = unix.Munmap(b.mem[i])
		}
		if b.fds[i] > 0 {
			_ = unix.Close(b.fds[i])
		}
		if b.handles[i] != 0 {
			a := make(args, 4)
			a.put32(0, b.handles[i])
			_ = drmIoctl(k.fd(), drmModeDestroyDmb, a)
		}
	}
}

// mapPicBuf maps picture b's three planes, once. Wants picMu.
func (k *kmsOut) mapPicBuf(b *picBuf) error {
	for p := range 3 {
		if b.mem[p] != nil {
			continue
		}
		mp := make(args, 16)
		mp.put32(0, b.handles[p])
		if err := drmIoctl(k.fd(), drmModeMapDumb, mp); err != nil {
			return fmt.Errorf("MAP_DUMB: %w", err)
		}
		mem, err := unix.Mmap(int(k.fd()), int64(mp.u64(8)), b.size[p], unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap: %w", err)
		}
		b.mem[p] = mem
	}
	return nil
}

// PictureMem is picture i of set gen's three planes, Y, U and V, mapped to be written: for pictures
// the cores decode, read straight into them. Write-combined: written in order, never read.
func (d *Device) PictureMem(gen, i int) ([3][]byte, error) {
	if d.kms == nil {
		return [3][]byte{}, errors.New("screen: no pictures")
	}
	k := d.kms
	k.picMu.Lock()
	defer k.picMu.Unlock()
	set := k.pics
	if set == nil || set.gen != gen || i < 0 || i >= len(set.bufs) {
		return [3][]byte{}, errors.New("screen: the picture is gone")
	}
	b := &set.bufs[i]
	if err := k.mapPicBuf(b); err != nil {
		return [3][]byte{}, err
	}
	return b.mem, nil
}

// PresentPicture shows picture i of set gen, its src rectangle scaled into dst, black around it, the
// canvas's rectangle over drawn over it (the video's controls). A set that has gone is an error.
func (d *Device) PresentPicture(gen, i int, src, dst, over image.Rectangle) error {
	if err := d.settle(); err != nil {
		return err
	}
	if !d.CanPresentYUV() {
		return errors.New("screen: no video plane")
	}
	k, v := d.kms, d.kms.video
	dst = dst.Intersect(image.Rect(0, 0, d.panelW, d.panelH))
	if dst.Empty() {
		return errors.New("screen: the picture is off the panel")
	}
	if !v.blackPage {
		if err := d.blackPages(); err != nil {
			return err
		}
		v.blackPage = true
	}
	if len(v.yuv) > 0 {
		d.freeYUV() // a copied picture's buffers, from a video before
	}
	k.picMu.Lock()
	set := k.pics
	if set == nil || set.gen != gen || i < 0 || i >= len(set.bufs) {
		k.picMu.Unlock()
		return fmt.Errorf("screen: picture %d of set %d is gone", i, gen)
	}
	src = src.Intersect(image.Rect(0, 0, set.w, set.h))
	err := k.setPlane(v.overlay, set.bufs[i].fb, dst, src)
	if err == nil {
		v.on, v.showGen = true, gen
	}
	k.picMu.Unlock()
	if err != nil {
		return fmt.Errorf("screen: SETPLANE: %w", err)
	}
	return d.controls(over)
}

// PictureI420 reads picture i of set gen's src rectangle back as I420 in GStreamer's layout (I420Size),
// for a screenshot: slowly, the buffers being write-combined.
func (d *Device) PictureI420(gen, i int, src image.Rectangle) ([]byte, error) {
	if d.kms == nil {
		return nil, errors.New("screen: no pictures")
	}
	k := d.kms
	k.picMu.Lock()
	defer k.picMu.Unlock()
	set := k.pics
	if set == nil || set.gen != gen || i < 0 || i >= len(set.bufs) {
		return nil, errors.New("screen: the picture is gone")
	}
	b := &set.bufs[i]
	src = src.Intersect(image.Rect(0, 0, set.w, set.h))
	w, h := src.Dx()&^1, src.Dy()&^1
	out := make([]byte, 0, I420Size(w, h))
	strides := [3]int{(w + 3) &^ 3, (w/2 + 3) &^ 3, (w/2 + 3) &^ 3}
	if err := k.mapPicBuf(b); err != nil {
		return nil, err
	}
	for p := range 3 {
		x0, y0, pw, ph := src.Min.X, src.Min.Y, w, h
		if p > 0 {
			x0, y0, pw, ph = x0/2, y0/2, w/2, h/2
		}
		for y := y0; y < y0+ph; y++ {
			at := y*b.pitch[p] + x0
			if at+pw > len(b.mem[p]) {
				return nil, syscall.EINVAL
			}
			out = append(out, b.mem[p][at:at+pw]...)
			out = append(out, make([]byte, strides[p]-pw)...)
		}
	}
	return out, nil
}
