package runner

import (
	"errors"
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

	// testNamespaceExample is the namespace a NAMESPACED variant of
	// testKindExample/testNameExample lives in. The cluster-scoped variant
	// uses the empty string.
	testNamespaceExample = "default"
	// testNamespaceOther is a SECOND non-empty namespace, distinct from
	// testNamespaceExample, for tests that must prove namespace equality is
	// checked exactly rather than merely by "is it empty or not" — two
	// namespaced resources of the same Kind+Name in different namespaces
	// share neither an empty-vs-non-empty split nor an apiVersion group
	// difference, so only exact namespace comparison tells them apart.
	testNamespaceOther = "other"
	// testAPIVersionClusterScoped and testAPIVersionNamespaced are the two
	// apiVersion strings a dual-scope provider's unified example manifests
	// carry for the SAME Kind+Name — differing only in API group, per the
	// namespaced-group naming convention. They stand in for any provider's
	// real groups; the matching logic under test is group-string-agnostic.
	testAPIVersionClusterScoped = "example.crossplane.io/v1alpha1"
	testAPIVersionNamespaced    = "example.m.crossplane.io/v1alpha1"
)

// testReconcileLogLine is a benign structured controller log line — the
// reconciler announcing a reconcile, not an Update() call. It is the
// fakeCluster's default `kubectl logs` output, standing for a controller
// that is demonstrably alive and logging while making no backend writes.
const testReconcileLogLine = `2026-01-01T00:00:00Z	DEBUG	provider-example	Reconciling	{"controller": "exampleresource.cluster", "request": {"name":"example-resource"}}`

// newTestUpdateLogLine builds the controller log line the managed reconciler
// writes on a successful Update(), for the given reconcile request. An empty
// namespace produces the cluster-scoped shape, where the request carries no
// namespace key at all — which is exactly how a cluster-scoped resource is
// told apart from a namespaced sibling sharing its Kind and Name.
func newTestUpdateLogLine(name, namespace string) string {
	req := `{"name":"` + name + `"}`
	if namespace != "" {
		req = `{"name":"` + name + `","namespace":"` + namespace + `"}`
	}
	return "2026-01-01T00:00:00Z\tDEBUG\tprovider-example\t" + logMsgUpdated +
		"\t{\"controller\": \"exampleresource\", \"request\": " + req + `, "version": "1"}`
}

// errTestLogUnavailable stands for any reason `kubectl logs` could not be
// read — RBAC, a pod that has gone away, an API server hiccup.
var errTestLogUnavailable = errors.New("logs forbidden")

// quietLogObservation is a live-but-quiet controller-log observation: the
// instrument ran, saw the controller logging, and attributed no Update()
// call to the resource under test. It is what every buildConvergeResult test
// that is not itself exercising the log instrument means by "the log said
// nothing was wrong".
func quietLogObservation() updateLogObservation {
	return updateLogObservation{Lines: 1, Window: time.Second}
}

// newTestEventItem builds a CLUSTER-SCOPED eventItem for the given reason,
// aggregated count, kind, and name — reducing repetition of the anonymous
// InvolvedObject struct literal across test cases. Namespace is left empty
// and APIVersion is left unset, matching every existing test's fixtures
// (none of which exercise namespace/apiVersion scoping).
func newTestEventItem(reason string, count int32, kind, name string) eventItem {
	e := eventItem{Reason: reason, Count: count}
	e.InvolvedObject.Kind = kind
	e.InvolvedObject.Name = name
	return e
}

