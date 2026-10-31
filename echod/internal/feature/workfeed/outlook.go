package workfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Outlook: the owner's Microsoft 365 calendar, today and the next days, from Microsoft Graph's
// /me/calendarView, every five minutes. The device signs in by itself with OAuth 2.0's device-code flow:
// with no token kept, and only while the board is on the screen, it asks Microsoft for a code, and the
// board shows the code and the address to enter it at (microsoft.com/devicelogin) from a phone. What
// comes back is a refresh token, kept in outlook-token.json beside work.json (mode 600) and swapped for
// a new one at each refresh, so the sign-in is once. A refresh token Microsoft no longer takes (revoked,
// a password changed, ninety days unused) is dropped, and the board asks for a sign-in again.
//
// The app it signs in as is the owner's own: an app registration in Microsoft Entra (portal.azure.com,
// App registrations, New registration) with "Accounts in any organizational directory and personal
// Microsoft accounts", "Allow public client flows" on (Authentication), and the delegated Microsoft
// Graph permissions Calendars.Read and offline_access. Its Application (client) ID goes in work.json as
// "outlook": {"client_id": "…"}; "tenant" is "common" unless the work tenant's id or domain is wanted
// (some tenants let only their own apps in, or ask an admin to consent). Microsoft's own first-party
// client IDs are not borrowed: they are not meant for other programs.
//
// calendarView answers no ETag or If-None-Match, and its delta form keeps a window fixed from the first
// call, which a calendar that moves with the day does not suit; a poll every five minutes asking only the
// fields drawn is the polite way. A 429 is waited out as Graph asks.

// Where Microsoft is; a test points them at its own server.
var (
	msLogin = "https://login.microsoftonline.com"
	msGraph = "https://graph.microsoft.com/v1.0"
)

const (
	outlookEvery  = 5 * time.Minute
	outlookScopes = "Calendars.Read offline_access"
	outlookDays   = 3 // today and the days after it asked for
	// signInIdle is how often the calendar looks again while it has no token and nobody is at the board
	// to sign in: no network, only whether somebody has come.
	signInIdle = 10 * time.Second
)

// OutlookSettings is work.json's "outlook".
type OutlookSettings struct {
	ClientID string `json:"client_id"`
	Tenant   string `json:"tenant"` // "common" unless set
	Days     int    `json:"days"`   // today and the days after it; 3 unless set
}

func (s OutlookSettings) set() bool { return strings.TrimSpace(s.ClientID) != "" }

func (s OutlookSettings) tenant() string {
	if t := strings.TrimSpace(s.Tenant); t != "" {
		return url.PathEscape(t)
	}
	return "common"
}

func (s OutlookSettings) days() int {
	if s.Days <= 0 {
		return outlookDays
	}
	return min(s.Days, 14)
}

// Event is one meeting.
type Event struct {
	ID         string
	Title      string
	Start, End time.Time
	AllDay     bool
	Location   string
	Online     bool   // a Teams (or other online) meeting
	JoinURL    string // where to join it
	Organizer  string
	Tentative  bool // shown as tentative, or not yet answered
	Free       bool // shown as free: on the calendar, not taking the time
	URL        string
}

// SignIn is a device-code sign-in waiting for the owner: the code, where to enter it, until when.
type SignIn struct {
	Code    string
	URL     string
	Expires time.Time
}

// outlookToken is outlook-token.json: the refresh token, and the app and tenant it is for, so a change
// of either in work.json is a new sign-in rather than a token refused forever.
type outlookToken struct {
	ClientID string `json:"client_id"`
	Tenant   string `json:"tenant"`
	Refresh  string `json:"refresh_token"`
}

// outlookState is the sign-in between polls.
type outlookState struct {
	mu      sync.Mutex
	loaded  bool
	tok     outlookToken
	access  string
	expires time.Time

	// The device code being waited on, and how often Microsoft lets it be asked about.
	device   string
	signIn   *SignIn
	interval time.Duration
}

