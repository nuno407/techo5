package workfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub: the unread notifications, the pull requests waiting on the owner's review, and the owner's own
// open pull requests. Three requests a poll, two of them search (30 a minute allowed); the notifications
// are asked with If-Modified-Since, and a 304 is neither counted against the limit nor a change.

// githubAPI is where GitHub's API is when the settings do not say; a test points it at its own server.
var githubAPI = "https://api.github.com"

// githubState is what is kept between polls: the notifications as last fetched, and when, for the 304.
type githubState struct {
	modified string // Last-Modified of the last notifications that came
	notes    []Item
	noteErr  string // the notifications this token cannot reach, said on the board
}

// hotReasons are the notification reasons that wait on the owner.
var hotReasons = map[string]string{
	"review_requested":   "Review",
	"mention":            "Mention",
	"team_mention":       "Mention",
	"assign":             "Assigned",
	"security_alert":     "Security",
	"approval_requested": "Approval",
}

func (g *githubState) fetch(ctx context.Context, s GitHubSettings, every time.Duration) (Feed, time.Duration, error) {
	api := strings.TrimRight(s.API, "/")
	if api == "" {
		api = githubAPI
	}
	get := func(path string, modified string) (answer, error) {
		req, err := http.NewRequest("GET", api+path, nil)
		if err != nil {
			return answer{}, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Token))
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if modified != "" {
			req.Header.Set("If-Modified-Since", modified)
		}
		return do(ctx, req)
	}

	// The notifications. A fine-grained token is refused them (403), which leaves the rest of the
	// column working and says why the notifications are missing.
	a, err := get("/notifications?per_page=30", g.modified)
	if err != nil {
		return Feed{}, every, fmt.Errorf("notifications: %w", err)
	}
	switch a.status {
	case http.StatusNotModified:
	case http.StatusOK:
		notes, err := parseNotifications(a.body)
		if err != nil {
			return Feed{}, every, fmt.Errorf("notifications: %w", err)
		}
		g.notes, g.modified, g.noteErr = notes, a.header.Get("Last-Modified"), ""
	case http.StatusUnauthorized:
		return Feed{}, every, fmt.Errorf("the token was refused")
	case http.StatusForbidden, http.StatusNotFound:
		g.notes, g.modified, g.noteErr = nil, "", "Notifications need a classic token with the notifications scope"
	default:
		return Feed{}, every, statusError("notifications", a)
	}
	if p, err := strconv.Atoi(a.header.Get("X-Poll-Interval")); err == nil && time.Duration(p)*time.Second > every {
		every = time.Duration(p) * time.Second
	}

	reviews, err := g.search(get, "is:open is:pr archived:false review-requested:@me", "Review", true)
	if err != nil {
		return Feed{}, every, err
	}
	mine, err := g.search(get, "is:open is:pr archived:false author:@me", "PR", false)
	if err != nil {
		return Feed{}, every, err
	}

	fd := Feed{Problem: g.noteErr}
	unread, mentions := 0, 0
	seen := map[string]bool{}
	for _, it := range reviews {
		seen[it.URL] = true
		fd.Items = append(fd.Items, it)
	}
	for _, it := range g.notes {
		unread++
		if it.Kind == "Mention" {
			mentions++
		}
		if seen[it.URL] {
			continue // a review request is in its notification too: once is enough
		}
		seen[it.URL] = true
		fd.Items = append(fd.Items, it)
	}
	for _, it := range mine {
		if !seen[it.URL] {
			fd.Items = append(fd.Items, it)
		}
	}
	fd.Counts = []Count{
		{Label: "Reviews", N: len(reviews), Hot: true},
		{Label: "Mentions", N: mentions, Hot: true},
		{Label: "Unread", N: unread},
		{Label: "My PRs", N: len(mine)},
	}
	return fd, every, nil
}

// search is one issue search as items.
func (g *githubState) search(get func(string, string) (answer, error), q, kind string, hot bool) ([]Item, error) {
	a, err := get("/search/issues?per_page=15&sort=updated&q="+url.QueryEscape(q), "")
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("the token was refused")
	default:
		return nil, statusError("search", a)
	}
	return parseSearch(a.body, kind, hot)
}

func parseNotifications(b []byte) ([]Item, error) {
	var notes []struct {
		ID        string    `json:"id"`
		Unread    bool      `json:"unread"`
		Reason    string    `json:"reason"`
		UpdatedAt time.Time `json:"updated_at"`
		Subject   struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Type  string `json:"type"`
		} `json:"subject"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(b, &notes); err != nil {
		return nil, err
	}
	var out []Item
	for _, n := range notes {
		if !n.Unread {
			continue
		}
		kind, hot := hotReasons[n.Reason]
		if !hot {
			kind = subjectKind(n.Subject.Type)
		}
		link := webURL(n.Subject.URL)
		if link == "" {
			link = n.Repository.HTMLURL
		}
		out = append(out, Item{
			ID: "gh-n-" + n.ID, Kind: kind, Title: n.Subject.Title, Where: n.Repository.FullName,
			At: n.UpdatedAt, Hot: hot, URL: link,
			Body: fmt.Sprintf("%s in %s, because of %s.", subjectKind(n.Subject.Type), n.Repository.FullName,
				strings.ReplaceAll(n.Reason, "_", " ")),
		})
	}
	return out, nil
}

func parseSearch(b []byte, kind string, hot bool) ([]Item, error) {
	var res struct {
		Items []struct {
			ID        int64                  `json:"id"`
			Number    int                    `json:"number"`
			Title     string                 `json:"title"`
			HTMLURL   string                 `json:"html_url"`
			RepoURL   string                 `json:"repository_url"`
			UpdatedAt time.Time              `json:"updated_at"`
			Draft     bool                   `json:"draft"`
			Comments  int                    `json:"comments"`
			User      struct{ Login string } `json:"user"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range res.Items {
		repo := it.RepoURL
		if i := strings.Index(repo, "/repos/"); i >= 0 {
			repo = repo[i+len("/repos/"):]
		}
		body := fmt.Sprintf("#%d in %s by %s, %d comments.", it.Number, repo, it.User.Login, it.Comments)
		if it.Draft {
			body += " Draft."
		}
		out = append(out, Item{
			ID: "gh-i-" + strconv.FormatInt(it.ID, 10), Kind: kind, Title: it.Title,
			Where: fmt.Sprintf("%s #%d", repo, it.Number), Who: it.User.Login, At: it.UpdatedAt, Hot: hot,
			URL: it.HTMLURL, Body: body,
		})
	}
	return out, nil
}

func subjectKind(t string) string {
	switch t {
	case "PullRequest":
		return "PR"
	case "CheckSuite", "WorkflowRun":
		return "CI"
	case "":
		return "Note"
	}
	return t
}

// webURL turns an API address for an issue or a pull request into the page for it:
// api.github.com/repos/o/r/pulls/12 is github.com/o/r/pull/12.
func webURL(api string) string {
	u, err := url.Parse(api)
	if err != nil || api == "" {
		return ""
	}
	i := strings.Index(u.Path, "/repos/")
	if i < 0 {
		return ""
	}
	path := strings.Replace(u.Path[i+len("/repos/"):], "/pulls/", "/pull/", 1)
	// GitHub Enterprise answers under its own host (…/api/v3/repos/…), which is its pages' host too.
	host := u.Host
	if host == "api.github.com" {
		host = "github.com"
	}
	return "https://" + host + "/" + path
}
