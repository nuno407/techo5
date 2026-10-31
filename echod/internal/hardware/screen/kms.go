//go:build !dot && !panel

package screen

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// KMS: the panel through the kernel's mode setting (DRM) instead of its fbdev emulation, where the
// board has one worth it (the Surface RT's tegra-drm). The emulation gives a single page that is
// scanned out while it is written, so a frame can be seen half drawn; here the daemon is the DRM
// master and owns two dumb buffers, draws on the one not on show and flips to it at the next vblank
// (DRM_MODE_PAGE_FLIP_EVENT). Present's logic stays as it is: the two buffers are its two pages, each
// with its RAM shadow, so a frame still writes only the rows that differ from what that page holds.
//
// Everything is ioctls on the card's fd, packed by hand into byte slices: the structs hold __u64s,
// which the kernel aligns to 8 on 32-bit ARM and Go only to 4, so a Go struct would not match.
//
// The emulation is still there underneath. When the daemon closes the card the kernel puts the
// emulation's own mode back (drm_fb_helper's lastclose), which is why Close copies the last frame
// into /dev/fb0 first: the panel then goes on showing it, as it does with the framebuffer path.

const (
	drmIoctlBase = 'd'

	drmModeGetRes     = 0xa0
	drmModeGetCrtc    = 0xa1
	drmModeSetCrtc    = 0xa2
	drmModeGetEncoder = 0xa6
	drmModeGetConn    = 0xa7
	drmModeAddFB      = 0xae
	drmModeRmFB       = 0xaf
	drmModePageFlip   = 0xb0
	drmModeCreateDumb = 0xb2
	drmModeMapDumb    = 0xb3
	drmModeDestroyDmb = 0xb4
	drmModeGetPlaneRs = 0xb5
	drmModeGetPlane   = 0xb6
	drmModeSetPlane   = 0xb7
	drmModeAddFB2     = 0xb8
	drmPrimeFDToHndl  = 0x2e
	drmGemClose       = 0x09

	drmModeConnected     = 1
	drmModeTypePreferred = 1 << 3
	drmPageFlipEvent     = 1
	drmEventFlipComplete = 2

	modeInfoSize = 68 // struct drm_mode_modeinfo

	// kmsFlipTimeout is how long a flip may take before it is given up on: a vblank is 16 ms, so a
	// flip that has not completed in this long never will (the CRTC was turned off under it).
	kmsFlipTimeout = 200 * time.Millisecond
)

// drmIoc is _IOWR('d', nr, size).
func drmIoc(nr uintptr, size int) uintptr {
	return 3<<30 | uintptr(size)<<16 | drmIoctlBase<<8 | nr
}