// tokenPath is where the refresh token is kept.
func tokenPath() string { return filepath.Join(settingsDir, "outlook-token.json") }

func (o *outlookState) load(s OutlookSettings) {
	if o.loaded {
		return
	}
	o.loaded = true
	b, err := os.ReadFile(tokenPath())
	if err != nil {
		return
	}
	var t outlookToken
	if json.Unmarshal(b, &t) == nil && t.ClientID == s.ClientID && t.Tenant == s.tenant() {
		o.tok = t
	}
}

// save keeps the refresh token, readable by root alone, through a temporary file.
func (o *outlookState) save() error {
	b, err := json.Marshal(o.tok)
	if err != nil {
		return err
	}
	tmp := tokenPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // WriteFile keeps an existing file's mode
		return err
	}
	return os.Rename(tmp, tokenPath())
}

// forget drops the refresh token, here and on disk: the next poll asks for a sign-in.
func (o *outlookState) forget() {
	o.tok.Refresh, o.access, o.expires = "", "", time.Time{}
	_ = os.Remove(tokenPath())
}

// fetch is the calendar: its meetings when signed in, or the sign-in to show.
func (o *outlookState) fetch(ctx context.Context, s OutlookSettings, looked bool) (Feed, time.Duration, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load(s)
	if o.tok.ClientID != s.ClientID || o.tok.Tenant != s.tenant() {
		// Another app or tenant in work.json since the sign-in: that token is not for it.
		o.forget()
		o.device, o.signIn = "", nil
		o.tok = outlookToken{ClientID: s.ClientID, Tenant: s.tenant()}
	}
	if o.tok.Refresh == "" {
		return o.signingIn(ctx, s, looked)
	}
	if o.access == "" || time.Until(o.expires) < time.Minute {
		if err := o.refresh(ctx, s); err != nil {
			if errors.Is(err, errSignedOut) {
				slog.Info("workfeed: Outlook no longer takes the sign-in; asking for a new one")
				o.forget()
				return o.signingIn(ctx, s, looked)
			}
			return Feed{}, outlookEvery, err
		}
	}
	events, err := o.calendar(ctx, s)
	if errors.Is(err, errSignedOut) {
		// The access token refused before its time: once more with a fresh one.
		o.access = ""
		if err = o.refresh(ctx, s); err == nil {
			events, err = o.calendar(ctx, s)
		}
		if errors.Is(err, errSignedOut) {
			o.forget()
			return o.signingIn(ctx, s, looked)
		}
	}
	if err != nil {
		return Feed{}, outlookEvery, err
	}
	return Feed{Events: events}, outlookEvery, nil
}

// errSignedOut is Microsoft no longer taking the token: a new sign-in is needed.
var errSignedOut = errors.New("signed out")

// signingIn is the sign-in's next step: a code asked for while the board is up, then asked about at the
// pace Microsoft gives until it is entered, declined, or runs out. With nobody at the board nothing is
// asked: a code nobody sees is fifteen minutes of a code for nothing.
func (o *outlookState) signingIn(ctx context.Context, s OutlookSettings, looked bool) (Feed, time.Duration, error) {
	now := time.Now()
	if o.signIn != nil && now.After(o.signIn.Expires) {
		o.device, o.signIn = "", nil
	}
	if o.signIn == nil {
		if !looked {
			return Feed{State: SigningIn, SignIn: &SignIn{}}, signInIdle, nil
		}
		if err := o.newCode(ctx, s); err != nil {
			return Feed{}, outlookEvery, err
		}
		return Feed{State: SigningIn, SignIn: o.signInCopy()}, o.interval, nil
	}
	var res struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {s.ClientID},
		"device_code": {o.device},
	}
	if err := o.post(ctx, s, "token", form, &res); err != nil {
		return Feed{}, outlookEvery, err
	}
	switch res.Error {
	case "":
	case "authorization_pending":
		return Feed{State: SigningIn, SignIn: o.signInCopy()}, o.interval, nil
	case "slow_down":
		o.interval += 5 * time.Second
		return Feed{State: SigningIn, SignIn: o.signInCopy()}, o.interval, nil
	case "authorization_declined", "expired_token", "bad_verification_code":
		// A new code next time the board is up.
		o.device, o.signIn = "", nil
		return Feed{State: SigningIn, SignIn: &SignIn{}}, signInIdle, nil
	default:
		return Feed{}, outlookEvery, msError("sign-in", res.Error, res.Desc)
	}
	if res.Refresh == "" {
		return Feed{}, outlookEvery, errors.New("sign-in: no refresh token came: is offline_access granted?")
	}
	o.device, o.signIn = "", nil
	o.tok.Refresh, o.access = res.Refresh, res.Access
	o.expires = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	if err := o.save(); err != nil {
		return Feed{}, outlookEvery, fmt.Errorf("keeping the token: %w", err)
	}
	slog.Info("workfeed: signed in to Outlook")
	events, err := o.calendar(ctx, s)
	if err != nil {
		return Feed{}, outlookEvery, err
	}
	return Feed{Events: events}, outlookEvery, nil
}

