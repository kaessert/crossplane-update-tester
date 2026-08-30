package runner

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestConvergeAllDeadlineAnchorsOnLastBaseline pins the correctness property
// the whole barrier rests on.
//
// Arming is not instantaneous and not uniform: each target waits for its own
// generation to settle and for its own Ready condition, so baselines land at
// different times. If the shared window were anchored on the FIRST baseline
// (or on the moment the barrier started), every target armed later than that
// would observe LESS than pollInterval*1.5 — silently weakening the check
// into one that can miss a reconcile cycle entirely, while still reporting a
// confident pass. Anchoring on the LAST baseline is what makes the shared
// window at least as strong as a private one for every participant.
func TestConvergeAllDeadlineAnchorsOnLastBaseline(t *testing.T) {
	base := time.Now()
	opts := ConvergeOptions{PollInterval: 10 * time.Second} // window = 15s

	baselines := []*convergeBaseline{
		{ArmedAt: base},                       // armed first
		{ArmedAt: base.Add(30 * time.Second)}, // armed 30s later
		{ArmedAt: base.Add(5 * time.Second)},
	}
	targets := []ConvergeTarget{{Opts: opts}, {Opts: opts}, {Opts: opts}}

	deadline, armed := convergeAllDeadline(targets, baselines)

	if armed != 3 {
		t.Fatalf("armed = %d, want 3", armed)
	}
	want := base.Add(30 * time.Second).Add(15 * time.Second)
	if !deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v (last baseline + one window)", deadline, want)
	}
	// The property restated as the guarantee each participant actually
	// needs, which is what a future edit must not break.
	for i, b := range baselines {
		if observed := deadline.Sub(b.ArmedAt); observed < convergeWait(opts) {
			t.Errorf("target %d observed %s, want >= %s", i, observed, convergeWait(opts))
		}
	}
}

// TestConvergeAllDeadlineUsesLongestWindow proves the anchor also respects
// the LONGEST per-target window, not the first one it encounters. Targets
// may declare different poll intervals (a provider raising
// UPDATE_TESTER_POLL_INTERVAL for one slow resource), and a window sized off
// the wrong target short-changes the slow one.
func TestConvergeAllDeadlineUsesLongestWindow(t *testing.T) {
	base := time.Now()
	targets := []ConvergeTarget{
		{Opts: ConvergeOptions{PollInterval: 10 * time.Second}}, // 15s
		{Opts: ConvergeOptions{PollInterval: 60 * time.Second}}, // 90s
	}
	baselines := []*convergeBaseline{{ArmedAt: base}, {ArmedAt: base}}

	deadline, _ := convergeAllDeadline(targets, baselines)

	if want := base.Add(90 * time.Second); !deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v (longest window wins)", deadline, want)
	}
}

// TestConvergeAllSkippedTargetDoesNotOpenAWindow proves a fleet consisting
// only of converge-skip resources costs no wait at all — the barrier must
// not sleep for resources that are not being observed.
func TestConvergeAllSkippedTargetDoesNotOpenAWindow(t *testing.T) {
	f := &fakeCluster{generation: 1, readyAfterCalls: 1, atProvider: map[string]interface{}{"zone": "a"}}
	r := newFakeRunner(f)
	r.sleepFunc = func(time.Duration) {}

	targets := []ConvergeTarget{{
		Label:    "Skipped/one",
		Runner:   r,
		Manifest: &manifest.Manifest{Kind: testKindExample, Name: testNameExample, ConvergeSkip: "structurally cannot converge"},
		Opts:     ConvergeOptions{PollInterval: 30 * time.Second},
	}}

	start := time.Now()
	results := RunConvergeAll(targets, 4)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("barrier slept %s for an all-skipped fleet; want no window at all", elapsed)
	}
	if len(results) != 1 || results[0].Result == nil || !results[0].Result.Skipped {
		t.Fatalf("expected a single skipped result, got %+v", results)
	}
}

