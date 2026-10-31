package skyview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Aircraft is one aircraft as last heard of.
type Aircraft struct {
	ID       string // the ICAO address, or Flightradar24's id
	Callsign string
	Reg      string
	Type     string // ICAO type designator: A320, B738…
	From, To string // airports, where the source knows them (Flightradar24)
	Squawk   string
	Lat, Lon float64
	Track    float64 // degrees from north
	AltFt    int
	Ground   bool
	SpeedKt  float64
	VRate    int       // feet a minute, climbing positive
	At       time.Time // when the position was measured
	// Trail is where it was at the answers before, oldest first: a short line behind it.
	Trail [][2]float64
}

const (
	// glideFor is how far ahead of its last position an aircraft is moved along its track, at most: past
	// that, an aircraft not heard of again is left where it was rather than flown off on a guess.
	glideFor = 45 * time.Second
	// forgetAfter is how long an aircraft no longer in the answers stays on the board.
	forgetAfter = 60 * time.Second
	trailLen    = 16

	adsbLolEvery  = 8 * time.Second
	dump1090Every = 3 * time.Second
	demoEvery     = 2 * time.Second
)

// Position is where the aircraft is likely to be at now: its last position moved along its track at its
// ground speed for the time since, up to glideFor.
func (a Aircraft) Position(now time.Time) (lat, lon float64) {
	dt := now.Sub(a.At)
	if a.Ground || a.SpeedKt <= 0 || dt <= 0 {
		return a.Lat, a.Lon
	}
	dt = min(dt, glideFor)
	m := a.SpeedKt * 1852 / 3600 * dt.Seconds()
	th := a.Track * math.Pi / 180
	lat = a.Lat + m*math.Cos(th)/111320
	lon = a.Lon + m*math.Sin(th)/(111320*math.Cos(a.Lat*math.Pi/180))
	return lat, lon
}

// Where the aircraft come from; a test points them at its own server.
var (
	adsbLolPoint  = "https://api.adsb.lol/v2/point/%.4f/%.4f/%d"
	fr24Positions = "https://fr24api.flightradar24.com/api/live/flight-positions/full?bounds=%.3f,%.3f,%.3f,%.3f"
)

// errRefused is a source that will not answer this token: Flightradar24's 401, 402 or 403.
var errRefused = errors.New("refused")

// fetchADSBLol is the aircraft within radius km of lat, lon from adsb.lol (readsb's JSON).
func fetchADSBLol(ctx context.Context, lat, lon, radius float64) ([]Aircraft, error) {
	nm := int(math.Ceil(radius / 1.852))
	b, err := fetch(ctx, fmt.Sprintf(adsbLolPoint, lat, lon, min(max(nm, 1), 250)), 8<<20)
	if err != nil {
		return nil, err
	}
	return parseReadsb(b, time.Now())
}

// fetchDump1090 is a local receiver's aircraft.json: dump1090-fa, readsb and tar1090 all serve one.
func fetchDump1090(ctx context.Context, url string) ([]Aircraft, error) {
	b, err := fetch(ctx, url, 8<<20)
	if err != nil {
		return nil, err
	}
	return parseReadsb(b, time.Now())
}

