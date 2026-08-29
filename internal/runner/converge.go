package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// Event reasons emitted by the crossplane-runtime managed reconciler on
// every external.Update() call.
const (
	eventReasonUpdated      = "UpdatedExternalResource"
	eventReasonCannotUpdate = "CannotUpdateExternalResource"
)

// Log messages emitted by the crossplane-runtime managed reconciler on every
// external.Update() call, from the SAME call site as the event reasons above:
//
//	log.Debug("Successfully requested update of external resource", ...)
//	record.Event(managed, event.Normal(reasonUpdated, "Successfully requested..."))
//
// Two channels carrying one fact, with opposite failure modes — which is why
// this check reads both rather than picking one.
//
// The event channel is aggregated and rate-limited by client-go. A resource
// updating on every poll tick has its aggregated .count frozen for minutes at
// a time and then flushed in a single jump: measured against a live 10s-poll
// provider, one count sat unchanged across ~13 Update() calls spanning 135s
// before jumping by 15 at once. A convergence window of PollInterval*1.5
// landing inside such a gap observes a delta of zero and reports a resource
// that has never stopped calling Update() as stable. Measured over six
// windows on that provider, the event delta detected the loop in none of
// them.
//
// The log channel is neither aggregated nor rate-limited — one line per call,
// always — and detected the same loop in every window, with no false positive
// on either a healthy resource or the healthy sibling scope of the looping
// one. Its own failure mode is that the reconciler logs this at DEBUG, so a
// provider started without --debug emits nothing; countUpdateLogLinesIn
// reports the instrument's liveness separately so that silence is never read
// as "zero Update() calls".
const (
	logMsgUpdated      = "Successfully requested update of external resource"
	logMsgCannotUpdate = "Cannot update external resource"
)

// EventReasonCreated is the event reason emitted by the crossplane-runtime
// managed reconciler on every external.Create() call. Counting occurrences
// of this reason across a resource's entire lifecycle is the signal that
// distinguishes a genuine identity-resolve RECOVERY from a silent
// DUPLICATE: both outcomes leave the resource Ready with a
// correctly-prefixed external-name (Create derives the backend object type
// from the desired spec, not from how the prior identity was found), but
// only a duplicate ever produces a second CreatedExternalResource event.
// See RunResolveRecover.
//
// It is exported so a caller reporting a ResolveRecoverResult can name the
// event an operator would grep for, without keeping a second copy of the
// string that could drift out of step with the one actually counted.
const EventReasonCreated = "CreatedExternalResource"

// ConvergeOptions configures a convergence check run.
type ConvergeOptions struct {
	// PollInterval is the provider's poll interval; the check waits
	// PollInterval * 1.5 to guarantee at least one full reconcile cycle.
	PollInterval time.Duration
	// IgnoreFields excludes named atProvider fields from the snapshot diff
	// (e.g. server timestamps, rolling counters).
	IgnoreFields []string
	// Timeout bounds the pre-check that waits for generation to settle to
	// observedGeneration.
	Timeout time.Duration
	// ReadinessTimeout bounds the pre-check that waits for the resource's
	// Ready condition to reach "True" before the baseline snapshot is taken.
	// Defaults to the same 120s as Timeout when zero. On timeout the check
	// proceeds to snapshot and diff exactly as it would without this gate —
	// it only narrows the window in which a live, still-settling readiness
	// fact can be captured as the baseline and later misread as drift.
	ReadinessTimeout time.Duration
}

// ConvergeResult holds the outcome of a convergence check.
type ConvergeResult struct {
	Skipped     bool
	SkipMsg     string
	Passed      bool
	Message     string
	Diagnostics []string
}

// RunConverge executes the post-create convergence check: it asserts the
// resource reaches steady state after creation with zero spurious Update
// calls.
//
// Algorithm:
//  1. Resolve the resource from the manifest.
//  2. PRE-CHECK: poll until metadata.generation == status.conditions[].observedGeneration.
//  3. SYNCED GATE: poll until the Synced condition is "True" at the resource's
//     CURRENT generation, bounded by Timeout. Step 2 alone cannot see a
//     reconcile that failed to persist a write — a late-init conflict still
//     stamps observedGeneration == the current generation on the Synced
//     condition it marks False — so "settled" there can already be true on a
//     pass that never succeeded. Unlike step 4, a timeout here FAILS the
//     check with "RESOURCE NOT IN STEADY STATE". A resource whose reconciler
//     emits no Synced condition at all is treated as not applicable and does
//     not block.
//  4. READINESS GATE: poll until the Ready condition is "True", bounded by
//     ReadinessTimeout. A resource still coming up mirrors live readiness
//     facts into atProvider; snapshotting before it settles reads those as
//     drift. On timeout this proceeds anyway (see ConvergeOptions.ReadinessTimeout)
//     rather than replacing a field-level diagnostic with a bare timeout.
//  5. RECORD: snapshot atProvider, generation, and update-event count.
//  6. WAIT: pollInterval * 1.5.
//  7. ASSERT: atProvider unchanged, zero new update events, generation
//     unchanged, and Ready is still "True" (a readiness flap is reported as
//     its own diagnostic, not folded into the atProvider diff).
func (r *Runner) RunConverge(m *manifest.Manifest, opts ConvergeOptions) (*ConvergeResult, error) {
	baseline, early, err := r.convergeArm(m, opts)
	if err != nil {
		return nil, err
	}
	if early != nil {
		return early, nil
	}

	waitDur := convergeWait(opts)
	time.Sleep(waitDur)

	return r.convergeAssert(m, opts, baseline, waitDur)
}

