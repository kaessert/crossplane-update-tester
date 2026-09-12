package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/roundtrip"
)

// ─── pure function tests ────────────────────────────────────────────────

func TestUnassertedClearRoute(t *testing.T) {
	cases := map[string]struct {
		route roundtrip.ClearRoute
		want  bool
	}{
		"SiblingClearIsUnasserted":        {roundtrip.RouteSiblingClear, true},
		"SiblingWithValuesIsUnasserted":   {roundtrip.RouteSiblingWithValues, true},
		"AncestorTombstoneIsUnasserted":   {roundtrip.RouteAncestorTombstone, true},
		"PerKeyNullIsDirectlyAsserted":    {roundtrip.RoutePerKeyNull, false},
		"SelfTombstoneIsDirectlyAsserted": {roundtrip.RouteSelfTombstone, false},
		"EmptyRouteIsNotUnasserted":       {roundtrip.ClearRoute(""), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := unassertedClearRoute(tc.route); got != tc.want {
				t.Errorf("unassertedClearRoute(%q) = %v, want %v", tc.route, got, tc.want)
			}
		})
	}
}

func TestIsAncestorOf(t *testing.T) {
	cases := map[string]struct {
		ancestor, leaf string
		want           bool
	}{
		"DirectParent":                   {"network", "network.subnets", true},
		"MultiLevel":                     {"network", "network.subnets.cidr", true},
		"SamePathIsNotAnAncestor":        {"network", "network", false},
		"TextualPrefixIsNotSegmentMatch": {"net", "network.subnets", false},
		"UnrelatedPath":                  {"labels", "network.subnets", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isAncestorOf(tc.ancestor, tc.leaf); got != tc.want {
				t.Errorf("isAncestorOf(%q, %q) = %v, want %v", tc.ancestor, tc.leaf, got, tc.want)
			}
		})
	}
}

