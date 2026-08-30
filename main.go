// Command update-tester reads Crossplane example manifests and runs
// per-field update tests, offline mutable-field coverage checks, and
// post-create convergence checks against a live cluster to validate the
// Update() reconciler path.
//
// Update-path proof: each `run` field test forces a second, independent
// reconcile after the one that calls Update(), so status.atProvider is
// refreshed by a genuine post-update Observe instead of depending on the
// provider's background poll tick. The second reconcile is triggered by
// patching a private metadata annotation (NudgeReconcile) rather than by
// repeating the status-conditions clear used to drive the first one: most
// generated controllers watch with resource.DesiredStateChanged(), which
// only reacts to an annotation, label, or generation (spec) change, so a
// status-only patch alone would be filtered out and never reach the
// reconciler. It also counts the aggregated UpdatedExternalResource /
// CannotUpdateExternalResource events for the resource before and after the
// patch — a field whose value matches the target but whose event count
// never grew is reported as NOT-EVIDENCED, not PASS, because a value match
// without an event is not proof Update() ran. A field that still takes
// poll-interval-scale time to converge despite all of this is annotated
// "slow-observe" rather than left looking like an ordinary fast PASS —
// poll-interval-scale meaning exactly that: the bar is half of the
// provider's poll interval (`run --poll-interval`), so it tracks the
// provider under test rather than a fixed number of seconds. This should
// only happen when the backend itself is slow to propagate the change,
// since the forced second reconcile removes the timing race that used to
// produce it. If a slow-observe result appears alongside a low
// update-event count, treat it as a real signal and check the controller
// logs for repeated Update calls rather than assuming it is a benign
// propagation delay.
//
// The `run` command also proactively restarts the controller Deployment
// partway through a long field list to earn back a fresh event-spam-filter
// burst (client-go silently drops events beyond a per-process ceiling). If
// that restart fails, every field tested afterward has its evidence check
// marked UNTRUSTED rather than being allowed to report a clean PASS or
// NOT-EVIDENCED: with the burst state unknown, neither outcome can be
// trusted to prove or disprove that Update() ran, so the run's summary line
// can never read "0 not-evidenced" while masking an unreliable result.
//
// Usage:
//
//	update-tester run <manifest.yaml> [--timeout 120] [--poll-interval 60s]
//	update-tester converge <manifest.yaml> [--poll-interval 60s] [--ignore-fields a,b] [--timeout 120s] [--readiness-timeout 120s]
//	update-tester converge-all <m1.yaml,m2.yaml,...> [--poll-interval 60s] [--concurrency 8] [--timeout 120s] [--readiness-timeout 120s]
//	update-tester batch <m1.yaml,m2.yaml,...> [--parallel 1] [--timeout 120] [--poll-interval 60s]
//	update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]
//	update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
//	update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
//	update-tester resolve-recover <manifest.yaml> [--timeout 120]
//	update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
//	update-tester roundtrip-verify <m1.yaml,m2.yaml,...> --backend <real|simulator> [--root <dir>] [--timeout 30]
//	update-tester residual <examples-dir>
//	update-tester hook <invocation-name> [--root <dir>] [--manifest <path>] [--skip-converge]
//	update-tester version
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/cli"
	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/hook"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
	"github.com/kaessert/crossplane-update-tester/internal/residual"
	"github.com/kaessert/crossplane-update-tester/internal/roundtrip"
	"github.com/kaessert/crossplane-update-tester/internal/runner"
	"github.com/kaessert/crossplane-update-tester/internal/validator"
)

// errUnknownCommand marks the one error class that warrants reprinting the
// usage text: the operator named a subcommand that does not exist. Every
// other failure is about a manifest or a cluster, where usage text is noise.
var errUnknownCommand = errors.New("unknown command")

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "help", "--help", "-h":
		printUsage()
		return
	}

	if err := runCommand(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if errors.Is(err, errUnknownCommand) {
			fmt.Fprintln(os.Stderr)
			printUsage()
		}
		os.Exit(1)
	}
}

// runCommand dispatches one subcommand. It is the single dispatch point for
// both the process entry point and the `hook` sequence, so a step the hook
// runs is byte-for-byte the same command an operator can reproduce by hand.
//
// A non-nil error means the command failed — either because it could not run
// (bad manifest, unreachable cluster) or because the check it ran did not
// pass. Both map to exit status 1; the check-specific detail has already been
// printed by then.
func runCommand(name string, args []string) error {
	switch name {
	case "run":
		return cmdRun(args)
	case "validate":
		return cmdValidate(args)
	case "expect-skeleton":
		return cmdExpectSkeleton(args)
	case "converge":
		return cmdConverge(args)
	case "converge-all":
		return cmdConvergeAll(args)
	case "batch":
		return cmdBatch(args)
	case "check-external-name-prefix":
		return cmdCheckExternalNamePrefix(args)
	case "resolve-recover":
		return cmdResolveRecover(args)
	case "roundtrip-diff":
		return cmdRoundtripDiff(args)
	case "roundtrip-verify":
		return cmdRoundtripVerify(args)
	case "residual":
		return cmdResidual(args)
	case "hook":
		return cmdHook(args)
	case "version":
		return cmdVersion(os.Stdout)
	default:
		return fmt.Errorf("%w: %s", errUnknownCommand, name)
	}
}

// usageSynopsis lists every subcommand's invocation line, one per line, with
// no leading indentation. printUsage indents and prints it below; the
// package doc comment above main's declaration, and README.md's "## Commands"
// fence, each carry a plain-text mirror of the same lines — a Go doc comment
// cannot reference a constant, so those two stay in sync only because
// TestUsageSynopsisSourcesAgree (main_test.go) fails the moment any of the
// three drifts from this one, the next time a subcommand or flag changes.
const usageSynopsis = `update-tester run <manifest.yaml> [--timeout 120] [--poll-interval 60s]
update-tester converge <manifest.yaml> [--poll-interval 60s] [--ignore-fields a,b] [--timeout 120s] [--readiness-timeout 120s]
update-tester converge-all <m1.yaml,m2.yaml,...> [--poll-interval 60s] [--concurrency 8] [--timeout 120s] [--readiness-timeout 120s]
update-tester batch <m1.yaml,m2.yaml,...> [--parallel 1] [--timeout 120] [--poll-interval 60s]
update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]
update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
update-tester resolve-recover <manifest.yaml> [--timeout 120]
update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
update-tester roundtrip-verify <m1.yaml,m2.yaml,...> --backend <real|simulator> [--root <dir>] [--timeout 30]
update-tester residual <examples-dir>
update-tester hook <invocation-name> [--root <dir>] [--manifest <path>] [--skip-converge]
update-tester version`

// indentLines prefixes every line of s with prefix. usageSynopsis is kept
// unindented so it is directly comparable against the other two synopsis
// surfaces in a test; printUsage re-indents it here for display.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `update-tester — Crossplane per-field update E2E tester

Usage:
`+indentLines(usageSynopsis, "  ")+`

Flags may appear before or after the manifest path.

