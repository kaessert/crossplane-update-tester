package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/runner"
	"github.com/kaessert/crossplane-update-tester/internal/validator"
)

// counts mirrors the six return values of printResults, so a table can
// declare an expected outcome in one literal instead of six parallel fields.
type counts struct {
	passed, failed, noop, notEvidenced, untrusted, knownDefect int
}

func run(results []runner.TestResult) (string, counts) {
	var buf bytes.Buffer
	p, f, n, ne, u, kd := printResults(&buf, results)
	return buf.String(), counts{p, f, n, ne, u, kd}
}

func TestPrintResultsCounters(t *testing.T) {
	tests := []struct {
		name    string
		results []runner.TestResult
		want    counts
		// contains lists substrings the printed report must carry: the
		// marker and the verdict word for each classified result.
		contains []string
	}{
		{
			// The pre-patch value (Before) differs from the post-patch target
			// (Expected/Actual): the printed line must show that real
			// transition, not Expected → Actual, which would print the same
			// value "a" on both sides of the arrow and read as a no-op that
			// never happened.
			name: "PassIsCountedOnce",
			results: []runner.TestResult{{
				Field: "comment", Passed: true, Before: "b", BeforeKnown: true,
				Expected: "a", Actual: "a", Duration: 2 * time.Second,
			}},
			want:     counts{passed: 1},
			contains: []string{`  ✓ comment: "b" → "a" (2s)`, "Differential: all non-target fields stable ✓"},
		},
		{
			name: "SlowPassIsAnnotatedButStillPasses",
			results: []runner.TestResult{{
				Field: "comment", Passed: true, SlowObserve: true, Before: "b", BeforeKnown: true,
				Expected: "a", Actual: "a", Duration: 45 * time.Second,
			}},
			want:     counts{passed: 1},
			contains: []string{`  ✓ comment: "b" → "a" (45s, slow-observe)`},
		},
		{
			// The pre-patch read failed (BeforeKnown false), e.g. the field
			// was absent from both spec.forProvider and status.atProvider
			// before the first write. The line must fall back to explicit
			// expected/observed labels rather than printing a bare arrow with
			// an unknown left side.
			name: "PassWithoutKnownBeforeFallsBackToLabels",
			results: []runner.TestResult{{
				Field: "comment", Passed: true, Expected: "a", Actual: "a", Duration: 2 * time.Second,
			}},
			want:     counts{passed: 1},
			contains: []string{`  ✓ comment: expected "a", observed "a" (2s)`},
		},
		{
			name:     "SkipIsNeitherPassedNorFailed",
			results:  []runner.TestResult{{Field: "comment", Skipped: true, SkipMsg: "needs companion data"}},
			want:     counts{},
			contains: []string{"  ⊘ comment: SKIPPED (needs companion data)"},
		},
		{
			name:     "NoOpIsAFailure",
			results:  []runner.TestResult{{Field: "comment", NoOp: true, Error: errors.New("value already equals target"), Duration: time.Second}},
			want:     counts{failed: 1, noop: 1},
			contains: []string{"  ⦸ comment: NO-OP"},
		},
		{
			name:     "NotEvidencedIsAFailure",
			results:  []runner.TestResult{{Field: "comment", NotEvidenced: true, Error: errors.New("no update event"), Duration: 3 * time.Second}},
			want:     counts{failed: 1, notEvidenced: 1},
			contains: []string{"  ⚡ comment: NOT-EVIDENCED"},
		},
		{
			name:     "ErrorIsAFailure",
			results:  []runner.TestResult{{Field: "comment", Error: errors.New("patch rejected"), Duration: time.Second}},
			want:     counts{failed: 1},
			contains: []string{"  ✗ comment: ERROR (patch rejected) (1s)"},
		},
		{
			name:     "MismatchIsAFailure",
			results:  []runner.TestResult{{Field: "comment", Expected: "a", Actual: "b", Duration: time.Second}},
			want:     counts{failed: 1},
			contains: []string{`  ✗ comment: expected "a", got "b" (1s)`},
		},
		{
			name: "SideEffectsAreReportedAndSuppressTheStableLine",
			results: []runner.TestResult{{
				Field: "comment", Passed: true, Before: "old", BeforeKnown: true, Expected: "a", Actual: "a", Duration: time.Second,
				SideFx: []differ.FieldChange{{Field: "ttl", OldValue: "30", NewValue: "60"}},
			}},
			want:     counts{passed: 1},
			contains: []string{"    ⚠ side effects: ttl: 30 → 60"},
		},
		{
			name: "MixedRunCountsEveryModeSeparately",
			results: []runner.TestResult{
				{Field: "a", Passed: true, Before: "0", BeforeKnown: true, Expected: "1", Actual: "1"},
				{Field: "b", Skipped: true, SkipMsg: "immutable"},
				{Field: "c", NoOp: true, Error: errors.New("no-op")},
				{Field: "d", NotEvidenced: true, Error: errors.New("no event")},
				{Field: "e", EvidenceUntrusted: true, Passed: true},
				{Field: "f", Expected: "1", Actual: "2"},
			},
			want: counts{passed: 1, failed: 4, noop: 1, notEvidenced: 1, untrusted: 1},
		},
		{
			// Non-convergence is the entry's EXPECTED outcome: it must be
			// visible in the report, but must NOT count as a failure or a
			// pass — see runner.TestResult.KnownDefect.
			name: "KnownDefectNonConvergenceIsItsOwnClassNotAFailure",
			results: []runner.TestResult{{
				Field: "useTls", KnownDefect: "e9ce03ee-920d-46f5-9aa3-120228b196fb",
				NotEvidenced: true, Error: errors.New("no update event"), Duration: 3 * time.Second,
			}},
			want:     counts{knownDefect: 1},
			contains: []string{"  ⚑ useTls: KNOWN-DEFECT (e9ce03ee-920d-46f5-9aa3-120228b196fb) — non-convergence expected and confirmed"},
		},
		{
			// The suppressed defect actually converged: this MUST fail the
			// run hard and name the ticket ID, so a fixed defect cannot
			// silently keep running under a stale knownDefect token.
			name: "KnownDefectConvergedIsAHardFailure",
			results: []runner.TestResult{{
				Field: "useTls", KnownDefect: "e9ce03ee-920d-46f5-9aa3-120228b196fb", KnownDefectConverged: true,
				Passed: true, Before: "false", BeforeKnown: true, Expected: "true", Actual: "true", Duration: time.Second,
			}},
			want: counts{failed: 1},
			contains: []string{
				"  ✗ useTls: KNOWN-DEFECT CONVERGED (e9ce03ee-920d-46f5-9aa3-120228b196fb)",
				"delete the knownDefect token",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, got := run(tc.results)
			if got != tc.want {
				t.Errorf("counts = %+v, want %+v\noutput:\n%s", got, tc.want, out)
			}
			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\noutput:\n%s", want, out)
				}
			}
		})
	}
}

// TestUntrustedResultFailsTheRun pins the exit-code contract for a result
// whose evidence check ran after a mid-run event-burst reset failed. Neither
// PASS nor NOT-EVIDENCED proves anything in that state, so the result must
// count as a failure — which is what makes cmdRun exit non-zero — and must
// NOT be folded into the not-evidenced counter, so the summary line can
// never read a clean "0 not-evidenced" while masking it.
func TestUntrustedResultFailsTheRun(t *testing.T) {
	tests := []struct {
		name   string
		result runner.TestResult
	}{
		{name: "UntrustedWhilePassing", result: runner.TestResult{Field: "comment", EvidenceUntrusted: true, Passed: true, Before: "b", BeforeKnown: true, Expected: "a", Actual: "a"}},
		{name: "UntrustedWhileNotEvidenced", result: runner.TestResult{Field: "comment", EvidenceUntrusted: true, NotEvidenced: true}},
		{name: "UntrustedAlone", result: runner.TestResult{Field: "comment", EvidenceUntrusted: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, got := run([]runner.TestResult{tc.result})

			if got.failed == 0 {
				t.Errorf("untrusted result did not fail the run: %+v", got)
			}
			if got.untrusted != 1 {
				t.Errorf("untrusted = %d, want 1", got.untrusted)
			}
			if got.passed != 0 {
				t.Errorf("passed = %d, want 0 — an untrusted result must never count as a pass", got.passed)
			}
			if got.notEvidenced != 0 {
				t.Errorf("notEvidenced = %d, want 0 — untrusted is its own failure mode", got.notEvidenced)
			}
			if !strings.Contains(out, "‽ comment: UNTRUSTED") {
				t.Errorf("output missing the untrusted marker\noutput:\n%s", out)
			}
		})
	}
}

