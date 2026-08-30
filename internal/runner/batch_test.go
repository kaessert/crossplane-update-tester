package runner

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// concurrencyProbe tracks how many DISTINCT fixture IDs have at least one
// in-flight (wrapped) exec call at the same instant, and the maximum ever
// observed. This is the batch tests' stand-in for "N workers are really
// running at once" — a fixture's OWN exec calls are always serial (RunTests
// iterates m.Tests one at a time), so overlap can only ever appear ACROSS
// fixtures, which is exactly cross-fixture concurrency RunBatch exists to
// provide.
type concurrencyProbe struct {
	mu     sync.Mutex
	active map[string]int
	max    int
}

func newConcurrencyProbe() *concurrencyProbe {
	return &concurrencyProbe{active: map[string]int{}}
}

func (p *concurrencyProbe) start(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[id]++
	if p.active[id] > 1 {
		panic(fmt.Sprintf("concurrencyProbe: fixture %q had 2 exec calls in flight at once — field tests within one fixture must stay strictly serial", id))
	}
	if n := len(p.active); n > p.max {
		p.max = n
	}
}

func (p *concurrencyProbe) end(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[id]--
	if p.active[id] <= 0 {
		delete(p.active, id)
	}
}

func (p *concurrencyProbe) observedMax() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// probedExec wraps a fakeCluster's exec with a small artificial delay (so
// two fixtures running at once actually OVERLAP in wall clock instead of
// finishing too fast to ever collide) and reports every call to probe under
// id. It also panics if the SAME id has two calls in flight simultaneously
// — see concurrencyProbe.start — which would mean RunBatch had invoked one
// target's RunTests twice concurrently, or that RunTests itself stopped
// being serial within a fixture.
func probedExec(id string, base func([]string) (string, error), probe *concurrencyProbe, delay time.Duration) func([]string) (string, error) {
	return func(args []string) (string, error) {
		probe.start(id)
		defer probe.end(id)
		if delay > 0 {
			time.Sleep(delay)
		}
		return base(args)
	}
}

// buildProbedBatchTarget constructs one fixture: an independent fakeCluster
// (so no state is ever shared between fixtures), a Runner built exactly
// like every other test in this package (newFakeRunner), and its execFunc
// wrapped so every call is delayed and reported to probe under label.
func buildProbedBatchTarget(label string, numFields int, probe *concurrencyProbe, delay time.Duration) BatchTarget {
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f)
	r.execFunc = probedExec(label, f.exec, probe, delay)
	return BatchTarget{Label: label, Runner: r, Manifest: manifestWithSequentialFieldTests(numFields)}
}

// TestRunBatchDefaultParallelismIsSerial is the "--parallel unset is a true
// no-op" acceptance criterion, verified BY BEHAVIOUR: with BatchOptions{}
// (Parallel left at its zero value), no two fixtures may ever have an
// exec call in flight at the same instant, across a fixture count large
// enough that any accidental concurrency would show up as more than one
// active label.
func TestRunBatchDefaultParallelismIsSerial(t *testing.T) {
	probe := newConcurrencyProbe()
	const n = 4
	targets := make([]BatchTarget, n)
	for i := range targets {
		targets[i] = buildProbedBatchTarget(fmt.Sprintf("fixture-%d", i), 2, probe, time.Millisecond)
	}

	RunBatch(targets, nil, BatchOptions{})

	if got := probe.observedMax(); got != 1 {
		t.Errorf("observed max concurrent fixtures = %d, want exactly 1 (--parallel unset must be strictly serial)", got)
	}
}

// TestRunBatchParallelSettingAllowsRealConcurrency is the flip side: with
// --parallel set above 1, fixtures really do overlap, bounded by the
// setting.
func TestRunBatchParallelSettingAllowsRealConcurrency(t *testing.T) {
	probe := newConcurrencyProbe()
	const n = 8
	const parallel = 4
	targets := make([]BatchTarget, n)
	for i := range targets {
		targets[i] = buildProbedBatchTarget(fmt.Sprintf("fixture-%d", i), 3, probe, 2*time.Millisecond)
	}

	RunBatch(targets, nil, BatchOptions{Parallel: parallel})

	got := probe.observedMax()
	if got <= 1 {
		t.Fatalf("observed max concurrent fixtures = %d, want > 1 (--parallel %d should let fixtures overlap)", got, parallel)
	}
	if got > parallel {
		t.Errorf("observed max concurrent fixtures = %d, want <= %d (the configured ceiling)", got, parallel)
	}
}

