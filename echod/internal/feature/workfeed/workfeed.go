package workfeed

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/component"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hook"
)

func init() {
	component.Register(component.Network, Get(), component.Order(90))
}

// Source is where items come from.
type Source string

const (
	GitHub Source = "github"
	Slack  Source = "slack"
	Jira   Source = "jira"
	// Outlook is the calendar: not a column but the agenda strip over them (View.Calendar).
	Outlook Source = "outlook"
)

// Sources are the board's columns, in order.
var Sources = []Source{GitHub, Slack, Jira}

// polled are the sources polled: the columns and the calendar.
var polled = []Source{GitHub, Slack, Jira, Outlook}

// State is how a source is.
type State int

const (
	Off       State = iota // no token: the board says what to add
	Loading                // set, never fetched yet
	Ready                  // fetched; Problem may say the last poll failed, the items being the ones before
	Failed                 // set, and every poll so far has failed
	SigningIn              // the calendar waiting for the owner to sign in (Feed.SignIn)
)

// Feed is one source as the board shows it.
type Feed struct {
	Source  Source
	Name    string
	State   State
	Problem string // why the last poll failed, or a part of the source this token cannot reach
	Setup   string // for Off: what to add, and where
	Demo    bool   // made-up items (Settings.Demo)
	Counts  []Count
	Items   []Item // newest first, those needing attention ahead of the rest
	Fetched time.Time

	// The calendar's: its meetings in order, and while it waits for a sign-in, what to enter where.
	Events []Event
	SignIn *SignIn
}

// Count is one number at the head of a column: Reviews 2, Mentions 1.
type Count struct {
	Label string
	N     int
	Hot   bool // asks for the owner: drawn in the accent when N > 0
}

// Item is one line of a column, and what its detail card shows.
type Item struct {
	ID    string // stable across polls, so a card left open stays on its item
	Kind  string // Review, Mention, PR, DM, Issue…
	Title string
	Where string // repository, channel, project
	Who   string
	At    time.Time
	Hot   bool   // waiting on the owner
	Body  string // the detail card's text: a message, a status
	URL   string
}

// View is the whole board.
type View struct {
	Feeds    []Feed
	Calendar Feed   // Outlook's meetings, over the columns
	Hot      int    // items needing attention, across sources
	Problem  string // work.json could not be read
	Settings string // where the tokens go
}

// Polling, per source; four times slower while nobody is looking (lookedFor).
const (
	githubEvery = 2 * time.Minute
	slackEvery  = 3 * time.Minute
	jiraEvery   = 5 * time.Minute
	idleFactor  = 4
	lookedFor   = 15 * time.Minute
	// pollTimeout bounds one source's poll, Slack's walk through its direct messages included.
	pollTimeout = 90 * time.Second
	// maxItems is how many items a source keeps.
	maxItems = 20
)

// Feature is the Work board's data.
type Feature struct {
	// Changed fires when there is something new to draw. Listeners must not block.
	Changed hook.Hook[struct{}]

	mu     sync.Mutex
	feeds  map[Source]*Feed
	next   map[Source]time.Time // when each source is next due
	looked time.Time            // the board last shown (Looked)
	poke   chan struct{}

	gh githubState
	sl slackState
	ol outlookState
}

var (
	once   sync.Once
	shared *Feature
)

// Get is the Work board's data.
func Get() *Feature {
	once.Do(func() {
		shared = &Feature{feeds: map[Source]*Feed{}, next: map[Source]time.Time{}, poke: make(chan struct{}, 1)}
	})
	return shared
}

func (f *Feature) Name() string { return "workfeed" }

// Looked is the board being on the screen: the sources go back to their quick pace, and one not
// fetched for longer than that is fetched now.
func (f *Feature) Looked() {
	f.mu.Lock()
	was := f.looked
	f.looked = time.Now()
	f.mu.Unlock()
	if time.Since(was) > time.Minute {
		select {
		case f.poke <- struct{}{}:
		default:
		}
	}
}

// Run polls whichever sources are set, each when it is due.
func (f *Feature) Run(ctx context.Context) error {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		f.round(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-f.poke:
		}
	}
}

// round polls the sources that are due, side by side, and waits for them.
func (f *Feature) round(ctx context.Context) {
	s, _ := current()
	now := time.Now()
	f.mu.Lock()
	idle := now.Sub(f.looked) > lookedFor
	f.mu.Unlock()
	var wg sync.WaitGroup
	for _, src := range polled {
		if !configured(s, src) {
			f.forget(src)
			continue
		}
		f.mu.Lock()
		due := !now.Before(f.next[src])
		if due {
			// Not again until this one is done and its interval has gone by; set now so a round meanwhile
			// does not start it twice.
			f.next[src] = now.Add(time.Hour)
		}
		f.mu.Unlock()
		if !due {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.poll(ctx, s, src, idle, !idle)
		}()
	}
	wg.Wait()
}

