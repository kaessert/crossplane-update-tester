package runner

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimitEvent is one distinguishable rate-limit observation: a 429
// response the shared client set received from the backend it is talking
// to, together with the Retry-After it declared (zero when the backend sent
// none, or sent a form this tool does not parse — see parseRetryAfter).
//
// This is kept structurally distinct from an ordinary transport or API
// error: a caller that folded a 429 into generic error handling could not
// tell a rate-limited backend apart from a slow or broken one, and the
// batch mode this type exists for is meant to answer exactly that question
// for cloud-backed providers whose real backend enforces a request ceiling.
type RateLimitEvent struct {
	At         time.Time
	RetryAfter time.Duration
	URL        string
}

// ThrottleTracker observes every HTTP response a shared client set's
// requests receive and counts a CONSECUTIVE run of 429s. A single 429 is
// not the signal this exists to catch: client-go's own transport already
// retries a 429 internally (via its rate-limited backoff) before a caller
// ever sees one surface as an error, so one isolated 429 is ordinary,
// already-handled backend pressure. A SUSTAINED run of them — several in a
// row, with no successful response between — is the signal that the
// current concurrency level is genuinely too high for the backend, and is
// what OnThrottle's callback is invoked against.
//
// A non-429 response resets the consecutive counter to zero. This is
// deliberate: the counter measures "is the backend telling us to slow down
// RIGHT NOW", not a lifetime total, so a brief burst that already recovered
// must not permanently read as sustained pressure on a later, unrelated
// look at the streak.
type ThrottleTracker struct {
	mu          sync.Mutex
	consecutive int
	events      []RateLimitEvent
	onThrottle  func(streak int)
}

// newThrottleTracker returns a ThrottleTracker with no callback registered.
// Recording still happens (Events() reflects every 429 seen) even with no
// callback wired — SetOnThrottle is how a caller opts into being notified
// synchronously, not a precondition for tracking at all.
func newThrottleTracker() *ThrottleTracker {
	return &ThrottleTracker{}
}

// SetOnThrottle registers fn to be called, synchronously and while holding
// no lock a caller could deadlock against, every time Observe records a
// 429 — not only when a threshold is crossed. The decision of WHICH streak
// length counts as "sustained" belongs to the caller (see RunBatch's
// sustainedThrottleStreak), not to this type: a tracker has no opinion on
// how many workers are safe, only on what the backend just said.
func (t *ThrottleTracker) SetOnThrottle(fn func(streak int)) {
	t.mu.Lock()
	t.onThrottle = fn
	t.mu.Unlock()
}

// Observe records one HTTP response's outcome. statusCode is the raw HTTP
// status; retryAfter is the already-parsed Retry-After header (zero when
// absent or unparseable — see parseRetryAfter). Returns the CURRENT
// consecutive-429 streak after recording this response, so a caller that
// does not use SetOnThrottle can still poll the streak without a second,
// separately-timed read racing this one.
func (t *ThrottleTracker) Observe(statusCode int, retryAfter time.Duration, url string) (streak int) {
	t.mu.Lock()
	var fn func(int)
	if statusCode == http.StatusTooManyRequests {
		t.consecutive++
		t.events = append(t.events, RateLimitEvent{At: time.Now(), RetryAfter: retryAfter, URL: url})
		streak = t.consecutive
		fn = t.onThrottle
	} else {
		t.consecutive = 0
		streak = 0
	}
	t.mu.Unlock()

	if fn != nil {
		fn(streak)
	}
	return streak
}

// Events returns every 429 this tracker has recorded, in observation order.
// The returned slice is a copy — callers may not mutate this tracker's
// internal state through it.
func (t *ThrottleTracker) Events() []RateLimitEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RateLimitEvent, len(t.events))
	copy(out, t.events)
	return out
}

// throttleRoundTripper wraps an http.RoundTripper, feeding every response's
// status code and Retry-After header into a ThrottleTracker before
// returning the response UNCHANGED. It never short-circuits, retries, or
// alters what the wrapped RoundTripper produced — it only observes — so
// layering this onto a *rest.Config's transport changes nothing about any
// existing request's outcome; the only new effect is that the tracker now
// knows about it.
type throttleRoundTripper struct {
	next    http.RoundTripper
	tracker *ThrottleTracker
}

// RoundTrip implements http.RoundTripper.
func (rt *throttleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	url := ""
	if req.URL != nil {
		url = req.URL.String()
	}
	rt.tracker.Observe(resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), url)
	return resp, err
}

// parseRetryAfter parses an RFC 7231 Retry-After header's delta-seconds
// form — the only form a Kubernetes API server (APF throttling) or a
// typical rate-limited HTTP backend sends. The HTTP-date form is not
// supported: no backend this tool talks to has ever been observed to send
// it, and guessing at a date parse is worse than the honest zero this
// returns for anything it cannot read. An absent or unparseable header
// returns zero, never an error — the tracker still counts the 429 itself;
// only the backoff hint is best-effort.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