// drmIoctl runs DRM ioctl nr with buf as its argument, in and out, again while it is interrupted.
func drmIoctl(fd uintptr, nr uintptr, buf []byte) error {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, drmIoc(nr, len(buf)), uintptr(p))
		if e == syscall.EINTR || e == syscall.EAGAIN {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// args is an ioctl argument packed by hand: little-endian fields at byte offsets.
type args []byte

func (a args) u32(at int) uint32      { return binary.LittleEndian.Uint32(a[at:]) }
func (a args) u64(at int) uint64      { return binary.LittleEndian.Uint64(a[at:]) }
func (a args) put32(at int, v uint32) { binary.LittleEndian.PutUint32(a[at:], v) }
func (a args) put64(at int, v uint64) { binary.LittleEndian.PutUint64(a[at:], v) }
func (a args) ptr(at int, b []uint32) { // a __u64 user pointer to b's first element
	if len(b) > 0 {
		a.put64(at, uint64(uintptr(unsafe.Pointer(&b[0]))))
	}
}

// kmsMode is a struct drm_mode_modeinfo as the kernel gave it, with the fields read from it.
type kmsMode struct {
	raw       [modeInfoSize]byte
	w, h      int
	refresh   int
	preferred bool
	name      string
}

func parseMode(b []byte) kmsMode {
	var m kmsMode
	copy(m.raw[:], b)
	a := args(m.raw[:])
	m.w = int(binary.LittleEndian.Uint16(a[4:]))
	m.h = int(binary.LittleEndian.Uint16(a[14:]))
	m.refresh = int(a.u32(24))
	m.preferred = a.u32(32)&drmModeTypePreferred != 0
	n := 0
	for n < 32 && a[36+n] != 0 {
		n++
	}
	m.name = string(a[36 : 36+n])
	return m
}

// pickMode is the connector's preferred mode, else its first: the kernel lists the native one first.
func pickMode(modes []kmsMode) (kmsMode, bool) {
	for _, m := range modes {
		if m.preferred {
			return m, true
		}
	}
	if len(modes) > 0 {
		return modes[0], true
	}
	return kmsMode{}, false
}

// kmsBuf is one page: a dumb buffer, its framebuffer and its mapping.
type kmsBuf struct {
	handle, fb uint32
	mem        []byte
}

// kmsOut is the card, the CRTC and connector it drives and the two pages.
type kmsOut struct {
	f     *os.File
	path  string
	crtc  uint32
	conn  uint32
	mode  kmsMode
	pitch int
	bufs  []kmsBuf
	set   bool      // the CRTC shows one of ours (the first pan sets the mode)
	flip  bool      // a flip was asked for and has not completed
	slow  int       // flips that timed out, for the log
	video *kmsVideo // the planes for a video (kms_video.go); nil without an overlay for it

	// The hardware decoder's picture buffers (kms_picture.go), made and let go by the video's goroutine.
	picMu  sync.Mutex
	picGen int
	pics   *picSet
}

// openKMS opens the first card with a connected connector and sets up two pages in its native mode.
// Nothing is shown yet: the first pan sets the mode, with the first frame already drawn.
func openKMS() (*kmsOut, error) {
	cards, _ := filepath.Glob("/dev/dri/card*")
	sort.Strings(cards)
	var errs []error
	for _, path := range cards {
		k, err := openCard(path)
		if err == nil {
			return k, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	if len(errs) == 0 {
		return nil, errors.New("no DRM card")
	}
	return nil, errors.Join(errs...)
}

func openCard(path string) (*kmsOut, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	k := &kmsOut{f: f, path: path}
	if err := k.find(); err != nil {
		f.Close()
		return nil, err
	}
	for range 2 {
		b, err := k.newBuf(k.mode.w, k.mode.h)
		if err != nil {
			k.close()
			return nil, err
		}
		k.bufs = append(k.bufs, b)
	}
	k.findVideoPlanes()
	return k, nil
}

func (k *kmsOut) fd() uintptr { return k.f.Fd() }

// find picks the connected connector, its mode, and the CRTC its encoder drives.
func (k *kmsOut) find() error {
	res := make(args, 64)
	if err := drmIoctl(k.fd(), drmModeGetRes, res); err != nil {
		return fmt.Errorf("GETRESOURCES: %w", err)
	}
	crtcs := make([]uint32, res.u32(36))
	conns := make([]uint32, res.u32(40))
	encs := make([]uint32, res.u32(44))
	res = make(args, 64)
	res.ptr(8, crtcs)
	res.ptr(16, conns)
	res.ptr(24, encs)
	res.put32(36, uint32(len(crtcs)))
	res.put32(40, uint32(len(conns)))
	res.put32(44, uint32(len(encs)))
	if err := drmIoctl(k.fd(), drmModeGetRes, res); err != nil {
		return fmt.Errorf("GETRESOURCES: %w", err)
	}
	for _, c := range conns {
		modes, enc, possible, ok := k.connector(c)
		if !ok {
			continue
		}
		mode, ok := pickMode(modes)
		if !ok {
			continue
		}
		crtc := uint32(0)
		if enc != 0 {
			e := make(args, 20)
			e.put32(0, enc)
			if drmIoctl(k.fd(), drmModeGetEncoder, e) == nil {
				crtc = e.u32(8)
				possible = e.u32(12)
			}
		}
		if crtc == 0 {
			for i, id := range crtcs {
				if possible&(1<<i) != 0 {
					crtc = id
					break
				}
			}
		}
		if crtc == 0 {
			continue
		}
		k.conn, k.crtc, k.mode = c, crtc, mode
		return nil
	}
	return errors.New("no connected display")
}

// connector reads connector id: its modes and its encoder, and whether it is connected.
func (k *kmsOut) connector(id uint32) (modes []kmsMode, enc, possible uint32, ok bool) {
	c := make(args, 80)
	c.put32(48, id)
	if err := drmIoctl(k.fd(), drmModeGetConn, c); err != nil {
		return nil, 0, 0, false
	}
	nModes, nProps, nEncs := c.u32(32), c.u32(36), c.u32(40)
	if c.u32(60) != drmModeConnected || nModes == 0 {
		return nil, 0, 0, false
	}
	modeBuf := make([]uint32, nModes*modeInfoSize/4)
	props := make([]uint32, nProps)
	values := make([]uint32, nProps*2) // __u64 each
	encs := make([]uint32, nEncs)
	c = make(args, 80)
	c.ptr(0, encs)
	c.ptr(8, modeBuf)
	c.ptr(16, props)
	c.ptr(24, values)
	c.put32(32, nModes)
	c.put32(36, nProps)
	c.put32(40, nEncs)
	c.put32(48, id)
	if err := drmIoctl(k.fd(), drmModeGetConn, c); err != nil {
		return nil, 0, 0, false
	}
	n := min(int(c.u32(32)), int(nModes))
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&modeBuf[0])), len(modeBuf)*4)
	for i := range n {
		modes = append(modes, parseMode(raw[i*modeInfoSize:(i+1)*modeInfoSize]))
	}
	return modes, c.u32(44), 0, true
}