// TestPrintUnchangedAssertions covers printUnchangedAssertions directly —
// the gating verdict cmdRun relies on for the assert-unchanged directive
// (see manifest.Manifest.AssertUnchanged): no fields declared prints
// nothing and never fails, every declared field holding its baseline prints
// a PASS line for each and never fails, and a violated field prints a
// WIPED line naming the triggering field test and fails the run. This is
// also the recorded proof that a deliberately-broken assertion produces a
// FAILED run: DriftedFieldFails's output line is the artifact.
func TestPrintUnchangedAssertions(t *testing.T) {
	tests := []struct {
		name       string
		fields     []string
		violations []runner.UnchangedAssertion
		wantFailed bool
		contains   []string
		wantEmpty  bool
	}{
		{
			name:      "NoFieldsDeclaredPrintsNothing",
			fields:    nil,
			wantEmpty: true,
		},
		{
			name:       "EveryFieldHoldsIsVisibleAndDoesNotFail",
			fields:     []string{"legacyRuleList"},
			violations: nil,
			wantFailed: false,
			contains:   []string{"Unchanged-field assertions:", `  ✓ legacyRuleList: unchanged across run`},
		},
		{
			name:   "DriftedFieldFails",
			fields: []string{"legacyRuleList"},
			violations: []runner.UnchangedAssertion{{
				Field: "legacyRuleList", Baseline: `["rule-a"]`, Observed: "[]", AfterField: "comment",
			}},
			wantFailed: true,
			contains:   []string{`  ✗ legacyRuleList: WIPED after patching "comment" (was "[\"rule-a\"]", now "[]")`},
		},
		{
			name:   "OnlyTheDriftedFieldAmongSeveralIsMarkedFailed",
			fields: []string{"stableField", "legacyRuleList"},
			violations: []runner.UnchangedAssertion{{
				Field: "legacyRuleList", Baseline: `["rule-a"]`, Observed: "[]", AfterField: "comment",
			}},
			wantFailed: true,
			contains: []string{
				`  ✓ stableField: unchanged across run`,
				`  ✗ legacyRuleList: WIPED after patching "comment" (was "[\"rule-a\"]", now "[]")`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gotFailed := printUnchangedAssertions(&buf, tc.fields, tc.violations)
			out := buf.String()

			if tc.wantEmpty && out != "" {
				t.Errorf("expected no output for a manifest with no assert-unchanged fields, got:\n%s", out)
			}
			if gotFailed != tc.wantFailed {
				t.Errorf("printUnchangedAssertions() = %v, want %v", gotFailed, tc.wantFailed)
			}
			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\noutput:\n%s", want, out)
				}
			}
		})
	}
}

