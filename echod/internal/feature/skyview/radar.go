package skyview

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The rain: RainViewer's newest past frame. Its list of frames is asked every radarEvery while the
// board is up, and a new frame's tiles are fetched as the old one's are let go: for the moment between,
// the rain is drawn where its tiles are in.

// radarIndex is where RainViewer lists its frames; a test points it at its own server.
var radarIndex = "https://api.rainviewer.com/public/weather-maps.json"

const (
	radarEvery = 5 * time.Minute
	// radarTop is the closest zoom RainViewer's free tiles come at; closer, its tiles are enlarged.
	radarTop = 7
	// radarAlpha is how much of the rain shows over the map, out of 255: enough to read, not so much
	// the map under heavy rain is lost.
	radarAlpha = 190
)

// radarFrame is the frame shown: its tiles' layer and when it was measured.
type radarFrame struct {
	layer string // "rv:" and the frame's address
	at    time.Time
}

// fetchRadar is RainViewer's newest past frame.
func fetchRadar(ctx context.Context) (radarFrame, error) {
	b, err := fetch(ctx, radarIndex, 1<<20)
	if err != nil {
		return radarFrame{}, err
	}
	var index struct {
		Host  string `json:"host"`
		Radar struct {
			Past []struct {
				Time int64  `json:"time"`
				Path string `json:"path"`
			} `json:"past"`
		} `json:"radar"`
	}
	if err := json.Unmarshal(b, &index); err != nil {
		return radarFrame{}, err
	}
	past := index.Radar.Past
	if len(past) == 0 || index.Host == "" {
		return radarFrame{}, errors.New("RainViewer listed no frames")
	}
	p := past[len(past)-1]
	return radarFrame{layer: "rv:" + index.Host + p.Path, at: time.Unix(p.Time, 0)}, nil
}