// newBuf makes a w×h XRGB8888 dumb buffer, its framebuffer and its mapping. A dumb buffer comes
// zeroed.
func (k *kmsOut) newBuf(w, h int) (kmsBuf, error) {
	cr := make(args, 32)
	cr.put32(0, uint32(h))
	cr.put32(4, uint32(w))
	cr.put32(8, 32)
	if err := drmIoctl(k.fd(), drmModeCreateDumb, cr); err != nil {
		return kmsBuf{}, fmt.Errorf("CREATE_DUMB: %w", err)
	}
	b := kmsBuf{handle: cr.u32(16)}
	pitch, size := int(cr.u32(20)), int(cr.u64(24))
	if k.pitch == 0 {
		k.pitch = pitch
	}
	fb := make(args, 28)
	fb.put32(4, uint32(w))
	fb.put32(8, uint32(h))
	fb.put32(12, uint32(pitch))
	fb.put32(16, 32)
	fb.put32(20, 24)
	fb.put32(24, b.handle)
	if err := drmIoctl(k.fd(), drmModeAddFB, fb); err != nil {
		k.freeBuf(b)
		return kmsBuf{}, fmt.Errorf("ADDFB: %w", err)
	}
	b.fb = fb.u32(0)
	if err := k.mapBuf(&b, size); err != nil {
		k.freeBuf(b)
		return kmsBuf{}, err
	}
	return b, nil
}

