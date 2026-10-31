package vde

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The kernel side: tegra-vde's V4L2 stateless decoder node and its media device, through ioctls on
// structs packed by hand (the kernel lays out __u64s 8-aligned on 32-bit ARM, Go only 4). The layouts
// are the time64 ABI's, which this kernel and Alpine's musl both use: struct v4l2_buffer is 80 bytes.

const (
	vidiocQueryCap  = 0x80685600
	vidiocSFmt      = 0xc0cc5605
	vidiocGFmt      = 0xc0cc5604
	vidiocReqBufs   = 0xc0145608
	vidiocQueryBuf  = 0xc0505609
	vidiocQBuf      = 0xc050560f
	vidiocDQBuf     = 0xc0505611
	vidiocStreamOn  = 0x40045612
	vidiocStreamOff = 0x40045613
	vidiocSExtCtrls = 0xc0185648

	mediaIocRequestAlloc = 0x80047c05
	mediaRequestQueue    = 0x7c80
	mediaRequestReinit   = 0x7c81

	bufTypeCapture = 9  // V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE
	bufTypeOutput  = 10 // V4L2_BUF_TYPE_VIDEO_OUTPUT_MPLANE
	memMMAP        = 1
	memDMABUF      = 4

	bufFlagError         = 0x40
	bufFlagRequestFD     = 0x00800000
	bufFlagTimestampCopy = 0x4000

	fourccH264Slice = 0x34363253 // 'S264'
	fourccYUV420M   = 0x32314d59 // 'YM12'

	ctrlWhichRequest = 0x0f010000
	cidSPS           = 0xa40902
	cidPPS           = 0xa40903
	cidDecodeParams  = 0xa40907

	bufSize    = 80
	planeSize  = 60
	formatSize = 204

	spsSize    = 1048
	ppsSize    = 12
	paramsSize = 560
	dpbEntries = 16
)

// device is an open tegra-vde with its media device.
type device struct {
	video, media *os.File
	reqs         []int // media requests, one per output buffer
	out          [][]byte
	outLen       []int
	outBusy      []bool
	capN         int
	planes       int
	fmt          Format
}

// Format is what the decoder writes: three planes (Y, U, V), each Pitch bytes a row and Size bytes in
// all, for a Width×Height coded picture.
type Format struct {
	Width, Height int
	Pitch, Size   [3]int
}

// Find is the decoder's video node and its media node, or an error saying why there is none.
func Find() (video, media string, err error) {
	names, _ := filepath.Glob("/sys/class/video4linux/video*/name")
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil || strings.TrimSpace(string(b)) != "tegra-vde" {
			continue
		}
		dir := filepath.Dir(n)
		ms, _ := filepath.Glob(filepath.Join(dir, "device", "media*"))
		if len(ms) == 0 {
			return "", "", errors.New("vde: tegra-vde has no media device")
		}
		return "/dev/" + filepath.Base(dir), "/dev/" + filepath.Base(ms[0]), nil
	}
	return "", "", errors.New("vde: no tegra-vde")
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

func ioctlBuf(fd uintptr, req uintptr, b []byte) error { return ioctl(fd, req, unsafe.Pointer(&b[0])) }

type le []byte

func (a le) u32(at int) uint32      { return binary.LittleEndian.Uint32(a[at:]) }
func (a le) put16(at int, v uint16) { binary.LittleEndian.PutUint16(a[at:], v) }
func (a le) put32(at int, v uint32) { binary.LittleEndian.PutUint32(a[at:], v) }
func (a le) put64(at int, v uint64) { binary.LittleEndian.PutUint64(a[at:], v) }

// heap is n zero bytes on the heap, never the stack: a struct whose address goes into another one
// as a number must not move, and only a goroutine's stack moves.
//
//go:noinline
func heap(n int) le { return make(le, n) }

