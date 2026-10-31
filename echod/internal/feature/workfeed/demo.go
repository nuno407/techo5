package workfeed

import (
	"time"
)

// demoFeed is a source's made-up column, for a board to look at before its token is there
// (Settings.Demo). The times are kept relative to now, so the ages on the board read as they would.
func demoFeed(src Source, now time.Time) Feed {
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	fd := Feed{Source: src, Name: nameOf(src), State: Ready, Demo: true, Fetched: now}
	switch src {
	case GitHub:
		fd.Items = []Item{
			{ID: "d-gh-1", Kind: "Review", Title: "Draw the rain map from cached tiles", Where: "acme/panel #412", Who: "rita",
				At: ago(12), Hot: true, Body: "#412 in acme/panel by rita, 3 comments.", URL: "https://github.com/acme/panel/pull/412"},
			{ID: "d-gh-2", Kind: "Mention", Title: "Backlight curve on the RT", Where: "acme/firmware", Who: "joao",
				At: ago(48), Hot: true, Body: "Issue in acme/firmware, because of mention.", URL: "https://github.com/acme/firmware/issues/88"},
			{ID: "d-gh-3", Kind: "Review", Title: "Retry the Wi-Fi join once", Where: "acme/netd #97", Who: "ana",
				At: ago(130), Hot: true, Body: "#97 in acme/netd by ana, 0 comments.", URL: "https://github.com/acme/netd/pull/97"},
			{ID: "d-gh-4", Kind: "CI", Title: "build: arm7 failed on main", Where: "acme/panel", At: ago(65),
				Body: "CI in acme/panel, because of ci activity.", URL: "https://github.com/acme/panel/actions"},
			{ID: "d-gh-5", Kind: "PR", Title: "Sky board: aircraft trails", Where: "acme/panel #415", Who: "me",
				At: ago(200), Body: "#415 in acme/panel by me, 1 comments.", URL: "https://github.com/acme/panel/pull/415"},
			{ID: "d-gh-6", Kind: "PR", Title: "Work board: Jira column", Where: "acme/panel #409", Who: "me",
				At: ago(1500), Body: "#409 in acme/panel by me, 6 comments. Draft.", URL: "https://github.com/acme/panel/pull/409"},
		}
		fd.Counts = []Count{{"Reviews", 2, true}, {"Mentions", 1, true}, {"Unread", 4, false}, {"My PRs", 2, false}}
	case Slack:
		fd.Items = []Item{
			{ID: "d-sl-1", Kind: "DM", Title: "can you look at the deploy before standup?", Where: "direct message", Who: "Marta",
				At: ago(4), Hot: true, Body: "can you look at the deploy before standup? the canary is at 5% and holding"},
			{ID: "d-sl-2", Kind: "Mention", Title: "@me the RT build is green again", Where: "#hardware", Who: "Pedro",
				At: ago(35), Hot: true, Body: "@me the RT build is green again, kernel 20 boots in the trial slot"},
			{ID: "d-sl-3", Kind: "DM", Title: "lunch thursday?", Where: "group message", Who: "Inês",
				At: ago(95), Hot: true, Body: "lunch thursday? the new place by the river"},
			{ID: "d-sl-4", Kind: "Mention", Title: "@me thanks for the review", Where: "#panel", Who: "Rui",
				At: ago(1600), Body: "@me thanks for the review, merged"},
		}
		fd.Counts = []Count{{"DMs", 2, true}, {"Mentions", 1, true}}
	case Jira:
		fd.Items = []Item{
			{ID: "d-j-1", Kind: "Assigned", Title: "Touch ghosts on a noisy supply", Where: "PNL-231 · In Progress", Who: "Carla",
				At: ago(22), Hot: true, Body: "Bug · In Progress · Panel · High priority", URL: "https://acme.atlassian.net/browse/PNL-231"},
			{ID: "d-j-2", Kind: "Assigned", Title: "Sky board: flight labels overlap", Where: "PNL-240 · To Do", Who: "Carla",
				At: ago(300), Body: "Task · To Do · Panel · Medium priority", URL: "https://acme.atlassian.net/browse/PNL-240"},
			{ID: "d-j-3", Kind: "Assigned", Title: "Document the work.json tokens", Where: "PNL-238 · In Review", Who: "Nuno",
				At: ago(700), Body: "Task · In Review · Panel · Low priority", URL: "https://acme.atlassian.net/browse/PNL-238"},
			{ID: "d-j-4", Kind: "Watching", Title: "Release 0.9 checklist", Where: "REL-12 · In Progress", Who: "Tiago",
				At: ago(160), Body: "Epic · In Progress · Releases", URL: "https://acme.atlassian.net/browse/REL-12"},
		}
		fd.Counts = []Count{{"Needs you", 1, true}, {"Assigned", 3, false}, {"Changed today", 2, false}, {"Watching", 1, false}}
	}
	sortItems(fd.Items)
	return fd
}

// DemoView is the whole board made up, every source as demo fills it: for the screen's own tests and
// previews.
func DemoView(now time.Time) View {
	v := View{Settings: SettingsPath()}
	for _, src := range Sources {
		fd := demoFeed(src, now)
		for _, it := range fd.Items {
			if it.Hot {
				v.Hot++
			}
		}
		v.Feeds = append(v.Feeds, fd)
	}
	return v
}
