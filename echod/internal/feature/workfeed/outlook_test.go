package workfeed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMicrosoft is the sign-in and Graph: a device code entered on the second ask, refresh tokens that
// rotate, and a calendar with one meeting under way and one cancelled.
type fakeMicrosoft struct {
	asks     atomic.Int32
	refresh  atomic.Int32
	revoked  atomic.Bool
	calendar atomic.Int32
}

func (m *fakeMicrosoft) serve(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	switch {
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/devicecode"):
		if r.Form.Get("scope") != outlookScopes || r.Form.Get("client_id") != "app" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_request","error_description":"AADSTS9000: bad\r\nTrace ID: x"}`))
			return
		}
		w.Write([]byte(`{"device_code":"dev","user_code":"ABCD1234","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`))
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if m.asks.Add(1) == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			w.Write([]byte(`{"access_token":"a1","refresh_token":"r1","expires_in":3600}`))
		case "refresh_token":
			if m.revoked.Load() {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS700082: expired"}`))
				return
			}
			n := m.refresh.Add(1)
			w.Write([]byte(`{"access_token":"a2","refresh_token":"r` + string(rune('1'+n)) + `","expires_in":3600}`))
		}
	case r.URL.Path == "/me/calendarView":
		m.calendar.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer a") || r.Header.Get("Prefer") != `outlook.timezone="UTC"` {
			w.WriteHeader(401)
			return
		}
		now := time.Now().UTC()
		f := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.0000000") }
		json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
			{"id": "1", "subject": "Stand-up", "start": map[string]string{"dateTime": f(now.Add(-10 * time.Minute))},
				"end": map[string]string{"dateTime": f(now.Add(20 * time.Minute))}, "isOnlineMeeting": true,
				"onlineMeeting": map[string]string{"joinUrl": "https://teams/x"}, "showAs": "busy"},
			{"id": "2", "subject": "Gone", "isCancelled": true, "start": map[string]string{"dateTime": f(now)}, "end": map[string]string{"dateTime": f(now)}},
			{"id": "3", "subject": "Review", "start": map[string]string{"dateTime": f(now.Add(time.Hour))},
				"end": map[string]string{"dateTime": f(now.Add(2 * time.Hour))}, "location": map[string]string{"displayName": "Room 1"}},
		}})
	default:
		w.WriteHeader(404)
	}
}

func useMicrosoft(t *testing.T) *fakeMicrosoft {
	m := &fakeMicrosoft{}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	login, graph, dir := msLogin, msGraph, settingsDir
	t.Cleanup(func() { msLogin, msGraph, settingsDir = login, graph, dir })
	msLogin, msGraph, settingsDir = srv.URL, srv.URL, t.TempDir()
	return m
}

// The device-code sign-in: nothing asked while nobody is at the board, then a code to show, then the
// meetings once it is entered, with the refresh token kept for root alone.
func TestOutlookSignIn(t *testing.T) {
	m := useMicrosoft(t)
	s := OutlookSettings{ClientID: "app"}
	var o outlookState
	ctx := context.Background()

	fd, every, err := o.fetch(ctx, s, false)
	if err != nil || fd.State != SigningIn || fd.SignIn.Code != "" || every != signInIdle || m.asks.Load() != 0 {
		t.Fatalf("unlooked: %+v %v %v", fd, every, err)
	}
	fd, every, err = o.fetch(ctx, s, true)
	if err != nil || fd.SignIn == nil || fd.SignIn.Code != "ABCD1234" || every != 5*time.Second {
		t.Fatalf("code: %+v %v %v", fd, every, err)
	}
	fd, _, err = o.fetch(ctx, s, true) // pending
	if err != nil || fd.State != SigningIn || fd.SignIn.Code != "ABCD1234" {
		t.Fatalf("pending: %+v %v", fd, err)
	}
	fd, every, err = o.fetch(ctx, s, true) // entered
	if err != nil || len(fd.Events) != 2 || every != outlookEvery {
		t.Fatalf("signed in: %+v %v", fd, err)
	}
	fi, err := os.Stat(tokenPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	cur, next := NowAndNext(fd.Events, time.Now())
	if cur == nil || cur.Title != "Stand-up" || cur.JoinURL != "https://teams/x" || next == nil || next.Location != "Room 1" {
		t.Errorf("now %+v next %+v", cur, next)
	}

	// A restart: the kept refresh token is swapped for a new one, which is kept in its place.
	var again outlookState
	if fd, _, err = again.fetch(ctx, s, false); err != nil || len(fd.Events) != 2 {
		t.Fatalf("after a restart: %+v %v", fd, err)
	}
	b, _ := os.ReadFile(tokenPath())
	if !strings.Contains(string(b), `"refresh_token":"r2"`) {
		t.Errorf("rotated token not kept: %s", b)
	}

	// Revoked: dropped, and a new sign-in asked for.
	m.revoked.Store(true)
	var third outlookState
	fd, _, err = third.fetch(ctx, s, true)
	if err != nil || fd.State != SigningIn || fd.SignIn.Code == "" {
		t.Fatalf("revoked: %+v %v", fd, err)
	}
	if _, err := os.Stat(tokenPath()); !os.IsNotExist(err) {
		t.Error("the revoked token is still on disk")
	}
}

// A refused app is said in Microsoft's first line, without its trace.
func TestOutlookRefusedApp(t *testing.T) {
	useMicrosoft(t)
	var o outlookState
	_, _, err := o.fetch(context.Background(), OutlookSettings{ClientID: "other"}, true)
	if err == nil || !strings.Contains(err.Error(), "AADSTS9000: bad") || strings.Contains(err.Error(), "Trace") {
		t.Errorf("%v", err)
	}
}

func TestParseEventsAllDay(t *testing.T) {
	loc := time.FixedZone("WET", 0)
	evs, err := parseEvents([]byte(`{"value":[{"id":"a","subject":"Holiday","isAllDay":true,
		"start":{"dateTime":"2026-11-02T00:00:00.0000000"},"end":{"dateTime":"2026-11-03T00:00:00.0000000"}},
		{"id":"b","subject":"","showAs":"free","start":{"dateTime":"2026-11-01T09:30:00.0000000"},"end":{"dateTime":"2026-11-01T10:00:00.0000000"}}]}`), loc)
	if err != nil || len(evs) != 2 || evs[0].Title != "(no title)" || !evs[0].Free || !evs[1].AllDay || evs[1].Start.Day() != 2 {
		t.Errorf("%+v %v", evs, err)
	}
}

// With no client id the calendar is an empty state that says what to add, and nothing is fetched.
func TestOutlookNotSetUp(t *testing.T) {
	defer func(d string) { settingsDir = d }(settingsDir)
	settingsDir = t.TempDir()
	f := &Feature{feeds: map[Source]*Feed{}, next: map[Source]time.Time{}}
	v := f.View()
	if v.Calendar.State != Off || !strings.Contains(v.Calendar.Setup, "client_id") {
		t.Errorf("%+v", v.Calendar)
	}
}