// newTestEventItemScoped builds an eventItem carrying an explicit namespace
// and apiVersion, for tests that exercise dual-scope event attribution — a
// cluster-scoped and a namespaced resource sharing the same Kind and Name.
func newTestEventItemScoped(reason string, count int32, kind, name, namespace, apiVersion string) eventItem {
	e := newTestEventItem(reason, count, kind, name)
	e.InvolvedObject.Namespace = namespace
	e.InvolvedObject.APIVersion = apiVersion
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
			got := sumEventOccurrences(tc.list, tc.kind, tc.name, "", "")
			if got != tc.want {
				t.Errorf("sumEventOccurrences() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSumEventOccurrencesByReasonScopesByNamespaceAndAPIVersion is the
// direct proof for this change's acceptance criteria: a cluster-scoped and
// a namespaced resource sharing the same Kind and Name — the unified
// example-manifest convention every dual-scope provider follows — must each
// see only their own events, never the other's. A cluster-scoped resource's
// namespace argument ("") must match ONLY events whose involvedObject
// itself carries an empty namespace, not events from any namespace.
func TestSumEventOccurrencesByReasonScopesByNamespaceAndAPIVersion(t *testing.T) {
	// One fixture list containing BOTH variants of the same Kind+Name: a
	// cluster-scoped Item (empty namespace, cluster-scoped apiVersion) and
	// a namespaced Item (non-empty namespace, namespaced apiVersion) — the
	// exact scenario a unified dual-scope example manifest produces.
	list := eventList{Items: []eventItem{
		newTestEventItemScoped(eventReasonUpdated, 3, testKindExample, testNameExample, "", testAPIVersionClusterScoped),
		newTestEventItemScoped(eventReasonUpdated, 5, testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced),
	}}

	t.Run("ClusterScopedSeesOnlyItsOwnEvents", func(t *testing.T) {
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, "", testAPIVersionClusterScoped, eventReasonUpdated)
		if got != 3 {
			t.Errorf("cluster-scoped count = %d, want 3 (must not include the namespaced variant's 5)", got)
		}
	})

	t.Run("NamespacedSeesOnlyItsOwnEvents", func(t *testing.T) {
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced, eventReasonUpdated)
		if got != 5 {
			t.Errorf("namespaced count = %d, want 5 (must not include the cluster-scoped variant's 3)", got)
		}
	})

	t.Run("ClusterScopedNamespaceArgumentDoesNotMatchAnyNamespace", func(t *testing.T) {
		// A regression-proof for the specific failure mode named in the
		// AC: passing "" must mean "match empty namespace only", not "match
		// every namespace" — asserted here by confirming the cluster-scoped
		// query never picks up the namespaced Item's count.
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, "", testAPIVersionClusterScoped, eventReasonUpdated)
		if got == 3+5 {
			t.Fatal("cluster-scoped query summed both variants — \"\" is being treated as \"any namespace\"")
		}
	})
}

// TestSumEventOccurrencesByReasonEmptyAPIVersionIsTreatedAsUnknownGroup is
// the direct proof for the stated empty-apiVersion policy: an event whose
// involvedObject carries no apiVersion at all (an event source that never
// populates it — the exact defect this ticket traces to) must still be
// counted, not silently dropped to zero. Namespace alone already scopes the
// match to one resource here, so admitting an unknown group cannot bleed in
// a sibling's events.
func TestSumEventOccurrencesByReasonEmptyAPIVersionIsTreatedAsUnknownGroup(t *testing.T) {
	list := eventList{Items: []eventItem{
		// No InvolvedObject.APIVersion set at all — mirrors a fake or real
		// event source that never populates the field.
		newTestEventItem(eventReasonUpdated, 4, testKindExample, testNameExample),
	}}

	got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, "", testAPIVersionClusterScoped, eventReasonUpdated)
	if got != 4 {
		t.Errorf("got %d, want 4 — an empty involvedObject.apiVersion must not zero out a genuinely matching event", got)
	}
}

// TestSumEventOccurrencesByReasonToleratesVersionSkewWithinSameGroup proves
// the matcher compares apiVersion by GROUP only: a served object reporting a
// newer or older version than the manifest declares — the ordinary result of
// a CRD version bump or a conversion webhook — must still be counted as long
// as the group (and namespace) agree.
func TestSumEventOccurrencesByReasonToleratesVersionSkewWithinSameGroup(t *testing.T) {
	const (
		manifestAPIVersion = "example.crossplane.io/v1alpha1"
		servedAPIVersion   = "example.crossplane.io/v1beta1" // same group, different version
	)
	list := eventList{Items: []eventItem{
		newTestEventItemScoped(eventReasonUpdated, 2, testKindExample, testNameExample, "", servedAPIVersion),
	}}

	got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, "", manifestAPIVersion, eventReasonUpdated)
	if got != 2 {
		t.Errorf("got %d, want 2 — a version skew within the same group must not zero the count", got)
	}
}

