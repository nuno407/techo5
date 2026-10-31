//go:build !dot

package video

import (
	"image"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// The cores' pictures on a screen with a 4:2:0 plane (PlaneDecodeArgs), read from ffmpeg's pipe
// straight into the screen's own buffers (PictureBufs), which the overlay plane then shows as they
// are: the read is the one copy a picture gets, from the pipe into the buffer, where reading into
// memory of the session's and copying from there into the plane's buffer was two.

// planePool is the screen's buffers the pictures are read into: each free, or a picture queued or on
// the panel.
type planePool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	mem     [][3][]byte
	busy    []bool
	stopped bool
}

// Release takes a picture's buffer back.
func (p *planePool) Release(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= 0 && i < len(p.busy) {
		p.busy[i] = false
	}
	p.cond.Broadcast()
}

// stop wakes a read waiting for a buffer, which then gives up.
func (p *planePool) stop() {
	p.mu.Lock()
	p.stopped = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// get is a free buffer, waiting for one; starved is asked now and then meanwhile (it may drop a
// picture). False once stopped.
func (p *planePool) get(starved func()) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.stopped {
			return 0, false
		}
		for i, b := range p.busy {
			if !b {
				p.busy[i] = true
				return i, true
			}
		}
		t := time.AfterFunc(50*time.Millisecond, func() {
			p.mu.Lock()
			p.cond.Broadcast()
			p.mu.Unlock()
		})
		p.cond.Wait()
		t.Stop()
		if !p.stopped {
			p.mu.Unlock()
			starved()
			p.mu.Lock()
		}
	}
}

// planeBuffers is how many pictures may be read ahead into the screen's buffers.
func planeBuffers(size int) int { return min(max(queueBytes/max(size, 1), 4), 24) }

// openPlanes makes the screen's buffers for the cores' w×h pictures. nil if the screen cannot, and the
// pictures go the way of any other then.
func (s *session) openPlanes(bufs PictureBufs, p Planar, dst image.Rectangle) *planePool {
	pitch := [3]int{p.W, p.W / 2, p.W / 2}
	size := [3]int{p.W * p.H, p.W / 2 * p.H / 2, p.W / 2 * p.H / 2}
	n := planeBuffers(i420Size(p.W, p.H))
	set, _, err := bufs.NewPictureBufs(n, p.W, p.H, pitch, size)
	if err != nil {
		slog.Warn("video: no buffers on the screen for the pictures", "id", s.id, "err", err)
		return nil
	}
	pool := &planePool{busy: make([]bool, n)}
	pool.cond = sync.NewCond(&pool.mu)
	for i := range n {
		m, err := bufs.PictureMem(set, i)
		if err != nil {
			slog.Warn("video: the screen's buffers cannot be written", "id", s.id, "err", err)
			bufs.ReleasePictureBufs(set)
			return nil
		}
		pool.mem = append(pool.mem, m)
	}
	s.mu.Lock()
	s.vde = pool
	s.vdeRun = &vdeRun{bufs: bufs, set: set, close: pool.stop}
	s.planeDst = dst
	s.mu.Unlock()
	return pool
}

// readPlanes reads the pictures off their pipe into the screen's buffers until it ends.
func (s *session) readPlanes(r *os.File, pool *planePool, p Planar) error {
	s.mu.Lock()
	set, dst := s.vdeRun.set, s.planeDst
	s.mu.Unlock()
	crop := image.Rect(0, 0, p.W, p.H)
	sizes := [3]int{p.W * p.H, p.W / 2 * p.H / 2, p.W / 2 * p.H / 2}
	stats := time.NewTicker(statsEvery)
	defer stats.Stop()
	shown, dropped := 0, 0
	for {
		i, ok := pool.get(s.starved)
		if !ok {
			return nil // stopped
		}
		for k, n := range sizes {
			if _, err := io.ReadFull(r, pool.mem[i][k][:n]); err != nil {
				pool.Release(i)
				s.mu.Lock()
				s.videoIn = true
				s.mu.Unlock()
				return err
			}
		}
		s.arrived(&Frame{Picture: true, Set: set, Buf: i, Crop: crop, W: p.W, H: p.H, Dst: dst, owner: s})
		select {
		case <-stats.C:
			s.mu.Lock()
			sh, dr := s.shown-shown, s.dropped-dropped
			shown, dropped = s.shown, s.dropped
			s.mu.Unlock()
			slog.Info("video: playing", "id", s.id, "shown", sh, "dropped", dr, "at", s.clock().Round(100*time.Millisecond),
				"fps", float64(sh)/statsEvery.Seconds())
		default:
		}
	}
}