func configured(s Settings, src Source) bool {
	switch src {
	case GitHub:
		return s.GitHub.set()
	case Slack:
		return s.Slack.set()
	case Jira:
		return s.Jira.set()
	case Outlook:
		return s.Outlook.set()
	}
	return false
}

// forget drops what a source had once its token is taken away.
func (f *Feature) forget(src Source) {
	f.mu.Lock()
	_, had := f.feeds[src]
	delete(f.feeds, src)
	delete(f.next, src)
	f.mu.Unlock()
	if had {
		f.Changed.Emit(struct{}{})
	}
}

// errLater is a source saying it was asked too often: the poll is tried again after the wait, and the
// board keeps what it had without calling it a problem.
type errLater struct{ wait time.Duration }

func (e errLater) Error() string {
	return "asked too often; again in " + e.wait.Round(time.Second).String()
}

// poll fetches one source and keeps what came.
func (f *Feature) poll(ctx context.Context, s Settings, src Source, idle, looked bool) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	every := map[Source]time.Duration{GitHub: githubEvery, Slack: slackEvery, Jira: jiraEvery, Outlook: outlookEvery}[src]
	var got Feed
	var err error
	switch src {
	case GitHub:
		got, every, err = f.gh.fetch(ctx, s.GitHub, every)
	case Slack:
		got, err = f.sl.fetch(ctx, s.Slack)
	case Jira:
		got, err = fetchJira(ctx, s.Jira)
	case Outlook:
		got, every, err = f.ol.fetch(ctx, s.Outlook, looked)
	}
	if idle && got.State != SigningIn {
		every *= idleFactor
	}
	var later errLater
	if errors.As(err, &later) {
		every = max(every, later.wait)
	}
	f.mu.Lock()
	f.next[src] = time.Now().Add(every)
	old := f.feeds[src]
	switch {
	case err == nil:
		got.Source, got.Name, got.Fetched = src, nameOf(src), time.Now()
		if got.State != SigningIn {
			got.State = Ready
		}
		sortItems(got.Items)
		if len(got.Items) > maxItems {
			got.Items = got.Items[:maxItems]
		}
		if old != nil && old.Problem != "" && old.Problem != got.Problem {
			slog.Info("workfeed: back", "source", src)
		}
		f.feeds[src] = &got
	case old != nil && old.State == Ready:
		// Kept as it was, with a word about the last poll. Said in the log once, when it starts.
		if old.Problem != err.Error() && !errors.As(err, &later) {
			slog.Info("workfeed: poll failed, keeping the last items", "source", src, "err", err)
			old.Problem = err.Error()
		}
	default:
		if old == nil || old.Problem != err.Error() {
			slog.Info("workfeed: poll failed", "source", src, "err", err)
		}
		f.feeds[src] = &Feed{Source: src, Name: nameOf(src), State: Failed, Problem: err.Error()}
	}
	f.mu.Unlock()
	f.Changed.Emit(struct{}{})
}

// sortItems puts what waits on the owner first, then the newest.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Hot != items[j].Hot {
			return items[i].Hot
		}
		return items[i].At.After(items[j].At)
	})
}

func nameOf(src Source) string {
	switch src {
	case GitHub:
		return "GitHub"
	case Slack:
		return "Slack"
	case Jira:
		return "Jira"
	case Outlook:
		return "Outlook"
	}
	return string(src)
}

// setupLine is what an Off source says to do.
func setupLine(src Source) string {
	switch src {
	case GitHub:
		return `Add "github": {"token": "…"} to ` + SettingsPath()
	case Slack:
		return `Add "slack": {"token": "xoxp-…"} to ` + SettingsPath()
	case Jira:
		return `Add "jira": {"site", "email", "token"} to ` + SettingsPath()
	case Outlook:
		return `Add "outlook": {"client_id": "…"} to ` + SettingsPath()
	}
	return ""
}

// View is the board as it stands: each source fetched, loading, or not set up (made up, with demo on).
func (f *Feature) View() View {
	s, problem := current()
	v := View{Problem: problem, Settings: SettingsPath()}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, src := range Sources {
		var fd Feed
		switch {
		case !configured(s, src) && s.Demo:
			fd = demoFeed(src, now)
		case !configured(s, src):
			fd = Feed{Source: src, Name: nameOf(src), State: Off, Setup: setupLine(src)}
		case f.feeds[src] != nil:
			fd = *f.feeds[src]
		default:
			fd = Feed{Source: src, Name: nameOf(src), State: Loading}
		}
		for _, it := range fd.Items {
			if it.Hot {
				v.Hot++
			}
		}
		v.Feeds = append(v.Feeds, fd)
	}
	switch {
	case !configured(s, Outlook) && s.Demo:
		v.Calendar = demoCalendar(now)
	case !configured(s, Outlook):
		v.Calendar = Feed{Source: Outlook, Name: nameOf(Outlook), State: Off, Setup: setupLine(Outlook)}
	case f.feeds[Outlook] != nil:
		v.Calendar = *f.feeds[Outlook]
	default:
		v.Calendar = Feed{Source: Outlook, Name: nameOf(Outlook), State: Loading}
	}
	return v
}