Commands:
  run        Execute update tests against a live cluster
  validate   Check annotation coverage against Go type definitions
  expect-skeleton
             Print the non-omitempty Observation-struct keys a NEW
             expect: block for one field would need to name — keys
             only, a dev aid, not a check
  converge   Assert the resource reaches steady state after creation
             with zero spurious Update calls
  converge-all
             The same assertion for many resources against ONE shared
             observation window instead of one window each. Convergence
             performs reads only, so the windows can be shared; the
             result is both faster and strictly stronger, since every
             resource is observed over the same stretch of wall clock.
  batch      Run per-field update tests (the same check the run command
             performs) for many manifests CONCURRENTLY in this one
             process, sharing one client set and one backend rate-limit
             signal across every worker. --parallel unset or 1 (the
             default) is strictly serial and behaves identically to
             invoking run once per manifest — concurrency is opt-in per
             invocation. Field tests within one manifest always run one
             at a time regardless of --parallel; only different manifests
             ever run at once. On sustained rate-limiting from the
             backend, the active worker ceiling is reduced automatically
             and the reduction is reported, never silent.
  check-external-name-prefix
             Assert the live resource's crossplane.io/external-name
             annotation has the prefix declared by the manifest's
             crossplane.io/expect-external-name-prefix annotation. For
             resources whose backend models more than one object type
             behind a single kind, where a wrong identity search
             silently resolves against the other object type.
  resolve-recover
             Pause reconciliation, strip the crossplane.io/external-name
             annotation, unpause, and assert the controller recovers to
             the SAME backend object (exactly one CreatedExternalResource
             event across the resource's lifecycle) rather than silently
             creating a duplicate. Exercises the ref-less identity-search
             path a standing ref-addressed lifecycle never reaches.
  roundtrip-diff
             Print an advisory report of which spec.forProvider fields
             round-trip faithfully into status.atProvider for one or more
             already-live manifests: what the backend defaulted, dropped,
             or changed. Read-only, and never fails on what it finds —
             converge-all inlines the same report next to a FAILING
             target's own verdict when UPDATE_TESTER_ROOT is set.
  roundtrip-verify
             The enforcing counterpart to roundtrip-diff: derives the
             must-test denominator from the SAME live classification and
             fails when a must-test field's skip: waiver does not hold up
             against its own live row. Emits one JSON report per manifest
             on every invocation, pass or fail. Additionally reports
             cell-denominator crediting, container-clear coverage (always
             advisory — never affects the exit code) and a waiver-bucket
             classification; --backend declares real or simulator
             provenance for those additive fields and is REQUIRED — there
             is no default and no inference from a provider name or
             endpoint.
  residual   Walk a whole examples/ tree and report the repo-scope
             cell-denominator residual: every disposition-carrying skip:
             entry, parsed through the tool's own manifest parser. Prints
             fixtures_with_annotation and parsed_ok as a pair and exits
             non-zero when they differ — offline and read-only, no
             cluster touched
  hook       Derive a manifest from the name this binary was invoked
             under and run the full post-assert sequence for it
  version    Print the version of the tool in use
  help       Print this message`)
}

// ─── run ──────────────────────────────────────────────────────────────────

// runOptions holds the parsed command line of the `run` subcommand.
type runOptions struct {
	manifestPath string
	timeout      int
	// pollInterval is the provider's poll interval. `run` does not wait on
	// that cadence for anything — it only calibrates the slow-observe
	// annotation (see runner.Runner's slowObserveThreshold). What a field
	// test waits for is --timeout.
	pollInterval time.Duration
}

// parseRunArgs parses the `run` command line. Like every parse* function
// here it reorders its arguments first (see cli.ReorderArgs) so that a flag
// written after the manifest path still takes effect instead of being
// silently discarded by flag.FlagSet.Parse.
func parseRunArgs(args []string) (runOptions, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	timeout := fs.Int("timeout", 120, "Timeout in seconds for kubectl wait")
	pollInterval := fs.Duration("poll-interval", 60*time.Second,
		"Provider poll interval; calibrates the slow-observe annotation. Does NOT change what run waits for — that is --timeout")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return runOptions{}, err
	}
	if fs.NArg() < 1 {
		return runOptions{}, errors.New("usage: update-tester run <manifest.yaml> [--timeout 120] [--poll-interval 60s]")
	}
	return runOptions{manifestPath: fs.Arg(0), timeout: *timeout, pollInterval: *pollInterval}, nil
}

func cmdRun(args []string) error {
	opts, err := parseRunArgs(args)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(opts.manifestPath)
	if err != nil {
		return err
	}
	if len(m.Tests) == 0 {
		return fmt.Errorf("no %s annotation found in manifest", manifest.AnnotationKey)
	}

	// Count tested vs skipped.
	var skipped int
	for _, t := range m.Tests {
		if t.Skip.Present() {
			skipped++
		}
	}

	fmt.Printf("Testing %s/%s (%d fields, %d skipped)\n",
		m.Kind, m.Name, len(m.Tests), skipped)

	results, unchangedViolations, err := runner.NewRunner(opts.manifestPath, opts.timeout).
		WithPollInterval(opts.pollInterval).
		RunTests(m)
	if err != nil {
		return err
	}

	passed, failed, noop, notEvidenced, untrusted := printResults(os.Stdout, results)
	assertUnchangedFailed := printUnchangedAssertions(os.Stdout, m.AssertUnchanged, unchangedViolations)

	total := passed + failed
	fmt.Printf("%s: %d/%d tested, %d/%d skipped, %d no-op, %d not-evidenced, %d untrusted\n",
		verdict(failed == 0 && !assertUnchangedFailed), passed, total, skipped, len(m.Tests), noop, notEvidenced, untrusted)

	if failed > 0 && assertUnchangedFailed {
		return fmt.Errorf("%d of %d field tests failed, and %d assert-unchanged field(s) drifted", failed, total, len(unchangedViolations))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d field tests failed", failed, total)
	}
	if assertUnchangedFailed {
		return fmt.Errorf("%d assert-unchanged field(s) drifted during the run", len(unchangedViolations))
	}
	return nil
}

// printUnchangedAssertions prints the outcome of every manifest-declared
// assert-unchanged field (see manifest.Manifest.AssertUnchanged), whether it
// held for the whole run or drifted, and reports whether any drifted. A
// manifest that declares none prints nothing and reports no failure — this
// is a strict opt-in on top of ordinary per-field update-test behaviour.
//
// A passing field is printed alongside a failing one, not just a failing
// one, so a reviewer scanning a green E2E log can still see the guard ran —
// the same reason a PASS line exists in printResults above.
func printUnchangedAssertions(w io.Writer, fields []string, violations []runner.UnchangedAssertion) (anyFailed bool) {
	if len(fields) == 0 {
		return false
	}

	byField := make(map[string]runner.UnchangedAssertion, len(violations))
	for _, v := range violations {
		byField[v.Field] = v
	}

	printfTo(w, "Unchanged-field assertions:\n")
	for _, f := range fields {
		if v, ok := byField[f]; ok {
			anyFailed = true
			printfTo(w, "  \u2717 %s: WIPED after patching %q (was %q, now %q)\n", f, v.AfterField, v.Baseline, v.Observed)
			continue
		}
		printfTo(w, "  \u2713 %s: unchanged across run\n", f)
	}
	printfTo(w, "\n")
	return anyFailed
}

// printResults prints one line per test result (plus any side effects) and
// returns the passed/failed counts, plus the no-op, not-evidenced, and
// untrusted counts (reported separately so each distinct verdict is easy to
// spot in the summary line without being confused with a genuine PASS or
// SKIP). A PASS whose field converged at or above the runner's slow-observe
// threshold is annotated "slow-observe" inline — it is still a PASS backed by
// positive update-event evidence, not a reason for a reviewer to suspect the
// result. An UNTRUSTED result is reported and counted as a failure
// regardless of the field's own Passed/NotEvidenced value: it ran after a
// burst-reset failure earlier in the same run, so neither outcome can be
// trusted to prove or disprove that Update() ran — the run's summary line
// must not be able to read a clean "0 not-evidenced" in that case.
func printResults(w io.Writer, results []runner.TestResult) (passed, failed, noop, notEvidenced, untrusted int) {
	var hasSideFx bool
	for _, r := range results {
		switch {
		case r.Skipped:
			printfTo(w, "  ⊘ %s: SKIPPED (%s)\n", r.Field, r.SkipMsg)
			continue
		case r.NoOp:
			printfTo(w, "  ⦸ %s: NO-OP (%v) (%s)\n", r.Field, r.Error, fmtDuration(r.Duration))
			failed++
			noop++
		case r.EvidenceUntrusted:
			printfTo(w, "  ‽ %s: UNTRUSTED (evidence check unreliable — an earlier event-burst reset failed mid-run) (%s)\n",
				r.Field, fmtDuration(r.Duration))
			failed++
			untrusted++
		case r.NotEvidenced:
			printfTo(w, "  ⚡ %s: NOT-EVIDENCED (%v) (%s)\n", r.Field, r.Error, fmtDuration(r.Duration))
			failed++
			notEvidenced++
		case r.Error != nil:
			printfTo(w, "  ✗ %s: ERROR (%v) (%s)\n", r.Field, r.Error, fmtDuration(r.Duration))
			failed++
		case r.Passed && r.SlowObserve:
			printfTo(w, "  ✓ %s: %s (%s, slow-observe)\n",
				r.Field, passTransition(r), fmtDuration(r.Duration))
			passed++
		case r.Passed:
			printfTo(w, "  ✓ %s: %s (%s)\n",
				r.Field, passTransition(r), fmtDuration(r.Duration))
			passed++
		default:
			printfTo(w, "  ✗ %s: expected %q, got %q (%s)\n",
				r.Field, r.Expected, r.Actual, fmtDuration(r.Duration))
			failed++
		}
		if len(r.SideFx) > 0 {
			hasSideFx = true
			printSideEffects(w, r.SideFx)
		}
	}

	printfTo(w, "\n")
	if !hasSideFx {
		printfTo(w, "  Differential: all non-target fields stable ✓\n")
		printfTo(w, "\n")
	}
	return passed, failed, noop, notEvidenced, untrusted
}

// passTransition formats the value pairing shown on a PASS line. It prefers
// the pre-patch value captured immediately before the patch (r.Before), so
// the line reads as a genuine before → after transition. Expected and Actual
// are both the post-update target on a PASS, so pairing them instead — as
// this used to do — prints the same value on both sides of the arrow and
// reads as the no-op the update-test exists to catch, even though the no-op
// guard already ran and the field genuinely changed. r.Before is only
// unset when the pre-patch read itself failed, in which case the pairing
// falls back to explicit expected/observed labels instead of a bare arrow,
// so the line is never printed in a shape that COULD be misread as a
// before → after transition when it is not one.
func passTransition(r runner.TestResult) string {
	if r.BeforeKnown {
		return fmt.Sprintf("%q → %q", r.Before, r.Actual)
	}
	return fmt.Sprintf("expected %q, observed %q", r.Expected, r.Actual)
}

// printSideEffects prints the fields that changed unexpectedly alongside a
// target field update.
func printSideEffects(w io.Writer, changes []differ.FieldChange) {
	printfTo(w, "    ⚠ side effects: ")
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, fmt.Sprintf("%s: %s → %s", c.Field, c.OldValue, c.NewValue))
	}
	printfTo(w, "%s\n", strings.Join(parts, ", "))
}

// ─── validate ─────────────────────────────────────────────────────────────

// validateOptions holds the parsed command line of the `validate` subcommand.
type validateOptions struct {
	manifestPath  string
	typesFile     string
	controllerDir string
	// root is the provider repo root holding apis/cluster and
	// apis/namespaced. It is only consulted when typesFile is empty — see
	// cmdValidate, which resolves the types file by identity under
	// root/apis/<scope> in that case.
	root string
}

func parseValidateArgs(args []string) (validateOptions, error) {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	typesFile := fs.String("types-file", "",
		"Path to Go types file containing Parameters struct (optional — resolved by identity under "+
			"--root/apis/<scope> when omitted)")
	controllerDir := fs.String("controller-dir", "",
		"Path to the resource's controller package directory (optional). When set, also flags an expect:/value: "+
			"object that omits a member the controller declares server-echoed via a registered go-cmp Transformer "+
			"normalizer, even when that member carries omitempty")
	root := fs.String("root", "",
		"Provider repo root holding apis/cluster and apis/namespaced, used to resolve --types-file by identity "+
			"when it is omitted (default: working directory)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return validateOptions{}, err
	}
	if fs.NArg() < 1 {
		return validateOptions{}, errors.New(
			"usage: update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]")
	}

	resolvedRoot := *root
	if resolvedRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return validateOptions{}, fmt.Errorf("determining working directory for --root: %w", err)
		}
		resolvedRoot = wd
	}

	return validateOptions{manifestPath: fs.Arg(0), typesFile: *typesFile, controllerDir: *controllerDir, root: resolvedRoot}, nil
}

func cmdValidate(args []string) error {
	opts, err := parseValidateArgs(args)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(opts.manifestPath)
	if err != nil {
		return err
	}

	typesFile := opts.typesFile
	if typesFile == "" {
		typesFile, err = validator.FindTypesFile(opts.root, m.APIVersion, m.Kind)
		if err != nil {
			return fmt.Errorf("resolving types file: %w", err)
		}
	}

	fields, err := validator.ParseGoTypes(typesFile, m.Kind)
	if err != nil {
		return err
	}

	result := validator.ValidateManifest(m, fields)
	validator.PrintValidation(result)

	findings := validator.CheckObservability(typesFile, m.Kind, fields, m.Tests)
	validator.PrintObservability(findings)

	siblingFindings := validator.CheckMergePatchSiblings(m)
	validator.PrintMergePatchSiblings(siblingFindings)

	incompleteFindings := validator.CheckIncompleteExpectations(typesFile, fields, m)
	validator.PrintIncompleteExpectations(incompleteFindings)

	echoFindings, err := validator.CheckServerEchoedExpectations(typesFile, fields, m, opts.controllerDir)
	if err != nil {
		return fmt.Errorf("checking server-echoed expectations: %w", err)
	}
	validator.PrintServerEchoedExpectations(echoFindings)

	noOpFindings := validator.CheckGuaranteedNoOp(m)
	validator.PrintGuaranteedNoOp(noOpFindings)

	skipReasonFindings := validator.CheckSkipReasons(m, fields)
	validator.PrintSkipReasons(skipReasonFindings)

	printContainerClearCells(opts.root, opts.manifestPath, m)

	if !result.AllGood {
		return errors.New("mutable-field coverage is incomplete")
	}
	if len(findings) > 0 {
		return errors.New("update-test expectation is structurally unobservable in atProvider")
	}
	if len(siblingFindings) > 0 {
		return errors.New("update-test entry leaves a sibling key surviving an RFC 7386 merge patch unaddressed")
	}
	if len(incompleteFindings) > 0 {
		return errors.New("update-test expectation omits a non-omitempty Observation struct member")
	}
	if len(echoFindings) > 0 {
		return errors.New("update-test expectation omits a member the provider's controller declares server-echoed")
	}
	if len(noOpFindings) > 0 {
		return errors.New("update-test entry repeats the field's own create-time value and can never exercise the update path")
	}
	if len(skipReasonFindings) > 0 {
		return errors.New("skip: reason does not resolve against this manifest's own declared coverage")
	}
	return nil
}

// printContainerClearCells is `validate`'s wiring for the container-clear
// cell gate (roundtrip.ContainerClearCoverage, grouped into cells by
// roundtrip.BuildClearCellReport) — the first place this check reaches any
// provider's own Makefile target, since roundtrip-verify's copy of the
// same computation sits behind `roundtrip-verify`, a subcommand no
// provider invokes.
//
// REPORT-ONLY: the return value is deliberately discarded by cmdValidate
// and nothing here is wired into result.AllGood or any of cmdValidate's
// other exit-code decisions — flipping this from advisory to enforcing is
// a distinct, later, conversation-reserved act (see roundtrip.
// ContainerClearFinding's own doc comment for why). A CRD that cannot be
// found is silently skipped, exactly as roundtrip-verify already treats
// that case: `validate` has always run without a CRD present (its other
// checks resolve against the Go types file, never the CRD), so a provider
// mid-generation with no CRD yet must not start failing a check that was
// never gating before.
//
// root ALONE is not a reliable place to find package/crds: six of the
// fleet's seven update-test.validate Makefile recipes invoke this command
// as `--types-file \"$$PWD/$$types\" \"$$PWD/$$f\"`, never passing --root at
// all, and `UPDATE_TESTER := go -C tools/update-tester tool ...` changes
// the TOOL PROCESS'S OWN working directory to tools/update-tester before it
// ever runs — so parseValidateArgs' os.Getwd() fallback silently resolves
// root to the wrong directory on every one of those six, and
// roundtrip.FindCRD finds nothing. manifestPath is unaffected by that: the
// shell builds "$$PWD/$$f" into an absolute path BEFORE go -C ever runs, so
// inferProviderRootFromManifest walks up from ITS OWN directory instead,
// which reaches the provider root reliably regardless of what the tool
// process's own cwd became.
func printContainerClearCells(root, manifestPath string, m *manifest.Manifest) {
	crd, _ := roundtrip.FindCRD(root, m.APIVersion, m.Kind)
	if crd == nil {
		if inferredRoot, ok := inferProviderRootFromManifest(manifestPath); ok {
			crd, _ = roundtrip.FindCRD(inferredRoot, m.APIVersion, m.Kind)
		}
	}
	if crd == nil {
		return
	}
	findings, err := roundtrip.ContainerClearCoverage(crd, m)
	if err != nil {
		printfTo(os.Stdout, "container-clear: ERROR — %s\n", err)
		return
	}
	roundtrip.PrintClearCellReport(func(format string, args ...interface{}) {
		printfTo(os.Stdout, format, args...)
	}, roundtrip.BuildClearCellReport(findings))
}

// inferProviderRootFromManifest walks upward from manifestPath's own
// absolute directory looking for a "package/crds" directory, returning the
// first one found. See printContainerClearCells' own doc comment for why
// this fallback exists — root's os.Getwd() default cannot be trusted under
// `go -C tools/update-tester tool ...`, but manifestPath's own absolute
// path was already resolved by the invoking shell before that happened.
func inferProviderRootFromManifest(manifestPath string) (string, bool) {
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(abs)
	for {
		if info, err := os.Stat(filepath.Join(dir, "package", "crds")); err == nil && info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ─── expect-skeleton ────────────────────────────────────────────────────────

// expectSkeletonOptions holds the parsed command line of the
// `expect-skeleton` subcommand.
type expectSkeletonOptions struct {
	typesFile string
	kind      string
	field     string
}

func parseExpectSkeletonArgs(args []string) (expectSkeletonOptions, error) {
	fs := flag.NewFlagSet("expect-skeleton", flag.ContinueOnError)
	kind := fs.String("kind", "", "The resource Kind whose {Kind}Parameters struct declares field")
	field := fs.String("field", "", "The top-level JSON field name to emit an expect: skeleton for")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return expectSkeletonOptions{}, err
	}
	if fs.NArg() < 1 {
		return expectSkeletonOptions{}, errors.New("usage: update-tester expect-skeleton <types.go> --kind <Kind> --field <field>")
	}
	if *kind == "" {
		return expectSkeletonOptions{}, errors.New("--kind is required")
	}
	if *field == "" {
		return expectSkeletonOptions{}, errors.New("--field is required")
	}
	return expectSkeletonOptions{typesFile: fs.Arg(0), kind: *kind, field: *field}, nil
}

// cmdExpectSkeleton is a dev aid: given a Kind and a top-level field name, it
// prints the exact set of non-omitempty Observation-struct keys a NEW
// update-test expect: block for that field would need to name — keys only,
// each set to a clearly-labelled placeholder, never a guessed value. It
// exists for the class of nested-composite field where hand-deriving that
// key set means reading the generated Observation struct by hand; nothing
// else in the tool consumes its output, and no provider is required to use
// it. See cli.BuildExpectSkeleton for the resolution this reuses.
func cmdExpectSkeleton(args []string) error {
	opts, err := parseExpectSkeletonArgs(args)
	if err != nil {
		return err
	}

	keys, err := cli.BuildExpectSkeleton(opts.typesFile, opts.kind, opts.field)
	if err != nil {
		return err
	}

	cli.PrintExpectSkeleton(os.Stdout, opts.field, keys)
	return nil
}

// ─── converge ─────────────────────────────────────────────────────────────

// convergeOptions holds the parsed command line of the `converge` subcommand.
type convergeOptions struct {
	manifestPath     string
	pollInterval     time.Duration
	ignoreFields     []string
	timeout          time.Duration
	readinessTimeout time.Duration
}

func parseConvergeArgs(args []string) (convergeOptions, error) {
	fs := flag.NewFlagSet("converge", flag.ContinueOnError)
	pollInterval := fs.Duration("poll-interval", 60*time.Second, "Provider poll interval; determines wait duration")
	ignoreFields := fs.String("ignore-fields", "", "Comma-separated atProvider fields excluded from the snapshot diff (unioned with the manifest's own ignore-fields: directive)")
	timeout := fs.Duration("timeout", 120*time.Second, "Max time for the pre-check to settle")
	readinessTimeout := fs.Duration("readiness-timeout", 120*time.Second,
		"Max time to wait for the Ready condition before the baseline snapshot; on timeout the check proceeds anyway")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return convergeOptions{}, err
	}
	if fs.NArg() < 1 {
		return convergeOptions{}, errors.New(
			"usage: update-tester converge <manifest.yaml> [--poll-interval 60s] [--ignore-fields a,b] [--timeout 120s] [--readiness-timeout 120s]")
	}

	var ignore []string
	if *ignoreFields != "" {
		ignore = strings.Split(*ignoreFields, ",")
	}
	if err := manifest.ValidateIgnoreFields(ignore); err != nil {
		return convergeOptions{}, err
	}
	return convergeOptions{
		manifestPath:     fs.Arg(0),
		pollInterval:     *pollInterval,
		ignoreFields:     ignore,
		timeout:          *timeout,
		readinessTimeout: *readinessTimeout,
	}, nil
}

// convergeOptionsFor builds the runner.ConvergeOptions for a single-resource
// `converge` invocation, sourcing IgnoreFields from
// mergeIgnoreFields(opts.ignoreFields, m.IgnoreFields) so the manifest's own
// "ignore-fields:" directive is honoured on the hook path (hookSteps runs
// `converge`, never `converge-all`, so before this the directive was read by
// no code any provider's E2E run actually executes). opts.ignoreFields keeps
// working exactly as before — the CLI flag (and, via hookStepArgs, the
// UPDATE_TESTER_IGNORE_FIELDS environment variable) is still honoured, now
// unioned with the manifest's own set instead of being the only source.
// Split out from cmdConverge so it is testable without a live cluster —
// nothing here executes a Runner, it only configures one.
func convergeOptionsFor(opts convergeOptions, m *manifest.Manifest) runner.ConvergeOptions {
	return runner.ConvergeOptions{
		PollInterval:     opts.pollInterval,
		IgnoreFields:     mergeIgnoreFields(opts.ignoreFields, m.IgnoreFields),
		Timeout:          opts.timeout,
		ReadinessTimeout: opts.readinessTimeout,
	}
}

func cmdConverge(args []string) error {
	opts, err := parseConvergeArgs(args)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(opts.manifestPath)
	if err != nil {
		return err
	}

	r := runner.NewRunner(opts.manifestPath, int(opts.timeout.Seconds()))
	result, err := r.RunConverge(m, convergeOptionsFor(opts, m))
	if err != nil {
		return err
	}

	printConvergeResult(os.Stdout, m, result)

	// A skipped convergence check is a deliberate manifest-level opt-out
	// (converge-skip), not a failure: the resource is known to change an
	// atProvider field on every observe cycle, so the check cannot mean
	// anything for it.
	if result.Skipped {
		return nil
	}
	if !result.Passed {
		return errors.New("resource did not converge")
	}
	return nil
}

// printConvergeResult prints the outcome of a convergence check. Diagnostics
// are printed whenever present, whether or not the check passed — a
// readiness pre-check note is informational rather than a failure reason,
// but an operator reading a passing result should still see it.
func printConvergeResult(w io.Writer, m *manifest.Manifest, r *runner.ConvergeResult) {
	printfTo(w, "Converge check: %s/%s\n", m.Kind, m.Name)
	switch {
	case r.Skipped:
		printfTo(w, "  ⊘ CONVERGE-SKIP: %s\n", r.SkipMsg)
		return
	case r.Passed:
		printfTo(w, "  ✓ converge: %s\n", r.Message)
	default:
		printfTo(w, "  ✗ converge: %s\n", r.Message)
	}
	for _, d := range r.Diagnostics {
		printfTo(w, "    - %s\n", d)
	}
}

// ─── converge-all ─────────────────────────────────────────────────────────

// convergeAllOptions holds the parsed command line of `converge-all`.
//
// --ignore-fields here is a FLEET-WIDE DEFAULT, not the only mechanism: each
// target's real exclusion set is its OWN manifest's "ignore-fields:"
// annotation directive (manifest.Manifest.IgnoreFields), and the flag's set
// is unioned onto every target on top of that — see buildConvergeTargets.
// Using the flag alone across a fleet with divergent per-resource exclusions
// (a Loadbalancer's forwardRules is meaningless to a Network) would silently
// widen every resource's exclusion set to the union of all of them, the same
// fleet-wide-blindness failure documented for converge-skip. The flag stays
// useful for a set every resource genuinely shares (f5xc's uniform "status");
// ticket dcbdabdb is where the per-resource mechanism was added.
type convergeAllOptions struct {
	manifestPaths    []string
	pollInterval     time.Duration
	timeout          time.Duration
	readinessTimeout time.Duration
	concurrency      int
	ignoreFields     []string
}

func parseConvergeAllArgs(args []string) (convergeAllOptions, error) {
	fs := flag.NewFlagSet("converge-all", flag.ContinueOnError)
	pollInterval := fs.Duration("poll-interval", 60*time.Second, "Provider poll interval; determines the shared window duration")
	timeout := fs.Duration("timeout", 120*time.Second, "Max time for each pre-check to settle")
	readinessTimeout := fs.Duration("readiness-timeout", 120*time.Second,
		"Max time to wait for the Ready condition before each baseline snapshot")
	concurrency := fs.Int("concurrency", 8, "Max resources armed or asserted at once")
	// A FLEET-WIDE DEFAULT, unioned onto each target's own per-manifest
	// "ignore-fields:" directive rather than replacing it — see
	// convergeAllOptions and buildConvergeTargets. Lossless alone only where
	// every resource in the case shares one set (f5xc: uniformly "status");
	// a divergent fleet (vultr: latestBackup / ruleCount,dateModified /
	// kvm,powerStatus,serverStatus) needs the per-manifest directive too.
	ignoreFields := fs.String("ignore-fields", "", "Comma-separated atProvider fields excluded from the diff for every target (unioned with each manifest's own ignore-fields: directive)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return convergeAllOptions{}, err
	}
	if fs.NArg() < 1 {
		return convergeAllOptions{}, errors.New(
			"usage: update-tester converge-all <m1.yaml,m2.yaml,...> [--poll-interval 60s] [--concurrency 8]")
	}

	// Accept both a comma-separated list (matching the uptest CLI's
	// UPTEST_INPUT_MANIFESTS convention the providers already use) and
	// repeated positional arguments.
	var paths []string
	for _, arg := range fs.Args() {
		for _, p := range strings.Split(arg, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	var ignore []string
	for _, f := range strings.Split(*ignoreFields, ",") {
		if f = strings.TrimSpace(f); f != "" {
			ignore = append(ignore, f)
		}
	}
	if err := manifest.ValidateIgnoreFields(ignore); err != nil {
		return convergeAllOptions{}, err
	}
	return convergeAllOptions{
		manifestPaths:    paths,
		pollInterval:     *pollInterval,
		timeout:          *timeout,
		readinessTimeout: *readinessTimeout,
		concurrency:      *concurrency,
		ignoreFields:     ignore,
	}, nil
}

// buildConvergeTargets parses each manifest path and constructs its
// ConvergeTarget, sourcing IgnoreFields per-resource: the manifest's own
// "ignore-fields:" annotation directive, plus the fleet-wide --ignore-fields
// flag unioned on top (see convergeAllOptions). Split out from cmdConvergeAll
// so the per-resource sourcing can be tested without a live cluster — nothing
// here executes a Runner, it only configures one.
func buildConvergeTargets(opts convergeAllOptions) ([]runner.ConvergeTarget, error) {
	targets := make([]runner.ConvergeTarget, 0, len(opts.manifestPaths))
	for _, p := range opts.manifestPaths {
		m, err := manifest.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		targets = append(targets, runner.ConvergeTarget{
			Label:    fmt.Sprintf("%s/%s", m.Kind, m.Name),
			Runner:   runner.NewRunner(p, int(opts.timeout.Seconds())),
			Manifest: m,
			Opts: runner.ConvergeOptions{
				PollInterval:     opts.pollInterval,
				Timeout:          opts.timeout,
				ReadinessTimeout: opts.readinessTimeout,
				IgnoreFields:     mergeIgnoreFields(opts.ignoreFields, m.IgnoreFields),
			},
		})
	}
	return targets, nil
}

// mergeIgnoreFields unions the fleet-wide default with a single target's own
// per-manifest exclusion list, deduplicating and keeping a stable order
// (fleet-wide entries first, in flag order; then the manifest's own entries,
// in annotation order) so command output and tests are deterministic.
func mergeIgnoreFields(fleetWide, perResource []string) []string {
	if len(fleetWide) == 0 {
		return perResource
	}
	if len(perResource) == 0 {
		return fleetWide
	}
	seen := make(map[string]bool, len(fleetWide)+len(perResource))
	merged := make([]string, 0, len(fleetWide)+len(perResource))
	for _, f := range fleetWide {
		if !seen[f] {
			seen[f] = true
			merged = append(merged, f)
		}
	}
	for _, f := range perResource {
		if !seen[f] {
			seen[f] = true
			merged = append(merged, f)
		}
	}
	return merged
}

func cmdConvergeAll(args []string) error {
	opts, err := parseConvergeAllArgs(args)
	if err != nil {
		return err
	}

	targets, err := buildConvergeTargets(opts)
	if err != nil {
		return err
	}

	printfTo(os.Stdout, "Converge barrier: %d resource(s), one shared %s window\n",
		len(targets), time.Duration(float64(opts.pollInterval)*1.5))

	start := time.Now()
	results := runner.RunConvergeAll(targets, opts.concurrency)
	elapsed := time.Since(start)

	// UPDATE_TESTER_ROOT is read here, not as a --root flag, deliberately:
	// this keeps converge-all's own flag surface — and therefore its
	// --help output — byte-identical whether or not a caller ever sets
	// it. Unset (the default for every existing caller), no CRD lookup is
	// attempted and the summary is exactly what FormatConvergeAllSummary
	// alone would have produced. Set, a FAILING target's advisory
	// spec.forProvider <-> status.atProvider round-trip report — see
	// package roundtrip — is inlined immediately under that target's own
	// verdict line, so the finding is visible at the moment the run
	// actually failed instead of buried in a separate trailing report a
	// reviewer has to go looking for.
	findings := runner.RoundtripFindingsForFailures(targets, results, os.Getenv(envRoundtripRoot))

	summary, ok := runner.FormatConvergeAllSummaryWithFindings(results, findings)
	printfTo(os.Stdout, "%s", summary)
	printfTo(os.Stdout, "barrier wall clock: %s\n", elapsed.Round(time.Millisecond))

	if !ok {
		return errors.New("one or more resources did not converge")
	}
	return nil
}

// envRoundtripRoot names the provider repository root — the directory
// holding package/crds/ — that converge-all uses to find each failing
// target's CRD for the advisory round-trip report above. An environment
// variable rather than a flag so converge-all's own --help output never
// changes based on whether a caller sets it.
const envRoundtripRoot = "UPDATE_TESTER_ROOT"

// ─── batch ────────────────────────────────────────────────────────────────

// batchOptions holds the parsed command line of the `batch` subcommand.
type batchOptions struct {
	manifestPaths []string
	timeout       int
	pollInterval  time.Duration
	// parallel is the initial worker ceiling. defaultBatchCLIParallel (1)
	// when the flag is never set — see that constant's doc comment for
	// why 1, not converge-all's 8, is batch's own default.
	parallel int
}

// defaultBatchCLIParallel is --parallel's default. It is 1, not
// converge-all's default concurrency of 8: batch mode ships dormant, so an
// invocation that never sets --parallel must run every fixture strictly one
// at a time, identically to running `run` once per manifest today. A
// provider opts into concurrency by passing --parallel explicitly, one
// provider at a time, each with its own measurement.
const defaultBatchCLIParallel = 1

func parseBatchArgs(args []string) (batchOptions, error) {
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	timeout := fs.Int("timeout", 120, "Timeout in seconds for kubectl wait, applied to every fixture")
	pollInterval := fs.Duration("poll-interval", 60*time.Second,
		"Provider poll interval; calibrates the slow-observe annotation for every fixture")
	parallel := fs.Int("parallel", defaultBatchCLIParallel,
		"Max fixtures run concurrently in this one process. 1 (the default) is strictly serial")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return batchOptions{}, err
	}
	if fs.NArg() < 1 {
		return batchOptions{}, errors.New(
			"usage: update-tester batch <m1.yaml,m2.yaml,...> [--parallel 1] [--timeout 120] [--poll-interval 60s]")
	}
	if *parallel < 1 {
		return batchOptions{}, fmt.Errorf("--parallel must be >= 1, got %d", *parallel)
	}

	// Accept both a comma-separated list and repeated positional
	// arguments, exactly like converge-all's own manifest-list parsing.
	var paths []string
	for _, arg := range fs.Args() {
		for _, p := range strings.Split(arg, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		return batchOptions{}, errors.New("batch: no manifest paths given")
	}

	return batchOptions{manifestPaths: paths, timeout: *timeout, pollInterval: *pollInterval, parallel: *parallel}, nil
}

// buildBatchTargets parses every manifest path into a runner.BatchTarget,
// failing fast — before any client is built or any fixture runs — on a
// manifest with no update-test annotation, exactly like cmdRun's own
// single-manifest check.
func buildBatchTargets(opts batchOptions) ([]runner.BatchTarget, error) {
	targets := make([]runner.BatchTarget, 0, len(opts.manifestPaths))
	for _, p := range opts.manifestPaths {
		m, err := manifest.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(m.Tests) == 0 {
			return nil, fmt.Errorf("%s: no %s annotation found in manifest", p, manifest.AnnotationKey)
		}
		targets = append(targets, runner.BatchTarget{
			Label:    fmt.Sprintf("%s/%s", m.Kind, m.Name),
			Runner:   runner.NewRunner(p, opts.timeout).WithPollInterval(opts.pollInterval),
			Manifest: m,
		})
	}
	return targets, nil
}

// cmdBatch runs `run`'s own per-field update-test check for every named
// manifest, CONCURRENTLY in this one process, bounded by --parallel and a
// single shared client-go client set (runner.NewSharedClients) — see
// runner.RunBatch's own doc comment for the full design: cross-fixture
// parallel, intra-fixture serial, one shared client set and rate-limit
// signal regardless of --parallel, and index-attributed (never
// completion-ordered) output.
func cmdBatch(args []string) error {
	opts, err := parseBatchArgs(args)
	if err != nil {
		return err
	}

	targets, err := buildBatchTargets(opts)
	if err != nil {
		return err
	}

	clients, err := runner.NewSharedClients()
	if err != nil {
		return fmt.Errorf("building shared client set: %w", err)
	}
	for _, t := range targets {
		clients.Apply(t.Runner)
	}

	fmt.Printf("Batch: %d fixture(s), parallel=%d\n", len(targets), opts.parallel)
	summary := runner.RunBatch(targets, clients, runner.BatchOptions{Parallel: opts.parallel})

	var failedFixtures int
	for _, res := range summary.Results {
		fmt.Printf("\n=== %s ===\n", res.Label)
		if res.Err != nil {
			fmt.Printf("  \u2717 ERROR: %v\n", res.Err)
			failedFixtures++
			continue
		}

		var buf bytes.Buffer
		passed, failed, noop, notEvidenced, untrusted := printResults(&buf, res.Results)
		var assertUnchangedFields []string
		if res.Manifest != nil {
			assertUnchangedFields = res.Manifest.AssertUnchanged
		}
		assertUnchangedFailed := printUnchangedAssertions(&buf, assertUnchangedFields, res.UnchangedViolations)
		printfTo(os.Stdout, "%s", buf.String())

		total := passed + failed
		fmt.Printf("%s: %d/%d tested, %d no-op, %d not-evidenced, %d untrusted\n",
			verdict(failed == 0 && !assertUnchangedFailed), passed, total, noop, notEvidenced, untrusted)
		if failed > 0 || assertUnchangedFailed {
			failedFixtures++
		}
	}

	if len(summary.Throttle) > 0 {
		fmt.Println("\nBackend throttling detected:")
		for _, ev := range summary.Throttle {
			fmt.Printf("  \u26a0 reduced parallelism %d \u2192 %d (%s) at %s\n",
				ev.From, ev.To, ev.Reason, ev.At.Format(time.RFC3339))
		}
	}

	fmt.Printf("\nBatch: %d/%d fixture(s) passed\n", len(targets)-failedFixtures, len(targets))
	if failedFixtures > 0 {
		return fmt.Errorf("%d of %d fixtures failed", failedFixtures, len(targets))
	}
	return nil
}

// ─── check-external-name-prefix ───────────────────────────────────────────

// timeoutOptions holds the parsed command line of the subcommands whose only
// flag is an integer timeout in seconds.
type timeoutOptions struct {
	manifestPath string
	timeout      int
}

func parseTimeoutArgs(command string, defaultTimeout int, usage string, args []string) (timeoutOptions, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	timeout := fs.Int("timeout", defaultTimeout, "Timeout in seconds for kubectl calls")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return timeoutOptions{}, err
	}
	if fs.NArg() < 1 {
		return timeoutOptions{}, errors.New(usage)
	}
	return timeoutOptions{manifestPath: fs.Arg(0), timeout: *timeout}, nil
}

func parseCheckExternalNamePrefixArgs(args []string) (timeoutOptions, error) {
	return parseTimeoutArgs("check-external-name-prefix", 30,
		"usage: update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]", args)
}

// cmdCheckExternalNamePrefix asserts that the live resource's
// crossplane.io/external-name annotation has the prefix the manifest
// declares via crossplane.io/expect-external-name-prefix. This exists for
// resources whose backend models more than one object type behind a single
// Kubernetes kind, where an identity search issued against the wrong type
// returns zero matches and the reconciler silently creates a duplicate —
// a failure invisible to a plain Ready assertion. See
// runner.CheckExternalNamePrefix for the underlying (pure, unit-testable)
// check.
func cmdCheckExternalNamePrefix(args []string) error {
	opts, err := parseCheckExternalNamePrefixArgs(args)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(opts.manifestPath)
	if err != nil {
		return err
	}
	if m.ExpectExternalNamePrefix == "" {
		return fmt.Errorf("manifest has no %s annotation — nothing to check", manifest.ExpectExternalNamePrefixKey)
	}

	r := runner.NewRunner(opts.manifestPath, opts.timeout)
	if err := r.ResolveResource(m); err != nil {
		return err
	}

	name, err := r.ExternalName()
	if err != nil {
		return err
	}

	ok, reason := runner.CheckExternalNamePrefix(name, m.ExpectExternalNamePrefix)
	fmt.Printf("External-name prefix check: %s/%s\n", m.Kind, m.Name)
	if !ok {
		fmt.Printf("  ✗ %s\n", reason)
		return errors.New(reason)
	}
	fmt.Printf("  ✓ external-name %q has expected prefix %q\n", name, m.ExpectExternalNamePrefix)
	return nil
}

// ─── resolve-recover ──────────────────────────────────────────────────────

func parseResolveRecoverArgs(args []string) (timeoutOptions, error) {
	return parseTimeoutArgs("resolve-recover", 120,
		"usage: update-tester resolve-recover <manifest.yaml> [--timeout 120]", args)
}

// cmdResolveRecover asserts that a resource whose backend models more than
// one object type behind a single kind recovers its identity via search
// (rather than duplicating) when its crossplane.io/external-name annotation
// is stripped and reconciliation is resumed. See runner.Runner's
// RunResolveRecover for the full algorithm and the rationale for the two
// independent pass signals.
func cmdResolveRecover(args []string) error {
	opts, err := parseResolveRecoverArgs(args)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(opts.manifestPath)
	if err != nil {
		return err
	}

	result, err := runner.NewRunner(opts.manifestPath, opts.timeout).RunResolveRecover(m)
	if err != nil {
		return err
	}

	printResolveRecoverResult(os.Stdout, m, result)

	if !result.Passed {
		return errors.New("resource did not recover its identity by search")
	}
	return nil
}

// printResolveRecoverResult prints the outcome of a resolve-recover check.
func printResolveRecoverResult(w io.Writer, m *manifest.Manifest, r *runner.ResolveRecoverResult) {
	printfTo(w, "Resolve-recover check: %s/%s\n", m.Kind, m.Name)
	printfTo(w, "  external-name before strip: %q\n", r.ExternalNameBefore)
	printfTo(w, "  external-name after recovery: %q\n", r.ExternalNameAfter)
	printfTo(w, "  %s events across lifecycle: %d\n", runner.EventReasonCreated, r.CreateEventCount)
	switch {
	case r.Passed:
		printfTo(w, "  ✓ recovery: %s\n", r.Message)
	default:
		printfTo(w, "  ✗ recovery: %s\n", r.Message)
		for _, d := range r.Diagnostics {
			printfTo(w, "    - %s\n", d)
		}
	}
}

// ─── roundtrip-diff ───────────────────────────────────────────────────────

// roundtripDiffOptions holds the parsed command line of `roundtrip-diff`.
type roundtripDiffOptions struct {
	manifestPaths []string
	root          string
	timeout       int
}

func parseRoundtripDiffArgs(args []string) (roundtripDiffOptions, error) {
	fs := flag.NewFlagSet("roundtrip-diff", flag.ContinueOnError)
	root := fs.String("root", "", "Provider repo root holding package/crds (default: working directory)")
	timeout := fs.Int("timeout", 30, "Timeout in seconds for kubectl calls")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return roundtripDiffOptions{}, err
	}
	if fs.NArg() < 1 {
		return roundtripDiffOptions{}, errors.New(
			"usage: update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]")
	}

	// Accept both a comma-separated list and repeated positional
	// arguments, matching converge-all's own manifest-list convention.
	var paths []string
	for _, arg := range fs.Args() {
		for _, p := range strings.Split(arg, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}

	resolvedRoot := *root
	if resolvedRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return roundtripDiffOptions{}, fmt.Errorf("determining working directory for --root: %w", err)
		}
		resolvedRoot = wd
	}

	return roundtripDiffOptions{manifestPaths: paths, root: resolvedRoot, timeout: *timeout}, nil
}