// openDevice opens the decoder for coded pictures of w×h and nOut bitstream buffers of at least
// outBytes.
func openDevice(videoPath, mediaPath string, w, h, nOut int) (*device, error) {
	v, err := os.OpenFile(videoPath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	m, err := os.OpenFile(mediaPath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		v.Close()
		return nil, err
	}
	d := &device{video: v, media: m}
	if err := d.setup(w, h, nOut); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *device) fd() uintptr { return d.video.Fd() }

func (d *device) setup(w, h, nOut int) error {
	f := heap(formatSize)
	f.put32(0, bufTypeOutput)
	f.put32(4, uint32(w))
	f.put32(8, uint32(h))
	f.put32(12, fourccH264Slice)
	f[4+180] = 1 // num_planes
	if err := ioctlBuf(d.fd(), vidiocSFmt, f); err != nil {
		return fmt.Errorf("vde: S_FMT bitstream: %w", err)
	}
	f = heap(formatSize)
	f.put32(0, bufTypeCapture)
	f.put32(4, uint32(w))
	f.put32(8, uint32(h))
	f.put32(12, fourccYUV420M)
	f[4+180] = 3
	if err := ioctlBuf(d.fd(), vidiocSFmt, f); err != nil {
		return fmt.Errorf("vde: S_FMT pictures: %w", err)
	}
	if f.u32(12) != fourccYUV420M || f[4+180] != 3 {
		return fmt.Errorf("vde: the decoder writes %08x in %d planes, not YM12", f.u32(12), f[4+180])
	}
	d.planes = 3
	d.fmt.Width, d.fmt.Height = int(f.u32(4)), int(f.u32(8))
	for i := range 3 {
		at := 4 + 20 + 20*i
		d.fmt.Size[i] = int(f.u32(at))
		d.fmt.Pitch[i] = int(f.u32(at + 4))
	}

	// The bitstream buffers, mapped (write-combined in the kernel).
	rb := heap(20)
	rb.put32(0, uint32(nOut))
	rb.put32(4, bufTypeOutput)
	rb.put32(8, memMMAP)
	if err := ioctlBuf(d.fd(), vidiocReqBufs, rb); err != nil {
		return fmt.Errorf("vde: REQBUFS bitstream: %w", err)
	}
	n := int(rb.u32(0))
	if n == 0 {
		return errors.New("vde: no bitstream buffers")
	}
	for i := range n {
		b, planes := newBuf(i, bufTypeOutput, memMMAP, 1)
		if err := ioctlBuf(d.fd(), vidiocQueryBuf, b); err != nil {
			return fmt.Errorf("vde: QUERYBUF: %w", err)
		}
		length, off := int(planes.u32(4)), int64(planes.u32(8))
		mem, err := unix.Mmap(int(d.fd()), off, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("vde: mmap: %w", err)
		}
		d.out = append(d.out, mem)
		var req int32
		if err := ioctl(d.media.Fd(), mediaIocRequestAlloc, unsafe.Pointer(&req)); err != nil {
			return fmt.Errorf("vde: REQUEST_ALLOC: %w", err)
		}
		d.reqs = append(d.reqs, int(req))
	}
	d.outLen = make([]int, n)
	d.outBusy = make([]bool, n)
	return nil
}

// newBuf is a struct v4l2_buffer for buffer index of a multi-planar queue, with its planes array.
func newBuf(index, typ, mem int, planes int) (le, le) {
	b := heap(bufSize)
	p := heap(planeSize * planes)
	b.put32(0, uint32(index))
	b.put32(4, uint32(typ))
	b.put32(60, uint32(mem))
	b.put32(64, uint32(uintptr(unsafe.Pointer(&p[0]))))
	b.put32(68, uint32(planes))
	return b, p
}

// useBuffers makes the decoder write into these, three dmabufs each (Y, U, V).
func (d *device) useBuffers(fds [][3]int) error {
	rb := heap(20)
	rb.put32(0, uint32(len(fds)))
	rb.put32(4, bufTypeCapture)
	rb.put32(8, memDMABUF)
	if err := ioctlBuf(d.fd(), vidiocReqBufs, rb); err != nil {
		return fmt.Errorf("vde: REQBUFS pictures: %w", err)
	}
	if int(rb.u32(0)) < len(fds) {
		return fmt.Errorf("vde: %d picture buffers, want %d", rb.u32(0), len(fds))
	}
	d.capN = len(fds)
	for _, t := range []uint32{bufTypeOutput, bufTypeCapture} {
		v := t
		if err := ioctl(d.fd(), vidiocStreamOn, unsafe.Pointer(&v)); err != nil {
			return fmt.Errorf("vde: STREAMON: %w", err)
		}
	}
	return nil
}

// queuePicture hands capture buffer i to the decoder to write the next picture into.
func (d *device) queuePicture(i int, fds [3]int) error {
	b, p := newBuf(i, bufTypeCapture, memDMABUF, 3)
	for k := range 3 {
		p.put32(planeSize*k+8, uint32(int32(fds[k])))
	}
	return ioctlBuf(d.fd(), vidiocQBuf, b)
}

// decode decodes one picture, its slices in bitstream (Annex B), with its controls, into the capture
// buffer queued last; ts is the picture's timestamp, which the capture buffer takes. It waits for the
// decoder and says whether the picture came out whole.
func (d *device) decode(bitstream []byte, sps, pps, params []byte, ts uint64, timeout time.Duration) error {
	i := 0
	mem := d.out[i]
	if len(bitstream) > len(mem)-64 {
		return fmt.Errorf("vde: a picture of %d bytes, the buffer takes %d", len(bitstream), len(mem))
	}
	copy(mem, bitstream)
	req := d.reqs[i]

	ctrls := heap(20 * 3)
	for k, c := range []struct {
		id uint32
		b  []byte
	}{{cidSPS, sps}, {cidPPS, pps}, {cidDecodeParams, params}} {
		at := 20 * k
		ctrls.put32(at, c.id)
		ctrls.put32(at+4, uint32(len(c.b)))
		ctrls.put32(at+12, uint32(uintptr(unsafe.Pointer(&c.b[0]))))
	}
	ec := heap(24)
	ec.put32(0, ctrlWhichRequest)
	ec.put32(4, 3)
	ec.put32(12, uint32(int32(req)))
	ec.put32(20, uint32(uintptr(unsafe.Pointer(&ctrls[0]))))
	if err := ioctlBuf(d.fd(), vidiocSExtCtrls, ec); err != nil {
		return fmt.Errorf("vde: S_EXT_CTRLS: %w (control %d)", err, ec.u32(8))
	}
	keep(sps, pps, params, ctrls)

	b, p := newBuf(i, bufTypeOutput, memMMAP, 1)
	p.put32(0, uint32(len(bitstream)))
	b.put32(12, bufFlagRequestFD)
	b.put64(24, ts/1e9)
	b.put64(32, ts%1e9/1e3)
	b.put32(76, uint32(int32(req)))
	if err := ioctlBuf(d.fd(), vidiocQBuf, b); err != nil {
		return fmt.Errorf("vde: QBUF bitstream: %w", err)
	}
	if err := ioctl(uintptr(req), mediaRequestQueue, nil); err != nil {
		return fmt.Errorf("vde: REQUEST_QUEUE: %w", err)
	}
	// The request completes when the decoder is done with the picture.
	fds := []unix.PollFd{{Fd: int32(req), Events: unix.POLLPRI}}
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return errors.New("vde: the decoder did not finish the picture")
		}
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("vde: poll: %w", err)
		}
		if n > 0 {
			break
		}
	}
	ob, _ := newBuf(0, bufTypeOutput, memMMAP, 1)
	if err := ioctlBuf(d.fd(), vidiocDQBuf, ob); err != nil {
		return fmt.Errorf("vde: DQBUF bitstream: %w", err)
	}
	cb, _ := newBuf(0, bufTypeCapture, memDMABUF, 3)
	if err := ioctlBuf(d.fd(), vidiocDQBuf, cb); err != nil {
		return fmt.Errorf("vde: DQBUF picture: %w", err)
	}
	if err := ioctl(uintptr(req), mediaRequestReinit, nil); err != nil {
		return fmt.Errorf("vde: REQUEST_REINIT: %w", err)
	}
	if cb.u32(12)&bufFlagError != 0 {
		return errPicture
	}
	return nil
}

// errPicture is a picture the decoder could not decode whole.
var errPicture = errors.New("vde: the decoder could not decode a picture")

// keep holds its arguments until here: the kernel has read them through the pointers packed above.
//
//go:noinline
func keep(...[]byte) {}

func (d *device) close() {
	if d == nil {
		return
	}
	for _, t := range []uint32{bufTypeOutput, bufTypeCapture} {
		v := t
		_ = ioctl(d.fd(), vidiocStreamOff, unsafe.Pointer(&v))
	}
	for _, m := range d.out {
		_ = unix.Munmap(m)
	}
	d.out = nil
	for _, r := range d.reqs {
		_ = unix.Close(r)
	}
	d.reqs = nil
	if d.video != nil {
		// Freeing the queues lets go of the dmabufs the pictures were in.
		for _, q := range []struct{ t, m uint32 }{{bufTypeCapture, memDMABUF}, {bufTypeOutput, memMMAP}} {
			rb := heap(20)
			rb.put32(4, q.t)
			rb.put32(8, q.m)
			_ = ioctlBuf(d.fd(), vidiocReqBufs, rb)
		}
		d.video.Close()
	}
	if d.media != nil {
		d.media.Close()
	}
}
