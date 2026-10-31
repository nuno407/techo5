//go:build !dot && !spot && !panel

package camera

// The Surface RT's front camera: an OmniVision OV9726 (1280x800 RAW10, no ISP) on MIPI CSI into the
// Tegra 3's video input (tegra-video), one plain single-planar V4L2 capture node whose controls
// are the sensor's. surface_rt docs/hw/camera.md has the bring-up.
//
// The VI stores each 10-bit pixel in a 16-bit word, but not least significant bits aligned: the word is
// (v>>3)<<8 | (v&7)<<4 | v>>6, so v = ((w>>8)&0x7f)<<3 | ((w>>4)&7). Each frame is repacked here into the
// MediaTek layout every other board's frame comes in (four pixels in five bytes, least significant bits
// first), and in the Bayer order that layout's code reads, RGGB or BGGR: from there on metering,
// levels, render, Full and the luma grid are shared. The sensor is mounted upside down: its own
// horizontal flip and the VI's vertical flip turn it upright (the sensor's vertical flip makes the VI
// reject frames), and the Bayer order is what the node reports with both on.
//
// It runs at 5 frames a second (vertical_blanking stretches the frame; the exposure can then be up to
// a frame long, 200 ms, before any gain): enough for presence and a person, at a twentieth of a core.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// srtSensor is the OV9726 as the VI gives it at 720p, 5 frames a second: 42 MHz pixel clock, 1664
// clocks a line, 720+4328 lines a frame. Black level 16 a pixel.
var srtSensor = sensorSpec{w: 1280, h: 720, outW: 640, outH: 360, minShut: 1, frameLines: srtFrameLines - 4,
	maxShut: srtFrameLines - 4, blackCap: 48, pedestal: 16, srt: true}

const (
	srtVBlank     = 4328
	srtFrameLines = 720 + srtVBlank

	v4l2BufCapture = 1 // V4L2_BUF_TYPE_VIDEO_CAPTURE

	cidHFlip  = 0x00980914
	cidVFlip  = 0x00980915
	cidVBlank = 0x009e0901

	pixSRGGB10 = 0x30314752 // 'RG10'
	pixSBGGR10 = 0x30314742 // 'BG10'
	pixSGRBG10 = 0x30314142 // 'BA10'
	pixSGBRG10 = 0x30314247 // 'GB10'
)

// srtCam is one run of the capture node.
type srtCam struct {
	fd    int
	maps  [][]byte
	lay   bufLayout
	buf   *[88]byte
	shift bool   // a column off the order the shared code reads: each line starts one pixel in
	out   []byte // the repacked frame
}

// srtNode is the VI's capture node.
func srtNode() string {
	n, _ := v4l2Node(func(s string) bool { return strings.Contains(s, ".vi-output") })
	return n
}

// srtEnabledPath turns the Surface RT's camera on. Off by default: the tablet's camera LED lights
// whenever the sensor runs, and Home Assistant's camera entity alone runs it every few seconds, so the
// camera, and every feature that would open it (the entity, presence, person), is not there at all
// until this file exists:
//
//	touch /data/techo5-linux/camera-enabled    # then restart the daemon
var srtEnabledPath = "/data/techo5-linux/camera-enabled"

func srtAvailable() bool {
	if !layout.SurfaceRT() {
		return false
	}
	if _, err := os.Stat(srtEnabledPath); err != nil {
		return false
	}
	return srtNode() != ""
}

