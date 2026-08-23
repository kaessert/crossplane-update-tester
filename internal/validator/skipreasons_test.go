package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// writeSkipReasonFixture writes a minimal manifest carrying the given
// update-test annotation body to name inside dir, returning its absolute
// path — the shape resolveCoveredElsewhere's "by:" pointer needs, since
// manifest.Parse reads a real file from disk.
func writeSkipReasonFixture(t *testing.T, dir, name, annotationBody string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := fmt.Sprintf(`apiVersion: example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: %s
  annotations:
    crossplane.io/update-test: |
%s
spec:
  forProvider: {}
`, strings.TrimSuffix(name, ".yaml"), annotationBody)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
	return path
}

// TestCheckSkipReasonsUnionArm covers both directions of union-arm's offline
// resolution: a sibling that IS declared on the target struct resolves
// clean, and one that is NOT is flagged naming both fields.
func TestCheckSkipReasonsUnionArm(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
		{GoName: "DefaultBotSetting", JSONName: fieldArmB},
	}

	cases := map[string]struct {
		reason      string
		sibling     string
		wantFinding bool
	}{
		"SiblingDeclared": {
			reason:      "defaultBotSetting is a real field on WidgetParameters",
			sibling:     fieldArmB,
			wantFinding: false,
		},
		"SiblingNotDeclared": {
			reason:      "thirdArmSetting does not exist on WidgetParameters",
			sibling:     fieldArmC,
			wantFinding: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{
				Kind: kindWidget,
				Tests: []manifest.UpdateTest{
					{Field: fieldArmA, Skip: manifest.SkipInfo{Reason: manifest.SkipUnionArm, Sibling: tc.sibling}},
				},
			}

			findings := CheckSkipReasons(m, fields)
			if got := len(findings) > 0; got != tc.wantFinding {
				t.Fatalf("%s: got findings %+v, wantFinding=%v", tc.reason, findings, tc.wantFinding)
			}
			if tc.wantFinding {
				f := findings[0]
				if f.Field != fieldArmA {
					t.Errorf("%s: finding.Field = %q, want %q", tc.reason, f.Field, fieldArmA)
				}
				if !strings.Contains(f.Detail, fieldArmA) || !strings.Contains(f.Detail, tc.sibling) {
					t.Errorf("%s: finding.Detail = %q, want it to name both %q and %q", tc.reason, f.Detail, fieldArmA, tc.sibling)
				}
			}
		})
	}
}

