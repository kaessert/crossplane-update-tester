package runner

import (
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/roundtrip"
)

// RoundtripFindingsForFailures computes, for every FAILING target in
// results (a genuine converge failure — not skipped, not erroring before
// the live object could even be read), the advisory spec.forProvider <->
// status.atProvider round-trip report described in package roundtrip,
// rendered as ready-to-print lines and keyed by the target's own Label. See
// FormatConvergeAllSummaryWithFindings for how the caller inlines the
// result next to that target's verdict.
//
// root is the provider repository root — the directory holding
// package/crds/ — supplied by the caller (main.go's converge-all wiring
// reads it from an environment variable so the flag surface stays
// unchanged; see the UPDATE_TESTER_ROOT doc comment there). An empty root
// means the caller has no root to offer, and this function returns an
// empty map without touching the cluster at all — the same "nothing to add
// here" a target that cannot be diffed for any other reason also produces.
//
// Every skip in the loop below is deliberately silent, matching
// test/hooks/lib/roundtrip_diff.py's own run_mode: this whole report is
// advisory, so a target that cannot be diffed — no matching CRD, a
// since-deleted live object, a schema this package cannot navigate — is
// just absent from the returned map. The caller's plain fail-format path
// already covers that target; this function only ever ADDS detail, never
// removes or changes a verdict.
func RoundtripFindingsForFailures(targets []ConvergeTarget, results []ConvergeAllResult, root string) map[string]string {
	findings := make(map[string]string)
	if root == "" {
		return findings
	}

	for i, r := range results {
		if r.Err != nil || r.Result == nil || r.Result.Skipped || r.Result.Passed {
			continue
		}
		t := targets[i]
		if t.Runner == nil || t.Manifest == nil {
			continue
		}

		obj, err := t.Runner.GetObject()
		if err != nil {
			continue
		}
		crd, _ := roundtrip.FindCRD(root, t.Manifest.APIVersion, t.Manifest.Kind)
		if crd == nil {
			continue
		}
		rows, err := roundtrip.DiffReport(crd, obj)
		if err != nil {
			continue
		}
		lines := roundtrip.FormatFindingsLines(rows)
		if len(lines) == 0 {
			continue
		}

		findings[r.Label] = strings.Join(lines, "\n")
	}

	return findings
}