// TestPassTransition pins passTransition's two shapes directly: a known
// pre-patch value produces a plain arrow, and an unknown one falls back to
// explicit labels rather than printing an arrow with a blank or misleading
// left side.
func TestPassTransition(t *testing.T) {
	tests := []struct {
		name   string
		result runner.TestResult
		want   string
	}{
		{
			name:   "KnownBeforeProducesAnArrow",
			result: runner.TestResult{Before: "transaction", BeforeKnown: true, Expected: "session", Actual: "session"},
			want:   `"transaction" → "session"`,
		},
		{
			name: "UnknownBeforeFallsBackToLabels",
			// BeforeKnown left false — the pre-patch read failed, so Before
			// must not be trusted even if it happens to be non-empty.
			result: runner.TestResult{Before: "stale-leftover", Expected: "session", Actual: "session"},
			want:   `expected "session", observed "session"`,
		},
		{
			name:   "KnownEmptyBeforeIsDistinctFromUnknown",
			result: runner.TestResult{Before: "", BeforeKnown: true, Expected: "x", Actual: "x"},
			want:   `"" → "x"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := passTransition(tc.result); got != tc.want {
				t.Errorf("passTransition(%+v) = %q, want %q", tc.result, got, tc.want)
			}
		})
	}
}

// TestPassLineIsNotReadableAsANoOp is the regression test for the defect
// this display fix addresses: a genuine transaction→session update used to
// print as `mode: "session" → "session"` — Expected paired with Actual, both
// the post-update target — which reads exactly like the no-op the update
// test exists to catch, even though the transition genuinely happened. The
// PASS line must show the value that actually changed on the left.
func TestPassLineIsNotReadableAsANoOp(t *testing.T) {
	result := runner.TestResult{
		Field: "mode", Passed: true, SlowObserve: true,
		Before: "transaction", BeforeKnown: true,
		Expected: "session", Actual: "session",
		Duration: 12 * time.Second,
	}

	out, got := run([]runner.TestResult{result})

	if got.passed != 1 {
		t.Fatalf("counts = %+v, want 1 passed", got)
	}
	const wantLine = `  ✓ mode: "transaction" → "session" (12s, slow-observe)`
	if !strings.Contains(out, wantLine) {
		t.Errorf("output missing %q\noutput:\n%s", wantLine, out)
	}
	const noOpShape = `"session" → "session"`
	if strings.Contains(out, noOpShape) {
		t.Errorf("output contains %q — a real transition must never print the same value on both sides of the arrow\noutput:\n%s", noOpShape, out)
	}
}

// TestHookStepsGateOnPrefixAnnotationAndSkipConverge covers all four
// combinations of the two independent switches hookSteps reads —
// ExpectExternalNamePrefix set/unset and skipConverge on/off — and asserts
// the full ordered command list each time, not just a length or a presence
// check. The absent-flag arm (skipConverge=false) must equal, byte for byte,
// what hookSteps produced before the flag existed: that is what keeps the
// default off for the six providers that never pass it.
func TestHookStepsGateOnPrefixAnnotationAndSkipConverge(t *testing.T) {
	tests := []struct {
		name         string
		manifest     *manifest.Manifest
		skipConverge bool
		want         []hookStep
	}{
		{
			name:         "NoPrefixConvergeOn",
			manifest:     &manifest.Manifest{Kind: "ExampleResource", Name: "example"},
			skipConverge: false,
			want: []hookStep{
				{banner: "converge", command: "converge"},
				{banner: "run", command: "run"},
				{banner: "post-update converge", command: "converge"},
			},
		},
		{
			name:         "PrefixSetConvergeOn",
			manifest:     &manifest.Manifest{Kind: "ExampleResource", Name: "example", ExpectExternalNamePrefix: "example/"},
			skipConverge: false,
			want: []hookStep{
				{banner: "converge", command: "converge"},
				{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
				{banner: "resolve-recover", command: "resolve-recover"},
				{banner: "run", command: "run"},
				{banner: "post-update converge", command: "converge"},
			},
		},
		{
			name:         "NoPrefixConvergeSkipped",
			manifest:     &manifest.Manifest{Kind: "ExampleResource", Name: "example"},
			skipConverge: true,
			want: []hookStep{
				{banner: "run", command: "run"},
			},
		},
		{
			name:         "PrefixSetConvergeSkipped",
			manifest:     &manifest.Manifest{Kind: "ExampleResource", Name: "example", ExpectExternalNamePrefix: "example/"},
			skipConverge: true,
			want: []hookStep{
				{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
				{banner: "resolve-recover", command: "resolve-recover"},
				{banner: "run", command: "run"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hookSteps(tc.manifest, tc.skipConverge)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hookSteps(skipConverge=%v) = %+v, want %+v", tc.skipConverge, got, tc.want)
			}
		})
	}
}

// TestHookStepsDefaultIsByteIdenticalToPreFlagBehaviour is the explicit
// regression the default-off acceptance criterion asks for: with the flag
// absent (skipConverge's zero value, false), hookSteps must reproduce
// exactly what it produced before --skip-converge existed, for both the
// prefix-set and prefix-unset manifest shapes.
func TestHookStepsDefaultIsByteIdenticalToPreFlagBehaviour(t *testing.T) {
	tests := []struct {
		name     string
		manifest *manifest.Manifest
		want     []hookStep
	}{
		{
			name:     "WithoutPrefixAnnotation",
			manifest: &manifest.Manifest{Kind: "ExampleResource", Name: "example"},
			want: []hookStep{
				{banner: "converge", command: "converge"},
				{banner: "run", command: "run"},
				{banner: "post-update converge", command: "converge"},
			},
		},
		{
			name:     "WithPrefixAnnotation",
			manifest: &manifest.Manifest{Kind: "ExampleResource", Name: "example", ExpectExternalNamePrefix: "example/"},
			want: []hookStep{
				{banner: "converge", command: "converge"},
				{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
				{banner: "resolve-recover", command: "resolve-recover"},
				{banner: "run", command: "run"},
				{banner: "post-update converge", command: "converge"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// var hookOptions{} zero value: skipConverge defaults to false.
			var opts hookOptions
			got := hookSteps(tc.manifest, opts.skipConverge)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hookSteps() with flag absent = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestHookStepsAreAllDispatchable guards the seam between hookSteps and the
// dispatcher: a step naming a command runCommand does not know would fail
// only in a live E2E run, after a cluster had already been stood up.
func TestHookStepsAreAllDispatchable(t *testing.T) {
	known := map[string]bool{
		"run": true, "validate": true, "converge": true,
		"check-external-name-prefix": true, "resolve-recover": true,
		"hook": true, "version": true,
	}
	for _, skipConverge := range []bool{false, true} {
		for _, s := range hookSteps(&manifest.Manifest{ExpectExternalNamePrefix: "example/"}, skipConverge) {
			if !known[s.command] {
				t.Errorf("step %q dispatches unknown command %q (skipConverge=%v)", s.banner, s.command, skipConverge)
			}
		}
	}
}

func TestParseHookEnv(t *testing.T) {
	tests := []struct {
		name         string
		timeout      string
		pollInterval string
		ignoreFields string
		want         hookEnv
		wantErr      bool
	}{
		{name: "UnsetLeavesSubcommandDefaults", want: hookEnv{}},
		{name: "TimeoutSeconds", timeout: "600", want: hookEnv{timeout: 600}},
		{name: "PollIntervalDuration", pollInterval: "90s", want: hookEnv{pollInterval: 90 * time.Second}},
		{name: "Both", timeout: "600", pollInterval: "2m", want: hookEnv{timeout: 600, pollInterval: 2 * time.Minute}},
		{name: "IgnoreFieldsSingle", ignoreFields: "latestBackup", want: hookEnv{ignoreFields: []string{"latestBackup"}}},
		{name: "IgnoreFieldsMultiple", ignoreFields: "a,b", want: hookEnv{ignoreFields: []string{"a", "b"}}},
		{
			name: "AllThree", timeout: "600", pollInterval: "2m", ignoreFields: "latestBackup",
			want: hookEnv{timeout: 600, pollInterval: 2 * time.Minute, ignoreFields: []string{"latestBackup"}},
		},
		{name: "TimeoutNotANumber", timeout: "600s", wantErr: true},
		{name: "TimeoutNotPositive", timeout: "0", wantErr: true},
		{name: "PollIntervalNotADuration", pollInterval: "90", wantErr: true},
		{name: "PollIntervalNotPositive", pollInterval: "0s", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHookEnv(tc.timeout, tc.pollInterval, tc.ignoreFields)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseHookEnv(%q, %q, %q) = %+v, want error", tc.timeout, tc.pollInterval, tc.ignoreFields, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHookEnv(%q, %q, %q): %v", tc.timeout, tc.pollInterval, tc.ignoreFields, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseHookEnv(%q, %q, %q) = %+v, want %+v", tc.timeout, tc.pollInterval, tc.ignoreFields, got, tc.want)
			}
		})
	}
}

// TestHookStepArgs pins which overrides reach which step. The poll interval
// is the interesting one: it reaches every step that accepts it — the two
// `converge` runs, which wait on it, and `run`, which calibrates its
// slow-observe annotation with it — so a single environment variable
// describes the provider once and both checks are measured against the same
// cadence. It must NOT reach the two identity steps, which do not define the
// flag and would fail to parse it. An unset override passes no flag at all,
// leaving each subcommand's own documented default in force rather than
// having the hook restate it.
func TestHookStepArgs(t *testing.T) {
	const path = "/repo/examples/example-resource/example-resource.yaml"

	tests := []struct {
		name string
		step hookStep
		env  hookEnv
		want []string
	}{
		{
			name: "NoOverridesPassesOnlyTheManifest",
			step: hookStep{banner: "run", command: "run"},
			want: []string{path},
		},
		{
			name: "NoOverridesPassesOnlyTheManifestToConvergeEither",
			step: hookStep{banner: "converge", command: "converge"},
			want: []string{path},
		},
		{
			name: "RunTakesSecondsAsAnInteger",
			step: hookStep{banner: "run", command: "run"},
			env:  hookEnv{timeout: 600},
			want: []string{"--timeout", "600", path},
		},
		{
			name: "ConvergeTakesSecondsAsADuration",
			step: hookStep{banner: "converge", command: "converge"},
			env:  hookEnv{timeout: 600},
			want: []string{"--timeout", "10m0s", path},
		},
		{
			name: "ConvergeTakesThePollInterval",
			step: hookStep{banner: "post-update converge", command: "converge"},
			env:  hookEnv{pollInterval: 90 * time.Second},
			want: []string{"--poll-interval", "1m30s", path},
		},
		{
			name: "RunTakesThePollIntervalToo",
			step: hookStep{banner: "run", command: "run"},
			env:  hookEnv{pollInterval: 90 * time.Second},
			want: []string{"--poll-interval", "1m30s", path},
		},
		{
			name: "RunTakesBothOverrides",
			step: hookStep{banner: "run", command: "run"},
			env:  hookEnv{timeout: 600, pollInterval: 90 * time.Second},
			want: []string{"--poll-interval", "1m30s", "--timeout", "600", path},
		},
		{
			name: "PollIntervalIsNotPassedToResolveRecover",
			step: hookStep{banner: "resolve-recover", command: "resolve-recover"},
			env:  hookEnv{pollInterval: 90 * time.Second},
			want: []string{path},
		},
		{
			name: "PollIntervalIsNotPassedToTheExternalNameCheck",
			step: hookStep{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
			env:  hookEnv{pollInterval: 90 * time.Second},
			want: []string{path},
		},
		{
			name: "ConvergeTakesIgnoreFields",
			step: hookStep{banner: "converge", command: "converge"},
			env:  hookEnv{ignoreFields: []string{"latestBackup"}},
			want: []string{"--ignore-fields", "latestBackup", path},
		},
		{
			name: "ConvergeTakesMultipleIgnoreFieldsJoined",
			step: hookStep{banner: "post-update converge", command: "converge"},
			env:  hookEnv{ignoreFields: []string{"a", "b"}},
			want: []string{"--ignore-fields", "a,b", path},
		},
		{
			name: "IgnoreFieldsIsNotPassedToRun",
			step: hookStep{banner: "run", command: "run"},
			env:  hookEnv{ignoreFields: []string{"latestBackup"}},
			want: []string{path},
		},
		{
			name: "IgnoreFieldsIsNotPassedToResolveRecover",
			step: hookStep{banner: "resolve-recover", command: "resolve-recover"},
			env:  hookEnv{ignoreFields: []string{"latestBackup"}},
			want: []string{path},
		},
		{
			name: "IgnoreFieldsIsNotPassedToTheExternalNameCheck",
			step: hookStep{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
			env:  hookEnv{ignoreFields: []string{"latestBackup"}},
			want: []string{path},
		},
		{
			name: "ConvergeTakesEveryOverrideTogether",
			step: hookStep{banner: "converge", command: "converge"},
			env:  hookEnv{timeout: 600, pollInterval: 90 * time.Second, ignoreFields: []string{"latestBackup"}},
			want: []string{"--poll-interval", "1m30s", "--timeout", "10m0s", "--ignore-fields", "latestBackup", path},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hookStepArgs(tc.step, path, tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hookStepArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHookStepArgsPollIntervalIsAcceptedByEveryStepGivenIt closes the loop
// between hookStepArgs and the subcommands it builds argv for: a step handed
// --poll-interval that does not define the flag fails at parse time, in a
// live E2E run, long after a cluster was stood up. Rather than restating the
// list of accepting commands, this parses the argv with the real parser for
// each step the sequence can produce.
func TestHookStepArgsPollIntervalIsAcceptedByEveryStepGivenIt(t *testing.T) {
	const path = "/repo/examples/example-resource/example-resource.yaml"
	// ignoreFields is included here too: hookStepArgs itself gates it to
	// "converge" steps only (see the len(env.ignoreFields) > 0 && s.command
	// == "converge" guard), so an env carrying it alongside timeout and
	// pollInterval must still produce argv the real parser for every OTHER
	// step accepts — proving the gate, not just asserting it by inspection.
	env := hookEnv{timeout: 600, pollInterval: 90 * time.Second, ignoreFields: []string{"latestBackup"}}

	parsers := map[string]func([]string) error{
		"run":      func(a []string) error { _, err := parseRunArgs(a); return err },
		"converge": func(a []string) error { _, err := parseConvergeArgs(a); return err },
		"check-external-name-prefix": func(a []string) error {
			_, err := parseCheckExternalNamePrefixArgs(a)
			return err
		},
		"resolve-recover": func(a []string) error { _, err := parseResolveRecoverArgs(a); return err },
	}

	for _, s := range hookSteps(&manifest.Manifest{ExpectExternalNamePrefix: "example/"}, false) {
		t.Run(s.banner, func(t *testing.T) {
			parse, ok := parsers[s.command]
			if !ok {
				t.Fatalf("no parser registered for step command %q", s.command)
			}
			args := hookStepArgs(s, path, env)
			if s.command != "converge" && strings.Contains(strings.Join(args, " "), "--ignore-fields") {
				t.Errorf("%s received --ignore-fields (%q), want it gated to converge only", s.command, args)
			}
			if err := parse(args); err != nil {
				t.Errorf("%s rejects the argv the hook builds for it (%q): %v", s.command, args, err)
			}
		})
	}
}

// TestFlagAfterPositionalTakesEffect is the regression test for the single
// most-reported ergonomic bug in the copies this tool replaces: Go's flag
// package stops scanning at the first non-flag argument, so
// "run manifest.yaml --timeout 600" used to drop the flag silently and leave
// the operator staring at a timeout they never asked for. Every subcommand
// reorders its arguments, so both orders must behave identically.
func TestFlagAfterPositionalTakesEffect(t *testing.T) {
	const path = "manifest.yaml"

	t.Run("Run", func(t *testing.T) {
		for _, args := range [][]string{
			{"--timeout", "600", path},
			{path, "--timeout", "600"},
			{path, "--timeout=600"},
		} {
			got, err := parseRunArgs(args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.timeout != 600 {
				t.Errorf("parseRunArgs(%q) = %+v, want manifest %q and timeout 600", args, got, path)
			}
		}

		// --poll-interval reorders like every other flag, and defaults to
		// the 60s the slow-observe threshold has always assumed — so a
		// caller that never mentions it gets exactly today's behaviour.
		for _, args := range [][]string{
			{"--poll-interval", "10s", path},
			{path, "--poll-interval", "10s"},
			{path, "--poll-interval=10s"},
		} {
			got, err := parseRunArgs(args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.pollInterval != 10*time.Second {
				t.Errorf("parseRunArgs(%q) = %+v, want manifest %q and poll interval 10s", args, got, path)
			}
		}

		got, err := parseRunArgs([]string{path})
		if err != nil {
			t.Fatalf("parseRunArgs(%q): %v", []string{path}, err)
		}
		if got.pollInterval != 60*time.Second {
			t.Errorf("parseRunArgs default poll interval = %s, want 60s", got.pollInterval)
		}
	})

	t.Run("Converge", func(t *testing.T) {
		for _, args := range [][]string{
			{"--poll-interval", "90s", "--ignore-fields", "a,b", "--timeout", "300s", path},
			{path, "--poll-interval", "90s", "--ignore-fields", "a,b", "--timeout", "300s"},
			{"--poll-interval", "90s", path, "--timeout", "300s", "--ignore-fields", "a,b"},
		} {
			got, err := parseConvergeArgs(args)
			if err != nil {
				t.Fatalf("parseConvergeArgs(%q): %v", args, err)
			}
			want := convergeOptions{
				manifestPath:     path,
				pollInterval:     90 * time.Second,
				ignoreFields:     []string{"a", "b"},
				timeout:          300 * time.Second,
				readinessTimeout: 120 * time.Second,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("parseConvergeArgs(%q) = %+v, want %+v", args, got, want)
			}
		}

		// --readiness-timeout reorders like every other flag, and defaults
		// to the same 120s as --timeout so a caller that never mentions it
		// gets exactly today's behaviour.
		for _, args := range [][]string{
			{"--readiness-timeout", "45s", path},
			{path, "--readiness-timeout", "45s"},
			{path, "--readiness-timeout=45s"},
		} {
			got, err := parseConvergeArgs(args)
			if err != nil {
				t.Fatalf("parseConvergeArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.readinessTimeout != 45*time.Second {
				t.Errorf("parseConvergeArgs(%q) = %+v, want manifest %q and readiness timeout 45s", args, got, path)
			}
		}
	})

	t.Run("Validate", func(t *testing.T) {
		for _, args := range [][]string{
			{"--types-file", "types.go", path},
			{path, "--types-file", "types.go"},
		} {
			got, err := parseValidateArgs(args)
			if err != nil {
				t.Fatalf("parseValidateArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.typesFile != "types.go" {
				t.Errorf("parseValidateArgs(%q) = %+v, want manifest %q and types-file types.go", args, got, path)
			}
			if got.controllerDir != "" {
				t.Errorf("parseValidateArgs(%q).controllerDir = %q, want empty when --controller-dir is not passed", args, got.controllerDir)
			}
		}

		// --controller-dir reorders like every other flag, and is
		// optional — a caller that never mentions it gets today's
		// behaviour (the server-echoed check disabled).
		for _, args := range [][]string{
			{"--types-file", "types.go", "--controller-dir", "internal/controller/widget", path},
			{path, "--types-file", "types.go", "--controller-dir", "internal/controller/widget"},
			{path, "--types-file=types.go", "--controller-dir=internal/controller/widget"},
		} {
			got, err := parseValidateArgs(args)
			if err != nil {
				t.Fatalf("parseValidateArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.typesFile != "types.go" || got.controllerDir != "internal/controller/widget" {
				t.Errorf("parseValidateArgs(%q) = %+v, want manifest %q, types-file types.go, controller-dir internal/controller/widget", args, got, path)
			}
		}
	})

	t.Run("CheckExternalNamePrefix", func(t *testing.T) {
		for _, args := range [][]string{
			{"--timeout", "45", path},
			{path, "--timeout", "45"},
		} {
			got, err := parseCheckExternalNamePrefixArgs(args)
			if err != nil {
				t.Fatalf("parseCheckExternalNamePrefixArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.timeout != 45 {
				t.Errorf("parseCheckExternalNamePrefixArgs(%q) = %+v, want manifest %q and timeout 45", args, got, path)
			}
		}
	})

	t.Run("ResolveRecover", func(t *testing.T) {
		for _, args := range [][]string{
			{"--timeout", "240", path},
			{path, "--timeout", "240"},
		} {
			got, err := parseResolveRecoverArgs(args)
			if err != nil {
				t.Fatalf("parseResolveRecoverArgs(%q): %v", args, err)
			}
			if got.manifestPath != path || got.timeout != 240 {
				t.Errorf("parseResolveRecoverArgs(%q) = %+v, want manifest %q and timeout 240", args, got, path)
			}
		}
	})

	t.Run("Hook", func(t *testing.T) {
		for _, args := range [][]string{
			{"--root", "/repo", "post-assert-example.sh"},
			{"post-assert-example.sh", "--root", "/repo"},
		} {
			got, err := parseHookArgs(args)
			if err != nil {
				t.Fatalf("parseHookArgs(%q): %v", args, err)
			}
			if got.invocation != "post-assert-example.sh" || got.root != "/repo" {
				t.Errorf("parseHookArgs(%q) = %+v, want invocation post-assert-example.sh and root /repo", args, got)
			}
			if got.skipConverge {
				t.Errorf("parseHookArgs(%q).skipConverge = true, want false (flag absent)", args)
			}
		}
	})

	t.Run("HookSkipConverge", func(t *testing.T) {
		for _, args := range [][]string{
			{"--root", "/repo", "--skip-converge", "post-assert-example.sh"},
			{"post-assert-example.sh", "--root", "/repo", "--skip-converge"},
			{"--skip-converge", "post-assert-example.sh", "--root", "/repo"},
		} {
			got, err := parseHookArgs(args)
			if err != nil {
				t.Fatalf("parseHookArgs(%q): %v", args, err)
			}
			if got.invocation != "post-assert-example.sh" || got.root != "/repo" || !got.skipConverge {
				t.Errorf("parseHookArgs(%q) = %+v, want invocation post-assert-example.sh, root /repo, skipConverge true", args, got)
			}
		}
	})

	t.Run("RoundtripDiff", func(t *testing.T) {
		for _, args := range [][]string{
			{"--root", "/repo", "--timeout", "45", path},
			{path, "--root", "/repo", "--timeout", "45"},
			{path, "--timeout=45", "--root=/repo"},
		} {
			got, err := parseRoundtripDiffArgs(args)
			if err != nil {
				t.Fatalf("parseRoundtripDiffArgs(%q): %v", args, err)
			}
			if len(got.manifestPaths) != 1 || got.manifestPaths[0] != path || got.root != "/repo" || got.timeout != 45 {
				t.Errorf("parseRoundtripDiffArgs(%q) = %+v, want manifestPaths [%q], root /repo, timeout 45", args, got, path)
			}
		}
	})
}

// TestParseConvergeArgsRejectsDottedIgnoreField pins the flag-sourced half of
// the ignore-fields validation fix: manifest.ValidateIgnoreFields already
// rejected a dotted entry in the manifest's own "ignore-fields:" directive,
// but before this the --ignore-fields FLAG on `converge` — the path every
// provider in the fleet actually uses today — let the identical entry
// through silently, reached runner.ConvergeOptions.IgnoreFields, matched no
// top-level status.atProvider key in the diff, and produced the same
// "converge fails on drift you believed you excluded" outcome with no
// diagnostic. This is also the path UPDATE_TESTER_IGNORE_FIELDS reaches via
// hookStepArgs (see TestHookConvergeIgnoreFieldsRejectsDottedEntry below),
// so fixing parseConvergeArgs fixes both entry points at once.
func TestParseConvergeArgsRejectsDottedIgnoreField(t *testing.T) {
	const path = "/repo/examples/example-resource/example-resource.yaml"

	cases := map[string]struct {
		reason        string
		args          []string
		wantErrSubstr string
	}{
		"SingleDottedEntry": {
			reason:        "a nested path passed via --ignore-fields is rejected, naming the offending entry",
			args:          []string{"--ignore-fields", "ruleChoice.legacyRuleList", path},
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
		"DottedEntryAmongValidOnes": {
			reason:        "one bad entry in a comma-separated --ignore-fields list is still caught, even alongside otherwise-valid top-level names",
			args:          []string{"--ignore-fields", "latestBackup,ruleChoice.legacyRuleList,kvm", path},
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseConvergeArgs(tc.args)
			if err == nil {
				t.Fatalf("%s: parseConvergeArgs(%q) error = nil, want an error rejecting the dotted ignore-fields entry", tc.reason, tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: parseConvergeArgs(%q) error = %q, want it to contain %q", tc.reason, tc.args, err.Error(), tc.wantErrSubstr)
			}
		})
	}

	// A valid, non-dotted --ignore-fields still parses cleanly — the fix
	// must not reject the flag entirely.
	got, err := parseConvergeArgs([]string{"--ignore-fields", "latestBackup", path})
	if err != nil {
		t.Fatalf("parseConvergeArgs with a valid top-level field: %v", err)
	}
	if !reflect.DeepEqual(got.ignoreFields, []string{"latestBackup"}) {
		t.Errorf("parseConvergeArgs valid --ignore-fields = %#v, want [latestBackup]", got.ignoreFields)
	}
}

// TestParseConvergeAllArgsRejectsDottedIgnoreField is the same fix, pinned
// against converge-all's own --ignore-fields flag (the fleet-wide default
// unioned onto each target's manifest — see convergeAllOptions).
func TestParseConvergeAllArgsRejectsDottedIgnoreField(t *testing.T) {
	const path = "/repo/examples/example-resource/example-resource.yaml"

	cases := map[string]struct {
		reason        string
		args          []string
		wantErrSubstr string
	}{
		"SingleDottedEntry": {
			reason:        "a nested path passed via converge-all's --ignore-fields is rejected, naming the offending entry",
			args:          []string{"--ignore-fields", "ruleChoice.legacyRuleList", path},
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
		"DottedEntryAmongValidOnes": {
			reason:        "one bad entry in a comma-separated list is still caught, even alongside otherwise-valid top-level names",
			args:          []string{"--ignore-fields", "status,ruleChoice.legacyRuleList", path},
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseConvergeAllArgs(tc.args)
			if err == nil {
				t.Fatalf("%s: parseConvergeAllArgs(%q) error = nil, want an error rejecting the dotted ignore-fields entry", tc.reason, tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: parseConvergeAllArgs(%q) error = %q, want it to contain %q", tc.reason, tc.args, err.Error(), tc.wantErrSubstr)
			}
		})
	}

	got, err := parseConvergeAllArgs([]string{"--ignore-fields", "status", path})
	if err != nil {
		t.Fatalf("parseConvergeAllArgs with a valid top-level field: %v", err)
	}
	if !reflect.DeepEqual(got.ignoreFields, []string{"status"}) {
		t.Errorf("parseConvergeAllArgs valid --ignore-fields = %#v, want [status]", got.ignoreFields)
	}
}

// TestHookConvergeIgnoreFieldsRejectsDottedEntry proves the third entry
// point named in the fix's acceptance criteria: UPDATE_TESTER_IGNORE_FIELDS
// reaches validation too, because the hook never parses the env var itself —
// it forwards it into `converge`'s own --ignore-fields flag via
// hookStepArgs, and runCommand("converge", ...) dispatches straight into
// cmdConverge -> parseConvergeArgs. Round-tripping hookStepArgs' own output
// back through parseConvergeArgs is therefore a faithful reproduction of
// what cmdHook actually does at runtime, without needing a live cluster.
func TestHookConvergeIgnoreFieldsRejectsDottedEntry(t *testing.T) {
	const path = "/repo/examples/example-resource/example-resource.yaml"

	env := hookEnv{ignoreFields: []string{"latestBackup", "ruleChoice.legacyRuleList"}}
	step := hookStep{banner: "post-update converge", command: "converge"}

	args := hookStepArgs(step, path, env)

	_, err := parseConvergeArgs(args)
	if err == nil {
		t.Fatalf("parseConvergeArgs(%q) (from hookStepArgs with a dotted UPDATE_TESTER_IGNORE_FIELDS entry) error = nil, want an error", args)
	}
	wantErrSubstr := `ignore-fields entry "ruleChoice.legacyRuleList"`
	if !strings.Contains(err.Error(), wantErrSubstr) {
		t.Errorf("parseConvergeArgs(%q) error = %q, want it to contain %q", args, err.Error(), wantErrSubstr)
	}
}

func TestParseArgsRequiresItsPositional(t *testing.T) {
	tests := []struct {
		name  string
		parse func([]string) error
	}{
		{name: "Run", parse: func(a []string) error { _, err := parseRunArgs(a); return err }},
		{name: "Converge", parse: func(a []string) error { _, err := parseConvergeArgs(a); return err }},
		{name: "CheckExternalNamePrefix", parse: func(a []string) error { _, err := parseCheckExternalNamePrefixArgs(a); return err }},
		{name: "ResolveRecover", parse: func(a []string) error { _, err := parseResolveRecoverArgs(a); return err }},
		{name: "Hook", parse: func(a []string) error { _, err := parseHookArgs(a); return err }},
		{name: "RoundtripDiff", parse: func(a []string) error { _, err := parseRoundtripDiffArgs(a); return err }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.parse(nil); err == nil {
				t.Error("missing required argument accepted, want a usage error")
			}
		})
	}
}

func TestParseValidateRequiresTypesFile(t *testing.T) {
	if _, err := parseValidateArgs([]string{"manifest.yaml"}); err == nil {
		t.Error("validate without --types-file accepted, want an error")
	}
}

// TestMergeIgnoreFields covers the union/dedup/ordering contract
// buildConvergeTargets relies on: the fleet-wide default and a target's own
// per-manifest exclusion list combine without either silently discarding the
// other, without duplicating a field named in both, and with a stable order
// (fleet-wide first, then the manifest's own entries) so command output is
// deterministic.
func TestMergeIgnoreFields(t *testing.T) {
	cases := map[string]struct {
		reason      string
		fleetWide   []string
		perResource []string
		want        []string
	}{
		"BothEmpty": {
			reason:      "no exclusions at all is a valid, common case — nil, not an empty non-nil slice",
			fleetWide:   nil,
			perResource: nil,
			want:        nil,
		},
		"FleetWideOnlyNoManifestDirective": {
			reason:      "a manifest that predates the ignore-fields: directive still gets the flag's set (f5xc's uniform case)",
			fleetWide:   []string{"status"},
			perResource: nil,
			want:        []string{"status"},
		},
		"PerResourceOnlyNoFlag": {
			reason:      "the common vultr case: no fleet-wide flag at all, only each manifest's own directive",
			fleetWide:   nil,
			perResource: []string{"latestBackup"},
			want:        []string{"latestBackup"},
		},
		"BothPresentDisjoint": {
			reason:      "the flag's set and the manifest's own set both apply — neither replaces the other",
			fleetWide:   []string{"status"},
			perResource: []string{"latestBackup"},
			want:        []string{"status", "latestBackup"},
		},
		"OverlappingFieldIsNotDuplicated": {
			reason:      "a field named in both sets appears exactly once in the merged result",
			fleetWide:   []string{"status", "lastSyncTime"},
			perResource: []string{"lastSyncTime", "kvm"},
			want:        []string{"status", "lastSyncTime", "kvm"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := mergeIgnoreFields(tc.fleetWide, tc.perResource)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: mergeIgnoreFields(%v, %v) = %#v, want %#v", tc.reason, tc.fleetWide, tc.perResource, got, tc.want)
			}
		})
	}
}

// TestBuildConvergeTargetsSourcesIgnoreFieldsPerResource pins the actual
// defect: a converge-all invocation covering several manifests with
// DIFFERENT "ignore-fields:" directives must configure each target's own
// ConvergeOptions.IgnoreFields from that target's own manifest, not from one
// shared list — the vultr shape (database / firewall-rule / instance, three
// disjoint exclusion sets) that a single fleet-wide flag cannot represent
// without unioning all three onto every resource.
func TestBuildConvergeTargetsSourcesIgnoreFieldsPerResource(t *testing.T) {
	dir := t.TempDir()

	writeManifest := func(fileName, kind, name, ignoreFieldsDirective string) string {
		path := filepath.Join(dir, fileName)
		yamlDoc := "apiVersion: vultr.example.crossplane.io/v1alpha1\n" +
			"kind: " + kind + "\n" +
			"metadata:\n" +
			"  name: " + name + "\n" +
			"  annotations:\n" +
			"    crossplane.io/update-test: |\n" +
			"      ignore-fields: " + ignoreFieldsDirective + "\n" +
			"      - field: label\n" +
			"        value: \"updated\"\n"
		if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
			t.Fatalf("writing fixture manifest %s: %v", fileName, err)
		}
		return path
	}

	databasePath := writeManifest("database.yaml", "Database", "example-database", "latestBackup")
	firewallPath := writeManifest("firewall-rule.yaml", "FirewallRule", "example-firewall-rule", "ruleCount,dateModified")
	instancePath := writeManifest("instance.yaml", "Instance", "example-instance", "kvm,powerStatus,serverStatus")

	targets, err := buildConvergeTargets(convergeAllOptions{
		manifestPaths: []string{databasePath, firewallPath, instancePath},
		pollInterval:  60 * time.Second,
		timeout:       120 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildConvergeTargets() error = %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("len(targets) = %d, want 3", len(targets))
	}

	want := map[string][]string{
		"Database/example-database":          {"latestBackup"},
		"FirewallRule/example-firewall-rule": {"ruleCount", "dateModified"},
		"Instance/example-instance":          {"kvm", "powerStatus", "serverStatus"},
	}
	seen := make(map[string]bool, len(targets))
	for _, tgt := range targets {
		seen[tgt.Label] = true
		wantFields, ok := want[tgt.Label]
		if !ok {
			t.Errorf("unexpected target label %q", tgt.Label)
			continue
		}
		if !reflect.DeepEqual(tgt.Opts.IgnoreFields, wantFields) {
			t.Errorf("target %s: IgnoreFields = %#v, want %#v — a resource's exclusion set must not leak onto a sibling target",
				tgt.Label, tgt.Opts.IgnoreFields, wantFields)
		}
	}
	for label := range want {
		if !seen[label] {
			t.Errorf("expected target %q was not built", label)
		}
	}
}

// TestBuildConvergeTargetsUnionsFleetWideFlagOntoEachManifest confirms the
// fleet-wide --ignore-fields flag still applies — as an ADDITIONAL default
// unioned onto each target, not as the only mechanism (see
// convergeAllOptions). A manifest with no directive of its own gets exactly
// the flag's set; a manifest that also declares its own gets the union of
// both.
func TestBuildConvergeTargetsUnionsFleetWideFlagOntoEachManifest(t *testing.T) {
	dir := t.TempDir()

	writeManifest := func(fileName, name string, directiveLine string) string {
		path := filepath.Join(dir, fileName)
		yamlDoc := "apiVersion: f5xc.example.crossplane.io/v1alpha1\n" +
			"kind: LoadBalancer\n" +
			"metadata:\n" +
			"  name: " + name + "\n"
		if directiveLine != "" {
			yamlDoc += "  annotations:\n" +
				"    crossplane.io/update-test: |\n" +
				"      " + directiveLine + "\n" +
				"      - field: label\n" +
				"        value: \"updated\"\n"
		}
		if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
			t.Fatalf("writing fixture manifest %s: %v", fileName, err)
		}
		return path
	}

	noDirectivePath := writeManifest("no-directive.yaml", "example-no-directive", "")
	ownDirectivePath := writeManifest("own-directive.yaml", "example-own-directive", "ignore-fields: forwardRules")

	targets, err := buildConvergeTargets(convergeAllOptions{
		manifestPaths: []string{noDirectivePath, ownDirectivePath},
		pollInterval:  60 * time.Second,
		timeout:       120 * time.Second,
		ignoreFields:  []string{"status"},
	})
	if err != nil {
		t.Fatalf("buildConvergeTargets() error = %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("len(targets) = %d, want 2", len(targets))
	}

	for _, tgt := range targets {
		switch tgt.Label {
		case "LoadBalancer/example-no-directive":
			if want := []string{"status"}; !reflect.DeepEqual(tgt.Opts.IgnoreFields, want) {
				t.Errorf("target %s: IgnoreFields = %#v, want %#v (fleet-wide default only)", tgt.Label, tgt.Opts.IgnoreFields, want)
			}
		case "LoadBalancer/example-own-directive":
			if want := []string{"status", "forwardRules"}; !reflect.DeepEqual(tgt.Opts.IgnoreFields, want) {
				t.Errorf("target %s: IgnoreFields = %#v, want %#v (fleet-wide default unioned with the manifest's own directive)", tgt.Label, tgt.Opts.IgnoreFields, want)
			}
		default:
			t.Errorf("unexpected target label %q", tgt.Label)
		}
	}
}

// TestConvergeOptionsForSourcesManifestIgnoreFields pins the single-resource
// counterpart to TestBuildConvergeTargetsSourcesIgnoreFieldsPerResource: the
// hook path runs `hook` -> `converge` (cmdConverge), never `converge-all`, so
// before this fix a manifest's own "ignore-fields:" directive was read by no
// code any provider's E2E run actually executes — only the --ignore-fields
// flag (and the UPDATE_TESTER_IGNORE_FIELDS environment variable forwarded
// through it) took effect. convergeOptionsFor must now union both sources,
// exactly as buildConvergeTargets already does for converge-all.
func TestConvergeOptionsForSourcesManifestIgnoreFields(t *testing.T) {
	dir := t.TempDir()
	writeManifest := func(fileName, directiveLine string) string {
		path := filepath.Join(dir, fileName)
		yamlDoc := "apiVersion: network.example.crossplane.io/v1alpha1\n" +
			"kind: Network\n" +
			"metadata:\n" +
			"  name: example-network\n"
		if directiveLine != "" {
			yamlDoc += "  annotations:\n" +
				"    crossplane.io/update-test: |\n" +
				"      " + directiveLine + "\n" +
				"      - field: comment\n" +
				"        value: \"updated\"\n"
		}
		if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
			t.Fatalf("writing fixture manifest %s: %v", fileName, err)
		}
		return path
	}

	cases := map[string]struct {
		reason        string
		directiveLine string
		flagIgnore    []string
		want          []string
	}{
		"ManifestDirectiveOnlyNoFlag": {
			reason:        "the common case: no --ignore-fields flag at all, only the manifest's own directive — this is the exact shape the hook path runs, and it used to configure IgnoreFields as nil",
			directiveLine: "ignore-fields: latestBackup",
			flagIgnore:    nil,
			want:          []string{"latestBackup"},
		},
		"FlagOnlyNoManifestDirective": {
			reason:     "a manifest that predates the directive still gets the flag's set — the pre-fix behaviour must not regress",
			flagIgnore: []string{"status"},
			want:       []string{"status"},
		},
		"BothPresentUnioned": {
			reason:        "the flag and the manifest's own directive both apply — neither replaces the other",
			directiveLine: "ignore-fields: latestBackup",
			flagIgnore:    []string{"status"},
			want:          []string{"status", "latestBackup"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeManifest(name+".yaml", tc.directiveLine)
			m, err := manifest.Parse(path)
			if err != nil {
				t.Fatalf("%s: manifest.Parse() error = %v", tc.reason, err)
			}
			opts := convergeOptions{
				manifestPath: path,
				pollInterval: 60 * time.Second,
				ignoreFields: tc.flagIgnore,
				timeout:      120 * time.Second,
			}
			got := convergeOptionsFor(opts, m)
			if !reflect.DeepEqual(got.IgnoreFields, tc.want) {
				t.Errorf("%s: convergeOptionsFor().IgnoreFields = %#v, want %#v", tc.reason, got.IgnoreFields, tc.want)
			}
		})
	}
}

func TestRunCommandRejectsUnknownCommand(t *testing.T) {
	err := runCommand("converg", nil)
	if err == nil {
		t.Fatal("unknown command accepted")
	}
	if !errors.Is(err, errUnknownCommand) {
		t.Errorf("error %v does not wrap errUnknownCommand — main would not print usage", err)
	}
	if !strings.Contains(err.Error(), "converg") {
		t.Errorf("error %q does not name the command the operator typed", err)
	}
}

// TestRunCommandDispatchesRoundtripDiff proves the new subcommand actually
// reaches runCommand's dispatch switch: calling it with no positional
// argument must fail with roundtrip-diff's OWN usage error, not
// errUnknownCommand — the failure mode that would appear if the "case
// roundtrip-diff" line above were ever accidentally removed or misspelled.
func TestRunCommandDispatchesRoundtripDiff(t *testing.T) {
	err := runCommand("roundtrip-diff", nil)
	if err == nil {
		t.Fatal("roundtrip-diff with no manifest accepted, want a usage error")
	}
	if errors.Is(err, errUnknownCommand) {
		t.Errorf("error %v wraps errUnknownCommand — roundtrip-diff is not wired into the dispatch switch", err)
	}
	if !strings.Contains(err.Error(), "roundtrip-diff") {
		t.Errorf("error %q does not name the roundtrip-diff usage", err)
	}
}

// TestParseRoundtripDiffArgsDefaultsRootToWorkingDirectory mirrors
// cmdHook's own --root default (parseHookArgs / cmdHook), which every other
// user of hook.Resolve already relies on: an operator invoking the command
// directly, without --root, gets the process's own working directory
// rather than an error.
func TestParseRoundtripDiffArgsDefaultsRootToWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	got, err := parseRoundtripDiffArgs([]string{"manifest.yaml"})
	if err != nil {
		t.Fatalf("parseRoundtripDiffArgs: %v", err)
	}
	if got.root != wd {
		t.Errorf("root = %q, want the working directory %q", got.root, wd)
	}
}

// TestParseRoundtripDiffArgsAcceptsCommaSeparatedManifests mirrors
// converge-all's own manifest-list convention (parseConvergeAllArgs): a
// single comma-separated argument and repeated positional arguments must be
// equivalent.
func TestParseRoundtripDiffArgsAcceptsCommaSeparatedManifests(t *testing.T) {
	want := []string{"a.yaml", "b.yaml", "c.yaml"}

	commaJoined, err := parseRoundtripDiffArgs([]string{"--root", "/repo", "a.yaml,b.yaml,c.yaml"})
	if err != nil {
		t.Fatalf("parseRoundtripDiffArgs (comma-joined): %v", err)
	}
	if !reflect.DeepEqual(commaJoined.manifestPaths, want) {
		t.Errorf("manifestPaths (comma-joined) = %#v, want %#v", commaJoined.manifestPaths, want)
	}

	repeated, err := parseRoundtripDiffArgs([]string{"--root", "/repo", "a.yaml", "b.yaml", "c.yaml"})
	if err != nil {
		t.Fatalf("parseRoundtripDiffArgs (repeated positional): %v", err)
	}
	if !reflect.DeepEqual(repeated.manifestPaths, want) {
		t.Errorf("manifestPaths (repeated positional) = %#v, want %#v", repeated.manifestPaths, want)
	}
}

func TestVersionFrom(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{name: "NoBuildInfo", want: devVersion},
		{
			name: "BuiltFromAWorkingTree",
			info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: devVersion}},
			want: devVersion,
		},
		{
			name: "BuiltAsItsOwnMainModule",
			info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.2.0"}},
			want: "v0.2.0",
		},
		{
			name: "InstalledAsAToolOfAConsumerStub",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/provider/tools/update-tester", Version: devVersion},
				Deps: []*debug.Module{
					{Path: "gopkg.in/yaml.v3", Version: "v3.0.1"},
					{Path: modulePath, Version: "v0.2.0"},
				},
			},
			want: "v0.2.0",
		},
		{
			name: "ReplacedDependencyReportsTheReplacement",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/provider/tools/update-tester", Version: devVersion},
				Deps: []*debug.Module{
					{Path: modulePath, Version: "v0.2.0", Replace: &debug.Module{Path: modulePath, Version: "v0.3.0-rc1"}},
				},
			},
			want: "v0.3.0-rc1",
		},
		{
			name: "UnrelatedMainModuleAndNoDependencyFallsBack",
			info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/other", Version: ""}},
			want: devVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFrom(tc.info); got != tc.want {
				t.Errorf("versionFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCmdVersionPrintsAVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdVersion(&buf); err != nil {
		t.Fatalf("cmdVersion: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "update-tester ") || !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("cmdVersion printed %q, want a single \"update-tester <version>\" line", buf.String())
	}
}

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "Zero", d: 0, want: "0s"},
		{name: "Seconds", d: 42 * time.Second, want: "42s"},
		{name: "RoundsToWholeSeconds", d: 1500 * time.Millisecond, want: "2s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmtDuration(tc.d); got != tc.want {
				t.Errorf("fmtDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

func TestVerdict(t *testing.T) {
	if got := verdict(true); got != "PASS" {
		t.Errorf("verdict(true) = %q, want PASS", got)
	}
	if got := verdict(false); got != "FAIL" {
		t.Errorf("verdict(false) = %q, want FAIL", got)
	}
}

// readmeCommandsFenceLines extracts the update-tester invocation lines from
// README.md's "## Commands" fenced code block, trimming each line's leading
// indentation so it is directly comparable to usageSynopsis. The test runs
// with the repo root as its working directory, so "README.md" resolves
// without needing to locate the module root.
func readmeCommandsFenceLines(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	var lines []string
	inSection, inFence := false, false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "## Commands"):
			inSection = true
		case inSection && !inFence && trimmed == "```":
			inFence = true
		case inFence && trimmed == "```":
			return lines
		case inFence && trimmed != "":
			lines = append(lines, trimmed)
		}
	}

	t.Fatalf(`README.md: no closed fence found under "## Commands"`)
	return nil
}

