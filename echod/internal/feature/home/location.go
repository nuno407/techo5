package home

// Location is home's place as the rain map finds it (homeLocation): Home Assistant's home zone, the
// place kept on the device, or Home Assistant's configuration. For the Sky board (feature/skyview).
func Location() (lat, lon float64, err error) { return homeLocation() }