// TestSumEventOccurrencesByReasonGroupMismatchStillExcludes confirms the
// group check still does real work: two DIFFERENT groups, same namespace
// (a scenario the namespace check alone cannot catch), must not be summed
// together.
func TestSumEventOccurrencesByReasonGroupMismatchStillExcludes(t *testing.T) {
	list := eventList{Items: []eventItem{
		newTestEventItemScoped(eventReasonUpdated, 9, testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced),
	}}

	got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, testNamespaceExample, testAPIVersionClusterScoped, eventReasonUpdated)
	if got != 0 {
		t.Errorf("got %d, want 0 — a genuinely different group, same namespace, must not match", got)
	}
}

// TestSumEventOccurrencesByReasonNamespaceAloneDiscriminatesSameGroup proves
// the namespace check is independently load-bearing, not merely redundant
// with the apiVersion-group check. Every other fixture in this file that
// puts two variants of the same Kind+Name in one list varies the apiVersion
// GROUP alongside the namespace, so the group check alone is sufficient to
// pass those and the namespace check is never the thing actually under
// test. Here all three events share Kind, Name AND apiVersion group, and
// differ ONLY in involvedObject.namespace — one cluster-scoped (""), and
// TWO namespaced, in two DIFFERENT non-empty namespaces — so a query must be
// attributed to its own namespace on the namespace check alone.
//
// The two namespaced events additionally close the gap a cluster-scoped vs.
// namespaced pair leaves open: "" vs. a non-empty namespace also differs in
// namespacedness, so a matcher that only compared namespacedness (empty vs.
// non-empty) rather than exact namespace equality would still pass that
// pair. Two DIFFERENT non-empty namespaces share no such shortcut — only
// exact equality tells them apart.
func TestSumEventOccurrencesByReasonNamespaceAloneDiscriminatesSameGroup(t *testing.T) {
	list := eventList{Items: []eventItem{
		newTestEventItemScoped(eventReasonUpdated, 6, testKindExample, testNameExample, "", testAPIVersionNamespaced),
		newTestEventItemScoped(eventReasonUpdated, 2, testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced),
		newTestEventItemScoped(eventReasonUpdated, 9, testKindExample, testNameExample, testNamespaceOther, testAPIVersionNamespaced),
	}}

	t.Run("ClusterScopedQuerySeesOnlyItsOwnEvents", func(t *testing.T) {
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, "", testAPIVersionNamespaced, eventReasonUpdated)
		if got != 6 {
			t.Errorf("cluster-scoped count = %d, want 6 (must not include the namespaced items' 2 or 9 — same group, so only namespace tells them apart)", got)
		}
	})

	t.Run("NamespacedQuerySeesOnlyItsOwnEvents", func(t *testing.T) {
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced, eventReasonUpdated)
		if got != 2 {
			t.Errorf("namespaced count = %d, want 2 (must not include the cluster-scoped item's 6 or the other namespace's 9 — same group, so only namespace tells them apart)", got)
		}
	})

	t.Run("SecondNamespacedQuerySeesOnlyItsOwnEvents", func(t *testing.T) {
		// The direct proof this test exists for: two NAMESPACED resources
		// of the same Kind+Name+group in different namespaces must not
		// cross-count each other. Neither is empty, so a matcher that only
		// compared namespacedness (empty vs. non-empty) rather than exact
		// namespace equality would wrongly sum this with the other
		// namespace's 2 as well as its own 9.
		got := sumEventOccurrencesByReason(list, testKindExample, testNameExample, testNamespaceOther, testAPIVersionNamespaced, eventReasonUpdated)
		if got != 9 {
			t.Errorf("namespaced count = %d, want 9 (must not include the cluster-scoped item's 6 or the other namespace's 2)", got)
		}
	})
}

