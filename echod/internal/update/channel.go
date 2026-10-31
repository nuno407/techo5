package update

import "github.com/HuskerMinion/techo5/echod/internal/layout"

// Channel is which stream of releases a device follows. The URLs are compiled in rather than
// configured, so Home Assistant can move a device between our own streams but cannot aim it at
// somebody else's binary — the device runs this as root, and a text field would be a way to replace it.
type Channel int

const (
	// Stable is what a device follows unless somebody says otherwise. GitHub's "latest" excludes
	// prereleases, so a stable device never sees a dev build.
	Stable Channel = iota

	// Dev is the rolling release whose assets are replaced by hand, for a device being worked on.
	Dev
)

// Label is what the setting is called in Home Assistant, and what is stored.
func (c Channel) Label() string {
	switch c {
	case Dev:
		return "dev"
	}
	return "stable"
}

// URL is where that channel's manifest lives, and nowhere on a build that publishes none (Published):
// a panel's app updates it.
//
// Both are plain asset downloads rather than api.github.com: the REST API allows sixty unauthenticated
// requests an hour per address, which several devices behind one router would share with everything else
// on the network. Asset paths do not count against it.
func (c Channel) URL() string {
	if !Published() {
		return ""
	}
	switch c {
	case Dev:
		return releases + "/download/dev/manifest.json"
	}
	return releases + "/latest/download/manifest.json"
}

// Published is whether this build has releases to check at all: a panel's daemon comes inside its
// app, which updates itself. Nor does the Surface RT follow them: its daemon is built for it and
// installed by hand, and the Show's arm binary in a release, which does not know the board, would
// replace it.
func Published() bool { return releases != "" && !layout.SurfaceRT() }

// Channels is every channel, in the order Home Assistant should offer them.
func Channels() []Channel { return []Channel{Stable, Dev} }