// mainDocCommentUsageLines extracts the Usage: block from main.go's package
// doc comment (the comment immediately above "package main"), stripping the
// "//" comment marker and one leading tab so the result is directly
// comparable to usageSynopsis.
func mainDocCommentUsageLines(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}

	var lines []string
	inUsage := false
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "//") {
			if inUsage {
				break // "package main" ends the doc comment
			}
			continue
		}
		content := strings.TrimPrefix(line, "//")
		if strings.TrimSpace(content) == "Usage:" {
			inUsage = true
			continue
		}
		if !inUsage {
			continue
		}
		if trimmed := strings.TrimSpace(content); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}

	if len(lines) == 0 {
		t.Fatalf("main.go: no Usage: block found in the package doc comment")
	}
	return lines
}

// TestUsageSynopsisSourcesAgree guards the three surfaces that document the
// CLI's subcommand list against drifting apart: usageSynopsis (the literal
// printUsage prints, referenced directly here — no shelling out to `go run`),
// main.go's package doc comment Usage: block, and README.md's "## Commands"
// fence. This has been hand-repaired twice already (once for converge-all,
// once for hook's --skip-converge); the next flag or subcommand added must
// fail this test instead of drifting a third surface silently.
//
// Normalisation is limited to each line's leading indentation: flag
// spelling, brackets and argument placeholders are compared byte-for-byte
// via reflect.DeepEqual on the full ordered line slices.
func TestUsageSynopsisSourcesAgree(t *testing.T) {
	var want []string
	for _, line := range strings.Split(usageSynopsis, "\n") {
		want = append(want, strings.TrimSpace(line))
	}

	t.Run("PackageDocComment", func(t *testing.T) {
		got := mainDocCommentUsageLines(t)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("main.go's package doc comment Usage: block diverges from usageSynopsis:\ngot:  %#v\nwant: %#v", got, want)
		}
	})

	t.Run("ReadmeCommandsFence", func(t *testing.T) {
		got := readmeCommandsFenceLines(t)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("README.md's ## Commands fence diverges from usageSynopsis:\ngot:  %#v\nwant: %#v", got, want)
		}
	})
}

