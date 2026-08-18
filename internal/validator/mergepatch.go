package validator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// MergePatchSiblingFinding records a single update-test entry whose
// partial-object "value:" leaves one or more top-level sibling keys of the
// create-time spec.forProvider object surviving the RFC 7386 merge patch the
// runner actually applies (`kubectl patch --type=merge`, see
// runner.buildMergePatch). The runner's own comparison afterwards
// (jsonEqual) is a whole-value reflect.DeepEqual against the raw
// value:/expect: object, so a survivor the effective expectation does not
// also name makes that comparison unsatisfiable by construction — the field
// test always polls to timeout.
type MergePatchSiblingFinding struct {
	// Field is the manifest field name from the update-test entry (the
	// "field:" value).
	Field string
	// Keys are the sorted top-level key(s), present in the create-time
	// object and untouched by the patch, that survive the merge without
	// being accounted for in the effective expectation.
	Keys []string
}

// CheckMergePatchSiblings scans m.Tests for entries whose "value:" patches
// an object-valued field, where the SAME field already holds an object in
// m.ForProvider (the manifest's own spec.forProvider at create time), and
// the create-time object carries a top-level key the patch never mentions —
// a key an RFC 7386 merge patch preserves verbatim rather than replacing.
//
// It is deliberately conservative, the same bar as CheckObservability: this
// check's value is that it never cries wolf. The following are all left
// unflagged rather than guessed at:
//   - a "skip:" entry
//   - a scalar or list "value:" — RFC 7386 replaces a list wholesale, so
//     there is no partial-merge hazard to detect
//   - a field absent from m.ForProvider at create time, or whose create-time
//     value is not itself an object (e.g. still server-defaulted to nothing)
//   - a top-level key the patch DOES mention, whether to overwrite it or to
//     clear it with an explicit null — either way the key is addressed, not
//     left to survive unaddressed
//   - a survivor key already named in an "expect:" block, whatever its
//     value — that is this check's own remedy for the non-union form of
//     the trap, and the check must not fight it. The runner compares
//     against status.atProvider, not a spec-derived merge simulation, so a
//     correct expect: block can legitimately record a server-back-filled
//     value this check has no way to predict; a wrong-but-present value is
//     an ordinary expectation mismatch the live run catches, not this
//     check's job to adjudicate
//
// Everything needed to decide this is in the manifest itself — the
// "crossplane.io/update-test" annotation and the same file's
// spec.forProvider — so this check runs entirely offline, with no generated
// types file and no live cluster.
func CheckMergePatchSiblings(m *manifest.Manifest) []MergePatchSiblingFinding {
	var findings []MergePatchSiblingFinding
	for _, t := range m.Tests {
		if t.Skip != "" {
			continue
		}

		patch, ok := t.Value.(map[string]interface{})
		if !ok {
			// Scalar or list — RFC 7386 replaces either wholesale, so
			// there is no sibling to survive.
			continue
		}

		createVal, ok := navigateForProvider(m.ForProvider, t.Field)
		if !ok {
			continue
		}
		createObj, ok := createVal.(map[string]interface{})
		if !ok {
			continue
		}

		bad := survivingSiblingKeys(createObj, patch, effectiveExpectation(t))
		if len(bad) == 0 {
			continue
		}
		findings = append(findings, MergePatchSiblingFinding{Field: t.Field, Keys: bad})
	}
	return findings
}

// effectiveExpectation returns the value the runner actually compares the
// post-patch field against: "expect:" when present, else "value:" — the
// same precedence runner.go's runFieldTest applies.
func effectiveExpectation(t manifest.UpdateTest) interface{} {
	if t.Expect != nil {
		return t.Expect
	}
	return t.Value
}

// survivingSiblingKeys returns the sorted top-level keys of createObj that
// the patch never mentions (neither to overwrite nor to null out) and that
// the effective expectation does not separately name.
//
// Presence in the effective expectation is enough — the same standard
// already applied to patch above. The check cannot predict a server
// back-filled value (the runner compares against status.atProvider, not
// this function's spec-derived merge simulation), so it has no authority to
// adjudicate the value; it only confirms the key was accounted for at all.
func survivingSiblingKeys(createObj, patch map[string]interface{}, effective interface{}) []string {
	effObj, _ := effective.(map[string]interface{})

	var bad []string
	for key := range createObj {
		if _, addressed := patch[key]; addressed {
			// The patch names this key — either a new value or an
			// explicit null. RFC 7386 overwrites or deletes it; it is
			// never left to survive unaddressed.
			continue
		}
		if effObj != nil {
			if _, ok := effObj[key]; ok {
				// The author already named this key in the effective
				// expectation — the remedy this check exists to push
				// toward. Its value is not this check's to adjudicate.
				continue
			}
		}
		bad = append(bad, key)
	}
	sort.Strings(bad)
	return bad
}

// navigateForProvider walks forProvider along a dot-separated field path and
// returns the value found there. This mirrors the dotted-path convention the
// update-test annotation and the runner's own navigateSpecForProvider use
// elsewhere in this tool, so a nested field name resolves the same way here
// as it does at patch time. Returns ok=false the moment the path runs into a
// non-object or a missing key — including when forProvider itself is nil,
// which happens for a manifest with no spec.forProvider at all.
func navigateForProvider(forProvider map[string]interface{}, field string) (interface{}, bool) {
	var cur interface{} = forProvider
	for _, part := range strings.Split(field, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, exists := m[part]
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// PrintMergePatchSiblings outputs any MergePatchSiblingFinding results to
// stdout. It is a distinct diagnostic block from both PrintValidation's
// MISSING report and PrintObservability's UNOBSERVABLE report: an entry can
// be fully "covered" and fully observable while its merge result still
// diverges from what the runner will ever compare it against.
//
// The surviving key names are printed but the remedy is not prescribed —
// whether the right fix is an expect: block recording the merged shape, or
// an explicit null clearing a mutually-exclusive sibling member, depends on
// whether the field is a union, which this check cannot determine from the
// manifest alone.
func PrintMergePatchSiblings(findings []MergePatchSiblingFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Merge-patch sibling survivors:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: SIBLING-SURVIVES — key(s) %s are absent from value: and will survive the RFC 7386 merge patch unaddressed; either add expect: recording the merged shape, or set the surviving key(s) to null if they are a mutually exclusive alternative being replaced\n",
			f.Field, strings.Join(f.Keys, ", "))
	}
	fmt.Println()
	fmt.Println("FAIL: some update-test entries leave a sibling key surviving an RFC 7386 merge patch unaddressed.")
}
