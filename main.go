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
//	update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]
//	update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
//	update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
//	update-tester resolve-recover <manifest.yaml> [--timeout 120]
//	update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
//	update-tester roundtrip-verify <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
//	update-tester hook <invocation-name> [--root <dir>] [--manifest <path>] [--skip-converge]
//	update-tester version
package main

import (
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
	case "check-external-name-prefix":
		return cmdCheckExternalNamePrefix(args)
	case "resolve-recover":
		return cmdResolveRecover(args)
	case "roundtrip-diff":
		return cmdRoundtripDiff(args)
	case "roundtrip-verify":
		return cmdRoundtripVerify(args)
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
update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]
update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
update-tester resolve-recover <manifest.yaml> [--timeout 120]
update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
update-tester roundtrip-verify <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
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
             on every invocation, pass or fail.
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

	// Count tested vs skipped vs known-defect.
	var skipped, knownDefectDeclared int
	for _, t := range m.Tests {
		if t.Skip.Present() {
			skipped++
		}
		if t.KnownDefect != "" {
			knownDefectDeclared++
		}
	}

	fmt.Printf("Testing %s/%s (%d fields, %d skipped, %d known-defect)\n",
		m.Kind, m.Name, len(m.Tests), skipped, knownDefectDeclared)

	results, unchangedViolations, err := runner.NewRunner(opts.manifestPath, opts.timeout).
		WithPollInterval(opts.pollInterval).
		RunTests(m)
	if err != nil {
		return err
	}

	passed, failed, noop, notEvidenced, untrusted, knownDefect := printResults(os.Stdout, results)
	assertUnchangedFailed := printUnchangedAssertions(os.Stdout, m.AssertUnchanged, unchangedViolations)

	total := passed + failed
	fmt.Printf("%s: %d/%d tested, %d/%d skipped, %d no-op, %d not-evidenced, %d untrusted, %d known-defects\n",
		verdict(failed == 0 && !assertUnchangedFailed), passed, total, skipped, len(m.Tests), noop, notEvidenced, untrusted, knownDefect)

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
// returns the passed/failed counts, plus the no-op, not-evidenced,
// untrusted, and known-defect counts (reported separately so each distinct
// verdict is easy to spot in the summary line without being confused with a
// genuine PASS or SKIP). A PASS whose field converged at or above the
// runner's slow-observe threshold is annotated "slow-observe" inline — it is
// still a PASS backed by positive update-event evidence, not a reason for a
// reviewer to suspect the result. An UNTRUSTED result is reported and
// counted as a failure regardless of the field's own Passed/NotEvidenced
// value: it ran after a burst-reset failure earlier in the same run, so
// neither outcome can be trusted to prove or disprove that Update() ran —
// the run's summary line must not be able to read a clean "0 not-evidenced"
// in that case.
//
// A KnownDefect result (see runner.TestResult.KnownDefect) is its OWN
// verdict class, not folded into either passed or failed: non-convergence is
// the entry's expected outcome and must not read as a failure, but it is
// also not a PASS — the field is not actually working. The one exception is
// KnownDefectConverged: the field DID converge, which means the suppressed
// defect appears to be fixed, and that is reported as a hard failure naming
// the ticket ID so the run cannot go green with a stale suppression still in
// place.
func printResults(w io.Writer, results []runner.TestResult) (passed, failed, noop, notEvidenced, untrusted, knownDefect int) {
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
		case r.KnownDefect != "" && r.KnownDefectConverged:
			printfTo(w, "  ✗ %s: KNOWN-DEFECT CONVERGED (%s) — the suppressed defect appears to be fixed; delete the knownDefect token and restore this entry to a plain value:/expect: test (%s)\n",
				r.Field, r.KnownDefect, fmtDuration(r.Duration))
			failed++
		case r.KnownDefect != "":
			printfTo(w, "  ⚑ %s: KNOWN-DEFECT (%s) — non-convergence expected and confirmed (%s)\n",
				r.Field, r.KnownDefect, fmtDuration(r.Duration))
			knownDefect++
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
	return passed, failed, noop, notEvidenced, untrusted, knownDefect
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
}

func parseRoundtripVerifyArgs(args []string) (roundtripVerifyOptions, error) {
	fs := flag.NewFlagSet("roundtrip-verify", flag.ContinueOnError)
	root := fs.String("root", "", "Provider repo root holding package/crds (default: working directory)")
	timeout := fs.Int("timeout", 30, "Timeout in seconds for kubectl calls")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return roundtripVerifyOptions{}, err
	}
	if fs.NArg() < 1 {
		return roundtripVerifyOptions{}, errors.New(
			"usage: update-tester roundtrip-verify <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]")
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

	return roundtripVerifyOptions{manifestPaths: paths, root: resolvedRoot, timeout: *timeout}, nil
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
type roundtripVerifyReportJSON struct {
	Kind          string                        `json:"kind"`
	Name          string                        `json:"name"`
	Rows          []roundtripVerifyRowJSON      `json:"rows"`
	MustTestCount int                           `json:"mustTestCount"`
	Findings      []roundtripVerifyFindingJSON  `json:"findings"`
	Excluded      []roundtripVerifyExcludedJSON `json:"excluded"`
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

		findings, mustTestCount, excluded := roundtrip.DenominatorReport(m, rows)

		report := roundtripVerifyReportJSON{
			Kind:          m.Kind,
			Name:          m.Name,
			Rows:          toRoundtripVerifyRowJSON(rows),
			MustTestCount: mustTestCount,
			Findings:      toRoundtripVerifyFindingJSON(findings),
			Excluded:      toRoundtripVerifyExcludedJSON(excluded),
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encoding roundtrip-verify report for %s/%s: %w", m.Kind, m.Name, err)
		}
		printfTo(os.Stdout, "%s\n", encoded)

		printfTo(os.Stdout, "roundtrip-verify: %s/%s\n", m.Kind, m.Name)
		roundtrip.PrintDenominatorFindings(func(format string, args ...interface{}) {
			printfTo(os.Stdout, format, args...)
		}, mustTestCount, findings, excluded)

		produced++
		if len(findings) > 0 {
			anyFindings = true
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
