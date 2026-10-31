package workfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Jira Cloud: the issues assigned to the owner and not done (or the settings' own JQL), and those the
// owner watches that someone else changed in the last two days. Two searches a poll, through
// /rest/api/3/search/jql, the search Atlassian kept when it retired /search. Jira's JQL has nothing for
// "mentions me": a mention reaches the owner by email and as a change to a watched issue, which the
// second search shows. An issue waits on the owner when it is theirs and of high priority, or theirs and
// changed in the last day.

const (
	jiraMine     = "assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC"
	jiraWatching = "watcher = currentUser() AND (assignee != currentUser() OR assignee is EMPTY) AND updated >= -2d ORDER BY updated DESC"
	jiraFields   = "summary,status,priority,updated,issuetype,project,reporter,assignee"
)

// hotPriorities are the priorities that make an assigned issue wait on the owner.
var hotPriorities = map[string]bool{"Highest": true, "High": true, "Blocker": true, "Critical": true}

func fetchJira(ctx context.Context, s JiraSettings) (Feed, error) {
	mineQ := s.JQL
	if strings.TrimSpace(mineQ) == "" {
		mineQ = jiraMine
	}
	mine, err := jiraSearch(ctx, s, mineQ, true)
	if err != nil {
		return Feed{}, err
	}
	watched, err := jiraSearch(ctx, s, jiraWatching, false)
	if err != nil {
		return Feed{}, err
	}
	fd := Feed{Items: append(mine, watched...)}
	hot, today := 0, 0
	for _, it := range mine {
		if it.Hot {
			hot++
		}
		if time.Since(it.At) < 24*time.Hour {
			today++
		}
	}
	fd.Counts = []Count{
		{Label: "Needs you", N: hot, Hot: true},
		{Label: "Assigned", N: len(mine)},
		{Label: "Changed today", N: today},
		{Label: "Watching", N: len(watched)},
	}
	return fd, nil
}

func jiraSearch(ctx context.Context, s JiraSettings, jql string, mine bool) ([]Item, error) {
	q := url.Values{"jql": {jql}, "maxResults": {"15"}, "fields": {jiraFields}}
	req, err := http.NewRequest("GET", s.base()+"/rest/api/3/search/jql?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(strings.TrimSpace(s.Email), strings.TrimSpace(s.Token))
	req.Header.Set("Accept", "application/json")
	a, err := do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	switch a.status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("the email and token were refused")
	case http.StatusBadRequest:
		return nil, fmt.Errorf("the JQL was refused: %s", jiraMessage(a.body))
	default:
		return nil, statusError("search", a)
	}
	return parseJira(a.body, s.base(), mine)
}

// jiraTime is Jira's time, 2026-10-30T18:04:11.123+0000, whose zone has no colon.
type jiraTime time.Time

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = jiraTime(v)
			return nil
		}
	}
	return nil // a time it cannot read is no time, not a failed poll
}

func parseJira(b []byte, base string, mine bool) ([]Item, error) {
	var res struct {
		Issues []struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Fields struct {
				Summary string   `json:"summary"`
				Updated jiraTime `json:"updated"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
				Priority *struct {
					Name string `json:"name"`
				} `json:"priority"`
				IssueType struct {
					Name string `json:"name"`
				} `json:"issuetype"`
				Project struct {
					Key  string `json:"key"`
					Name string `json:"name"`
				} `json:"project"`
				Reporter *struct {
					DisplayName string `json:"displayName"`
				} `json:"reporter"`
				Assignee *struct {
					DisplayName string `json:"displayName"`
				} `json:"assignee"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	var out []Item
	for _, is := range res.Issues {
		f := is.Fields
		prio := ""
		if f.Priority != nil {
			prio = f.Priority.Name
		}
		who := ""
		switch {
		case !mine && f.Assignee != nil:
			who = f.Assignee.DisplayName
		case f.Reporter != nil:
			who = f.Reporter.DisplayName
		}
		at := time.Time(f.Updated)
		kind := "Assigned"
		if !mine {
			kind = "Watching"
		}
		body := fmt.Sprintf("%s · %s · %s", f.IssueType.Name, f.Status.Name, f.Project.Name)
		if prio != "" {
			body += " · " + prio + " priority"
		}
		out = append(out, Item{
			ID: "jira-" + is.ID, Kind: kind, Title: f.Summary, Where: is.Key + " · " + f.Status.Name, Who: who,
			At: at, Hot: mine && (hotPriorities[prio] || time.Since(at) < 24*time.Hour), Body: body,
			URL: base + "/browse/" + is.Key,
		})
	}
	return out, nil
}

// jiraMessage is the first of the errors Jira gave, for a refused JQL.
func jiraMessage(b []byte) string {
	var e struct {
		ErrorMessages []string `json:"errorMessages"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.ErrorMessages) > 0 {
		return e.ErrorMessages[0]
	}
	return "Bad Request"
}
