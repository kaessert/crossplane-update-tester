package runner

import (
	"errors"
	"strconv"
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

// testLogLineTimestamp is the RFC3339 timestamp every DEFAULT
// controller-log fixture in this file below carries — testReconcileLogLine
// and every newTestUpdateLogLine call. It is computed once, at package
// init, as an hour past whatever the real wall clock reads at that moment.
// Package init always runs before any individual test's own convergeArm
// captures its own ArmedAt (see countUpdateLogLinesIn's anchor), and no
// unit test in this package runs for anywhere near an hour, so this
// timestamp is guaranteed to land after every ArmedAt the suite ever
// records — regardless of what calendar date the suite happens to run on.
// A test that specifically exercises the anchor (a line that must land
// BEFORE a given ArmedAt) builds its own timestamp instead; see
// TestRunConvergeIgnoresOwnPreArmRenameLogLine.
var testLogLineTimestamp = time.Now().Add(time.Hour).Format(time.RFC3339)

// testReconcileLogLine is a benign structured controller log line — the
// reconciler announcing a reconcile, not an Update() call. It is the
// fakeCluster's default `kubectl logs` output, standing for a controller
// that is demonstrably alive and logging while making no backend writes.
var testReconcileLogLine = testLogLineTimestamp + `	DEBUG	provider-example	Reconciling	{"controller": "exampleresource.cluster", "request": {"name":"example-resource"}}`

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
	return testLogLineTimestamp + "\tDEBUG\tprovider-example\t" + logMsgUpdated +
		"\t{\"controller\": \"exampleresource\", \"request\": " + req + `, "version": "1"}`
}

// newTestUpdateLogLineAt is newTestUpdateLogLine with an explicit timestamp
// — for a test that must place the line at a precise instant relative to a
// baseline's ArmedAt, rather than at the suite-wide default future offset
// testLogLineTimestamp carries.
func newTestUpdateLogLineAt(ts time.Time, name, namespace string) string {
	req := `{"name":"` + name + `"}`
	if namespace != "" {
		req = `{"name":"` + name + `","namespace":"` + namespace + `"}`
	}
	return ts.Format(time.RFC3339) + "\tDEBUG\tprovider-example\t" + logMsgUpdated +
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

// TestConditionsAllTrue covers conditionsAllTrue's AND-over-declared-types
// semantics — the primitive recordConvergeOutcome uses to judge readiness
// against a manifest's declared uptest.upbound.io/conditions override
// instead of a hardcoded "Ready". Every declared type must be present AND
// "True"; one missing, or one present with any other status, fails the
// whole check.
func TestConditionsAllTrue(t *testing.T) {
	readyTrue := map[string]interface{}{"type": "Ready", "status": "True"}
	readyFalse := map[string]interface{}{"type": "Ready", "status": "False"}
	syncedTrue := map[string]interface{}{"type": "Synced", "status": "True"}
	syncedFalse := map[string]interface{}{"type": "Synced", "status": "False"}

	cases := map[string]struct {
		reason string
		obj    map[string]interface{}
		types  []string
		want   bool
	}{
		"SingleDeclaredTypeTrue": {
			reason: "the default, single-condition case behind every manifest that never declares an override",
			obj:    objWithConditions(readyTrue),
			types:  []string{"Ready"},
			want:   true,
		},
		"SingleDeclaredTypeFalse": {
			obj:   objWithConditions(readyFalse),
			types: []string{"Ready"},
			want:  false,
		},
		"DeclaredTypeAbsentFails": {
			reason: "a resource that has never reported the declared condition at all is not ready, not an error",
			obj:    objWithConditions(syncedTrue),
			types:  []string{"Ready"},
			want:   false,
		},
		"DeclaredNonReadyConditionTrueDespiteReadyFalse": {
			reason: "the exact CodeBaseIntegration shape: Ready is permanently False, but the manifest declares Synced as its ready condition and Synced is True",
			obj:    objWithConditions(readyFalse, syncedTrue),
			types:  []string{"Synced"},
			want:   true,
		},
		"MultiValueDeclarationRequiresAllTrue": {
			obj:   objWithConditions(readyTrue, syncedTrue),
			types: []string{"Ready", "Synced"},
			want:  true,
		},
		"MultiValueDeclarationOneFalseFailsTheWhole": {
			obj:   objWithConditions(readyTrue, syncedFalse),
			types: []string{"Ready", "Synced"},
			want:  false,
		},
		"NoConditionsAtAll": {
			reason: "a resource that has not been reconciled yet reports not-ready, not an error",
			obj:    map[string]interface{}{jsonKeyStatus: map[string]interface{}{}},
			types:  []string{"Ready"},
			want:   false,
		},
		"MissingStatus": {
			obj:   map[string]interface{}{},
			types: []string{"Ready"},
			want:  false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := conditionsAllTrue(tc.obj, tc.types)
			if got != tc.want {
				reason := tc.reason
				if reason == "" {
					reason = name
				}
				t.Errorf("%s: conditionsAllTrue() = %v, want %v", reason, got, tc.want)
			}
		})
	}
}