// convergeWait is the observation window: long enough to guarantee at least
// one full reconcile cycle has elapsed since the baseline was taken.
func convergeWait(opts ConvergeOptions) time.Duration {
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 60 * time.Second
	}
	return time.Duration(float64(pollInterval) * 1.5)
}

// convergeBaseline is everything convergeAssert needs to evaluate a
// resource once the observation window has elapsed. ArmedAt is when the
// baseline snapshot was actually taken, which is what a shared window must
// be measured from — see RunConvergeAll. PodIdentity is the provider
// controller Pod convergeArm confirmed had already settled before taking
// this baseline; convergeAssert compares its own read against it to detect
// a restart spoiling the window (see convergeAssertAttempt).
type convergeBaseline struct {
	Snapshot    []byte
	Events      int
	Gen         int64
	Notes       []string
	ArmedAt     time.Time
	PodIdentity controllerPodIdentity
}

// controllerPodSettleThreshold is the minimum age a provider controller Pod
// must have reached before convergeArm may take its baseline snapshot. A
// Pod fresher than this may still be running its own cold-start reconcile
// burst against every resource on the cluster — the same "still coming up"
// fact the readiness gate (see ConvergeOptions.ReadinessTimeout) already
// accounts for on the managed resource, applied here to the process that
// reconciles it.
const controllerPodSettleThreshold = 15 * time.Second

// controllerPodSettleTimeout bounds how long convergeArm waits for the
// provider controller Pod to age past controllerPodSettleThreshold before
// giving up and reporting RESOURCE NOT IN STEADY STATE — the same verdict
// an unsettled generation reports, for the same reason: nothing has been
// observed to repeat yet, so this is not evidence of a loop.
const controllerPodSettleTimeout = 120 * time.Second

// controllerPodSettlePollInterval paces convergeArm's wait for the
// controller Pod to settle. Matches waitGenerationSettled/waitReady's own
// 2-second cadence rather than inventing a second one.
const controllerPodSettlePollInterval = 2 * time.Second

// convergeMaxRestartRetries bounds how many times convergeAssertAttempt
// will re-arm and re-measure a resource whose provider controller Pod
// identity changed between arm and assert, before giving up and reporting
// an explicit inconclusive verdict rather than ever silently passing. Two
// tolerates one burst-reset restart landing right at the edge of the
// observation window plus one more from an unrelated concurrent cause,
// without masking a controller that is being restarted continuously by
// something else entirely.
const convergeMaxRestartRetries = 2

// waitControllerPodSettled polls resolveControllerPodIdentity until the Pod
// it reports is at least threshold old, or timeout elapses. It mirrors
// waitGenerationSettled/waitReady's bounded-poll shape exactly.
func (r *Runner) waitControllerPodSettled(threshold, timeout time.Duration) (identity controllerPodIdentity, settled bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		identity, err = r.resolveControllerPodIdentity()
		if err != nil {
			return identity, false, err
		}
		if time.Since(identity.CreatedAt) >= threshold {
			return identity, true, nil
		}
		if time.Now().After(deadline) {
			return identity, false, nil
		}
		r.sleep(controllerPodSettlePollInterval)
	}
}

// podSettleParams resolves the effective settle threshold and timeout,
// falling back to the package constants when a test has not overridden
// Runner.podSettleThreshold / podSettleTimeout.
func (r *Runner) podSettleParams() (threshold, timeout time.Duration) {
	threshold = r.podSettleThreshold
	if threshold <= 0 {
		threshold = controllerPodSettleThreshold
	}
	timeout = r.podSettleTimeout
	if timeout <= 0 {
		timeout = controllerPodSettleTimeout
	}
	return threshold, timeout
}