func openSRT() (*srtCam, error) {
	if !srtAvailable() {
		return nil, errors.New("the camera is off (" + srtEnabledPath + ")")
	}
	path := srtNode()
	if path == "" {
		return nil, errors.New("no camera (tegra-video)")
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	v := &srtCam{fd: fd, lay: v4l2Layout(), buf: heapNew[[88]byte]()}
	for _, c := range []struct {
		id  uint32
		val int32
	}{{cidHFlip, 1}, {cidVFlip, 1}, {cidVBlank, srtVBlank}} {
		if err := setCtrl(fd, c.id, c.val); err != nil {
			v.close()
			return nil, fmt.Errorf("camera control %#x: %w", c.id, err)
		}
	}

	f := make([]byte, wordSize+200) // v4l2_format
	u := wordSize
	put32(f, 0, v4l2BufCapture)
	if err := ioctl(fd, vioc(3, 4, wordSize+200), unsafe.Pointer(&f[0])); err != nil { // G_FMT
		v.close()
		return nil, fmt.Errorf("camera format: %w", err)
	}
	w, h, pix, bpl := le32(f, u), le32(f, u+4), le32(f, u+8), le32(f, u+16)
	if int(w) != sensorW || int(h) != sensorH || int(bpl) != sensorW*2 {
		v.close()
		return nil, fmt.Errorf("camera format %dx%d, %d bytes a line, want %dx%d 16-bit", w, h, bpl, sensorW, sensorH)
	}
	var blue bool
	switch pix {
	case pixSRGGB10:
	case pixSBGGR10:
		blue = true
	case pixSGRBG10:
		v.shift = true
	case pixSGBRG10:
		v.shift, blue = true, true
	default:
		v.close()
		return nil, fmt.Errorf("camera pixel format %#x", pix)
	}
	sensor.blueFirst = blue // read by the shared code only while a frame is worked on, after this
	v.out = make([]byte, frameBytes)

	var req [20]byte
	put32(req[:], 0, v4l2Bufs)
	put32(req[:], 4, v4l2BufCapture)
	put32(req[:], 8, v4l2MemoryMmap)
	if err := ioctl(fd, vioc(3, 8, 20), unsafe.Pointer(&req[0])); err != nil {
		v.close()
		return nil, fmt.Errorf("camera buffers: %w", err)
	}
	v.maps = make([][]byte, le32(req[:], 0))
	for i := range v.maps {
		v.setBuf(uint32(i))
		if err := ioctl(fd, vioc(3, 9, v.lay.size), unsafe.Pointer(&v.buf[0])); err != nil {
			v.close()
			return nil, fmt.Errorf("camera buffer %d: %w", i, err)
		}
		offset, length := le32(v.buf[:], v.lay.mOff), le32(v.buf[:], v.lay.length)
		m, err := unix.Mmap(fd, int64(offset), int(length), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			v.close()
			return nil, fmt.Errorf("camera buffer %d map: %w", i, err)
		}
		v.maps[i] = m
		if err := ioctl(fd, vioc(3, 15, v.lay.size), unsafe.Pointer(&v.buf[0])); err != nil {
			v.close()
			return nil, fmt.Errorf("camera buffer %d queue: %w", i, err)
		}
	}
	slog.Info("camera: OV9726 on tegra-video", "node", path, "bayer", fourcc(pix), "fps", 42e6/(1664*srtFrameLines))
	return v, nil
}

func fourcc(v uint32) string {
	return string([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

func (v *srtCam) setBuf(i uint32) {
	*v.buf = [88]byte{}
	put32(v.buf[:], 0, i)
	put32(v.buf[:], 4, v4l2BufCapture)
	put32(v.buf[:], v.lay.seq+4, v4l2MemoryMmap)
}

// exposure sets the shutter in lines and the gain in the shared 1/64 steps: the OV9726's analogue
// gain is in 1/16 (16 = 1x, 248 = 15.5x).
func (v *srtCam) exposure(shutter, gain int) {
	if err := setCtrl(v.fd, cidExposure, int32(shutter)); err != nil {
		slog.Warn("camera: exposure", "lines", shutter, "err", err)
	}
	if err := setCtrl(v.fd, cidAnalogGain, int32(min(max(gain/4, 16), 248))); err != nil {
		slog.Warn("camera: gain", "gain", gain, "err", err)
	}
}

// stream runs until stop closes, calling frame with each frame repacked.
func (v *srtCam) stream(stop chan struct{}, frame func(bayer []byte)) {
	typ := int32(v4l2BufCapture)
	if err := ioctl(v.fd, vioc(1, 18, 4), unsafe.Pointer(&typ)); err != nil {
		slog.Error("camera: stream on", "err", err)
		<-stop
		return
	}
	defer ioctl(v.fd, vioc(1, 19, 4), unsafe.Pointer(&typ))
	quiet := time.Now()
	for {
		select {
		case <-stop:
			return
		default:
		}
		pfd := []unix.PollFd{{Fd: int32(v.fd), Events: unix.POLLIN}}
		if n, err := unix.Poll(pfd, 500); err != nil || n == 0 {
			if time.Since(quiet) > 5*time.Second {
				slog.Warn("camera: no frames for 5s")
				quiet = time.Now()
			}
			continue
		}
		v.setBuf(0)
		if err := ioctl(v.fd, vioc(3, 17, v.lay.size), unsafe.Pointer(&v.buf[0])); err != nil {
			if !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EAGAIN) {
				slog.Warn("camera: dequeue", "err", err)
				time.Sleep(200 * time.Millisecond)
			}
			continue
		}
		quiet = time.Now()
		i, used := le32(v.buf[:], 0), le32(v.buf[:], 8)
		if int(i) < len(v.maps) && int(used) >= sensorW*sensorH*2 {
			repackTegra(v.out, v.maps[i], sensorW, sensorH, v.shift)
			frame(v.out)
		}
		if err := ioctl(v.fd, vioc(3, 15, v.lay.size), unsafe.Pointer(&v.buf[0])); err != nil {
			slog.Warn("camera: requeue", "err", err)
		}
	}
}

// repackTegra turns the VI's 16-bit words into packed 10-bit Bayer, four pixels in five bytes, least
// significant bits first. shift starts each line one pixel in (the last pixel repeats the one two
// before it, the same color), which turns a G-first order into an R- or B-first one.
func repackTegra(dst, src []byte, w, h int, shift bool) {
	bpl := w * 10 / 8
	words := unsafe.Slice((*uint16)(unsafe.Pointer(&src[0])), len(src)/2)
	for y := range h {
		line := words[y*w : (y+1)*w : (y+1)*w]
		out := dst[y*bpl : (y+1)*bpl : (y+1)*bpl]
		px := func(x int) uint32 {
			if shift {
				x++
				if x >= w {
					x -= 2
				}
			}
			s := uint32(line[x])
			return (s>>8&0x7f)<<3 | (s >> 4 & 7)
		}
		for x, o := 0, 0; x+3 < w; x, o = x+4, o+5 {
			p0, p1, p2, p3 := px(x), px(x+1), px(x+2), px(x+3)
			out[o] = byte(p0)
			out[o+1] = byte(p0>>8 | p1<<2)
			out[o+2] = byte(p1>>6 | p2<<4)
			out[o+3] = byte(p2>>4 | p3<<6)
			out[o+4] = byte(p3 >> 2)
		}
	}
}

func (v *srtCam) close() {
	for _, m := range v.maps {
		if m != nil {
			unix.Munmap(m)
		}
	}
	v.maps = nil
	if v.fd >= 0 {
		var req [20]byte
		put32(req[:], 4, v4l2BufCapture)
		put32(req[:], 8, v4l2MemoryMmap)
		ioctl(v.fd, vioc(3, 8, 20), unsafe.Pointer(&req[0]))
		syscall.Close(v.fd)
		v.fd = -1
	}
}
