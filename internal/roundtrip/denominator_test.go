package roundtrip

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// wantFinding is the compact assertion shape denominatorCases uses to
// describe an expected MustTestFinding without repeating its full Detail
// text in every case — findingsContain below checks Field/Classification
// only, and each case also asserts on a fragment of Detail separately when
// that fragment is the point of the case.
type wantFinding struct {
	field          string
	classification string
}

func findingsContain(findings []MustTestFinding, want wantFinding) bool {
	for _, f := range findings {
		if f.Field == want.field && f.Classification == want.classification {
			return true
		}
	}
	return false
}

// TestDenominatorReportClassificationVerdicts is the ticket's own table:
// every classification, crossed with every skip: shape that could plausibly
// be declared against it, asserting whether DenominatorReport treats the
// combination as resolved or as a finding.
func TestDenominatorReportClassificationVerdicts(t *testing.T) {
	cases := map[string]struct {
		field   string
		row     Row
		skip    manifest.SkipInfo
		wantErr bool // true means a finding is expected for this field
	}{
		"equal with legacy skip is the cheap path": {
			field:   "description",
			row:     Row{Path: "description", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
			skip:    manifest.LegacySkip("low value"),
			wantErr: false,
		},
		"equal with structured union-arm skip is still cheap": {
			field:   "description",
			row:     Row{Path: "description", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
			skip:    manifest.SkipInfo{Reason: manifest.SkipUnionArm, Sibling: "other"},
			wantErr: false,
		},
		"value-changed with legacy skip is a finding": {
			field:   "priority",
			row:     Row{Path: "priority", Classification: ClassValueChanged, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 2},
			skip:    manifest.LegacySkip("looked stable in manual testing"),
			wantErr: true,
		},
		"value-changed with write-only skip is REJECTED, not confirmed": {
			field:   "priority",
			row:     Row{Path: "priority", Classification: ClassValueChanged, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 2},
			skip:    manifest.SkipInfo{Reason: manifest.SkipWriteOnly},
			wantErr: true,
		},
		"defaulted-by-server with vendor-defect skip is still a finding": {
			field:   "region",
			row:     Row{Path: "region", Classification: ClassDefaultedByServer, SpecFound: false, MirrorFound: true, MirrorValue: "us-east"},
			skip:    manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "backend always defaults it", Ticket: "TICK-1"},
			wantErr: true,
		},
		"defaulted-by-server with fixture-missing skip is still a finding": {
			field:   "region",
			row:     Row{Path: "region", Classification: ClassDefaultedByServer, SpecFound: false, MirrorFound: true, MirrorValue: "us-east"},
			skip:    manifest.SkipInfo{Reason: manifest.SkipFixtureMissing, Ticket: "TICK-2"},
			wantErr: true,
		},
		"present-in-spec-absent-from-mirror with write-only skip is CONFIRMED": {
			field:   "privateKey",
			row:     Row{Path: "privateKey", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "secret"},
			skip:    manifest.SkipInfo{Reason: manifest.SkipWriteOnly},
			wantErr: false,
		},
		"present-in-spec-absent-from-mirror with vendor-defect skip is a finding": {
			field:   "privateKey",
			row:     Row{Path: "privateKey", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "secret"},
			skip:    manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e", Ticket: "t"},
			wantErr: true,
		},
		"present-in-spec-absent-from-mirror with legacy skip is a finding": {
			field:   "privateKey",
			row:     Row{Path: "privateKey", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "secret"},
			skip:    manifest.LegacySkip("write-only, trust me"),
			wantErr: true,
		},
		"present-in-mirror-absent-from-spec is out of the denominator regardless of skip": {
			field:   "id",
			row:     Row{Path: "id", Classification: ClassPresentInMirrorAbsentFromSpec, MirrorFound: true, MirrorValue: "srv-1"},
			skip:    manifest.LegacySkip("server-only"),
			wantErr: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{
				Kind: "Widget",
				Name: "example",
				Tests: []manifest.UpdateTest{
					{Field: tc.field, Skip: tc.skip},
				},
			}
			findings, _ := DenominatorReport(m, []Row{tc.row})
			got := findingsContain(findings, wantFinding{field: tc.field, classification: tc.row.Classification})
			if got != tc.wantErr {
				t.Errorf("DenominatorReport finding for %q = %v, want %v (findings: %+v)", tc.field, got, tc.wantErr, findings)
			}
		})
	}
}