// TestConvergeAllSharesOneWindow is the claim the change exists to make: N
// resources cost ONE window, not N. It also proves each target still gets a
// real verdict rather than a shortcut.
func TestConvergeAllSharesOneWindow(t *testing.T) {
	const (
		n      = 6
		poll   = 120 * time.Millisecond // window = 180ms
		window = time.Duration(float64(poll) * 1.5)
	)

	targets := make([]ConvergeTarget, 0, n)
	for range n {
		f := &fakeCluster{
			generation:      1,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"zone": "a"},
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		targets = append(targets, ConvergeTarget{
			Label:    "ExampleResource/x",
			Runner:   r,
			Manifest: &manifest.Manifest{Kind: testKindExample, Name: testNameExample},
			Opts:     ConvergeOptions{PollInterval: poll, Timeout: time.Second, ReadinessTimeout: time.Second},
		})
	}

	start := time.Now()
	results := RunConvergeAll(targets, 4)
	elapsed := time.Since(start)

	// The serial form would cost n*window. Allow generous headroom for the
	// arm/assert kubectl round trips; the point is the ORDER of magnitude,
	// not a tight bound that would make this test flaky under load.
	if serial := n * window; elapsed >= serial {
		t.Errorf("barrier took %s, which is not better than the serial cost %s", elapsed, serial)
	}
	if elapsed < window {
		t.Errorf("barrier took %s, less than one full window %s — the window was not actually observed", elapsed, window)
	}

	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("target %d: unexpected error %v", i, res.Err)
		}
		if !res.Result.Passed {
			t.Errorf("target %d: expected Passed, got %q %v", i, res.Result.Message, res.Result.Diagnostics)
		}
	}
}

// TestConvergeAllReportsPerTargetVerdicts proves one drifting resource is
// reported as itself and does not contaminate the verdict of the others
// sharing its window — the barrier aggregates results, it does not merge
// them.
func TestConvergeAllReportsPerTargetVerdicts(t *testing.T) {
	newTarget := func(label string, drifting bool) ConvergeTarget {
		f := &fakeCluster{
			generation:      1,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"zone": "a"},
		}
		if drifting {
			// An update-event count for THIS resource that grows between
			// the baseline and outcome reads: the controller is issuing
			// Update() calls, which is the genuine loop signal.
			f.siblingKind = testKindExample
			f.siblingName = testNameExample
			f.siblingEventBase = 1
			f.siblingEventGrowthPerCall = 5
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		return ConvergeTarget{
			Label:    label,
			Runner:   r,
			Manifest: &manifest.Manifest{Kind: testKindExample, Name: testNameExample},
			Opts:     ConvergeOptions{PollInterval: time.Millisecond, Timeout: time.Second, ReadinessTimeout: time.Second},
		}
	}

	results := RunConvergeAll([]ConvergeTarget{
		newTarget("stable/one", false),
		newTarget("drifting/two", true),
		newTarget("stable/three", false),
	}, 4)

	if results[0].Result == nil || !results[0].Result.Passed {
		t.Errorf("stable/one should pass, got %+v", results[0].Result)
	}
	if results[1].Result == nil || results[1].Result.Passed {
		t.Errorf("drifting/two should fail, got %+v", results[1].Result)
	}
	if results[2].Result == nil || !results[2].Result.Passed {
		t.Errorf("stable/three should pass, got %+v", results[2].Result)
	}

	summary, ok := FormatConvergeAllSummary(results)
	if ok {
		t.Error("summary reported ok despite a failing target")
	}
	if !strings.Contains(summary, "drifting/two") {
		t.Errorf("summary does not name the failing target:\n%s", summary)
	}
	if !strings.Contains(summary, "2 passed, 1 failed") {
		t.Errorf("summary tally wrong:\n%s", summary)
	}
}