// objWithConditions builds a decoded resource object (the shape
// conditionsAllTrue/namedCondition read) carrying exactly the given
// status.conditions entries, in order.
func objWithConditions(conds ...map[string]interface{}) map[string]interface{} {
	raw := make([]interface{}, len(conds))
	for i, c := range conds {
		raw[i] = c
	}
	return map[string]interface{}{
		jsonKeyStatus: map[string]interface{}{
			"conditions": raw,
		},
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

// TestRunConvergeHonoursDeclaredReadyCondition is this ticket's central
// falsifiability proof, exercised end to end through RunConverge against
// the fake cluster rather than against buildConvergeResult in isolation —
// proving the override is actually WIRED all the way from the manifest's
// annotation-derived field through to the flap verdict.
//
// Both subtests share the IDENTICAL fake-cluster shape — permanently
// unready (neverReady), permanently synced (alwaysSynced), a stable
// atProvider snapshot — so the only variable between them is the
// manifest's own ReadyConditions declaration. That isolates the effect to
// the declaration itself, exactly matching the live reproducer measured on
// provider-f5xc's CodeBaseIntegration: Synced settles, Ready never does,
// and that is the resource's documented, intended steady state.
func TestRunConvergeHonoursDeclaredReadyCondition(t *testing.T) {
	opts := ConvergeOptions{
		PollInterval:     time.Millisecond,
		ReadinessTimeout: 5 * time.Millisecond,
		Timeout:          time.Second,
	}
	newFakeAlwaysSyncedNeverReady := func() *fakeCluster {
		return &fakeCluster{
			generation:   1,
			neverReady:   true,
			alwaysSynced: true,
			atProvider:   map[string]interface{}{"zone": "a"},
		}
	}

	t.Run("DeclaredNonReadyConditionPasses", func(t *testing.T) {
		m := &manifest.Manifest{
			Kind: testKindExample, Name: testNameExample,
			ReadyConditions: []string{"Synced"},
		}
		f := newFakeAlwaysSyncedNeverReady()
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		result, err := r.RunConverge(m, opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if !result.Passed {
			t.Fatalf("expected Passed=true: the manifest declares %q as its ready condition and it is True throughout, got %+v", "Synced", result)
		}
		for _, d := range result.Diagnostics {
			if strings.Contains(d, "readiness flap") {
				t.Errorf("did not expect a readiness flap: Ready is permanently False, but the manifest never declared Ready as its condition, got %v", result.Diagnostics)
			}
		}
	})

	t.Run("UndeclaredConditionStillFailsOnTheSamePermanentlyFalseReady", func(t *testing.T) {
		// No ReadyConditions declared — EffectiveReadyConditions falls
		// back to the "Ready" default, exactly as every manifest was
		// judged before this override existed.
		m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
		f := newFakeAlwaysSyncedNeverReady()
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}

		result, err := r.RunConverge(m, opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if result.Passed {
			t.Fatalf("expected Passed=false: no override is declared, the default \"Ready\" condition governs, and Ready is permanently False, got %+v", result)
		}
		var sawFlap bool
		for _, d := range result.Diagnostics {
			if strings.Contains(d, "readiness flap") {
				sawFlap = true
			}
		}
		if !sawFlap {
			t.Errorf("expected a readiness flap diagnostic, got %v", result.Diagnostics)
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
			calls, lines := countUpdateLogLinesIn(out, tc.resource, tc.namespace, time.Time{})
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
	calls, lines := countUpdateLogLinesIn("", testNameExample, "", time.Time{})
	if calls != 0 || lines != 0 {
		t.Fatalf("empty output: calls, lines = %d, %d; want 0, 0", calls, lines)
	}
	calls, lines = countUpdateLogLinesIn(testReconcileLogLine, testNameExample, "", time.Time{})
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
	calls, lines := countUpdateLogLinesIn(out, testNameExample, "", time.Time{})
	if calls != 0 {
		t.Errorf("calls = %d, want 0 — an unattributable line must not be credited to any resource", calls)
	}
	if lines != 4 {
		t.Errorf("lines = %d, want 4", lines)
	}
}

// TestCountUpdateLogLinesInAnchorsOnArmedAt pins the fix for the false
// RECONCILIATION LOOP measured live on provider-vsphere's namespaced
// Folder: a barrier armed at T that reads a controller log line timestamped
// AT or BEFORE T must never attribute that line to this window, however far
// back the underlying query reached — because that line describes something
// that happened before the window opened, most commonly the test harness's
// own pre-arm rename landing inside kubectl's whole-second --since
// rounding. The SAME single Update() call, moved to a timestamp AFTER
// armedAt, is genuine in-window evidence and must still be counted. Both
// directions are required — either one alone would leave the other failure
// mode unproven (a detector that discards everything is just as wrong as
// one that discards nothing).
func TestCountUpdateLogLinesInAnchorsOnArmedAt(t *testing.T) {
	armedAt := time.Date(2026, 8, 30, 14, 26, 32, 0, time.UTC)

	t.Run("OneUpdateOneSecondBeforeArmedAtIsNotCounted", func(t *testing.T) {
		// The recorded shape: the harness's rename Update() landed at
		// 14:26:31Z, one second before the barrier armed at 14:26:32Z.
		line := newTestUpdateLogLineAt(armedAt.Add(-time.Second), testNameExample, "")
		calls, lines := countUpdateLogLinesIn(line, testNameExample, "", armedAt)
		if calls != 0 {
			t.Errorf("calls = %d, want 0 — a call timestamped before the window armed must never be attributed to it", calls)
		}
		if lines != 0 {
			t.Errorf("lines = %d, want 0 — the line is outside the window entirely, not merely uncounted", lines)
		}
	})

	t.Run("SameSingleUpdateInsideTheWindowIsStillCounted", func(t *testing.T) {
		// The identical call, moved one second AFTER the barrier armed:
		// genuine in-window evidence, and the anchor must not suppress it.
		line := newTestUpdateLogLineAt(armedAt.Add(time.Second), testNameExample, "")
		calls, lines := countUpdateLogLinesIn(line, testNameExample, "", armedAt)
		if calls != 1 {
			t.Errorf("calls = %d, want 1 — a call timestamped after the window armed is genuine evidence and must still be counted", calls)
		}
		if lines != 1 {
			t.Errorf("lines = %d, want 1", lines)
		}
	})

	t.Run("LineTimestampedExactlyAtArmedAtIsDiscarded", func(t *testing.T) {
		// "at or before" per the doc comment: a line stamped at the exact
		// armed instant is not yet inside the window either.
		line := newTestUpdateLogLineAt(armedAt, testNameExample, "")
		calls, lines := countUpdateLogLinesIn(line, testNameExample, "", armedAt)
		if calls != 0 || lines != 0 {
			t.Errorf("calls, lines = %d, %d; want 0, 0 — a line stamped exactly at armedAt is not inside the window", calls, lines)
		}
	})

	t.Run("UnparseableTimestampIsKeptNotDiscarded", func(t *testing.T) {
		// The anchor exists to narrow an over-wide query, never to
		// suppress a signal it cannot itself place in time: a line with no
		// usable timestamp must survive, exactly as it did before the
		// anchor existed.
		line := "not-a-timestamp\tDEBUG\tprovider-example\t" + logMsgUpdated +
			`\t{"controller": "exampleresource", "request": {"name":"` + testNameExample + `"}, "version": "1"}`
		calls, lines := countUpdateLogLinesIn(line, testNameExample, "", armedAt)
		if calls != 1 || lines != 1 {
			t.Errorf("calls, lines = %d, %d; want 1, 1 — an unparseable timestamp must not be discarded", calls, lines)
		}
	})

	t.Run("ZeroArmedAtDisablesTheAnchorEntirely", func(t *testing.T) {
		// The zero Time is the "no window to anchor against" sentinel
		// every pre-existing direct call site in this file uses — it must
		// reproduce the pre-anchor behaviour exactly, whatever the line's
		// own timestamp says.
		line := newTestUpdateLogLineAt(armedAt.Add(-time.Hour), testNameExample, "")
		calls, lines := countUpdateLogLinesIn(line, testNameExample, "", time.Time{})
		if calls != 1 || lines != 1 {
			t.Errorf("calls, lines = %d, %d; want 1, 1 — the zero Time must disable the anchor, not merely widen it", calls, lines)
		}
	})
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

// TestRunConvergeDetectsAtProviderDrift drives a genuine atProvider drift
// through a real RunConverge call — re-homing hack/smoke-test.sh's "7a.
// failure injection: reconcile loop (drifting atProvider)" scenario as a
// Go-value assertion. The harness's other two checks for this scenario —
// the hook aborting after this step and never reaching the "run" step's
// patch — are consequences of Passed=false that belong to the caller
// sequencing the 5 steps, not to converge's own detection logic:
// RunConverge itself never calls Patch, which patchCalls==0 below proves
// directly.
func TestRunConvergeDetectsAtProviderDrift(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		APIVersion: testAPIVersionClusterScoped,
	}
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"zone": "a"},
		kind:            testKindExample,
		name:            testNameExample,
		apiVersion:      testAPIVersionClusterScoped,
		driftField:      "zone",
		driftValue:      "b",
		// convergeArm reads the resource 4 times before the baseline
		// snapshot is captured (waitGenerationSettled, waitSynced,
		// waitReady, then the baseline Snapshot() itself) — call 5 is
		// convergeAssert's post-window Snapshot(), the first read that
		// must see the drift. Firing any earlier would bake the drift
		// into the baseline itself and the diff would see no change at
		// all.
		driftAfterGetCalls: 5,
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
		t.Fatalf("expected Passed=false: a genuine atProvider drift must fail, got %+v", result)
	}
	if result.Message != "RECONCILIATION LOOP DETECTED" {
		t.Errorf("Message = %q, want RECONCILIATION LOOP DETECTED", result.Message)
	}
	if !strings.Contains(strings.Join(result.Diagnostics, "\n"), "atProvider changed") {
		t.Errorf("Diagnostics = %v, want an entry naming the atProvider change", result.Diagnostics)
	}
	if f.patchCalls != 0 {
		t.Errorf("patchCalls = %d, want 0 — a convergence check is read-only and must never patch the resource it is only observing", f.patchCalls)
	}
}

// TestRunConvergeDetectsLoopFromEventCountAlone drives a genuine
// event-count-only loop (repeated update events, no atProvider drift at
// all) through a real RunConverge call — re-homing hack/smoke-test.sh's
// "7c. failure injection: reconciliation loop via repeated update events
// (no atProvider drift)" scenario. The controller log stays quiet
// throughout (the default fakeCluster logLines, a single benign reconcile
// line), so this proves the event delta alone is sufficient to catch the
// loop, independent of the log instrument
// TestRunConvergeDetectsLoopFromControllerLogAlone covers below.
func TestRunConvergeDetectsLoopFromEventCountAlone(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		APIVersion: testAPIVersionClusterScoped,
	}
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"zone": "a"},
		kind:            testKindExample,
		name:            testNameExample,
		apiVersion:      testAPIVersionClusterScoped,
		// Every `get events` read bumps the aggregated count by 1, with
		// zero atProvider change — the loop this test isolates. RunConverge
		// reads events exactly twice (baseline, then outcome), so the
		// observed delta is exactly 1.
		generations:              []int32{0},
		eventGrowthPerEventsCall: 1,
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
		t.Fatalf("expected Passed=false: a growing update-event count with no atProvider drift must still fail, got %+v", result)
	}
	if result.Message != "RECONCILIATION LOOP DETECTED" {
		t.Errorf("Message = %q, want RECONCILIATION LOOP DETECTED", result.Message)
	}
	if !strings.Contains(strings.Join(result.Diagnostics, "\n"), "1 new update event(s) observed") {
		t.Errorf("Diagnostics = %v, want an entry naming a non-zero new-event count — converge must still be COUNTING events, not just diffing atProvider", result.Diagnostics)
	}
	if f.patchCalls != 0 {
		t.Errorf("patchCalls = %d, want 0 — a convergence check is read-only and must never patch the resource it is only observing", f.patchCalls)
	}
}

// TestRunConvergeLogloopDiagnosticsNameCallCountAndKeepEventDeltaAtZero is
// TestRunConvergeDetectsLoopFromControllerLogAlone's diagnostics
// counterpart, re-homing hack/smoke-test.sh's "7e..." diagnostic checks:
// the reported Update() call count must be non-zero and the event-delta
// diagnostic must never appear, proving the verdict came from the LOG
// alone rather than from an event delta this scenario deliberately holds
// at zero — the measured live failure client-go's event rate limiter
// produces.
func TestRunConvergeLogloopDiagnosticsNameCallCountAndKeepEventDeltaAtZero(t *testing.T) {
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

	joined := strings.Join(result.Diagnostics, "\n")
	if !strings.Contains(joined, "Update() call(s) in the controller log") {
		t.Errorf("Diagnostics = %v, want an entry naming the controller-log Update() call count", result.Diagnostics)
	}
	if strings.Contains(joined, "0 Update() call(s) in the controller log") {
		t.Errorf("Diagnostics = %v, the reported Update() call count must be non-zero", result.Diagnostics)
	}
	if strings.Contains(joined, "new update event(s) observed") {
		t.Errorf("Diagnostics = %v, the event delta must stay silent — this scenario isolates the LOG as the only signal", result.Diagnostics)
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

// TestRunConvergeIgnoresOwnPreArmRenameLogLine reproduces the measured
// false RECONCILIATION LOOP end to end through RunConverge: the post-assert
// hook renames the resource immediately before the barrier arms, and that
// rename's own Update() call lands in the controller log a moment BEFORE
// convergeArm takes its baseline — inside kubectl's whole-second --since
// rounding, but outside the observation window itself. The window anchor
// (see countUpdateLogLinesIn) must discard it, and the resource must report
// stable with zero updates rather than a loop it never actually made.
//
// preArm is captured before RunConverge runs, so the baseline's own ArmedAt
// (captured moments later, inside convergeArm) is always >= preArm — which
// makes preArm.Add(-time.Second) deterministically BEFORE ArmedAt with no
// clock injection required, on any machine, on any date.
func TestRunConvergeIgnoresOwnPreArmRenameLogLine(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		Namespace:  testNamespaceExample,
		APIVersion: testAPIVersionNamespaced,
	}
	preArm := time.Now()
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"path": "/DC0/vm"},
		kind:            testKindExample,
		name:            testNameExample,
		namespace:       testNamespaceExample,
		apiVersion:      testAPIVersionNamespaced,
		generations:     []int32{0},
		logLines: strings.Join([]string{
			testReconcileLogLine,
			// The harness's own pre-assert-hook rename, timestamped a
			// second before the barrier is about to arm — exactly the
			// shape recorded live (rename at 14:26:31Z, barrier armed at
			// 14:26:32Z).
			newTestUpdateLogLineAt(preArm.Add(-time.Second), testNameExample, testNamespaceExample),
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
		t.Fatalf("expected Passed=true: a rename Update() timestamped before the barrier armed is not evidence of a loop, got %+v", result)
	}
	if result.Message != "resource stable (1 cycle observed, 0 updates)" {
		t.Errorf("Message = %q, want the stable-resource message with 0 updates — the pre-arm rename must not be attributed to this window", result.Message)
	}
}

// TestRunConvergeStillDetectsLoopInsideTheWindow is
// TestRunConvergeIgnoresOwnPreArmRenameLogLine's counterpart: the identical
// single Update() call, moved to a timestamp safely AFTER the barrier
// arms, is genuine in-window evidence and must still fail. Proving only the
// pre-arm direction would leave open the possibility that the anchor was
// implemented by discarding every log line rather than the ones before it.
func TestRunConvergeStillDetectsLoopInsideTheWindow(t *testing.T) {
	m := &manifest.Manifest{
		Kind:       testKindExample,
		Name:       testNameExample,
		Namespace:  testNamespaceExample,
		APIVersion: testAPIVersionNamespaced,
	}
	// A comfortable margin past whatever ArmedAt convergeArm records a
	// moment after this fixture is built — well inside the (real,
	// millisecond-scale) observation window this fast test actually waits.
	inWindow := time.Now().Add(time.Hour)
	f := &fakeCluster{
		generation:      1,
		readyAfterCalls: 1,
		atProvider:      map[string]interface{}{"path": "/DC0/vm"},
		kind:            testKindExample,
		name:            testNameExample,
		namespace:       testNamespaceExample,
		apiVersion:      testAPIVersionNamespaced,
		generations:     []int32{0},
		logLines: strings.Join([]string{
			testReconcileLogLine,
			newTestUpdateLogLineAt(inWindow, testNameExample, testNamespaceExample),
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
		t.Fatalf("expected Passed=false: an Update() call genuinely inside the window must still fail, got %+v", result)
	}
	if result.Message != "RECONCILIATION LOOP DETECTED" {
		t.Errorf("Message = %q, want RECONCILIATION LOOP DETECTED — the anchor must never mask a real in-window call", result.Message)
	}
}

// sequentialPodIdentity returns a podIdentityFunc stand-in that reports
// names[0], names[1], ... in order, then keeps repeating the LAST name
// forever once the list is exhausted — standing for a controller Pod that
// restarts some number of times and then stays put. Every reported
// identity carries a CreatedAt far in the past, so it always satisfies
// convergeArm's settle gate on the very first read regardless of the
// threshold in effect; these tests are about identity-change detection,
// not about the settle gate covered by TestConvergeArmPodSettleGate.
func sequentialPodIdentity(names ...string) func() (controllerPodIdentity, error) {
	i := 0
	return func() (controllerPodIdentity, error) {
		name := names[i]
		if i < len(names)-1 {
			i++
		}
		return controllerPodIdentity{Name: name, CreatedAt: time.Now().Add(-time.Hour)}, nil
	}
}

// TestConvergeArmPodSettleGate pins convergeArm's new pre-check: it must
// refuse to take a baseline while the provider controller Pod is younger
// than the settle threshold — reporting the same RESOURCE NOT IN STEADY
// STATE verdict an unsettled generation reports, and naming the Pod and its
// age — and must record the settled Pod's identity into the baseline once
// it does proceed, which is what convergeAssertAttempt later compares
// against.
func TestConvergeArmPodSettleGate(t *testing.T) {
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	opts := ConvergeOptions{
		PollInterval:     time.Millisecond,
		ReadinessTimeout: time.Second,
		Timeout:          time.Second,
	}

	t.Run("YoungPodRefusesBaseline", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		r.podSettleTimeout = 10 * time.Millisecond
		r.podIdentityFunc = func() (controllerPodIdentity, error) {
			return controllerPodIdentity{Name: "provider-example-fresh", CreatedAt: time.Now()}, nil
		}

		baseline, early, err := r.convergeArm(m, opts)
		if err != nil {
			t.Fatalf("convergeArm() error = %v", err)
		}
		if baseline != nil {
			t.Fatalf("expected no baseline when the controller Pod never settles within the timeout, got %+v", baseline)
		}
		if early == nil {
			t.Fatal("expected an early verdict, got nil")
		}
		if early.Passed {
			t.Errorf("expected Passed=false, got %+v", early)
		}
		if early.Message != "RESOURCE NOT IN STEADY STATE" {
			t.Errorf("Message = %q, want %q — an unsettled controller Pod is not evidence of a loop", early.Message, "RESOURCE NOT IN STEADY STATE")
		}
		if len(early.Diagnostics) != 1 || !strings.Contains(early.Diagnostics[0], "provider-example-fresh") {
			t.Errorf("expected exactly one diagnostic naming the young Pod, got %v", early.Diagnostics)
		}
	})

	t.Run("SettledPodRecordsIdentityInBaseline", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		r.podIdentityFunc = func() (controllerPodIdentity, error) {
			return controllerPodIdentity{Name: "provider-example-old", CreatedAt: time.Now().Add(-time.Hour)}, nil
		}

		baseline, early, err := r.convergeArm(m, opts)
		if err != nil {
			t.Fatalf("convergeArm() error = %v", err)
		}
		if early != nil {
			t.Fatalf("expected no early verdict for an already-settled Pod, got %+v", early)
		}
		if baseline == nil {
			t.Fatal("expected a baseline, got nil")
		}
		if baseline.PodIdentity.Name != "provider-example-old" {
			t.Errorf("baseline.PodIdentity.Name = %q, want %q", baseline.PodIdentity.Name, "provider-example-old")
		}
	})
}

// TestConvergeAssertRestartDetection is this ticket's central proof: a
// provider controller Pod restart during the observation window must never
// be reported as RECONCILIATION LOOP DETECTED, a resource whose window is
// spoiled on every attempt must never be silently passed, and — the
// regression guard — a window with NO restart must behave exactly as it
// did before this change, in both the happy-path and genuine-loop
// directions.
func TestConvergeAssertRestartDetection(t *testing.T) {
	newManifest := func() *manifest.Manifest {
		return &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	}
	opts := ConvergeOptions{
		PollInterval:     time.Millisecond,
		ReadinessTimeout: time.Second,
		Timeout:          time.Second,
	}

	t.Run("NoRestartHappyPathIsByteIdenticalToPreChangeBehavior", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		r.podIdentityFunc = sequentialPodIdentity("provider-example-abc")

		result, err := r.RunConverge(newManifest(), opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if !result.Passed {
			t.Fatalf("expected Passed=true with no restart observed, got %+v", result)
		}
		// This is the exact message buildConvergeResult has always produced
		// for this scenario (see TestBuildConvergeResultHeadline) — pinned
		// here to prove the new gate cannot fire spuriously on the happy
		// path.
		const want = "resource stable (1 cycle observed, 0 updates)"
		if result.Message != want {
			t.Errorf("Message = %q, want %q — the pod-restart gate must be a no-op when no restart occurred", result.Message, want)
		}
		if len(result.Diagnostics) != 0 {
			t.Errorf("expected zero diagnostics on the happy path, got %v", result.Diagnostics)
		}
	})

	t.Run("OneRestartReArmsAndStillPasses", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		r.podIdentityFunc = sequentialPodIdentity("provider-example-old", "provider-example-new")

		result, err := r.RunConverge(newManifest(), opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if !result.Passed {
			t.Fatalf("expected Passed=true: a restart mid-window is a legitimate re-reconcile, not drift, got %+v", result)
		}
		if result.Message == "RECONCILIATION LOOP DETECTED" {
			t.Error("a provider controller Pod restart must never be reported as a reconciliation loop")
		}
		// The re-arm's own signal: a passing verdict must still say a
		// restart happened, naming both Pod identities and the attempt
		// number, so a live run can confirm the re-arm fired rather than
		// inferring it from the absence of a false loop verdict.
		if len(result.Diagnostics) != 1 {
			t.Fatalf("expected exactly one diagnostic (the restart note) on a passing re-armed result, got %v", result.Diagnostics)
		}
		note := result.Diagnostics[0]
		for _, want := range []string{"attempt 1", "provider-example-old", "provider-example-new"} {
			if !strings.Contains(note, want) {
				t.Errorf("restart note %q missing %q", note, want)
			}
		}
	})

	t.Run("RestartOnEveryAttemptReportsInconclusiveNeverASilentPass", func(t *testing.T) {
		f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		i := 0
		r.podIdentityFunc = func() (controllerPodIdentity, error) {
			i++
			// A fresh, never-repeating name on every single call: the
			// controller Pod is (implausibly) restarting continuously,
			// so every re-arm's own baseline is immediately spoiled too.
			return controllerPodIdentity{Name: "provider-example-" + strconv.Itoa(i), CreatedAt: time.Now().Add(-time.Hour)}, nil
		}

		result, err := r.RunConverge(newManifest(), opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if result.Passed {
			t.Fatalf("a resource whose window was spoiled on every attempt must never silently pass, got %+v", result)
		}
		if result.Message != "CONVERGENCE INCONCLUSIVE" {
			t.Errorf("Message = %q, want %q", result.Message, "CONVERGENCE INCONCLUSIVE")
		}
		if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0], "restarted 3 time(s)") {
			t.Errorf("expected exactly one diagnostic naming the bounded restart count (convergeMaxRestartRetries=2, so 3 observed restarts before giving up), got %v", result.Diagnostics)
		}
	})

	t.Run("GenuineLoopWithNoRestartStillReportsLoopDetected", func(t *testing.T) {
		f := &fakeCluster{
			generation:      3,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"zone": "a"},
			kind:            testKindExample,
			name:            testNameExample,
			// The event channel never moves — the rate limiter has not
			// flushed — so only the controller-log instrument catches
			// this, exactly as in TestRunConvergeDetectsLoopFromControllerLogAlone.
			generations: []int32{0},
			logLines: strings.Join([]string{
				testReconcileLogLine,
				newTestUpdateLogLine(testNameExample, ""),
				newTestUpdateLogLine(testNameExample, ""),
			}, "\n"),
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		r.podIdentityFunc = sequentialPodIdentity("provider-example-stable")

		result, err := r.RunConverge(newManifest(), opts)
		if err != nil {
			t.Fatalf("RunConverge() error = %v", err)
		}
		if result.Passed {
			t.Fatalf("a genuine Update()-call loop with no restart observed must still fail, got %+v", result)
		}
		if result.Message != "RECONCILIATION LOOP DETECTED" {
			t.Errorf("Message = %q, want %q — the restart-awareness gate must never mask a real loop", result.Message, "RECONCILIATION LOOP DETECTED")
		}
	})
}
