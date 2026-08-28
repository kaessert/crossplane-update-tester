package roundtrip

import "github.com/kaessert/crossplane-update-tester/internal/manifest"

// WaiverBucket is one of two dispositions for an existing skip: entry,
// machine-derived from its own live row so a fleet's waiver population can
// be triaged without a human re-reading each one.
type WaiverBucket string

const (
	// BucketFalse marks a waiver whose row is a must-test classification
	// the tool itself disproves — this waiver is actively hiding a real
	// deviation; the disposition is to author the must-test coverage it
	// was hiding, never to simply delete it.
	BucketFalse WaiverBucket = "false"
	// BucketKeep marks a waiver that is not disprovable, covering three
	// distinct inputs that all share one disposition: no row exists at all
	// (the field was never populated on either side of the live object); a
	// row exists and is a CONFIRMED exception DenominatorReport already
	// accepts (a genuinely write-only field, or a CEL-immutable exclusion);
	// or a row exists and classifies equal — a value set at CREATE
	// round-tripped, which says nothing about whether the UPDATE path the
	// waiver guards can be exercised at all. Keep the waiver in every case;
	// its cost and priority are its evidence tier, not this bucket.
	BucketKeep WaiverBucket = "keep"
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
				Field: t.Field, Bucket: BucketKeep,
				Detail: "field was never populated on either side of the live object",
			})
		case len(findings) > 0:
			out = append(out, WaiverFinding{Field: t.Field, Bucket: BucketFalse, Detail: findings[0].Detail})
		default:
			// Every row that reaches here is kept: a CONFIRMED exception
			// (write-only, or CEL-immutable) at zero ongoing cost, or a row
			// that classifies equal — a value set at CREATE round-tripped,
			// which says nothing about whether the UPDATE path the waiver
			// guards can be exercised at all, so equal is never grounds to
			// delete it. The disposition is identical either way; only the
			// evidence tier behind the waiver differs, and that is not
			// this function's concern.
			out = append(out, WaiverFinding{
				Field: t.Field, Bucket: BucketKeep,
				Detail: "row classifies " + row.Classification + " — keep the waiver",
			})
		}
	}
	return out
}
