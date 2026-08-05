package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// Kinds and names used across this package's tests. They stand in for any
// managed resource: the runner's logic is entirely kind-agnostic, so the
// fixtures are deliberately generic.
const (
	testKindExample = "ExampleResource"
	testNameExample = "example-resource"
	testKindOther   = "OtherResource"
	testNameOther   = "example-other-resource"
	// testNameOtherInstance is a second instance of testKindExample — used
	// to prove event matching is per-object, not per-kind.
	testNameOtherInstance = "some-other-example-resource"
)

// newTestEventItem builds an eventItem for the given reason, aggregated
// count, kind, and name — reducing repetition of the anonymous
// InvolvedObject struct literal across test cases.
func newTestEventItem(reason string, count int32, kind, name string) eventItem {
	e := eventItem{Reason: reason, Count: count}
	e.InvolvedObject.Kind = kind
	e.InvolvedObject.Name = name
	return e
}

// TestSumEventOccurrences covers the event-aggregation counting logic that
// backs countUpdateEvents. client-go's event recorder aggregates repeated
// identical events onto a single Item by incrementing that Item's .count
// field instead of appending a new Item, so the function under test must
// sum .count rather than count Items.
func TestSumEventOccurrences(t *testing.T) {
	cases := map[string]struct {
		list eventList
		kind string
		name string
		want int
	}{
		"AggregatedSingleItemCountSeven": {
			// The exact defect scenario: one Item, .count=7. len(items) would
			// wrongly report 1; the fix must report 7.
			list: eventList{Items: []eventItem{
				newTestEventItem(eventReasonCannotUpdate, 7, testKindOther, testNameOther),
			}},
			kind: testKindOther,
			name: testNameOther,
			want: 7,
		},
		"AggregationDeltaFromBaselineToOutcome": {
			// Simulates the before/after pattern used by RunConverge: a
			// baseline snapshot at .count=N, then an outcome snapshot at
			// .count=N+k. The raw sum at the outcome snapshot must be N+k so
			// the caller's delta computation (afterEvents - beforeEvents)
			// yields k, not 0.
			list: eventList{Items: []eventItem{
				newTestEventItem(eventReasonUpdated, 12, testKindExample, testNameExample),
			}},
			kind: testKindExample,
			name: testNameExample,
			want: 12,
		},
		"ZeroCountTreatedAsSingleOccurrence": {
			// A non-aggregated event has .count == 0 (the field is only
			// populated by client-go once a duplicate is coalesced). It must
			// still count as exactly one occurrence, not zero.
			list: eventList{Items: []eventItem{
				newTestEventItem(eventReasonUpdated, 0, testKindExample, testNameExample),
			}},
			kind: testKindExample,
			name: testNameExample,
			want: 1,
		},
		"MultipleItemsSummed": {
			// Two aggregated Items for the same object (e.g. one for each
			// reason) must have their counts summed together.
			list: eventList{Items: []eventItem{
				newTestEventItem(eventReasonUpdated, 3, testKindExample, testNameExample),
				newTestEventItem(eventReasonCannotUpdate, 4, testKindExample, testNameExample),
			}},
			kind: testKindExample,
			name: testNameExample,
			want: 7,
		},
		"NonMatchingReasonIgnored": {
			list: eventList{Items: []eventItem{
				newTestEventItem("Synced", 9, testKindExample, testNameExample),
			}},
			kind: testKindExample,
			name: testNameExample,
			want: 0,
		},
		"NonMatchingInvolvedObjectIgnored": {
			list: eventList{Items: []eventItem{
				newTestEventItem(eventReasonUpdated, 5, testKindExample, testNameOtherInstance),
			}},
			kind: testKindExample,
			name: testNameExample,
			want: 0,
		},
		"EmptyList": {
			list: eventList{},
			kind: testKindExample,
			name: testNameExample,
			want: 0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := sumEventOccurrences(tc.list, tc.kind, tc.name)
			if got != tc.want {
				t.Errorf("sumEventOccurrences() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBuildConvergeResultReportsAggregatedUpdateDelta ensures the pass/fail
// decision reflects an update delta derived from aggregated event counts,
// not raw Item counts. A looping resource whose events are aggregated into
// one Item across the wait window (before=1, after=8, i.e. 7 new
// occurrences) must fail.
func TestBuildConvergeResultReportsAggregatedUpdateDelta(t *testing.T) {
	cases := map[string]struct {
		beforeEvents int
		afterEvents  int
		wantPassed   bool
	}{
		"StableNoNewEvents": {
			beforeEvents: 3,
			afterEvents:  3,
			wantPassed:   true,
		},
		"AggregatedDeltaDetected": {
			// before=1 occurrence, after=8 occurrences (single aggregated
			// Item whose .count grew by 7) — must be detected as 7 new
			// updates, not treated as 0 because the Item count didn't change.
			beforeEvents: 1,
			afterEvents:  8,
			wantPassed:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := buildConvergeResult(nil, 1, 1, tc.beforeEvents, tc.afterEvents, true, nil)
			if result.Passed != tc.wantPassed {
				t.Errorf("Passed = %v, want %v (diagnostics: %v)", result.Passed, tc.wantPassed, result.Diagnostics)
			}
		})
	}
}

// TestBuildConvergeResultFailsOnAtProviderDrift confirms the snapshot-diff
// signal is a real gate independently of the event delta: an atProvider
// field that keeps changing across a quiet window is a reconciliation loop
// even when no update event was recorded for it.
func TestBuildConvergeResultFailsOnAtProviderDrift(t *testing.T) {
	diff := []differ.FieldChange{{Field: "someCounter", OldValue: "1", NewValue: "2"}}

	result := buildConvergeResult(diff, 1, 1, 0, 0, true, nil)
	if result.Passed {
		t.Fatalf("expected Passed=false when atProvider drifted, got %+v", result)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected a diagnostic naming the drifted field")
	}
}

// TestBuildConvergeResultReadinessFlapFailsIndependently confirms a final
// Ready!=True reading is its own failure reason, distinct from the
// atProvider diff: a resource with a perfectly stable atProvider snapshot
// (zero diff, zero new events, unchanged generation) still fails the check
// if it is not Ready at the final snapshot.
func TestBuildConvergeResultReadinessFlapFailsIndependently(t *testing.T) {
	result := buildConvergeResult(nil, 1, 1, 0, 0, false, nil)
	if result.Passed {
		t.Fatalf("expected Passed=false on a readiness flap with no atProvider drift, got %+v", result)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected exactly one diagnostic (the readiness flap), got %v", result.Diagnostics)
	}
	if strings.Contains(result.Diagnostics[0], "atProvider changed") {
		t.Errorf("readiness flap diagnostic must not be folded into the atProvider diff wording: %q", result.Diagnostics[0])
	}
}

// TestBuildConvergeResultHeadline pins the headline message to the failure
// reason rather than assuming every failure is a reconciliation loop: only
// an atProvider diff or a new update event is real evidence the controller
// wrote something repeatedly. A readiness flap and/or an unsettled
// generation, on their own, are failures but not loops.
func TestBuildConvergeResultHeadline(t *testing.T) {
	driftDiff := []differ.FieldChange{{Field: "someCounter", OldValue: "1", NewValue: "2"}}

	cases := map[string]struct {
		diff                      []differ.FieldChange
		gen, afterGen             int64
		beforeEvents, afterEvents int
		afterReady                bool
		wantMessage               string
	}{
		"ReadinessFlapOnlyIsNotALoop": {
			diff: nil, gen: 1, afterGen: 1,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  false,
			wantMessage: "RESOURCE NOT IN STEADY STATE",
		},
		"GenerationChangeOnlyIsNotALoop": {
			diff: nil, gen: 1, afterGen: 2,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  true,
			wantMessage: "RESOURCE NOT IN STEADY STATE",
		},
		"ReadinessFlapAndGenerationChangeTogetherStillNotALoop": {
			diff: nil, gen: 1, afterGen: 2,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  false,
			wantMessage: "RESOURCE NOT IN STEADY STATE",
		},
		"AtProviderDriftIsALoop": {
			diff: driftDiff, gen: 1, afterGen: 1,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  true,
			wantMessage: "RECONCILIATION LOOP DETECTED",
		},
		"UpdateEventDeltaIsALoop": {
			diff: nil, gen: 1, afterGen: 1,
			beforeEvents: 1, afterEvents: 3,
			afterReady:  true,
			wantMessage: "RECONCILIATION LOOP DETECTED",
		},
		"DriftAlongsideReadinessFlapStillReportsLoopVerbatim": {
			diff: driftDiff, gen: 1, afterGen: 1,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  false,
			wantMessage: "RECONCILIATION LOOP DETECTED",
		},
		"NothingWrongPasses": {
			diff: nil, gen: 1, afterGen: 1,
			beforeEvents: 0, afterEvents: 0,
			afterReady:  true,
			wantMessage: "resource stable (1 cycle observed, 0 updates)",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := buildConvergeResult(tc.diff, tc.gen, tc.afterGen, tc.beforeEvents, tc.afterEvents, tc.afterReady, nil)
			if result.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q (diagnostics: %v)", result.Message, tc.wantMessage, result.Diagnostics)
			}
		})
	}
}

// TestBuildConvergeResultReadinessTimeoutNoteNeverReplacesFieldDiagnostic is
// the direct proof for the AC that matters most here: a baseline
// readiness-pre-check note must be ADDED to the diagnostics, never
// SUBSTITUTED for the field-level atProvider diagnostic a genuine
// reconciliation loop would otherwise produce.
func TestBuildConvergeResultReadinessTimeoutNoteNeverReplacesFieldDiagnostic(t *testing.T) {
	diff := []differ.FieldChange{{Field: "someCounter", OldValue: "1", NewValue: "2"}}
	notes := []string{"readiness pre-check: Ready condition did not reach True within 1s before the baseline snapshot; proceeding with field-level diagnostics"}

	result := buildConvergeResult(diff, 1, 1, 0, 0, true, notes)
	if result.Passed {
		t.Fatalf("expected Passed=false: the atProvider diff is a real reconciliation-loop signal, got %+v", result)
	}
	if len(result.Diagnostics) != 2 {
		t.Fatalf("expected both the readiness note AND the field-level diagnostic, got %v", result.Diagnostics)
	}
	if !strings.Contains(result.Diagnostics[0], "readiness pre-check") {
		t.Errorf("Diagnostics[0] = %q, want the readiness note first", result.Diagnostics[0])
	}
	if !strings.Contains(result.Diagnostics[1], "atProvider changed") {
		t.Errorf("Diagnostics[1] = %q, want the atProvider diff diagnostic — it must never be dropped", result.Diagnostics[1])
	}
}

// TestBuildConvergeResultReadinessTimeoutNoteSurvivesAPass confirms the note
// is surfaced even when the run otherwise passes cleanly — an operator
// reading a passing result should still see that the baseline was taken
// before Ready was confirmed True.
func TestBuildConvergeResultReadinessTimeoutNoteSurvivesAPass(t *testing.T) {
	notes := []string{"readiness pre-check: Ready condition did not reach True within 1s before the baseline snapshot; proceeding with field-level diagnostics"}

	result := buildConvergeResult(nil, 1, 1, 0, 0, true, notes)
	if !result.Passed {
		t.Fatalf("expected Passed=true: nothing actually drifted, got %+v", result)
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0], "readiness pre-check") {
		t.Errorf("expected the readiness note to survive on a passing result, got %v", result.Diagnostics)
	}
}

// TestWaitReady covers the three mandatory readiness-gate paths named in
// this change's acceptance criteria: Ready immediately (the gate is a
// no-op), Ready after a dip (the resource settles mid-poll), and Ready
// never reached (the gate times out and reports ready=false with a nil
// error, so RunConverge can proceed rather than fail outright).
func TestWaitReady(t *testing.T) {
	t.Run("ReadyImmediately", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1}
		r := newFakeRunner(f)
		var slept int
		r.sleepFunc = func(time.Duration) { slept++ }

		ready, err := r.waitReady(time.Second)
		if err != nil {
			t.Fatalf("waitReady() error = %v", err)
		}
		if !ready {
			t.Fatal("waitReady() = false, want true — Ready on the very first read")
		}
		if slept != 0 {
			t.Errorf("waitReady() slept %d time(s), want 0 — the gate must be a no-op when already Ready", slept)
		}
		if f.getObjectCalls != 1 {
			t.Errorf("getObjectCalls = %d, want exactly 1", f.getObjectCalls)
		}
	})

	t.Run("ReadyAfterADip", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 3}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		ready, err := r.waitReady(time.Second)
		if err != nil {
			t.Fatalf("waitReady() error = %v", err)
		}
		if !ready {
			t.Fatal("waitReady() = false, want true — the resource settles before the timeout")
		}
		if f.getObjectCalls != 3 {
			t.Errorf("getObjectCalls = %d, want 3 (two NotReady reads, then True)", f.getObjectCalls)
		}
	})

	t.Run("NeverReady", func(t *testing.T) {
		f := &fakeCluster{generation: 1, neverReady: true}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		ready, err := r.waitReady(20 * time.Millisecond)
		if err != nil {
			t.Fatalf("waitReady() error = %v, want nil — a timeout is not itself a failure", err)
		}
		if ready {
			t.Fatal("waitReady() = true, want false — the Ready condition never reports True")
		}
		if f.getObjectCalls == 0 {
			t.Error("expected at least one poll before the timeout was recognised")
		}
	})
}

