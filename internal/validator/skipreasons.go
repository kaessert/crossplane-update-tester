package validator

import (
	"fmt"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// SkipReasonFinding names one structured "skip:" entry whose claim
// CheckSkipReasons can check offline did not hold. Unlike a MISSING or
// clear-target-unknown FieldValidation, this is not a coverage gap — the
// field DOES carry a skip: entry — it is the entry's own reason failing the
// specific check that reason promises.
type SkipReasonFinding struct {
	// Field is the manifest field name from the update-test entry (the
	// "field:" value) that declared the skip: reason under check.
	Field string
	// Detail explains what failed to resolve, naming every field or
	// manifest path involved.
	Detail string
}

// CheckSkipReasons resolves the two structured skip: reasons whose claim can
// be checked without touching a live cluster:
//
//   - union-arm: sibling: must name a field present in the SAME Parameters
//     struct (fields, from this manifest's own generated Go types).
//   - covered-elsewhere: by: must resolve, following any chain of further
//     covered-elsewhere entries, to a manifest and field that is itself
//     directly tested — not skipped in any form, not missing, and never
//     revisiting a manifest#field pair already seen in the same chain.
//
// vendor-defect, fixture-missing and write-only need no offline resolution
// here — see manifest.SkipInfo's own parse-time checks (validateSkipInfo)
// for what those three reasons already require of their own text.
func CheckSkipReasons(m *manifest.Manifest, fields []FieldInfo) []SkipReasonFinding {
	fieldSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldSet[f.JSONName] = true
	}

	var findings []SkipReasonFinding
	for _, t := range m.Tests {
		if !t.Skip.Present() || t.Skip.Legacy {
			continue
		}
		switch t.Skip.Reason {
		case manifest.SkipUnionArm:
			if !fieldSet[t.Skip.Sibling] {
				findings = append(findings, SkipReasonFinding{
					Field: t.Field,
					Detail: fmt.Sprintf(
						"skip: reason union-arm on %q names sibling %q, which is not a declared field on %sParameters",
						t.Field, t.Skip.Sibling, m.Kind),
				})
			}
		case manifest.SkipCoveredElsewhere:
			if err := resolveCoveredElsewhere(t.Field, t.Skip.By, nil); err != nil {
				findings = append(findings, SkipReasonFinding{Field: t.Field, Detail: err.Error()})
			}
		}
	}
	return findings
}

// resolveCoveredElsewhere follows a "covered-elsewhere" chain starting at by
// (shaped "<path>#<field>"), failing when the target manifest cannot be
// parsed, the field has no direct entry there, that entry is itself skipped
// in any form, or the chain revisits a "<path>#<field>" pair already seen —
// a cycle.
//
// seen accumulates "<path>#<field>" keys across the whole chain starting
// from originField's own by:; pass nil on the initial call.
func resolveCoveredElsewhere(originField, by string, seen map[string]bool) error {
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[by] {
		return fmt.Errorf(
			"skip: reason covered-elsewhere on %q has a cycle: %q is already in this chain", originField, by)
	}
	seen[by] = true

	path, field, ok := strings.Cut(by, "#")
	if !ok || path == "" || field == "" {
		return fmt.Errorf(
			"skip: reason covered-elsewhere on %q: by: %q must be shaped \"<path>#<field>\"", originField, by)
	}

	target, err := manifest.Parse(path)
	if err != nil {
		return fmt.Errorf("skip: reason covered-elsewhere on %q: resolving by: %q: %w", originField, by, err)
	}

	var entry *manifest.UpdateTest
	for i := range target.Tests {
		if target.Tests[i].Field == field {
			entry = &target.Tests[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf(
			"skip: reason covered-elsewhere on %q: %s has no update-test entry for field %q", originField, by, field)
	}

	if !entry.Skip.Present() {
		return nil // directly tested — the chain resolves.
	}
	if !entry.Skip.Legacy && entry.Skip.Reason == manifest.SkipCoveredElsewhere {
		return resolveCoveredElsewhere(originField, entry.Skip.By, seen)
	}
	return fmt.Errorf(
		"skip: reason covered-elsewhere on %q: %s is itself skipped (not directly tested)", originField, by)
}

// PrintSkipReasons outputs any SkipReasonFinding results to stdout, as its
// own labelled block distinct from every other validate check: a field can
// be fully covered by a structured skip: entry and still make a claim that
// entry's own reason does not back up.
func PrintSkipReasons(findings []SkipReasonFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Unresolved skip: reasons:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: %s\n", f.Field, f.Detail)
	}
	fmt.Println()
	fmt.Println("FAIL: some skip: reasons do not resolve against this manifest's own declared coverage.")
}