// TestRunBatchAttributesResultsByInputIndex proves results land at each
// target's own input index rather than in completion order: fixtures are
// given DELIBERATELY different amounts of work (so they finish in a
// scrambled order under real concurrency), and every result must still
// carry the Label its own index was built with.
func TestRunBatchAttributesResultsByInputIndex(t *testing.T) {
	probe := newConcurrencyProbe()
	// Deliberately decreasing amounts of work, so later-indexed targets
	// finish FIRST under concurrency — an append-in-completion-order bug
	// would visibly scramble the labels.
	sizes := []int{6, 5, 4, 3, 2, 1}
	targets := make([]BatchTarget, len(sizes))
	for i, n := range sizes {
		targets[i] = buildProbedBatchTarget(fmt.Sprintf("fixture-%d", i), n, probe, time.Millisecond)
	}

	summary := RunBatch(targets, nil, BatchOptions{Parallel: 6})

	if len(summary.Results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(summary.Results), len(targets))
	}
	for i, res := range summary.Results {
		wantLabel := fmt.Sprintf("fixture-%d", i)
		if res.Label != wantLabel {
			t.Errorf("Results[%d].Label = %q, want %q", i, res.Label, wantLabel)
		}
		if res.Err != nil {
			t.Errorf("Results[%d]: unexpected error: %v", i, res.Err)
		}
		if len(res.Results) != sizes[i] {
			t.Errorf("Results[%d]: got %d field results, want %d", i, len(res.Results), sizes[i])
		}
	}
}

// TestRunBatchVerdictParityAcrossParallelism is the "verdict parity proven,
// not asserted" acceptance criterion: the SAME fixture set, freshly built
// twice (each RunTests call mutates its own fakeCluster's state, so a
// second run needs its own independent copy), produces IDENTICAL per-field
// verdicts whether run at --parallel 1 or --parallel 8. Duration is the
// only field allowed to differ — it is wall-clock and never part of a
// verdict.
func TestRunBatchVerdictParityAcrossParallelism(t *testing.T) {
	build := func() []BatchTarget {
		const n = 6
		targets := make([]BatchTarget, n)
		for i := 0; i < n; i++ {
			f := &fakeCluster{
				forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
				atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(0)},
				generation:  1,
				kind:        testKindExample,
				name:        testNameExample,
				// Alternate evidenced/not-evidenced so the comparison
				// spans more than one verdict shape (PASS and
				// NOT-EVIDENCED), not just a single uniform outcome a
				// weaker parity bug could still pass.
				recordUpdateEvent: i%2 == 0,
			}
			r := newFakeRunner(f)
			// A NOT-EVIDENCED verdict busy-retries countUpdateEvents for
			// the whole evidence window before concluding — shrink it far
			// below newFakeRunner's own testEvidenceWindow so the three
			// intentionally-not-evidenced fixtures here do not dominate
			// this test's wall clock across two full batch runs.
			r.evidenceWindow = time.Millisecond
			targets[i] = BatchTarget{
				Label:    fmt.Sprintf("fixture-%d", i),
				Runner:   r,
				Manifest: manifestWithSequentialFieldTests(3),
			}
		}
		return targets
	}

	serial := RunBatch(build(), nil, BatchOptions{Parallel: 1})
	parallel := RunBatch(build(), nil, BatchOptions{Parallel: 8})

	opts := cmp.Options{
		cmpopts.IgnoreFields(TestResult{}, "Duration"),
		cmp.Comparer(func(a, b error) bool {
			if a == nil || b == nil {
				return a == b
			}
			return a.Error() == b.Error()
		}),
	}
	if diff := cmp.Diff(serial.Results, parallel.Results, opts); diff != "" {
		t.Errorf("verdicts differ between --parallel 1 and --parallel 8 (-serial +parallel):\n%s", diff)
	}
}

// TestRunOneBatchTargetPropagatesRunTestsError proves a target that fails
// to EXECUTE (as opposed to one whose field tests fail with an ordinary
// verdict) surfaces that failure on BatchResult.Err, still correctly
// labelled.
func TestRunOneBatchTargetPropagatesRunTestsError(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
	}
	r := newFakeRunner(f)
	wantErr := errors.New("simulated snapshot failure")
	r.execFunc = func(args []string) (string, error) {
		if len(args) > 0 && args[0] == kubectlGetSubcommand {
			return "", wantErr
		}
		return f.exec(args)
	}

	target := BatchTarget{Label: "broken", Runner: r, Manifest: manifestWithSequentialFieldTests(1)}
	res := runOneBatchTarget(target)

	if res.Label != "broken" {
		t.Errorf("Label = %q, want %q", res.Label, "broken")
	}
	if res.Err == nil {
		t.Fatal("Err = nil, want a propagated error")
	}
	if len(res.Results) != 0 {
		t.Errorf("Results = %v, want empty on an execution failure", res.Results)
	}
}