// cmdRoundtripDiff prints the advisory spec.forProvider <-> status.atProvider
// round-trip report for one or more already-live manifests — one `kubectl
// get` per manifest, plus a scan of --root/package/crds to find each one's
// CRD. Like the Python tool it replaces (see package roundtrip's doc
// comment), this is read-only end to end and its own exit status reflects
// only whether it could RUN, never what it found: a manifest that cannot be
// resolved, has no matching CRD, or cannot be diffed for any other reason is
// skipped with a note on stderr rather than failing the command.
func cmdRoundtripDiff(args []string) error {
	opts, err := parseRoundtripDiffArgs(args)
	if err != nil {
		return err
	}

	reported := 0
	for _, p := range opts.manifestPaths {
		m, err := manifest.Parse(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-diff: SKIP %s — %v\n", p, err)
			continue
		}

		r := runner.NewRunner(p, opts.timeout)
		if err := r.ResolveResource(m); err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-diff: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}
		obj, err := r.GetObject()
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-diff: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}

		crd, _ := roundtrip.FindCRD(opts.root, m.APIVersion, m.Kind)
		if crd == nil {
			fmt.Fprintf(os.Stderr, "roundtrip-diff: SKIP %s/%s — no matching CRD under %s\n",
				m.Kind, m.Name, filepath.Join(opts.root, "package", "crds"))
			continue
		}
		rows, err := roundtrip.DiffReport(crd, obj)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-diff: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}

		printfTo(os.Stdout, "%s\n", roundtrip.FormatReport(m.Kind, m.Name, rows))
		reported++
	}

	if reported == 0 {
		fmt.Fprintln(os.Stderr, "roundtrip-diff: no resources produced a report (see stderr for skips, if any)")
	}
	return nil
}

