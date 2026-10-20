//go:build !dot

package presence

import (
	"testing"
	"time"
)

// A face seen by somebody sitting still keeps the screen awake, but it is not movement: the Motion
// sensor stays off, and the room's own "somebody near" stays movement's.
func TestAStillFaceLeavesMotionOff(t *testing.T) {
	f := Get()
	f.mu.Lock()
	savedWatching, savedPresent, savedFace, savedSeen, savedFrom := f.watching, f.present, f.faceAt, f.lastSeen, f.watchFrom
	now := time.Now()
	f.watching, f.present, f.faceAt, f.lastSeen, f.watchFrom = true, false, time.Time{}, time.Time{}, now.Add(-time.Hour)
	f.mu.Unlock()
	t.Cleanup(func() {
		f.mu.Lock()
		f.watching, f.present, f.faceAt, f.lastSeen, f.watchFrom = savedWatching, savedPresent, savedFace, savedSeen, savedFrom
		f.mu.Unlock()
		f.sensor.Set(false)
	})
	f.sensor.Set(false)

	f.face(now)
	if f.sensor.Get() {
		t.Error("a still face turned the Motion sensor on")
	}
	f.mu.Lock()
	present := f.present
	f.mu.Unlock()
	if present {
		t.Error("a still face counted as movement")
	}
	if !Present() {
		t.Error("a face does not keep the screen's somebody near")
	}
	if d := EmptyFor(now); d != 0 {
		t.Errorf("EmptyFor with a face in view = %v, want 0", d)
	}
	if d := EmptyFor(now.Add(hold + time.Minute)); d != hold+time.Minute {
		t.Errorf("EmptyFor a minute after the face's hold = %v, want %v", d, hold+time.Minute)
	}
	f.expire(now.Add(hold + time.Minute))
	if f.sensor.Get() {
		t.Error("the Motion sensor came on")
	}
}
