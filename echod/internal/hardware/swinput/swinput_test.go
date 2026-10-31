package swinput

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func event(typ, code uint16, value int32) []byte {
	size := eventSize()
	e := make([]byte, size)
	t := size - 8
	binary.LittleEndian.PutUint16(e[t:], typ)
	binary.LittleEndian.PutUint16(e[t+2:], code)
	binary.LittleEndian.PutUint32(e[t+4:], uint32(value))
	return e
}

func TestOnlyTheLidSwitchCounts(t *testing.T) {
	size := eventSize()
	if v, ok := switchEvent(event(evSW, swLid, 1), size, swLid); !ok || !v {
		t.Error("a closing cover missed")
	}
	if v, ok := switchEvent(event(evSW, swLid, 0), size, swLid); !ok || v {
		t.Error("an opening cover missed")
	}
	if _, ok := switchEvent(event(0, 0, 0), size, swLid); ok {
		t.Error("a sync taken for the cover")
	}
	if _, ok := switchEvent(event(evSW, 2, 1), size, swLid); ok {
		t.Error("the headphone switch taken for the cover")
	}
}

func TestTheSensorIsFoundByName(t *testing.T) {
	dir := t.TempDir()
	for name, dev := range map[string]string{"event3": "gpio-keys", "event9": "GPIO Hall Effect Sensor", "event2": "Microsoft Surface RT WM8962 Headphones Jack"} {
		d := filepath.Join(dir, name, "device")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "name"), []byte(dev+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := inputDir
	inputDir = dir
	defer func() { inputDir = saved }()
	if got := find(Lid.match); got != "/dev/input/event9" {
		t.Fatalf("found %q", got)
	}
	if got := find(Headphones.match); got != "/dev/input/event2" {
		t.Fatalf("headphones found at %q", got)
	}
}

func TestChangesAreSaidOnce(t *testing.T) {
	var got []bool
	s := &Switch{}
	stop := s.Changed.Listen(func(v bool) { got = append(got, v) })
	defer stop()
	s.set(true)
	s.set(true)
	s.set(false)
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("said %v", got)
	}
}
