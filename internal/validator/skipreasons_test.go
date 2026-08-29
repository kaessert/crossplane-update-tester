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

// TestCheckSkipReasonsCoveredElsewhereManifestRelative covers the defect
// this resolution rule was rewritten to fix: a "by:" value written the way
// every provider example manifest actually writes it — "../<sibling
// dir>/<file>.yaml#field", relative to the manifest THAT CONTAINS the
// reference — must resolve, exactly like a
// uptest.upbound.io/post-assert-hook path in the same annotation block
// already does. Before the fix, "by:" was resolved against the
// update-tester process's own working directory instead, so this exact
// shape failed whenever the process was not started from the referring
// manifest's own directory.
func TestCheckSkipReasonsCoveredElsewhereManifestRelative(t *testing.T) {
	root := t.TempDir()
	group1 := filepath.Join(root, "examples", "group1")
	group2 := filepath.Join(root, "examples", "group2")
	mustMkdirAll(t, group1)
	mustMkdirAll(t, group2)

	writeSkipReasonFixture(t, group2, "target.yaml", `      - field: comment
        value: "already exercised here"`)
	originPath := writeSkipReasonFixture(t, group1, "origin.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../group2/target.yaml#comment`)

	m, err := manifest.Parse(originPath)
	if err != nil {
		t.Fatalf("parsing origin manifest: %v", err)
	}

	if findings := CheckSkipReasons(m, nil); len(findings) != 0 {
		t.Fatalf("expected the manifest-relative by: to resolve cleanly, got %+v", findings)
	}
}

// TestCheckSkipReasonsCoveredElsewhereIsCWDIndependent pins the specific
// regression this rewrite closes: the SAME manifest, reached via two
// differently-shaped relative arguments from two different process working
// directories, must resolve identically. Before the fix, "by:" resolution
// depended on the process's own working directory rather than on the
// referring manifest's location, so this test would previously pass from
// one of the two working directories and fail from the other — see the
// resolveCoveredElsewhere doc comment for why manifest-relative resolution
// removes that dependency.
func TestCheckSkipReasonsCoveredElsewhereIsCWDIndependent(t *testing.T) {
	root := t.TempDir()
	group1 := filepath.Join(root, "examples", "group1")
	group2 := filepath.Join(root, "examples", "group2")
	mustMkdirAll(t, group1)
	mustMkdirAll(t, group2)

	writeSkipReasonFixture(t, group2, "target.yaml", `      - field: comment
        value: "already exercised here"`)
	writeSkipReasonFixture(t, group1, "origin.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../group2/target.yaml#comment`)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting starting working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatalf("restoring working directory to %s: %v", origWD, err)
		}
	})

	cases := map[string]struct {
		cwd     string
		relPath string
	}{
		"FromRepoRoot":     {cwd: root, relPath: filepath.Join("examples", "group1", "origin.yaml")},
		"FromExamplesDir":  {cwd: filepath.Join(root, "examples"), relPath: filepath.Join("group1", "origin.yaml")},
		"FromReferringDir": {cwd: group1, relPath: "origin.yaml"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.Chdir(tc.cwd); err != nil {
				t.Fatalf("chdir to %s: %v", tc.cwd, err)
			}
			defer func() {
				if err := os.Chdir(origWD); err != nil {
					t.Fatalf("restoring working directory to %s: %v", origWD, err)
				}
			}()

			m, err := manifest.Parse(tc.relPath)
			if err != nil {
				t.Fatalf("parsing %s from cwd %s: %v", tc.relPath, tc.cwd, err)
			}
			if findings := CheckSkipReasons(m, nil); len(findings) != 0 {
				t.Fatalf("cwd=%s: expected no findings, got %+v", tc.cwd, findings)
			}
		})
	}
}

// TestCheckSkipReasonsCoveredElsewhereChainResolvesPerHop pins the chain
// half of the fix: hop N of a covered-elsewhere chain must resolve its own
// by: relative to hop N-1's directory, not the origin manifest's. mid.yaml
// sits one directory level deeper than origin.yaml relative to final.yaml,
// so a by: value that (incorrectly) stayed anchored to origin.yaml's
// directory for every hop would walk one level too far up and fail to find
// final.yaml at all.
func TestCheckSkipReasonsCoveredElsewhereChainResolvesPerHop(t *testing.T) {
	root := t.TempDir()
	aDir := filepath.Join(root, "group", "a")
	bDir := filepath.Join(root, "group", "b")
	cDir := filepath.Join(root, "c")
	mustMkdirAll(t, aDir)
	mustMkdirAll(t, bDir)
	mustMkdirAll(t, cDir)

	writeSkipReasonFixture(t, cDir, "final.yaml", `      - field: comment
        value: "directly tested here"`)
	writeSkipReasonFixture(t, bDir, "mid.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../../c/final.yaml#comment`)
	originPath := writeSkipReasonFixture(t, aDir, "origin.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../b/mid.yaml#comment`)

	m, err := manifest.Parse(originPath)
	if err != nil {
		t.Fatalf("parsing origin manifest: %v", err)
	}

	findings := CheckSkipReasons(m, nil)
	if len(findings) != 0 {
		t.Fatalf("expected the chain to resolve hop-by-hop, got %+v", findings)
	}
}

// mustMkdirAll creates dir (and any missing parents), failing the test on
// error.
func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating directory %s: %v", dir, err)
	}
}
