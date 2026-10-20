//go:build !dot

package presence

import (
	"log/slog"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/camera"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/facedetect"
)

// Faces: where the board has the blocks for it (the 2nd gen Show 8's ISP and face detector,
// hardware/facedetect), a frame a second is also looked at for a face. A face keeps the screen awake
// (Present, EmptyFor) and turns the Face sensor on, but it is not movement: the Motion sensor and the
// room's own "somebody near" stay movement's alone, as Motion is defined. The room detector sees only
// change, so somebody reading on the sofa goes still and, after hold, the screen would go dark with
// them in front of it; their face keeps it on. A face is a box most of the detector's candidates agreed
// on (faceVotes), so one stray candidate in a patterned cushion is not somebody.
const (
	faceEvery = time.Second
	faceVotes = 2
)

// faces is one watch's face detection: opened at its first frame, closed with the watch.
type faces struct {
	det    *facedetect.Detector
	off    bool // the blocks would not open or failed: the watch goes on with movement alone
	last   time.Time
	seeing bool
}

// facesAvailable is whether the board has the blocks to look for faces.
func facesAvailable() bool { return facedetect.Available() }

// FacesAvailable is whether the board has a face detector, for the screen's settings.
func FacesAvailable() bool { return facesAvailable() }

// look runs the frame through the detector when it is time: seen if a face is in it, looked if it
// was looked at (it is once a second, while the Face detection switch is on and the detector works).
func (fs *faces) look(fr *camera.Frame) (seen, looked bool) {
	if fs.off || !config.Get().Presence.Faces || fr.At.Sub(fs.last) < faceEvery || !facedetect.Available() {
		return false, false
	}
	fs.last = fr.At
	raw, w, h, bpl, t, ok := fr.ISPRaw()
	if !ok {
		return false, false
	}
	if fs.det == nil {
		d, err := facedetect.New(w, h)
		if err != nil {
			slog.Warn("presence: no face detection; movement alone", "err", err)
			fs.off = true
			return false, false
		}
		fs.det = d
	}
	found, err := fs.det.Detect(raw, bpl, facedetect.Tone(t))
	if err != nil {
		slog.Warn("presence: face detection failed; movement alone", "err", err)
		fs.close()
		fs.off = true
		return false, false
	}
	seen = len(found) > 0 && found[0].Votes >= faceVotes
	if seen != fs.seeing {
		fs.seeing = seen
		if seen {
			slog.Info("presence: a face", "box", found[0].Box, "votes", found[0].Votes)
		}
	}
	return seen, true
}

func (fs *faces) close() {
	if fs.det != nil {
		fs.det.Close()
		fs.det = nil
	}
}

// face notes a face in a frame taken at: it keeps the screen awake for hold, and leaves the room's
// movement, and with it the Motion sensor, alone.
func (f *Feature) face(at time.Time) {
	f.mu.Lock()
	was := f.faceNear(at)
	f.faceAt = at
	f.mu.Unlock()
	if !was {
		slog.Info("presence: a face near")
		f.Changed.Emit(struct{}{})
	}
}

// faceNear is whether a face was seen within hold of now, with f.mu held.
func (f *Feature) faceNear(now time.Time) bool {
	return !f.faceAt.IsZero() && now.Sub(f.faceAt) <= hold
}