// ─── roundtrip-verify ─────────────────────────────────────────────────────

// roundtripVerifyOptions holds the parsed command line of `roundtrip-verify`.
type roundtripVerifyOptions struct {
	manifestPaths []string
	root          string
	timeout       int
	// backend is the provider's DECLARED backend classification (see
	// roundtrip.BackendType) — REQUIRED, with no fallback that guesses
	// one: omitting --backend is a parse error, never an assumption.
	backend roundtrip.BackendType
}

func parseRoundtripVerifyArgs(args []string) (roundtripVerifyOptions, error) {
	fs := flag.NewFlagSet("roundtrip-verify", flag.ContinueOnError)
	root := fs.String("root", "", "Provider repo root holding package/crds (default: working directory)")
	timeout := fs.Int("timeout", 30, "Timeout in seconds for kubectl calls")
	backend := fs.String("backend", "", "REQUIRED. Declared backend classification for cell-denominator provenance: real or simulator")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return roundtripVerifyOptions{}, err
	}
	if fs.NArg() < 1 {
		return roundtripVerifyOptions{}, errors.New(
			"usage: update-tester roundtrip-verify <m1.yaml,m2.yaml,...> --backend <real|simulator> [--root <dir>] [--timeout 30]")
	}

	// --backend is REQUIRED, with no fallback that guesses: an undeclared
	// provider is an error, never an assumption (roundtrip.ParseBackendType
	// enforces the same closed set with no default, but this command must
	// not even reach it with an empty string — that used to leave the
	// backend silently unset and every cell-denominator report unable to
	// distinguish a simulator-derived classification from a real one).
	if *backend == "" {
		return roundtripVerifyOptions{}, errors.New(
			"roundtrip-verify: --backend is required (real or simulator) — it is never inferred from a provider name, endpoint, or URL")
	}
	backendType, err := roundtrip.ParseBackendType(*backend)
	if err != nil {
		return roundtripVerifyOptions{}, err
	}

	// Accept both a comma-separated list and repeated positional
	// arguments, matching roundtrip-diff's own manifest-list convention.
	var paths []string
	for _, arg := range fs.Args() {
		for _, p := range strings.Split(arg, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}

	resolvedRoot := *root
	if resolvedRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return roundtripVerifyOptions{}, fmt.Errorf("determining working directory for --root: %w", err)
		}
		resolvedRoot = wd
	}

	return roundtripVerifyOptions{manifestPaths: paths, root: resolvedRoot, timeout: *timeout, backend: backendType}, nil
}