// TestConvergeAllSecondTargetUnderSharedWindow is the multi-manifest
// regression the false RECONCILIATION LOOP was actually measured on: two
// manifests under one shared barrier window (standing for the
// UPTEST_MANIFESTS_<RESOURCE> comma pair one `make e2e.<resource>` run
// bundles — the cluster-scoped and namespaced variants of a resource, each
// resolved through its OWN Runner and OWN controller-log source) where the
// FIRST manifest's post-assert-hook rename lands with comfortable
// separation from its own arm, but the SECOND manifest's rename can land
// inside kubectl's whole-second --since rounding of ITS OWN, later
// ArmedAt. Both targets share one barrier; only the second is exposed.
//
// preRun is captured before RunConvergeAll runs at all, so EVERY target's
// real ArmedAt (captured inside convergeArm, during the call) is
// guaranteed >= preRun regardless of which target the barrier's internal
// concurrency happens to arm first — which is what lets
// preRun.Add(-time.Second) stand in for "before this target's own window"
// deterministically, with no dependency on goroutine scheduling order.
func TestConvergeAllSecondTargetUnderSharedWindow(t *testing.T) {
	newTarget := func(label, logLines string) ConvergeTarget {
		f := &fakeCluster{
			generation:      1,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"path": "/DC0/vm"},
			kind:            testKindExample,
			name:            testNameExample,
			generations:     []int32{0},
			logLines:        logLines,
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		return ConvergeTarget{
			Label:    label,
			Runner:   r,
			Manifest: &manifest.Manifest{Kind: testKindExample, Name: testNameExample},
			Opts:     ConvergeOptions{PollInterval: time.Millisecond, Timeout: time.Second, ReadinessTimeout: time.Second},
		}
	}

	t.Run("PreArmRenameOnSecondTargetIsIgnored", func(t *testing.T) {
		preRun := time.Now()
		targets := []ConvergeTarget{
			// The cluster-scoped sibling: quiet, no rename in its own log
			// window at all — standing for the manifest whose hook runs
			// first and gains the extra second of separation.
			newTarget("Folder/example-folder",
				strings.Join([]string{testReconcileLogLine}, "\n")),
			// The namespaced sibling: its own post-assert-hook rename,
			// timestamped before RunConvergeAll even started — so it is
			// unambiguously before THIS target's own ArmedAt too, whatever
			// real instant that turns out to be. Each target reads through
			// its own separate fakeCluster/Runner, so this line can never
			// leak into the first target's own log query either way.
			newTarget("Folder/example-folder-ns",
				strings.Join([]string{
					testReconcileLogLine,
					newTestUpdateLogLineAt(preRun.Add(-time.Second), testNameExample, ""),
				}, "\n")),
		}

		results := RunConvergeAll(targets, 4)

		for i, res := range results {
			if res.Err != nil {
				t.Fatalf("target %d (%s): unexpected error %v", i, targets[i].Label, res.Err)
			}
			if !res.Result.Passed {
				t.Errorf("target %d (%s): expected Passed=true, got %q %v — a pre-arm rename must not be attributed to either target's own shared-barrier window",
					i, targets[i].Label, res.Result.Message, res.Result.Diagnostics)
			}
		}
	})

	t.Run("InWindowUpdateOnSecondTargetStillDetected", func(t *testing.T) {
		// A comfortable hour past "now": genuinely inside every target's
		// real (millisecond-scale) observation window under this fast
		// test, and unambiguously after every target's own ArmedAt.
		inWindow := time.Now().Add(time.Hour)
		targets := []ConvergeTarget{
			newTarget("Folder/example-folder",
				strings.Join([]string{testReconcileLogLine}, "\n")),
			newTarget("Folder/example-folder-ns",
				strings.Join([]string{
					testReconcileLogLine,
					newTestUpdateLogLineAt(inWindow, testNameExample, ""),
				}, "\n")),
		}

		results := RunConvergeAll(targets, 4)

		if results[0].Err != nil || !results[0].Result.Passed {
			t.Errorf("Folder/example-folder: expected Passed=true (quiet), got err=%v result=%+v", results[0].Err, results[0].Result)
		}
		if results[1].Err != nil {
			t.Fatalf("Folder/example-folder-ns: unexpected error %v", results[1].Err)
		}
		if results[1].Result.Passed {
			t.Fatalf("Folder/example-folder-ns: expected Passed=false — a genuine in-window Update() call must still be detected under a shared barrier, got %+v", results[1].Result)
		}
		if results[1].Result.Message != "RECONCILIATION LOOP DETECTED" {
			t.Errorf("Folder/example-folder-ns: Message = %q, want RECONCILIATION LOOP DETECTED", results[1].Result.Message)
		}
	})
}