// mapBuf maps a dumb buffer of size bytes into b.mem.
func (k *kmsOut) mapBuf(b *kmsBuf, size int) error {
	mp := make(args, 16)
	mp.put32(0, b.handle)
	if err := drmIoctl(k.fd(), drmModeMapDumb, mp); err != nil {
		return fmt.Errorf("MAP_DUMB: %w", err)
	}
	mem, err := unix.Mmap(int(k.fd()), int64(mp.u64(8)), size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	b.mem = mem
	return nil
}

func (k *kmsOut) freeBuf(b kmsBuf) {
	if b.mem != nil {
		_ = unix.Munmap(b.mem)
	}
	if b.fb != 0 {
		a := make(args, 4)
		a.put32(0, b.fb)
		_ = drmIoctl(k.fd(), drmModeRmFB, a)
	}
	if b.handle != 0 {
		a := make(args, 4)
		a.put32(0, b.handle)
		_ = drmIoctl(k.fd(), drmModeDestroyDmb, a)
	}
}

// show puts page i on the panel: the first time by setting the mode with it, after that by a flip at
// the next vblank, which completes on its own (wait).
func (k *kmsOut) show(i int) error {
	if err := k.wait(); err != nil {
		return err
	}
	if !k.set {
		c := make(args, 104)
		conns := []uint32{k.conn}
		c.ptr(0, conns)
		c.put32(8, 1)
		c.put32(12, k.crtc)
		c.put32(16, k.bufs[i].fb)
		c.put32(32, 1) // mode_valid
		copy(c[36:], k.mode.raw[:])
		if err := drmIoctl(k.fd(), drmModeSetCrtc, c); err != nil {
			return fmt.Errorf("screen: SETCRTC: %w", err)
		}
		k.set = true
		return nil
	}
	p := make(args, 24)
	p.put32(0, k.crtc)
	p.put32(4, k.bufs[i].fb)
	p.put32(8, drmPageFlipEvent)
	if err := drmIoctl(k.fd(), drmModePageFlip, p); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			// Still flipping (a flip that timed out): wait for one more vblank and ask again.
			k.flip = true
			if werr := k.wait(); werr != nil {
				return werr
			}
			err = drmIoctl(k.fd(), drmModePageFlip, p)
		}
		if err != nil {
			return fmt.Errorf("screen: PAGE_FLIP: %w", err)
		}
	}
	k.flip = true
	return nil
}

