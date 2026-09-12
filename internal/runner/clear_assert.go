package runner

import (
	"fmt"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/roundtrip"
)

// ClearAssertion records one container-clear cell's REPRESENTATIVE member
// whose offline credit rides a route the runner itself never directly
// asserts — RouteSiblingClear, RouteSiblingWithValues or
// RouteAncestorTombstone (see roundtrip.ContainerClearCoverage's own doc
// comment for what produces each of the five routes). The other two routes,
// RoutePerKeyNull and RouteSelfTombstone, both credit the entry testing the
// leaf's OWN field, so the ordinary pollField/compareFieldValue assertion
// already observes them — only these three credit a DIFFERENT field's
// patch, which nothing before this type ever checked.
//
// RunTests checks Representative's real status.atProvider value
// immediately after TriggerField's own patch and reconcile — the exact
// point checkAssertUnchanged already reads a fresh snapshot at — and
// reports a violation here exactly when the OBSERVED MIRROR disagrees with
// the credit's own claim that the field emptied. This is deliberately
// never checked against spec: a merge patch Kubernetes accepted but the
// backend silently discarded (the infobloxnios FixedAddress.options shape —
// a submitted empty list the Grid returns 200 to and never actually
// clears) still reads as "cleared" from spec's own point of view, which is
// exactly the self-deceiving check this type exists to refuse. Every
// ClearAssertion RunTests returns is a GATING failure the caller must treat
// the same as a failed field test — see checkClearAssertions.
type ClearAssertion struct {
	// Representative is the credited leaf's own dotted path — the same
	// value roundtrip.ClearCellReport.Representative carries.
	Representative string
	// Route names the credit mechanism offline validation assigned
	// Representative — one of roundtrip.RouteSiblingClear,
	// roundtrip.RouteSiblingWithValues or roundtrip.RouteAncestorTombstone.
	Route roundtrip.ClearRoute
	// TriggerField names the update-test entry whose patch is claimed to
	// have cleared Representative: the sibling Clear/WithValues entry, or
	// the ancestor's own Clear entry — see triggerFieldFor.
	TriggerField string
	// Observed is Representative's actual post-patch value in
	// status.atProvider, stringified the same way every other value in
	// this package is (see stringifyFieldValue). Non-empty here is what
	// makes this a violation — see clearedValue.
	Observed string
}

// pendingClearAssertion is one covered cell's representative, still
// awaiting its live check against the field test that is claimed to have
// cleared it — see unobservedClearCredits.
type pendingClearAssertion struct {
	representative string
	route          roundtrip.ClearRoute
}

// UnprovenClearReason names why a covered cell's offline credit was never
// checked against its live post-state at all — as opposed to ClearAssertion,
// which names a credit that WAS checked and found false. Each value has a
// different remedy, which is why RunTests reports them distinctly rather
// than folding them into one undifferentiated "unproven" label — see
// checkClearAssertions' caller in RunTests for where each is produced.
type UnprovenClearReason string

const (
	// UnprovenLenderSkipped means the update-test entry whose patch was
	// claimed to produce this credit carries a skip: key, so RunTests
	// never ran it and the live check that would have proven the credit
	// never fired. Remedy: rewrite or remove the skip: on the lender.
	UnprovenLenderSkipped UnprovenClearReason = "lender skipped"
	// UnprovenLenderNoOp means the lender ran, but its target value
	// already equalled the resource's current value, so it short-circuited
	// to NO-OP before any patch reached the backend — the live check never
	// had a post-patch snapshot to read. Remedy: change the lender's test
	// value so it actually differs from the resource's current state.
	UnprovenLenderNoOp UnprovenClearReason = "lender no-op"
	// UnprovenNoTrigger means no update-test entry's Clear list or
	// WithValues map could be matched to the credited representative at
	// all — see triggerFieldFor. Remedy: an attribution bug in whichever
	// entry was meant to carry this credit, or in the offline classifier
	// itself.
	UnprovenNoTrigger UnprovenClearReason = "no resolvable trigger"
)