// convergeArm runs every step that must complete BEFORE the observation
// window opens: the skip check, resource resolution, the generation-settle
// and readiness gates, and the baseline snapshot.
//
// It returns either a baseline (window may open) or an early ConvergeResult
// that is already the final verdict (converge-skip, or a generation that
// never settled). Exactly one of the two is non-nil.
//
// This is split out of RunConverge so that the single-resource path and the
// barrier path in RunConvergeAll share one implementation. The steps are
// all reads: nothing here mutates the cluster or the backend, which is what
// makes it safe to arm many resources concurrently.
func (r *Runner) convergeArm(m *manifest.Manifest, opts ConvergeOptions) (*convergeBaseline, *ConvergeResult, error) {
	if m.ConvergeSkip != "" {
		return nil, &ConvergeResult{Skipped: true, SkipMsg: m.ConvergeSkip}, nil
	}

	if err := r.ResolveResource(m); err != nil {
		return nil, nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	settled, gen, obsGen, err := r.waitGenerationSettled(timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("pre-check: %w", err)
	}
	if !settled {
		// Not a reconciliation loop: nothing has been observed to change
		// repeatedly, the resource has simply never reached a settled
		// generation within the timeout. See buildConvergeResult for the
		// equivalent distinction on the post-wait path.
		return nil, &ConvergeResult{
			Passed:  false,
			Message: "RESOURCE NOT IN STEADY STATE",
			Diagnostics: []string{
				fmt.Sprintf("pre-check: generation (%d) did not settle to observedGeneration (%d) within %s", gen, obsGen, timeout),
			},
		}, nil
	}

	// waitGenerationSettled alone has a blind spot this closes: a reconcile
	// that FAILS to persist a write (a late-init 409 conflict, for one)
	// still stamps observedGeneration == the current generation on the
	// Synced condition it marks False (ReconcileError) — so "settled"
	// above can already be true on a pass that never actually succeeded.
	// waitSynced additionally requires that condition's STATUS to be
	// True, not merely its observedGeneration to match, closing the same
	// gap here that reconcileOnce closes for the per-field update path.
	synced, syncedStatus, syncedGen, syncedObsGen, err := r.waitSynced(timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("pre-check: %w", err)
	}
	if !synced {
		return nil, &ConvergeResult{
			Passed:  false,
			Message: "RESOURCE NOT IN STEADY STATE",
			Diagnostics: []string{
				fmt.Sprintf("pre-check: Synced condition did not reach True at generation %d within %s (last seen %q at observedGeneration %d)",
					syncedGen, timeout, syncedStatus, syncedObsGen),
			},
		}, nil
	}
	// waitSynced can itself observe the generation advance past what
	// waitGenerationSettled saw (a late-init write landing while THIS
	// call polled) — rebase gen to that fresher value so the final
	// "generation changed" comparison against afterGen (in
	// buildConvergeResult) is measured from the generation the baseline
	// snapshot below is actually about to be taken at, not from a value
	// this same pre-check has since superseded.
	gen = syncedGen

	readinessTimeout := opts.ReadinessTimeout
	if readinessTimeout <= 0 {
		readinessTimeout = 120 * time.Second
	}

	var notes []string
	baselineReady, err := r.waitReady(readinessTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("readiness pre-check: %w", err)
	}
	if !baselineReady {
		// Proceed anyway: the field-level diagnostic below is never
		// discarded in favour of this note, only supplemented by it.
		notes = append(notes, fmt.Sprintf(
			"readiness pre-check: Ready condition did not reach True within %s before the baseline snapshot; proceeding with field-level diagnostics",
			readinessTimeout))
	}

	// Refuse to take the baseline while the provider controller Pod is
	// still within its own cold-start window: a Pod that has just been
	// (re)created may still be running the reconcile burst every
	// controller performs against its whole watch set on start-up, and a
	// baseline taken mid-burst would misread that burst as drift the
	// instant it lands. Unlike the readiness gate above, a timeout here
	// DOES fail the check — see waitControllerPodSettled's caller below.
	podSettleThreshold, podSettleTimeout := r.podSettleParams()
	podIdentity, podSettled, err := r.waitControllerPodSettled(podSettleThreshold, podSettleTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("provider controller pod pre-check: %w", err)
	}
	if !podSettled {
		// Not a reconciliation loop: this is the same distinction
		// waitGenerationSettled's timeout draws above — nothing has been
		// observed to repeat, the controller Pod has simply never aged
		// past the settle threshold within the timeout (most likely a
		// crash-loop on the controller itself, or a burst reset landing
		// back to back with another restart).
		return nil, &ConvergeResult{
			Passed:  false,
			Message: "RESOURCE NOT IN STEADY STATE",
			Diagnostics: []string{
				fmt.Sprintf("pre-check: provider controller pod %q is %s old, younger than the %s settle threshold, within %s",
					podIdentity.Name, time.Since(podIdentity.CreatedAt).Round(time.Second), podSettleThreshold, podSettleTimeout),
			},
		}, nil
	}

	before, beforeEvents, err := r.recordConvergeBaseline(m)
	if err != nil {
		return nil, nil, err
	}

	return &convergeBaseline{
		Snapshot:    before,
		Events:      beforeEvents,
		Gen:         gen,
		Notes:       notes,
		ArmedAt:     time.Now(),
		PodIdentity: podIdentity,
	}, nil, nil
}

// convergeAssert runs every step that must happen AFTER the observation
// window has elapsed: the provider-controller-Pod-identity check, the
// post-wait snapshot, the diff, and the verdict.
func (r *Runner) convergeAssert(m *manifest.Manifest, opts ConvergeOptions, b *convergeBaseline, waitDur time.Duration) (*ConvergeResult, error) {
	return r.convergeAssertAttempt(m, opts, b, waitDur, 0)
}

// convergeAssertAttempt is convergeAssert's actual body, carrying an
// attempt counter so it can re-arm and re-measure a resource whose
// provider controller Pod was replaced during the observation window,
// bounded by convergeMaxRestartRetries.
//
// Detection is a single identity read compared against the one convergeArm
// recorded in the baseline — both reads go through
// resolveControllerPodIdentity, which is derived from the cluster
// (resolveControllerDeploymentName plus a Pod list) rather than from any
// state carried by this process. That matters because the restart this
// exists to catch is typically issued by a DIFFERENT process invocation
// entirely — the "run" subcommand's resetEventBurst, executed well before
// "converge-all" ever starts — so an in-memory restart timestamp would be
// structurally unavailable here even if one existed. Reading the cluster's
// own state also means this check catches a restart from ANY cause —
// resetEventBurst, an OOM kill, an operator action, a package re-install —
// rather than only the one this tool itself issues.
//
// A restart is a legitimate full re-reconcile, not evidence of drift, so
// the spoiled window is discarded and re-measured rather than having its
// restart-caused Update() calls subtracted out: subtraction would require
// guessing which calls were caused by the restart, and converge is
// read-only, so re-measuring costs one more short window rather than a
// live run.
func (r *Runner) convergeAssertAttempt(m *manifest.Manifest, opts ConvergeOptions, b *convergeBaseline, waitDur time.Duration, restarts int) (*ConvergeResult, error) {
	if b.PodIdentity.Name != "" {
		current, err := r.resolveControllerPodIdentity()
		if err != nil {
			return nil, fmt.Errorf("provider controller pod identity check: %w", err)
		}
		if current.Name != b.PodIdentity.Name {
			if restarts >= convergeMaxRestartRetries {
				// Never silently pass: a resource whose window was spoiled
				// on every attempt is not a resource that converged, and
				// reporting RECONCILIATION LOOP DETECTED here would blame
				// the controller for exactly the restarts this check exists
				// to look past.
				return &ConvergeResult{
					Passed:  false,
					Message: "CONVERGENCE INCONCLUSIVE",
					Diagnostics: []string{
						fmt.Sprintf("provider controller pod restarted %d time(s) during observation (last baseline pod %q, now %q) — the observation window was spoiled on every attempt, so no pass/fail verdict can be reported",
							restarts+1, b.PodIdentity.Name, current.Name),
					},
				}, nil
			}

			newBaseline, early, err := r.convergeArm(m, opts)
			if err != nil {
				return nil, err
			}
			if early != nil {
				return early, nil
			}
			// Re-armed and re-measuring instead of returning INCONCLUSIVE
			// (the branch above): note the restart on the fresh baseline so
			// it survives into the eventual verdict — buildConvergeResult
			// surfaces notes whether the retry ultimately passes or fails,
			// which is what lets an operator (or this project's own live
			// runs) confirm the re-arm actually fired instead of inferring
			// it from the absence of a false RECONCILIATION LOOP DETECTED.
			// b.Notes is carried forward first so a SECOND restart within
			// convergeMaxRestartRetries keeps the first restart's note
			// rather than the fresh convergeArm call silently replacing it.
			restartNote := fmt.Sprintf(
				"provider controller pod restarted during observation (attempt %d: baseline pod %q, now %q) — window discarded and re-measured",
				restarts+1, b.PodIdentity.Name, current.Name)
			newBaseline.Notes = append(append(append([]string{}, b.Notes...), newBaseline.Notes...), restartNote)
			freshWait := convergeWait(opts)
			time.Sleep(freshWait)
			return r.convergeAssertAttempt(m, opts, newBaseline, freshWait, restarts+1)
		}
	}

	after, afterEvents, afterGen, afterReady, err := r.recordConvergeOutcome(m)
	if err != nil {
		return nil, err
	}

	// Read the controller's own log for the window just waited out. A failure
	// here is carried into the verdict rather than returned: an unreadable log
	// costs this check its most reliable instrument, which the operator must
	// be told about, but it is not itself evidence about the resource.
	logCalls, logLines, logErr := r.countUpdateLogCalls(m, waitDur)
	logObs := updateLogObservation{Calls: logCalls, Lines: logLines, Err: logErr, Window: waitDur}

	diff, err := differ.DiffSnapshotsExcluding(b.Snapshot, after, opts.IgnoreFields)
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}

	return buildConvergeResult(diff, b.Gen, afterGen, b.Events, afterEvents, afterReady, logObs, b.Notes), nil
}