// wait waits for the flip asked for last, if one is still on its way: only then is the page that was
// on show free to draw on.
func (k *kmsOut) wait() error {
	if !k.flip {
		return nil
	}
	deadline := time.Now().Add(kmsFlipTimeout)
	buf := make([]byte, 256)
	for k.flip {
		left := time.Until(deadline)
		if left <= 0 {
			k.flip = false
			k.slow++
			if k.slow == 1 || k.slow%100 == 0 {
				slog.Warn("screen: a page flip did not complete", "times", k.slow)
			}
			return nil
		}
		fds := []unix.PollFd{{Fd: int32(k.fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("screen: poll: %w", err)
		}
		if n == 0 {
			continue
		}
		r, err := unix.Read(int(k.fd()), buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return fmt.Errorf("screen: DRM event: %w", err)
		}
		if flipDone(buf[:r]) {
			k.flip = false
		}
	}
	return nil
}

// flipDone is whether the events read from the card include a completed flip.
func flipDone(b []byte) bool {
	for len(b) >= 8 {
		typ, n := binary.LittleEndian.Uint32(b), int(binary.LittleEndian.Uint32(b[4:]))
		if typ == drmEventFlipComplete {
			return true
		}
		if n < 8 || n > len(b) {
			return false
		}
		b = b[n:]
	}
	return false
}

// crtcIndex is the CRTC's place in the card's list, which the planes' possible_crtcs bits count by.
func (k *kmsOut) crtcIndex() int {
	res := make(args, 64)
	if drmIoctl(k.fd(), drmModeGetRes, res) != nil {
		return -1
	}
	crtcs := make([]uint32, res.u32(36))
	res = make(args, 64)
	res.ptr(8, crtcs)
	res.put32(36, uint32(len(crtcs)))
	if drmIoctl(k.fd(), drmModeGetRes, res) != nil {
		return -1
	}
	for i, id := range crtcs {
		if id == k.crtc {
			return i
		}
	}
	return -1
}

func (k *kmsOut) close() {
	_ = k.wait()
	k.picMu.Lock()
	if k.pics != nil {
		k.pics.done = true
		k.dropPictures(true)
	}
	k.picMu.Unlock()
	if v := k.video; v != nil {
		for _, b := range v.yuv {
			k.freeBuf(b.kmsBuf)
		}
		if v.ctl.mem != nil {
			k.freeBuf(v.ctl)
		}
		k.video = nil
	}
	for _, b := range k.bufs {
		k.freeBuf(b)
	}
	k.bufs = nil
	_ = k.f.Close()
}

// String describes the output, for the log.
func (k *kmsOut) String() string {
	video := "no video plane"
	if k.video != nil {
		video = fmt.Sprintf("video on plane %d, controls on plane %d", k.video.overlay, k.video.cursor)
	}
	return fmt.Sprintf("%s crtc %d connector %d mode %s@%d, %s", k.path, k.crtc, k.conn, k.mode.name, k.mode.refresh, video)
}

// useKMS is whether Open tries KMS first: on the Surface RT, unless TECHO5_SCREEN=fbdev asks for the
// framebuffer (for comparing the two).
func useKMS() bool {
	return layout.SurfaceRT() && os.Getenv("TECHO5_SCREEN") != "fbdev"
}

// openKMSDevice is a Device on KMS's two pages.
func openKMSDevice() (*Device, error) {
	k, err := openKMS()
	if err != nil {
		return nil, err
	}
	d := &Device{kms: k, pages: len(k.bufs), panelW: k.mode.w, panelH: k.mode.h, line: k.pitch}
	d.pageBytes = d.line * d.panelH
	for _, b := range k.bufs {
		if len(b.mem) < d.pageBytes {
			k.close()
			return nil, fmt.Errorf("screen: a dumb buffer of %d bytes, want %d", len(b.mem), d.pageBytes)
		}
	}
	d.shift = [4]uint{16, 8, 0, 24} // XRGB8888, as the framebuffer emulation's
	// Dumb buffers come zeroed, so each page's shadow starts as zeros and is never read back from the
	// buffer itself: write-combined memory reads at 30 MB/s.
	d.shadow = make([][]byte, d.pages)
	for i := range d.shadow {
		d.shadow[i] = make([]byte, d.pageBytes)
	}
	d.row = make([]byte, d.panelW*4)
	d.page = 1 // so the first frame goes to page 0
	if rotated {
		d.canvas = image.NewRGBA(image.Rect(0, 0, d.panelH, d.panelW))
	} else {
		d.canvas = image.NewRGBA(image.Rect(0, 0, d.panelW, d.panelH))
	}
	return d, nil
}

// pageFor is page next's memory, once it is free to draw on: on KMS, once the flip away from it is
// done.
func (d *Device) pageFor(next int) ([]byte, error) {
	if d.kms == nil {
		return d.mem[next*d.pageBytes : (next+1)*d.pageBytes], nil
	}
	if err := d.kms.wait(); err != nil {
		return nil, err
	}
	return d.kms.bufs[next].mem[:d.pageBytes], nil
}

// leaveVideoFor is the pages about to show something of their own: the video's planes go.
func (d *Device) leaveVideoFor() {
	if d.kms != nil && d.kms.video != nil && (d.kms.video.on || d.kms.video.blackPage) {
		d.leaveVideo()
	}
}

// leaveKMS hands the panel back to the framebuffer emulation with the frame on show copied into it,
// so the panel keeps showing it once the kernel restores the emulation's mode, and closes the card.
func (d *Device) leaveKMS() {
	d.leaveVideoFor()
	k := d.kms
	d.kms = nil
	_ = k.wait()
	if d.page < len(d.shadow) && d.shadow[d.page] != nil {
		copyToFB(d.shadow[d.page], d.line, d.panelW, d.panelH)
	}
	k.close()
}

// copyToFB writes a frame of rows line bytes apart into /dev/fb0, where it is the same size.
func copyToFB(src []byte, line, w, h int) {
	f, err := os.OpenFile(altPath, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	var v varInfo
	var fx fixInfo
	if ioctl(f.Fd(), fbioGetVScreenInfo, unsafe.Pointer(&v)) != nil || ioctl(f.Fd(), fbioGetFScreenInfo, unsafe.Pointer(&fx)) != nil {
		return
	}
	if int(v.Xres) != w || int(v.Yres) != h || v.BitsPerPixel != 32 {
		return
	}
	fbLine := int(fx.LineLength)
	mem, err := unix.Mmap(int(f.Fd()), 0, fbLine*h, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return
	}
	defer unix.Munmap(mem)
	for y := range h {
		copy(mem[y*fbLine:y*fbLine+w*4], src[y*line:y*line+w*4])
	}
}