// TestFormatConvergeAllSummaryPassingTargetWithDiagnosticsPrintsThem pins
// the fix this ticket exists to make: a PASSING target whose Diagnostics
// are non-empty (the restart note buildConvergeResult attaches on a
// re-armed retry, or the pre-existing readiness-timeout / controller-log
// notes) must have them printed, exactly as printConvergeResult already
// does for the single-target path — not just the verdict line.
func TestFormatConvergeAllSummaryPassingTargetWithDiagnosticsPrintsThem(t *testing.T) {
	results := []ConvergeAllResult{
		{Label: "HttpLoadbalancer/example-http-loadbalancer", Result: &ConvergeResult{
			Passed:  true,
			Message: "resource stable (1 cycle observed, 0 updates)",
			Diagnostics: []string{
				"provider controller pod restarted during observation (attempt 1: baseline pod \"provider-f5xc-abc\", now \"provider-f5xc-def\") — window discarded and re-measured",
			},
		}},
	}

	got, ok := formatConvergeAllSummary(results, nil)

	if !ok {
		t.Fatalf("ok = false, want true — the only target passed")
	}
	want := fmt.Sprintf("  ✓ %-40s %s\n", "HttpLoadbalancer/example-http-loadbalancer", "resource stable (1 cycle observed, 0 updates)") +
		"      provider controller pod restarted during observation (attempt 1: baseline pod \"provider-f5xc-abc\", now \"provider-f5xc-def\") — window discarded and re-measured\n" +
		"\n1 passed, 0 failed, 0 skipped, 0 errored\n"
	if got != want {
		t.Errorf("formatConvergeAllSummary() =\n%q\nwant:\n%q", got, want)
	}
}

// TestFormatConvergeAllSummaryPassingTargetWithoutDiagnosticsUnchanged pins
// the AC that the fix must not alter the verdict or the byte shape of a
// passing target that carries no Diagnostics — the common case, and the
// one every existing caller already depends on.
func TestFormatConvergeAllSummaryPassingTargetWithoutDiagnosticsUnchanged(t *testing.T) {
	results := []ConvergeAllResult{
		{Label: "ExampleResource/x", Result: &ConvergeResult{Passed: true, Message: "resource stable (1 cycle observed, 0 updates)"}},
	}

	got, ok := formatConvergeAllSummary(results, nil)

	if !ok {
		t.Fatalf("ok = false, want true")
	}
	want := fmt.Sprintf("  ✓ %-40s %s\n", "ExampleResource/x", "resource stable (1 cycle observed, 0 updates)") +
		"\n1 passed, 0 failed, 0 skipped, 0 errored\n"
	if got != want {
		t.Errorf("formatConvergeAllSummary() =\n%q\nwant:\n%q", got, want)
	}
}

// TestFormatConvergeAllSummaryNotePresenceNeverFlipsOk proves a note on a
// passing target's Diagnostics can never turn ok false, and — separately —
// that a note on a FAILING target's Diagnostics (already the pre-existing
// behaviour) does not change the count-derived ok either way. This is the
// "verdict is unchanged" acceptance criterion: pass/fail/skip/error counts
// and ok are derived solely from Result.Passed / Skipped / Err, never from
// whether Diagnostics happens to be empty.
func TestFormatConvergeAllSummaryNotePresenceNeverFlipsOk(t *testing.T) {
	withNote := []ConvergeAllResult{
		{Label: "a", Result: &ConvergeResult{Passed: true, Message: "stable", Diagnostics: []string{"a note"}}},
	}
	withoutNote := []ConvergeAllResult{
		{Label: "a", Result: &ConvergeResult{Passed: true, Message: "stable"}},
	}

	_, okWith := formatConvergeAllSummary(withNote, nil)
	_, okWithout := formatConvergeAllSummary(withoutNote, nil)

	if !okWith || !okWithout || okWith != okWithout {
		t.Errorf("okWith=%v okWithout=%v, want both true and equal — a note must never flip ok", okWith, okWithout)
	}
}

