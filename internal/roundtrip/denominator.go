package roundtrip

import (
	"fmt"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// mustTestStatement is printed once at the top of every denominator report
// so the rule this file enforces is visible next to the verdict it
// produces, not only in source comments a reader of the report never sees.
const mustTestStatement = "must-test rule: equal is the only classification a skip: waiver is cheap against; " +
	"value-changed and defaulted-by-server may never be waived (any reason, including write-only); " +
	"present-in-spec-absent-from-mirror may be waived only by write-only; " +
	"a field with no live row at all cannot use the cheap equal path and needs a structured (non-legacy) skip: reason"

// noRowClassification is a synthetic classification MustTestFinding uses
// for a field that carries a skip: entry but was never populated on either
// side of the live object — DiffReport itself never reports such a field at
// all (see its own doc comment), so there is no roundtrip.Row to point to.
const noRowClassification = "no-row"

// MustTestFinding names one field whose live roundtrip observation conflicts
// with what its manifest "skip:" entry claims — either a classification that
// may never be waived at all, a write-only claim a live row disproves, or a
// waiver resting on a field nothing has ever observed.
type MustTestFinding struct {
	// Field is the update-test entry's own "field:" value.
	Field string
	// Classification is the roundtrip classification the finding is about,
	// or noRowClassification when DiffReport produced no row for Field at
	// all.
	Classification string
	// Detail explains what failed and names both observed values (when a
	// row exists) so a reader never has to re-run the live check to see
	// what was actually returned.
	Detail string
}

// mustTestClassSet is the three classifications a field must be genuinely
// tested against unless the one narrower exception below applies — see the
// package doc comment on DiffReport's own five-way classification.
func isMustTestClass(classification string) bool {
	switch classification {
	case ClassValueChanged, ClassDefaultedByServer, ClassPresentInSpecAbsentFromMirror:
		return true
	default:
		return false
	}
}

// isWriteOnly reports whether t's skip: entry is the structured write-only
// reason — the one exception that can rescue a
// present-in-spec-absent-from-mirror field from being a must-test finding.
// A legacy free-prose skip never counts, even when its text happens to say
// "write-only": only the structured, closed-set reason is checked here.
func isWriteOnly(t manifest.UpdateTest) bool {
	return !t.Skip.Legacy && t.Skip.Reason == manifest.SkipWriteOnly
}

// DenominatorReport derives the must-test set from rows — DiffReport's
// classification of every field observed on m's own live object — and
// checks every "skip:"-carrying entry in m.Tests against its own row.
//
// mustTestCount is the size of the must-test set itself: every row whose
// classification is value-changed, defaulted-by-server or
// present-in-spec-absent-from-mirror, counted regardless of whether the
// manifest currently tests, skips, or omits that field entirely — the
// number this package's own doc comment says nobody has ever measured
// live, because every prior figure was static.
//
// findings lists every skip:-carrying entry whose waiver this function can
// disprove against rows:
//
//   - value-changed / defaulted-by-server: ANY skip: entry at all is a
//     finding — these two classifications are exactly what this tool exists
//     to catch (silent drops, value normalization, defaulting), so no
//     reason code rescues them, including write-only: a field that changed
//     or was defaulted round-trips through the mirror by definition, which
//     already disproves write-only's own claim.
//   - present-in-spec-absent-from-mirror: a skip: entry is a finding UNLESS
//     its reason is the structured write-only — that is the one case this
//     row shape confirms.
//   - equal: never a finding, whatever the skip: entry says — the cheap
//     path.
//   - present-in-mirror-absent-from-spec: not a spec field, so a manifest
//     test entry cannot legitimately name one; nothing to check here even
//     if one somehow does.
//   - no row at all (the field was never populated on either side of the
//     live object): a legacy free-prose skip: is a finding — with no row to
//     confirm anything, a free-prose sentence is exactly the unchecked
//     claim this whole chain exists to stop crediting cheaply. write-only is
//     ALSO a finding here, even though it is structured: its citation IS a
//     present-in-spec-absent-from-mirror row (validateSkipInfo requires no
//     other companion key from it), so a no-row field gives it nothing to
//     resolve against. The other four structured reasons (union-arm,
//     covered-elsewhere, vendor-defect, fixture-missing) are accepted here —
//     each carries its own citation (sibling:/by:/evidence:+ticket:/
//     ticket:) that validator.CheckSkipReasons resolves independently of any
//     live row.
//
// A field with no skip: entry at all (directly tested, or genuinely
// uncovered) is untouched here — direct testing is already proven by `run`,
// and uncovered fields are already reported MISSING by validator.ValidateManifest.
func DenominatorReport(m *manifest.Manifest, rows []Row) (findings []MustTestFinding, mustTestCount int) {
	byPath := make(map[string]Row, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
		if isMustTestClass(r.Classification) {
			mustTestCount++
		}
	}

	for _, t := range m.Tests {
		if !t.Skip.Present() {
			continue
		}

		row, hasRow := byPath[t.Field]
		if !hasRow {
			switch {
			case t.Skip.Legacy:
				findings = append(findings, MustTestFinding{
					Field:          t.Field,
					Classification: noRowClassification,
					Detail: "field was never populated on either side of the live object (no spec.forProvider " +
						"value, no status.atProvider mirror) — a free-prose skip: cannot rely on the empirically-cheap " +
						"equal path here; cite a structured skip: reason instead",
				})
			case isWriteOnly(t):
				// write-only is the one structured reason with no citation
				// of its own — validateSkipInfo requires no companion key
				// for it, because its whole claim is resolved against a
				// live present-in-spec-absent-from-mirror row (see the
				// case below). With no row at all there is nothing to
				// confirm the claim against, so it cannot be accepted
				// here the way the other four structured reasons are: a
				// no-row field's write-only claim is exactly the
				// unconfirmable case this function exists to close —
				// deleting the field from the example would otherwise
				// re-earn this exact waiver for free.
				findings = append(findings, MustTestFinding{
					Field:          t.Field,
					Classification: noRowClassification,
					Detail: "field was never populated on either side of the live object, so skip: " +
						"{reason: write-only} has no row to confirm its claim against — this reason resolves " +
						"only against a present-in-spec-absent-from-mirror row",
				})
			}
			// Every other structured reason (union-arm, covered-elsewhere,
			// vendor-defect, fixture-missing) carries its own citation
			// (sibling:/by:/evidence:+ticket:/ticket:) that validator's
			// offline checks resolve independently of any live row — see
			// validator.CheckSkipReasons and manifest.validateSkipInfo.
			continue
		}

		switch row.Classification {
		case ClassEqual:
			// The cheap path — accepted under any reason, structured or
			// legacy.
		case ClassPresentInSpecAbsentFromMirror:
			if isWriteOnly(t) {
				continue // CONFIRMED — this is the shape write-only claims.
			}
			findings = append(findings, MustTestFinding{
				Field:          t.Field,
				Classification: row.Classification,
				Detail: fmt.Sprintf(
					"must be tested, or waived skip: {reason: write-only} — spec=%s mirror=%s",
					formatValue(row.SpecFound, row.SpecValue), formatValue(row.MirrorFound, row.MirrorValue)),
			})
		case ClassValueChanged, ClassDefaultedByServer:
			if isWriteOnly(t) {
				findings = append(findings, MustTestFinding{
					Field:          t.Field,
					Classification: row.Classification,
					Detail: fmt.Sprintf(
						"skip: reason write-only REJECTED — this field IS mirrored: spec=%s mirror=%s",
						formatValue(row.SpecFound, row.SpecValue), formatValue(row.MirrorFound, row.MirrorValue)),
				})
				continue
			}
			findings = append(findings, MustTestFinding{
				Field:          t.Field,
				Classification: row.Classification,
				Detail: fmt.Sprintf(
					"must be tested — no skip: reason may waive this classification: spec=%s mirror=%s",
					formatValue(row.SpecFound, row.SpecValue), formatValue(row.MirrorFound, row.MirrorValue)),
			})
		case ClassPresentInMirrorAbsentFromSpec:
			// Not a spec field at all — out of the denominator.
		}
	}

	return findings, mustTestCount
}

// PrintDenominatorFindings renders mustTestStatement, the must-test set
// size, and every finding to stdout via printFn (os.Stdout in production,
// a buffer under test).
func PrintDenominatorFindings(printFn func(format string, args ...interface{}), mustTestCount int, findings []MustTestFinding) {
	printFn("%s\n", mustTestStatement)
	printFn("must-test set size: %d field(s)\n", mustTestCount)

	if len(findings) == 0 {
		printFn("All must-test fields are either genuinely tested or hold a confirmed waiver.\n")
		return
	}

	printFn("\nUnresolved must-test findings:\n")
	for _, f := range findings {
		printFn("  ✗ %s (%s): %s\n", f.Field, f.Classification, f.Detail)
	}
	printFn("\nFAIL: some must-test fields carry a waiver this live run cannot confirm.\n")
}