func TestClearedValue(t *testing.T) {
	cases := map[string]struct {
		s    string
		want bool
	}{
		"AbsentOrNull": {"", true},
		"EmptyList":    {"[]", true},
		"EmptyMap":     {"{}", true},
		"NonEmptyList": {`["a"]`, false},
		"NonEmptyMap":  {`{"a":"1"}`, false},
		"Scalar":       {"still-here", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := clearedValue(tc.s); got != tc.want {
				t.Errorf("clearedValue(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

func TestTriggerFieldFor(t *testing.T) {
	tests := []manifest.UpdateTest{
		{Field: "name", Value: "new-name", Clear: []string{"tags"}},
		{Field: "network", Value: "unused", Clear: []string{"network"}},
		{Field: "useOptions", Value: false, WithValues: map[string]interface{}{"options": []interface{}{}}},
	}

	t.Run("SiblingClearExactPath", func(t *testing.T) {
		got, ok := triggerFieldFor("tags", roundtrip.RouteSiblingClear, tests)
		if !ok || got != "name" {
			t.Errorf("triggerFieldFor(tags, SiblingClear) = (%q, %v), want (\"name\", true)", got, ok)
		}
	})

	t.Run("AncestorTombstoneWalksUpToAncestorEntry", func(t *testing.T) {
		got, ok := triggerFieldFor("network.subnets", roundtrip.RouteAncestorTombstone, tests)
		if !ok || got != "network" {
			t.Errorf("triggerFieldFor(network.subnets, AncestorTombstone) = (%q, %v), want (\"network\", true)", got, ok)
		}
	})

	t.Run("SiblingWithValuesEmptyList", func(t *testing.T) {
		got, ok := triggerFieldFor("options", roundtrip.RouteSiblingWithValues, tests)
		if !ok || got != "useOptions" {
			t.Errorf("triggerFieldFor(options, SiblingWithValues) = (%q, %v), want (\"useOptions\", true)", got, ok)
		}
	})

	t.Run("NoResolvableTriggerReportsFalse", func(t *testing.T) {
		if _, ok := triggerFieldFor("nosuchleaf", roundtrip.RouteSiblingClear, tests); ok {
			t.Error("triggerFieldFor found a trigger for a leaf no entry names, want false")
		}
	})

	t.Run("WithValuesNonEmptyListNeverMatches", func(t *testing.T) {
		nonEmpty := []manifest.UpdateTest{
			{Field: "useOptions", Value: false, WithValues: map[string]interface{}{"options": []interface{}{"a"}}},
		}
		if _, ok := triggerFieldFor("options", roundtrip.RouteSiblingWithValues, nonEmpty); ok {
			t.Error("triggerFieldFor matched a non-empty withValues list, want false")
		}
	})
}

// TestUnobservedClearCredits covers unobservedClearCredits directly,
// including the "UnresolvableTriggerIsReportedUnresolved" subtest below —
// the third silent path (UnprovenNoTrigger). That subtest is deliberately
// exercised at THIS level rather than as a RunTests-level integration test
// like the skipped-lender and no-op-lender paths get: coverageFor and
// triggerFieldFor both walk the identical t.Clear/t.WithValues structures
// built from the SAME m.Tests slice, with identical matching predicates
// (exact path, or isAncestorOf's segment-exact prefix), so a credited
// representative that coverageFor found NEVER fails to be found again by
// triggerFieldFor through any CRD+manifest combination the classifier
// itself can construct — the path is a defensive guard against a FUTURE
// divergence between the two, not a currently reachable live scenario.
// Constructing it here, with a synthetic ClearCellReport naming a
// Representative no test entry mentions at all, is the correct level: it
// exercises exactly the code this ticket's fix added, with no need to
// first find (or fabricate) a CRD/manifest pair the shared classifier
// would never itself produce.
func TestUnobservedClearCredits(t *testing.T) {
	tests := []manifest.UpdateTest{
		{Field: "name", Value: "new-name", Clear: []string{"tags"}},
	}
	t.Run("CoveredUnassertedRouteIsPending", func(t *testing.T) {
		reports := []roundtrip.ClearCellReport{
			{Covered: true, Representative: "tags", Route: roundtrip.RouteSiblingClear},
		}
		got, unresolved := unobservedClearCredits(reports, tests)
		if len(got["name"]) != 1 || got["name"][0].representative != "tags" {
			t.Errorf("unobservedClearCredits = %+v, want one pending assertion on trigger %q for \"tags\"", got, "name")
		}
		if len(unresolved) != 0 {
			t.Errorf("unresolved = %+v, want none for a resolvable trigger", unresolved)
		}
	})

	t.Run("UncoveredCellIsNeverPending", func(t *testing.T) {
		reports := []roundtrip.ClearCellReport{
			{Covered: false, Representative: "", Route: ""},
		}
		got, unresolved := unobservedClearCredits(reports, tests)
		if len(got) != 0 {
			t.Errorf("unobservedClearCredits = %+v, want empty for an uncovered cell", got)
		}
		if len(unresolved) != 0 {
			t.Errorf("unresolved = %+v, want none for an uncovered cell", unresolved)
		}
	})

	t.Run("DirectlyAssertedRouteIsNeverPending", func(t *testing.T) {
		reports := []roundtrip.ClearCellReport{
			{Covered: true, Representative: "tags", Route: roundtrip.RoutePerKeyNull},
		}
		got, unresolved := unobservedClearCredits(reports, tests)
		if len(got) != 0 {
			t.Errorf("unobservedClearCredits = %+v, want empty for RoutePerKeyNull (directly asserted)", got)
		}
		if len(unresolved) != 0 {
			t.Errorf("unresolved = %+v, want none for RoutePerKeyNull (directly asserted)", unresolved)
		}
	})

	t.Run("UnresolvableTriggerIsReportedUnresolved", func(t *testing.T) {
		reports := []roundtrip.ClearCellReport{
			{Covered: true, Representative: "nosuchleaf", Route: roundtrip.RouteSiblingClear},
		}
		got, unresolved := unobservedClearCredits(reports, tests)
		if len(got) != 0 {
			t.Errorf("unobservedClearCredits = %+v, want empty when no entry names the representative", got)
		}
		if len(unresolved) != 1 {
			t.Fatalf("unresolved = %+v, want exactly one entry when no entry names the representative", unresolved)
		}
		u := unresolved[0]
		if u.Representative != "nosuchleaf" || u.Route != roundtrip.RouteSiblingClear || u.Reason != UnprovenNoTrigger {
			t.Errorf("unresolved[0] = %+v, want Representative=nosuchleaf Route=%s Reason=%s", u, roundtrip.RouteSiblingClear, UnprovenNoTrigger)
		}
		if u.TriggerField != "" {
			t.Errorf("unresolved[0].TriggerField = %q, want empty (no entry could be matched)", u.TriggerField)
		}
	})
}

func TestCheckClearAssertions(t *testing.T) {
	pending := map[string][]pendingClearAssertion{
		"name": {{representative: "tags", route: roundtrip.RouteSiblingClear}},
	}

	t.Run("GenuinelyEmptiedProducesNoViolation", func(t *testing.T) {
		violations, err := checkClearAssertions([]byte(`{}`), pending, "name")
		if err != nil {
			t.Fatalf("checkClearAssertions: unexpected error: %v", err)
		}
		if len(violations) != 0 {
			t.Errorf("violations = %+v, want none when the representative reads absent", violations)
		}
	})

	t.Run("StillPopulatedProducesGatingViolation", func(t *testing.T) {
		violations, err := checkClearAssertions([]byte(`{"tags":["a"]}`), pending, "name")
		if err != nil {
			t.Fatalf("checkClearAssertions: unexpected error: %v", err)
		}
		if len(violations) != 1 {
			t.Fatalf("got %d violations, want 1: %+v", len(violations), violations)
		}
		v := violations[0]
		if v.Representative != "tags" || v.TriggerField != "name" || v.Route != roundtrip.RouteSiblingClear || v.Observed != `["a"]` {
			t.Errorf("violation = %+v, want Representative=tags TriggerField=name Route=%s Observed=[\"a\"]", v, roundtrip.RouteSiblingClear)
		}
	})

	t.Run("UnrelatedTriggerFieldIsANoOp", func(t *testing.T) {
		violations, err := checkClearAssertions([]byte(`{"tags":["a"]}`), pending, "unrelated-field")
		if err != nil {
			t.Fatalf("checkClearAssertions: unexpected error: %v", err)
		}
		if len(violations) != 0 {
			t.Errorf("violations = %+v, want none for a trigger field with no pending assertion", violations)
		}
	})
}

// ─── clearCellReportsFor: CRD-backed, offline ───────────────────────────

// clearAssertFixtureCRD declares a scalar (name), a top-level list (tags),
// a top-level scalar/list pair used for the withValues shape (useOptions,
// options), and a nested object (network) whose own nested list
// (network.subnets) is only reachable through an ancestor clear: on
// network — exactly the shapes TestRunTestsClearCredit* below patch
// against a live (fake) object.
const clearAssertFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: widgets.crossplane.io
  names:
    kind: ExampleResource
    plural: exampleresources
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  name:
                    type: string
                  tags:
                    type: array
                    items:
                      type: string
                  useOptions:
                    type: boolean
                  options:
                    type: array
                    items:
                      type: string
                  network:
                    type: object
                    properties:
                      subnets:
                        type: object
                        additionalProperties:
                          type: string
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

func writeClearAssertFixtureCRD(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "package", "crds")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating package/crds: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "widget.yaml"), []byte(clearAssertFixtureCRD), 0o600); err != nil {
		t.Fatalf("writing CRD fixture: %v", err)
	}
	return root
}

func TestClearCellReportsFor(t *testing.T) {
	t.Run("EmptyRootReturnsNil", func(t *testing.T) {
		m := &manifest.Manifest{APIVersion: "widgets.crossplane.io/v1alpha1", Kind: "ExampleResource"}
		if got := clearCellReportsFor("", m); got != nil {
			t.Errorf("clearCellReportsFor(\"\", ...) = %+v, want nil", got)
		}
	})

	t.Run("NoMatchingCRDReturnsNil", func(t *testing.T) {
		root := writeClearAssertFixtureCRD(t)
		m := &manifest.Manifest{APIVersion: "other.crossplane.io/v1alpha1", Kind: "NoSuchKind"}
		if got := clearCellReportsFor(root, m); got != nil {
			t.Errorf("clearCellReportsFor for an unmatched CRD = %+v, want nil", got)
		}
	})

	t.Run("SiblingClearCellIsReportedCovered", func(t *testing.T) {
		root := writeClearAssertFixtureCRD(t)
		m := &manifest.Manifest{
			APIVersion: "widgets.crossplane.io/v1alpha1", Kind: "ExampleResource",
			Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"tags"}}},
		}
		reports := clearCellReportsFor(root, m)
		var found bool
		for _, r := range reports {
			if r.Representative == "tags" {
				found = true
				if r.Route != roundtrip.RouteSiblingClear {
					t.Errorf("tags credited via %s, want %s", r.Route, roundtrip.RouteSiblingClear)
				}
			}
		}
		if !found {
			t.Errorf("no covered cell reported tags as its representative: %+v", reports)
		}
	})
}

