package roundtrip

import (
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestClassifyWaiversThreeBucketTable covers the three-bucket table directly:
// redundant (on an equal row), false (on a must-test row the tool
// disproves), no-row (never populated). Also covers the two CONFIRMED
// exceptions (write-only, CEL-immutable) that this function folds into
// no-row's "keep" disposition rather than a fourth bucket.
func TestClassifyWaiversThreeBucketTable(t *testing.T) {
	cases := map[string]struct {
		row        Row
		skip       manifest.SkipInfo
		wantBucket WaiverBucket
	}{
		"equal row is redundant": {
			row:        Row{Path: "name", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
			skip:       manifest.LegacySkip("low value"),
			wantBucket: BucketRedundant,
		},
		"value-changed row is false": {
			row:        Row{Path: "region", Classification: ClassValueChanged, SpecFound: true, SpecValue: "us", MirrorFound: true, MirrorValue: "eu"},
			skip:       manifest.LegacySkip("looked stable"),
			wantBucket: BucketFalse,
		},
		"defaulted-by-server row is false": {
			row:        Row{Path: "az", Classification: ClassDefaultedByServer, MirrorFound: true, MirrorValue: "az-1"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e", Ticket: "t"},
			wantBucket: BucketFalse,
		},
		"present-in-spec-absent-from-mirror without write-only is false": {
			row:        Row{Path: "secret", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e", Ticket: "t"},
			wantBucket: BucketFalse,
		},
		"present-in-spec-absent-from-mirror with confirmed write-only is no-row (keep)": {
			row:        Row{Path: "secret", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s"},
			skip:       manifest.SkipInfo{Reason: manifest.SkipWriteOnly},
			wantBucket: BucketNoRow,
		},
		"CEL-immutable present-in-spec-absent-from-mirror is no-row (keep)": {
			row:        Row{Path: "id", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s", Immutable: true},
			skip:       manifest.LegacySkip("immutable field"),
			wantBucket: BucketNoRow,
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

// TestClassifyWaiversNoRowBucket confirms a skip:-carrying entry whose
// field was never populated on either side of the live object is bucketed
// no-row.
func TestClassifyWaiversNoRowBucket(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "neverSeen", Skip: manifest.LegacySkip("optional")},
		},
	}
	out := ClassifyWaivers(m, nil)
	if len(out) != 1 || out[0].Bucket != BucketNoRow {
		t.Errorf("ClassifyWaivers(no row) = %+v, want one BucketNoRow finding", out)
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

// TestClassifyWaiversRealisticMixProducesAllThreeBuckets confirms a
// manifest carrying one waiver of each real-world shape is classified into
// exactly the three buckets this package defines, with the CORRECT bucket
// for each.
func TestClassifyWaiversRealisticMixProducesAllThreeBuckets(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "description", Skip: manifest.LegacySkip("low value")},        // redundant
			{Field: "priority", Skip: manifest.LegacySkip("assumed stable")},      // false
			{Field: "computedOnly", Skip: manifest.LegacySkip("never populated")}, // no-row
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
		"description":  BucketRedundant,
		"priority":     BucketFalse,
		"computedOnly": BucketNoRow,
	}
	for field, bucket := range want {
		if got[field] != bucket {
			t.Errorf("field %q bucket = %q, want %q", field, got[field], bucket)
		}
	}
}
