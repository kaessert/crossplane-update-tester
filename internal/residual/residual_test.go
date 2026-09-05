package residual

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// writeFixture writes a minimal manifest carrying body as its
// crossplane.io/update-test annotation value (a YAML block scalar), at
// root/relPath, creating any missing parent directories.
func writeFixture(t *testing.T, root, relPath, body string) string {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating parent dirs for %s: %v", relPath, err)
	}
	indented := "      " + strings.ReplaceAll(strings.TrimRight(body, "\n"), "\n", "\n      ") + "\n"
	src := "apiVersion: widget.example.crossplane.io/v1alpha1\n" +
		"kind: Widget\n" +
		"metadata:\n" +
		"  name: example-widget\n" +
		"  annotations:\n" +
		"    crossplane.io/update-test: |\n" + indented
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
	return path
}

// writePlainFile writes raw content verbatim, for fixtures that need
// something other than writeFixture's single-annotation shape (a manifest
// with no annotation at all, or deliberately malformed YAML).
func writePlainFile(t *testing.T, root, relPath, content string) string {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating parent dirs for %s: %v", relPath, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
	return path
}

// TestScanCountsFixturesAndRows covers the basic shape: a fixture with two
// disposition-carrying skip: entries, a fixture with none (a plain tested
// field, no rows), and a companion file that carries no annotation at all
// and must never be counted.
func TestScanCountsFixturesAndRows(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "widget/a.yaml", `
- field: privateKey
  skip:
    reason: write-only
    disposition: statically-provable
- field: region
  skip:
    reason: write-only
    disposition: one-live-patch
`)
	writeFixture(t, root, "widget/b.yaml", `
- field: name
  value: "updated"
`)
	writePlainFile(t, root, "widget/companion-secret.yaml", `apiVersion: v1
kind: Secret
metadata:
  name: widget-creds
`)

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Counts.FixturesWithAnnotation != 2 {
		t.Errorf("FixturesWithAnnotation = %d, want 2 (companion-secret.yaml has no annotation)", res.Counts.FixturesWithAnnotation)
	}
	if res.Counts.ParsedOK != 2 {
		t.Errorf("ParsedOK = %d, want 2", res.Counts.ParsedOK)
	}
	if len(res.Failures) != 0 {
		t.Errorf("Failures = %#v, want none", res.Failures)
	}

	want := []Row{
		{Fixture: filepath.Join("widget", "a.yaml"), Field: "region", Disposition: manifest.DispositionOneLivePatch},
		{Fixture: filepath.Join("widget", "a.yaml"), Field: "privateKey", Disposition: manifest.DispositionStaticallyProvable},
	}
	if len(res.Rows) != len(want) {
		t.Fatalf("Rows = %#v, want %d entries", res.Rows, len(want))
	}
	// Scan sorts by (disposition rank, fixture, field); statically-provable
	// ranks before one-live-patch in Dispositions, so privateKey comes first.
	wantOrdered := []Row{want[1], want[0]}
	if !reflect.DeepEqual(res.Rows, wantOrdered) {
		t.Errorf("Rows = %#v, want %#v", res.Rows, wantOrdered)
	}
}

// TestScanDirectivePrefixedAnnotationParses covers the shape that broke
// every ad-hoc script: a converge-skip:/assert-unchanged:/ignore-fields:
// directive line preceding the field-entry list. None of the three is
// valid as a sibling of a top-level YAML sequence, so a caller that feeds
// the raw annotation text straight to a generic YAML loader fails here —
// manifest.Parse must not.
func TestScanDirectivePrefixedAnnotationParses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "widget.yaml")
	src := `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
  annotations:
    crossplane.io/update-test: |
      converge-skip: "atProvider.lastSyncTime changes every observe cycle"
      assert-unchanged: legacyRuleList
      ignore-fields: latestBackup
      - field: privateKey
        skip:
          reason: write-only
          disposition: declared-exclusion
          declared-by: a human
          reconfirm: 2027-01-01
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Counts.FixturesWithAnnotation != 1 || res.Counts.ParsedOK != 1 {
		t.Fatalf("Counts = %+v, want both 1 (directive-prefixed form must parse cleanly)", res.Counts)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("Failures = %#v, want none", res.Failures)
	}
	want := []Row{{Fixture: "widget.yaml", Field: "privateKey", Disposition: manifest.DispositionDeclaredExclusion}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", res.Rows, want)
	}
}

// TestScanMalformedFixtureFailsLoudlyNamingFile is the loud-failure proof:
// a fixture whose text carries the annotation key but whose YAML is
// broken must be counted in FixturesWithAnnotation (so the pair goes out
// of balance), must NOT be counted in ParsedOK, and must appear in
// Failures naming the exact file — never silently treated as "no
// annotation" (which would report a clean 0/0 for this fixture) and never
// silently dropped from the count entirely.
func TestScanMalformedFixtureFailsLoudlyNamingFile(t *testing.T) {
	root := t.TempDir()
	good := writeFixture(t, root, "widget/a.yaml", `