// ─── RunTests-level integration tests ───────────────────────────────────

// TestRunTestsClearCreditSiblingClearPassesWhenRepresentativeGenuinelyEmpties
// drives a full RunTests call through a manifest whose ONLY entry patches
// `name` and folds a `clear: [tags]` sibling tombstone into the same
// merge patch. tags is never itself t.Field for any entry, so nothing
// before this feature ever asserted its real post-state — a well-behaved
// fake backend genuinely empties it here, and RunTests must report zero
// clear-credit violations.
func TestRunTestsClearCreditSiblingClearPassesWhenRepresentativeGenuinelyEmpties(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider:       map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		atProvider:        map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"tags"}}},
	}

	results, _, clearViolations, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected the name field test itself to pass, got %+v", results)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations, want 0 (tags genuinely emptied): %+v", len(clearViolations), clearViolations)
	}
}

// TestRunTestsClearCreditGatesOnSilentDiscard is the fixed-address shape
// measured live on provider-infobloxnios: a `withValues: {options: []}`
// sibling folds an empty list into the SAME merge patch as `useOptions`'s
// own field test, the backend accepts the request (200) and the useOptions
// test itself PASSES, but the backend silently discards the write to
// `options` — it never actually empties. A spec-side check would see
// exactly what was submitted and pass; RunTests must catch this because it
// reads the OBSERVED MIRROR instead.
func TestRunTestsClearCreditGatesOnSilentDiscard(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider:           map[string]interface{}{"useOptions": true, "options": []interface{}{"opt-a"}},
		atProvider:            map[string]interface{}{"useOptions": true, "options": []interface{}{"opt-a"}},
		generation:            1,
		kind:                  testKindExample,
		name:                  testNameExample,
		recordUpdateEvent:     true,
		discardAtProviderPath: "options",
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{
			Field: "useOptions", Value: false,
			WithValues: map[string]interface{}{"options": []interface{}{}},
		}},
	}

	results, _, clearViolations, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected the useOptions field test itself to pass (the backend DID flip it), got %+v", results)
	}

	if len(clearViolations) != 1 {
		t.Fatalf("got %d clear-credit violations, want exactly 1 (options never actually emptied): %+v", len(clearViolations), clearViolations)
	}
	v := clearViolations[0]
	if v.Representative != "options" {
		t.Errorf("violation Representative = %q, want %q", v.Representative, "options")
	}
	if v.Route != roundtrip.RouteSiblingWithValues {
		t.Errorf("violation Route = %q, want %q", v.Route, roundtrip.RouteSiblingWithValues)
	}
	if v.TriggerField != "useOptions" {
		t.Errorf("violation TriggerField = %q, want %q (the entry whose patch claimed the credit)", v.TriggerField, "useOptions")
	}
	if v.Observed != `["opt-a"]` {
		t.Errorf("violation Observed = %q, want %q (the value the backend never actually cleared)", v.Observed, `["opt-a"]`)
	}
}

