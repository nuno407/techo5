package workfeed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// Settings is work.json: the tokens and sites, and whether to fill the sources without a token with
// made-up items. Never saved by the device: the owner writes it.
type Settings struct {
	Demo   bool           `json:"demo"`
	GitHub GitHubSettings `json:"github"`
	Slack  SlackSettings  `json:"slack"`
	Jira   JiraSettings   `json:"jira"`
}

type GitHubSettings struct {
	Token string `json:"token"`
	// API is GitHub's API root, for GitHub Enterprise; empty is api.github.com.
	API string `json:"api"`
}

type SlackSettings struct {
	Token string `json:"token"`
}

type JiraSettings struct {
	Site  string `json:"site"` // acme.atlassian.net, with or without https://
	Email string `json:"email"`
	Token string `json:"token"`
	JQL   string `json:"jql"` // in place of the default: assigned to me and not done
}

func (s GitHubSettings) set() bool { return strings.TrimSpace(s.Token) != "" }
func (s SlackSettings) set() bool  { return strings.TrimSpace(s.Token) != "" }
func (s JiraSettings) set() bool {
	return strings.TrimSpace(s.Site) != "" && strings.TrimSpace(s.Email) != "" && strings.TrimSpace(s.Token) != ""
}

// base is the site as a URL root: https:// added when it was written without.
func (s JiraSettings) base() string {
	site := strings.TrimRight(strings.TrimSpace(s.Site), "/")
	if !strings.Contains(site, "://") {
		site = "https://" + site
	}
	return site
}

// settingsDir is where work.json is; a test moves it.
var settingsDir = layout.StateDir

// SettingsPath is the file the tokens are kept in, for the board to say where to put them.
func SettingsPath() string { return filepath.Join(settingsDir, "work.json") }

// The file as last read, and what it was when it was read: it is looked at again when its time or size
// changes, which is at most a stat a poll.
var settings struct {
	sync.Mutex
	got     Settings
	mod     time.Time
	size    int64
	problem string // the file is there but could not be read: said on the board
}

// current is work.json as it is now, and what is wrong with it when it could not be read. No file is no
// settings and no problem: every source is then an empty state.
func current() (Settings, string) {
	settings.Lock()
	defer settings.Unlock()
	fi, err := os.Stat(SettingsPath())
	if err != nil {
		settings.got, settings.mod, settings.size, settings.problem = Settings{}, time.Time{}, 0, ""
		return settings.got, ""
	}
	if fi.ModTime().Equal(settings.mod) && fi.Size() == settings.size {
		return settings.got, settings.problem
	}
	settings.mod, settings.size = fi.ModTime(), fi.Size()
	b, err := os.ReadFile(SettingsPath())
	var s Settings
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		// Kept as it was before: a file half written by an editor is not every token gone.
		settings.problem = "work.json: " + err.Error()
		return settings.got, settings.problem
	}
	settings.got, settings.problem = s, ""
	return s, ""
}