// TestCheckSkipReasonsCoveredElsewhere covers covered-elsewhere's offline
// resolution: a target manifest whose named field is directly tested
// resolves clean; one that is missing, itself skipped, or forms a cycle back
// to a manifest#field pair already in the chain, is flagged.
func TestCheckSkipReasonsCoveredElsewhere(t *testing.T) {
	dir := t.TempDir()

	testedPath := writeSkipReasonFixture(t, dir, "tested.yaml", `      - field: comment
        value: "already exercised here"`)
	missingFieldPath := writeSkipReasonFixture(t, dir, "missing-field.yaml", `      - field: other
        value: "unrelated"`)
	skippedPath := writeSkipReasonFixture(t, dir, "skipped.yaml", `      - field: comment
        skip: "not exercised here either"`)

	t.Run("ResolvesToDirectlyTestedEntry", func(t *testing.T) {
		m := &manifest.Manifest{
			Kind: kindWidget,
			Tests: []manifest.UpdateTest{
				{Field: fieldComment, Skip: manifest.SkipInfo{
					Reason: manifest.SkipCoveredElsewhere,
					By:     testedPath + "#comment",
				}},
			},
		}
		if findings := CheckSkipReasons(m, nil); len(findings) != 0 {
			t.Errorf("expected no findings, got %+v", findings)
		}
	})

	t.Run("TargetManifestMissingFile", func(t *testing.T) {
		m := &manifest.Manifest{
			Kind: kindWidget,
			Tests: []manifest.UpdateTest{
				{Field: fieldComment, Skip: manifest.SkipInfo{
					Reason: manifest.SkipCoveredElsewhere,
					By:     filepath.Join(dir, "does-not-exist.yaml") + "#comment",
				}},
			},
		}
		findings := CheckSkipReasons(m, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %+v", findings)
		}
	})

	t.Run("TargetHasNoEntryForField", func(t *testing.T) {
		m := &manifest.Manifest{
			Kind: kindWidget,
			Tests: []manifest.UpdateTest{
				{Field: fieldComment, Skip: manifest.SkipInfo{
					Reason: manifest.SkipCoveredElsewhere,
					By:     missingFieldPath + "#comment",
				}},
			},
		}
		findings := CheckSkipReasons(m, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %+v", findings)
		}
		if !strings.Contains(findings[0].Detail, "no update-test entry") {
			t.Errorf("finding.Detail = %q, want it to say the target has no entry for the field", findings[0].Detail)
		}
	})

	t.Run("TargetEntryItselfSkipped", func(t *testing.T) {
		m := &manifest.Manifest{
			Kind: kindWidget,
			Tests: []manifest.UpdateTest{
				{Field: fieldComment, Skip: manifest.SkipInfo{
					Reason: manifest.SkipCoveredElsewhere,
					By:     skippedPath + "#comment",
				}},
			},
		}
		findings := CheckSkipReasons(m, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %+v", findings)
		}
		if !strings.Contains(findings[0].Detail, "itself skipped") {
			t.Errorf("finding.Detail = %q, want it to say the target is itself skipped", findings[0].Detail)
		}
	})

	t.Run("CycleIsDetected", func(t *testing.T) {
		// a.yaml#comment points at b.yaml#comment, which points back at
		// a.yaml#comment — a two-hop cycle.
		aPath := filepath.Join(dir, "a.yaml")
		bPath := filepath.Join(dir, "b.yaml")
		writeSkipReasonFixture(t, dir, "a.yaml", fmt.Sprintf(`      - field: comment
        skip:
          reason: covered-elsewhere
          by: %s#comment`, bPath))
		writeSkipReasonFixture(t, dir, "b.yaml", fmt.Sprintf(`      - field: comment
        skip:
          reason: covered-elsewhere
          by: %s#comment`, aPath))

		m := &manifest.Manifest{
			Kind: kindWidget,
			Tests: []manifest.UpdateTest{
				{Field: fieldComment, Skip: manifest.SkipInfo{
					Reason: manifest.SkipCoveredElsewhere,
					By:     aPath + "#comment",
				}},
			},
		}
		findings := CheckSkipReasons(m, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %+v", findings)
		}
		if !strings.Contains(findings[0].Detail, "cycle") {
			t.Errorf("finding.Detail = %q, want it to name a cycle", findings[0].Detail)
		}
	})
}

// TestCheckSkipReasonsIgnoresLegacyAndOtherReasons confirms CheckSkipReasons
// only resolves union-arm and covered-elsewhere: a legacy free-prose skip:,
// and the three reasons with no offline resolution (vendor-defect,
// fixture-missing, write-only), are never flagged.
func TestCheckSkipReasonsIgnoresLegacyAndOtherReasons(t *testing.T) {
	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{Field: "legacyField", Skip: manifest.LegacySkip("free prose, nothing to check")},
			{Field: "vendorField", Skip: manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e", Ticket: "TCK-000001"}},
			{Field: "fixtureField", Skip: manifest.SkipInfo{Reason: manifest.SkipFixtureMissing, Ticket: "TCK-000002"}},
			{Field: "writeOnlyField", Skip: manifest.SkipInfo{Reason: manifest.SkipWriteOnly}},
		},
	}

	if findings := CheckSkipReasons(m, nil); len(findings) != 0 {
		t.Errorf("expected no findings for legacy/vendor-defect/fixture-missing/write-only entries, got %+v", findings)
	}
}