// TestDenominatorReportNoRow covers the case DiffReport itself never
// reports at all: a field with a skip: entry but no row because it was
// never populated on either side of the live object.
func TestDenominatorReportNoRow(t *testing.T) {
	cases := map[string]struct {
		skip        manifest.SkipInfo
		wantFinding bool
	}{
		"legacy skip with no row is a finding": {
			skip:        manifest.LegacySkip("optional, never exercised"),
			wantFinding: true,
		},
		"structured union-arm skip with no row is accepted here (its own citation resolves it elsewhere)": {
			skip:        manifest.SkipInfo{Reason: manifest.SkipUnionArm, Sibling: "other"},
			wantFinding: false,
		},
		"structured write-only skip with no row is a finding — its own citation IS a row, and there is none": {
			skip:        manifest.SkipInfo{Reason: manifest.SkipWriteOnly},
			wantFinding: true,
		},
		"structured fixture-missing skip with no row is accepted here": {
			skip:        manifest.SkipInfo{Reason: manifest.SkipFixtureMissing, Ticket: "T-1"},
			wantFinding: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{
				Kind: "Widget",
				Name: "example",
				Tests: []manifest.UpdateTest{
					{Field: "neverSet", Skip: tc.skip},
				},
			}
			// No rows at all — "neverSet" is not observed on either side.
			findings, mustTestCount := DenominatorReport(m, nil)
			if mustTestCount != 0 {
				t.Errorf("mustTestCount = %d, want 0 (no rows at all)", mustTestCount)
			}
			got := findingsContain(findings, wantFinding{field: "neverSet", classification: noRowClassification})
			if got != tc.wantFinding {
				t.Errorf("no-row finding = %v, want %v (findings: %+v)", got, tc.wantFinding, findings)
			}
		})
	}
}

// TestDenominatorReportDirectlyTestedFieldNeverFlagged confirms a field
// with NO skip: entry at all (directly tested via value:/expect:, proven by
// `run`) is never checked against its row, however risky that row's own
// classification looks — DenominatorReport only resolves WAIVERS.
func TestDenominatorReportDirectlyTestedFieldNeverFlagged(t *testing.T) {
	m := &manifest.Manifest{
		Kind: "Widget",
		Name: "example",
		Tests: []manifest.UpdateTest{
			{Field: "priority", Value: 5}, // no Skip at all — directly tested
		},
	}
	rows := []Row{
		{Path: "priority", Classification: ClassValueChanged, SpecFound: true, SpecValue: 5, MirrorFound: true, MirrorValue: 6},
	}
	findings, mustTestCount := DenominatorReport(m, rows)
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none — a directly-tested field carries no skip: to resolve", findings)
	}
	if mustTestCount != 1 {
		t.Errorf("mustTestCount = %d, want 1 — the must-test set is intrinsic to the live row, independent of coverage", mustTestCount)
	}
}

// TestDenominatorReportMustTestCount confirms the must-test set size counts
// every row in the three must-test classes, regardless of how (or whether)
// the manifest covers each field — the number this whole chain has never
// measured live.
func TestDenominatorReportMustTestCount(t *testing.T) {
	rows := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 1},
		{Path: "b", Classification: ClassValueChanged, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 2},
		{Path: "c", Classification: ClassDefaultedByServer, MirrorFound: true, MirrorValue: "x"},
		{Path: "d", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "secret"},
		{Path: "e", Classification: ClassPresentInMirrorAbsentFromSpec, MirrorFound: true, MirrorValue: "srv"},
	}
	m := &manifest.Manifest{Kind: "Widget", Name: "example"}
	_, mustTestCount := DenominatorReport(m, rows)
	if mustTestCount != 3 {
		t.Errorf("mustTestCount = %d, want 3 (b, c, d — a is equal, e is mirror-only)", mustTestCount)
	}
}