// recordConvergeBaseline snapshots atProvider and the update-event count
// before the convergence wait begins.
func (r *Runner) recordConvergeBaseline(m *manifest.Manifest) (snapshot []byte, events int, err error) {
	snapshot, err = r.Snapshot()
	if err != nil {
		return nil, 0, fmt.Errorf("recording snapshot: %w", err)
	}
	events, err = r.countUpdateEvents(m.Kind, m.Name, m.Namespace, m.APIVersion)
	if err != nil {
		return nil, 0, fmt.Errorf("counting events: %w", err)
	}
	return snapshot, events, nil
}

// recordConvergeOutcome snapshots atProvider, the update-event count, the
// generation, and the Ready condition after the convergence wait completes.
// Generation and readiness are both read from a single decoded object so
// the two facts describe the exact same read of the resource.
func (r *Runner) recordConvergeOutcome(m *manifest.Manifest) (snapshot []byte, events int, gen int64, ready bool, err error) {
	snapshot, err = r.Snapshot()
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("post-wait snapshot: %w", err)
	}
	events, err = r.countUpdateEvents(m.Kind, m.Name, m.Namespace, m.APIVersion)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("counting events: %w", err)
	}
	obj, err := r.GetObject()
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("reading resource: %w", err)
	}
	gen, err = extractGeneration(obj)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("reading generation: %w", err)
	}
	ready = isReadyTrue(obj)
	return snapshot, events, gen, ready, nil
}

