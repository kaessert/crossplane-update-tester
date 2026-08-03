package main

import (
	"bytes"
	"errors"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/runner"
)

// counts mirrors the five return values of printResults, so a table can
// declare an expected outcome in one literal instead of five parallel fields.
type counts struct {
	passed, failed, noop, notEvidenced, untrusted int
}

func run(results []runner.TestResult) (string, counts) {
	var buf bytes.Buffer
	p, f, n, ne, u := printResults(&buf, results)
	return buf.String(), counts{p, f, n, ne, u}
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
			name:     "PassIsCountedOnce",
			results:  []runner.TestResult{{Field: "comment", Passed: true, Expected: "a", Actual: "a", Duration: 2 * time.Second}},
			want:     counts{passed: 1},
			contains: []string{`  ✓ comment: "a" → "a" (2s)`, "Differential: all non-target fields stable ✓"},
		},
		{
			name:     "SlowPassIsAnnotatedButStillPasses",
			results:  []runner.TestResult{{Field: "comment", Passed: true, SlowObserve: true, Expected: "a", Actual: "a", Duration: 45 * time.Second}},
			want:     counts{passed: 1},
			contains: []string{`  ✓ comment: "a" → "a" (45s, slow-observe)`},
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
				Field: "comment", Passed: true, Expected: "a", Actual: "a", Duration: time.Second,
				SideFx: []differ.FieldChange{{Field: "ttl", OldValue: "30", NewValue: "60"}},
			}},
			want:     counts{passed: 1},
			contains: []string{"    ⚠ side effects: ttl: 30 → 60"},
		},
		{
			name: "MixedRunCountsEveryModeSeparately",
			results: []runner.TestResult{
				{Field: "a", Passed: true, Expected: "1", Actual: "1"},
				{Field: "b", Skipped: true, SkipMsg: "immutable"},
				{Field: "c", NoOp: true, Error: errors.New("no-op")},
				{Field: "d", NotEvidenced: true, Error: errors.New("no event")},
				{Field: "e", EvidenceUntrusted: true, Passed: true},
				{Field: "f", Expected: "1", Actual: "2"},
			},
			want: counts{passed: 1, failed: 4, noop: 1, notEvidenced: 1, untrusted: 1},
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
		{name: "UntrustedWhilePassing", result: runner.TestResult{Field: "comment", EvidenceUntrusted: true, Passed: true, Expected: "a", Actual: "a"}},
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

func TestHookStepsGateOnPrefixAnnotation(t *testing.T) {
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
			got := hookSteps(tc.manifest)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hookSteps() = %+v, want %+v", got, tc.want)
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
	for _, s := range hookSteps(&manifest.Manifest{ExpectExternalNamePrefix: "example/"}) {
		if !known[s.command] {
			t.Errorf("step %q dispatches unknown command %q", s.banner, s.command)
		}
	}
}

func TestParseHookEnv(t *testing.T) {
	tests := []struct {
		name         string
		timeout      string
		pollInterval string
		want         hookEnv
		wantErr      bool
	}{
		{name: "UnsetLeavesSubcommandDefaults", want: hookEnv{}},
		{name: "TimeoutSeconds", timeout: "600", want: hookEnv{timeout: 600}},
		{name: "PollIntervalDuration", pollInterval: "90s", want: hookEnv{pollInterval: 90 * time.Second}},
		{name: "Both", timeout: "600", pollInterval: "2m", want: hookEnv{timeout: 600, pollInterval: 2 * time.Minute}},
		{name: "TimeoutNotANumber", timeout: "600s", wantErr: true},
		{name: "TimeoutNotPositive", timeout: "0", wantErr: true},
		{name: "PollIntervalNotADuration", pollInterval: "90", wantErr: true},
		{name: "PollIntervalNotPositive", pollInterval: "0s", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHookEnv(tc.timeout, tc.pollInterval)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseHookEnv(%q, %q) = %+v, want error", tc.timeout, tc.pollInterval, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHookEnv(%q, %q): %v", tc.timeout, tc.pollInterval, err)
			}
			if got != tc.want {
				t.Errorf("parseHookEnv(%q, %q) = %+v, want %+v", tc.timeout, tc.pollInterval, got, tc.want)
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
	env := hookEnv{timeout: 600, pollInterval: 90 * time.Second}

	parsers := map[string]func([]string) error{
		"run":      func(a []string) error { _, err := parseRunArgs(a); return err },
		"converge": func(a []string) error { _, err := parseConvergeArgs(a); return err },
		"check-external-name-prefix": func(a []string) error {
			_, err := parseCheckExternalNamePrefixArgs(a)
			return err
		},
		"resolve-recover": func(a []string) error { _, err := parseResolveRecoverArgs(a); return err },
	}

	for _, s := range hookSteps(&manifest.Manifest{ExpectExternalNamePrefix: "example/"}) {
		t.Run(s.banner, func(t *testing.T) {
			parse, ok := parsers[s.command]
			if !ok {
				t.Fatalf("no parser registered for step command %q", s.command)
			}
			args := hookStepArgs(s, path, env)
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
				manifestPath: path,
				pollInterval: 90 * time.Second,
				ignoreFields: []string{"a", "b"},
				timeout:      300 * time.Second,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("parseConvergeArgs(%q) = %+v, want %+v", args, got, want)
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
		}
	})
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
