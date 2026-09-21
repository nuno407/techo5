package camera

import (
	"context"
	"log/slog"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
	"google.golang.org/protobuf/proto"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/camera"
)

// The ESPHome camera entity. The library has no camera domain, so this feature describes the
// entity itself (component.Describer puts it ahead of the library's list) and answers the image
// requests: Home Assistant asks for a single picture for the entity's still, and for a stream
// while someone is watching, re-asking every few seconds for as long as they do.

const (
	// objectID is the entity's identifier: camera.<device>_camera in Home Assistant.
	objectID = "camera"

	// chunk is how much JPEG goes in one CameraImageResponse; ESPHome sends 1 KiB pieces.
	chunk = 1024

	// streamFor is how long one stream request keeps frames flowing; Home Assistant renews it
	// well inside that while the stream is open.
	streamFor = 5 * time.Second

	// Leave bandwidth for voice while keeping the live view responsive.
	streamEvery = 200 * time.Millisecond
)

// key is the entity key, the library's FNV-1 of the object id.
func key(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h *= 16777619
		h ^= uint32(s[i])
	}
	return h
}

// DescribesEntities marks the feature as a component.Describer.
func (f *Feature) DescribesEntities() {}

// Handle answers the entity list and the image requests.
func (f *Feature) Handle(ctx context.Context, c *esphome.Conn, msg proto.Message) error {
	switch m := msg.(type) {
	case *api.ListEntitiesRequest:
		if !camera.Available() {
			return nil
		}
		return c.Send(&api.ListEntitiesCameraResponse{
			ObjectId: objectID,
			Key:      key(objectID),
			Name:     "Camera",
			Icon:     "mdi:camera",
		})
	case *api.CameraImageRequest:
		if !camera.Available() {
			return nil
		}
		if m.GetStream() {
			f.streamTo(c)
		} else if m.GetSingle() {
			go f.single(c)
		}
	}
	return nil
}

// send ships one JPEG in chunks, the last marked done.
func send(c *esphome.Conn, jpg []byte) error {
	k := key(objectID)
	for off := 0; off < len(jpg); off += chunk {
		end := off + chunk
		if end > len(jpg) {
			end = len(jpg)
		}
		if err := c.Send(&api.CameraImageResponse{Key: k, Data: jpg[off:end], Done: end == len(jpg)}); err != nil {
			return err
		}
	}
	return nil
}

// single answers one still.
func (f *Feature) single(c *esphome.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotWait)
	defer cancel()
	fr, err := camera.Get().Snapshot(ctx)
	if err != nil {
		slog.Warn("camera entity: still", "err", err)
		return
	}
	jpg, err := encodeFull(fr)
	if err != nil {
		return
	}
	if err := send(c, jpg); err != nil {
		slog.Debug("camera entity: send", "err", err)
	}
}

// streams is the connections being streamed to, each with its deadline.
type streamState struct {
	mu    sync.Mutex
	until map[*esphome.Conn]time.Time
}

var streams = streamState{until: map[*esphome.Conn]time.Time{}}

// streamTo starts, or extends, a stream to c.
func (f *Feature) streamTo(c *esphome.Conn) {
	streams.mu.Lock()
	_, running := streams.until[c]
	streams.until[c] = time.Now().Add(streamFor)
	streams.mu.Unlock()
	if running {
		return
	}
	go f.pump(c)
}

// pump sends frames to c until its deadline passes or the connection is gone.
func (f *Feature) pump(c *esphome.Conn) {
	defer func() {
		streams.mu.Lock()
		delete(streams.until, c)
		streams.mu.Unlock()
	}()
	release, err := camera.Get().Acquire()
	if err != nil {
		slog.Warn("camera entity: stream", "err", err)
		return
	}
	defer release()
	frames := make(chan *camera.Frame, 1)
	cancel := camera.Get().Frames.Listen(func(fr *camera.Frame) {
		// Replace a queued frame so a slow sender always resumes with the latest image.
		select {
		case <-frames:
		default:
		}
		select {
		case frames <- fr:
		default:
		}
	})
	defer cancel()
	last := time.Time{}
	for {
		streams.mu.Lock()
		until := streams.until[c]
		streams.mu.Unlock()
		if time.Now().After(until) {
			return
		}
		select {
		case fr := <-frames:
			if time.Since(last) < streamEvery {
				continue
			}
			jpg, err := encode(fr)
			if err != nil {
				return
			}
			if err := send(c, jpg); err != nil {
				return
			}
			last = time.Now()
		case <-time.After(time.Second):
		}
	}
}
