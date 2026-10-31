package workfeed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const userAgent = "TECHO5 (https://github.com/HuskerMinion/techo5)"

var client = &http.Client{Timeout: 20 * time.Second}

// answer is a response read whole: up to 2 MB, which no page of anything asked for here comes near.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// do sends req and reads the answer. A 429, or a 403 that says the rate limit is spent, is errLater with
// the wait the server gave; any other status is the caller's to judge.
func do(ctx context.Context, req *http.Request) (answer, error) {
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return answer{}, err
	}
	a := answer{status: resp.StatusCode, header: resp.Header, body: b}
	if a.status == http.StatusTooManyRequests || (a.status == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		return a, errLater{wait: retryAfter(resp.Header)}
	}
	return a, nil
}

// retryAfter is how long a server asked to be left alone: Retry-After in seconds, or GitHub's reset
// time, and a minute when it says neither.
func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if d := time.Until(time.Unix(t, 0)); d > 0 {
			return d + 5*time.Second
		}
	}
	return time.Minute
}

// statusError is a status nothing else made sense of, in a few words.
func statusError(what string, a answer) error {
	return fmt.Errorf("%s: %s", what, http.StatusText(a.status))
}
