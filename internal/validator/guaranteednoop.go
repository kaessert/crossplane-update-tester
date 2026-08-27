package validator

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// GuaranteedNoOpFinding records a single update-test entry whose "value:"
// deep-equals the value already present at the entry's field path in the
// manifest's own create-time spec.forProvider. A merge patch that repeats a
// value the API server already holds makes no change for it to persist —
// metadata.generation never bumps, and the controller's Update() path is
// never invoked. runner.go's own pre-patch guard (jsonEqual(t.Value,
// before) in runFieldTest) catches exactly this at run time and aborts the
// field test before ever building the patch; this check answers the same
// question offline, from the manifest alone, before a live run burns a
// cluster and an admission slot re-discovering something the manifest
// already said.
type GuaranteedNoOpFinding struct {
	// Field is the manifest field name from the update-test entry
	// ("field:").
	Field string
	// Value is the entry's own "value:" — printed back so the finding is
	// self-contained without a second manifest lookup.
	Value interface{}
}

// CheckGuaranteedNoOp scans m.Tests for a non-skipped entry whose "value:"
// is the same shape already present at the entry's field path in
// m.ForProvider, the manifest's own create-time spec.forProvider.
//
// It compares t.Value — never the effective expectation ("expect:" when
// present) — because that is exactly what runner.go's runFieldTest compares
// against the live pre-patch read: jsonEqual(t.Value, before). "expect:"
// only ever changes what the POST-patch poll accepts; it plays no part in
// the guard that runs BEFORE the patch is even built, so substituting it
// here would make this check disagree with the runtime verdict it exists to
// predict.
//
// It is deliberately conservative, the same bar as this package's other
// offline checks: an entry is flagged only when EVERY earlier non-skipped
// entry left the field's top-level name untouched — neither as its own
// "field:", named in its "clear:" list, nor named in its "withValues:" map.
// Entries run in order against one live resource, so an earlier entry
// naming the same top-level field — by any of those three routes — may
// have changed what is actually there by the time THIS entry's patch is
// built. This check has no way to know whether that earlier entry
// converged, so it declines to guess and leaves the later entry unflagged
// rather than risk a false positive — see topLevelField.
//
// A field absent from m.ForProvider at create time (ok=false from
// navigateForProvider) is left unflagged: there is nothing to compare
// against, and a currently-absent field is by definition not what the
// patch would repeat.
func CheckGuaranteedNoOp(m *manifest.Manifest) []GuaranteedNoOpFinding {
	var findings []GuaranteedNoOpFinding
	touched := make(map[string]bool)

	for _, t := range m.Tests {
		if t.Skip.Present() {
			continue
		}

		top := topLevelField(t.Field)
		if !touched[top] {
			if createVal, ok := navigateForProvider(m.ForProvider, t.Field); ok && guaranteedNoOpEqual(t.Value, createVal) {
				findings = append(findings, GuaranteedNoOpFinding{Field: t.Field, Value: t.Value})
			}
		}

		touched[top] = true
		for _, c := range t.Clear {
			touched[topLevelField(c)] = true
		}
		for sibling := range t.WithValues {
			touched[topLevelField(sibling)] = true
		}
	}

	return findings
}

// topLevelField returns the first dot-separated segment of field — the
// granularity "clear:" and the merge-patch builder both operate at (see
// manifest.ValidateClear: clear only ever names a top-level sibling), and so
// the granularity CheckGuaranteedNoOp's preceding-entry guard tracks.
func topLevelField(field string) string {
	if idx := strings.IndexByte(field, '.'); idx != -1 {
		return field[:idx]
	}
	return field
}

// guaranteedNoOpEqual reports whether value and createTime describe the same
// JSON shape, normalising both through a JSON marshal/unmarshal round trip
// first so a type difference introduced purely by how each side was decoded
// (both sides here come from the SAME manifest, decoded by the same YAML
// parser, but the normalisation costs nothing and keeps this helper correct
// even if that ever stops being true) does not produce a false negative.
// This mirrors the normalisation runner.go's own jsonEqual applies to its
// "expected" argument — the same pre-patch comparison this check exists to
// predict without a live cluster — adapted to compare two already-decoded
// Go values against each other instead of a decoded value against a raw
// string read back from kubectl.
func guaranteedNoOpEqual(value, createTime interface{}) bool {
	vBytes, vErr := json.Marshal(value)
	cBytes, cErr := json.Marshal(createTime)
	if vErr != nil || cErr != nil {
		return reflect.DeepEqual(value, createTime)
	}

	var vNorm, cNorm interface{}
	if json.Unmarshal(vBytes, &vNorm) != nil || json.Unmarshal(cBytes, &cNorm) != nil {
		return reflect.DeepEqual(value, createTime)
	}
	return reflect.DeepEqual(vNorm, cNorm)
}

// PrintGuaranteedNoOp outputs any GuaranteedNoOpFinding results to stdout,
// as its own labelled block distinct from every other validate check: an
// entry can be fully covered, fully observable, leave no sibling surviving
// and no expectation incomplete, and still never exercise the Update() path
// at all, because the value being sent was already there.
func PrintGuaranteedNoOp(findings []GuaranteedNoOpFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Guaranteed no-ops:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: GUARANTEED-NO-OP — value: already equals the create-time spec.forProvider value at this field, so the merge patch changes nothing, metadata.generation never bumps, and Update() is never invoked; change value: to something the resource does not already hold\n",
			f.Field)
	}
	fmt.Println()
	fmt.Println("FAIL: some update-test entries repeat the field's own create-time value and can never exercise the update path.")
}
