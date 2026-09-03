package runner

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// ConvergeTarget is one resource participating in a shared convergence
// window: the Runner that will observe it, its parsed manifest, the
// per-resource options (IgnoreFields differ per resource), and a Label used
// only for reporting.
type ConvergeTarget struct {
	Label    string
	Runner   *Runner
	Manifest *manifest.Manifest
	Opts     ConvergeOptions
}

// ConvergeAllResult is one target's verdict. Exactly one of Result and Err
// is non-nil: Err carries a failure to *evaluate* the resource (a client-go
// read error, unresolvable manifest), which is distinct from a
// ConvergeResult whose Passed is false — a verdict the check reached
// successfully.
type ConvergeAllResult struct {
	Label  string
	Result *ConvergeResult
	Err    error
}

// defaultConvergeAllConcurrency bounds how many resources are armed or
// asserted at once. Every step reads through the one long-lived client-go
// client behind the shared QPS/Burst limiter kubeRESTConfig sets, so this
// now caps in-flight API-server requests rather than concurrent processes —
// an unbounded fan-out over a 65-resource provider would still queue behind
// that shared limiter, but this keeps the fan-out itself bounded too.
const defaultConvergeAllConcurrency = 8

// RunConvergeAll evaluates every target against a SINGLE shared observation
// window instead of giving each one its own.
//
// # Why this is not just an optimisation
//
// The serial form runs N independent windows back to back, so a provider
// with N resources spends N * pollInterval * 1.5 asleep — for provider-vultr
// (54 annotated manifests, two converge steps per post-assert hook) that is
// 108 sequential waits. Every one of those waits is observing a DIFFERENT
// stretch of wall-clock time, which is strictly weaker than observing one
// common stretch: drift that only manifests when several controllers are
// reconciling at once falls between the windows and is never seen.
//
// # Why it is safe
//
// convergeArm and convergeAssert perform reads only — a client-go get on
// the resource and on its events. Neither mutates the cluster or the backend,
// so no target can perturb another's observation, and the checks may be
// interleaved freely. This is what distinguishes convergence from the
// per-field update tests in RunTests, which patch the resource under test
// and restart the provider Deployment, and therefore cannot share a window.
//
// # Why the window is measured from the LAST baseline
//
// Arming is not instantaneous: each target waits for its generation to
// settle and for Ready, so baselines land at different times. The window
// must satisfy every target's own guarantee of "at least one full reconcile
// cycle since MY baseline", so it closes at
//
//	max(ArmedAt) + max(convergeWait(opts))
//
// Every target therefore observes at least its own required duration, and
// all but the last observe strictly more. Taking the max of the per-target
// waits (rather than assuming a single interval) keeps that guarantee when
// targets declare different poll intervals.
func RunConvergeAll(targets []ConvergeTarget, concurrency int) []ConvergeAllResult {
	if concurrency <= 0 {
		concurrency = defaultConvergeAllConcurrency
	}

	results := make([]ConvergeAllResult, len(targets))
	baselines := make([]*convergeBaseline, len(targets))

	// PHASE 1 — arm every target. A target that returns an early verdict
	// (converge-skip, unsettled generation) or an error is recorded now and
	// takes no part in the shared window.
	forEachConcurrently(len(targets), concurrency, func(i int) {
		t := targets[i]
		results[i].Label = t.Label
		b, early, err := t.Runner.convergeArm(t.Manifest, t.Opts)
		switch {
		case err != nil:
			results[i].Err = err
		case early != nil:
			results[i].Result = early
		default:
			baselines[i] = b
		}
	})

	// PHASE 2 — the barrier. One wait for the whole fleet.
	deadline, armed := convergeAllDeadline(targets, baselines)
	if armed == 0 {
		return results
	}
	if d := time.Until(deadline); d > 0 {
		time.Sleep(d)
	}

	// PHASE 3 — assert every armed target against the closed window.
	forEachConcurrently(len(targets), concurrency, func(i int) {
		if baselines[i] == nil {
			return
		}
		t := targets[i]
		// The observed window is this target's OWN elapsed time, not
		// convergeWait: the barrier closes at max(ArmedAt)+maxWait, so a
		// resource armed early was under observation for longer. Counting
		// Update() log calls over a shorter window than was actually
		// observed would under-report a loop on exactly those resources.
		res, err := t.Runner.convergeAssert(t.Manifest, t.Opts, baselines[i], time.Since(baselines[i].ArmedAt))
		if err != nil {
			results[i].Err = err
			return
		}
		results[i].Result = res
	})

	return results
}