// buildConvergeResult evaluates the pre/post snapshots, event counts,
// controller-log observation, generations, and final readiness, and assembles
// the final ConvergeResult.
// notes are informational diagnostics (e.g. the baseline readiness-timeout
// note) that are always surfaced, whether or not they change the verdict —
// they must never be lost behind a field-level diagnostic, and must never
// silently replace one either.
func buildConvergeResult(diff []differ.FieldChange, gen, afterGen int64, beforeEvents, afterEvents int, afterReady bool, logObs updateLogObservation, notes []string) *ConvergeResult {
	var problems []string
	passed := true
	// loopSignal is true only for the failure modes that are actually
	// evidence of the controller repeatedly writing something: the
	// atProvider snapshot moved, a new update event fired, or the
	// controller's own log records Update() calls it made. A readiness
	// flap or a bare generation change are signs the resource has not
	// settled, not signs of a loop, so they must never earn the headline
	// on their own.
	loopSignal := false

	if len(diff) > 0 {
		passed = false
		loopSignal = true
		problems = append(problems, fmt.Sprintf("atProvider changed: %s", differ.FormatChanges(diff)))
	}
	if afterEvents > beforeEvents {
		passed = false
		loopSignal = true
		problems = append(problems, fmt.Sprintf("%d new update event(s) observed (%s/%s)",
			afterEvents-beforeEvents, eventReasonUpdated, eventReasonCannotUpdate))
	}
	// The controller-log instrument. It is read in addition to the event
	// delta above, never instead of it: the two channels fail in opposite
	// directions (see logMsgUpdated), and OR-ing them can only ever add a
	// signal, never suppress one. Neither is redundant — the event delta is
	// the only instrument left when a provider runs without --debug, and the
	// log is the only one that survives client-go's rate limiter.
	switch {
	case logObs.Err != nil:
		notes = append(notes, fmt.Sprintf(
			"controller-log instrument unavailable (%v); loop detection fell back to the update-event delta alone, which client-go rate-limits and which under-reports a resource looping at poll cadence",
			logObs.Err))
	case logObs.Window > 0 && logObs.Lines == 0:
		// Told apart from "zero Update() calls" deliberately: the reconciler
		// writes these lines at DEBUG, so an empty window means the
		// instrument saw nothing at all rather than seeing a quiet
		// controller. Reporting that as a clean pass is the exact silence
		// this check was added to end.
		notes = append(notes, fmt.Sprintf(
			"controller log returned no lines over the %s window — the log instrument observed nothing rather than observing zero Update() calls; check the provider is running with --debug",
			logObs.Window))
	case logObs.Calls > 0:
		passed = false
		loopSignal = true
		problems = append(problems, fmt.Sprintf(
			"%d Update() call(s) in the controller log over %s",
			logObs.Calls, logObs.Window))
	}
	if afterGen != gen {
		passed = false
		problems = append(problems, fmt.Sprintf("generation changed: %d → %d", gen, afterGen))
	}
	if !afterReady {
		// Its own named diagnostic, deliberately kept out of the atProvider
		// diff list above: a readiness flap is a distinct failure mode from
		// a field drifting, even though both are reported through the same
		// Diagnostics slice.
		passed = false
		problems = append(problems, "readiness flap: Ready condition was not True at the final snapshot")
	}

	diagnostics := append(append([]string{}, notes...), problems...)

	result := &ConvergeResult{Passed: passed}
	switch {
	case passed:
		result.Message = fmt.Sprintf("resource stable (1 cycle observed, %d updates)", afterEvents-beforeEvents)
	case loopSignal:
		// A genuine atProvider drift, update-event delta, or Update() call
		// recorded in the controller's own log: this is what operators and
		// past tickets search for, so the string stays verbatim regardless
		// of what else also failed alongside it.
		result.Message = "RECONCILIATION LOOP DETECTED"
	default:
		// The only problems are a readiness flap and/or an unsettled
		// generation — real, but not a loop: nothing was observed
		// repeating.
		result.Message = "RESOURCE NOT IN STEADY STATE"
	}
	if len(diagnostics) > 0 {
		result.Diagnostics = diagnostics
	}
	return result
}

// waitGenerationSettled polls the resource until metadata.generation equals
// the minimum observedGeneration across status.conditions, or timeout
// elapses. crossplane-runtime's ObservedGenerationPropagationManager sets
// every condition's observedGeneration to metadata.generation whenever any
// condition is (re)computed, so once the controller has fully reconciled,
// all conditions carry the same, current generation.
func (r *Runner) waitGenerationSettled(timeout time.Duration) (settled bool, gen int64, obsGen int64, err error) {
	deadline := time.Now().Add(timeout)
	for {
		obj, gerr := r.GetObject()
		if gerr != nil {
			return false, 0, 0, gerr
		}
		g, gerr := extractGeneration(obj)
		if gerr != nil {
			return false, 0, 0, gerr
		}
		gen = g
		if og, found := extractObservedGeneration(obj); found {
			obsGen = og
			if gen == obsGen {
				return true, gen, obsGen, nil
			}
		}
		if time.Now().After(deadline) {
			return false, gen, obsGen, nil
		}
		r.sleep(2 * time.Second)
	}
}

// waitReady polls the resource until its status.conditions carries a Ready
// condition with status "True", or timeout elapses. It mirrors
// waitGenerationSettled's bounded-poll shape exactly — same deadline
// pattern, same 2s cadence — rather than inventing a second waiting idiom.
// Unlike waitGenerationSettled, a timeout here is not itself a failure: the
// caller (RunConverge) decides what to do with ready=false, since the whole
// point of this gate is to never let a readiness timeout replace a
// field-level converge diagnostic.
func (r *Runner) waitReady(timeout time.Duration) (ready bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		obj, gerr := r.GetObject()
		if gerr != nil {
			return false, gerr
		}
		if isReadyTrue(obj) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		r.sleep(2 * time.Second)
	}
}

// namedCondition reports one status.conditions entry by type: its status
// string ("True"/"False"/"Unknown") and observedGeneration. found is false
// when status.conditions is absent, empty, or carries no entry of condType
// at all. observedGeneration is -1 when the entry is present but its
// observedGeneration is missing or unparsable, which can never equal a
// real generation (generations start at 1), so a caller comparing it
// against metadata.generation always (correctly) treats that entry as
// stale rather than accidentally matching on a zero value.
func namedCondition(obj map[string]interface{}, condType string) (status string, observedGeneration int64, found bool) {
	statusMap, ok := obj[jsonKeyStatus].(map[string]interface{})
	if !ok {
		return "", 0, false
	}
	condsRaw, ok := statusMap["conditions"].([]interface{})
	if !ok {
		return "", 0, false
	}
	for _, cRaw := range condsRaw {
		c, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t != condType {
			continue
		}
		s, _ := c["status"].(string)
		og, ogErr := toInt64(c["observedGeneration"])
		if ogErr != nil {
			og = -1
		}
		return s, og, true
	}
	return "", 0, false
}

