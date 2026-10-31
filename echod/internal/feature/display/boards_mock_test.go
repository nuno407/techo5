//go:build mockup && !dot && !spot

package display

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/skyview"
	"github.com/HuskerMinion/techo5/echod/internal/feature/workfeed"
)

// Mockups of the boards on the Surface RT's 1366x768 (MOCK_OUT): Work with its made-up items and an
// item's card, and Sky drawn from the live sources, as the device would (the map's tiles, RainViewer's
// rain and adsb.lol's aircraft around MOCK_LAT, MOCK_LON, Lisbon unless set). Needs the network.
func TestMockBoards(t *testing.T) {
	out := os.Getenv("MOCK_OUT")
	if out == "" {
		t.Skip("MOCK_OUT not set")
	}
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	previewFont(t)
	const w, h = 1366, 768
	now := time.Now()
	save := func(name string, s scene) {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		newRenderer(img).draw(s)
		f, err := os.Create(filepath.Join(out, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, img)
	}
	work := workfeed.DemoView(now)
	save("work", scene{now: now, phase: "idle", board: boardView{name: boardWork, work: work}})
	save("work-card", scene{now: now, phase: "idle", board: boardView{name: boardWork, work: work, detail: work.Feeds[0].Items[0].ID}})

	lat, lon := os.Getenv("MOCK_LAT"), os.Getenv("MOCK_LON")
	if lat == "" {
		lat, lon = "38.72", "-9.14"
	}
	dir := t.TempDir()
	if cache := os.Getenv("MOCK_TILES"); cache != "" {
		dir = cache
	}
	os.WriteFile(filepath.Join(dir, "sky.json"), []byte(`{"lat": `+lat+`, "lon": `+lon+`}`), 0o644)
	skyview.UseDir(dir)
	sky := skyview.Get()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sky.Run(ctx)
	scratch := image.NewRGBA(image.Rect(0, 0, w, h))
	shot := func(name string, pick bool) {
		// Until the aircraft are in and the tiles have stopped arriving.
		var v skyview.View
		for i := 0; i < 60; i++ {
			sky.Looked(w, h)
			v = sky.View(w, h)
			before := scratch.Pix[w*4*h/2]
			sky.Draw(scratch)
			if len(v.Aircraft) > 0 && v.RadarOn && i > 8 && before == scratch.Pix[w*4*h/2] {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(3 * time.Second)
		sky.Draw(scratch)
		v = sky.View(w, h)
		sel := ""
		if pick {
			for _, a := range v.Aircraft {
				if !a.Ground {
					sel = a.ID
				}
			}
		}
		save(name, scene{now: time.Now(), phase: "idle", board: boardView{name: boardSky, sky: v, selected: sel}})
		t.Logf("%s: %d aircraft from %s, note %q", name, len(v.Aircraft), v.Source, v.Note)
	}
	shot("sky", true)
	sky.ZoomAt(-2, image.Pt(w/2, h/2))
	shot("sky-wide", false)
	sky.ZoomAt(+3, image.Pt(w/2, h/2))
	sky.Pan(-200, 120)
	shot("sky-close", false)
}
