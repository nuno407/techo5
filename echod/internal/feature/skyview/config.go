package skyview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// Settings is sky.json (doc.go). Never saved by the device: the owner writes it.
type Settings struct {
	Aircraft  string  `json:"aircraft"`
	FR24Token string  `json:"fr24_token"`
	FR24Every int     `json:"fr24_every"` // seconds between two asks of Flightradar24
	Dump1090  string  `json:"dump1090"`   // a local receiver's aircraft.json
	RadiusKm  float64 `json:"radius_km"`
	WindyKey  string  `json:"windy_key"`
	Map       string  `json:"map"`   // "dark" (CARTO) or "osm"
	Radar     *bool   `json:"radar"` // the rain over the map; on unless false
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Zoom      int     `json:"zoom"`
}

// The aircraft sources (Settings.Aircraft).
const (
	SourceAuto     = "auto"
	SourceFR24     = "fr24"
	SourceADSBLol  = "adsblol"
	SourceDump1090 = "dump1090"
	SourceDemo     = "demo"
	SourceOff      = "off"
)

const (
	defaultRadius = 150.0 // km around the map's middle the aircraft are asked for
	maxRadius     = 460.0 // adsb.lol's limit, 250 nautical miles
	defaultZoom   = 9
	minZoom       = 5
	maxZoom       = 13
	fr24Every     = 60 * time.Second
)

func (s Settings) source() string {
	switch v := strings.ToLower(strings.TrimSpace(s.Aircraft)); v {
	case SourceFR24, SourceADSBLol, SourceDump1090, SourceDemo, SourceOff:
		return v
	}
	if strings.TrimSpace(s.Dump1090) != "" {
		return SourceDump1090
	}
	return SourceADSBLol
}

func (s Settings) radius() float64 {
	if s.RadiusKm <= 0 {
		return defaultRadius
	}
	return min(s.RadiusKm, maxRadius)
}

func (s Settings) zoom() int {
	if s.Zoom == 0 {
		return defaultZoom
	}
	return min(max(s.Zoom, minZoom), maxZoom)
}

func (s Settings) radarOn() bool { return s.Radar == nil || *s.Radar }

func (s Settings) fr24Every() time.Duration {
	if s.FR24Every <= 0 {
		return fr24Every
	}
	return max(time.Duration(s.FR24Every)*time.Second, 10*time.Second)
}

// settingsDir is where sky.json and the tiles are; a test moves it.
var settingsDir = layout.StateDir

// SettingsPath is the file the Sky board's settings are kept in.
func SettingsPath() string { return filepath.Join(settingsDir, "sky.json") }

var settings struct {
	sync.Mutex
	got     Settings
	mod     time.Time
	size    int64
	problem string
}

// current is sky.json as it is now (no file is every default), and what is wrong with it when it could
// not be read, the settings before it being kept meanwhile.
func current() (Settings, string) {
	settings.Lock()
	defer settings.Unlock()
	fi, err := os.Stat(SettingsPath())
	if err != nil {
		settings.got, settings.mod, settings.size, settings.problem = Settings{}, time.Time{}, 0, ""
		return settings.got, ""
	}
	if fi.ModTime().Equal(settings.mod) && fi.Size() == settings.size {
		return settings.got, settings.problem
	}
	settings.mod, settings.size = fi.ModTime(), fi.Size()
	b, err := os.ReadFile(SettingsPath())
	var s Settings
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		settings.problem = "sky.json: " + err.Error()
		return settings.got, settings.problem
	}
	settings.got, settings.problem = s, ""
	return s, ""
}

// UseDir moves where sky.json and the tiles are kept: for the screen's previews, which run off the
// device.
func UseDir(dir string) {
	settings.Lock()
	settingsDir, settings.mod = dir, time.Time{}
	settings.Unlock()
	f := Get()
	f.tiles.mu.Lock()
	f.tiles.dir = filepath.Join(dir, "tiles")
	f.tiles.mu.Unlock()
}
