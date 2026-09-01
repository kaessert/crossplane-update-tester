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
			{Field: "vendorField", Skip: manifest.SkipInfo{Reason: manifest.SkipVendorDefect, Evidence: "e"}},
			{Field: "fixtureField", Skip: manifest.SkipInfo{Reason: manifest.SkipFixtureMissing, Evidence: "e"}},
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
// by: relative to hop N-1's OWN directory, never the origin manifest's.
// origin.yaml and mid.yaml are placed at DIFFERENT DEPTHS on purpose —
// origin.yaml one level down (root/a/), mid.yaml three levels down
// (root/a/deeper/b/) — so the two candidate base directories cannot resolve
// mid's own by: to the same place. mid's by: ("../../final.yaml") walks up
// two levels from mid's OWN directory to reach root/a/final.yaml. Walking up
// two levels from origin's directory instead lands entirely outside the
// temp tree (no such file), which manifest.Parse then reports as an error —
// so a resolver that (incorrectly) reused origin's directory for every hop
// fails this test loudly rather than passing it by coincidence.
func TestCheckSkipReasonsCoveredElsewhereChainResolvesPerHop(t *testing.T) {
	root := t.TempDir()
	originDir := filepath.Join(root, "a")
	midDir := filepath.Join(root, "a", "deeper", "b")
	mustMkdirAll(t, originDir)
	mustMkdirAll(t, midDir)

	writeSkipReasonFixture(t, originDir, "final.yaml", `      - field: comment
        value: "directly tested here"`)
	writeSkipReasonFixture(t, midDir, "mid.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../../final.yaml#comment`)
	originPath := writeSkipReasonFixture(t, originDir, "origin.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: deeper/b/mid.yaml#comment`)

	m, err := manifest.Parse(originPath)
	if err != nil {
		t.Fatalf("parsing origin manifest: %v", err)
	}

	findings := CheckSkipReasons(m, nil)
	if len(findings) != 0 {
		t.Fatalf("expected the chain to resolve hop-by-hop, got %+v", findings)
	}
}

// TestCheckSkipReasonsCoveredElsewhereReusedSpellingChainIsNotAFalseCycle is
// the exact four-manifest reproduction of the false-cycle regression this
// fix closes: a/origin.yaml and b/origin2.yaml both spell their by: as
// "target.yaml#comment", but each resolves (per its own directory) to a
// DIFFERENT target.yaml. A resolver keyed on the raw by: string reports a
// cycle here even though the chain visits four distinct files and ends at
// one that is genuinely directly tested.
func TestCheckSkipReasonsCoveredElsewhereReusedSpellingChainIsNotAFalseCycle(t *testing.T) {
	root := t.TempDir()
	groupA := filepath.Join(root, "a")
	groupB := filepath.Join(root, "b")
	mustMkdirAll(t, groupA)
	mustMkdirAll(t, groupB)

	writeSkipReasonFixture(t, groupB, "target.yaml", `      - field: comment
        value: "directly tested here"`)
	writeSkipReasonFixture(t, groupB, "origin2.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: target.yaml#comment`)
	writeSkipReasonFixture(t, groupA, "target.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: ../b/origin2.yaml#comment`)
	originPath := writeSkipReasonFixture(t, groupA, "origin.yaml", `      - field: comment
        skip:
          reason: covered-elsewhere
          by: target.yaml#comment`)

	m, err := manifest.Parse(originPath)
	if err != nil {
		t.Fatalf("parsing origin manifest: %v", err)
	}

	findings := CheckSkipReasons(m, nil)
	if len(findings) != 0 {
		t.Fatalf("expected no findings — four distinct files, no real cycle — got %+v", findings)
	}
}

// TestResolveCoveredElsewhereSameSpellingDifferentTargetIsNotACycle is a
// white-box test of resolveCoveredElsewhere's seen-keying directly: it
// pre-seeds seen with the RAW by: spelling "target.yaml#comment" — the key
// the pre-fix code used — and then resolves that exact same spelling from a
// DIFFERENT base directory, landing on a genuinely different, directly
// tested file. A resolver keyed on the raw spelling would report this as a
// cycle; one keyed on the resolved absolute path must not, because the two
// "target.yaml#comment" occurrences never refer to the same file.
func TestResolveCoveredElsewhereSameSpellingDifferentTargetIsNotACycle(t *testing.T) {
	root := t.TempDir()
	groupA := filepath.Join(root, "a")
	mustMkdirAll(t, groupA)
	writeSkipReasonFixture(t, groupA, "target.yaml", `      - field: comment
        value: "directly tested here"`)

	// Simulate a prior hop that visited a DIFFERENT "target.yaml" (e.g. one
	// resolved from groupB) by seeding the pre-fix raw-string key alone —
	// deliberately NOT the resolved absolute path of groupA's target.yaml.
	seen := map[string]bool{"target.yaml#comment": true}

	if err := resolveCoveredElsewhere("origin", "target.yaml#comment", groupA, seen); err != nil {
		t.Fatalf("expected no cycle for a spelling that resolves to an unvisited file, got: %v", err)
	}
}

// TestResolveCoveredElsewhereDifferentSpellingSameTargetIsACycle is the
// other half: two DIFFERENT by: spellings that resolve to the SAME absolute
// file+field must still be caught as a cycle. seen is pre-populated with the
// RESOLVED absolute path a prior hop reached; resolving a differently
// spelled relative by: that lands on that same absolute path must fail.
func TestResolveCoveredElsewhereDifferentSpellingSameTargetIsACycle(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	mustMkdirAll(t, sub)
	resolvedTarget := filepath.Join(sub, "target.yaml")

	seen := map[string]bool{resolvedTarget + "#comment": true}

	// baseDir + "../sub/target.yaml" resolves to the exact same absolute
	// path already in seen, despite the by: string never having been
	// spelled "target.yaml#comment" before.
	err := resolveCoveredElsewhere("origin", "../sub/target.yaml#comment", filepath.Join(root, "other"), seen)
	if err == nil {
		t.Fatal("expected a cycle error for a differently-spelled by: resolving to an already-visited file, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %q, want it to name a cycle", err.Error())
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
