package skyview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseReadsb(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := []byte(`{"ac":[
		{"hex":"4CA1","flight":"TAP12  ","r":"CS-TNP","t":"A320","lat":38.7,"lon":-9.1,"alt_baro":12000,"gs":300,"track":90,"seen_pos":2},
		{"hex":"4CA2","lat":38.8,"lon":-9.2,"alt_baro":"ground","gs":5,"seen_pos":1},
		{"hex":"4CA3","alt_baro":30000},
		{"hex":"4CA4","lat":1,"lon":1,"seen_pos":90}]}`)
	got, err := parseReadsb(b, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Callsign != "TAP12" || got[0].AltFt != 12000 || got[0].ID != "4ca1" || !got[1].Ground {
		t.Fatalf("%+v", got)
	}
	if !got[0].At.Equal(now.Add(-2 * time.Second)) {
		t.Errorf("at %v", got[0].At)
	}
	// dump1090's older names.
	got, _ = parseReadsb([]byte(`{"aircraft":[{"hex":"a1","lat":1,"lon":2,"altitude":5000,"speed":120}]}`), now)
	if len(got) != 1 || got[0].AltFt != 5000 || got[0].SpeedKt != 120 {
		t.Errorf("%+v", got)
	}
}

func TestParseFR24(t *testing.T) {
	got, err := parseFR24([]byte(`{"data":[{"fr24_id":"3a","hex":"4CA1","callsign":"TAP12","lat":38.7,"lon":-9.1,"track":90,
		"alt":12000,"gspeed":300,"vspeed":-640,"timestamp":"2026-10-31T01:00:00Z","type":"A320","reg":"CS-TNP","orig_iata":"LIS","dest_iata":"OPO"}]}`))
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	a := got[0]
	if a.ID != "4ca1" || a.From != "LIS" || a.To != "OPO" || a.VRate != -640 || a.Ground {
		t.Errorf("%+v", a)
	}
}

func TestFR24Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Version") != "v1" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(402)
	}))
	defer srv.Close()
	defer func(u string) { fr24Positions = u }(fr24Positions)
	fr24Positions = srv.URL + "/?bounds=%.3f,%.3f,%.3f,%.3f"
	if _, err := fetchFR24(context.Background(), "t", 38.7, -9.1, 100); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("%v", err)
	}
}

func TestParseWindy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := []byte(`{"ts":[1799996400000,1800007200000],"units":{"past3hprecip-surface":"m"},
		"wind_u-surface":[3,0],"wind_v-surface":[4,0],"gust-surface":[10,0],"past3hprecip-surface":[0.0012,0]}`)
	w, err := parseWindy(b, now)
	if err != nil {
		t.Fatal(err)
	}
	// u 3 east, v 4 north: 5 m/s, blowing to the north-east, so from the south-west.
	if math.Abs(w.SpeedKmh-18) > 0.01 || math.Abs(w.GustKmh-36) > 0.01 || math.Abs(w.From-216.87) > 0.1 || math.Abs(w.RainMm-1.2) > 0.001 {
		t.Errorf("%+v", w)
	}
}

func TestPosition(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	a := Aircraft{Lat: 0, Lon: 0, Track: 90, SpeedKt: 360, At: at}
	lat, lon := a.Position(at.Add(10 * time.Second))
	// 360 kt is 185.2 m/s: 1852 m east in ten seconds.
	if math.Abs(lat) > 1e-9 || math.Abs(lon*111320-1852) > 1 {
		t.Errorf("%v %v", lat, lon)
	}
	lat2, lon2 := a.Position(at.Add(10 * time.Minute))
	if math.Abs(lon2*111320-1852*4.5) > 1 || math.Abs(lat2) > 1e-9 {
		t.Errorf("not held at glideFor: %v", lon2)
	}
}

func TestMergeKeepsTrails(t *testing.T) {
	now := time.Now()
	m := merge(nil, []Aircraft{{ID: "a", Lat: 1, Lon: 1, At: now}}, now)
	m = merge(m, []Aircraft{{ID: "a", Lat: 2, Lon: 2, At: now}}, now)
	m = merge(m, []Aircraft{{ID: "b", Lat: 3, Lon: 3, At: now}}, now)
	if len(m) != 2 || len(m["a"].Trail) != 1 || m["a"].Trail[0] != [2]float64{1, 1} {
		t.Errorf("%+v", m)
	}
	m = merge(m, nil, now.Add(2*forgetAfter))
	if len(m) != 0 {
		t.Errorf("not forgotten: %+v", m)
	}
}

func TestZoomKeepsThePoint(t *testing.T) {
	f := &Feature{zoom: 9, w: 1000, h: 600}
	f.cx, f.cy = worldPixel(38.7, -9.1, 9)
	p := image.Pt(800, 100)
	lat0, lon0 := latLon(f.cx+300, f.cy-200, 9)
	f.ZoomAt(+1, p)
	lat1, lon1 := latLon(f.cx+300, f.cy-200, 10)
	if f.zoom != 10 || math.Abs(lat0-lat1) > 1e-6 || math.Abs(lon0-lon1) > 1e-6 {
		t.Errorf("zoom %d: %v,%v vs %v,%v", f.zoom, lat0, lon0, lat1, lon1)
	}
	f.Pan(100, 0)
	if lat2, lon2 := latLon(f.cx, f.cy, 10); lon2 >= lon1 || math.Abs(lat2-38.7) > 0.5 {
		t.Errorf("pan right should move the map's middle west: %v", lon2)
	}
}