// TestFormatConvergeAllSummaryFailingBranchByteIdentical pins the failing
// branch's existing output shape — diagnostics then roundtrip findings, in
// that order — proving this ticket's passing-branch addition left it
// untouched.
func TestFormatConvergeAllSummaryFailingBranchByteIdentical(t *testing.T) {
	results := []ConvergeAllResult{
		{Label: "ExampleResource/failing", Result: &ConvergeResult{
			Passed:      false,
			Message:     "RECONCILIATION LOOP DETECTED",
			Diagnostics: []string{"atProvider changed: zone: \"a\" -> \"b\""},
		}},
	}
	findings := map[string]string{
		"ExampleResource/failing": "defaulted-by-server  corsPolicy.disabled  spec=<absent> mirror=false",
	}

	got, ok := formatConvergeAllSummary(results, findings)

	if ok {
		t.Fatal("ok = true, want false — one target failed")
	}
	want := fmt.Sprintf("  ✗ %-40s %s\n", "ExampleResource/failing", "RECONCILIATION LOOP DETECTED") +
		"      atProvider changed: zone: \"a\" -> \"b\"\n" +
		"      defaulted-by-server  corsPolicy.disabled  spec=<absent> mirror=false\n" +
		"\n0 passed, 1 failed, 0 skipped, 0 errored\n"
	if got != want {
		t.Errorf("formatConvergeAllSummary() =\n%q\nwant:\n%q", got, want)
	}
}

// TestConvergeAllAppliesPerTargetIgnoreFieldsIndependently proves the
// per-target IgnoreFields plumbing shares one barrier without letting one
// target's exclusion set leak onto another's — the vultr shape, where three
// resources need three DIFFERENT exclusion sets in the same run.
//
// All three targets drift in "kvm" between the baseline and outcome
// snapshots. Only the target that names "kvm" in its OWN IgnoreFields may
// pass; a sibling target sharing the same window but excluding a DIFFERENT
// field must still report the kvm drift as a failure. A single fleet-wide
// exclusion set (the pre-fix behaviour) could not produce this split: it
// would apply the same set to every target and either hide the drift
// everywhere or catch it everywhere.
func TestConvergeAllAppliesPerTargetIgnoreFieldsIndependently(t *testing.T) {
	newDriftingTarget := func(label string, ignoreFields []string) ConvergeTarget {
		f := &fakeCluster{
			generation:      1,
			readyAfterCalls: 1,
			atProvider:      map[string]interface{}{"kvm": "off", "zone": "a"},
			driftField:      "kvm",
			driftValue:      "on",
			// convergeArm reads the resource 4 times before the baseline
			// snapshot is captured (waitGenerationSettled, waitSynced,
			// waitReady, then the baseline Snapshot() itself) — call 5 is
			// convergeAssert's post-window Snapshot(), the first read that
			// must see the drift. Firing any earlier would bake the drift
			// into the baseline itself and the diff would see no change at
			// all, which is exactly the failure mode this threshold exists
			// to avoid.
			driftAfterGetCalls: 5,
		}
		r := newFakeRunner(f)
		r.sleepFunc = func(time.Duration) {}
		return ConvergeTarget{
			Label:    label,
			Runner:   r,
			Manifest: &manifest.Manifest{Kind: testKindExample, Name: testNameExample},
			Opts: ConvergeOptions{
				PollInterval:     time.Millisecond,
				Timeout:          time.Second,
				ReadinessTimeout: time.Second,
				IgnoreFields:     ignoreFields,
			},
		}
	}

	results := RunConvergeAll([]ConvergeTarget{
		newDriftingTarget("database/one", []string{"latestBackup"}),                         // does NOT ignore kvm
		newDriftingTarget("firewall-rule/two", []string{"ruleCount", "dateModified"}),       // does NOT ignore kvm
		newDriftingTarget("instance/three", []string{"kvm", "powerStatus", "serverStatus"}), // DOES ignore kvm
	}, 4)

	if results[0].Result == nil || results[0].Result.Passed {
		t.Errorf("database/one excludes latestBackup, not kvm — the kvm drift must still fail it, got %+v", results[0].Result)
	}
	if results[1].Result == nil || results[1].Result.Passed {
		t.Errorf("firewall-rule/two excludes ruleCount/dateModified, not kvm — the kvm drift must still fail it, got %+v", results[1].Result)
	}
	if results[2].Result == nil || !results[2].Result.Passed {
		t.Errorf("instance/three excludes kvm itself — the same drift must not fail it, got %+v", results[2].Result)
	}
}