// parseReadsb reads readsb's aircraft list, as adsb.lol's API ("ac") and a receiver's aircraft.json
// ("aircraft") send it. Altitude is a number of feet or "ground"; an aircraft without a position, or
// whose position is over a minute old, is left out.
func parseReadsb(b []byte, now time.Time) ([]Aircraft, error) {
	type plane struct {
		Hex      string          `json:"hex"`
		Flight   string          `json:"flight"`
		Reg      string          `json:"r"`
		Type     string          `json:"t"`
		Lat      *float64        `json:"lat"`
		Lon      *float64        `json:"lon"`
		AltBaro  json.RawMessage `json:"alt_baro"`
		Altitude json.RawMessage `json:"altitude"` // dump1090's older name
		GS       float64         `json:"gs"`
		Speed    float64         `json:"speed"`
		Track    float64         `json:"track"`
		BaroRate int             `json:"baro_rate"`
		Squawk   string          `json:"squawk"`
		SeenPos  float64         `json:"seen_pos"`
	}
	var res struct {
		AC       []plane `json:"ac"`
		Aircraft []plane `json:"aircraft"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	var out []Aircraft
	for _, p := range append(res.AC, res.Aircraft...) {
		if p.Lat == nil || p.Lon == nil || p.SeenPos > 60 {
			continue
		}
		a := Aircraft{
			ID: strings.ToLower(strings.TrimPrefix(p.Hex, "~")), Callsign: strings.TrimSpace(p.Flight), Reg: p.Reg, Type: p.Type,
			Lat: *p.Lat, Lon: *p.Lon, Track: p.Track, SpeedKt: p.GS, VRate: p.BaroRate, Squawk: p.Squawk,
			At: now.Add(-time.Duration(p.SeenPos * float64(time.Second))),
		}
		if a.SpeedKt == 0 {
			a.SpeedKt = p.Speed
		}
		alt := p.AltBaro
		if len(alt) == 0 {
			alt = p.Altitude
		}
		var ft float64
		switch {
		case strings.Contains(string(alt), "ground"):
			a.Ground = true
		case json.Unmarshal(alt, &ft) == nil:
			a.AltFt = int(ft)
		}
		out = append(out, a)
	}
	return out, nil
}

// fetchFR24 is the aircraft inside the box around lat, lon from Flightradar24's official API.
func fetchFR24(ctx context.Context, token string, lat, lon, radius float64) ([]Aircraft, error) {
	dLat := radius / 111.32
	dLon := radius / (111.32 * math.Max(math.Cos(lat*math.Pi/180), 0.1))
	url := fmt.Sprintf(fr24Positions, math.Min(lat+dLat, 85), math.Max(lat-dLat, -85), lon-dLon, lon+dLon)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Version", "v1")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	b, err := send(req, 8<<20)
	var se statusErr
	if errors.As(err, &se) && (se.code == 401 || se.code == 402 || se.code == 403) {
		return nil, fmt.Errorf("%w (%s)", errRefused, se)
	}
	if err != nil {
		return nil, err
	}
	return parseFR24(b)
}

func parseFR24(b []byte) ([]Aircraft, error) {
	var res struct {
		Data []struct {
			FR24ID    string    `json:"fr24_id"`
			Hex       string    `json:"hex"`
			Callsign  string    `json:"callsign"`
			Flight    string    `json:"flight"`
			Lat       float64   `json:"lat"`
			Lon       float64   `json:"lon"`
			Track     float64   `json:"track"`
			Alt       int       `json:"alt"`
			GSpeed    float64   `json:"gspeed"`
			VSpeed    int       `json:"vspeed"`
			Squawk    string    `json:"squawk"`
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Reg       string    `json:"reg"`
			OrigIATA  string    `json:"orig_iata"`
			DestIATA  string    `json:"dest_iata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	out := make([]Aircraft, 0, len(res.Data))
	for _, d := range res.Data {
		id := strings.ToLower(d.Hex)
		if id == "" {
			id = d.FR24ID
		}
		call := d.Callsign
		if call == "" {
			call = d.Flight
		}
		at := d.Timestamp
		if at.IsZero() {
			at = time.Now()
		}
		out = append(out, Aircraft{
			ID: id, Callsign: call, Reg: d.Reg, Type: d.Type, From: d.OrigIATA, To: d.DestIATA, Squawk: d.Squawk,
			Lat: d.Lat, Lon: d.Lon, Track: d.Track, AltFt: d.Alt, Ground: d.Alt <= 0 && d.GSpeed < 40,
			SpeedKt: d.GSpeed, VRate: d.VSpeed, At: at,
		})
	}
	return out, nil
}

// demoAircraft is a few made-up aircraft flying circles and lines around lat, lon, for a board to look
// at with no source (aircraft "demo").
func demoAircraft(lat, lon float64, now time.Time) []Aircraft {
	type route struct {
		call, typ        string
		radiusKm, period float64 // a circle: its size and how long a lap takes, in seconds
		alt, kt          int
		phase            float64
	}
	routes := []route{
		{"TAP1234", "A20N", 18, 600, 9000, 260, 0},
		{"RYR8QF", "B38M", 45, 1500, 34000, 450, 1.3},
		{"EZY61LP", "A320", 70, 2100, 37000, 470, 2.6},
		{"IBE3121", "A21N", 30, 900, 15000, 320, 4.1},
		{"N712PH", "C172", 8, 420, 2500, 95, 5.0},
	}
	cosLat := math.Cos(lat * math.Pi / 180)
	var out []Aircraft
	for i, r := range routes {
		ang := r.phase + 2*math.Pi*math.Mod(float64(now.Unix()), r.period)/r.period
		a := Aircraft{ID: fmt.Sprintf("demo%d", i), Callsign: r.call, Type: r.typ, AltFt: r.alt, SpeedKt: float64(r.kt), At: now}
		a.Lat = lat + r.radiusKm*math.Cos(ang)/111.32
		a.Lon = lon + r.radiusKm*math.Sin(ang)/(111.32*cosLat)
		// ang is a bearing from home, growing clockwise: the way along the circle is a quarter turn on.
		a.Track = math.Mod(ang*180/math.Pi+90+360, 360)
		a.SpeedKt = 0 // placed each answer, not glided: the circle is not a line
		out = append(out, a)
	}
	return out
}

// merge is the aircraft from an answer, each with its trail from the ones before; an aircraft missing
// from the answer is kept until forgetAfter, so one dropped by a single answer does not blink.
func merge(old map[string]Aircraft, got []Aircraft, now time.Time) map[string]Aircraft {
	out := make(map[string]Aircraft, len(got))
	for _, a := range got {
		if prev, ok := old[a.ID]; ok {
			a.Trail = prev.Trail
			if prev.Lat != a.Lat || prev.Lon != a.Lon {
				a.Trail = append(a.Trail[:len(a.Trail):len(a.Trail)], [2]float64{prev.Lat, prev.Lon})
			}
			if len(a.Trail) > trailLen {
				a.Trail = a.Trail[len(a.Trail)-trailLen:]
			}
		}
		out[a.ID] = a
	}
	for id, a := range old {
		if _, ok := out[id]; !ok && now.Sub(a.At) < forgetAfter {
			out[id] = a
		}
	}
	return out
}
