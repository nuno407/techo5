package skyview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Windy's point forecast for home (api.windy.com/api/point-forecast/v2): the wind, its gusts and the
// rain to come, from GFS, at the hour nearest now. Asked every windyEvery while the board is up, and
// only with a key (Settings.WindyKey). Windy's map layers, its radar among them, are not offered
// outside its own JavaScript map, so this is what Windy adds to a board drawn on the device.

// windyPoint is Windy's Point Forecast API; a test points it at its own server.
var windyPoint = "https://api.windy.com/api/point-forecast/v2"

const windyEvery = 30 * time.Minute

// Wind is the forecast for home at the hour nearest now.
type Wind struct {
	At       time.Time
	SpeedKmh float64
	GustKmh  float64
	From     float64 // degrees: where the wind blows from
	RainMm   float64 // in the three hours to At
	Model    string
}

func fetchWindy(ctx context.Context, key string, lat, lon float64, now time.Time) (Wind, error) {
	body, _ := json.Marshal(map[string]any{
		"lat": math.Round(lat*100) / 100, "lon": math.Round(lon*100) / 100, "model": "gfs",
		"parameters": []string{"wind", "windGust", "precip"}, "levels": []string{"surface"}, "key": strings.TrimSpace(key),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", windyPoint, bytes.NewReader(body))
	if err != nil {
		return Wind{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	b, err := send(req, 2<<20)
	var se statusErr
	if errors.As(err, &se) && (se.code == 400 || se.code == 401 || se.code == 403) {
		return Wind{}, fmt.Errorf("Windy refused the key (%s)", se)
	}
	if err != nil {
		return Wind{}, err
	}
	return parseWindy(b, now)
}

func parseWindy(b []byte, now time.Time) (Wind, error) {
	var res map[string]json.RawMessage
	if err := json.Unmarshal(b, &res); err != nil {
		return Wind{}, err
	}
	series := func(name string) []float64 {
		var v []*float64
		_ = json.Unmarshal(res[name], &v)
		out := make([]float64, len(v))
		for i, p := range v {
			if p != nil {
				out[i] = *p
			} else {
				out[i] = math.NaN()
			}
		}
		return out
	}
	var ts []int64
	if err := json.Unmarshal(res["ts"], &ts); err != nil || len(ts) == 0 {
		return Wind{}, errors.New("Windy sent no forecast")
	}
	u, v, gust, rain := series("wind_u-surface"), series("wind_v-surface"), series("gust-surface"), series("past3hprecip-surface")
	best := 0
	for i, t := range ts {
		if math.Abs(float64(t/1000-now.Unix())) < math.Abs(float64(ts[best]/1000-now.Unix())) {
			best = i
		}
	}
	at := func(s []float64) float64 {
		if best < len(s) && !math.IsNaN(s[best]) {
			return s[best]
		}
		return 0
	}
	uu, vv := at(u), at(v)
	w := Wind{At: time.UnixMilli(ts[best]), SpeedKmh: math.Hypot(uu, vv) * 3.6, GustKmh: at(gust) * 3.6, Model: "GFS"}
	// u and v are where the air goes; the wind is named for where it comes from.
	w.From = math.Mod(math.Atan2(-uu, -vv)*180/math.Pi+360, 360)
	var units map[string]string
	_ = json.Unmarshal(res["units"], &units)
	w.RainMm = at(rain)
	if units["past3hprecip-surface"] == "m" {
		w.RainMm *= 1000 // Windy's answer says metres
	}
	return w, nil
}