// roundtripVerifyRowJSON is the machine-readable shape one roundtrip.Row
// renders as in cmdRoundtripVerify's per-manifest report.
type roundtripVerifyRowJSON struct {
	Path           string      `json:"path"`
	Classification string      `json:"classification"`
	SpecFound      bool        `json:"specFound"`
	SpecValue      interface{} `json:"specValue,omitempty"`
	MirrorFound    bool        `json:"mirrorFound"`
	MirrorValue    interface{} `json:"mirrorValue,omitempty"`
	// Immutable mirrors roundtrip.Row.Immutable — present here so a reader
	// of the raw JSON can see WHY a present-in-spec-absent-from-mirror row
	// was removed from mustTestCount without cross-referencing excluded by
	// field name.
	Immutable bool `json:"immutable,omitempty"`
}

// roundtripVerifyFindingJSON is the machine-readable shape one
// roundtrip.MustTestFinding renders as.
type roundtripVerifyFindingJSON struct {
	Field          string `json:"field"`
	Classification string `json:"classification"`
	Detail         string `json:"detail"`
}

// roundtripVerifyExcludedJSON is the machine-readable shape one
// roundtrip.ExcludedFinding renders as — a field removed from mustTestCount
// because it is CEL-immutable, distinct from roundtripVerifyFindingJSON:
// this never contributed to anyFindings and never gates the command's exit
// code.
type roundtripVerifyExcludedJSON struct {
	Field          string `json:"field"`
	Classification string `json:"classification"`
	Detail         string `json:"detail"`
}

