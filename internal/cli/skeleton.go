package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/validator"
)

// ExpectSkeletonPlaceholder is the value BuildExpectSkeleton's caller should
// print next to every key it returns. It cannot know the field's real value
// — only which keys a new expect: block would need to name — so callers must
// never print anything a reader could mistake for a resolved value.
const ExpectSkeletonPlaceholder = "TODO"

// BuildExpectSkeleton resolves the exact set of non-omitempty
// Observation-struct keys a NEW update-test expect: block for kind's field
// would need to name, for a nested-composite (object, or list-of-object)
// field of the {kind}Parameters struct declared in typesPath.
//
// It reuses validator's own resolution and incompleteness-detection
// primitives rather than re-deriving struct resolution: it synthesizes a
// single throwaway manifest.Manifest carrying one deliberately-empty
// update-test entry for field — an empty object, the one value that can
// never satisfy any non-omitempty key by construction — and asks
// validator.CheckIncompleteExpectations what it is missing. Every key that
// call reports missing against an empty expectation is, by definition,
// every key a complete expect: block for field would have to name; this
// mirrors exactly what an author would see today by writing "expect: {}" in
// a real manifest and running `validate`, just without needing a real
// manifest or a real create-time spec.forProvider to drive it.
//
// Returns (nil, nil) — no keys, no error — when field's declared shape
// resolves to zero non-omitempty Observation-struct members. That is a
// valid, common outcome (no expect: block is needed at all for this field),
// NOT the same thing as field's Observation-side shape being unresolvable
// altogether; validator.CheckIncompleteExpectations reports both cases
// identically (no finding), so this function cannot tell them apart any
// more than that check already can — see its own doc comment for the list
// of shapes it leaves deliberately unresolved (a dotted field path, a
// scalar or cross-package type, a nested type whose Observation-side shape
// cannot be found at all). Run `validate` against a real manifest naming
// this field for a more specific diagnosis.
//
// Returns an error only when field itself cannot be resolved at all: either
// typesPath does not declare a {kind}Parameters struct, or that struct
// declares no field named field.
func BuildExpectSkeleton(typesPath, kind, field string) ([]string, error) {
	fields, err := validator.ParseGoTypes(typesPath, kind)
	if err != nil {
		return nil, fmt.Errorf("parsing %sParameters from %s: %w", kind, typesPath, err)
	}

	found := false
	for _, f := range fields {
		if f.JSONName == field {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%sParameters in %s declares no field named %q", kind, typesPath, field)
	}

	m := &manifest.Manifest{
		Kind: kind,
		Tests: []manifest.UpdateTest{
			{
				// Deliberately empty: every non-omitempty Observation
				// member is, by construction, absent from it — see the
				// doc comment above.
				Field: field,
				Value: map[string]interface{}{},
			},
		},
	}

	findings := validator.CheckIncompleteExpectations(typesPath, fields, m)
	if len(findings) == 0 {
		return nil, nil
	}

	// Exactly one synthesized entry went in, for exactly this field, so at
	// most one finding can come out.
	keys := append([]string(nil), findings[0].Keys...)
	sort.Strings(keys)
	return keys, nil
}

// FormatExpectSkeleton renders keys as a YAML expect: block fragment, each
// key set to ExpectSkeletonPlaceholder rather than a guessed value. The
// caller (a real update-test entry's expect: block) supplies its own
// indentation and surrounding field:/value: lines — this renders only the
// key: value pairs, sorted, one per line.
//
// An empty keys slice renders to the empty string: the caller decides
// whether that means "print nothing" or "no expect: block is needed", since
// this function has no way to distinguish that outcome from an unresolvable
// field — see BuildExpectSkeleton's doc comment.
//
// keys is sorted before rendering so output is deterministic regardless of
// the order the caller passes them in — BuildExpectSkeleton already returns
// a sorted slice, but this function does not rely on every caller doing
// that.
func FormatExpectSkeleton(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, k := range sorted {
		fmt.Fprintf(&b, "  %s: %s\n", k, ExpectSkeletonPlaceholder)
	}
	return b.String()
}

// PrintExpectSkeleton writes the skeleton for field to w: either the
// rendered expect: block, or a clearly-labelled explanation when there is
// nothing to render — never silence, so a caller piping this into a script
// always gets a line to check.
func PrintExpectSkeleton(w io.Writer, field string, keys []string) {
	if len(keys) == 0 {
		_, _ = fmt.Fprintf(w, "# %s: no non-omitempty Observation-struct keys resolved for this field.\n"+
			"# This means either no expect: block is needed, or the field's Observation-side\n"+
			"# shape could not be resolved at all — run `validate` against a real manifest\n"+
			"# naming this field to tell those two apart.\n", field)
		return
	}
	_, _ = fmt.Fprintf(w, "expect: # skeleton for %s — keys only, fill in the real value(s)\n", field)
	_, _ = fmt.Fprint(w, FormatExpectSkeleton(keys))
}