// TestDenominatorReportFalsePositiveArm is the ticket's required
// false-positive arm: a resource whose live object mirrors every field
// faithfully (every row equal) must produce ZERO must-test findings,
// whatever skip: shapes its manifest happens to carry.
func TestDenominatorReportFalsePositiveArm(t *testing.T) {
	rows := []Row{
		{Path: "name", Classification: ClassEqual, SpecFound: true, SpecValue: "n", MirrorFound: true, MirrorValue: "n"},
		{Path: "size", Classification: ClassEqual, SpecFound: true, SpecValue: 3, MirrorFound: true, MirrorValue: 3},
		{Path: "tags", Classification: ClassEqual, SpecFound: true, SpecValue: []interface{}{"a"}, MirrorFound: true, MirrorValue: []interface{}{"a"}},
	}
	m := &manifest.Manifest{
		Kind: "Widget",
		Name: "example",
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "n"},
			{Field: "size", Skip: manifest.LegacySkip("low value")},
			{Field: "tags", Skip: manifest.SkipInfo{Reason: manifest.SkipUnionArm, Sibling: "other"}},
		},
	}
	findings, mustTestCount := DenominatorReport(m, rows)
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none — every row is a known-good full mirror (equal)", findings)
	}
	if mustTestCount != 0 {
		t.Errorf("mustTestCount = %d, want 0 — equal is not a must-test classification", mustTestCount)
	}
}

// TestDenominatorReportFindingDetailNamesBothValues confirms a finding's
// Detail carries both the spec and mirror values it observed, so a reader
// never has to re-run the live check to see what was actually returned.
func TestDenominatorReportFindingDetailNamesBothValues(t *testing.T) {
	m := &manifest.Manifest{
		Kind: "Widget",
		Name: "example",
		Tests: []manifest.UpdateTest{
			{Field: "priority", Skip: manifest.LegacySkip("assumed stable")},
		},
	}
	rows := []Row{
		{Path: "priority", Classification: ClassValueChanged, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 2},
	}
	findings, _ := DenominatorReport(m, rows)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly 1", findings)
	}
	d := findings[0].Detail
	if !strings.Contains(d, "spec=1") || !strings.Contains(d, "mirror=2") {
		t.Errorf("Detail = %q, want it to name both spec=1 and mirror=2", d)
	}
}

// TestPrintDenominatorFindingsStatesTheEqualRule asserts the report states,
// in its own emitted text, that equal is the only classification making a
// skip: waiver cheap — required so the rule is visible to a reader of the
// report, not only to a reader of this file's source.
func TestPrintDenominatorFindingsStatesTheEqualRule(t *testing.T) {
	var out strings.Builder
	printFn := func(format string, args ...interface{}) {
		fmt.Fprintf(&out, format, args...)
	}
	PrintDenominatorFindings(printFn, 0, nil)
	got := out.String()
	if !strings.Contains(got, "equal is the only classification") {
		t.Errorf("report = %q, want it to state that equal is the only classification making a waiver cheap", got)
	}
	if !strings.Contains(got, "must-test set size: 0") {
		t.Errorf("report = %q, want it to state the must-test set size", got)
	}
}

