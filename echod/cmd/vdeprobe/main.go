//go:build linux

// Command vdeprobe is the bench check for the Surface RT's H.264 decoder path (hardware/vde): it
// decodes an Annex B file on the VDE into buffers of tegra-drm's (the render node, as the screen's
// buffers are the card's), and prints the MD5 of each picture as shown, cropped, Y then U then V, the
// way ffmpeg's framemd5 does, so the output compares line for line with
//
//	ffmpeg -i clip.h264 -f framemd5 - | grep -v '^#' | awk -F, '{print $6}'
//
// With -bench it reads nothing back and says how much CPU each picture took instead.
//
//	vdeprobe [-bench] clip.h264
package main

import (
	"crypto/md5"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/vde"
)

const (
	tegraGemCreate = 0xc0106440
	tegraGemMmap   = 0xc0106441
	primeHandleFD  = 0xc00c642d
	drmCloexecRDWR = 0x80000 | 2
)

func ioctl(fd uintptr, req uintptr, b []byte) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&b[0]))); e != 0 {
		return e
	}
	return nil
}

type bo struct {
	fd  int
	mem []byte
}

// newBO is a tegra GEM buffer of size bytes, mapped, and a dmabuf of it.
func newBO(drm *os.File, size int) (bo, error) {
	c := make([]byte, 16)
	binary.LittleEndian.PutUint64(c, uint64(size))
	if err := ioctl(drm.Fd(), tegraGemCreate, c); err != nil {
		return bo{}, fmt.Errorf("GEM_CREATE: %w", err)
	}
	handle := binary.LittleEndian.Uint32(c[12:])
	m := make([]byte, 16)
	binary.LittleEndian.PutUint32(m, handle)
	if err := ioctl(drm.Fd(), tegraGemMmap, m); err != nil {
		return bo{}, fmt.Errorf("GEM_MMAP: %w", err)
	}
	mem, err := unix.Mmap(int(drm.Fd()), int64(binary.LittleEndian.Uint64(m[8:])), size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return bo{}, err
	}
	p := make([]byte, 12)
	binary.LittleEndian.PutUint32(p, handle)
	binary.LittleEndian.PutUint32(p[4:], drmCloexecRDWR)
	if err := ioctl(drm.Fd(), primeHandleFD, p); err != nil {
		return bo{}, fmt.Errorf("PRIME_HANDLE_TO_FD: %w", err)
	}
	return bo{fd: int(int32(binary.LittleEndian.Uint32(p[8:]))), mem: mem}, nil
}

func main() {
	bench := flag.Bool("bench", false, "time the decode, read nothing back")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: vdeprobe [-bench] clip.h264")
		os.Exit(2)
	}
	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fail(err)
	}
	drm, err := os.OpenFile("/dev/dri/renderD128", os.O_RDWR, 0)
	if err != nil {
		fail(err)
	}
	var sps []byte
	for i := 0; i+4 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3]&0x1f == 7 {
			sps = data[i+3 : min(i+3+64, len(data))]
			break
		}
	}
	var bos [][3]bo
	var f vde.Format
	mk := func(n int, ff vde.Format) ([][3]int, error) {
		f = ff
		var fds [][3]int
		for range n {
			var b [3]bo
			var fd [3]int
			for k := range 3 {
				var err error
				if b[k], err = newBO(drm, (ff.Size[k]+4095)&^4095); err != nil {
					return nil, err
				}
				fd[k] = b[k].fd
			}
			bos = append(bos, b)
			fds = append(fds, fd)
		}
		return fds, nil
	}
	var d *vde.Decoder
	shown := 0
	out := func(buf int) {
		shown++
		if !*bench {
			fmt.Printf("%x\n", picMD5(bos[buf], f, d))
		}
		d.Release(buf)
	}
	d, err = vde.Open(sps, mk, out, nil)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "vdeprobe: %dx%d, crop %v, pitch %v, %d buffers\n", f.Width, f.Height, d.Crop(), f.Pitch, d.Buffers())
	var r0, r1 unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &r0)
	t0 := time.Now()
	for i := 0; i < len(data); i += 64 << 10 {
		if _, err := d.Write(data[i:min(i+64<<10, len(data))]); err != nil {
			fail(err)
		}
	}
	if err := d.End(); err != nil {
		fail(err)
	}
	wall := time.Since(t0)
	_ = unix.Getrusage(unix.RUSAGE_SELF, &r1)
	cpu := time.Duration(r1.Utime.Nano()+r1.Stime.Nano()-r0.Utime.Nano()-r0.Stime.Nano()) * time.Nanosecond
	fmt.Fprintf(os.Stderr, "vdeprobe: %d pictures decoded, %d shown, %d damaged, %v (%.2f ms each), CPU %v (%.2f ms each)\n",
		d.Pictures, shown, d.Damaged, wall.Round(time.Millisecond), float64(wall.Microseconds())/1000/float64(max(d.Pictures, 1)),
		cpu.Round(time.Millisecond), float64(cpu.Microseconds())/1000/float64(max(d.Pictures, 1)))
	d.Close()
}

// picMD5 is the MD5 of a picture's visible part, Y, U, V.
func picMD5(b [3]bo, f vde.Format, d *vde.Decoder) [16]byte {
	c := d.Crop()
	h := md5.New()
	for k := range 3 {
		x0, y0, x1, y1 := c.Min.X, c.Min.Y, c.Max.X, c.Max.Y
		if k > 0 {
			x0, y0, x1, y1 = x0/2, y0/2, (x1+1)/2, (y1+1)/2
		}
		// Read back a row at a time into RAM: the buffer is write-combined, slow to read.
		row := make([]byte, x1-x0)
		for y := y0; y < y1; y++ {
			copy(row, b[k].mem[y*f.Pitch[k]+x0:y*f.Pitch[k]+x1])
			h.Write(row)
		}
	}
	var s [16]byte
	copy(s[:], h.Sum(nil))
	return s
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "vdeprobe:", err)
	os.Exit(1)
}
