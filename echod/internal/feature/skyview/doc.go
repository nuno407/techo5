// Package skyview is the Sky board's data: a map around home, the rain radar over it and the aircraft
// in the air, fetched and kept by the device; display's render_sky.go draws it. Nothing is a web view:
// the map is tiles composed here, the aircraft are positions drawn over them.
//
// The map is OpenStreetMap's own tiles, darkened on the device for a screen in a dark room (or as they
// come, "map": "osm"), fetched two at a time with TECHO5's User-Agent as the tile policy asks, and
// kept on /data for two weeks (tiles.go): a map once seen is not asked again, and a pan over known
// ground costs nothing. The radar is RainViewer's newest frame, the source the clock's rain map already
// uses; its free tiles stop at zoom 7, so at closer zooms its tile is cut and enlarged once and kept.
// Windy has no radar that can be drawn outside its own JavaScript map (the Map Forecast API is a web
// library, and its terms tie the layers to it); with a Windy key the board shows Windy's point forecast
// for home instead (wind, gusts, rain), which its Point Forecast API allows.
//
// The aircraft come from, in sky.json's "aircraft":
//
//   - "auto" (the default): a local dump1090 or readsb when "dump1090" names its aircraft.json, otherwise
//     adsb.lol, a free community ADS-B network (its data under the ODbL, credited on the board).
//   - "fr24": Flightradar24's official API (fr24api.flightradar24.com) with "fr24_token". It is paid by
//     credits per aircraft returned, so it is asked only every "fr24_every" seconds (60 unless set), and
//     a token it refuses (no credits left, no subscription) falls back to adsb.lol, said on the board.
//   - "adsblol", "dump1090", "demo" (made-up aircraft circling home), "off".
//
// Aircraft and the radar are fetched only while the board is on the screen, and for a minute after:
// adsb.lol every eight seconds, the radar's list of frames every five minutes, Windy every half hour.
// Between two answers each aircraft is moved along its track at its speed, so they glide rather than
// jump.
//
// sky.json is in the state directory (/data/misc/techo5/sky.json), read again when it changes; every
// field may be left out:
//
//	{
//	  "aircraft": "auto", "fr24_token": "", "fr24_every": 60, "dump1090": "http://pi.local/tar1090/data/aircraft.json",
//	  "radius_km": 150, "windy_key": "", "map": "dark", "radar": true,
//	  "lat": 0, "lon": 0, "zoom": 9
//	}
//
// lat and lon replace home's place from Home Assistant; zoom is where the board opens (5 to 13).
package skyview
