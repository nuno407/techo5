//go:build !dot && !spot

package display

import (
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
)

// The boards draw on every panel: Work with its three columns, an item's card, and a source that is not
// set up; Sky with its aircraft. With SHOW_PREVIEW set, each is written there to look at.
func TestBoardsDraw(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	at := time.Date(2026, 10, 31, 9, 41, 0, 0, time.Local)
	work := workfeed.DemoView(at)
	off := workfeed.DemoView(at)
	off.Feeds[2] = workfeed.Feed{Source: workfeed.Jira, Name: "Jira", State: workfeed.Off, Setup: `Add "jira": {"site", "email", "token"} to /data/misc/techo5/work.json`}
	off.Feeds[1] = workfeed.Feed{Source: workfeed.Slack, Name: "Slack", State: workfeed.Failed, Problem: "auth.test: invalid_auth"}
	planes := []skyview.Aircraft{
		{ID: "a", Callsign: "TAP1234", AltFt: 9000, SpeedKt: 260, Track: 45, Lat: 38.80, Lon: -9.10, At: at},
		{ID: "b", Callsign: "RYR8QF", AltFt: 34000, SpeedKt: 450, Track: 200, Lat: 38.60, Lon: -9.40, At: at,
			Trail: [][2]float64{{38.7, -9.3}, {38.65, -9.35}}},
		{ID: "c", Callsign: "N712PH", Ground: true, Lat: 38.77, Lon: -9.13, At: at},
	}
	signIn := workfeed.DemoView(at)
	signIn.Calendar = workfeed.Feed{Source: workfeed.Outlook, Name: "Outlook", State: workfeed.SigningIn,
		SignIn: &workfeed.SignIn{Code: "F7KQ2PLMX", URL: "https://microsoft.com/devicelogin", Expires: at.Add(14 * time.Minute)}}
	off.Calendar = workfeed.Feed{Source: workfeed.Outlook, Name: "Outlook", State: workfeed.Off, Setup: `Add "outlook": {"client_id": "…"} to /data/misc/techo5/work.json`}
	scenes := map[string]scene{
		"work-signin":  {now: at, phase: "idle", board: boardView{name: boardWork, data: signIn}},
		"work-meeting": {now: at, phase: "idle", board: boardView{name: boardWork, data: work, detail: work.Calendar.Events[1].ID}},
		"work":         {now: at, phase: "idle", board: boardView{name: boardWork, data: work}},
		"work-card":    {now: at, phase: "idle", board: boardView{name: boardWork, data: work, detail: work.Feeds[1].Items[0].ID}},
		"work-off":     {now: at, phase: "idle", board: boardView{name: boardWork, data: off}},
		"sky":          {now: at, phase: "idle", board: boardView{name: boardSky, selected: "a"}},
	}
	dir := os.Getenv("SHOW_PREVIEW")
	for name, s := range scenes {
		for _, panel := range []struct {
			name       string
			wide, high int
		}{{"", showWide, showHigh}, {"-surface", 1366, 768}, {"-skylight", skylightWide, skylightHigh}} {
			img := image.NewRGBA(image.Rect(0, 0, panel.wide, panel.high))
			r := newRenderer(img)
			if s.board.name == boardSky {
				v := skyview.View{Zoom: 9, W: panel.wide, H: panel.high, HomeOK: true, HomeLat: 38.72, HomeLon: -9.14,
					Aircraft: planes, Source: "adsb.lol", Credit: "© OpenStreetMap contributors © CARTO"}
				x, y := skyWorld(38.72, -9.14, 9)
				v.X0, v.Y0 = x-float64(panel.wide)/2, y-float64(panel.high)/2
				s.board.data = v
			}
			r.draw(s)
			boardHits.Lock()
			zones := boardHits.zones
			boardHits.Unlock()
			kinds := map[string]int{}
			for _, z := range zones {
				kinds[z.kind]++
			}
			if kinds["tab"] != 2 || kinds["close"] != 1 {
				t.Errorf("%s%s: zones %v", name, panel.name, kinds)
			}
			if name == "work" && kinds["item"] < 12 {
				t.Errorf("%s%s: %d items drawn", name, panel.name, kinds["item"])
			}
			if name == "sky" && (kinds["plane"] != 3 || kinds["zoomin"] != 1 || kinds["card"] != 1) {
				t.Errorf("%s%s: zones %v", name, panel.name, kinds)
			}
			if dir == "" {
				continue
			}
			f, err := os.Create(filepath.Join(dir, "board-"+name+panel.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(f, img)
			f.Close()
		}
	}
}

// skyWorld is a place's world pixel at a zoom, in Web Mercator as skyview places the map.
func skyWorld(lat, lon float64, zoom int) (float64, float64) {
	n := 256 * math.Pow(2, float64(zoom))
	phi := lat * math.Pi / 180
	return (lon + 180) / 360 * n, (1 - math.Log(math.Tan(phi)+1/math.Cos(phi))/math.Pi) / 2 * n
}