// roundtripVerifyReportJSON is the full machine-readable report for one
// manifest. It is printed on EVERY manifest roundtrip-verify can reach a
// live object for — pass or fail — never gated on whether a finding turned
// up, so a caller never has to infer "were rows even computed?" from the
// exit code the way converge-all's advisory inline report requires.
//
// Backend/Seed/Cells/ContainerClear/Waivers are additive: every field this
// ticket adds. None of them ever changes anyFindings (see
// buildRoundtripVerifyReport) — Cells/ContainerClear/Waivers are simply
// additional information a reader may act on later, not a new way for
// this command to fail, and Backend itself is never read to make that
// decision either, even though it is now a required, always-populated
// value.
type roundtripVerifyReportJSON struct {
	Kind          string                        `json:"kind"`
	Name          string                        `json:"name"`
	Rows          []roundtripVerifyRowJSON      `json:"rows"`
	MustTestCount int                           `json:"mustTestCount"`
	Findings      []roundtripVerifyFindingJSON  `json:"findings"`
	Excluded      []roundtripVerifyExcludedJSON `json:"excluded"`
	// Backend is empty when undeclared (the default for every provider
	// today) — never guessed.
	Backend string `json:"backend,omitempty"`
	// Seed is the rotation schedule's own seed for this run — recorded and
	// reported so a reader can reproduce exactly which members were chosen.
	Seed           int64                `json:"seed"`
	Cells          []cellCreditJSON     `json:"cells,omitempty"`
	ContainerClear []containerClearJSON `json:"containerClear,omitempty"`
	// ContainerClearError is set instead of ContainerClear when
	// roundtrip.ContainerClearCoverage itself errors — most notably its own
	// ineligible-and-covered contradiction check. Never gates this command's
	// exit code (container-clear stays report-only), but a contradiction is
	// reported here rather than silently collapsing ContainerClear to an
	// empty list, which would read identically to "nothing declared".
	ContainerClearError string              `json:"containerClearError,omitempty"`
	Waivers             []waiverFindingJSON `json:"waivers,omitempty"`
}

// cellCreditJSON is the machine-readable shape one roundtrip.CellCredit (an
// `equal` cell) OR one roundtrip.ClearCellReport (a container-clear cell)
// renders as — the SAME array both directions join (RULING 1's own
// requirement: "clear cells join the existing cells[] array", no parallel
// DTO), distinguished by Direction. SimulatorSatisfied restates the
// report's own Backend declaration on every equal-cell line, per the
// provenance requirement: a reader filtering or grepping cell lines never
// has to cross-reference a separate top-level field to see whether a cell
// was satisfied by a simulator-derived classification; it is always false
// on a clear-direction line, which this offline check never derives from a
// live backend at all.
//
// Depth, Vacuous, UndispositionedMembers and Route are populated ONLY on a
// clear-direction line (Direction == "clear") — RULING 1's depth axis,
// RULING 2's existential/universal split, and the named credit route AC 10
// requires. All four are the zero value, and omitted, on every equal-cell
// line, so an existing reader parsing only
// Classification/Shape/Direction/Members/Representatives/Credited/Sticky/
// SimulatorSatisfied sees no change to those fields' own shape.
type cellCreditJSON struct {
	Classification     string   `json:"classification"`
	Shape              string   `json:"shape"`
	Direction          string   `json:"direction"`
	Members            []string `json:"members"`
	Representatives    []string `json:"representatives"`
	Credited           []string `json:"credited"`
	Sticky             []string `json:"sticky,omitempty"`
	SimulatorSatisfied bool     `json:"simulatorSatisfied,omitempty"`
	// The four fields below are clear-direction-only — see this type's own
	// doc comment.
	Depth                  string   `json:"depth,omitempty"`
	Vacuous                bool     `json:"vacuous,omitempty"`
	UndispositionedMembers []string `json:"undispositionedMembers,omitempty"`
	Route                  string   `json:"route,omitempty"`
}