// convergeAllDeadline returns the instant at which the shared window closes,
// and how many targets are armed. See RunConvergeAll for why the deadline is
// anchored on the latest baseline and the longest per-target wait.
func convergeAllDeadline(targets []ConvergeTarget, baselines []*convergeBaseline) (deadline time.Time, armed int) {
	var maxWait time.Duration
	for i, b := range baselines {
		if b == nil {
			continue
		}
		armed++
		if b.ArmedAt.After(deadline) {
			deadline = b.ArmedAt
		}
		if w := convergeWait(targets[i].Opts); w > maxWait {
			maxWait = w
		}
	}
	return deadline.Add(maxWait), armed
}

// forEachConcurrently runs fn(0..n-1) with at most `limit` in flight. Each
// fn writes only to its own index of the shared slices, so no further
// synchronisation is needed.
func forEachConcurrently(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// FormatConvergeAllSummary renders a one-verdict-line-per-target summary
// plus a tally, followed by that target's own Diagnostics (if any) —
// present whether the target passed or failed, since a note such as a
// controller-pod restart or a readiness-timeout is informational rather
// than a failure reason, and an operator reading a passing result should
// still see it. Returns the text and whether every target passed.
func FormatConvergeAllSummary(results []ConvergeAllResult) (text string, ok bool) {
	return formatConvergeAllSummary(results, nil)
}

// FormatConvergeAllSummaryWithFindings renders the same summary as
// FormatConvergeAllSummary, but additionally prints, immediately after a
// FAILING target's own verdict line, the pre-formatted round-trip findings
// text supplied for that target's Label (see RoundtripFindingsForFailures) —
// adjacent to the verdict, not in a separate trailing report. A target with
// no entry in findings (including every passing, skipped, or errored one —
// see the switch below) renders identically to FormatConvergeAllSummary.
//
// Findings are advisory only: ok is derived from the SAME pass/fail/error
// counts FormatConvergeAllSummary itself uses, so their presence or content
// can never change whether the run is reported as passing. Passing a nil or
// empty findings map reproduces FormatConvergeAllSummary's output exactly —
// that equivalence is itself asserted by a test, not just implied by the
// shared implementation below.
func FormatConvergeAllSummaryWithFindings(results []ConvergeAllResult, findings map[string]string) (text string, ok bool) {
	return formatConvergeAllSummary(results, findings)
}

// formatConvergeAllSummary is the shared implementation behind both
// exported summary formatters above. findings is only ever consulted in the
// FAILING branch — a passing, skipped, or errored target never looks it up
// — which is what makes a findings entry for any other target's Label
// structurally inert rather than merely untested.
func formatConvergeAllSummary(results []ConvergeAllResult, findings map[string]string) (text string, ok bool) {
	var b []byte
	pass, fail, skip, errs := 0, 0, 0, 0
	for _, r := range results {
		switch {
		case r.Err != nil:
			errs++
			b = append(b, fmt.Sprintf("  ✗ %-40s ERROR: %v\n", r.Label, r.Err)...)
		case r.Result.Skipped:
			skip++
			b = append(b, fmt.Sprintf("  ⊘ %-40s CONVERGE-SKIP: %s\n", r.Label, r.Result.SkipMsg)...)
		case r.Result.Passed:
			pass++
			b = append(b, fmt.Sprintf("  ✓ %-40s %s\n", r.Label, r.Result.Message)...)
			// A passing result can still carry Diagnostics — the restart,
			// readiness-timeout and controller-log-unavailable notes
			// buildConvergeResult attaches whether or not the check
			// passed (see ConvergeResult.Diagnostics doc). The
			// single-target path (printConvergeResult) already surfaces
			// these on a pass; without this loop converge-all silently
			// dropped them for every passing target. findings is never
			// consulted here — it is failure-only, per the doc comment
			// above.
			for _, d := range r.Result.Diagnostics {
				b = append(b, fmt.Sprintf("      %s\n", d)...)
			}
		default:
			fail++
			b = append(b, fmt.Sprintf("  ✗ %-40s %s\n", r.Label, r.Result.Message)...)
			for _, d := range r.Result.Diagnostics {
				b = append(b, fmt.Sprintf("      %s\n", d)...)
			}
			if f := findings[r.Label]; f != "" {
				for _, line := range strings.Split(f, "\n") {
					b = append(b, fmt.Sprintf("      %s\n", line)...)
				}
			}
		}
	}
	b = append(b, fmt.Sprintf("\n%d passed, %d failed, %d skipped, %d errored\n", pass, fail, skip, errs)...)
	return string(b), fail == 0 && errs == 0
}
