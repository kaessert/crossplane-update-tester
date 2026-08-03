package runner

import (
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
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
			result := buildConvergeResult(nil, 1, 1, tc.beforeEvents, tc.afterEvents)
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

	result := buildConvergeResult(diff, 1, 1, 0, 0)
	if result.Passed {
		t.Fatalf("expected Passed=false when atProvider drifted, got %+v", result)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected a diagnostic naming the drifted field")
	}
}
