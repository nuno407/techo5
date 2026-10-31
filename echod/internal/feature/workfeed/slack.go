package workfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Slack: the mentions of the last day (search.messages, which needs search:read) and the direct
// messages with something unread (users.conversations, then conversations.info on each, which says
// what was last read and what came last). Slack's methods are rate-limited by tier, conversations.info
// at about fifty a minute: the direct messages are asked a second and a quarter apart, at most
// slackDMs of them, which keeps a poll inside the limit with room for the owner's own clients. A 429 is
// waited out as it asks (errLater).

// slackAPI is Slack's Web API; a test points it at its own server.
var slackAPI = "https://slack.com/api"

const (
	slackDMs     = 30                      // direct messages looked at in a poll
	slackGap     = 1250 * time.Millisecond // between two conversations.info
	slackMention = 24 * time.Hour          // a mention this recent waits on the owner
)

// slackState is what is kept between polls: who the token is, and the names already looked up.
type slackState struct {
	mu    sync.Mutex
	me    string
	team  string // the workspace's address, for the links
	names map[string]string
}

// slackCall is one Web API method: GET with the token, the JSON answer's ok checked. A false ok is
// errSlack with Slack's word for it.
func slackCall(ctx context.Context, token, method string, q url.Values, out any) error {
	req, err := http.NewRequest("GET", slackAPI+"/"+method+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	a, err := do(ctx, req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if a.status != http.StatusOK {
		return statusError(method, a)
	}
	var head struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	if err := json.Unmarshal(a.body, &head); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if !head.OK {
		return errSlack{method: method, code: head.Error, needed: head.Needed}
	}
	return json.Unmarshal(a.body, out)
}

type errSlack struct{ method, code, needed string }

func (e errSlack) Error() string {
	if e.needed != "" {
		return fmt.Sprintf("%s: %s (%s)", e.method, e.code, e.needed)
	}
	return e.method + ": " + e.code
}

func (s *slackState) fetch(ctx context.Context, set SlackSettings) (Feed, error) {
	tok := set.Token
	s.mu.Lock()
	me := s.me
	s.mu.Unlock()
	if me == "" {
		var who struct {
			UserID string `json:"user_id"`
			URL    string `json:"url"`
		}
		if err := slackCall(ctx, tok, "auth.test", url.Values{}, &who); err != nil {
			var se errSlack
			if errors.As(err, &se) && (se.code == "invalid_auth" || se.code == "not_authed" || se.code == "token_revoked") {
				return Feed{}, errors.New("the token was refused")
			}
			return Feed{}, err
		}
		s.mu.Lock()
		s.me, s.team, me = who.UserID, strings.TrimRight(who.URL, "/"), who.UserID
		s.mu.Unlock()
	}

	var fd Feed
	var notes []string
	mentions, err := s.mentions(ctx, tok, me)
	if se := (errSlack{}); errors.As(err, &se) && se.code == "missing_scope" {
		notes, err = append(notes, "Mentions need the search:read scope"), nil
	}
	if err != nil {
		return Feed{}, err
	}
	dms, err := s.directs(ctx, tok, me)
	if se := (errSlack{}); errors.As(err, &se) && se.code == "missing_scope" {
		notes, err = append(notes, "Direct messages need im:read and im:history"), nil
	}
	if err != nil {
		return Feed{}, err
	}
	hot := 0
	for _, m := range mentions {
		if m.Hot {
			hot++
		}
	}
	fd.Items = append(mentions, dms...)
	fd.Counts = []Count{{Label: "DMs", N: len(dms), Hot: true}, {Label: "Mentions", N: hot, Hot: true}}
	fd.Problem = strings.Join(notes, " · ")
	return fd, nil
}

// mentions are the messages naming the owner, newest first; those of the last day wait on the owner.
func (s *slackState) mentions(ctx context.Context, tok, me string) ([]Item, error) {
	var res struct {
		Messages struct {
			Matches []struct {
				Ts        string `json:"ts"`
				Text      string `json:"text"`
				User      string `json:"user"`
				Username  string `json:"username"`
				Permalink string `json:"permalink"`
				Channel   struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					IsIM   bool   `json:"is_im"`
					IsMPIM bool   `json:"is_mpim"`
				} `json:"channel"`
			} `json:"matches"`
		} `json:"messages"`
	}
	q := url.Values{"query": {"<@" + me + ">"}, "sort": {"timestamp"}, "sort_dir": {"desc"}, "count": {"10"}}
	if err := slackCall(ctx, tok, "search.messages", q, &res); err != nil {
		return nil, err
	}
	var out []Item
	for _, m := range res.Messages.Matches {
		if m.User == me {
			continue
		}
		at := slackTime(m.Ts)
		where := "#" + m.Channel.Name
		if m.Channel.IsIM || m.Channel.IsMPIM {
			where = "direct message"
		}
		who := m.Username
		if who == "" {
			who = s.name(ctx, tok, m.User)
		}
		text := s.plain(ctx, tok, m.Text)
		out = append(out, Item{
			ID: "sl-m-" + m.Channel.ID + "-" + m.Ts, Kind: "Mention", Title: firstLine(text), Where: where, Who: who,
			At: at, Hot: time.Since(at) < slackMention, Body: text, URL: m.Permalink,
		})
	}
	return out, nil
}

// directs are the direct messages (one to one and group) whose last message the owner has not read.
func (s *slackState) directs(ctx context.Context, tok, me string) ([]Item, error) {
	var list struct {
		Channels []struct {
			ID      string `json:"id"`
			User    string `json:"user"`
			IsMPIM  bool   `json:"is_mpim"`
			Name    string `json:"name"`
			Updated int64  `json:"updated"`
		} `json:"channels"`
	}
	q := url.Values{"types": {"im,mpim"}, "exclude_archived": {"true"}, "limit": {"100"}}
	if err := slackCall(ctx, tok, "users.conversations", q, &list); err != nil {
		return nil, err
	}
	chans := list.Channels
	// The ones most lately changed first, where Slack says; the first slackDMs of them.
	sort.SliceStable(chans, func(i, j int) bool { return chans[i].Updated > chans[j].Updated })
	if len(chans) > slackDMs {
		chans = chans[:slackDMs]
	}
	var out []Item
	for i, c := range chans {
		if i > 0 {
			select {
			case <-ctx.Done():
				return out, nil // what was found so far: the rest next time
			case <-time.After(slackGap):
			}
		}
		var info struct {
			Channel struct {
				LastRead      string `json:"last_read"`
				UnreadDisplay *int   `json:"unread_count_display"`
				Latest        *struct {
					Ts   string `json:"ts"`
					User string `json:"user"`
					Text string `json:"text"`
				} `json:"latest"`
			} `json:"channel"`
		}
		if err := slackCall(ctx, tok, "conversations.info", url.Values{"channel": {c.ID}}, &info); err != nil {
			var later errLater
			if errors.As(err, &later) && len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		ch := info.Channel
		if ch.Latest == nil && ch.LastRead != "" {
			// Not every token is told the last message here: it is then asked for, the newest since the
			// last read one.
			var hist struct {
				Messages []struct {
					Ts   string `json:"ts"`
					User string `json:"user"`
					Text string `json:"text"`
				} `json:"messages"`
			}
			time.Sleep(slackGap)
			q := url.Values{"channel": {c.ID}, "oldest": {ch.LastRead}, "limit": {"1"}}
			if err := slackCall(ctx, tok, "conversations.history", q, &hist); err == nil && len(hist.Messages) > 0 {
				m := hist.Messages[0]
				ch.Latest = &struct {
					Ts   string `json:"ts"`
					User string `json:"user"`
					Text string `json:"text"`
				}{m.Ts, m.User, m.Text}
			}
		}
		if ch.Latest == nil || ch.Latest.User == me {
			continue
		}
		unread := slackTime(ch.Latest.Ts).After(slackTime(ch.LastRead))
		if ch.UnreadDisplay != nil {
			unread = *ch.UnreadDisplay > 0
		}
		if !unread {
			continue
		}
		who := s.name(ctx, tok, ch.Latest.User)
		where := "direct message"
		if c.IsMPIM {
			where = "group message"
		}
		text := s.plain(ctx, tok, ch.Latest.Text)
		link := ""
		if s.team != "" {
			link = s.team + "/archives/" + c.ID
		}
		out = append(out, Item{
			ID: "sl-d-" + c.ID, Kind: "DM", Title: firstLine(text), Where: where, Who: who,
			At: slackTime(ch.Latest.Ts), Hot: true, Body: text, URL: link,
		})
	}
	return out, nil
}

// name is a user's display name, looked up once.
func (s *slackState) name(ctx context.Context, tok, id string) string {
	if id == "" {
		return ""
	}
	s.mu.Lock()
	n, ok := s.names[id]
	s.mu.Unlock()
	if ok {
		return n
	}
	var res struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	n = "someone"
	if err := slackCall(ctx, tok, "users.info", url.Values{"user": {id}}, &res); err == nil {
		switch {
		case res.User.Profile.DisplayName != "":
			n = res.User.Profile.DisplayName
		case res.User.Profile.RealName != "":
			n = res.User.Profile.RealName
		case res.User.Name != "":
			n = res.User.Name
		}
	} else {
		return n // not kept: asked again next time
	}
	s.mu.Lock()
	if s.names == nil {
		s.names = map[string]string{}
	}
	s.names[id] = n
	s.mu.Unlock()
	return n
}

var (
	slackUser = regexp.MustCompile(`<@([A-Z0-9]+)(\|[^>]*)?>`)
	slackLink = regexp.MustCompile(`<([^|>]+)\|([^>]+)>`)
	slackBare = regexp.MustCompile(`<(#[A-Z0-9]+\|)?([^>]+)>`)
)

// plain is a message's mrkdwn as words: <@U1> as @name, <url|label> as the label, the entities undone.
func (s *slackState) plain(ctx context.Context, tok, text string) string {
	text = slackUser.ReplaceAllStringFunc(text, func(m string) string {
		id := slackUser.FindStringSubmatch(m)[1]
		return "@" + s.name(ctx, tok, id)
	})
	text = slackLink.ReplaceAllString(text, "$2")
	text = slackBare.ReplaceAllString(text, "$2")
	return strings.TrimSpace(html.UnescapeString(text))
}

// slackTime is a Slack ts, seconds and microseconds, as a time; the zero time for "" or "0000…".
func slackTime(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(n, us*1000)
}

// firstLine is the start of a text, for a one-line title.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