// TestRunConvergeReadinessGate exercises the readiness gate through
// RunConverge itself, end to end against the fake cluster — proving the
// gate is actually WIRED into RunConverge's baseline and final snapshots,
// not merely correct in isolation.
func TestRunConvergeReadinessGate(t *testing.T) {
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}

	t.Run("ReadyImmediatelyIsANoOp", func(t *testing.T) {
		f := &fakeCluster{
			generation:      1,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"zone": "a"},
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		result, err := r.RunConverge(m, ConvergeOptions{
			PollInterval:     time.Millisecond,
			ReadinessTimeout: time.Second,
			Timeout:          time.Second,
		})
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if !result.Passed {
			t.Fatalf("expected Passed=true, got %+v", result)
		}
		for _, d := range result.Diagnostics {
			if strings.Contains(d, "readiness pre-check") {
				t.Errorf("did not expect a readiness-timeout note when Ready from the start: %v", result.Diagnostics)
			}
		}
	})

	t.Run("TimeoutProceedsAndNotesButDoesNotReplaceFailure", func(t *testing.T) {
		f := &fakeCluster{
			generation: 1,
			neverReady: true,
			atProvider: map[string]interface{}{"zone": "a"},
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		result, err := r.RunConverge(m, ConvergeOptions{
			PollInterval:     time.Millisecond,
			ReadinessTimeout: 5 * time.Millisecond,
			Timeout:          time.Second,
		})
		if err != nil {
			t.Fatalf("RunConverge() error = %v, want nil — a readiness timeout must not abort the run", err)
		}
		// neverReady means Ready is also False at the final snapshot, so
		// this is a genuine readiness flap on top of the baseline timeout
		// note — RunConverge must surface BOTH, proving the timeout note
		// never displaces a real diagnostic.
		if result.Passed {
			t.Fatalf("expected Passed=false: Ready was never True, including at the final snapshot, got %+v", result)
		}
		var sawTimeoutNote, sawFlap bool
		for _, d := range result.Diagnostics {
			if strings.Contains(d, "readiness pre-check") {
				sawTimeoutNote = true
			}
			if strings.Contains(d, "readiness flap") {
				sawFlap = true
			}
		}
		if !sawTimeoutNote {
			t.Errorf("expected a readiness pre-check timeout note, got %v", result.Diagnostics)
		}
		if !sawFlap {
			t.Errorf("expected a readiness flap diagnostic for the final snapshot, got %v", result.Diagnostics)
		}
		if result.Message != "RESOURCE NOT IN STEADY STATE" {
			t.Errorf("Message = %q, want %q — nothing here is evidence of a reconciliation loop, only that Ready never went True", result.Message, "RESOURCE NOT IN STEADY STATE")
		}
	})
}

