//go:build !dot

package video

import (
	"context"
	"errors"
	"image"
	"io"
	"log/slog"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/vde"
)

// The Surface RT's VDE driven from the daemon itself (hardware/vde) rather than through GStreamer:
// ffmpeg's Annex B goes to the decoder, which writes each picture straight into one of the screen's
// buffers, and the screen puts that buffer on its overlay plane as it is. No picture crosses a pipe or
// is copied by a core: at 720p that was a third of a core in the daemon and two thirds in GStreamer.
// GStreamer stays for a screen without such buffers, or a decoder that does not open.

// releaser takes a picture's buffer back.
type releaser interface{ Release(buf int) }

// vdeRun is what writes a session's pictures into the screen's set of buffers: the decoder, or the
// cores' pictures read into them (planes.go); close stops it.
type vdeRun struct {
	close func()
	bufs  PictureBufs
	set   int
}

// playVDE plays the stream on the VDE in the daemon, head being what checkHW read of dec's picture and
// sps its first SPS. started is false when the decoder could not start (err says why), and nothing of
// the stream has been read past head.
func (s *session) playVDE(dec *decoder, bufs PictureBufs, head, sps []byte, info Info) (started bool, readErr, err error) {
	run := &vdeRun{bufs: bufs}
	fit := FitIn(info, s.screen)
	dst := image.Rect(fit.X, fit.Y, fit.X+fit.W, fit.Y+fit.H)
	mk := func(n int, f vde.Format) ([][3]int, error) {
		set, fds, err := bufs.NewPictureBufs(n, f.Width, f.Height, f.Pitch, f.Size)
		if err != nil {
			return nil, err
		}
		run.set = set
		return fds, nil
	}
	var crop image.Rectangle
	out := func(buf int) {
		s.arrived(&Frame{Picture: true, Set: run.set, Buf: buf, Crop: crop, W: crop.Dx(), H: crop.Dy(), Dst: dst, owner: s})
	}
	d, err := vde.Open(sps, mk, out, s.starved)
	if err != nil {
		if run.set != 0 {
			bufs.ReleasePictureBufs(run.set)
		}
		return false, nil, err
	}
	run.close, crop = d.Close, d.Crop()
	f := d.Format()
	s.mu.Lock()
	s.dec, s.vde, s.vdeRun = dec, d, run
	s.hw = &hwPicture{w: crop.Dx(), h: crop.Dy(), dst: dst}
	s.rate = ExactRate(info.FPS)
	s.mu.Unlock()
	slog.Info("video: decoding in hardware", "id", s.id, "decoder", "tegra-vde, in the daemon", "picture", itoa(f.Width)+"x"+itoa(f.Height),
		"shown", crop, "buffers", d.Buffers(), "rate", s.rate.String())
	s.startSound(dec)
	stop := context.AfterFunc(s.ctx, d.Stop)
	defer stop()

	readErr = s.feedVDE(d, dec, head)
	s.mu.Lock()
	s.videoIn = true
	s.mu.Unlock()
	return true, readErr, nil
}

// feedVDE hands the decoder the stream, head first, until it ends.
func (s *session) feedVDE(d *vde.Decoder, dec *decoder, head []byte) error {
	if _, err := d.Write(head); err != nil {
		return s.vdeErr(err)
	}
	stats := time.NewTicker(statsEvery)
	defer stats.Stop()
	shown, dropped := 0, 0
	buf := make([]byte, 64<<10)
	for {
		n, err := dec.video.Read(buf)
		if n > 0 {
			if _, werr := d.Write(buf[:n]); werr != nil {
				return s.vdeErr(werr)
			}
		}
		if errors.Is(err, io.EOF) {
			if eerr := d.End(); eerr != nil {
				return s.vdeErr(eerr)
			}
			if d.Damaged > 0 {
				slog.Warn("video: pictures the decoder could not decode whole", "id", s.id, "damaged", d.Damaged, "of", d.Pictures)
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		select {
		case <-stats.C:
			s.mu.Lock()
			sh, dr := s.shown-shown, s.dropped-dropped
			shown, dropped = s.shown, s.dropped
			s.mu.Unlock()
			slog.Info("video: playing", "id", s.id, "shown", sh, "dropped", dr, "at", s.clock().Round(100*time.Millisecond),
				"fps", float64(sh)/statsEvery.Seconds(), "damaged", d.Damaged)
		default:
		}
	}
}

// vdeErr is a decoder's failure as the session ends with it; a decoder stopped because the session
// was is no failure.
func (s *session) vdeErr(err error) error {
	if s.ctx.Err() != nil {
		return nil
	}
	return err
}

// closeVDE lets the decoder (or the cores' reading) and the screen's buffers go, once the session is
// over.
func (s *session) closeVDE() {
	s.mu.Lock()
	run := s.vdeRun
	s.vde, s.vdeRun = nil, nil
	s.mu.Unlock()
	if run == nil {
		return
	}
	run.close()
	run.bufs.ReleasePictureBufs(run.set)
}

// arrived takes a decoded frame into the queue to be shown.
func (s *session) arrived(fr *Frame) {
	s.mu.Lock()
	fr.N = s.read
	s.read++
	s.ready = append(s.ready, fr)
	first := !s.playing
	s.playing = true
	if first {
		s.lastMove = time.Now()
		if !s.sound {
			s.wallFrom = time.Now()
		}
	}
	s.mu.Unlock()
	if first {
		s.changed()
	}
}

// starved is the decoder waiting for a buffer while every one is queued to be shown: as buffer does
// for the cores' frames, with the clock standing still the oldest goes.
func (s *session) starved() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ready) > 0 && !s.pausedLocked() && time.Since(s.lastMove) > stuckFor {
		s.dropped++
		s.lastMove = time.Now()
		fr := s.ready[0]
		s.ready = s.ready[1:]
		s.recycleLocked(fr)
	}
}
