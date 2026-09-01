package roundtrip

import (
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestClassifyWaiversTwoBucketTable covers the two-bucket table directly:
// keep (an equal row, a never-populated row, or a CONFIRMED exception),
// and false (on a must-test row the tool disproves). Also covers the two
// CONFIRMED exceptions (write-only, CEL-immutable) that this function
// folds into keep alongside the equal and no-row inputs, rather than a
// separate bucket each.
func TestClassifyWaiversTwoBucketTable(t *testing.T) {
	cases := map[string]struct {
		row        Row
		skip       manifest.SkipInfo
		wantBucket WaiverBucket
	}{
		"equal row is keep": {
			row:        Row{Path: "name", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
			skip:       manifest.LegacySkip("low value"),
			wantBucket: BucketKeep,
		},
		"value-changed row is false": {
			row:        Row{Path: "region", Classification: ClassValueChanged, SpecFound: true, SpecValue: "us", MirrorFound: true, MirrorValue: "eu"},
			skip:       manifest.LegacySkip("looked stable"),
			wantBucket: BucketFalse,
		},
		"defaulted-by-server row is false": {
			row:        Row{Path: "az", Classification: ClassDefaultedByServer, MirrorFound: true, MirrorValue: "az-1"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e"},
			wantBucket: BucketFalse,
		},
		"present-in-spec-absent-from-mirror without write-only is false": {
			row:        Row{Path: "secret", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e"},
			wantBucket: BucketFalse,
		},
		"present-in-spec-absent-from-mirror with confirmed write-only is keep": {
			row:        Row{Path: "secret", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipWriteOnly},
			wantBucket: BucketKeep,
		},
		"CEL-immutable present-in-spec-absent-from-mirror is keep": {
			row:        Row{Path: "id", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s", Immutable: true},
			skip:       manifest.LegacySkip("immutable field"),
			wantBucket: BucketKeep,
		},
		// AC-2 closure: a CEL-immutable field whose row ALSO classifies
		// equal (self == oldSelf, and the CREATE-time value round-tripped)
		// must reach keep, not the disposition that used to be redundant.
		// This is the vultr 3 / tailscale 2 / vclustercli 4 population.
		"CEL-immutable AND equal row is keep, not the retired disposition": {
			row:        Row{Path: "region", Classification: ClassEqual, SpecFound: true, SpecValue: "nyc1", MirrorFound: true, MirrorValue: "nyc1", Immutable: true},
			skip:       manifest.LegacySkip("immutable field"),
			wantBucket: BucketKeep,
		},
		// The plain population: an equal-and-mutable row with an ordinary
		// skip: entry (no immutability, no write-only reason) also reaches
		// keep — this is the population that previously produced the only
		// true bucket-1 (redundant) candidate in the fleet.
		"equal-and-mutable row with an ordinary skip is keep": {
			row:        Row{Path: "userScheme", Classification: ClassEqual, SpecFound: true, SpecValue: "legacy", MirrorFound: true, MirrorValue: "legacy"},
			skip:       manifest.LegacySkip("assumed stable"),
			wantBucket: BucketKeep,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{
				Kind: "Widget", Name: "example",
				Tests: []manifest.UpdateTest{{Field: tc.row.Path, Skip: tc.skip}},
			}
			out := ClassifyWaivers(m, []Row{tc.row})
			if len(out) != 1 {
				t.Fatalf("ClassifyWaivers returned %d findings, want 1: %+v", len(out), out)
			}
			if out[0].Bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q (detail: %s)", out[0].Bucket, tc.wantBucket, out[0].Detail)
			}
		})
	}
}

// TestClassifyWaiversNoRowIsKeep confirms a skip:-carrying entry whose
// field was never populated on either side of the live object is bucketed
// keep.
func TestClassifyWaiversNoRowIsKeep(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "neverSeen", Skip: manifest.LegacySkip("optional")},
		},
	}
	out := ClassifyWaivers(m, nil)
	if len(out) != 1 || out[0].Bucket != BucketKeep {
		t.Errorf("ClassifyWaivers(no row) = %+v, want one BucketKeep finding", out)
	}
}

// TestClassifyWaiversSkipsEntriesWithNoSkip confirms a directly-tested
// entry (no skip: key at all) is never classified into any bucket — only
// waivers are bucketed at all.
func TestClassifyWaiversSkipsEntriesWithNoSkip(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "x"}, // no Skip at all
		},
	}
	rows := []Row{{Path: "name", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"}}
	out := ClassifyWaivers(m, rows)
	if len(out) != 0 {
		t.Errorf("ClassifyWaivers on a directly-tested (non-skip) entry = %+v, want empty", out)
	}
}

// TestClassifyWaiversRealisticMixProducesBothBuckets confirms a manifest
// carrying one waiver of each real-world shape is classified into exactly
// the two buckets this package defines, with the CORRECT bucket for each.
func TestClassifyWaiversRealisticMixProducesBothBuckets(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "description", Skip: manifest.LegacySkip("low value")},        // keep (equal)
			{Field: "priority", Skip: manifest.LegacySkip("assumed stable")},      // false
			{Field: "computedOnly", Skip: manifest.LegacySkip("never populated")}, // keep (no row)
		},
	}
	rows := []Row{
		{Path: "description", Classification: ClassEqual, SpecFound: true, SpecValue: "d", MirrorFound: true, MirrorValue: "d"},
		{Path: "priority", Classification: ClassValueChanged, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 2},
	}

	out := ClassifyWaivers(m, rows)
	got := map[string]WaiverBucket{}
	for _, f := range out {
		got[f.Field] = f.Bucket
	}
	want := map[string]WaiverBucket{
		"description":  BucketKeep,
		"priority":     BucketFalse,
		"computedOnly": BucketKeep,
	}
	for field, bucket := range want {
		if got[field] != bucket {
			t.Errorf("field %q bucket = %q, want %q", field, got[field], bucket)
		}
	}
}