// TestApiGroup covers the group-extraction helper directly: the split point,
// a core-group (no slash) value, and the empty string.
func TestApiGroup(t *testing.T) {
	cases := map[string]struct {
		apiVersion string
		want       string
	}{
		"GroupAndVersion":     {apiVersion: "example.crossplane.io/v1alpha1", want: "example.crossplane.io"},
		"CoreGroupNoSlash":    {apiVersion: "v1", want: ""},
		"Empty":               {apiVersion: "", want: ""},
		"MultipleSlashesKeep": {apiVersion: "example.crossplane.io/v1alpha1/extra", want: "example.crossplane.io"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := apiGroup(tc.apiVersion)
			if got != tc.want {
				t.Errorf("apiGroup(%q) = %q, want %q", tc.apiVersion, got, tc.want)
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
			result := buildConvergeResult(nil, 1, 1, tc.beforeEvents, tc.afterEvents, true, quietLogObservation(), nil)
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

	result := buildConvergeResult(diff, 1, 1, 0, 0, true, quietLogObservation(), nil)
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
	result := buildConvergeResult(nil, 1, 1, 0, 0, false, quietLogObservation(), nil)
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
			result := buildConvergeResult(tc.diff, tc.gen, tc.afterGen, tc.beforeEvents, tc.afterEvents, tc.afterReady, quietLogObservation(), nil)
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

	result := buildConvergeResult(diff, 1, 1, 0, 0, true, quietLogObservation(), notes)
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

	result := buildConvergeResult(nil, 1, 1, 0, 0, true, quietLogObservation(), notes)
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

// TestWaitSynced covers waitSynced's three outcome paths, mirroring
// TestWaitReady's shape: an absent Synced condition is treated as "not
// applicable" and never blocks; a conflict-then-settle sequence (a late-init
// 409 retry, then the bump, then the genuine settle — see
// syncedConflictReads) is waited through to completion; and a Synced
// condition that never reaches True times out reporting synced=false with a
// nil error, exactly like waitReady's own timeout path, so the caller
// decides what a timeout here means rather than getting a bare error for a
// resource that is merely still catching up.
func TestWaitSynced(t *testing.T) {
	t.Run("AbsentConditionIsNotApplicable", func(t *testing.T) {
		f := &fakeCluster{generation: 1}
		r := newFakeRunner(f)
		var slept int
		r.sleepFunc = func(time.Duration) { slept++ }

		synced, status, gen, obsGen, err := r.waitSynced(time.Second)
		if err != nil {
			t.Fatalf("waitSynced() error = %v", err)
		}
		if !synced {
			t.Fatal("waitSynced() synced = false, want true — a reconciler that never emits Synced must not block")
		}
		if status != "" || obsGen != 0 {
			t.Errorf("status=%q obsGen=%d, want zero values when no Synced condition exists", status, obsGen)
		}
		if gen != 1 {
			t.Errorf("gen = %d, want 1", gen)
		}
		if slept != 0 {
			t.Errorf("waitSynced() slept %d time(s), want 0 — absence is resolved on the first read", slept)
		}
		if f.getObjectCalls != 1 {
			t.Errorf("getObjectCalls = %d, want exactly 1", f.getObjectCalls)
		}
	})

	t.Run("SettlesAfterConflictAndLateInitBump", func(t *testing.T) {
		f := &fakeCluster{generation: 1, syncedConflictReads: 1}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		synced, status, gen, obsGen, err := r.waitSynced(time.Second)
		if err != nil {
			t.Fatalf("waitSynced() error = %v", err)
		}
		if !synced {
			t.Fatalf("waitSynced() synced = false, want true — the resource settles before the timeout")
		}
		if status != "True" {
			t.Errorf("status = %q, want %q", status, "True")
		}
		if gen != 2 || obsGen != 2 {
			t.Errorf("gen=%d obsGen=%d, want both 2 — the late-init bump plus the final settle", gen, obsGen)
		}
		if f.getObjectCalls != 3 {
			t.Errorf("getObjectCalls = %d, want 3 (conflict, stale-bump, settle)", f.getObjectCalls)
		}
	})

	t.Run("NeverSyncedTimesOutWithoutError", func(t *testing.T) {
		f := &fakeCluster{generation: 1, neverSynced: true}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		synced, status, gen, _, err := r.waitSynced(20 * time.Millisecond)
		if err != nil {
			t.Fatalf("waitSynced() error = %v, want nil — a timeout is not itself a failure", err)
		}
		if synced {
			t.Fatal("waitSynced() synced = true, want false — the Synced condition never reports True")
		}
		if status != "False" {
			t.Errorf("status = %q, want %q", status, "False")
		}
		if gen != 1 {
			t.Errorf("gen = %d, want 1", gen)
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

// TestRunConvergePreCheckWaitsThroughLateInitConflictBeforeBaseline proves
// waitGenerationSettled ALONE has the exact blind spot this ticket is
// about: a reconcile that FAILS to persist a write (a late-init 409
// conflict) still stamps the Synced condition it marks False
// (ReconcileError) with observedGeneration == the CURRENT generation, so
// waitGenerationSettled reports "settled" after the very FIRST read here —
// before the late-init write that eventually succeeds has even happened,
// let alone the genuine settle afterward. Without the additional waitSynced
// call in RunConverge's pre-check, the baseline snapshot below would be
// taken at that premature, unsettled instant.
//
// syncedConflictReads is set high enough (3) that the premature-settle read
// (read 1, Synced False at the STARTING generation) is a real, distinct
// event from the eventual genuine settle a few reads later — proving the
// pre-check actually waited through the gap rather than the two coinciding
// by construction.
func TestRunConvergePreCheckWaitsThroughLateInitConflictBeforeBaseline(t *testing.T) {
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	f := &fakeCluster{
		generation:          1,
		atProvider:          map[string]interface{}{"zone": "a"},
		readyAfterCalls:     1,
		syncedConflictReads: 3,
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
		t.Fatalf("expected Passed=true once the pre-check has waited through the late-init conflict, bump, and genuine settle, got %+v", result)
	}
	if result.Message != "resource stable (1 cycle observed, 0 updates)" {
		t.Errorf("Message = %q, want the stable-resource message", result.Message)
	}
	// The simulated late-init write bumps the generation by one EXTRA step
	// (1 -> 2). If RunConverge had proceeded straight from
	// waitGenerationSettled's premature "settled" verdict (read 1), the
	// bump — and the read that observes it — would never have happened
	// before the baseline snapshot, and this would still read 1.
	if f.generation != 2 {
		t.Errorf("generation = %d, want 2 — the pre-check must wait through the late-init bump before RunConverge takes its baseline snapshot", f.generation)
	}
}

// TestRunConvergeIgnoresSiblingScopeEvents is the end-to-end proof that
// RunConverge's update-event delta is scoped by namespace and apiVersion,
// not just Kind+Name: a cluster-scoped resource and a namespaced resource
// that share a Kind and Name — the unified example-manifest convention
// every dual-scope provider follows — are tested here as siblings, with the
// SIBLING actively emitting new update events across the baseline/outcome
// snapshots while the resource under test emits none. Before events were
// scoped by namespace/apiVersion, the sibling's growing count bled into
// this delta and produced a false "RECONCILIATION LOOP DETECTED" for a
// resource that never actually changed.
func TestRunConvergeIgnoresSiblingScopeEvents(t *testing.T) {
	// The resource under test is the NAMESPACED variant.
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		Namespace:  testNamespaceExample,
		APIVersion: testAPIVersionNamespaced,
	}
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"zone": "a"},
		kind:            testKindExample,
		name:            testNameExample,
		namespace:       testNamespaceExample,
		apiVersion:      testAPIVersionNamespaced,
		// The resource under test never emits an update event of its own.
		generations: []int32{0},
		// The CLUSTER-SCOPED sibling — same Kind+Name, empty namespace,
		// the cluster-scoped apiVersion — starts at 1 occurrence and gains
		// 5 more between the baseline and outcome reads, simulating an
		// unrelated resource that is genuinely looping at the same time.
		siblingKind:               testKindExample,
		siblingName:               testNameExample,
		siblingNamespace:          "",
		siblingAPIVersion:         testAPIVersionClusterScoped,
		siblingEventBase:          1,
		siblingEventGrowthPerCall: 5,
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
		t.Fatalf("expected Passed=true: the sibling's growing event count must not be attributed to this resource, got %+v", result)
	}
	if result.Message != "resource stable (1 cycle observed, 0 updates)" {
		t.Errorf("Message = %q, want the stable-resource message with 0 updates — the sibling's 5 new events must not be counted", result.Message)
	}
}

// TestCountUpdateLogLinesInAttributesByReconcileRequest pins the attribution
// rule: an Update() log line belongs to the resource whose reconcile request
// it names, matched on name AND namespace. The fixture is the case that
// motivated parsing over substring matching — a cluster-scoped resource and a
// namespaced sibling sharing one Kind and one Name, as the unified
// example-manifest convention allows.
func TestCountUpdateLogLinesInAttributesByReconcileRequest(t *testing.T) {
	out := strings.Join([]string{
		testReconcileLogLine,
		newTestUpdateLogLine(testNameExample, ""),                   // cluster-scoped
		newTestUpdateLogLine(testNameExample, testNamespaceExample), // namespaced sibling
		newTestUpdateLogLine(testNameExample, testNamespaceExample),
		newTestUpdateLogLine(testNameOtherInstance, ""), // a different resource
	}, "\n")

	tests := []struct {
		name      string
		resource  string
		namespace string
		wantCalls int
	}{
		{"cluster-scoped resource counts only its own namespace-less request", testNameExample, "", 1},
		{"namespaced sibling counts only its own namespaced requests", testNameExample, testNamespaceExample, 2},
		{"a namespace it does not live in counts nothing", testNameExample, testNamespaceOther, 0},
		{"a resource that made no calls counts nothing", testNameOther, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls, lines := countUpdateLogLinesIn(out, tc.resource, tc.namespace)
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if lines != 5 {
				t.Errorf("lines = %d, want 5 — every line counts toward liveness, whatever it says", lines)
			}
		})
	}
}

// TestCountUpdateLogLinesInLivenessIsNotZeroCalls pins the distinction the
// whole instrument rests on: no output at all means the instrument saw
// nothing (a provider not running with --debug), which is a different fact
// from a live controller that made zero Update() calls.
func TestCountUpdateLogLinesInLivenessIsNotZeroCalls(t *testing.T) {
	calls, lines := countUpdateLogLinesIn("", testNameExample, "")
	if calls != 0 || lines != 0 {
		t.Fatalf("empty output: calls, lines = %d, %d; want 0, 0", calls, lines)
	}
	calls, lines = countUpdateLogLinesIn(testReconcileLogLine, testNameExample, "")
	if calls != 0 || lines != 1 {
		t.Fatalf("live but quiet: calls, lines = %d, %d; want 0, 1", calls, lines)
	}
}

// TestCountUpdateLogLinesInIgnoresUnattributableLines proves a line that
// carries an Update() message but no usable reconcile request is dropped
// rather than guessed at — it must not be credited to the resource under
// test just because it is the resource being checked.
func TestCountUpdateLogLinesInIgnoresUnattributableLines(t *testing.T) {
	out := strings.Join([]string{
		"plain text " + logMsgUpdated,                       // no JSON payload at all
		logMsgUpdated + `\t{"controller": "x"}`,             // payload without a request
		logMsgUpdated + `\t{"request": {"name":""}}`,        // request without a name
		logMsgUpdated + `\t{"request": {"name": not-json}}`, // malformed payload
	}, "\n")
	calls, lines := countUpdateLogLinesIn(out, testNameExample, "")
	if calls != 0 {
		t.Errorf("calls = %d, want 0 — an unattributable line must not be credited to any resource", calls)
	}
	if lines != 4 {
		t.Errorf("lines = %d, want 4", lines)
	}
}

// TestBuildConvergeResultLogInstrumentCatchesWhatEventsMiss is the measured
// case this instrument was added for: a resource calling Update() on every
// poll tick, whose aggregated event count did not move because client-go's
// rate limiter had not flushed inside the window. Live measurement on a
// 10s-poll provider: the event delta detected this in none of six windows,
// the log in all of them.
func TestBuildConvergeResultLogInstrumentCatchesWhatEventsMiss(t *testing.T) {
	logObs := updateLogObservation{Calls: 2, Lines: 40, Window: 15 * time.Second}
	// No atProvider drift, no event delta, generation unchanged, Ready true:
	// every pre-existing signal says stable.
	result := buildConvergeResult(nil, 7, 7, 100, 100, true, logObs, nil)

	if result.Passed {
		t.Fatalf("expected Passed=false: 2 Update() calls in the log is a loop however quiet the event channel was, got %+v", result)
	}
	if result.Message != "RECONCILIATION LOOP DETECTED" {
		t.Errorf("Message = %q, want the verbatim loop headline operators grep for", result.Message)
	}
	if !strings.Contains(strings.Join(result.Diagnostics, "\n"), "2 Update() call(s) in the controller log") {
		t.Errorf("Diagnostics = %v, want the Update() call count named", result.Diagnostics)
	}
}

// TestBuildConvergeResultLogInstrumentSilenceIsReported covers the two ways
// the log instrument can fail to observe. Neither may be reported as a clean
// pass without saying so: a silently blind instrument is the exact failure
// this check exists to end.
func TestBuildConvergeResultLogInstrumentSilenceIsReported(t *testing.T) {
	tests := []struct {
		name   string
		logObs updateLogObservation
		want   string
	}{
		{
			name:   "unreadable log names the fallback and its weakness",
			logObs: updateLogObservation{Err: errTestLogUnavailable, Window: 15 * time.Second},
			want:   "controller-log instrument unavailable",
		},
		{
			name:   "empty window is observed-nothing, not zero calls",
			logObs: updateLogObservation{Lines: 0, Window: 15 * time.Second},
			want:   "observed nothing rather than observing zero Update() calls",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := buildConvergeResult(nil, 1, 1, 0, 0, true, tc.logObs, nil)
			if !result.Passed {
				t.Errorf("Passed = false: an unobservable instrument is not itself evidence against the resource, got %+v", result)
			}
			if !strings.Contains(strings.Join(result.Diagnostics, "\n"), tc.want) {
				t.Errorf("Diagnostics = %v, want one containing %q", result.Diagnostics, tc.want)
			}
		})
	}
}

// TestRunConvergeDetectsLoopFromControllerLogAlone drives the whole check
// end to end against a fake cluster whose resource is stable in every way
// the old instruments could see, and which is calling Update() on every
// tick. Before the log instrument this run reported "resource stable".
func TestRunConvergeDetectsLoopFromControllerLogAlone(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		APIVersion: testAPIVersionClusterScoped,
	}
	f := &fakeCluster{
		generation:      3,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"zone": "a"},
		kind:            testKindExample,
		name:            testNameExample,
		apiVersion:      testAPIVersionClusterScoped,
		// The event channel never moves — the rate limiter has not flushed.
		generations: []int32{0},
		logLines: strings.Join([]string{
			testReconcileLogLine,
			newTestUpdateLogLine(testNameExample, ""),
			newTestUpdateLogLine(testNameExample, ""),
		}, "\n"),
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
	if result.Passed {
		t.Fatalf("expected Passed=false, got %+v", result)
	}
	if result.Message != "RECONCILIATION LOOP DETECTED" {
		t.Errorf("Message = %q, want RECONCILIATION LOOP DETECTED", result.Message)
	}
}

// TestRunConvergeIgnoresSiblingScopeLogLines is the log-side counterpart of
// TestRunConvergeIgnoresSiblingScopeEvents: the namespaced variant is the
// resource under test, its cluster-scoped sibling shares Kind and Name and is
// genuinely looping, and that must not be attributed here.
func TestRunConvergeIgnoresSiblingScopeLogLines(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		Namespace:  testNamespaceExample,
		APIVersion: testAPIVersionNamespaced,
	}
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"zone": "a"},
		kind:            testKindExample,
		name:            testNameExample,
		namespace:       testNamespaceExample,
		apiVersion:      testAPIVersionNamespaced,
		generations:     []int32{0},
		logLines: strings.Join([]string{
			testReconcileLogLine,
			// The CLUSTER-SCOPED sibling loops; the resource under test does not.
			newTestUpdateLogLine(testNameExample, ""),
			newTestUpdateLogLine(testNameExample, ""),
		}, "\n"),
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
		t.Fatalf("expected Passed=true: the sibling scope's Update() calls must not be attributed to this resource, got %+v", result)
	}
}