// conditionTypeSynced is the crossplane-runtime managed-reconciler
// condition type that reports whether the MOST RECENT reconcile
// successfully persisted the desired state (ReconcileSuccess) or failed
// trying to (ReconcileError). It is a different condition TYPE from
// Ready — Ready reports external-resource availability, set by Observe()
// on every successful GET regardless of whether that same pass went on to
// write anything successfully. A resource can therefore read Ready=True
// while Synced=False mid a late-init conflict-and-retry: WaitReady alone
// cannot see that, because it only ever asks about Ready.
const conditionTypeSynced = "Synced"

// waitSynced polls the resource until its Synced condition reads "True" AT
// THE RESOURCE'S CURRENT metadata.generation — not merely that a Synced
// condition is present, and not merely that it is "True" at some
// generation. Both distinctions matter for the same underlying race: a
// late-init spec write that conflicts and retries leaves Synced "False"
// (ReconcileError) at the CURRENT generation on the reconcile that failed
// to persist it; a late-init write that then succeeds bumps the
// generation, but the reconcile that persisted it can return before
// re-marking Synced, leaving a "False" (or even a stale "True") Synced
// condition whose observedGeneration lags the object's new generation.
// Only the reconcile the watch auto-triggers off that generation bump
// finally reaches the point where the reconciler evaluates the desired
// state against the external resource and marks Synced "True" at the
// generation that actually matters. Polling here (rather than a second
// `kubectl wait`) is what makes the generation comparison possible in the
// first place — kubectl's own condition wait has no way to also pin the
// generation the condition must have been computed against.
//
// A resource that never carries a Synced condition at all is not itself
// evidence of a problem — this package has no dependency on any specific
// reconciler's condition set, and every production crossplane-runtime
// managed reconciler emits one on essentially every completed reconcile
// (Ready and Synced are written together in one deferred status update).
// So an ABSENT Synced condition, checked once on the first read, is read
// as "this reconciler does not emit one": synced returns true immediately
// rather than blocking out the full timeout for a signal that will never
// arrive.
//
// The (bool, ..., error) shape mirrors waitGenerationSettled and waitReady
// deliberately: err is reserved for a genuine kubectl/parse failure, and a
// plain timeout — the resource is still catching up — is reported as
// synced=false with no error, so each caller decides for itself whether
// "still catching up" should fail outright (reconcileOnce, mid a field
// test) or degrade to a diagnostic (RunConverge's pre-check, which never
// bare-errors on a timeout).
func (r *Runner) waitSynced(timeout time.Duration) (synced bool, status string, gen, obsGen int64, err error) {
	deadline := time.Now().Add(timeout)
	for {
		obj, gerr := r.GetObject()
		if gerr != nil {
			return false, "", 0, 0, gerr
		}
		g, gerr := extractGeneration(obj)
		if gerr != nil {
			return false, "", 0, 0, gerr
		}
		gen = g
		s, og, found := namedCondition(obj, conditionTypeSynced)
		if !found {
			return true, "", gen, 0, nil
		}
		status, obsGen = s, og
		if status == "True" && obsGen == gen {
			return true, status, gen, obsGen, nil
		}
		if time.Now().After(deadline) {
			return false, status, gen, obsGen, nil
		}
		r.sleep(2 * time.Second)
	}
}

// countUpdateEvents counts occurrences of update-related events for the
// given involvedObject kind/name/namespace/apiVersion, whose reason is
// UpdatedExternalResource or CannotUpdateExternalResource. The kubectl list
// itself queries across all namespaces (see countEventsByReason); namespace
// and apiVersion are what actually scope the count to one resource. Pass
// namespace="" for a cluster-scoped resource — that matches only events
// whose involvedObject itself carries no namespace, never "any namespace".
//
// client-go's event recorder aggregates repeated identical events onto a
// single Item by incrementing that Item's .count field rather than
// appending a new Item, so the number of matching Items is NOT the number
// of occurrences. This function sums each matching Item's .count instead
// of counting Items (see sumEventOccurrences).
func (r *Runner) countUpdateEvents(kind, name, namespace, apiVersion string) (int, error) {
	return r.countEventsByReason(kind, name, namespace, apiVersion, eventReasonUpdated, eventReasonCannotUpdate)
}

// countCreateEvents counts occurrences of CreatedExternalResource events for
// the given involvedObject kind/name/namespace/apiVersion, across the
// resource's ENTIRE lifecycle (not a before/after delta like
// countUpdateEvents). Exactly one occurrence is the expected outcome for a
// resource that has been created once and recovered its identity via search
// (rather than a stored ref) without duplicating — see RunResolveRecover.
func (r *Runner) countCreateEvents(kind, name, namespace, apiVersion string) (int, error) {
	return r.countEventsByReason(kind, name, namespace, apiVersion, EventReasonCreated)
}

// updateLogObservation is what the controller-log instrument saw across one
// convergence window. Calls is the number of Update() calls attributed to the
// resource under test; Lines is every line the window returned, whatever it
// said, and exists so that "saw nothing" can be told apart from "saw zero
// Update() calls"; Err records that the instrument could not be read at all.
// A zero Window means the observation was never taken.
type updateLogObservation struct {
	Calls  int
	Lines  int
	Err    error
	Window time.Duration
}