// The map is drawn from tiles fetched once and kept on disk; a second store reads them from there.
func TestTilesComposeAndKeep(t *testing.T) {
	var asked atomic.Int32
	tile := func(c color.RGBA) []byte {
		img := image.NewRGBA(image.Rect(0, 0, 256, 256))
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
		}
		var b bytes.Buffer
		png.Encode(&b, img)
		return b.Bytes()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write(tile(color.RGBA{200, 10, 10, 255}))
	}))
	defer srv.Close()
	defer func(c string) { osmTiles = c }(osmTiles)
	osmTiles = srv.URL + "/%d/%d/%d.png"
	dir := t.TempDir()
	arrived := make(chan struct{}, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &Feature{zoom: 9, w: 512, h: 256}
	os.WriteFile(filepath.Join(dir, "sky.json"), []byte(`{"map": "osm"}`), 0o644)
	defer func(d string) { settingsDir = d }(settingsDir)
	settingsDir = dir
	f.cx, f.cy = 256*200+256, 256*150+128 // two whole tiles across
	f.tiles = newTileStore(dir, func() { arrived <- struct{}{} })
	go f.tiles.run(ctx)
	dst := image.NewRGBA(image.Rect(0, 0, 512, 256))
	f.Draw(dst) // asks for them
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("tiles did not arrive")
		}
	}
	f.gen++
	f.Draw(dst)
	if c := dst.RGBAAt(10, 10); c.R != 200 {
		t.Fatalf("left tile %v", c)
	}
	if c := dst.RGBAAt(500, 250); c.R != 200 {
		t.Fatalf("right tile %v", c)
	}
	n := asked.Load()
	g := &Feature{zoom: 9, w: 512, h: 256, cx: f.cx, cy: f.cy}
	g.tiles = newTileStore(dir, func() { arrived <- struct{}{} })
	go g.tiles.run(ctx)
	g.Draw(dst)
	for range 2 {
		<-arrived
	}
	if asked.Load() != n {
		t.Errorf("fetched again: %d then %d", n, asked.Load())
	}
}

// benchFeature is a view of the map with every tile, and the rain's, already in memory.
func benchFeature(b *testing.B) (*Feature, *image.RGBA) {
	defer func(d string) { settingsDir = d }(settingsDir)
	settingsDir = b.TempDir()
	f := &Feature{zoom: 9, w: 1366, h: 768}
	f.cx, f.cy = worldPixel(38.72, -9.14, 9)
	f.tiles = newTileStore("", func() {})
	f.radar = radarFrame{layer: "rv:test"}
	tile := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range tile.Pix {
		tile.Pix[i] = uint8(i)
	}
	for z := 7; z <= 9; z++ {
		n := 1 << z
		for x := range n {
			for y := range n {
				if z == 9 && (x < 240 || x > 250 || y < 190 || y > 200) {
					continue
				}
				if z < 9 && (x < 240>>(9-z) || x > 250>>(9-z) || y < 190>>(9-z) || y > 200>>(9-z)) {
					continue
				}
				f.tiles.keep(tileKey{mapDark, z, x, y}, tile)
				f.tiles.keep(tileKey{"rv:test", z, x, y}, tile)
			}
		}
	}
	f.tiles.mem.max, f.tiles.radar.max = 1000, 1000
	return f, image.NewRGBA(image.Rect(0, 0, 1366, 768))
}

// A frame while the map is still: one copy of the composed view.
func BenchmarkDrawStill(b *testing.B) {
	f, dst := benchFeature(b)
	f.Draw(dst)
	b.ResetTimer()
	for range b.N {
		f.Draw(dst)
	}
}

// A frame while a finger drags the map: the view composed again from the tiles in memory, the rain
// laid over it.
func BenchmarkDrawPanning(b *testing.B) {
	f, dst := benchFeature(b)
	f.Draw(dst)
	b.ResetTimer()
	for i := range b.N {
		f.Pan(1+i%3, 0)
		f.Draw(dst)
	}
}

// copySrc is draw.Draw with draw.Src, clipping and all.
func TestCopySrcIsDrawSrc(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for i := range src.Pix {
		src.Pix[i] = byte(i * 7)
	}
	for _, c := range []struct {
		r  image.Rectangle
		sp image.Point
	}{
		{image.Rect(0, 0, 50, 50), image.Point{}},
		{image.Rect(-10, -5, 30, 25), image.Point{}},
		{image.Rect(20, 10, 60, 40), image.Pt(5, 3)},
		{image.Rect(5, 5, 25, 15), image.Pt(-4, 2)},
	} {
		want := image.NewRGBA(image.Rect(0, 0, 50, 40))
		got := image.NewRGBA(image.Rect(0, 0, 50, 40))
		draw.Draw(want, c.r, src, c.sp, draw.Src)
		copySrc(got, c.r, src, c.sp)
		if !bytes.Equal(got.Pix, want.Pix) {
			t.Errorf("%v from %v: differs from draw.Draw", c.r, c.sp)
		}
	}
}
