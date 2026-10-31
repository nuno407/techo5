// Package swinput is the switches the kernel reports as input devices (EV_SW), where the device has
// any worth following: on the Surface RT the Type Cover's hall sensor ("GPIO Hall Effect Sensor",
// SW_LID: the cover over the screen, which the display puts out) and the headphone jack (the WM8962
// card's "Headphones Jack", SW_HEADPHONE_INSERT: headphones in, the speaker goes quiet).
package swinput

import (
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hook"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

const (
	evSW           = 5 // EV_SW
	swLid          = 0 // SW_LID
	swHeadphoneIns = 2 // SW_HEADPHONE_INSERT
)

// Switch is one switch of one input device.
type Switch struct {
	// Changed fires with the new state whenever the switch moves. Listeners must not block.
	Changed hook.Hook[bool]

	what  string
	match func(name string) bool
	code  uint16

	on    atomic.Bool
	here  atomic.Bool
	start sync.Once
}

var (
	// Lid is the Surface RT's cover: on is closed.
	Lid = &Switch{what: "lid", code: swLid, match: func(n string) bool { return n == "GPIO Hall Effect Sensor" }}
	// Headphones is the Surface RT's headphone jack: on is something plugged in.
	Headphones = &Switch{what: "headphones", code: swHeadphoneIns, match: func(n string) bool { return strings.HasSuffix(n, " Headphones Jack") }}

	// inputDir is where the input devices are looked up; a test points it elsewhere.
	inputDir = "/sys/class/input"
)

// Here is whether the device has the switch; it starts following it on the first call.
func (s *Switch) Here() bool {
	s.Watch()
	return s.here.Load()
}

// On is the switch's state. False where there is none.
func (s *Switch) On() bool {
	s.Watch()
	return s.on.Load()
}

// Watch starts following the switch, once. Only the Surface RT has these.
func (s *Switch) Watch() {
	s.start.Do(func() {
		if !layout.SurfaceRT() {
			return
		}
		path := find(s.match)
		if path == "" {
			slog.Info("swinput: none", "switch", s.what)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			slog.Warn("swinput: open", "switch", s.what, "path", path, "err", err)
			return
		}
		s.here.Store(true)
		s.on.Store(state(f, s.code))
		slog.Info("swinput", "switch", s.what, "path", path, "on", s.on.Load())
		safe.Go("swinput "+s.what, func() { s.follow(f) })
	})
}

// find is the event node of the input device whose name matches.
func find(match func(string) bool) string {
	names, _ := filepath.Glob(filepath.Join(inputDir, "event*", "device", "name"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err == nil && match(strings.TrimSpace(string(b))) {
			return "/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(n)))
		}
	}
	return ""
}

// state reads switch code as it is now (EVIOCGSW).
func state(f *os.File, code uint16) bool {
	var sw [8]byte
	req := uintptr(2)<<30 | uintptr(len(sw))<<16 | uintptr('E')<<8 | 0x1b
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&sw[0]))); e != 0 {
		return false
	}
	return sw[code/8]&(1<<(code%8)) != 0
}

// follow reads the device's events until it goes away.
func (s *Switch) follow(f *os.File) {
	defer f.Close()
	size := eventSize()
	buf := make([]byte, size*16)
	for {
		n, err := f.Read(buf)
		if err != nil {
			slog.Warn("swinput: read", "switch", s.what, "err", err)
			if n == 0 {
				return
			}
			time.Sleep(time.Second)
		}
		for i := 0; i+size <= n; i += size {
			if v, ok := switchEvent(buf[i:i+size], size, s.code); ok {
				s.set(v)
			}
		}
	}
}

// set records the state and says so when it changed.
func (s *Switch) set(v bool) {
	if s.on.Swap(v) != v {
		slog.Info("swinput", "switch", s.what, "on", v)
		s.Changed.Emit(v)
	}
}

// eventSize is struct input_event's size: a timeval of two longs, then type, code and value.
func eventSize() int { return int(2*unsafe.Sizeof(uintptr(0))) + 8 }

// switchEvent is the switch's state, if e is an event of switch code.
func switchEvent(e []byte, size int, code uint16) (bool, bool) {
	t := size - 8
	typ, c := binary.LittleEndian.Uint16(e[t:]), binary.LittleEndian.Uint16(e[t+2:])
	if typ != evSW || c != code {
		return false, false
	}
	return int32(binary.LittleEndian.Uint32(e[t+4:])) != 0, true
}