// TestRunBatchReducesConcurrencyOnSustainedBackendThrottling drives EVERY
// request the batch's shared client set observes to a simulated 429 —
// deterministic and race-free by construction (no reset ever interrupts
// the streak, so the outcome cannot depend on goroutine scheduling order)
// — and proves the backend rate-limit signal actually shrinks the worker
// ceiling, and that the run RECORDS every reduction rather than silently
// slowing down.
func TestRunBatchReducesConcurrencyOnSustainedBackendThrottling(t *testing.T) {
	tracker := newThrottleTracker()
	clients := &SharedClients{throttle: tracker}

	const n = 8
	targets := make([]BatchTarget, n)
	for i := 0; i < n; i++ {
		f := &fakeCluster{
			forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
			atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
			generation:        1,
			kind:              testKindExample,
			name:              testNameExample,
			recordUpdateEvent: true,
		}
		r := newFakeRunner(f)
		base := f.exec
		r.execFunc = func(args []string) (string, error) {
			// Every single request this batch issues reports as a 429
			// to the shared tracker BEFORE the fake cluster serves it —
			// mirroring an observing RoundTripper that never alters the
			// underlying response. The streak therefore only ever
			// grows: there is nothing here for goroutine ordering to
			// race against.
			tracker.Observe(http.StatusTooManyRequests, time.Second, "fake://backend")
			return base(args)
		}
		targets[i] = BatchTarget{Label: fmt.Sprintf("fixture-%d", i), Runner: r, Manifest: manifestWithSequentialFieldTests(3)}
	}

	summary := RunBatch(targets, clients, BatchOptions{Parallel: 8})

	if len(summary.Throttle) == 0 {
		t.Fatal("summary.Throttle is empty — sustained 429s must be recorded as throttle events")
	}
	first := summary.Throttle[0]
	if first.From != 8 {
		t.Errorf("first throttle event From = %d, want 8 (the original --parallel)", first.From)
	}
	if first.To != 4 {
		t.Errorf("first throttle event To = %d, want 4 (halved from 8)", first.To)
	}
	last := summary.Throttle[len(summary.Throttle)-1]
	if last.To != 1 {
		t.Errorf("final throttle ceiling = %d, want 1 (enough sustained 429s to floor it)", last.To)
	}
	for i := 1; i < len(summary.Throttle); i++ {
		if summary.Throttle[i].To >= summary.Throttle[i-1].To {
			t.Errorf("throttle event %d did not strictly decrease: %+v -> %+v", i, summary.Throttle[i-1], summary.Throttle[i])
		}
	}

	// Every fixture must still have completed and reported a real verdict
	// — adaptive throttling changes how many run AT ONCE, never whether a
	// fixture's own results are produced.
	for i, res := range summary.Results {
		if res.Err != nil {
			t.Errorf("Results[%d]: unexpected error under throttling: %v", i, res.Err)
		}
		if len(res.Results) != 3 {
			t.Errorf("Results[%d]: got %d field results, want 3", i, len(res.Results))
		}
	}
}

// TestRunBatchConcurrentAccessIsRaceFree exercises the batch path with
// several workers under `go test -race`: distinct fixtures writing to
// distinct result-slice indices, a shared AdaptiveLimiter and a shared
// ThrottleTracker all being touched from multiple goroutines at once. Any
// data race here fails the race detector regardless of what the assertions
// below check.
func TestRunBatchConcurrentAccessIsRaceFree(t *testing.T) {
	tracker := newThrottleTracker()
	clients := &SharedClients{throttle: tracker}
	var calls int32

	const n = 16
	targets := make([]BatchTarget, n)
	for i := 0; i < n; i++ {
		f := &fakeCluster{
			forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
			atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
			generation:        1,
			kind:              testKindExample,
			name:              testNameExample,
			recordUpdateEvent: true,
		}
		r := newFakeRunner(f)
		base := f.exec
		r.execFunc = func(args []string) (string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n%7 == 0 {
				tracker.Observe(http.StatusTooManyRequests, 0, "fake://backend")
			} else {
				tracker.Observe(http.StatusOK, 0, "fake://backend")
			}
			return base(args)
		}
		targets[i] = BatchTarget{Label: fmt.Sprintf("fixture-%d", i), Runner: r, Manifest: manifestWithSequentialFieldTests(2)}
	}

	summary := RunBatch(targets, clients, BatchOptions{Parallel: 6})
	if len(summary.Results) != n {
		t.Fatalf("got %d results, want %d", len(summary.Results), n)
	}
	seen := map[string]bool{}
	for i, res := range summary.Results {
		wantLabel := fmt.Sprintf("fixture-%d", i)
		if res.Label != wantLabel {
			t.Errorf("Results[%d].Label = %q, want %q", i, res.Label, wantLabel)
		}
		if seen[res.Label] {
			t.Errorf("Label %q attributed to more than one index", res.Label)
		}
		seen[res.Label] = true
	}
}