// TestRunTestsClearCreditAncestorTombstonePassesWhenNestedLeafGenuinelyEmpties
// covers the ancestor-tombstone route: a `clear: [network]` sibling removes
// the WHOLE network subtree under RFC-7386 merge-patch semantics, which
// genuinely takes network.subnets — a leaf no entry ever names directly —
// with it.
func TestRunTestsClearCreditAncestorTombstonePassesWhenNestedLeafGenuinelyEmpties(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider: map[string]interface{}{
			"name":    "old-name",
			"network": map[string]interface{}{"subnets": map[string]interface{}{"a": "1"}},
		},
		atProvider: map[string]interface{}{
			"name":    "old-name",
			"network": map[string]interface{}{"subnets": map[string]interface{}{"a": "1"}},
		},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"network"}}},
	}

	_, _, clearViolations, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations, want 0 (network.subnets genuinely swept away with its ancestor): %+v", len(clearViolations), clearViolations)
	}
}

// TestRunTestsClearCreditAncestorTombstoneGatesOnSurvivingNestedLeaf is the
// ancestor-tombstone negative counterpart: the backend accepts the merge
// patch removing `network` wholesale but a nested leaf beneath it survives
// anyway (a backend that reconstructs the subtree from some other stored
// state) — the credit's claim that the whole subtree emptied is false for
// this one member, and RunTests must gate on it even though nothing names
// network.subnets directly.
func TestRunTestsClearCreditAncestorTombstoneGatesOnSurvivingNestedLeaf(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider: map[string]interface{}{
			"name":    "old-name",
			"network": map[string]interface{}{"subnets": map[string]interface{}{"a": "1"}},
		},
		atProvider: map[string]interface{}{
			"name":    "old-name",
			"network": map[string]interface{}{"subnets": map[string]interface{}{"a": "1"}},
		},
		generation:            1,
		kind:                  testKindExample,
		name:                  testNameExample,
		recordUpdateEvent:     true,
		discardAtProviderPath: "network.subnets",
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"network"}}},
	}

	_, _, clearViolations, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(clearViolations) != 1 {
		t.Fatalf("got %d clear-credit violations, want exactly 1 (network.subnets survived its ancestor's tombstone): %+v", len(clearViolations), clearViolations)
	}
	v := clearViolations[0]
	if v.Representative != "network.subnets" {
		t.Errorf("violation Representative = %q, want %q", v.Representative, "network.subnets")
	}
	if v.Route != roundtrip.RouteAncestorTombstone {
		t.Errorf("violation Route = %q, want %q", v.Route, roundtrip.RouteAncestorTombstone)
	}
	if v.TriggerField != "name" {
		t.Errorf("violation TriggerField = %q, want %q (the entry whose clear: named the ancestor)", v.TriggerField, "name")
	}
	if v.Observed != `{"a":"1"}` {
		t.Errorf("violation Observed = %q, want %q", v.Observed, `{"a":"1"}`)
	}
}

