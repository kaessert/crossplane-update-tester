package runner

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestThrottleRoundTripperCountsConsecutive429s drives an ACTUAL rate-limited
// HTTP backend — a real httptest.Server, not a canned response object — and
// proves throttleRoundTripper both observes every 429 it returns and resets
// the streak the moment a request succeeds. This is the review's "the 429
// path is exercised" requirement at the lowest level: the exact production
// RoundTripper, wrapping the exact production http.Client shape, talking to
// a real server over a real socket.
func TestThrottleRoundTripperCountsConsecutive429s(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 3 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tracker := newThrottleTracker()
	var mu sync.Mutex
	var streaks []int
	tracker.SetOnThrottle(func(streak int) {
		mu.Lock()
		streaks = append(streaks, streak)
		mu.Unlock()
	})

	client := &http.Client{Transport: &throttleRoundTripper{next: http.DefaultTransport, tracker: tracker}}

	for i := 0; i < 5; i++ {
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []int{1, 2, 3}; !equalInts(streaks, want) {
		t.Errorf("OnThrottle streaks = %v, want %v (fires once per 429, not once overall, and stops once the backend recovers)", streaks, want)
	}

	events := tracker.Events()
	if len(events) != 3 {
		t.Fatalf("recorded %d rate-limit events, want 3 (one per 429 the fake backend sent)", len(events))
	}
	for i, e := range events {
		if e.RetryAfter != 2*time.Second {
			t.Errorf("events[%d].RetryAfter = %v, want 2s", i, e.RetryAfter)
		}
		if e.URL == "" {
			t.Errorf("events[%d].URL is empty, want the request URL", i)
		}
	}
}

// TestThrottleRoundTripperNonThrottlingResponseResetsStreak proves the
// streak is CONSECUTIVE, not cumulative: a success between two 429s must
// not let the second one appear to be a continuation of the first, or a
// backend that is merely occasionally slow would misreport as sustained
// rate-limiting.
func TestThrottleRoundTripperNonThrottlingResponseResetsStreak(t *testing.T) {
	sequence := []int{http.StatusTooManyRequests, http.StatusOK, http.StatusTooManyRequests}
	var idx int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := atomic.AddInt32(&idx, 1) - 1
		w.WriteHeader(sequence[i])
	}))
	defer server.Close()

	tracker := newThrottleTracker()
	var mu sync.Mutex
	var streaks []int
	tracker.SetOnThrottle(func(streak int) {
		mu.Lock()
		streaks = append(streaks, streak)
		mu.Unlock()
	})
	client := &http.Client{Transport: &throttleRoundTripper{next: http.DefaultTransport, tracker: tracker}}

	for range sequence {
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []int{1, 1}; !equalInts(streaks, want) {
		t.Errorf("streaks = %v, want %v — the intervening 200 must reset the streak back to 0 before the second 429", streaks, want)
	}
}

// TestParseRetryAfter pins the delta-seconds parse and its fallbacks.
func TestParseRetryAfter(t *testing.T) {
	cases := map[string]struct {
		in   string
		want time.Duration
	}{
		"empty":         {"", 0},
		"valid seconds": {"5", 5 * time.Second},
		"zero":          {"0", 0},
		"negative":      {"-1", 0},
		"non-numeric":   {"Wed, 21 Oct 2015 07:28:00 GMT", 0},
		"garbage":       {"soon", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestThrottleTrackerObserveWithNoCallback proves recording works even when
// no OnThrottle callback was ever registered — the callback is opt-in, not
// a precondition for Events() to reflect reality.
func TestThrottleTrackerObserveWithNoCallback(t *testing.T) {
	tracker := newThrottleTracker()
	streak := tracker.Observe(http.StatusTooManyRequests, time.Second, "http://example/x")
	if streak != 1 {
		t.Fatalf("streak = %d, want 1", streak)
	}
	if len(tracker.Events()) != 1 {
		t.Fatalf("Events() = %d, want 1", len(tracker.Events()))
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
