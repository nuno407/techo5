//go:build !dot && !panel

package screen

import (
	"fmt"
	"log/slog"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The Tegra 3's 2D engine (GR2D) through host1x's channel interface on the card the pages are on
// (surface_rt docs/hw/gpu.md; tools/hw/gpu/gr2d-bench.c is the same command stream in C). It copies
// between the pages and fills them, while the cores do something else: what present gives it is the
// rows a page lacks that the page on show already has, which the cores would otherwise write again
// from the canvas, and blackPages' fill. One job a frame, every rectangle in it: a job costs about half
// a millisecond however small.
//
// What it is not given: rows from the canvas. The canvas is cached memory, and the 2D engine reads
// RAM, not the caches: each frame would have to clean the canvas out of them first, which costs
// more (9 ms for a 1366x768 canvas, measured) than the cores writing the rows that changed.

const (
	tegraChannelOpen  = 0x50
	tegraChannelClose = 0x51
	tegraChannelMap   = 0x52
	tegraChannelUnmap = 0x53
	tegraSubmit       = 0x54
	tegraSyncptAlloc  = 0x60
	tegraSyncptFree   = 0x61
	tegraSyncptWait   = 0x62

	classGR2D           = 0x51
	channelMapRW        = 3
	submitCmdGatherUptr = 0
	syncCondOpDone      = 1

	// gr2dTimeout is how long a job may take: a whole page is 2 ms.
	gr2dTimeout = time.Second
)

func opSetClass(off, class, mask uint32) uint32 { return off<<16 | class<<6 | mask }
func opNonIncr(off, count uint32) uint32        { return 2<<28 | off<<16 | count }
func opMask(off, mask uint32) uint32            { return 3<<28 | off<<16 | mask }

// gr2d is an open channel to the 2D engine and the pages mapped into it.
type gr2d struct {
	fd      uintptr
	context uint32
	syncpt  uint32
	maps    []uint32 // the pages' mappings, by page
	words   []uint32 // the job being built
	relocs  []gr2dReloc
}

type gr2dReloc struct {
	word    int
	mapping uint32
}

// heapArgs is n zero bytes on the heap: the submit's arrays go to the kernel as addresses inside it.
//
//go:noinline
func heapArgs(n int) args { return make(args, n) }

// openGR2D opens the 2D engine on the card k drives and maps its pages; nil and why if it cannot.
func openGR2D(k *kmsOut) (*gr2d, error) {
	g := &gr2d{fd: k.fd()}
	o := make(args, 24)
	o.put32(0, classGR2D)
	if err := drmIoctl(g.fd, tegraChannelOpen, o); err != nil {
		return nil, fmt.Errorf("CHANNEL_OPEN: %w", err)
	}
	g.context = o.u32(8)
	s := make(args, 8)
	if err := drmIoctl(g.fd, tegraSyncptAlloc, s); err != nil {
		g.close()
		return nil, fmt.Errorf("SYNCPOINT_ALLOCATE: %w", err)
	}
	g.syncpt = s.u32(0)
	for _, b := range k.bufs {
		m := make(args, 16)
		m.put32(0, g.context)
		m.put32(4, b.handle)
		m.put32(8, channelMapRW)
		if err := drmIoctl(g.fd, tegraChannelMap, m); err != nil {
			g.close()
			return nil, fmt.Errorf("CHANNEL_MAP: %w", err)
		}
		g.maps = append(g.maps, m.u32(12))
	}
	return g, nil
}

func (g *gr2d) close() {
	for _, m := range g.maps {
		a := make(args, 8)
		a.put32(0, g.context)
		a.put32(4, m)
		_ = drmIoctl(g.fd, tegraChannelUnmap, a)
	}
	g.maps = nil
	if g.syncpt != 0 {
		a := make(args, 8)
		a.put32(0, g.syncpt)
		_ = drmIoctl(g.fd, tegraSyncptFree, a)
		g.syncpt = 0
	}
	if g.context != 0 {
		a := make(args, 8)
		a.put32(0, g.context)
		_ = drmIoctl(g.fd, tegraChannelClose, a)
		g.context = 0
	}
}

// reloc puts a placeholder for mapping's address in the job.
func (g *gr2d) reloc(mapping uint32) {
	g.relocs = append(g.relocs, gr2dReloc{word: len(g.words), mapping: mapping})
	g.words = append(g.words, 0xdeadbeef)
}

// copyRows queues copies of the row ranges (y0, y1 pairs) of page from to page to, w pixels wide, line
// bytes a row. Nothing runs until run.
func (g *gr2d) copyRows(to, from int, runs [][2]int, w, line int) {
	if len(runs) == 0 {
		return
	}
	g.words = append(g.words,
		opSetClass(0, classGR2D, 0),
		opMask(0x9, 0x9), 0x3a, 0, // trigger on dstps; cmdsel
		opMask(0x1e, 0x7), 0, 1<<20|2<<16, 0xcc, // controlsecond; controlmain: same depth, 32 bpp; rop copy
		opNonIncr(0x46, 1), 0, // linear
		opMask(0x2b, 0xe149))
	g.reloc(g.maps[to])
	g.words = append(g.words, uint32(line))
	g.reloc(g.maps[from])
	r := runs[0]
	g.words = append(g.words, uint32(line), uint32(r[1]-r[0])<<16|uint32(w), uint32(r[0])<<16, uint32(r[0])<<16)
	for _, r := range runs[1:] {
		g.words = append(g.words, opMask(0x38, 0x7), uint32(r[1]-r[0])<<16|uint32(w), uint32(r[0])<<16, uint32(r[0])<<16)
	}
}

// fill queues a fill of page's w×h with color (a pixel as it is in memory, little-endian).
func (g *gr2d) fill(page int, color uint32, w, h, line int) {
	g.words = append(g.words,
		opSetClass(0, classGR2D, 0),
		opMask(0x9, 0x9), 0x3a, 0,
		opMask(0x1e, 0x7), 0, 2<<16|1<<6|1<<2, 0xcc, // 32 bpp, solid fill; rop copy
		opMask(0x2b, 0x9))
	g.reloc(g.maps[page])
	g.words = append(g.words, uint32(line),
		opNonIncr(0x35, 1), color,
		opNonIncr(0x46, 1), 0,
		opMask(0x38, 0x5), uint32(h)<<16|uint32(w), 0)
}

// run submits the job queued and waits for it.
func (g *gr2d) run() error {
	if len(g.words) == 0 {
		return nil
	}
	g.words = append(g.words, opNonIncr(0, 1), syncCondOpDone<<8|g.syncpt)
	defer func() { g.words, g.relocs = g.words[:0], g.relocs[:0] }()

	gather := heapArgs(4 * len(g.words))
	for i, w := range g.words {
		gather.put32(4*i, w)
	}
	bufs := heapArgs(24 * len(g.relocs))
	for i, r := range g.relocs {
		bufs.put32(24*i, r.mapping)
		bufs.put32(24*i+16, uint32(r.word))
	}
	cmds := heapArgs(24)
	cmds.put32(0, submitCmdGatherUptr)
	cmds.put32(8, uint32(len(g.words)))
	sub := heapArgs(64)
	sub.put32(0, g.context)
	sub.put32(4, uint32(len(g.relocs)))
	sub.put32(8, 1)
	sub.put32(12, uint32(len(g.words)))
	sub.put64(16, uint64(uintptr(unsafe.Pointer(&bufs[0]))))
	sub.put64(24, uint64(uintptr(unsafe.Pointer(&cmds[0]))))
	sub.put64(32, uint64(uintptr(unsafe.Pointer(&gather[0]))))
	sub.put32(48, g.syncpt)
	sub.put32(56, 1) // increments
	err := drmIoctl(g.fd, tegraSubmit, sub)
	keepArgs(gather, bufs, cmds)
	if err != nil {
		return fmt.Errorf("CHANNEL_SUBMIT: %w", err)
	}
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	w := heapArgs(24)
	w.put64(0, uint64(ts.Nano()+int64(gr2dTimeout)))
	w.put32(8, g.syncpt)
	w.put32(12, sub.u32(60))
	for {
		// Not drmIoctl: a wait that timed out says EAGAIN, which is not to be asked again.
		_, _, e := unix.Syscall(unix.SYS_IOCTL, g.fd, drmIoc(tegraSyncptWait, len(w)), uintptr(unsafe.Pointer(&w[0])))
		switch e {
		case 0:
			return nil
		case unix.EINTR:
			continue
		}
		return fmt.Errorf("SYNCPOINT_WAIT: %w", e)
	}
}

//go:noinline
func keepArgs(...args) {}

// use2D is whether present may use the 2D engine: TECHO5_SCREEN_2D=off leaves it to the cores, for
// comparing the two.
var use2D = os.Getenv("TECHO5_SCREEN_2D") != "off"

// gpu2D is the 2D engine for k's pages, opened the first time it is asked for; nil once it has failed.
func (k *kmsOut) gpu2D() *gr2d {
	if k.g2dFailed || !k.xbgr || !use2D {
		return nil
	}
	if k.g2d == nil {
		g, err := openGR2D(k)
		if err != nil {
			k.g2dFailed = true
			slog.Warn("screen: no 2D engine; the cores write the pages", "err", err)
			return nil
		}
		k.g2d = g
	}
	return k.g2d
}

// g2dFail turns the 2D engine off after a job failed: the cores do its work from then on.
func (k *kmsOut) g2dFail(err error) {
	slog.Warn("screen: the 2D engine failed; the cores write the pages", "err", err)
	if k.g2d != nil {
		k.g2d.close()
		k.g2d = nil
	}
	k.g2dFailed = true
}