// TestRunTestsClearCreditDisabledWithNoRoot pins the degrade-gracefully
// contract WithRoot's own doc comment promises: a Runner built with no
// root to offer (every pre-existing caller of RunTests, and every test in
// runner_test.go) computes zero pending clear-credit assertions and
// reports zero violations — never an error — even against a manifest
// whose Clear entry would otherwise produce one.
func TestRunTestsClearCreditDisabledWithNoRoot(t *testing.T) {
	f := &fakeCluster{
		forProvider:           map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		atProvider:            map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		generation:            1,
		kind:                  testKindExample,
		name:                  testNameExample,
		recordUpdateEvent:     true,
		discardAtProviderPath: "tags", // would violate if the check ran at all
	}
	r := newFakeRunner(f) // no WithRoot call

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"tags"}}},
	}

	_, _, clearViolations, unprovenClear, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations with no root declared, want 0 (the check must be disabled entirely): %+v", len(clearViolations), clearViolations)
	}
	if len(unprovenClear) != 0 {
		t.Errorf("got %d unproven clear-credit entries with no root declared, want 0 (the check must be disabled entirely): %+v", len(unprovenClear), unprovenClear)
	}
}

// ─── RunTests-level: an unconsumed pendingClear entry must gate ─────────

// TestRunTestsClearCreditUnprovenWhenLenderSkipped is the shape of the
// FIRST silent path: an entry carries BOTH skip: and clear: on the SAME
// field. containerclear.go's offline credit builder walks every entry's
// clear: list with no skip filter (see ContainerClearCoverage — skip: is
// consulted only for Disposition, never for cell membership), so tags
// still reads as "covered via sibling clear:" offline even though the
// entry whose patch was supposed to prove it live never runs at all. Before
// this ticket's fix, RunTests built pendingClear and simply never checked
// whether every key in it was ever consumed — a skipped lender's key was
// never looked up, and the run exited 0 reporting nothing.
//
// This exact combination — skip: alongside clear:/withValues: on ONE entry
// — is already rejected at parse time by manifest.ValidateFieldEntryMix,
// so no manifest that reaches RunTests through the real `run`/`batch` CLI
// path (both go through manifest.Parse) can ever construct it; the
// &manifest.Manifest{} literal below reaches the same state ONLY by
// bypassing that parser, exactly as this file's other hand-built fixtures
// already do. The test still earns its place: it proves the ACCOUNTING
// fix (an unconsumed pendingClear entry gates) rather than the parser
// guard, and it is the only defence left for a manifest built any other
// way — a future caller that constructs a Manifest directly, or a parser
// guard that is ever loosened.
func TestRunTestsClearCreditUnprovenWhenLenderSkipped(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider:       map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		atProvider:        map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{
			Field: "name", Value: "new-name", Clear: []string{"tags"},
			Skip: manifest.LegacySkip("name is not independently testable on this backend"),
		}},
	}

	results, _, clearViolations, unprovenClear, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("expected the name field test itself to be skipped, got %+v", results)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations, want 0 (the live check for a skipped lender never ran at all): %+v", len(clearViolations), clearViolations)
	}
	if len(unprovenClear) != 1 {
		t.Fatalf("got %d unproven clear-credit entries, want exactly 1 (tags' credit rides a skipped lender): %+v", len(unprovenClear), unprovenClear)
	}
	u := unprovenClear[0]
	if u.Representative != "tags" {
		t.Errorf("unproven Representative = %q, want %q", u.Representative, "tags")
	}
	if u.Route != roundtrip.RouteSiblingClear {
		t.Errorf("unproven Route = %q, want %q", u.Route, roundtrip.RouteSiblingClear)
	}
	if u.TriggerField != "name" {
		t.Errorf("unproven TriggerField = %q, want %q (the skipped entry whose clear: claimed the credit)", u.TriggerField, "name")
	}
	if u.Reason != UnprovenLenderSkipped {
		t.Errorf("unproven Reason = %q, want %q", u.Reason, UnprovenLenderSkipped)
	}
}

