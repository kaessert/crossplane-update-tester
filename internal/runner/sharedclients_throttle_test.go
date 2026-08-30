package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// TestSharedClientsThrottleTrackerObservesRealBackendThrottling is the
// composition test the review's rejected AC ("PROVE THE 429 PATH IS
// EXERCISED") demanded: a real rate-limited httptest.Server, a client set
// built by newSharedClients — the PRODUCTION constructor, not a hand-built
// throttleRoundTripper and not tracker.Observe() called directly — issuing
// one real client-go List() call through it, with the AdaptiveLimiter wired
// exactly as RunBatch wires it.
//
// This is the seam MUTATION E deletes: the `cfg.WrapTransport = ...` block
// in newSharedClients is the only line that connects throttleRoundTripper —
// and therefore the ThrottleTracker, and therefore the AdaptiveLimiter — to
// any client this constructor builds. Delete it and every existing test in
// this package still passes, because none of them go through
// newSharedClients with a live backend: throttle_test.go hand-constructs
// the RoundTripper, sharedclients_test.go's builders return HTTP-free
// fakes, and the batch throttle policy test calls Observe() directly. This
// test is the one that goes red.
func TestSharedClientsThrottleTrackerObservesRealBackendThrottling(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/events") {
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := atomic.AddInt32(&calls, 1)
		if n <= 3 {
			// Retry-After: 0 — never a real delay. client-go's own
			// request-level retry would otherwise sleep the declared
			// duration before each of these three retries, turning this
			// into a real-time wait for no additional signal.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"EventList","items":[]}`))
	}))
	defer server.Close()

	// The production builders, EXCEPT restConfig — which every real caller
	// resolves from the ambient kubeconfig via kubeRESTConfig. Substituting
	// only that one step (Host pointed at the fake backend, no live
	// kubeconfig needed) and leaving clientset/dynamic/discovery as the
	// real k8s.io/client-go constructors is what makes this a test of
	// PRODUCTION composition rather than a fake standing in for it.
	builders := defaultSharedClientsBuilders
	builders.restConfig = func() (*rest.Config, error) {
		return &rest.Config{Host: server.URL}, nil
	}

	sc, err := newSharedClients(builders)
	if err != nil {
		t.Fatalf("newSharedClients: %v", err)
	}

	// Wire the AdaptiveLimiter exactly as RunBatch does (see batch.go)
	// rather than reimplementing the wiring here — a divergence between
	// the two would test something RunBatch does not actually do.
	limiter := NewAdaptiveLimiter(8)
	sc.Throttle().SetOnThrottle(adaptiveThrottleHandler(limiter))

	if _, err := sc.clientset.CoreV1().Events("default").List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Fatalf("Events().List: %v", err)
	}

	// AC 1: the ThrottleTracker recorded the 429s — through the real
	// throttleRoundTripper, wired by the real newSharedClients, not a
	// hand-built RoundTripper and not a direct Observe() call.
	events := sc.Throttle().Events()
	if len(events) != 3 {
		t.Fatalf("ThrottleTracker recorded %d event(s), want 3 (one per 429 the fake backend sent before it recovered)", len(events))
	}

	// AC 2: the reduction actually happened — not merely reachable code,
	// an observed ceiling change.
	if got, want := limiter.CurrentLimit(), 4; got != want {
		t.Fatalf("AdaptiveLimiter.CurrentLimit() = %d, want %d (8 halved once on the 3-streak)", got, want)
	}

	// AC 3: the reduction is RECORDED, with From/To/Reason — a silent
	// reduction is exactly the failure mode this test exists against.
	limiterEvents := limiter.Events()
	if len(limiterEvents) != 1 {
		t.Fatalf("AdaptiveLimiter recorded %d reduction event(s), want 1", len(limiterEvents))
	}
	if got, want := limiterEvents[0].From, 8; got != want {
		t.Errorf("reduction From = %d, want %d", got, want)
	}
	if got, want := limiterEvents[0].To, 4; got != want {
		t.Errorf("reduction To = %d, want %d", got, want)
	}
	if !strings.Contains(limiterEvents[0].Reason, "3 consecutive 429s") {
		t.Errorf("reduction Reason = %q, want it to name the streak that caused it", limiterEvents[0].Reason)
	}
}