// UnprovenClearCredit records one covered container-clear cell whose
// offline credit (see ClearAssertion's own doc comment for the three
// routes this applies to) was never checked against its live post-state,
// for the reason named by Reason. Like ClearAssertion, this is a GATING
// failure RunTests' caller must treat the same as a failed field test: an
// assertion that never ran proves nothing, and printing the offline
// "covered" verdict on the strength of it would report a check that did
// not happen as though it had — see checkClearAssertions and RunTests.
type UnprovenClearCredit struct {
	// Representative is the credited leaf's own dotted path — the same
	// value roundtrip.ClearCellReport.Representative carries.
	Representative string
	// Route names the credit mechanism offline validation assigned
	// Representative.
	Route roundtrip.ClearRoute
	// TriggerField names the update-test entry whose patch was claimed to
	// prove this credit. Empty when Reason is UnprovenNoTrigger, since no
	// entry could be matched to Representative at all.
	TriggerField string
	Reason       UnprovenClearReason
}

// unassertedClearRoute reports whether route is one of the three credit
// mechanisms whose credited leaf is never itself t.Field for any
// update-test entry the runner executes — see ClearAssertion's own doc
// comment for why RoutePerKeyNull and RouteSelfTombstone are excluded.
func unassertedClearRoute(route roundtrip.ClearRoute) bool {
	switch route {
	case roundtrip.RouteSiblingClear, roundtrip.RouteSiblingWithValues, roundtrip.RouteAncestorTombstone:
		return true
	default:
		return false
	}
}

// clearCellReportsFor computes the SAME container-clear cell verdict
// `validate` computes and prints (roundtrip.ContainerClearCoverage grouped
// by roundtrip.BuildClearCellReport) — reused rather than re-derived so
// `run` and `validate` can never disagree about which cell a leaf occupies
// or which member credits it (see AGENTS.md "a second spelling of the cell
// key is a second answer waiting to diverge" — the same principle
// motivates this call rather than a parallel implementation here).
//
// Returns nil whenever root is empty or no CRD can be found for m —
// exactly like validate's own printContainerClearCells, which silently
// skips the same way: a provider mid-generation with no CRD yet does not
// gain a live check that was never offline-derivable in the first place,
// and RunTests' own caller (WithRoot) already documents that an empty root
// disables this feature entirely.
func clearCellReportsFor(root string, m *manifest.Manifest) []roundtrip.ClearCellReport {
	if root == "" {
		return nil
	}
	crd, _ := roundtrip.FindCRD(root, m.APIVersion, m.Kind)
	if crd == nil {
		return nil
	}
	findings, err := roundtrip.ContainerClearCoverage(crd, m)
	if err != nil {
		return nil
	}
	return roundtrip.BuildClearCellReport(findings)
}

// unobservedClearCredits derives, from the SAME cell reports `validate`
// itself renders, which field test's patch is claimed to have produced
// each covered cell's unasserted credit (see unassertedClearRoute) — so
// RunTests knows exactly when to check the representative's real
// post-state. The map is keyed by TriggerField (see triggerFieldFor); a
// cell with no resolvable trigger cannot be keyed at all, since there is no
// field test to attribute the live check to — every unasserted route is, by
// construction, produced by SOME entry's Clear list or WithValues map, so
// this "should not happen", but an unfalsifiable claim is exactly what the
// second return value exists to stop being: every such cell is returned as
// an UnprovenClearCredit (Reason: UnprovenNoTrigger) rather than dropped,
// so RunTests' caller reports it as a gating failure instead of silence.
func unobservedClearCredits(reports []roundtrip.ClearCellReport, tests []manifest.UpdateTest) (map[string][]pendingClearAssertion, []UnprovenClearCredit) {
	out := make(map[string][]pendingClearAssertion)
	var unresolved []UnprovenClearCredit
	for _, report := range reports {
		if !report.Covered || !unassertedClearRoute(report.Route) {
			continue
		}
		trigger, ok := triggerFieldFor(report.Representative, report.Route, tests)
		if !ok {
			unresolved = append(unresolved, UnprovenClearCredit{
				Representative: report.Representative,
				Route:          report.Route,
				Reason:         UnprovenNoTrigger,
			})
			continue
		}
		out[trigger] = append(out[trigger], pendingClearAssertion{
			representative: report.Representative,
			route:          report.Route,
		})
	}
	return out, unresolved
}

// triggerFieldFor finds the update-test entry whose Clear list or
// WithValues map is the credited route for representative — the entry
// RunTests must check representative's real post-state after, since
// representative is never itself t.Field for an entry credited this way.
// The first matching entry wins, in manifest declaration order, matching
// how roundtrip.ContainerClearCoverage itself walks m.Tests when building
// clearedSiblings/withValuesEmptyList.
func triggerFieldFor(representative string, route roundtrip.ClearRoute, tests []manifest.UpdateTest) (string, bool) {
	for _, t := range tests {
		switch route {
		case roundtrip.RouteSiblingClear, roundtrip.RouteAncestorTombstone:
			for _, c := range t.Clear {
				if c == representative || isAncestorOf(c, representative) {
					return t.Field, true
				}
			}
		case roundtrip.RouteSiblingWithValues:
			if v, ok := t.WithValues[representative]; ok {
				if l, isList := v.([]interface{}); isList && len(l) == 0 {
					return t.Field, true
				}
			}
		}
	}
	return "", false
}

