package runner

import (
	"fmt"
	"sync"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// BatchTarget is one fixture participating in a batch run: the Runner that
// will execute its field tests — already carrying whatever timeout and poll
// interval the caller configured, and already pointed at a shared
// SharedClients via Apply — its parsed manifest, and a Label used only for
// output attribution.
type BatchTarget struct {
	Label    string
	Runner   *Runner
	Manifest *manifest.Manifest
}

// BatchResult is one target's outcome. Exactly one of Err and a populated
// Results is meaningful on failure, mirroring ConvergeAllResult's own split:
// Err carries a failure to EXECUTE the target at all (an unresolvable
// manifest, a kubeconfig-level error); a Results slice with individual
// failing entries is a verdict RunTests reached successfully — the field
// itself failed, which is not the same thing as the run failing to happen.
type BatchResult struct {
	Label               string
	Manifest            *manifest.Manifest
	Results             []TestResult
	UnchangedViolations []UnchangedAssertion
	Err                 error
}

// BatchOptions configures RunBatch.
type BatchOptions struct {
	// Parallel is the initial worker ceiling. <= 0 is treated as
	// defaultBatchParallel (1) — batch mode's serial, default behaviour.
	Parallel int
}

// BatchSummary is RunBatch's complete output: every target's result, at
// that target's own input INDEX (never completion order — see RunBatch),
// plus whatever adaptive-concurrency reductions the run's backend
// rate-limit signal triggered, in the order they happened.
type BatchSummary struct {
	Results  []BatchResult
	Throttle []ThrottleEvent
}

// defaultBatchParallel is BatchOptions.Parallel's effective value when a
// caller passes <= 0 (including simply never setting it). It is 1, not
// converge-all's defaultConvergeAllConcurrency of 8: batch mode ships
// dormant, so a caller that never opts in must observe EXACTLY today's
// serial, one-fixture-at-a-time behaviour — not a new default level of
// concurrency nobody asked for.
const defaultBatchParallel = 1

// sustainedThrottleStreak is how many CONSECUTIVE 429 responses the shared
// client set's ThrottleTracker must observe before RunBatch halves the
// active worker ceiling. A single 429 is not the signal: client-go's own
// transport already retries a 429 internally before a caller ever sees one
// surface as an error, so reacting to the first one would misreport an
// ordinary, already-handled retry as sustained backend pressure. A run of
// several with nothing successful between them is what "the current
// concurrency level is too high for this backend" actually looks like.
const sustainedThrottleStreak = 3

// RunBatch executes every target's RunTests CONCURRENTLY, bounded by an
// AdaptiveLimiter seeded at opts.Parallel (or defaultBatchParallel when
// unset).
//
// # Cross-fixture parallel, intra-fixture serial
//
// Only DIFFERENT targets (different backend objects) ever run at the same
// time. Within one target, RunTests iterates its manifest's field tests one
// at a time exactly as it always has — RunBatch calls RunTests once per
// target and never touches that loop. This is required, not merely
// convenient: two concurrent patches on ONE object collide on
// resourceVersion, and that object's aggregated event count cannot be
// attributed to whichever field test just moved it, so evidence-based
// verdicts would misattribute their proof the moment two field tests on the
// same object overlapped.
//
// # Deterministic, attributable output
//
// Results are written into the return slice at each target's own input
// index, never appended in completion order — the same reason
// RunConvergeAll's own results slice is pre-sized and index-addressed
// rather than appended to. A caller's output is therefore reproducible
// regardless of which fixture happens to finish first, and every result is
// traceable back to the fixture that produced it.
//
// # One shared client set
//
// clients is built ONCE by the caller (NewSharedClients) and pointed at
// every target's Runner (SharedClients.Apply) before this is ever called.
// RunBatch itself resolves no kubeconfig and builds no client — "one shared
// client set regardless of --parallel" is therefore a property of the
// CALLER's construction, verified once, rather than a per-worker branch
// inside this function that could regress independently of that check.
// clients may be nil (a target's Runner already fully wired for a
// standalone test, with no batch-level rate-limit signal to observe) —
// RunBatch degrades to plain bounded concurrency with no adaptive
// reduction in that case.
func RunBatch(targets []BatchTarget, clients *SharedClients, opts BatchOptions) BatchSummary {
	limit := opts.Parallel
	if limit <= 0 {
		limit = defaultBatchParallel
	}
	limiter := NewAdaptiveLimiter(limit)

	if clients != nil && clients.Throttle() != nil {
		clients.Throttle().SetOnThrottle(adaptiveThrottleHandler(limiter))
	}

	results := make([]BatchResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t BatchTarget) {
			defer wg.Done()
			limiter.Acquire()
			defer limiter.Release()
			results[i] = runOneBatchTarget(t)
		}(i, t)
	}
	wg.Wait()

	return BatchSummary{Results: results, Throttle: limiter.Events()}
}

// adaptiveThrottleHandler builds the ThrottleTracker callback RunBatch
// registers: on a streak of at least sustainedThrottleStreak consecutive
// 429s, halve the limiter's current ceiling (floor 1) and record why. Split
// out from RunBatch so the halving policy itself — as opposed to the wiring
// that connects a tracker to a limiter — is a unit directly testable
// without constructing a whole batch run.
func adaptiveThrottleHandler(limiter *AdaptiveLimiter) func(streak int) {
	return func(streak int) {
		if streak < sustainedThrottleStreak {
			return
		}
		cur := limiter.CurrentLimit()
		next := cur / 2
		if next < 1 {
			next = 1
		}
		limiter.Throttle(fmt.Sprintf("%d consecutive 429s from the backend", streak), next)
	}
}

// runOneBatchTarget executes one target's field tests and packages the
// outcome. Split out from RunBatch so it is directly testable (call-count
// assertions, single-target error paths) without a worker pool around it.
func runOneBatchTarget(t BatchTarget) BatchResult {
	results, violations, err := t.Runner.RunTests(t.Manifest)
	return BatchResult{
		Label:               t.Label,
		Manifest:            t.Manifest,
		Results:             results,
		UnchangedViolations: violations,
		Err:                 err,
	}
}
