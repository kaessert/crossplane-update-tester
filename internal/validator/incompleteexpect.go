package validator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// IncompleteExpectationFinding records a single update-test entry whose
// effective expectation omits a non-omitempty member of the target field's
// generated Observation struct. A non-omitempty Observation member always
// marshals into status.atProvider — as an explicit null when the API never
// set it — so an expectation that never names the key can never satisfy
// jsonEqual's whole-value comparison, exactly like a MergePatchSiblingFinding
// except this fires even when the member was never present in
// spec.forProvider at create time at all, which CheckMergePatchSiblings has
// no way to see.
type IncompleteExpectationFinding struct {
	// Field is the manifest field name from the update-test entry (the
	// "field:" value).
	Field string
	// Keys are the sorted non-omitempty Observation-struct member name(s)
	// absent from the effective expectation.
	Keys []string
}

// CheckIncompleteExpectations scans m.Tests for entries whose effective
// expectation — "expect:" when present, else "value:", the runner's own
// precedence — is an object, or a list of objects, omitting a top-level key
// that the target field's generated Observation struct declares WITHOUT
// "omitempty". Such a member always marshals in status.atProvider (as an
// explicit null when unset), so its absence from the expectation makes
// jsonEqual's whole-value comparison unsatisfiable by construction — the
// same trap CheckMergePatchSiblings exists for, except this fires even for a
// field being set for the very first time (absent from spec.forProvider
// entirely at create time), which CheckMergePatchSiblings' merge-patch
// simulation structurally cannot see: there is nothing for it to find
// "surviving" when the field never existed to begin with.
//
// It is deliberately conservative, the same bar as CheckObservability and
// CheckMergePatchSiblings: this check's value is that it never cries wolf.
// The following are all left unflagged rather than guessed at:
//   - a "skip:" entry
//   - a dotted field path — resolving a nested field's own nested struct is
//     out of scope, matching CheckObservability
//   - a field the Parameters struct does not declare, or whose declared Go
//     type is not a locally resolvable struct (a scalar, a list of
//     scalars, a map, or a cross-package type) — RFC 7386 replaces a
//     scalar or a list wholesale, so there is no per-member omission hazard
//     to detect for either
//   - a nested type whose Observation-side shape cannot be resolved at all
//     — neither its "<Type>Observation" companion struct, nor a field of
//     the same name declared on "<kind>Observation" itself (see
//     resolveObservationFieldsForKindField)
//   - a member key that CheckMergePatchSiblings' own survivingSiblingKeys
//     would independently flag for the SAME entry — that finding already
//     names the identical remedy (an expect: block, or an explicit null)
//     under the SIBLING-SURVIVES block, and re-reporting it here under a
//     second label would be the same defect reported twice rather than a
//     second, independent one
func CheckIncompleteExpectations(typesPath string, paramFields []FieldInfo, m *manifest.Manifest) []IncompleteExpectationFinding {
	byJSON := make(map[string]FieldInfo, len(paramFields))
	for _, f := range paramFields {
		byJSON[f.JSONName] = f
	}

	// Shared cache shape with CheckObservability's own — a types file with
	// many entries referencing the same nested struct is only parsed (and,
	// on a miss, sibling-file-searched) once per CheckIncompleteExpectations
	// call.
	obsCache := make(observationFieldCache)

	var findings []IncompleteExpectationFinding
	for _, t := range m.Tests {
		if t.Skip != "" || strings.Contains(t.Field, ".") {
			continue
		}

		fi, ok := byJSON[t.Field]
		if !ok {
			continue
		}

		elemType, ok := structElemType(fi.GoType)
		if !ok {
			continue
		}

		obsFields := resolveObservationFieldsForKindField(typesPath, m.Kind, t.Field, elemType, obsCache)
		if obsFields == nil {
			continue
		}

		effective := effectiveExpectation(t)
		effObjs := asObjects(effective)
		if len(effObjs) == 0 {
			continue
		}

		alreadyReported := mergePatchSiblingKeySet(m.ForProvider, t)

		bad := incompleteExpectationKeys(obsFields, effObjs, alreadyReported)
		if len(bad) == 0 {
			continue
		}
		findings = append(findings, IncompleteExpectationFinding{Field: t.Field, Keys: bad})
	}

	return findings
}

// mergePatchSiblingKeySet returns the set of keys CheckMergePatchSiblings'
// own survivingSiblingKeys would flag for the exact same test entry — the
// SIBLING-SURVIVES finding this check must not re-report under a second
// label. It mirrors the same "value: is an object, and the field already
// holds an object in forProvider at create time" gating CheckMergePatchSiblings
// applies, returning an empty set the moment either condition fails, since
// that is exactly when survivingSiblingKeys has nothing to compute either.
func mergePatchSiblingKeySet(forProvider map[string]interface{}, t manifest.UpdateTest) map[string]bool {
	patch, ok := t.Value.(map[string]interface{})
	if !ok {
		return nil
	}

	createVal, ok := navigateForProvider(forProvider, t.Field)
	if !ok {
		return nil
	}
	createObj, ok := createVal.(map[string]interface{})
	if !ok {
		return nil
	}

	keys := survivingSiblingKeys(createObj, patch, effectiveExpectation(t))
	if len(keys) == 0 {
		return nil
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// incompleteExpectationKeys returns the sorted, deduplicated set of
// non-omitempty Observation member names absent from ANY object in
// effObjs (a member missing from even one list element is unsatisfiable for
// that element's own read-back, since every non-omitempty member marshals
// on every element independently), excluding any key already present in
// alreadyReported.
func incompleteExpectationKeys(obsFields []FieldInfo, effObjs []map[string]interface{}, alreadyReported map[string]bool) []string {
	bad := make(map[string]bool)
	for _, f := range obsFields {
		if f.Omitempty {
			continue
		}
		if alreadyReported[f.JSONName] {
			continue
		}
		for _, obj := range effObjs {
			if _, present := obj[f.JSONName]; !present {
				bad[f.JSONName] = true
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	out := make([]string, 0, len(bad))
	for k := range bad {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PrintIncompleteExpectations outputs any IncompleteExpectationFinding
// results to stdout. It is a distinct diagnostic block from
// PrintMergePatchSiblings' SIBLING-SURVIVES report and PrintObservability's
// UNOBSERVABLE report: an entry can pass both of those and still omit a
// non-omitempty Observation member that was never present in spec.forProvider
// at create time at all, so neither of the other two checks has a create-time
// object to reason about.
func PrintIncompleteExpectations(findings []IncompleteExpectationFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Incomplete expectations:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: INCOMPLETE-EXPECT — key(s) %s are non-omitempty members of the generated Observation struct absent from expect:/value:, and will marshal (as null, if unset) regardless; add expect: naming the resolved value\n",
			f.Field, strings.Join(f.Keys, ", "))
	}
	fmt.Println()
	fmt.Println("FAIL: some update-test expectations omit a non-omitempty Observation struct member.")
}