- field: privateKey
  skip:
    reason: write-only
    disposition: statically-provable
`)
	bad := writePlainFile(t, root, "widget/b.yaml", `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: broken-widget
  annotations:
    crossplane.io/update-test: [ - field: x
`)

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Counts.FixturesWithAnnotation != 2 {
		t.Errorf("FixturesWithAnnotation = %d, want 2 (the malformed fixture's raw text still carries the key)", res.Counts.FixturesWithAnnotation)
	}
	if res.Counts.ParsedOK != 1 {
		t.Errorf("ParsedOK = %d, want 1 (only the well-formed fixture)", res.Counts.ParsedOK)
	}
	if res.Counts.FixturesWithAnnotation == res.Counts.ParsedOK {
		t.Fatal("Counts pair is balanced, want it to reflect the malformed fixture as a mismatch a caller can detect")
	}

	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %#v, want exactly one", res.Failures)
	}
	if res.Failures[0].Path != bad {
		t.Errorf("Failures[0].Path = %q, want %q (the malformed file must be named)", res.Failures[0].Path, bad)
	}
	if res.Failures[0].Err == nil {
		t.Error("Failures[0].Err = nil, want the underlying parse error")
	}

	// The well-formed sibling fixture must still be scanned — one broken
	// fixture must not blind the scan to everything else.
	want := []Row{{Fixture: filepath.Base(filepath.Dir(good)) + string(filepath.Separator) + filepath.Base(good), Field: "privateKey", Disposition: manifest.DispositionStaticallyProvable}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", res.Rows, want)
	}

	// The report itself must never claim zero rows for the malformed
	// fixture as if it were a clean, empty result.
	var buf bytes.Buffer
	PrintReport(&buf, res)
	out := buf.String()
	if !strings.Contains(out, bad) {
		t.Errorf("PrintReport output does not name the failed fixture %s:\n%s", bad, out)
	}
	if strings.Contains(out, "0 fixture(s) failed to parse") {
		t.Errorf("PrintReport claims zero parse failures despite one: %s", out)
	}
}

// TestScanIgnoresProseMentionsOfTheAnnotationKey confirms a comment that
// merely mentions "crossplane.io/update-test" in passing (never followed
// immediately by a colon at the start of a line) is not mistaken for an
// actual annotation — this is what keeps FixturesWithAnnotation from
// over-counting fixtures that only document the mechanism.
func TestScanIgnoresProseMentionsOfTheAnnotationKey(t *testing.T) {
	root := t.TempDir()
	writePlainFile(t, root, "widget/commented.yaml", `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
  annotations:
    # This resource's update tests are driven by the crossplane.io/update-test
    # annotation on a sibling fixture instead — see widget/other.yaml.
    meta.upbound.io/example-id: widget/v1alpha1/commented
`)

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Counts.FixturesWithAnnotation != 0 {
		t.Errorf("FixturesWithAnnotation = %d, want 0 — a prose mention must not count as the annotation itself", res.Counts.FixturesWithAnnotation)
	}
	if len(res.Rows) != 0 {
		t.Errorf("Rows = %#v, want none", res.Rows)
	}
}

// TestScanSkipsEntriesWithNoDisposition confirms a legacy free-prose skip
// and a structured skip with no disposition: key at all are never counted
// as rows here — this package reports only the evidence-tier axis, not
// every skip: entry in the fleet (that is validator's and roundtrip's own
// job).
func TestScanSkipsEntriesWithNoDisposition(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "widget.yaml", `
- field: legacyField
  skip: "free-prose reason, no structured disposition"
- field: unionField
  skip:
    reason: union-arm
    sibling: otherField
- field: dispositionField
  skip:
    reason: write-only
    disposition: defect
`)

	res, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []Row{{Fixture: "widget.yaml", Field: "dispositionField", Disposition: manifest.DispositionDefect}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("Rows = %#v, want %#v (only the disposition-carrying entry)", res.Rows, want)
	}
}

// TestPrintReportGroupsDeclaredExclusionSeparately confirms the rendered
// report lists declared-exclusion as its own group, distinct from the
// other three dispositions, and never as a bare total.
func TestPrintReportGroupsDeclaredExclusionSeparately(t *testing.T) {
	res := Result{
		Counts: Counts{FixturesWithAnnotation: 1, ParsedOK: 1},
		Rows: []Row{
			{Fixture: "a.yaml", Field: "fieldA", Disposition: manifest.DispositionStaticallyProvable},
			{Fixture: "a.yaml", Field: "fieldB", Disposition: manifest.DispositionDeclaredExclusion},
		},
	}

	var buf bytes.Buffer
	PrintReport(&buf, res)
	out := buf.String()

	spIdx := strings.Index(out, "disposition: statically-provable")
	deIdx := strings.Index(out, "disposition: declared-exclusion")
	if spIdx == -1 || deIdx == -1 {
		t.Fatalf("PrintReport output missing a disposition heading:\n%s", out)
	}
	if !strings.Contains(out, "fixtures_with_annotation=1 parsed_ok=1") {
		t.Errorf("PrintReport does not print the counts pair:\n%s", out)
	}
	if strings.Contains(out, "declared-exclusion (2)") {
		t.Errorf("declared-exclusion row was merged with the other disposition's count:\n%s", out)
	}
}

// TestHasAnnotationKeyLine is a direct table-driven test of the textual
// detector Scan relies on to make FixturesWithAnnotation independent of
// whether the file goes on to parse.
func TestHasAnnotationKeyLine(t *testing.T) {
	cases := map[string]struct {
		reason string
		data   string
		want   bool
	}{
		"IndentedUnderAnnotations": {
			reason: "the real shape: indented under metadata.annotations",
			data:   "metadata:\n  annotations:\n    crossplane.io/update-test: |\n      - field: x\n",
			want:   true,
		},
		"Unindented": {
			reason: "no indentation requirement — only the colon placement matters",
			data:   "crossplane.io/update-test: |\n  - field: x\n",
			want:   true,
		},
		"ProseMentionNoColon": {
			reason: "a sentence mentioning the key without an immediate colon is not a match",
			data:   "    # driven by the crossplane.io/update-test annotation.\n",
			want:   false,
		},
		"Empty": {
			reason: "an empty file has no annotation",
			data:   "",
			want:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hasAnnotationKeyLine([]byte(tc.data)); got != tc.want {
				t.Errorf("%s: hasAnnotationKeyLine = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestScanNonExistentRootIsAnError confirms a bad --root argument surfaces
// as a real error rather than a silent, empty report.
func TestScanNonExistentRootIsAnError(t *testing.T) {
	_, err := Scan(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("Scan over a nonexistent root returned nil error, want one")
	}
}

// TestScanSidecarParity is the residual false-green guard this package's
// own migration gap demanded: a migrated fixture (its update-test
// annotation moved into "<manifest>.yaml.uptest") must report the EXACT
// SAME Counts and Rows as the equivalent un-migrated fixture — never a
// clean ledger that silently stopped seeing it because the walk's
// extension filter can't find the annotation in a ".uptest" file.
func TestScanSidecarParity(t *testing.T) {
	inlineRoot := t.TempDir()
	writeFixture(t, inlineRoot, "widget/a.yaml", `
- field: privateKey
  skip:
    reason: write-only
    disposition: statically-provable
- field: region
  skip:
    reason: write-only
    disposition: one-live-patch
`)

	migratedRoot := t.TempDir()
	writePlainFile(t, migratedRoot, "widget/a.yaml", `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
`)
	writePlainFile(t, migratedRoot, "widget/a.yaml.uptest", `for: widget.example.crossplane.io/v1alpha1/Widget
crossplane.io/update-test: |
  - field: privateKey
    skip:
      reason: write-only
      disposition: statically-provable
  - field: region
    skip:
      reason: write-only
      disposition: one-live-patch
`)

	inlineRes, err := Scan(inlineRoot)
	if err != nil {
		t.Fatalf("Scan(inline): %v", err)
	}
	migratedRes, err := Scan(migratedRoot)
	if err != nil {
		t.Fatalf("Scan(migrated): %v", err)
	}

	if migratedRes.Counts.FixturesWithAnnotation == 0 {
		t.Fatalf("Scan(migrated).Counts.FixturesWithAnnotation = 0 — the exact false-green this guard exists to catch")
	}
	if !reflect.DeepEqual(inlineRes.Counts, migratedRes.Counts) {
		t.Errorf("Counts differ: inline = %+v, migrated = %+v", inlineRes.Counts, migratedRes.Counts)
	}
	if !reflect.DeepEqual(inlineRes.Rows, migratedRes.Rows) {
		t.Errorf("Rows differ: inline = %#v, migrated = %#v", inlineRes.Rows, migratedRes.Rows)
	}
}
