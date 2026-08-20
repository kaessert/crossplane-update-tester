package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// errTestKubectlUnavailable stands in for a real kubectl failure (e.g. the
// object was deleted between convergeAssert and this function's own
// GetObject re-read) — RoundtripFindingsForFailures never sees this
// directly, since a target whose Result.Err is set is excluded before any
// cluster access is attempted.
var errTestKubectlUnavailable = errors.New("kubectl unavailable")

// testRoundtripCRD is a minimal CRD fixture matching testKindExample /
// testAPIVersionRoundtrip, with one forProvider field the object under
// test sets but the mirror never echoes back (a present-in-spec-absent-
// from-mirror finding — the same shape as the F5 XC oneof-selector defect
// this whole feature exists to surface).
const (
	testAPIVersionRoundtrip = "example.crossplane.io/v1alpha1"
	testGroupRoundtrip      = "example.crossplane.io"
)

const testRoundtripCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: example.crossplane.io
  names:
    kind: ExampleResource
    plural: exampleresources
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  zone:
                    type: string
                  neverEchoed:
                    type: object
                    nullable: true
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  zone:
                    type: string
`

// writeRoundtripCRDFixture writes testRoundtripCRD under
// <dir>/package/crds/ and returns dir, ready to pass as
// RoundtripFindingsForFailures' root argument.
func writeRoundtripCRDFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	crdsDir := filepath.Join(dir, "package", "crds")
	if err := os.MkdirAll(crdsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(crdsDir, "example.crossplane.io_exampleresources.yaml"), []byte(testRoundtripCRD), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// newFailedResult builds the (target, result) pair for a target whose
// convergence check genuinely failed — Err nil, Result non-nil, neither
// Skipped nor Passed — the only shape RoundtripFindingsForFailures ever
// looks up a CRD for.
func newFailedTargetAndResult(label string, f *fakeCluster) (ConvergeTarget, ConvergeAllResult) {
	r := newFakeRunner(f)
	target := ConvergeTarget{
		Label:  label,
		Runner: r,
		Manifest: &manifest.Manifest{
			Kind:       testKindExample,
			Name:       testNameExample,
			APIVersion: testAPIVersionRoundtrip,
		},
	}
	result := ConvergeAllResult{
		Label:  label,
		Result: &ConvergeResult{Passed: false, Message: "atProvider drifted"},
	}
	return target, result
}

func TestRoundtripFindingsForFailuresEmptyRootSkipsEverything(t *testing.T) {
	f := &fakeCluster{forProvider: map[string]interface{}{"zone": "a", "neverEchoed": map[string]interface{}{}}, atProvider: map[string]interface{}{"zone": "a"}}
	target, result := newFailedTargetAndResult("ExampleResource/x", f)

	got := RoundtripFindingsForFailures([]ConvergeTarget{target}, []ConvergeAllResult{result}, "")
	if len(got) != 0 {
		t.Errorf("findings = %+v, want empty map when root is unset — this must never touch the cluster with no root to look up a CRD under", got)
	}
}

func TestRoundtripFindingsForFailuresReportsAGenuineFailure(t *testing.T) {
	root := writeRoundtripCRDFixture(t)
	f := &fakeCluster{
		forProvider: map[string]interface{}{"zone": "a", "neverEchoed": map[string]interface{}{}},
		atProvider:  map[string]interface{}{"zone": "a"},
	}
	target, result := newFailedTargetAndResult("ExampleResource/x", f)

	findings := RoundtripFindingsForFailures([]ConvergeTarget{target}, []ConvergeAllResult{result}, root)

	text, ok := findings["ExampleResource/x"]
	if !ok {
		t.Fatalf("findings has no entry for the failing target; findings=%+v", findings)
	}
	if !strings.Contains(text, "present-in-spec-absent-from-mirror") || !strings.Contains(text, "neverEchoed") {
		t.Errorf("findings text = %q, want it to name the present-in-spec-absent-from-mirror finding for neverEchoed", text)
	}
	if !strings.Contains(text, "field(s) classified") {
		t.Errorf("findings text = %q, want a trailing tally line", text)
	}
}

func TestRoundtripFindingsForFailuresSkipsPassingSkippedAndErroredTargets(t *testing.T) {
	root := writeRoundtripCRDFixture(t)

	passingTarget, passingResult := newFailedTargetAndResult("ExampleResource/passing", &fakeCluster{
		forProvider: map[string]interface{}{"zone": "a", "neverEchoed": map[string]interface{}{}},
		atProvider:  map[string]interface{}{"zone": "a"},
	})
	passingResult.Result.Passed = true

	skippedTarget, skippedResult := newFailedTargetAndResult("ExampleResource/skipped", &fakeCluster{
		forProvider: map[string]interface{}{"zone": "a", "neverEchoed": map[string]interface{}{}},
		atProvider:  map[string]interface{}{"zone": "a"},
	})
	skippedResult.Result = &ConvergeResult{Skipped: true, SkipMsg: "structurally cannot converge"}

	erroredTarget, erroredResult := newFailedTargetAndResult("ExampleResource/errored", &fakeCluster{})
	erroredResult.Result = nil
	erroredResult.Err = errTestKubectlUnavailable

	findings := RoundtripFindingsForFailures(
		[]ConvergeTarget{passingTarget, skippedTarget, erroredTarget},
		[]ConvergeAllResult{passingResult, skippedResult, erroredResult},
		root,
	)

	if len(findings) != 0 {
		t.Errorf("findings = %+v, want empty — none of these three targets is a genuine failure", findings)
	}
}

func TestRoundtripFindingsForFailuresNoMatchingCRDIsSilentlySkipped(t *testing.T) {
	root := t.TempDir() // no package/crds at all
	f := &fakeCluster{forProvider: map[string]interface{}{"zone": "a"}, atProvider: map[string]interface{}{"zone": "a"}}
	target, result := newFailedTargetAndResult("ExampleResource/x", f)

	findings := RoundtripFindingsForFailures([]ConvergeTarget{target}, []ConvergeAllResult{result}, root)
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want empty when no CRD matches — advisory, never fatal", findings)
	}
}

// TestFormatConvergeAllSummaryWithFindingsIsNonGating is the ticket's own
// stated proof obligation: a findings entry attached to a PASSING
// resource's label must never affect ok, because the switch in
// formatConvergeAllSummary only ever consults findings from the FAILING
// branch. A findings map that DID influence a passing verdict would make
// this test fail.
func TestFormatConvergeAllSummaryWithFindingsIsNonGating(t *testing.T) {
	results := []ConvergeAllResult{
		{Label: "ExampleResource/passing", Result: &ConvergeResult{Passed: true, Message: "steady"}},
	}
	// A findings entry keyed to the PASSING resource's own label — if the
	// implementation ever looked this up outside the failing branch, it
	// would still be inert; this test exists so a future refactor that
	// breaks that invariant fails loudly instead of by inspection.
	findings := map[string]string{"ExampleResource/passing": "defaulted-by-server  bogus  spec=None mirror=1"}

	got, ok := FormatConvergeAllSummaryWithFindings(results, findings)
	if !ok {
		t.Fatalf("ok = false, want true — the only passing target must not be failed by an unrelated findings entry")
	}
	if strings.Contains(got, "bogus") {
		t.Errorf("summary = %q, want it to NOT include a findings entry attached to a passing target's label", got)
	}

	plain, plainOK := FormatConvergeAllSummary(results)
	if got != plain || ok != plainOK {
		t.Errorf("FormatConvergeAllSummaryWithFindings(with an inert findings map) = (%q, %v), want it byte-identical to FormatConvergeAllSummary: (%q, %v)", got, ok, plain, plainOK)
	}
}

// TestFormatConvergeAllSummaryWithFindingsInsertsAdjacentToFailingVerdict
// proves findings for a genuinely FAILING target are inlined immediately
// under that target's own verdict line, not appended as a separate
// trailing report.
func TestFormatConvergeAllSummaryWithFindingsInsertsAdjacentToFailingVerdict(t *testing.T) {
	results := []ConvergeAllResult{
		{Label: "ExampleResource/failing", Result: &ConvergeResult{Passed: false, Message: "atProvider drifted"}},
		{Label: "ExampleResource/passing", Result: &ConvergeResult{Passed: true, Message: "steady"}},
	}
	findings := map[string]string{
		"ExampleResource/failing": "defaulted-by-server  corsPolicy.disabled  spec=<absent> mirror=false\n-- 1 field(s) classified (defaulted-by-server=1)",
	}

	got, ok := FormatConvergeAllSummaryWithFindings(results, findings)
	if ok {
		t.Fatal("ok = true, want false — one target genuinely failed")
	}

	lines := strings.Split(got, "\n")
	verdictIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "ExampleResource/failing") {
			verdictIdx = i
			break
		}
	}
	if verdictIdx == -1 {
		t.Fatalf("summary does not mention the failing target at all:\n%s", got)
	}
	if verdictIdx+1 >= len(lines) || !strings.Contains(lines[verdictIdx+1], "corsPolicy.disabled") {
		t.Errorf("finding was not inlined immediately after the failing verdict line; got:\n%s", got)
	}
	// The passing target's own line must carry no findings, proving the
	// insertion is scoped to the specific failing Label and not applied
	// fleet-wide.
	for _, l := range lines {
		if strings.Contains(l, "ExampleResource/passing") && strings.Contains(l, "corsPolicy") {
			t.Errorf("finding leaked onto the passing target's line: %q", l)
		}
	}
}