// reconcileRequest is the controller-runtime reconcile.Request embedded in
// every structured log line the managed reconciler writes.
type reconcileRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// updateLogWindowSlack widens the log query past the convergence window,
// because kubectl's --since takes whole seconds and a line written a fraction
// before the window opened would otherwise be lost to rounding. Over-reading
// is safe in the direction that matters: this check fails on a non-zero count,
// so a slightly wide window can only make a genuine loop easier to see, never
// manufacture one on a resource that made no calls at all.
const updateLogWindowSlack = time.Second

// countUpdateLogCalls reads the provider controller's own log across the
// convergence window and reports how many Update() calls it made for this
// resource — the loop signal that the event delta rate-limits away (see
// logMsgUpdated).
func (r *Runner) countUpdateLogCalls(m *manifest.Manifest, window time.Duration) (calls, lines int, err error) {
	since := int((window + updateLogWindowSlack).Seconds())
	if since < 1 {
		since = 1
	}
	// --tail=-1 is NOT redundant. kubectl defaults --tail to 10 whenever a
	// SELECTOR is used, and to -1 only for a single named pod — so the
	// obvious `logs -l ... --since=Ns` returns the last ten lines of the
	// controller's output rather than the window, silently and with a
	// zero exit. Measured against a live looping resource, that default
	// alone turned detection in every window into detection in two
	// windows out of three, because the ten most recent lines are mostly
	// other resources' reconcile chatter. ProviderLogs always sends
	// --tail=-1 for exactly this reason.
	out, err := r.kube().ProviderLogs(providerDeploymentNamespace, providerDeploymentSelector,
		fmt.Sprintf("%ds", since))
	if err != nil {
		return 0, 0, fmt.Errorf("reading controller log: %w", err)
	}
	calls, lines = countUpdateLogLinesIn(out, m.Name, m.Namespace)
	return calls, lines, nil
}

// countUpdateLogLinesIn attributes controller log lines to one resource.
//
// A line counts as an Update() call when it carries one of the reconciler's
// Update() messages AND its structured payload's reconcile request names
// exactly this resource. Attribution is by parsed request rather than by
// substring: the unified example-manifest convention every dual-scope
// provider follows lets a resource's cluster-scoped and namespaced variants
// share a Kind and a Name, differing only in whether the request carries a
// namespace — so a substring match on the name would credit the namespaced
// sibling's loop to the cluster-scoped resource, the same cross-scope
// confusion sumEventOccurrencesByReason exists to prevent on the event side.
// A cluster-scoped resource's namespace is the empty string and matches only
// a request carrying no namespace at all.
//
// lines counts every line the window returned, whatever it said. It is the
// instrument's liveness proxy: the reconciler writes these at DEBUG, so a
// provider started without --debug returns nothing, and zero calls out of
// zero lines means "could not see" rather than "did not happen".
func countUpdateLogLinesIn(out, name, namespace string) (calls, lines int) {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines++
		if !strings.Contains(line, logMsgUpdated) && !strings.Contains(line, logMsgCannotUpdate) {
			continue
		}
		req, ok := reconcileRequestOf(line)
		if !ok {
			continue
		}
		if req.Name == name && req.Namespace == namespace {
			calls++
		}
	}
	return calls, lines
}

// reconcileRequestOf decodes the reconcile request out of a structured log
// line, whose JSON payload begins at the line's first '{'. A line that
// carries no payload, or one that does not name a request, is reported as
// unattributable rather than guessed at.
func reconcileRequestOf(line string) (reconcileRequest, bool) {
	i := strings.IndexByte(line, '{')
	if i < 0 {
		return reconcileRequest{}, false
	}
	var payload struct {
		Request reconcileRequest `json:"request"`
	}
	if err := json.Unmarshal([]byte(line[i:]), &payload); err != nil {
		return reconcileRequest{}, false
	}
	if payload.Request.Name == "" {
		return reconcileRequest{}, false
	}
	return payload.Request, true
}

// countEventsByReason lists cluster events and sums the aggregated
// occurrence count of every event matching the given involvedObject
// kind/name/namespace/apiVersion and any of the given reasons. The
// underlying KubeClient query is narrowed server-side by a field selector
// on involvedObject kind/name/namespace (see ListEventsForObject), but
// still spans every namespace: cluster-scoped managed resources may have
// their events recorded outside the resource's own namespace, so the
// selector's namespace term matches the TARGET's namespace, never the
// Event object's own. sumEventOccurrencesByReason re-verifies namespace and
// apiVersion on every returned Item regardless, as a client-side backstop.
func (r *Runner) countEventsByReason(kind, name, namespace, apiVersion string, reasons ...string) (int, error) {
	out, err := r.kube().ListEventsForObject(kind, name, namespace)
	if err != nil {
		return 0, fmt.Errorf("listing events: %w", err)
	}

	var list eventList
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return 0, fmt.Errorf("parsing events JSON: %w", err)
	}

	return sumEventOccurrencesByReason(list, kind, name, namespace, apiVersion, reasons...), nil
}

// eventList mirrors the subset of a `kubectl get events -o json` response
// needed to count update-related event occurrences.
type eventList struct {
	Items []eventItem `json:"items"`
}

// eventItem mirrors a single Kubernetes Event. Count carries the number of
// times an aggregated event recurred; it is only populated for aggregated
// events (client-go's event recorder increments it in place instead of
// appending a new Item for a repeated identical event). A zero Count means
// the event was recorded exactly once and is treated as 1 occurrence.
//
// Namespace and APIVersion, alongside Kind and Name, are what let
// sumEventOccurrencesByReason tell apart a cluster-scoped resource and a
// namespaced resource that share the same Kind and Name — the unified
// example-manifest convention every dual-scope provider follows. Namespace
// is empty for a cluster-scoped involvedObject. APIVersion may legitimately
// be empty: not every event source populates it, and sumEventOccurrencesByReason
// treats that as "group unknown", not "group empty" — see its comment.
type eventItem struct {
	Reason         string `json:"reason"`
	Count          int32  `json:"count"`
	InvolvedObject struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		APIVersion string `json:"apiVersion"`
	} `json:"involvedObject"`
}