// TestPrintDenominatorFindingsListsEveryFinding confirms every finding
// reaches the printed report, each naming its own field.
func TestPrintDenominatorFindingsListsEveryFinding(t *testing.T) {
	var out strings.Builder
	printFn := func(format string, args ...interface{}) {
		fmt.Fprintf(&out, format, args...)
	}
	findings := []MustTestFinding{
		{Field: "priority", Classification: ClassValueChanged, Detail: "must be tested"},
		{Field: "privateKey", Classification: ClassPresentInSpecAbsentFromMirror, Detail: "must be tested, or waived write-only"},
	}
	PrintDenominatorFindings(printFn, 2, findings)
	got := out.String()
	for _, f := range findings {
		if !strings.Contains(got, f.Field) {
			t.Errorf("report = %q, missing finding for field %q", got, f.Field)
		}
	}
	if !strings.Contains(got, "FAIL") {
		t.Errorf("report = %q, want a FAIL line when findings are non-empty", got)
	}
}

// TestDenominatorReportAgainstRealF5xcFixtures runs DenominatorReport over
// the SAME real, trimmed provider-f5xc CRD/object fixtures roundtrip_test.go
// uses (a genuine live object captured during an actual E2E run — see that
// file's own doc comment) rather than synthetic rows, so this ticket's
// must-test set measurement is grounded in real backend behaviour and not
// only in invented data: the AppFirewall fixture measures a must-test set
// of 4 of 12 observed fields (3 present-in-spec-absent-from-mirror, 0
// value-changed/defaulted-by-server), and HttpLoadbalancer measures 4 of 8
// (3 defaulted-by-server, 1 value-changed) — the first live-classification
// numbers this campaign has ever produced.
func TestDenominatorReportAgainstRealF5xcFixtures(t *testing.T) {
	crd := mustDecodeCRD(t, appFirewallCRD)
	obj := mustDecodeObject(t, appFirewallObject)
	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport(AppFirewall): %v", err)
	}

	t.Run("PresentInSpecAbsentFromMirrorConfirmedByWriteOnly", func(t *testing.T) {
		// allowAllResponseCodes is a real present-in-spec-absent-from-mirror
		// row in this fixture — the exact shape write-only exists for.
		m := &manifest.Manifest{Tests: []manifest.UpdateTest{
			{Field: "allowAllResponseCodes", Skip: manifest.SkipInfo{Reason: manifest.SkipWriteOnly}},
		}}
		findings, _ := DenominatorReport(m, rows)
		if len(findings) != 0 {
			t.Errorf("findings = %+v, want none — write-only is confirmed by this real present-in-spec-absent-from-mirror row", findings)
		}
	})

	t.Run("PresentInSpecAbsentFromMirrorRejectsLegacySkip", func(t *testing.T) {
		m := &manifest.Manifest{Tests: []manifest.UpdateTest{
			{Field: "allowAllResponseCodes", Skip: manifest.LegacySkip("assumed write-only")},
		}}
		findings, _ := DenominatorReport(m, rows)
		if !findingsContain(findings, wantFinding{field: "allowAllResponseCodes", classification: ClassPresentInSpecAbsentFromMirror}) {
			t.Errorf("findings = %+v, want a finding for allowAllResponseCodes — a free-prose skip: is not write-only", findings)
		}
	})

	t.Run("EqualFieldIsCheapWhateverTheReason", func(t *testing.T) {
		m := &manifest.Manifest{Tests: []manifest.UpdateTest{
			{Field: "description", Skip: manifest.LegacySkip("low value")},
		}}
		findings, _ := DenominatorReport(m, rows)
		if len(findings) != 0 {
			t.Errorf("findings = %+v, want none — description is a real equal row", findings)
		}
	})

	t.Run("MustTestSetSizeMatchesTheMeasuredFigure", func(t *testing.T) {
		_, mustTestCount := DenominatorReport(&manifest.Manifest{}, rows)
		if mustTestCount != 4 {
			t.Errorf("mustTestCount = %d, want 4 (this fixture's measured must-test set: 3 present-in-spec-absent-from-mirror, 0 value-changed/defaulted-by-server)", mustTestCount)
		}
	})
}