// TestRunConvergeGenerationNeverSettlesReportsSteadyStateNotLoop pins the
// headline for the pre-check timeout path (waitGenerationSettled never
// observing metadata.generation reach observedGeneration): this has always
// been reported as "RECONCILIATION LOOP DETECTED", but a generation that
// simply never settles is not evidence a reconciler is looping — nothing
// has been observed to repeat.
func TestRunConvergeGenerationNeverSettlesReportsSteadyStateNotLoop(t *testing.T) {
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	// No readyAfterCalls and neverReady left false means fakeCluster never
	// embeds a status.conditions entry at all, so extractObservedGeneration
	// never finds one to compare against — waitGenerationSettled spins
	// until its own timeout without ever settling.
	f := &fakeCluster{generation: 1}
	r := newFakeRunner(f)
	r.sleepFunc = func(time.Duration) {}

	result, err := r.RunConverge(m, ConvergeOptions{
		PollInterval: time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunConverge() error = %v, want nil", err)
	}
	if result.Passed {
		t.Fatalf("expected Passed=false: generation never settled, got %+v", result)
	}
	if result.Message != "RESOURCE NOT IN STEADY STATE" {
		t.Errorf("Message = %q, want %q", result.Message, "RESOURCE NOT IN STEADY STATE")
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0], "pre-check: generation") {
		t.Errorf("expected exactly one pre-check diagnostic naming the unsettled generation, got %v", result.Diagnostics)
	}
}
