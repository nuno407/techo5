package wifi

import "testing"

func TestRSSIFromSignalPoll(t *testing.T) {
	if n, ok := rssi("RSSI=-34\nLINKSPEED=433\nNOISE=9999\nFREQUENCY=5320\n"); !ok || n != -34 {
		t.Errorf("rssi = %d, %v; want -34, true", n, ok)
	}
	if _, ok := rssi("FAIL\n"); ok {
		t.Error("a refusal has no RSSI")
	}
}