// containerClearJSON is the machine-readable shape one
// roundtrip.ContainerClearFinding renders as. REPORT-ONLY: nothing reads
// this slice to decide anyFindings — see buildRoundtripVerifyReport.
// Ineligible/Reason surface the third state (see
// roundtrip.ContainerClearFinding's own doc comment): Covered is always
// false when Ineligible is true, and Reason is empty otherwise.
// Disposition is also report-only and empty whenever Covered is true,
// Ineligible is true, or no disposition: was authored on the leaf's own
// skip: entry — omitempty means a report from a manifest with zero
// authored dispositions (every manifest in the fleet today) renders
// byte-identical to before this field existed. Route names Covered's own
// credit mechanism (one of the five ClearRoute constants) and is empty
// whenever Covered is false.
type containerClearJSON struct {
	Path        string `json:"path"`
	Shape       string `json:"shape"`
	Covered     bool   `json:"covered"`
	Ineligible  bool   `json:"ineligible,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Detail      string `json:"detail"`
	Disposition string `json:"disposition,omitempty"`
	Route       string `json:"route,omitempty"`
}

// waiverFindingJSON is the machine-readable shape one
// roundtrip.WaiverFinding (the waiver bucket classification) renders as.
type waiverFindingJSON struct {
	Field  string `json:"field"`
	Bucket string `json:"bucket"`
	Detail string `json:"detail"`
}

func toCellCreditJSON(credits []roundtrip.CellCredit, backend roundtrip.BackendType) []cellCreditJSON {
	provenance := roundtrip.NewProvenance(backend)
	out := make([]cellCreditJSON, len(credits))
	for i, c := range credits {
		out[i] = cellCreditJSON{
			Classification:     c.Key.Classification,
			Shape:              string(c.Key.Shape),
			Direction:          string(c.Key.Direction),
			Members:            c.Members,
			Representatives:    c.Representatives,
			Credited:           c.Credited,
			Sticky:             c.Sticky,
			SimulatorSatisfied: provenance.SimulatorSatisfied,
		}
	}
	return out
}

func toContainerClearJSON(findings []roundtrip.ContainerClearFinding) []containerClearJSON {
	out := make([]containerClearJSON, len(findings))
	for i, f := range findings {
		out[i] = containerClearJSON{
			Path: f.Path, Shape: string(f.Shape), Covered: f.Covered,
			Ineligible: f.Ineligible, Reason: string(f.Reason), Detail: f.Detail,
			Disposition: string(f.Disposition), Route: string(f.Route),
		}
	}
	return out
}

// toClearCellCreditJSON renders BuildClearCellReport's own
// roundtrip.ClearCellReport slice into cellCreditJSON lines — the SAME
// array shape toCellCreditJSON renders an equal cell's roundtrip.CellCredit
// as (RULING 1's "clear cells join the existing cells[] array", no
// parallel DTO). Representatives/Credited follow CellCredit's own
// vocabulary: Representatives is the single sticky credited member
// (RULING 3 — never a rotated subset), Credited is every OTHER eligible
// member of a covered cell, mechanically credited by cell membership. Both
// are empty on an uncovered or vacuous cell, since there is no
// representative to credit siblings from.
func toClearCellCreditJSON(reports []roundtrip.ClearCellReport) []cellCreditJSON {
	out := make([]cellCreditJSON, len(reports))
	for i, r := range reports {
		line := cellCreditJSON{
			Classification:         r.Key.Classification,
			Shape:                  string(r.Key.Shape),
			Direction:              string(r.Key.Direction),
			Members:                r.Members,
			Depth:                  string(r.Key.Depth),
			Vacuous:                r.Vacuous,
			UndispositionedMembers: r.UndispositionedMembers,
			Route:                  string(r.Route),
		}
		if r.Covered {
			line.Representatives = []string{r.Representative}
			for _, m := range r.EligibleMembers() {
				if m != r.Representative {
					line.Credited = append(line.Credited, m)
				}
			}
		}
		out[i] = line
	}
	return out
}

func toWaiverFindingJSON(findings []roundtrip.WaiverFinding) []waiverFindingJSON {
	out := make([]waiverFindingJSON, len(findings))
	for i, f := range findings {
		out[i] = waiverFindingJSON{Field: f.Field, Bucket: string(f.Bucket), Detail: f.Detail}
	}
	return out
}

func toRoundtripVerifyRowJSON(rows []roundtrip.Row) []roundtripVerifyRowJSON {
	out := make([]roundtripVerifyRowJSON, len(rows))
	for i, r := range rows {
		out[i] = roundtripVerifyRowJSON{
			Path:           r.Path,
			Classification: r.Classification,
			SpecFound:      r.SpecFound,
			SpecValue:      r.SpecValue,
			MirrorFound:    r.MirrorFound,
			MirrorValue:    r.MirrorValue,
			Immutable:      r.Immutable,
		}
	}
	return out
}

func toRoundtripVerifyFindingJSON(findings []roundtrip.MustTestFinding) []roundtripVerifyFindingJSON {
	out := make([]roundtripVerifyFindingJSON, len(findings))
	for i, f := range findings {
		out[i] = roundtripVerifyFindingJSON{Field: f.Field, Classification: f.Classification, Detail: f.Detail}
	}
	return out
}

func toRoundtripVerifyExcludedJSON(excluded []roundtrip.ExcludedFinding) []roundtripVerifyExcludedJSON {
	out := make([]roundtripVerifyExcludedJSON, len(excluded))
	for i, e := range excluded {
		out[i] = roundtripVerifyExcludedJSON{Field: e.Field, Classification: e.Classification, Detail: e.Detail}
	}
	return out
}

// rotationStateDir returns the directory roundtrip-verify persists rotation
// state files under. It follows the XDG Base Directory "state" home
// (data that should survive across restarts but is not worth backing up
// or syncing — an exact fit for a round-robin cursor): $XDG_STATE_HOME
// when set, else $HOME/.local/state, else a temp-dir fallback for a
// HOME-less environment (e.g. some CI runners) so the function never
// fails outright. Living outside any provider's git tree is deliberate:
// see rotationStatePath.
func rotationStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "update-tester", "rotation")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "update-tester", "rotation")
	}
	return filepath.Join(os.TempDir(), "update-tester", "rotation")
}

// rotationStatePath returns the path roundtrip-verify persists its
// RotationState at for root — one file per provider, named by a hash of
// root's absolute path, living under rotationStateDir rather than inside
// root itself. The round-robin schedule (see roundtrip.RotationState)
// still survives across separate invocations/runs exactly as before; what
// changed is WHERE it survives.
//
// root previously named a hidden file inside the provider repository, and
// no provider's .gitignore is guaranteed to cover it — a state file
// dropped into a git-controlled tree becomes untracked noise one
// `git add`-with-a-wildcard away from being committed, on every provider,
// forever. Hashing root's absolute path rather than embedding it verbatim
// avoids building a filename out of arbitrary path characters (Windows
// drive letters, unusual bytes) while still keeping one provider's
// rotation state from colliding with another's — the same root always
// hashes to the same name, so the schedule keeps resuming across runs.
func rotationStatePath(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(rotationStateDir(), fmt.Sprintf("%x.json", sum))
}

// manifestScope derives the rotation cursor's scope key for m — see
// stateKey and roundtrip.GroupCells' own doc comment for why cell
// membership, and therefore the cursor that credits it, must be scoped per
// manifest rather than per Kind.
//
// The value is m's own Kubernetes object identity: APIVersion, Kind,
// Namespace and Name together. Kind+Name alone collides for the
// cluster-scoped/namespaced example pair every resource ships (both
// carrying the identical metadata.name, distinguished only by apiVersion
// and namespace) — that collision let the two share one rotation cursor
// and left a measured 28.7% of one provider's equal-cell members never
// selected. Namespace is empty for a cluster-scoped manifest and
// non-empty for its namespaced twin, and APIVersion differs between the
// two API groups the dual-scope split generates, so this combination is
// unique per manifest even when Kind and Name coincide.
//
// A provider's existing rotation-state file keys its cursors by the old
// Kind+Name scope; after this change every existing key goes unmatched,
// so each manifest simply starts a fresh round-robin cursor from zero on
// its first run post-upgrade. That reconverges within
// RepresentativesPerRun's own bound and needs no migration.
func manifestScope(m *manifest.Manifest) string {
	return m.APIVersion + "|" + m.Kind + "|" + m.Namespace + "|" + m.Name
}

// buildRoundtripVerifyReport is cmdRoundtripVerify's pure core: every input
// already resolved (a parsed manifest, a matched CRD, DiffReport's own
// rows) so the full report shape — including every cell-denominator field
// this ticket adds — is unit-testable without a live cluster. See
// CheckExternalNamePrefix's own doc comment for why this carve-out exists:
// the edge cases need proving without kubectl.
//
// anyFindings mirrors cmdRoundtripVerify's own exit-code decision and is
// derived SOLELY from findings (roundtrip.DenominatorReport's own
// must-test violations, unchanged since before this ticket) — never from
// containerClear or waivers, which are additive, report-only fields. A
// provider with ZERO clear-direction coverage anywhere never causes
// anyFindings to become true on that basis alone. findings/excluded are
// returned alongside the JSON report so a caller can feed
// roundtrip.PrintDenominatorFindings without re-deriving them from the
// report's own JSON DTOs.
func buildRoundtripVerifyReport(
	m *manifest.Manifest,
	crd map[string]interface{},
	rows []roundtrip.Row,
	backend roundtrip.BackendType,
	rotation *roundtrip.RotationState,
) (report roundtripVerifyReportJSON, findings []roundtrip.MustTestFinding, excluded []roundtrip.ExcludedFinding, anyFindings bool) {
	var mustTestCount int
	findings, mustTestCount, excluded = roundtrip.DenominatorReport(m, rows)

	// scope identifies the manifest that produced rows, so the rotation
	// cursor CreditCells advances is this manifest's own — never shared
	// with another manifest that happens to produce the same CellKey (see
	// roundtrip.GroupCells' own doc comment on why cell scope is settled
	// at per-manifest, manifestScope for why Kind+Name alone cannot carry
	// that identity, and stateKey for the cursor this feeds).
	scope := manifestScope(m)
	cells := roundtrip.GroupCells(rows)
	credits, _ := roundtrip.CreditCells(cells, rotation, scope)

	var clearFindings []roundtrip.ContainerClearFinding
	var containerClearErr string
	if crd != nil {
		// An error here is no longer assumed impossible: ContainerClearCoverage
		// itself refuses to produce a finding that is both ineligible (see
		// roundtrip.IneligibilityReason) and covered by an existing manifest
		// entry, and the fleet HAS measured live instances of that
		// contradiction (an ancestor clear: tombstone that incidentally also
		// nulls a reference-resolution sibling; a self-tombstone authored
		// against a field a CEL rule actually requires). Surfacing it as
		// ContainerClearError — rather than silently discarding clearFindings
		// to an empty list, which would read identically to "nothing
		// declared" — keeps container-clear report-only (still never gates
		// this command's exit code) while making the contradiction visible
		// rather than invisible.
		var err error
		clearFindings, err = roundtrip.ContainerClearCoverage(crd, m)
		if err != nil {
			containerClearErr = err.Error()
			clearFindings = nil
		}
	}
	waivers := roundtrip.ClassifyWaivers(m, rows)

	// RULING 1's own requirement: clear cells join the SAME cells[] array
	// the equal-cell credits above populate, never a parallel array — see
	// toClearCellCreditJSON and cellCreditJSON's own doc comment. Grouped
	// only when ContainerClearCoverage produced no error, matching
	// ContainerClear's own empty-on-error behaviour above.
	cellsJSON := toCellCreditJSON(credits, backend)
	if clearFindings != nil {
		cellsJSON = append(cellsJSON, toClearCellCreditJSON(roundtrip.BuildClearCellReport(clearFindings))...)
	}

	report = roundtripVerifyReportJSON{
		Kind:                m.Kind,
		Name:                m.Name,
		Rows:                toRoundtripVerifyRowJSON(rows),
		MustTestCount:       mustTestCount,
		Findings:            toRoundtripVerifyFindingJSON(findings),
		Excluded:            toRoundtripVerifyExcludedJSON(excluded),
		Backend:             string(backend),
		Seed:                rotation.Seed,
		Cells:               cellsJSON,
		ContainerClear:      toContainerClearJSON(clearFindings),
		ContainerClearError: containerClearErr,
		Waivers:             toWaiverFindingJSON(waivers),
	}
	return report, findings, excluded, len(findings) > 0
}

// cmdRoundtripVerify is the enforcing counterpart to roundtrip-diff: where
// that command is purely advisory and never fails on what it finds, this
// one derives the must-test denominator from the SAME live classification
// (see package roundtrip's DenominatorReport) and fails when a must-test
// field's own skip: waiver does not hold up against its own live row.
//
// Rows are computed and printed for every manifest this command can reach a
// live object for, on every invocation — never conditioned on whether the
// resource's converge/run checks passed or failed, unlike converge-all's
// advisory inline report (see RoundtripFindingsForFailures), which stays
// exactly as it was: gated to failures, and never gating the command's own
// exit code. This command's exit code is the new, separate gate.
func cmdRoundtripVerify(args []string) error {
	opts, err := parseRoundtripVerifyArgs(args)
	if err != nil {
		return err
	}

	rotationPath := rotationStatePath(opts.root)
	rotation, err := roundtrip.LoadRotationState(rotationPath)
	if err != nil {
		return fmt.Errorf("loading rotation state: %w", err)
	}

	produced := 0
	anyFindings := false
	for _, p := range opts.manifestPaths {
		m, err := manifest.Parse(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: SKIP %s — %v\n", p, err)
			continue
		}

		r := runner.NewRunner(p, opts.timeout)
		if err := r.ResolveResource(m); err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}
		obj, err := r.GetObject()
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}

		crd, _ := roundtrip.FindCRD(opts.root, m.APIVersion, m.Kind)
		if crd == nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: SKIP %s/%s — no matching CRD under %s\n",
				m.Kind, m.Name, filepath.Join(opts.root, "package", "crds"))
			continue
		}
		rows, err := roundtrip.DiffReport(crd, obj)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: SKIP %s/%s — %v\n", m.Kind, m.Name, err)
			continue
		}

		report, manifestFindings, manifestExcluded, findingsForManifest := buildRoundtripVerifyReport(m, crd, rows, opts.backend, &rotation)
		encoded, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encoding roundtrip-verify report for %s/%s: %w", m.Kind, m.Name, err)
		}
		printfTo(os.Stdout, "%s\n", encoded)

		printfTo(os.Stdout, "roundtrip-verify: %s/%s\n", m.Kind, m.Name)
		if report.ContainerClearError != "" {
			printfTo(os.Stdout, "container-clear: ERROR — %s\n", report.ContainerClearError)
		}
		roundtrip.PrintDenominatorFindings(func(format string, args ...interface{}) {
			printfTo(os.Stdout, format, args...)
		}, report.MustTestCount, manifestFindings, manifestExcluded)

		produced++
		if findingsForManifest {
			anyFindings = true
		}
	}

	if produced > 0 {
		if err := rotation.Save(rotationPath); err != nil {
			fmt.Fprintf(os.Stderr, "roundtrip-verify: WARNING could not persist rotation state to %s: %v\n", rotationPath, err)
		}
	}

	if produced == 0 {
		return errors.New("roundtrip-verify: no resources produced a report (see stderr for skips, if any)")
	}
	if anyFindings {
		return errors.New("one or more must-test fields carry a skip: waiver this live run could not confirm")
	}
	return nil
}

// ─── residual ─────────────────────────────────────────────────────────────

// residualOptions holds the parsed command line of the `residual` subcommand.
type residualOptions struct {
	root string
}

func parseResidualArgs(args []string) (residualOptions, error) {
	fs := flag.NewFlagSet("residual", flag.ContinueOnError)
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return residualOptions{}, err
	}
	if fs.NArg() < 1 {
		return residualOptions{}, errors.New("usage: update-tester residual <examples-dir>")
	}
	return residualOptions{root: fs.Arg(0)}, nil
}

// cmdResidual walks a provider's whole examples/ tree and reports the
// repo-scope cell-denominator residual: every disposition-carrying skip:
// entry, parsed through the tool's own manifest parser rather than a
// caller-written re-implementation — see package residual's own doc
// comment for why every ad-hoc script taking this measurement by hand has
// under-reported it.
//
// This is offline and read-only: no cluster is touched, and nothing here
// is a live check of whether a waiver still holds (that is
// roundtrip-verify's job). Its own exit status reflects only whether every
// fixture carrying the annotation actually parsed — fixtures_with_annotation
// and parsed_ok are printed as a pair, and a fixture that fails to parse is
// named rather than silently folded into "no annotation".
func cmdResidual(args []string) error {
	opts, err := parseResidualArgs(args)
	if err != nil {
		return err
	}

	res, err := residual.Scan(opts.root)
	if err != nil {
		return fmt.Errorf("residual: %w", err)
	}

	residual.PrintReport(os.Stdout, res)

	if res.Counts.FixturesWithAnnotation != res.Counts.ParsedOK {
		names := make([]string, len(res.Failures))
		for i, f := range res.Failures {
			names[i] = f.Path
		}
		return fmt.Errorf("residual: fixtures_with_annotation=%d parsed_ok=%d — %d fixture(s) failed to parse: %s",
			res.Counts.FixturesWithAnnotation, res.Counts.ParsedOK, len(res.Failures), strings.Join(names, ", "))
	}
	return nil
}

// ─── hook ─────────────────────────────────────────────────────────────────

// Environment variables read by the `hook` subcommand. They exist so a CI
// job can retune the whole post-assert sequence without editing every
// symlink's manifest or the wrapper script.
const (
	// envManifest overrides manifest derivation entirely, for debugging a
	// manifest that does not follow the naming convention.
	envManifest = "MANIFEST"
	// envTimeout is an integer number of seconds.
	envTimeout = "UPDATE_TESTER_TIMEOUT"
	// envPollInterval is a Go duration string, e.g. "90s". It reaches both
	// the convergence checks (which wait on it) and the per-field run
	// (which calibrates its slow-observe annotation with it).
	envPollInterval = "UPDATE_TESTER_POLL_INTERVAL"
	// envIgnoreFields is a comma-separated list of atProvider field names,
	// forwarded only to the two `converge` steps — the only subcommand that
	// accepts --ignore-fields at all. Excludes named fields from the
	// snapshot diff for resources with a server-driven atProvider field
	// (e.g. a one-time timestamp populated asynchronously by the backend)
	// that is not itself evidence of drift.
	envIgnoreFields = "UPDATE_TESTER_IGNORE_FIELDS"
)

// hookOptions holds the parsed command line of the `hook` subcommand.
type hookOptions struct {
	invocation   string
	root         string
	manifestPath string
	skipConverge bool
}

func parseHookArgs(args []string) (hookOptions, error) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	root := fs.String("root", "", "Provider repo root the manifest is derived against (default: working directory)")
	manifestPath := fs.String("manifest", "", "Manifest path, overriding derivation from the invocation name")
	skipConverge := fs.Bool("skip-converge", false, "Drop both convergence steps from the post-assert sequence. Opt-in, per provider — only meaningful once the provider asserts convergence some other way (e.g. a shared observation window run separately). Defaults to false so an unmodified provider keeps its convergence coverage.")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return hookOptions{}, err
	}
	if fs.NArg() < 1 {
		return hookOptions{}, errors.New("usage: update-tester hook <invocation-name> [--root <dir>] [--manifest <path>] [--skip-converge]")
	}
	return hookOptions{invocation: fs.Arg(0), root: *root, manifestPath: *manifestPath, skipConverge: *skipConverge}, nil
}

// hookStep is one step of the post-assert sequence: a subcommand to run, and
// the label it is announced under.
//
// banner and command are separate because the sequence runs `converge`
// twice, and the two runs are not interchangeable to a reader of the log —
// see hookSteps.
type hookStep struct {
	banner  string
	command string
}

// hookSteps returns the post-assert sequence for a manifest, in order. It is
// a pure function of the parsed manifest (plus the skipConverge switch) so
// the sequence — the one part of the hook that cannot be exercised without a
// cluster — is unit-testable on its own.
//
// The sequence is:
//
//  1. converge, before anything is touched. A resource that is already
//     looping must abort the hook here, so the per-field tests below never
//     run against an unstable resource and report noise.
//  2. check-external-name-prefix and resolve-recover, only when the manifest
//     declares an expected external-name prefix. Both are meaningless
//     without that declaration, and both must run before the per-field tests
//     so their pause/patch machinery never interleaves with the tests'.
//  3. run — the per-field update tests.
//  4. converge again. This is not a repeat of step 1: step 3 proved that
//     values round-trip, which is not the same as proving the controller
//     stopped reconciling. A resource stuck in a perpetual Update loop
//     reports Ready on every cycle, so a Ready assertion cannot see it —
//     only a second convergence window, taken after every field transition
//     in the manifest, catches it.
//
// skipConverge drops both converge steps (1 and 4) when true, leaving
// [check-external-name-prefix, resolve-recover] -> run. It exists for a
// provider that asserts convergence some other way — e.g. one shared
// observation window run once for many resources, rather than one window
// per resource here — and must default to false: most providers have no
// such replacement, and a default-on flag would silently delete their
// convergence coverage.
func hookSteps(m *manifest.Manifest, skipConverge bool) []hookStep {
	var steps []hookStep
	if !skipConverge {
		steps = append(steps, hookStep{banner: "converge", command: "converge"})
	}

	if m.ExpectExternalNamePrefix != "" {
		steps = append(steps,
			hookStep{banner: "check-external-name-prefix", command: "check-external-name-prefix"},
			hookStep{banner: "resolve-recover", command: "resolve-recover"},
		)
	}

	steps = append(steps, hookStep{banner: "run", command: "run"})

	if !skipConverge {
		steps = append(steps, hookStep{banner: "post-update converge", command: "converge"})
	}

	return steps
}

// hookEnv holds the environment-supplied overrides for the checks the hook
// runs. A zero field means "not set": nothing is passed on the command line
// and each subcommand applies its own default.
type hookEnv struct {
	timeout      int
	pollInterval time.Duration
	ignoreFields []string
}

// parseHookEnv interprets the raw environment values. An unparseable value
// is an error rather than a silent fall-back to the default: a typo in a CI
// variable would otherwise surface much later as a timeout nobody asked for.
func parseHookEnv(timeout, pollInterval, ignoreFields string) (hookEnv, error) {
	var env hookEnv
	if timeout != "" {
		n, err := strconv.Atoi(timeout)
		if err != nil || n <= 0 {
			return hookEnv{}, fmt.Errorf("%s=%q is not a positive number of seconds", envTimeout, timeout)
		}
		env.timeout = n
	}
	if pollInterval != "" {
		d, err := time.ParseDuration(pollInterval)
		if err != nil || d <= 0 {
			return hookEnv{}, fmt.Errorf("%s=%q is not a positive Go duration", envPollInterval, pollInterval)
		}
		env.pollInterval = d
	}
	if ignoreFields != "" {
		env.ignoreFields = strings.Split(ignoreFields, ",")
	}
	return env, nil
}

// hookStepArgs builds the argument list for one step. Overrides are only
// passed when set, so an unset environment leaves each subcommand's own
// documented default in force rather than having the hook restate it.
//
// The poll interval reaches every step that accepts it — `converge`, which
// waits on it, and `run`, which calibrates its slow-observe annotation with
// it — so one environment variable describes the provider once and both
// checks are measured against the same cadence. The two identity checks do
// not take the flag at all and would reject it.
//
// --ignore-fields is narrower still: only `converge` accepts it at all (it
// excludes named atProvider fields from the snapshot diff), so it is passed
// to neither `run` nor the two identity checks, matching stepTakesPollInterval's
// own per-step gating.
func hookStepArgs(s hookStep, manifestPath string, env hookEnv) []string {
	var args []string
	if env.pollInterval > 0 && stepTakesPollInterval(s.command) {
		args = append(args, "--poll-interval", env.pollInterval.String())
	}
	if env.timeout > 0 {
		if s.command == "converge" {
			// converge's --timeout is a duration, unlike the other steps'.
			args = append(args, "--timeout", (time.Duration(env.timeout) * time.Second).String())
		} else {
			args = append(args, "--timeout", strconv.Itoa(env.timeout))
		}
	}
	if len(env.ignoreFields) > 0 && s.command == "converge" {
		args = append(args, "--ignore-fields", strings.Join(env.ignoreFields, ","))
	}
	return append(args, manifestPath)
}

// stepTakesPollInterval reports whether a subcommand accepts
// --poll-interval. Passing it to one that does not is a flag-parse error,
// not a harmless extra argument.
func stepTakesPollInterval(command string) bool {
	return command == "converge" || command == "run"
}

// cmdHook runs the full post-assert sequence for the manifest named by the
// invocation this binary was called under.
//
// A provider ships one symlink per resource, all pointing at the same
// wrapper, and the wrapper passes its own $0 through as the invocation name;
// that name — and nothing else — selects the manifest. See package hook for
// the derivation rules.
func cmdHook(args []string) error {
	opts, err := parseHookArgs(args)
	if err != nil {
		return err
	}

	root := opts.root
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return fmt.Errorf("determining working directory for --root: %w", err)
		}
	}

	override := opts.manifestPath
	if override == "" {
		override = os.Getenv(envManifest)
	}

	manifestPath, err := hook.Resolve(root, opts.invocation, override)
	if err != nil {
		return err
	}

	m, err := manifest.Parse(manifestPath)
	if err != nil {
		return err
	}

	env, err := parseHookEnv(os.Getenv(envTimeout), os.Getenv(envPollInterval), os.Getenv(envIgnoreFields))
	if err != nil {
		return err
	}

	for _, s := range hookSteps(m, opts.skipConverge) {
		fmt.Printf("==> update-tester: %s %s\n", s.banner, manifestPath)
		if err := runCommand(s.command, hookStepArgs(s, manifestPath, env)); err != nil {
			return fmt.Errorf("%s: %w", s.banner, err)
		}
	}
	return nil
}

// ─── version ──────────────────────────────────────────────────────────────

// modulePath is this tool's own module path, used to pick its version out of
// the build info of a binary whose main module is a consumer's tool stub.
const modulePath = "github.com/kaessert/crossplane-update-tester"

// devVersion is reported when the binary carries no pinned version, i.e. it
// was built from a working tree rather than resolved from a module.
const devVersion = "(devel)"

func cmdVersion(w io.Writer) error {
	var info *debug.BuildInfo
	if bi, ok := debug.ReadBuildInfo(); ok {
		info = bi
	}
	printfTo(w, "update-tester %s\n", versionFrom(info))
	return nil
}

// versionFrom extracts this module's version from build info.
//
// Consumers install the tool through a `tool` directive in a stub module, so
// the running binary's MAIN module is the consumer's stub, not this one, and
// this module appears among the dependencies at the version the stub pins.
// That pinned version is the whole point of the command — it is how an
// operator confirms which version produced a given E2E log — so the
// dependency list is consulted before falling back to the main module.
func versionFrom(info *debug.BuildInfo) string {
	if info == nil {
		return devVersion
	}
	if info.Main.Path == modulePath && info.Main.Version != "" {
		return info.Main.Version
	}
	for _, d := range info.Deps {
		if d == nil || d.Path != modulePath {
			continue
		}
		if d.Replace != nil && d.Replace.Version != "" {
			return d.Replace.Version
		}
		if d.Version != "" {
			return d.Version
		}
	}
	if info.Main.Version != "" {
		return info.Main.Version
	}
	return devVersion
}

// ─── shared helpers ───────────────────────────────────────────────────────

// printfTo writes a report line and discards the write error. Every caller
// is printing a human-readable check report to stdout (or, under test, to a
// buffer): there is no recovery a caller could attempt if that write failed,
// and returning the error would push error handling into every branch of
// every report printer for no gain.
func printfTo(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func fmtDuration(d time.Duration) string {
	return fmt.Sprintf("%.0fs", d.Seconds())
}

func verdict(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}
