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
//  3. READINESS GATE: poll until the Ready condition is "True", bounded by
//     ReadinessTimeout. A resource still coming up mirrors live readiness
//     facts into atProvider; snapshotting before it settles reads those as
//     drift. On timeout this proceeds anyway (see ConvergeOptions.ReadinessTimeout)
//     rather than replacing a field-level diagnostic with a bare timeout.
//  4. RECORD: snapshot atProvider, generation, and update-event count.
//  5. WAIT: pollInterval * 1.5.
//  6. ASSERT: atProvider unchanged, zero new update events, generation
//     unchanged, and Ready is still "True" (a readiness flap is reported as
//     its own diagnostic, not folded into the atProvider diff).
func (r *Runner) RunConverge(m *manifest.Manifest, opts ConvergeOptions) (*ConvergeResult, error) {
	if m.ConvergeSkip != "" {
		return &ConvergeResult{Skipped: true, SkipMsg: m.ConvergeSkip}, nil
	}

	if err := r.ResolveResource(m); err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	settled, gen, obsGen, err := r.waitGenerationSettled(timeout)
	if err != nil {
		return nil, fmt.Errorf("pre-check: %w", err)
	}
	if !settled {
		// Not a reconciliation loop: nothing has been observed to change
		// repeatedly, the resource has simply never reached a settled
		// generation within the timeout. See buildConvergeResult for the
		// equivalent distinction on the post-wait path.
		return &ConvergeResult{
			Passed:  false,
			Message: "RESOURCE NOT IN STEADY STATE",
			Diagnostics: []string{
				fmt.Sprintf("pre-check: generation (%d) did not settle to observedGeneration (%d) within %s", gen, obsGen, timeout),
			},
		}, nil
	}

	readinessTimeout := opts.ReadinessTimeout
	if readinessTimeout <= 0 {
		readinessTimeout = 120 * time.Second
	}

	var notes []string
	baselineReady, err := r.waitReady(readinessTimeout)
	if err != nil {
		return nil, fmt.Errorf("readiness pre-check: %w", err)
	}
	if !baselineReady {
		// Proceed anyway: the field-level diagnostic below is never
		// discarded in favour of this note, only supplemented by it.
		notes = append(notes, fmt.Sprintf(
			"readiness pre-check: Ready condition did not reach True within %s before the baseline snapshot; proceeding with field-level diagnostics",
			readinessTimeout))
	}

	before, beforeEvents, err := r.recordConvergeBaseline(m)
	if err != nil {
		return nil, err
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 60 * time.Second
	}
	waitDur := time.Duration(float64(pollInterval) * 1.5)
	time.Sleep(waitDur)

	after, afterEvents, afterGen, afterReady, err := r.recordConvergeOutcome(m)
	if err != nil {
		return nil, err
	}

	diff, err := differ.DiffSnapshotsExcluding(before, after, opts.IgnoreFields)
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}

	return buildConvergeResult(diff, gen, afterGen, beforeEvents, afterEvents, afterReady, notes), nil
}

// recordConvergeBaseline snapshots atProvider and the update-event count
// before the convergence wait begins.
func (r *Runner) recordConvergeBaseline(m *manifest.Manifest) (snapshot []byte, events int, err error) {
	snapshot, err = r.Snapshot()
	if err != nil {
		return nil, 0, fmt.Errorf("recording snapshot: %w", err)
	}
	events, err = r.countUpdateEvents(m.Kind, m.Name)
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
	events, err = r.countUpdateEvents(m.Kind, m.Name)
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
// generations, and final readiness, and assembles the final ConvergeResult.
// notes are informational diagnostics (e.g. the baseline readiness-timeout
// note) that are always surfaced, whether or not they change the verdict —
// they must never be lost behind a field-level diagnostic, and must never
// silently replace one either.
func buildConvergeResult(diff []differ.FieldChange, gen, afterGen int64, beforeEvents, afterEvents int, afterReady bool, notes []string) *ConvergeResult {
	var problems []string
	passed := true
	// loopSignal is true only for the two failure modes that are actually
	// evidence of the controller repeatedly writing something: the
	// atProvider snapshot moved, or a new update event fired. A readiness
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
		// A genuine atProvider drift or update-event delta: this is what
		// operators and past tickets search for, so the string stays
		// verbatim regardless of what else also failed alongside it.
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

// countUpdateEvents counts occurrences of update-related events for the
// given involvedObject kind/name, whose reason is UpdatedExternalResource
// or CannotUpdateExternalResource. Queries across all namespaces because
// cluster-scoped managed resources may have their events recorded outside
// the resource's own namespace.
//
// client-go's event recorder aggregates repeated identical events onto a
// single Item by incrementing that Item's .count field rather than
// appending a new Item, so the number of matching Items is NOT the number
// of occurrences. This function sums each matching Item's .count instead
// of counting Items (see sumEventOccurrences).
func (r *Runner) countUpdateEvents(kind, name string) (int, error) {
	return r.countEventsByReason(kind, name, eventReasonUpdated, eventReasonCannotUpdate)
}

// countCreateEvents counts occurrences of CreatedExternalResource events for
// the given involvedObject kind/name, across the resource's ENTIRE
// lifecycle (not a before/after delta like countUpdateEvents). Exactly one
// occurrence is the expected outcome for a resource that has been created
// once and recovered its identity via search (rather than a stored ref)
// without duplicating — see RunResolveRecover.
func (r *Runner) countCreateEvents(kind, name string) (int, error) {
	return r.countEventsByReason(kind, name, EventReasonCreated)
}

// countEventsByReason lists cluster events and sums the aggregated
// occurrence count of every event matching the given involvedObject
// kind/name and any of the given reasons. Queries across all namespaces
// because cluster-scoped managed resources may have their events recorded
// outside the resource's own namespace.
func (r *Runner) countEventsByReason(kind, name string, reasons ...string) (int, error) {
	out, err := r.runRaw("get", "events", "--all-namespaces", "-o", "json")
	if err != nil {
		return 0, fmt.Errorf("listing events: %w", err)
	}

	var list eventList
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return 0, fmt.Errorf("parsing events JSON: %w", err)
	}

	return sumEventOccurrencesByReason(list, kind, name, reasons...), nil
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
type eventItem struct {
	Reason         string `json:"reason"`
	Count          int32  `json:"count"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
}

// sumEventOccurrences sums the aggregated .count field of every event Item
// matching the given involvedObject kind/name and an update-related reason,
// treating a zero .count as a single (non-aggregated) occurrence.
func sumEventOccurrences(list eventList, kind, name string) int {
	return sumEventOccurrencesByReason(list, kind, name, eventReasonUpdated, eventReasonCannotUpdate)
}

// sumEventOccurrencesByReason sums the aggregated .count field of every
// event Item matching the given involvedObject kind/name and any of the
// given reasons, treating a zero .count as a single (non-aggregated)
// occurrence.
func sumEventOccurrencesByReason(list eventList, kind, name string, reasons ...string) int {
	want := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		want[reason] = true
	}

	var total int32
	for _, it := range list.Items {
		if it.InvolvedObject.Kind != kind || it.InvolvedObject.Name != name {
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