// isAncestorOf reports whether ancestor is a strict dotted-path ancestor of
// leaf — the same segment-exact walk roundtrip.ContainerClearCoverage's own
// clearedAncestor performs, kept here as its own small copy because that
// helper is unexported and this package's use case (finding WHICH entry to
// attribute a representative's live check to) differs from
// ContainerClearCoverage's own (finding WHETHER any ancestor was cleared at
// all). A textual prefix match would wrongly treat "net" as an ancestor of
// "network.subnets"; requiring the trailing "." makes the match
// segment-exact.
func isAncestorOf(ancestor, leaf string) bool {
	return strings.HasPrefix(leaf, ancestor+".")
}

// clearedValue reports whether s — a value already stringified by
// stringifyFieldValue/readSnapshotField — represents a genuinely cleared
// container: absent or explicit null (""), an empty list ("[]"), or an
// empty map ("{}"). These are exactly the post-states every route
// ContainerClearCoverage itself credits can produce (see
// selfTombstoned's own doc comment on the same three shapes). Anything
// else means the backend's mirror still carries real content, so the
// offline credit that assumed the merge patch took effect was wrong.
func clearedValue(s string) bool {
	switch s {
	case "", "[]", "{}":
		return true
	default:
		return false
	}
}

// checkClearAssertions checks every pending clear-credit representative
// whose TriggerField is afterField against snapshot — the post-patch,
// post-reconcile status.atProvider capture RunTests already takes for
// SideFx (see runFieldTest). Assert against the OBSERVED MIRROR, never
// spec: status.atProvider is what the backend actually reports back, while
// spec.forProvider only records what was SUBMITTED — and a submission the
// backend accepts and silently discards (200, no error, no effect) reads
// as "cleared" from spec's own point of view every time, which is not
// evidence of anything. Reading the same snapshot checkAssertUnchanged
// reads means a genuinely-cleared representative and a discarded one are
// told apart the only way that is possible: by what the backend reports
// back, not by what was sent.
//
// A read failure for one item does not stop the others from being
// checked; it is returned as err (the first one encountered), matching
// checkAssertUnchanged's own contract.
func checkClearAssertions(snapshot []byte, pending map[string][]pendingClearAssertion, afterField string) ([]ClearAssertion, error) {
	items := pending[afterField]
	if len(items) == 0 {
		return nil, nil
	}
	var out []ClearAssertion
	var firstErr error
	for _, item := range items {
		cur, _, err := readSnapshotField(snapshot, item.representative)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reading %q: %w", item.representative, err)
			}
			continue
		}
		if !clearedValue(cur) {
			out = append(out, ClearAssertion{
				Representative: item.representative,
				Route:          item.route,
				TriggerField:   afterField,
				Observed:       cur,
			})
		}
	}
	return out, firstErr
}

// unprovenReasonForField reports why field's own update-test entry never
// reached the clear-credit check inside RunTests' loop — called only for a
// pendingClear key RunTests has already determined was never consumed (see
// RunTests). field is drawn from pendingClear's own keys, which
// unobservedClearCredits only ever populates from triggerFieldFor's return —
// itself only ever a t.Field value out of the SAME m.Tests slice RunTests
// loops over — so exactly one TestResult must carry Field == field, and its
// Skipped/NoOp flags are the two states that stop the loop from reaching
// checkClearAssertions for that field (see RunTests' own gating condition,
// `!result.Skipped && !result.NoOp`). A field found neither skipped nor
// no-op'd would already have been consumed and never reach this function at
// all — a case this function does not attempt to represent as a fourth,
// unreachable category, and instead reports as UnprovenLenderNoOp: still a
// real member of that reason's family, since both share the same root cause
// (the lender ran through the gated branch without any error stopping it).
func unprovenReasonForField(field string, results []TestResult) UnprovenClearReason {
	for _, res := range results {
		if res.Field != field {
			continue
		}
		if res.Skipped {
			return UnprovenLenderSkipped
		}
		return UnprovenLenderNoOp
	}
	return UnprovenLenderNoOp
}
