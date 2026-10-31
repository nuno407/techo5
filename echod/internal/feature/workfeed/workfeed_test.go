package workfeed

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A source with no token is an empty state that says what to add; with demo on it is made up instead.
func TestViewWithoutTokens(t *testing.T) {
	defer func(d string) { settingsDir = d }(settingsDir)
	settingsDir = t.TempDir()
	f := &Feature{feeds: map[Source]*Feed{}, next: map[Source]time.Time{}}
	v := f.View()
	for _, fd := range v.Feeds {
		if fd.State != Off || !strings.Contains(fd.Setup, "work.json") {
			t.Errorf("%s: %+v", fd.Source, fd)
		}
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "work.json"), []byte(`{"demo": true, "jira": {"site": "x", "email": "e", "token": "t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	v = f.View()
	if !v.Feeds[0].Demo || !v.Feeds[1].Demo || v.Feeds[2].State != Loading || v.Hot == 0 {
		t.Errorf("demo: %+v", v)
	}
	// A broken file keeps the settings before it, and says so.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(settingsDir, "work.json"), []byte(`{"demo": tr`), 0o600)
	v = f.View()
	if v.Problem == "" || !v.Feeds[0].Demo {
		t.Errorf("broken file: %+v", v)
	}
}

func TestGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/notifications":
			if r.Header.Get("If-Modified-Since") == "Sat, 31 Oct 2026 00:00:00 GMT" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Last-Modified", "Sat, 31 Oct 2026 00:00:00 GMT")
			w.Header().Set("X-Poll-Interval", "300")
			w.Write([]byte(`[{"id":"1","unread":true,"reason":"mention","updated_at":"2026-10-30T10:00:00Z",
				"subject":{"title":"Fix it","url":"https://api.github.com/repos/o/r/issues/5","type":"Issue"},
				"repository":{"full_name":"o/r"}},
				{"id":"2","unread":true,"reason":"review_requested","updated_at":"2026-10-30T11:00:00Z",
				"subject":{"title":"Add it","url":"https://api.github.com/repos/o/r/pulls/7","type":"PullRequest"},
				"repository":{"full_name":"o/r"}},
				{"id":"3","unread":false,"reason":"subscribed","subject":{"title":"read"}}]`))
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested"):
			w.Write([]byte(`{"items":[{"id":70,"number":7,"title":"Add it","html_url":"https://github.com/o/r/pull/7",
				"repository_url":"https://api.github.com/repos/o/r","updated_at":"2026-10-30T11:00:00Z","user":{"login":"ana"}}]}`))
		case r.URL.Path == "/search/issues":
			w.Write([]byte(`{"items":[{"id":80,"number":8,"title":"Mine","html_url":"https://github.com/o/r/pull/8",
				"repository_url":"https://api.github.com/repos/o/r","updated_at":"2026-10-29T11:00:00Z","user":{"login":"me"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	var g githubState
	fd, every, err := g.fetch(context.Background(), GitHubSettings{Token: "tok", API: srv.URL}, githubEvery)
	if err != nil {
		t.Fatal(err)
	}
	if every != 5*time.Minute {
		t.Errorf("poll interval %v", every)
	}
	// The review request once, though it is a notification and a search result both.
	if len(fd.Items) != 3 || fd.Counts[0].N != 1 || fd.Counts[1].N != 1 || fd.Counts[2].N != 2 || fd.Counts[3].N != 1 {
		t.Fatalf("items %+v counts %+v", fd.Items, fd.Counts)
	}
	for _, it := range fd.Items {
		if it.Title == "Fix it" && (it.URL != "https://github.com/o/r/issues/5" || !it.Hot) {
			t.Errorf("mention %+v", it)
		}
	}
	// Nothing new: the notifications kept.
	fd, _, err = g.fetch(context.Background(), GitHubSettings{Token: "tok", API: srv.URL}, githubEvery)
	if err != nil || len(fd.Items) != 3 {
		t.Errorf("304: %v %d", err, len(fd.Items))
	}
	if _, _, err := g.fetch(context.Background(), GitHubSettings{Token: "bad", API: srv.URL}, githubEvery); err == nil {
		t.Error("a refused token passed")
	}
}

func TestWebURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com/repos/o/r/pulls/12":      "https://github.com/o/r/pull/12",
		"https://ghe.acme.com/api/v3/repos/o/r/issues/3": "https://ghe.acme.com/o/r/issues/3",
		"": "",
	} {
		if got := webURL(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestSlack(t *testing.T) {
	now := time.Now()
	recent := now.Add(-time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxp" {
			w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		switch r.URL.Path {
		case "/auth.test":
			w.Write([]byte(`{"ok":true,"user_id":"UME","url":"https://acme.slack.com/"}`))
		case "/search.messages":
			w.Write([]byte(`{"ok":true,"messages":{"matches":[{"ts":"` + itoa(recent) + `.000100","text":"hey <@UME> see <https://x.y|this>","user":"UA","username":"ana","permalink":"https://acme.slack.com/p1","channel":{"id":"C1","name":"general"}}]}}`))
		case "/users.conversations":
			w.Write([]byte(`{"ok":true,"channels":[{"id":"D1","user":"UB"},{"id":"D2","user":"UC"}]}`))
		case "/conversations.info":
			if r.URL.Query().Get("channel") == "D1" {
				w.Write([]byte(`{"ok":true,"channel":{"last_read":"1000.0","latest":{"ts":"` + itoa(recent) + `.0","user":"UB","text":"lunch?"}}}`))
			} else {
				w.Write([]byte(`{"ok":true,"channel":{"last_read":"` + itoa(recent) + `.0","latest":{"ts":"` + itoa(recent) + `.0","user":"UC","text":"read"}}}`))
			}
		case "/users.info":
			w.Write([]byte(`{"ok":true,"user":{"profile":{"display_name":"bea"}}}`))
		}
	}))
	defer srv.Close()
	defer func(a string) { slackAPI = a }(slackAPI)
	slackAPI = srv.URL
	var s slackState
	fd, err := s.fetch(context.Background(), SlackSettings{Token: "xoxp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fd.Items) != 2 || fd.Counts[0].N != 1 || fd.Counts[1].N != 1 {
		t.Fatalf("%+v", fd)
	}
	if fd.Items[0].Body != "hey @bea see this" || fd.Items[1].Who != "bea" || fd.Items[1].URL != "https://acme.slack.com/archives/D1" {
		t.Errorf("%+v", fd.Items)
	}
	var bad slackState
	if _, err := bad.fetch(context.Background(), SlackSettings{Token: "nope"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("refused token: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestJira(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("me@x:tok")) {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/rest/api/3/search/jql" {
			w.WriteHeader(404)
			return
		}
		if strings.HasPrefix(r.URL.Query().Get("jql"), "watcher") {
			w.Write([]byte(`{"issues":[]}`))
			return
		}
		w.Write([]byte(`{"issues":[{"id":"1","key":"P-1","fields":{"summary":"Burning","updated":"2026-10-01T10:00:00.000+0000",
			"status":{"name":"To Do"},"priority":{"name":"Highest"},"issuetype":{"name":"Bug"},"project":{"name":"Panel"}}},
			{"id":"2","key":"P-2","fields":{"summary":"Calm","updated":"2026-10-01T10:00:00.000+0000",
			"status":{"name":"To Do"},"priority":{"name":"Low"},"issuetype":{"name":"Task"},"project":{"name":"Panel"}}}]}`))
	}))
	defer srv.Close()
	fd, err := fetchJira(context.Background(), JiraSettings{Site: srv.URL, Email: "me@x", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fd.Items) != 2 || !fd.Items[0].Hot || fd.Items[1].Hot || fd.Counts[0].N != 1 || fd.Items[0].URL != srv.URL+"/browse/P-1" {
		t.Errorf("%+v", fd)
	}
	if _, err := fetchJira(context.Background(), JiraSettings{Site: srv.URL, Email: "me@x", Token: "bad"}); err == nil {
		t.Error("a refused token passed")
	}
}

func TestSlackTime(t *testing.T) {
	if got := slackTime("1700000000.000200"); got.Unix() != 1700000000 || got.Nanosecond() != 200000 {
		t.Errorf("%v", got)
	}
	if !slackTime("").IsZero() {
		t.Error("empty ts")
	}
}
