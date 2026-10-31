//go:build !dot && !spot && !panel

package buttons

import "testing"

// The mute button is KEY_POWER under Amazon's kernels and KEY_MICMUTE on the 2nd gen Show 8's
// mainline kernel; both are the mute, and neither is anything else.
func TestShowMuteCodes(t *testing.T) {
	for code, want := range map[uint16]Name{114: VolumeDown, 115: VolumeUp, 116: Mute, 248: Mute} {
		if got, ok := codes[code]; !ok || got != want {
			t.Errorf("code %d is %q (%v), want %q", code, got, ok, want)
		}
	}
	if len(codes) != 4 {
		t.Errorf("%d codes, want 4: %v", len(codes), codes)
	}
}

// Devices that declare the mute's code but are not the mute button: the FocalTech touchscreen's
// gestures, the 2nd gen Show 8's PMIC power key on mainline and the Skylight's RK809 power key. The
// real buttons, the Skylight's adc-keys among them, are read.
func TestShowNotButtons(t *testing.T) {
	for name, want := range map[string]bool{
		"fts_ts": true, "mtk-pmic-keys": true, "rk805 pwrkey": true,
		"gpio-keys": false, "gpio-privacy-button": false, "goodix-ts": false, "adc-keys": false,
	} {
		if notButtons[name] != want {
			t.Errorf("notButtons[%q] = %v, want %v", name, notButtons[name], want)
		}
	}
}

// The Surface RT's power key is KEY_POWER in the same gpio-keys as its volume keys: there it is not the
// mute, nor is anything but the volume.
func TestTheSurfaceRTReadsOnlyTheVolume(t *testing.T) {
	c := showCodes(true)
	if len(c) != 2 || c[114] != VolumeDown || c[115] != VolumeUp {
		t.Errorf("Surface RT codes %v, want only the volume keys", c)
	}
	if _, ok := c[116]; ok {
		t.Error("the Surface RT's power key is read as a button")
	}
}
