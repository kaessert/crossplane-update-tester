package runner

import (
	"sync"
	"time"
)

// ThrottleEvent is one recorded reduction of an AdaptiveLimiter's ceiling —
// the run-visible record that lets a caller distinguish "the backend told
// us to slow down, and we did" from a silent slowdown that would otherwise
// be indistinguishable from a merely-slow backend.
type ThrottleEvent struct {
	At     time.Time
	From   int
	To     int
	Reason string
}

// AdaptiveLimiter bounds how many callers may be active at once, starting
// at an initial ceiling and shrinking — never growing back on its own —
// when Throttle is called. It deliberately never recovers: a run that
// throttled down is reporting a real, measured ceiling for THIS run against
// THIS backend, and silently creeping back up would erase the very signal
// Throttle exists to record. A later run, or an operator raising the
// ceiling deliberately, starts fresh.
//
// This is a semaphore with a mutable capacity rather than a fixed-size
// channel: shrinking a buffered channel's capacity is not a supported
// operation in Go, and recreating one under load would either block
// forever on already-issued permits or discard them.
type AdaptiveLimiter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	active int
	events []ThrottleEvent
}

// NewAdaptiveLimiter returns a limiter with the given initial ceiling.
// initial <= 0 is treated as 1 — this limiter is never unbounded, since an
// unbounded limiter has nothing for Throttle to shrink from.
func NewAdaptiveLimiter(initial int) *AdaptiveLimiter {
	if initial <= 0 {
		initial = 1
	}
	l := &AdaptiveLimiter{limit: initial}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Acquire blocks until fewer than the current limit are active, then
// counts this caller as active. The limit is re-read on every wake, so a
// Throttle call that shrinks the limit while callers are blocked here takes
// effect immediately rather than only for callers that arrive later.
func (l *AdaptiveLimiter) Acquire() {
	l.mu.Lock()
	for l.active >= l.limit {
		l.cond.Wait()
	}
	l.active++
	l.mu.Unlock()
}

// Release counts one caller as no longer active and wakes any blocked
// Acquire callers so they can re-check the (possibly now-lower) limit.
func (l *AdaptiveLimiter) Release() {
	l.mu.Lock()
	l.active--
	l.cond.Broadcast()
	l.mu.Unlock()
}

// Throttle reduces the ceiling to newLimit and records why, returning
// whether it actually changed anything. It is a strict decrease only:
// newLimit >= the current limit, or < 1, is rejected and reported as no
// change — this is what makes the limiter's "never grows back" guarantee
// hold regardless of what a caller passes, rather than depending on every
// caller getting the arithmetic right.
func (l *AdaptiveLimiter) Throttle(reason string, newLimit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if newLimit >= l.limit || newLimit < 1 {
		return false
	}
	l.events = append(l.events, ThrottleEvent{At: time.Now(), From: l.limit, To: newLimit, Reason: reason})
	l.limit = newLimit
	l.cond.Broadcast()
	return true
}

// CurrentLimit returns the ceiling in effect right now.
func (l *AdaptiveLimiter) CurrentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// Events returns every reduction this limiter has recorded, in the order it
// happened. The returned slice is a copy.
func (l *AdaptiveLimiter) Events() []ThrottleEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ThrottleEvent, len(l.events))
	copy(out, l.events)
	return out
}