// sumEventOccurrences sums the aggregated .count field of every event Item
// matching the given involvedObject kind/name/namespace/apiVersion and an
// update-related reason, treating a zero .count as a single (non-aggregated)
// occurrence.
func sumEventOccurrences(list eventList, kind, name, namespace, apiVersion string) int {
	return sumEventOccurrencesByReason(list, kind, name, namespace, apiVersion, eventReasonUpdated, eventReasonCannotUpdate)
}

// apiGroup returns the group portion of an apiVersion string
// ("group/version" → "group"). A core-group value with no slash (e.g. "v1")
// has no group and returns "".
func apiGroup(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// sumEventOccurrencesByReason sums the aggregated .count field of every
// event Item matching the given involvedObject kind/name/namespace/
// apiVersion and any of the given reasons, treating a zero .count as a
// single (non-aggregated) occurrence.
//
// Matching namespace, in addition to kind and name, is what keeps a
// cluster-scoped resource and a namespaced resource of the same Kind+Name
// from being counted together: the unified example-manifest convention
// every dual-scope provider follows names both variants identically,
// differing only in involvedObject.namespace and involvedObject.apiVersion's
// GROUP. A cluster-scoped resource's namespace argument must be the empty
// string, which matches only events whose involvedObject itself carries an
// empty namespace — never "any namespace".
//
// apiVersion is compared by GROUP only, not the full string, and only when
// the event actually carries one:
//
//   - Comparing groups rather than the full apiVersion tolerates a version
//     skew between the manifest and the served object (a CRD version bump, a
//     conversion webhook) without silently zeroing the count for a resource
//     that is otherwise the right one. It is also exactly the discriminator
//     a dual-scope provider's two variants actually differ on — e.g.
//     "widget.crossplane.io" vs "widget.m.crossplane.io" — never the version.
//   - An event whose involvedObject carries an EMPTY apiVersion is treated as
//     "group unknown", not "group empty", and is never excluded on that
//     basis: not every event source populates the field, and excluding it
//     would silently zero a genuinely looping resource's count on any such
//     source. Namespace has already narrowed the match to one scope by this
//     point, so letting an unknown group through cannot reintroduce a
//     cross-scope false match — it only ever widens a match that is already
//     namespace-correct.
func sumEventOccurrencesByReason(list eventList, kind, name, namespace, apiVersion string, reasons ...string) int {
	want := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		want[reason] = true
	}
	wantGroup := apiGroup(apiVersion)

	var total int32
	for _, it := range list.Items {
		if it.InvolvedObject.Kind != kind || it.InvolvedObject.Name != name {
			continue
		}
		if it.InvolvedObject.Namespace != namespace {
			continue
		}
		if raw := it.InvolvedObject.APIVersion; raw != "" && apiGroup(raw) != wantGroup {
			continue
		}
		if !want[it.Reason] {
			continue
		}
		n := it.Count
		if n <= 0 {
			n = 1
		}
		total += n
	}
	return int(total)
}

// toInt64 converts a decoded-JSON numeric value (float64 from
// encoding/json, or occasionally int/int64) to int64.
func toInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("unexpected numeric type %T", v)
	}
}

// extractGeneration reads metadata.generation from a decoded resource object.
func extractGeneration(obj map[string]interface{}) (int64, error) {
	md, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("metadata not found or not an object")
	}
	genRaw, ok := md["generation"]
	if !ok {
		return 0, fmt.Errorf("metadata.generation not found")
	}
	return toInt64(genRaw)
}

// isReadyTrue reports whether a decoded resource object's status.conditions
// carries a condition of type "Ready" whose status is "True". A missing
// status, missing conditions, or a Ready condition with any other status
// (or no Ready condition at all) all report false — a resource that has not
// been reconciled yet is simply not ready, not an error.
func isReadyTrue(obj map[string]interface{}) bool {
	status, ok := obj[jsonKeyStatus].(map[string]interface{})
	if !ok {
		return false
	}
	condsRaw, ok := status["conditions"].([]interface{})
	if !ok {
		return false
	}
	for _, cRaw := range condsRaw {
		c, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t != "Ready" {
			continue
		}
		s, _ := c["status"].(string)
		return s == "True"
	}
	return false
}

// extractObservedGeneration reads the minimum observedGeneration across all
// status.conditions entries. Returns found=false if status.conditions is
// absent, empty, or none of its entries carry observedGeneration (e.g. the
// resource has not been reconciled yet).
func extractObservedGeneration(obj map[string]interface{}) (min int64, found bool) {
	status, ok := obj[jsonKeyStatus].(map[string]interface{})
	if !ok {
		return 0, false
	}
	condsRaw, ok := status["conditions"].([]interface{})
	if !ok || len(condsRaw) == 0 {
		return 0, false
	}

	min = -1
	for _, cRaw := range condsRaw {
		c, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		ogRaw, ok := c["observedGeneration"]
		if !ok {
			continue
		}
		og, err := toInt64(ogRaw)
		if err != nil {
			continue
		}
		if min == -1 || og < min {
			min = og
		}
	}
	if min == -1 {
		return 0, false
	}
	return min, true
}