// reReadmeValidateHeading matches the exact "### `validate` — offline
// coverage check" heading that opens the section
// readmeValidateStatusBullets scans, and reReadmeAnyHeading matches any
// "###" heading, used to detect where that section ends.
var (
	reReadmeValidateHeading = regexp.MustCompile("^### `validate` \u2014 offline coverage check$")
	reReadmeAnyHeading      = regexp.MustCompile(`^#{1,6}\s`)
	reReadmeBulletHead      = regexp.MustCompile(`^- (.+?) \x{2014} `)
	reReadmeCodeSpan        = regexp.MustCompile("`([^`]+)`")
)

// readmeValidateStatusBullets extracts the status names documented in
// README.md's "### `validate` — offline coverage check" section. Each
// bullet line there opens with one or more backtick-quoted status names
// followed by " — " and a description, e.g.:
//
//   - `tested` / `skipped` — the field appears in the annotation.
//
// which yields ["tested", "skipped"]. Only backtick spans appearing before
// the first em dash on a bullet's opening line are taken, so backtick code
// spans inside the description (e.g. `self == oldSelf`) are not mistaken
// for status names.
func readmeValidateStatusBullets(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	var statuses []string
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if reReadmeValidateHeading.MatchString(trimmed) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if reReadmeAnyHeading.MatchString(trimmed) {
			break // the next heading of any level ends the section
		}
		m := reReadmeBulletHead.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		for _, code := range reReadmeCodeSpan.FindAllStringSubmatch(m[1], -1) {
			statuses = append(statuses, code[1])
		}
	}

	if !inSection {
		t.Fatalf("README.md: no \"### `validate` — offline coverage check\" heading found")
	}
	if len(statuses) == 0 {
		t.Fatalf("README.md: no status bullets found under the `validate` offline coverage check section")
	}
	return statuses
}

// TestValidateStatusesDocumented guards README.md's `validate` offline
// coverage check status enumeration against drifting away from
// validator.KnownStatuses — the status set PrintValidation can actually
// emit. The comparison runs in both directions: a status PrintValidation can
// emit but README.md never mentions, and a status README.md documents that
// PrintValidation can never produce, are both failures. This has already
// silently drifted twice (README fell behind two added statuses across two
// separate feature commits); the next status added or removed must fail
// this test instead of leaving the README's enumeration wrong.
func TestValidateStatusesDocumented(t *testing.T) {
	code := validator.KnownStatuses()
	doc := readmeValidateStatusBullets(t)

	codeSet := make(map[string]bool, len(code))
	for _, s := range code {
		codeSet[s] = true
	}
	docSet := make(map[string]bool, len(doc))
	for _, s := range doc {
		docSet[s] = true
	}

	for _, s := range code {
		if !docSet[s] {
			t.Errorf("validator.KnownStatuses() includes %q, but README.md's `validate` status bullet list does not document it", s)
		}
	}
	for _, s := range doc {
		if !codeSet[s] {
			t.Errorf("README.md's `validate` status bullet list documents %q, but validator.KnownStatuses() does not include it — PrintValidation can never emit this status", s)
		}
	}
}