func (o *outlookState) signInCopy() *SignIn {
	si := *o.signIn
	return &si
}

// newCode asks Microsoft for a device code.
func (o *outlookState) newCode(ctx context.Context, s OutlookSettings) error {
	var res struct {
		Device    string `json:"device_code"`
		User      string `json:"user_code"`
		URI       string `json:"verification_uri"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	form := url.Values{"client_id": {s.ClientID}, "scope": {outlookScopes}}
	if err := o.post(ctx, s, "devicecode", form, &res); err != nil {
		return err
	}
	if res.Error != "" || res.Device == "" {
		return msError("sign-in", res.Error, res.Desc)
	}
	o.device = res.Device
	o.signIn = &SignIn{Code: res.User, URL: res.URI, Expires: time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)}
	o.interval = max(time.Duration(res.Interval)*time.Second, 5*time.Second)
	return nil
}

// refresh swaps the refresh token for an access token and a new refresh token, and keeps the new one.
func (o *outlookState) refresh(ctx context.Context, s OutlookSettings) error {
	var res struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.ClientID},
		"refresh_token": {o.tok.Refresh},
		"scope":         {outlookScopes},
	}
	if err := o.post(ctx, s, "token", form, &res); err != nil {
		return err
	}
	switch res.Error {
	case "":
	case "invalid_grant", "interaction_required":
		return errSignedOut
	default:
		return msError("refreshing the sign-in", res.Error, res.Desc)
	}
	o.access = res.Access
	o.expires = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	if res.Refresh != "" && res.Refresh != o.tok.Refresh {
		o.tok.Refresh = res.Refresh
		if err := o.save(); err != nil {
			return fmt.Errorf("keeping the token: %w", err)
		}
	}
	return nil
}

// post is a form to the tenant's OAuth endpoint (devicecode or token). Microsoft answers its errors as
// JSON with a 400, which is out for the caller to read rather than a failed request.
func (o *outlookState) post(ctx context.Context, s OutlookSettings, endpoint string, form url.Values, out any) error {
	u := fmt.Sprintf("%s/%s/oauth2/v2.0/%s", msLogin, s.tenant(), endpoint)
	req, err := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a, err := do(ctx, req)
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	if a.status != http.StatusOK && a.status != http.StatusBadRequest && a.status != http.StatusUnauthorized {
		return statusError("sign-in", a)
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	return nil
}

// msError is Microsoft's error in a few words: the first line of its description, without the trace
// and correlation ids it adds.
func msError(what, code, desc string) error {
	desc, _, _ = strings.Cut(desc, "\r\n")
	desc, _, _ = strings.Cut(desc, "\n")
	if code == "" {
		code = "no answer"
	}
	if desc != "" {
		return fmt.Errorf("%s: %s: %s", what, code, desc)
	}
	return fmt.Errorf("%s: %s", what, code)
}

// calendar is the meetings from the start of today to the end of the days asked for, in order.
func (o *outlookState) calendar(ctx context.Context, s OutlookSettings) ([]Event, error) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 0, s.days())
	q := url.Values{
		"startDateTime": {from.UTC().Format(time.RFC3339)},
		"endDateTime":   {to.UTC().Format(time.RFC3339)},
		"$select":       {"id,subject,start,end,isAllDay,isCancelled,showAs,location,isOnlineMeeting,onlineMeeting,organizer,responseStatus,webLink"},
		"$orderby":      {"start/dateTime"},
		"$top":          {"60"},
	}
	req, err := http.NewRequest("GET", msGraph+"/me/calendarView?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.access)
	req.Header.Set("Prefer", `outlook.timezone="UTC"`)
	a, err := do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errSignedOut
	case http.StatusForbidden:
		return nil, errors.New("calendar: Calendars.Read was not granted to the app")
	default:
		return nil, statusError("calendar", a)
	}
	return parseEvents(a.body, now.Location())
}

func parseEvents(b []byte, loc *time.Location) ([]Event, error) {
	type when struct {
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	}
	var res struct {
		Value []struct {
			ID          string `json:"id"`
			Subject     string `json:"subject"`
			Start       when   `json:"start"`
			End         when   `json:"end"`
			IsAllDay    bool   `json:"isAllDay"`
			IsCancelled bool   `json:"isCancelled"`
			ShowAs      string `json:"showAs"`
			Location    struct {
				DisplayName string `json:"displayName"`
			} `json:"location"`
			IsOnline      bool `json:"isOnlineMeeting"`
			OnlineMeeting *struct {
				JoinURL string `json:"joinUrl"`
			} `json:"onlineMeeting"`
			Organizer struct {
				EmailAddress struct {
					Name string `json:"name"`
				} `json:"emailAddress"`
			} `json:"organizer"`
			ResponseStatus struct {
				Response string `json:"response"`
			} `json:"responseStatus"`
			WebLink string `json:"webLink"`
		} `json:"value"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	at := func(w when, allDay bool) time.Time {
		// Asked for in UTC (Prefer: outlook.timezone), sent without a zone and with seven decimals.
		t, err := time.ParseInLocation("2006-01-02T15:04:05.9999999", w.DateTime, time.UTC)
		if err != nil {
			return time.Time{}
		}
		if allDay {
			// An all-day event is the owner's whole day, not UTC's: its date, at local midnight.
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		return t.In(loc)
	}
	var out []Event
	for _, v := range res.Value {
		if v.IsCancelled {
			continue
		}
		e := Event{
			ID: v.ID, Title: v.Subject, Start: at(v.Start, v.IsAllDay), End: at(v.End, v.IsAllDay), AllDay: v.IsAllDay,
			Location: strings.TrimSpace(v.Location.DisplayName), Online: v.IsOnline, Organizer: v.Organizer.EmailAddress.Name,
			Tentative: v.ShowAs == "tentative" || v.ResponseStatus.Response == "notResponded" || v.ResponseStatus.Response == "tentativelyAccepted",
			Free:      v.ShowAs == "free",
			URL:       v.WebLink,
		}
		if v.OnlineMeeting != nil {
			e.JoinURL = v.OnlineMeeting.JoinURL
		}
		if e.Title == "" {
			e.Title = "(no title)"
		}
		if e.Start.IsZero() {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// NowAndNext are the meeting under way at now and the next to start, among those that take the time
// (not all-day, not shown as free); either may be missing.
func NowAndNext(events []Event, now time.Time) (cur, next *Event) {
	for i := range events {
		e := &events[i]
		if e.AllDay || e.Free {
			continue
		}
		if !now.Before(e.Start) && now.Before(e.End) && cur == nil {
			cur = e
		}
		if e.Start.After(now) && next == nil {
			next = e
		}
	}
	return cur, next
}
