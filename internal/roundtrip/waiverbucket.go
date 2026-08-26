package roundtrip

import "github.com/kaessert/crossplane-update-tester/internal/manifest"

// WaiverBucket is one of three dispositions for an existing skip: entry,
// machine-derived from its own live row so a fleet's waiver population can
// be triaged without a human re-reading each one.
type WaiverBucket string

const (
	// BucketRedundant marks a waiver whose row classifies equal — cell
	// crediting already covers this field mechanically, so the waiver adds
	// nothing and should be deleted.
	BucketRedundant WaiverBucket = "redundant"
	// BucketFalse marks a waiver whose row is a must-test classification
	// the tool itself disproves — this waiver is actively hiding a real
	// deviation; the disposition is to author the must-test coverage it
	// was hiding, never to simply delete it.
	BucketFalse WaiverBucket = "false"
	// BucketNoRow marks a waiver where either no row exists at all (the
	// field was never populated on either side of the live object), or a
	// row exists but is a CONFIRMED exception DenominatorReport already
	// accepts (a genuinely write-only field, or a CEL-immutable
	// exclusion) — there is no fourth bucket for the latter case, and its
	// disposition is identical: keep the waiver, a structured reason is
	// still required, nothing to delete or author.
	BucketNoRow WaiverBucket = "no-row"
)

// WaiverFinding names one skip:-carrying entry's bucket and why.
type WaiverFinding struct {
	Field  string
	Bucket WaiverBucket
	Detail string
}

// ClassifyWaivers buckets every skip:-carrying entry in m.Tests against its
// own live row, reusing DenominatorReport's own resolution (rather than
// re-deriving it) so a waiver's bucket can never disagree with whether
// DenominatorReport itself would raise a finding for it.
//
// Buckets 1 (redundant) and 2 (false) must never be authored in the same
// change — deleting a false waiver without authoring the test it hid
// REDUCES coverage while looking like progress. This function only
// classifies; keeping the two dispositions apart is the caller's
// responsibility.
func ClassifyWaivers(m *manifest.Manifest, rows []Row) []WaiverFinding {
	byPath := make(map[string]Row, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
	}

	out := make([]WaiverFinding, 0, len(m.Tests))
	for _, t := range m.Tests {
		if !t.Skip.Present() {
			continue
		}

		// Resolve this ONE entry through DenominatorReport itself, so the
		// bucket decision can never diverge from what the enforcing
		// command would actually do with the same waiver.
		single := &manifest.Manifest{Kind: m.Kind, Name: m.Name, Tests: []manifest.UpdateTest{t}}
		findings, _, _ := DenominatorReport(single, rows)
		row, hasRow := byPath[t.Field]

		switch {
		case !hasRow:
			// No row means there is nothing to author a test against —
			// DenominatorReport may still raise a finding here (a legacy
			// free-prose no-row skip needs a structured citation instead),
			// but that is a citation defect, not a disprovable backend
			// behaviour the way BucketFalse means. Every no-row waiver gets
			// one disposition: keep it, a structured reason is still
			// required. Checked BEFORE findings so a legacy no-row skip
			// (which DOES raise a finding) is not misbucketed as "false".
			out = append(out, WaiverFinding{
				Field: t.Field, Bucket: BucketNoRow,
				Detail: "field was never populated on either side of the live object",
			})
		case row.Classification == ClassEqual:
			out = append(out, WaiverFinding{
				Field: t.Field, Bucket: BucketRedundant,
				Detail: "row classifies equal — cell crediting already covers this field mechanically; the waiver is redundant",
			})
		case len(findings) > 0:
			out = append(out, WaiverFinding{Field: t.Field, Bucket: BucketFalse, Detail: findings[0].Detail})
		default:
			out = append(out, WaiverFinding{
				Field: t.Field, Bucket: BucketNoRow,
				Detail: "row classifies " + row.Classification + " and is a CONFIRMED exception (write-only, or CEL-immutable) — keep the waiver",
			})
		}
	}
	return out
}
