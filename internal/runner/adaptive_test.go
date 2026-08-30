package runner

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdaptiveLimiterBoundsConcurrency proves the core semaphore property:
// at no point does the number of concurrently-active Acquire holders exceed
// the configured limit.
func TestAdaptiveLimiterBoundsConcurrency(t *testing.T) {
	limiter := NewAdaptiveLimiter(3)
	var active int32
	var maxObserved int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limiter.Acquire()
			defer limiter.Release()
			n := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxObserved)
				if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()

	if maxObserved > 3 {
		t.Errorf("observed %d concurrently active, want <= 3", maxObserved)
	}
	if maxObserved < 2 {
		t.Errorf("observed only %d concurrently active across 20 goroutines with limit 3 — the pool never actually ran in parallel, so this test proves nothing", maxObserved)
	}
}

// TestAdaptiveLimiterThrottleShrinksConcurrencyImmediately proves Throttle
// takes effect for callers ALREADY BLOCKED in Acquire, not merely for
// callers that arrive after it — a limiter that only applied a new ceiling
// to future Acquire calls would let an already-admitted burst finish at the
// old, too-high concurrency.
func TestAdaptiveLimiterThrottleShrinksConcurrencyImmediately(t *testing.T) {
	limiter := NewAdaptiveLimiter(4)
	release := make(chan struct{})
	var active int32
	var maxObserved int32
	var wg sync.WaitGroup

	track := func() {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxObserved)
			if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
				break
			}
		}
	}

	// Fill the original ceiling of 4.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limiter.Acquire()
			track()
			<-release
			atomic.AddInt32(&active, -1)
			limiter.Release()
		}()
	}
	waitForActive(t, &active, 4)

	// Throttle down to 2 while all 4 slots are held; queue 2 more callers.
	if !limiter.Throttle("test-induced", 2) {
		t.Fatal("Throttle(2) returned false, want true")
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limiter.Acquire()
			track()
			atomic.AddInt32(&active, -1)
			limiter.Release()
		}()
	}

	close(release)
	wg.Wait()

	if maxObserved > 4 {
		t.Fatalf("observed %d concurrently active, want <= 4 (the original ceiling, never exceeded)", maxObserved)
	}
	// After the throttle takes effect, active concurrency must never
	// exceed the NEW ceiling once the pre-throttle batch has drained.
	if got := limiter.CurrentLimit(); got != 2 {
		t.Errorf("CurrentLimit() = %d, want 2", got)
	}
}

// TestAdaptiveLimiterThrottleNeverIncreases proves the one-way guarantee:
// a caller passing a higher or equal newLimit — including by mistake — is
// rejected rather than silently growing the ceiling back.
func TestAdaptiveLimiterThrottleNeverIncreases(t *testing.T) {
	limiter := NewAdaptiveLimiter(4)
	if limiter.Throttle("bad", 4) {
		t.Error("Throttle(4) on a limiter already at 4 returned true, want false (no strict decrease)")
	}
	if limiter.Throttle("bad", 10) {
		t.Error("Throttle(10) on a limiter at 4 returned true, want false (must never increase)")
	}
	if limiter.Throttle("bad", 0) {
		t.Error("Throttle(0) returned true, want false (floor is 1)")
	}
	if limiter.CurrentLimit() != 4 {
		t.Fatalf("CurrentLimit() = %d, want 4 (unchanged by every rejected call)", limiter.CurrentLimit())
	}
	if !limiter.Throttle("good", 2) {
		t.Fatal("Throttle(2) returned false, want true")
	}
	if limiter.CurrentLimit() != 2 {
		t.Fatalf("CurrentLimit() = %d, want 2", limiter.CurrentLimit())
	}
	if events := limiter.Events(); len(events) != 1 || events[0].From != 4 || events[0].To != 2 || events[0].Reason != "good" {
		t.Fatalf("Events() = %+v, want one event 4->2 reason %q", events, "good")
	}
}

// TestNewAdaptiveLimiterFloorsAtOne proves a non-positive initial ceiling
// never produces a limiter with nothing to shrink from.
func TestNewAdaptiveLimiterFloorsAtOne(t *testing.T) {
	for _, initial := range []int{0, -5} {
		if got := NewAdaptiveLimiter(initial).CurrentLimit(); got != 1 {
			t.Errorf("NewAdaptiveLimiter(%d).CurrentLimit() = %d, want 1", initial, got)
		}
	}
}

// TestAdaptiveThrottleHandlerRequiresSustainedStreak proves the policy
// wired into RunBatch: a streak below sustainedThrottleStreak must not
// touch the limiter at all, and reaching it halves the ceiling (floored at
// 1) exactly once per crossing.
func TestAdaptiveThrottleHandlerRequiresSustainedStreak(t *testing.T) {
	limiter := NewAdaptiveLimiter(8)
	handler := adaptiveThrottleHandler(limiter)

	handler(1)
	handler(2)
	if limiter.CurrentLimit() != 8 {
		t.Fatalf("CurrentLimit() = %d after a sub-threshold streak, want unchanged 8", limiter.CurrentLimit())
	}

	handler(3)
	if got := limiter.CurrentLimit(); got != 4 {
		t.Fatalf("CurrentLimit() = %d after crossing the threshold, want 4 (halved from 8)", got)
	}

	handler(4)
	if got := limiter.CurrentLimit(); got != 2 {
		t.Fatalf("CurrentLimit() = %d after a second sustained streak, want 2 (halved again from 4)", got)
	}

	// Repeated halving must floor at 1, never reach 0.
	handler(5)
	handler(6)
	if got := limiter.CurrentLimit(); got != 1 {
		t.Fatalf("CurrentLimit() = %d, want floored at 1", got)
	}

	events := limiter.Events()
	if len(events) != 3 {
		t.Fatalf("recorded %d throttle events, want 3", len(events))
	}
}

// waitForActive polls until *active reaches want or the test times out —
// used only to synchronise on the semaphore actually reaching a known
// state before the next phase of a test, never as the assertion itself.
func waitForActive(t *testing.T, active *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(active) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active never reached %d within the deadline (last seen %d)", want, atomic.LoadInt32(active))
}