// TestRunTestsClearCreditUnprovenWhenLenderNoOps is the live shape of the
// SECOND silent path: the lender's OWN field-test value already equals the
// resource's current value, so runFieldTest's no-op detection short-circuits
// BEFORE any patch reaches the backend (see runFieldTest's "No-op
// detection" comment) — the live check for the credit it was supposed to
// prove never gets a post-patch snapshot to read. NO-OP is a non-failing,
// data-dependent outcome invisible to the offline classifier, so before
// this ticket's fix nothing downstream of it ever noticed the credit stayed
// unchecked.
func TestRunTestsClearCreditUnprovenWhenLenderNoOps(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider:       map[string]interface{}{"name": "already-set", "tags": []interface{}{"a"}},
		atProvider:        map[string]interface{}{"name": "already-set", "tags": []interface{}{"a"}},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		// Value repeats forProvider's own current "name" — runFieldTest
		// detects this as a no-op before any patch is built.
		Tests: []manifest.UpdateTest{{Field: "name", Value: "already-set", Clear: []string{"tags"}}},
	}

	results, _, clearViolations, unprovenClear, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].NoOp {
		t.Fatalf("expected the name field test itself to no-op, got %+v", results)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations, want 0 (the live check for a no-op lender never ran at all): %+v", len(clearViolations), clearViolations)
	}
	if len(unprovenClear) != 1 {
		t.Fatalf("got %d unproven clear-credit entries, want exactly 1 (tags' credit rides a no-op lender): %+v", len(unprovenClear), unprovenClear)
	}
	u := unprovenClear[0]
	if u.Representative != "tags" {
		t.Errorf("unproven Representative = %q, want %q", u.Representative, "tags")
	}
	if u.Route != roundtrip.RouteSiblingClear {
		t.Errorf("unproven Route = %q, want %q", u.Route, roundtrip.RouteSiblingClear)
	}
	if u.TriggerField != "name" {
		t.Errorf("unproven TriggerField = %q, want %q (the no-op'd entry whose clear: claimed the credit)", u.TriggerField, "name")
	}
	if u.Reason != UnprovenLenderNoOp {
		t.Errorf("unproven Reason = %q, want %q", u.Reason, UnprovenLenderNoOp)
	}
}

// TestRunTestsClearCreditProvenWhenLenderRunsCleanly is the green-case
// counterpart to the two tests above: a lender that is neither skipped nor
// a no-op runs normally, its live check executes, and — matching every
// other passing clear-credit test in this file — the run reports zero
// unproven credits. A regression that reported every pendingClear entry as
// unproven regardless of whether it was consumed would fail this test
// immediately.
func TestRunTestsClearCreditProvenWhenLenderRunsCleanly(t *testing.T) {
	root := writeClearAssertFixtureCRD(t)
	f := &fakeCluster{
		forProvider:       map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		atProvider:        map[string]interface{}{"name": "old-name", "tags": []interface{}{"a"}},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f).WithRoot(root)

	m := &manifest.Manifest{
		APIVersion: "widgets.crossplane.io/v1alpha1", Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"tags"}}},
	}

	results, _, clearViolations, unprovenClear, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected the name field test itself to pass, got %+v", results)
	}
	if len(clearViolations) != 0 {
		t.Errorf("got %d clear-credit violations, want 0 (tags genuinely emptied): %+v", len(clearViolations), clearViolations)
	}
	if len(unprovenClear) != 0 {
		t.Errorf("got %d unproven clear-credit entries, want 0 (the lender ran cleanly, so its credit was actually checked): %+v", len(unprovenClear), unprovenClear)
	}
}
