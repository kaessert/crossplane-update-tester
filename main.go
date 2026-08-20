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
//	update-tester validate <manifest.yaml> --types-file <types.go> [--controller-dir <dir>]
//	update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
//	update-tester resolve-recover <manifest.yaml> [--timeout 120]
//	update-tester hook <invocation-name> [--root <dir>] [--manifest <path>]
//	update-tester version
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/cli"
	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/hook"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
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
	case "converge":
		return cmdConverge(args)
	case "converge-all":
		return cmdConvergeAll(args)
	case "check-external-name-prefix":
		return cmdCheckExternalNamePrefix(args)
	case "resolve-recover":
		return cmdResolveRecover(args)
	case "hook":
		return cmdHook(args)
	case "version":
		return cmdVersion(os.Stdout)
	default:
		return fmt.Errorf("%w: %s", errUnknownCommand, name)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `update-tester — Crossplane per-field update E2E tester

Usage:
  update-tester run <manifest.yaml> [--timeout 120] [--poll-interval 60s]
  update-tester converge <manifest.yaml> [--poll-interval 60s] [--ignore-fields a,b] [--timeout 120s] [--readiness-timeout 120s]
  update-tester converge-all <m1.yaml,m2.yaml,...> [--poll-interval 60s] [--concurrency 8] [--timeout 120s] [--readiness-timeout 120s]
  update-tester validate <manifest.yaml> --types-file <types.go> [--controller-dir <dir>]
  update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
  update-tester resolve-recover <manifest.yaml> [--timeout 120]
  update-tester hook <invocation-name> [--root <dir>] [--manifest <path>]
  update-tester version

Flags may appear before or after the manifest path.

Commands:
  run        Execute update tests against a live cluster
  validate   Check annotation coverage against Go type definitions
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
		if t.Skip != "" {
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
// untrusted counts (all subsets of failed, reported separately so each
// distinct failure mode is easy to spot in the summary line without being
// confused with a genuine PASS or SKIP). A PASS whose field converged at or
// above the runner's slow-observe threshold is annotated "slow-observe"
// inline — it is still a PASS backed by positive update-event evidence, not
// a reason for a reviewer to suspect the result. An UNTRUSTED result is
// reported and counted as a failure regardless of the field's own
// Passed/NotEvidenced value: it ran after a burst-reset failure earlier in
// the same run, so neither outcome can be trusted to prove or disprove that
// Update() ran — the run's summary line must not be able to read a clean
// "0 not-evidenced" in that case.
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
}

func parseValidateArgs(args []string) (validateOptions, error) {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	typesFile := fs.String("types-file", "", "Path to Go types file containing Parameters struct")
	controllerDir := fs.String("controller-dir", "",
		"Path to the resource's controller package directory (optional). When set, also flags an expect:/value: "+
			"object that omits a member the controller declares server-echoed via a registered go-cmp Transformer "+
			"normalizer, even when that member carries omitempty")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return validateOptions{}, err
	}
	if fs.NArg() < 1 {
		return validateOptions{}, errors.New("usage: update-tester validate <manifest.yaml> --types-file <types.go> [--controller-dir <dir>]")
	}
	if *typesFile == "" {
		return validateOptions{}, errors.New("--types-file is required")
	}
	return validateOptions{manifestPath: fs.Arg(0), typesFile: *typesFile, controllerDir: *controllerDir}, nil
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

	fields, err := validator.ParseGoTypes(opts.typesFile, m.Kind)
	if err != nil {
		return err
	}

	result := validator.ValidateManifest(m, fields)
	validator.PrintValidation(result)

	findings := validator.CheckObservability(opts.typesFile, m.Kind, fields, m.Tests)
	validator.PrintObservability(findings)

	siblingFindings := validator.CheckMergePatchSiblings(m)
	validator.PrintMergePatchSiblings(siblingFindings)

	incompleteFindings := validator.CheckIncompleteExpectations(opts.typesFile, fields, m)
	validator.PrintIncompleteExpectations(incompleteFindings)

	echoFindings, err := validator.CheckServerEchoedExpectations(opts.typesFile, fields, m, opts.controllerDir)
	if err != nil {
		return fmt.Errorf("checking server-echoed expectations: %w", err)
	}
	validator.PrintServerEchoedExpectations(echoFindings)

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
	ignoreFields := fs.String("ignore-fields", "", "Comma-separated atProvider fields excluded from snapshot diff")
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
	return convergeOptions{
		manifestPath:     fs.Arg(0),
		pollInterval:     *pollInterval,
		ignoreFields:     ignore,
		timeout:          *timeout,
		readinessTimeout: *readinessTimeout,
	}, nil
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
	result, err := r.RunConverge(m, runner.ConvergeOptions{
		PollInterval:     opts.pollInterval,
		IgnoreFields:     opts.ignoreFields,
		Timeout:          opts.timeout,
		ReadinessTimeout: opts.readinessTimeout,
	})
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
// --ignore-fields here is FLEET-WIDE: one set applied to every target. That
// option is really per-resource (a Loadbalancer's forwardRules is meaningless
// to a Network), and a single flag applied across a whole fleet does silently
// widen every resource's exclusion set to the union of all of them — turning
// a targeted exclusion into fleet-wide blindness, which is the exact failure
// convention 0033 warns about for converge-skip. A real implementation reads
// each manifest's own exclusions; that is the open design fork.
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
	// POC (converge-barrier): a FLEET-WIDE ignore set. This is lossless only
	// where every resource in the case shares one set — true on f5xc, where
	// UPDATE_TESTER_IGNORE_FIELDS is uniformly "status". On a provider with
	// divergent per-target sets (vultr: latestBackup / ruleCount,dateModified
	// / kvm,powerStatus,serverStatus) a single flag would union them into
	// fleet-wide blindness, which is the open design fork.
	ignoreFields := fs.String("ignore-fields", "", "Comma-separated atProvider fields excluded from the diff (fleet-wide)")
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
	return convergeAllOptions{
		manifestPaths:    paths,
		pollInterval:     *pollInterval,
		timeout:          *timeout,
		readinessTimeout: *readinessTimeout,
		concurrency:      *concurrency,
		ignoreFields:     ignore,
	}, nil
}

func cmdConvergeAll(args []string) error {
	opts, err := parseConvergeAllArgs(args)
	if err != nil {
		return err
	}

	targets := make([]runner.ConvergeTarget, 0, len(opts.manifestPaths))
	for _, p := range opts.manifestPaths {
		m, err := manifest.Parse(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		targets = append(targets, runner.ConvergeTarget{
			Label:    fmt.Sprintf("%s/%s", m.Kind, m.Name),
			Runner:   runner.NewRunner(p, int(opts.timeout.Seconds())),
			Manifest: m,
			Opts: runner.ConvergeOptions{
				PollInterval:     opts.pollInterval,
				Timeout:          opts.timeout,
				ReadinessTimeout: opts.readinessTimeout,
				IgnoreFields:     opts.ignoreFields,
			},
		})
	}

	printfTo(os.Stdout, "Converge barrier: %d resource(s), one shared %s window\n",
		len(targets), time.Duration(float64(opts.pollInterval)*1.5))

	start := time.Now()
	results := runner.RunConvergeAll(targets, opts.concurrency)
	elapsed := time.Since(start)

	summary, ok := runner.FormatConvergeAllSummary(results)
	printfTo(os.Stdout, "%s", summary)
	printfTo(os.Stdout, "barrier wall clock: %s\n", elapsed.Round(time.Millisecond))

	if !ok {
		return errors.New("one or more resources did not converge")
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
