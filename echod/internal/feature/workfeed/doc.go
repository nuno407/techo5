// Package workfeed is the Work board's data: what GitHub, Slack and Jira have for the owner right now,
// and the owner's Outlook calendar (outlook.go), fetched by the device itself, kept, and handed to the
// screen (display's render_work.go and render_agenda.go) as one View.
//
// Each source is polled on its own clock, slowly: GitHub every two minutes (its notifications say how
// often they may be asked, X-Poll-Interval, and a 304 for nothing new costs nothing), Slack and Jira
// every three. While nobody has looked at the board for a quarter of an hour they are polled four times
// less often, so a board left alone is not the account's busiest client. What came last is kept: a
// failed poll leaves the items up with a word about the problem, and the board opens on what was there
// rather than on a spinner.
//
// The tokens are the owner's and never in the repository or Home Assistant: they are in work.json in
// the state directory (/data/misc/techo5/work.json), read again whenever it changes, so a token can be
// added over SSH without a restart. A source with no token is an empty state on the board that says
// what to add, not an error: nothing is logged for it. "demo": true fills the sources that have no
// token with made-up items, for a board to look at before the tokens are there.
//
//	{
//	  "demo": false,
//	  "github": {"token": "ghp_…"},
//	  "slack":  {"token": "xoxp-…"},
//	  "jira":   {"site": "acme.atlassian.net", "email": "me@acme.com", "token": "…", "jql": ""},
//	  "outlook": {"client_id": "…", "tenant": "common", "days": 3}
//	}
//
// GitHub's notifications take a classic token with the notifications scope (fine-grained ones are not
// let at them); the review requests and the owner's pull requests come from search, which a
// fine-grained read-only token is enough for. Slack's token is a user token (xoxp-) of an app installed
// in the owner's workspace with search:read (mentions), im:read and mpim:read (direct messages),
// im:history and mpim:history (their last message) and users:read (names). Jira's is an Atlassian API
// token for the email it belongs to; jql replaces the default "assigned to me and not done". Outlook takes
// no token here: the board shows a code to sign in with from a phone, once, and the refresh token is kept
// by the device itself (outlook.go says how to register the app whose client_id goes here).
package workfeed
