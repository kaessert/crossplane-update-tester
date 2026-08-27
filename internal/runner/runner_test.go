package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// testFieldNotifyDelay is a representative mutable scalar field used across
// the runFieldTest no-op/negative-case tests below.
const testFieldNotifyDelay = "notifyDelay"

// testFieldFeatureEnabled is a representative mutable boolean field.
const testFieldFeatureEnabled = "featureEnabled"

// testObjectTypePrefix stands in for the backend object-type prefix an
// external-name must carry when identity resolved against the EXPECTED
// object type of a resource whose backend models two object types behind
// one kind. testSiblingObjectTypePrefix is the other object type the same
// kind can resolve against — resolving against it is exactly the silent
// mis-identification the prefix check exists to catch.
const (
	testObjectTypePrefix        = "primary-type/"
	testSiblingObjectTypePrefix = "sibling-type/"
)

// testResourceIdentifier is the `kubectl get -o name` identifier of the
// resource under test, as ResolveResource caches it on Runner.resourceName.
const testResourceIdentifier = "exampleresource.example.crossplane.io/" + testNameExample

// testProviderDeployment is the controller Deployment name Crossplane's
// package manager derives from an installed provider package revision, and
// stamps onto the controller Pod's pkg.crossplane.io/revision label.
// testOtherProviderDeployment is a SECOND provider package installed in the
// same cluster — the situation resolveControllerDeploymentName must refuse
// to guess its way through.
const (
	testProviderDeployment      = "provider-example-000000000000"
	testOtherProviderDeployment = "provider-other-111111111111"
)

// kubectlGetSubcommand is the kubectl "get" subcommand name, shared across
// this file's fakeCluster dispatcher and the restartControllerDeployment
// argv assertions so the literal isn't repeated enough to trip goconst.
const kubectlGetSubcommand = "get"

// fakeCluster is an in-memory stand-in for a live cluster's view of a single
// managed resource. It implements the subset of kubectl behaviour Runner
// depends on (get -f -o name, get -o json, get events, get pods, patch,
// wait, rollout) so runFieldTest can be exercised without a real cluster.
//
// The resource itself is only ever read as a whole object (`-o json`):
// every value the runner needs, including the status.atProvider snapshot,
// is navigated and rendered in Go. handleGet therefore REJECTS any other
// output spec for a resource read rather than serving it — an argv that
// asks kubectl to render a subtree is a contract change this fake must not
// absorb silently.
type fakeCluster struct {
	forProvider map[string]interface{}
	atProvider  map[string]interface{}
	generation  int64
	// externalName, when non-empty, is surfaced as the live resource's
	// crossplane.io/external-name annotation — exercised by
	// TestExternalName*. Left empty, metadata carries no annotations map at
	// all (matching a resource observed before Create ever ran), which is
	// the default every other test in this file relies on.
	externalName string

	// kind and name identify the resource for event-evidence lookups
	// (kubectl get events ... involvedObject matching). Tests exercising
	// countUpdateEvents/runFieldTest's evidence check set these to match
	// the kind/name passed into runFieldTest. namespace and apiVersion do
	// the same for the involvedObject fields that scope a dual-scope
	// resource's events apart from its same-Kind-same-Name sibling; left
	// empty (the default for every test that predates namespace/apiVersion
	// scoping), they match a cluster-scoped resource.
	kind       string
	name       string
	namespace  string
	apiVersion string

	// siblingKind/siblingName/siblingNamespace/siblingAPIVersion, when
	// siblingKind is non-empty, make handleGetEvents also emit events for a
	// SECOND involvedObject — standing in for the other scope of a
	// dual-scope resource that shares this fake's Kind and Name but differs
	// in namespace/apiVersion (the unified example-manifest convention
	// every dual-scope provider follows). siblingEventBase is that sibling's
	// aggregated .count on the very first handleGetEvents call;
	// siblingEventGrowthPerCall adds that much MORE on every subsequent
	// call, simulating a sibling resource that keeps emitting new update
	// events across RunConverge's baseline/outcome reads — exactly the
	// scenario that bled into the resource-under-test's delta before events
	// were scoped by namespace/apiVersion.
	siblingKind               string
	siblingName               string
	siblingNamespace          string
	siblingAPIVersion         string
	siblingEventBase          int32
	siblingEventGrowthPerCall int32
	// siblingEventCallCount counts handleGetEvents calls, so
	// siblingEventGrowthPerCall can be applied per call.
	siblingEventCallCount int

	// resourceLines, when non-empty, overrides what
	// `kubectl get -f <manifest> -o name` prints — one line per manifest
	// document. Left empty, a single line naming the resource under test is
	// returned.
	resourceLines string

	// logLines, when non-empty, is what `kubectl logs -l <selector>` returns
	// for the convergence window — the controller-log loop instrument's raw
	// input (see countUpdateLogCalls). Left empty, a single benign reconcile
	// line is returned: a LIVE controller that made no Update() call, which
	// is what every test not exercising the instrument means by "quiet".
	// Distinguishing that from the empty string matters, because an empty
	// window is reported as "the instrument observed nothing", not as zero
	// Update() calls.
	logLines string
	// logErr, when non-nil, makes `kubectl logs` fail — the instrument is
	// unavailable rather than quiet.
	logErr error

	// providerPods holds the pkg.crossplane.io/revision label value of each
	// provider controller Pod the fake cluster is running, in the order
	// `kubectl get pods` would list them. Left empty, a single Pod for
	// testProviderDeployment is reported.
	providerPods []string

	// recordUpdateEvent, when true, makes each real field patch (not the
	// ClearConditions status patch) increment the active generation's
	// aggregated count by one — simulating a controller whose Update() call
	// emits an UpdatedExternalResource event. Left false, patches succeed
	// and atProvider still converges, but no event is ever recorded — the
	// "update not evidenced" scenario.
	recordUpdateEvent bool
	// eventsPerPatch, when non-zero, is how many update events a single
	// field-test patch produces — standing in for a controller whose
	// Update() path emits more than one UpdatedExternalResource/
	// CannotUpdateExternalResource event per logical field-test attempt
	// (retry/backoff cycles, late-init settling), the exact shape measured
	// live on ServicePolicyRule (3 events for several attempts). Left at
	// the default 0, handlePatch treats it as 1 — one event per patch,
	// which is every pre-existing test's assumption.
	eventsPerPatch int32
	// eventBudget caps how many events a single controller "process" (the
	// stretch between restart() calls) accumulates before further
	// increments are silently dropped — simulating client-go's in-process
	// event-spam-filter burst ceiling (see eventBurstCeiling in runner.go).
	// 0 (the default) means unlimited, for tests that are not exercising
	// the ceiling.
	eventBudget int32
	// generations holds one aggregated event count per controller
	// "process": index 0 is the count since the fake was created, and
	// restart() appends a new zeroed entry — mirroring how a real
	// controller restart discards client-go's in-memory event-aggregation
	// cache, so a post-restart event becomes a brand NEW Event object
	// instead of bumping the old one's .count further.
	generations []int32
	// restartCalls counts how many times restart() (wired in as
	// Runner.restartFunc) was invoked, so tests can assert the runner
	// actually triggered a burst reset rather than merely tolerating one.
	restartCalls int
	// rolloutCalls counts `kubectl rollout` invocations, so tests that
	// exercise the REAL restartControllerDeployment path (restartFunc left
	// nil) can assert whether a restart was actually issued.
	rolloutCalls int
	// emitZeroCountEvent, when true, reports the LAST generation's
	// aggregated .count field as 0 once it reaches at least 1 — mirroring
	// client-go's event recorder, which leaves .count unset (0) for an
	// event's first, unaggregated occurrence. Exercises sumEventOccurrences's
	// zero-guard (a 0 count is 1 occurrence, not 0) through the
	// runFieldTest evidence check rather than only through the standalone
	// sumEventOccurrences unit tests.
	emitZeroCountEvent bool

	patchCalls int
	waitCalls  int
	// nudgeCalls counts NudgeReconcile's metadata-annotation patches,
	// tracked separately from patchCalls (real field patches) so tests can
	// assert the forced second reconcile fired without conflating it with
	// the field patch itself.
	nudgeCalls int

	// getObjectCalls counts how many times handleGet has served the
	// resource-under-test object itself (get <resource> -o json — not the
	// events/pods/name lookups). readyAfterCalls compares against this to
	// simulate a Ready condition that only appears after some number of
	// polls, the way a real resource dips through NotReady before settling.
	getObjectCalls int
	// readyAfterCalls, when non-zero, makes handleGet embed a status.conditions
	// entry of type Ready: status "False" on every read before
	// getObjectCalls reaches this value, and "True" from that read onward.
	// Zero (the default) means no Ready condition is embedded at all,
	// matching a resource that has not been reconciled yet — every test in
	// this file that never sets this field is unaffected, since
	// extractObservedGeneration and isReadyTrue both already treat an absent
	// conditions list as "not yet", not as an error.
	readyAfterCalls int
	// neverReady, when true, makes handleGet embed a Ready condition whose
	// status is permanently "False" — used to exercise waitReady's timeout
	// path and RunConverge's degrade-and-proceed behaviour. Takes priority
	// over readyAfterCalls.
	neverReady bool

	// syncedConflictReads, when non-zero, makes handleGet embed a Synced
	// condition and drives it through the exact multi-pass late-init
	// conflict-then-settle race this bug is about — see
	// TestReconcileOnceWaitsThroughLateInitConflictBeforeReturning. Reads
	// are counted from getObjectCalls (see readyCondition — every read
	// increments it exactly once, so Ready and Synced always describe the
	// SAME read):
	//
	//   reads 1..syncedConflictReads:   Synced="False" (ReconcileError) at
	//                                    the CURRENT generation — a
	//                                    late-init 409 conflict retrying.
	//                                    Ready is unaffected: Observe()
	//                                    already marked it True on an
	//                                    earlier pass (see
	//                                    readyAfterCalls/neverReady).
	//   read syncedConflictReads+1:      metadata.generation bumps by ONE
	//                                    EXTRA step — the late-init spec
	//                                    write that DID succeed — but
	//                                    THIS read's Synced still reports
	//                                    "False" at the OLD generation:
	//                                    the reconcile that persisted the
	//                                    bump returned before re-marking
	//                                    Synced.
	//   any later read:                  Synced="True" at the NEW (bumped)
	//                                    generation — the pass the watch
	//                                    auto-triggers off that spec bump,
	//                                    which is also, on a real
	//                                    controller, the pass that finally
	//                                    evaluates and issues the field
	//                                    test's own genuine external
	//                                    Update().
	//
	// Zero (the default) leaves Synced modelling out of handleGet
	// entirely — matching every pre-existing test in this file, none of
	// which embeds a Synced condition at all.
	syncedConflictReads int
	// neverSynced, when true, makes handleGet embed a Synced condition
	// whose status is permanently "False" at the current generation —
	// used to exercise waitSynced's timeout path in isolation. Takes
	// priority over syncedConflictReads.
	neverSynced bool

	// silentWipeField and silentWipeValue, when silentWipeField is
	// non-empty, simulate a backend that resets an UNRELATED atProvider
	// field to silentWipeValue on every real field patch — the exact
	// defect the assert-unchanged directive exists to catch (see
	// checkAssertUnchanged). The tester's own merge patch never mentions
	// this field at all; the wipe happens purely as a side effect of
	// handlePatch, mirroring a backend that defaults an omitted union
	// member on every write regardless of which field the request touched.
	silentWipeField string
	silentWipeValue interface{}

	// driftField and driftValue, when driftField is non-empty, set an
	// atProvider field to a new value starting from the driftAfterGetCalls'th
	// read of the resource under test (1-based, counted the same way
	// readyAfterCalls is) — simulating a field that has genuinely changed by
	// the time the post-window snapshot is taken. Unlike silentWipeField,
	// this fires on a plain read, not on a patch: it exists to give converge
	// checks (which only ever read) a deterministic, call-count-driven way to
	// observe drift between the baseline and outcome snapshots, so a test can
	// prove that a target excluding driftField from its diff still passes
	// while a sibling target that does NOT exclude it fails on the SAME
	// underlying drift. driftAfterGetCalls == 0 (the default) disables this
	// entirely, matching every pre-existing test.
	driftField         string
	driftValue         interface{}
	driftAfterGetCalls int
}

// readyCondition reports the status.conditions entry handleGet should embed
// for the CURRENT read (incrementing getObjectCalls first, so the very
// first read is call 1, not call 0), and whether one should be embedded at
// all. observedGeneration is always the live generation once ANY condition
// is reported — mirroring a real controller, which sets it on every
// condition it (re)computes regardless of that condition's status — so
// readiness and generation-settling are independent knobs in these tests:
// setting readyAfterCalls or neverReady does not, by itself, make
// waitGenerationSettled loop.
func (f *fakeCluster) readyCondition() (cond map[string]interface{}, ok bool) {
	f.getObjectCalls++
	status := "False"
	switch {
	case f.neverReady:
		// status stays "False"
	case f.readyAfterCalls > 0:
		if f.getObjectCalls >= f.readyAfterCalls {
			status = "True"
		}
	default:
		return nil, false
	}
	return map[string]interface{}{
		"type":               "Ready",
		"status":             status,
		"observedGeneration": f.generation,
	}, true
}

// syncedCondition reports the Synced condition entry handleGet should embed
// for the CURRENT read, per syncedConflictReads' doc comment. It relies on
// readyCondition() having already run for this same read (so
// f.getObjectCalls is already the current read's 1-based index) — handleGet
// calls readyCondition() first, always.
func (f *fakeCluster) syncedCondition() (cond map[string]interface{}, ok bool) {
	if f.neverSynced {
		return map[string]interface{}{
			"type":               "Synced",
			"status":             "False",
			"observedGeneration": f.generation,
		}, true
	}
	if f.syncedConflictReads == 0 {
		return nil, false
	}
	switch {
	case f.getObjectCalls <= f.syncedConflictReads:
		return map[string]interface{}{
			"type":               "Synced",
			"status":             "False",
			"observedGeneration": f.generation,
		}, true
	case f.getObjectCalls == f.syncedConflictReads+1:
		// The late-init write that succeeded: bump the generation NOW
		// (before metadata.generation is captured by the caller), but
		// report Synced still "False" at the OLD generation — the
		// reconcile that persisted the bump returned before re-marking
		// Synced.
		stale := f.generation
		f.generation++
		return map[string]interface{}{
			"type":               "Synced",
			"status":             "False",
			"observedGeneration": stale,
		}, true
	default:
		return map[string]interface{}{
			"type":               "Synced",
			"status":             "True",
			"observedGeneration": f.generation,
		}, true
	}
}

// exec implements the Runner.execFunc signature, dispatching on the kubectl
// subcommand (first arg).
func (f *fakeCluster) exec(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeCluster: no args")
	}
	switch args[0] {
	case kubectlGetSubcommand:
		return f.handleGet(args)
	case "patch":
		return f.handlePatch(args)
	case "wait":
		f.waitCalls++
		return "", nil
	case "rollout":
		f.rolloutCalls++
		return "", nil
	case "logs":
		return f.handleLogs()
	default:
		return "", fmt.Errorf("fakeCluster: unhandled kubectl subcommand %q", args[0])
	}
}

func (f *fakeCluster) handleGet(args []string) (string, error) {
	if len(args) > 1 && args[1] == "events" {
		return f.handleGetEvents()
	}
	if len(args) > 1 && args[1] == "pods" {
		return f.handleGetPods(args)
	}
	if containsArg(args, "-f") {
		return f.handleGetResourceName()
	}
	if spec := outputSpecOf(args); spec != "json" {
		return "", fmt.Errorf("fakeCluster: the resource under test is only ever read with -o json, got -o %q: %v", spec, args)
	}
	var conds []interface{}
	if cond, ok := f.readyCondition(); ok {
		conds = append(conds, cond)
	}
	if cond, ok := f.syncedCondition(); ok {
		conds = append(conds, cond)
	}

	// metadata.generation is captured AFTER the condition helpers above run
	// — syncedCondition can itself bump f.generation mid-read (see its doc
	// comment), and this read's own metadata must reflect that bump, not
	// the value from before it.
	metadata := map[string]interface{}{"generation": f.generation}
	if f.externalName != "" {
		metadata["annotations"] = map[string]interface{}{
			externalNameAnnotation: f.externalName,
		}
	}
	if f.driftField != "" && f.driftAfterGetCalls > 0 && f.getObjectCalls >= f.driftAfterGetCalls {
		f.atProvider[f.driftField] = f.driftValue
	}
	status := map[string]interface{}{jsonKeyAtProvider: f.atProvider}
	if len(conds) > 0 {
		status["conditions"] = conds
	}
	obj := map[string]interface{}{
		"metadata":    metadata,
		"spec":        map[string]interface{}{"forProvider": f.forProvider},
		jsonKeyStatus: status,
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// handleGetResourceName backs `kubectl get -f <manifest> -o name`, which
// prints one line per document in the manifest.
func (f *fakeCluster) handleGetResourceName() (string, error) {
	if f.resourceLines != "" {
		return f.resourceLines, nil
	}
	return testResourceIdentifier + "\n", nil
}

// handleLogs backs `kubectl logs -n crossplane-system -l <selector>
// --tail=-1 --since=Ns`, the controller-log loop instrument's only cluster
// call. The default is one benign reconcile line — a controller that is
// demonstrably alive and logging, and that made no Update() call in the
// window — because "alive and quiet" and "not logging at all" are different
// verdicts and every test that does not set logLines means the former.
func (f *fakeCluster) handleLogs() (string, error) {
	if f.logErr != nil {
		return "", f.logErr
	}
	if f.logLines != "" {
		return f.logLines, nil
	}
	return testReconcileLogLine + "\n", nil
}

// handleGetPods backs `kubectl get pods -l pkg.crossplane.io/revision -o
// jsonpath=...`. It honours the two jsonpath shapes that matter: an
// `.items[0]` expression yields ONLY the first Pod's label (and kubectl's
// real index-out-of-bounds error when the list is empty), while a
// `{range .items[*]}` expression yields one line per Pod. Simulating that
// difference is what lets a test distinguish enumerate-all-and-refuse-to-
// guess from take-the-first-and-hope.
func (f *fakeCluster) handleGetPods(args []string) (string, error) {
	pods := f.providerPods
	if pods == nil {
		pods = []string{testProviderDeployment}
	}

	var jsonpath string
	for _, a := range args {
		if strings.HasPrefix(a, "jsonpath=") {
			jsonpath = a
		}
	}
	if jsonpath == "" {
		return "", fmt.Errorf("fakeCluster: get pods called without a jsonpath output spec: %v", args)
	}

	if strings.Contains(jsonpath, "range .items[*]") {
		var b strings.Builder
		for _, p := range pods {
			b.WriteString(p)
			b.WriteString("\n")
		}
		return b.String(), nil
	}

	// An `.items[0]` expression: kubectl fails outright on an empty list
	// rather than printing nothing.
	if len(pods) == 0 {
		return "", fmt.Errorf("array index out of bounds: index 0, length 0")
	}
	return pods[0] + "\n", nil
}

// handleGetEvents backs `kubectl get events --all-namespaces -o json`. It
// emits one aggregated Item per non-empty generation (see f.generations) —
// mirroring how a real controller restart causes client-go to create a
// brand NEW Event object rather than continuing to bump the throttled one —
// so countUpdateEvents/sumEventOccurrences must sum across Items, not just
// read the last one.
func (f *fakeCluster) handleGetEvents() (string, error) {
	f.siblingEventCallCount++
	list := eventList{}
	for i, count := range f.generations {
		if count <= 0 {
			continue
		}
		reported := count
		if f.emitZeroCountEvent && i == len(f.generations)-1 {
			reported = 0
		}
		item := eventItem{Reason: eventReasonUpdated, Count: reported}
		item.InvolvedObject.Kind = f.kind
		item.InvolvedObject.Name = f.name
		item.InvolvedObject.Namespace = f.namespace
		item.InvolvedObject.APIVersion = f.apiVersion
		list.Items = append(list.Items, item)
	}
	if f.siblingKind != "" {
		count := f.siblingEventBase + f.siblingEventGrowthPerCall*int32(f.siblingEventCallCount-1)
		item := eventItem{Reason: eventReasonUpdated, Count: count}
		item.InvolvedObject.Kind = f.siblingKind
		item.InvolvedObject.Name = f.siblingName
		item.InvolvedObject.Namespace = f.siblingNamespace
		item.InvolvedObject.APIVersion = f.siblingAPIVersion
		list.Items = append(list.Items, item)
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// restart simulates a controller pod restart: it appends a fresh generation
// so subsequent recordUpdateEvent increments accumulate against a new,
// unthrottled budget rather than the exhausted one. Wired in as
// Runner.restartFunc so RunTests's proactive burst-reset (eventBurstCeiling)
// can be exercised without a live cluster.
func (f *fakeCluster) restart() error {
	f.restartCalls++
	f.generations = append(f.generations, 0)
	return nil
}

func (f *fakeCluster) handlePatch(args []string) (string, error) {
	for _, a := range args {
		if a == "--subresource=status" {
			// ClearConditions — no state change needed for these tests.
			return "", nil
		}
	}

	var payload string
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			payload = args[i+1]
		}
	}
	if payload == "" {
		return "", fmt.Errorf("fakeCluster: patch called without -p payload")
	}

	var patch map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &patch); err != nil {
		return "", fmt.Errorf("fakeCluster: parsing patch payload: %w", err)
	}

	if _, isMetadataPatch := patch["metadata"]; isMetadataPatch {
		// NudgeReconcile — a metadata-annotation-only patch used to force a
		// reconcile without touching spec.forProvider. Counted separately
		// so it is never mistaken for a real field patch (which would
		// corrupt patchCalls/eventCount assertions below).
		f.nudgeCalls++
		return "", nil
	}

	specRaw, _ := patch["spec"].(map[string]interface{})
	forProviderRaw, _ := specRaw["forProvider"].(map[string]interface{})

	mergeInto(f.forProvider, forProviderRaw)
	// Simulate a controller that converges instantly, so pollField's first
	// read already matches — these tests are about the no-op guard, not
	// about polling behaviour.
	mergeInto(f.atProvider, forProviderRaw)

	// Simulate a backend that silently resets an unrelated atProvider field
	// on EVERY write, regardless of which field the merge patch actually
	// named — see silentWipeField's doc comment.
	if f.silentWipeField != "" {
		f.atProvider[f.silentWipeField] = f.silentWipeValue
	}

	f.generation++
	f.patchCalls++
	if f.recordUpdateEvent {
		if len(f.generations) == 0 {
			f.generations = append(f.generations, 0)
		}
		idx := len(f.generations) - 1
		perPatch := f.eventsPerPatch
		if perPatch <= 0 {
			perPatch = 1
		}
		// Each of the perPatch events is subject to the same per-generation
		// budget individually — mirroring a real spam filter, which drops
		// events past its burst ceiling one at a time rather than refusing
		// an entire multi-event write in one shot.
		for i := int32(0); i < perPatch; i++ {
			if f.eventBudget <= 0 || f.generations[idx] < f.eventBudget {
				f.generations[idx]++
			}
		}
	}
	return "", nil
}

// mergeInto applies a JSON-merge-patch style merge of patch into dst: nested
// objects are merged recursively, a nil value deletes the key, and any other
// value overwrites it.
func mergeInto(dst, patch map[string]interface{}) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if m, ok := v.(map[string]interface{}); ok {
			if dm, ok := dst[k].(map[string]interface{}); ok {
				mergeInto(dm, m)
				continue
			}
		}
		dst[k] = v
	}
}

// testEvidenceWindow is the evidenceWindow every test in this file gets via
// newFakeRunner: long enough that evidenceOutcome's retry loop can run
// through more than one iteration, short enough that the suite's many
// genuinely-never-evidenced regression tests (e.g.
// TestRunTestsWithoutRestartWiringStillDetectsGenuineNonEvidence, which
// exercises it once per field across dozens of fields) do not each spend
// evidenceRetryWindow's real 10 seconds finding that out. A test proving
// the retry mechanism itself overrides this with its own value — see
// TestEvidenceOutcomeRetriesUntilEventVisible and
// TestEvidenceOutcomeReportsNotEvidencedWhenNeverGrows.
const testEvidenceWindow = 5 * time.Millisecond

func newFakeRunner(f *fakeCluster) *Runner {
	return &Runner{
		resourceName: testResourceIdentifier,
		timeout:      "5s",
		execFunc:     f.exec,
		restartFunc:  f.restart,
		// sleepFunc/evidenceWindow: keep evidenceOutcome's post-patch
		// retry (see evidenceRetryWindow) fast under test rather than
		// spending real wall-clock time on every NotEvidenced case this
		// file exercises.
		sleepFunc:      func(time.Duration) {},
		evidenceWindow: testEvidenceWindow,
	}
}

// TestSnapshotReadsTheWholeObject pins the kubectl contract Snapshot
// depends on. The bytes it returns go straight to a JSON parser, so asking
// kubectl to render the status.atProvider subtree with a jsonpath
// expression made every convergence check depend on a rendering that
// changed across kubectl versions: before ~1.21, a non-scalar jsonpath
// result printed in Go syntax ("map[key:value]") and the check failed while
// PARSING the snapshot instead of reporting a verdict. Reading the whole
// object and marshalling the subtree here removes that dependency, so the
// argv must stay `get <resource> -o json` — a jsonpath read reappearing
// would reintroduce the trap invisibly, because it still passes against
// every kubectl a developer is likely to have installed.
func TestSnapshotReadsTheWholeObject(t *testing.T) {
	f := &fakeCluster{atProvider: map[string]interface{}{"zone": "b", "address": "a"}}
	r := newFakeRunner(f)

	var gotArgs [][]string
	r.execFunc = func(args []string) (string, error) {
		gotArgs = append(gotArgs, append([]string(nil), args...))
		return f.exec(args)
	}

	got, err := r.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: unexpected error: %v", err)
	}

	// Marshalling a decoded map sorts its keys, so the snapshot of a given
	// observed state is byte-stable regardless of the order kubectl
	// happened to print it in.
	const want = `{"address":"a","zone":"b"}`
	if string(got) != want {
		t.Errorf("Snapshot() = %s, want %s", got, want)
	}

	wantArgs := [][]string{{kubectlGetSubcommand, testResourceIdentifier, "-o", "json"}}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("Snapshot issued %q, want exactly %q", gotArgs, wantArgs)
	}
}

// TestSnapshotAtProviderShapes covers what Snapshot makes of each shape
// status.atProvider can actually have. The empty-object cases are load
// bearing: a resource whose observed state has not been populated yet is a
// normal pre-population state that several checks snapshot before anything
// has been written, so it must read as {} rather than fail the run. A
// present-but-not-an-object status is the opposite case — nothing
// downstream can interpret it as a field map, so coercing it to {} would
// report "all fields stable" about a status this tool never understood.
func TestSnapshotAtProviderShapes(t *testing.T) {
	cases := map[string]struct {
		reason     string
		object     string
		want       string
		wantErrHas string
	}{
		"Populated": {
			reason: "the ordinary case: the subtree is returned as canonical JSON",
			object: `{"status":{"atProvider":{"address":"a"},"conditions":[]}}`,
			want:   `{"address":"a"}`,
		},
		"NestedValuesArePreserved": {
			reason: "the differ compares whole values, so nested objects and arrays must survive intact",
			object: `{"status":{"atProvider":{"tags":["x","y"],"nested":{"k":1}}}}`,
			want:   `{"nested":{"k":1},"tags":["x","y"]}`,
		},
		"NoStatusYet": {
			reason: "a resource observed before its controller ever wrote status",
			object: `{"metadata":{"generation":1}}`,
			want:   "{}",
		},
		"NoAtProviderYet": {
			reason: "status exists but the observed state has not been populated",
			object: `{"status":{"conditions":[]}}`,
			want:   "{}",
		},
		"NullAtProvider": {
			reason: "an explicit null is the same pre-population state as an absent key",
			object: `{"status":{"atProvider":null}}`,
			want:   "{}",
		},
		"EmptyAtProvider": {
			reason: "an empty object round-trips as an empty object",
			object: `{"status":{"atProvider":{}}}`,
			want:   "{}",
		},
		"ScalarAtProviderIsAnError": {
			reason:     "a scalar cannot be a field map — report it rather than reading it as no fields at all",
			object:     `{"status":{"atProvider":"unexpected"}}`,
			wantErrHas: "expected a JSON object",
		},
		"ArrayAtProviderIsAnError": {
			reason:     "an array is equally uninterpretable as a field map",
			object:     `{"status":{"atProvider":[1,2]}}`,
			wantErrHas: "expected a JSON object",
		},
		"ScalarStatusIsAnError": {
			reason:     "a status that is not an object cannot be descended at all",
			object:     `{"status":"unexpected"}`,
			wantErrHas: "cannot navigate to",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &Runner{
				resourceName: testResourceIdentifier,
				execFunc: func([]string) (string, error) {
					return tc.object, nil
				},
			}

			got, err := r.Snapshot()
			if tc.wantErrHas != "" {
				if err == nil {
					t.Fatalf("%s: expected an error, got %s", tc.reason, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Errorf("%s: error = %q, want substring %q", tc.reason, err.Error(), tc.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if string(got) != tc.want {
				t.Errorf("%s: Snapshot() = %s, want %s", tc.reason, got, tc.want)
			}
		})
	}
}

// TestSlowObserveThreshold pins the derivation of the slow-observe bar from
// the provider's poll interval. The annotation exists to say "this field
// only converged on the scale of a poll cycle", so the bar has to move with
// the cycle: at a 10s interval a 6s pass IS poll-cycle-scale, while at the
// 60s default the same 6s pass is an ordinary fast PASS. The unset case is
// the compatibility guarantee — a Runner nobody told about a poll interval
// must behave exactly as it did when the threshold was a 30s constant.
func TestSlowObserveThreshold(t *testing.T) {
	cases := map[string]struct {
		reason        string
		pollInterval  time.Duration
		wantThreshold time.Duration
		// duration is a field test's total duration, and wantSlow whether
		// it earns the annotation at this threshold.
		duration time.Duration
		wantSlow bool
	}{
		"UnsetKeepsTheHistoricalThirtySeconds": {
			reason:        "an undeclared interval reads as the 60s default, i.e. the 30s bar this check has always used",
			pollInterval:  0,
			wantThreshold: 30 * time.Second,
			duration:      6 * time.Second,
			wantSlow:      false,
		},
		"UnsetAtExactlyTheBar": {
			reason:        "the comparison is inclusive: a duration equal to the threshold is already slow",
			pollInterval:  0,
			wantThreshold: 30 * time.Second,
			duration:      30 * time.Second,
			wantSlow:      true,
		},
		"DefaultDeclaredExplicitly": {
			reason:        "declaring the default explicitly must not change anything",
			pollInterval:  60 * time.Second,
			wantThreshold: 30 * time.Second,
			duration:      6 * time.Second,
			wantSlow:      false,
		},
		"FastPollingLowersTheBar": {
			reason:        "a provider polling every 10s makes a 6s pass poll-cycle-scale, where the 60s default would call it fast",
			pollInterval:  10 * time.Second,
			wantThreshold: 5 * time.Second,
			duration:      6 * time.Second,
			wantSlow:      true,
		},
		"SlowPollingRaisesTheBar": {
			reason:        "a provider polling every 300s makes a 100s pass ordinary, where a fixed 30s bar would have flagged it",
			pollInterval:  300 * time.Second,
			wantThreshold: 150 * time.Second,
			duration:      100 * time.Second,
			wantSlow:      false,
		},
		"NegativeIsTreatedAsUnset": {
			reason:        "a nonsensical interval falls back to the default rather than producing a negative bar that flags everything",
			pollInterval:  -5 * time.Second,
			wantThreshold: 30 * time.Second,
			duration:      6 * time.Second,
			wantSlow:      false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := (&Runner{}).WithPollInterval(tc.pollInterval)

			got := r.slowObserveThreshold()
			if got != tc.wantThreshold {
				t.Fatalf("%s: slowObserveThreshold() = %s, want %s", tc.reason, got, tc.wantThreshold)
			}
			if slow := tc.duration >= got; slow != tc.wantSlow {
				t.Errorf("%s: a %s field test at a %s threshold slow = %v, want %v",
					tc.reason, tc.duration, got, slow, tc.wantSlow)
			}
		})
	}
}

// TestRunFieldTestSlowObserveTracksPollInterval drives the derived
// threshold through the code path that actually sets the annotation, so the
// wiring between the declared poll interval and the reported result is
// proved rather than inferred from slowObserveThreshold alone.
//
// The declared intervals are deliberately extreme rather than realistic:
// the fake cluster converges instantly, so a field test here takes
// microseconds. A 2µs interval puts the bar (1µs) below any real run, and
// the default 60s interval puts it 30 seconds above one — which is exactly
// the comparison a 10s-polling provider makes about a 6s field and a
// 60s-polling provider makes about the same field, without either test
// having to sleep.
func TestRunFieldTestSlowObserveTracksPollInterval(t *testing.T) {
	cases := map[string]struct {
		reason       string
		pollInterval time.Duration
		wantSlow     bool
	}{
		"UndeclaredIntervalLeavesAFastPassUnannotated": {
			reason:       "the 30s default bar is far above any duration this fake can produce",
			pollInterval: 0,
			wantSlow:     false,
		},
		"DeclaredIntervalBelowTheDurationAnnotates": {
			reason:       "a bar derived from a tiny declared interval sits below the field test's own duration",
			pollInterval: 2 * time.Microsecond,
			wantSlow:     true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeCluster{
				forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(5)},
				atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(5)},
				generation:        1,
				kind:              testKindExample,
				name:              testNameExample,
				recordUpdateEvent: true,
			}
			r := newFakeRunner(f).WithPollInterval(tc.pollInterval)
			snapshot, err := json.Marshal(f.atProvider)
			if err != nil {
				t.Fatalf("marshalling snapshot: %v", err)
			}

			test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
			result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

			if !result.Passed {
				t.Fatalf("%s: expected a PASS to annotate, got %+v", tc.reason, result)
			}
			if result.SlowObserve != tc.wantSlow {
				t.Errorf("%s: SlowObserve = %v, want %v (duration %s, threshold %s)",
					tc.reason, result.SlowObserve, tc.wantSlow, result.Duration, r.slowObserveThreshold())
			}
		})
	}
}

// TestRunFieldTestNoOpDetection covers the no-op detection path: a field
// whose pre-patch value already equals the target value must be reported as
// a failure (not a PASS), and the patch/wait machinery must never fire since
// there is nothing for it to exercise.
func TestRunFieldTestNoOpDetection(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(10)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(10)},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
	result, newSnapshot := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if !result.NoOp {
		t.Fatalf("expected NoOp=true, got %+v", result)
	}
	if result.Passed {
		t.Fatal("no-op result must not be reported as Passed")
	}
	if result.Error == nil {
		t.Fatal("expected a non-nil error naming the no-op field and value")
	}
	const wantSubstr = "no-op: notifyDelay already equals 10"
	if !strings.Contains(result.Error.Error(), wantSubstr) {
		t.Errorf("error = %q, want substring %q", result.Error.Error(), wantSubstr)
	}
	if !result.BeforeKnown || result.Before != "10" {
		t.Errorf("Before = %q, BeforeKnown = %v, want \"10\", true — the no-op check already read the pre-patch value", result.Before, result.BeforeKnown)
	}
	if f.patchCalls != 0 {
		t.Errorf("expected 0 patch calls for a no-op field, got %d", f.patchCalls)
	}
	if f.waitCalls != 0 {
		t.Errorf("expected 0 wait calls for a no-op field, got %d", f.waitCalls)
	}
	if !bytes.Equal(newSnapshot, snapshot) {
		t.Errorf("no-op short-circuit must return the input snapshot unchanged")
	}
}

// TestRunFieldTestExecutesWhenValueDiffers covers the negative case: a field
// whose pre-patch value differs from the target is patched and tested
// normally, producing a genuine PASS backed by positive update-event
// evidence (see TestRunFieldTestNotEvidencedWhenNoUpdateEvent for the
// counter-case), and exercises the forced second reconcile that refreshes
// atProvider independently of the provider's poll interval.
func TestRunFieldTestExecutesWhenValueDiffers(t *testing.T) {
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
	result, newSnapshot := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if result.NoOp {
		t.Fatalf("expected NoOp=false when the pre-patch value differs, got %+v", result)
	}
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Passed {
		t.Fatalf("expected the update test to pass, got %+v", result)
	}
	// The pre-patch value (5) must survive on the result distinct from the
	// post-patch target (10, on both Expected and Actual once the test
	// passes) — it is what lets a PASS line show the transition that
	// actually happened instead of the target value paired with itself.
	if !result.BeforeKnown || result.Before != "5" {
		t.Errorf("Before = %q, BeforeKnown = %v, want \"5\", true", result.Before, result.BeforeKnown)
	}
	if !result.UpdateEvidenced {
		t.Errorf("expected UpdateEvidenced=true when the controller emits an update event, got %+v", result)
	}
	if result.NotEvidenced {
		t.Errorf("expected NotEvidenced=false for an evidenced update, got %+v", result)
	}
	if f.patchCalls != 1 {
		t.Errorf("expected exactly 1 patch call, got %d", f.patchCalls)
	}
	// 2 nudges, not 1: reconcileOnce nudges to force the reconcile it then
	// waits on (the spec Patch above is not a reliable trigger — it can
	// have already reconciled and been wiped by ClearConditions before
	// reconcileOnce gets a chance to observe it), and nudgeAndReconcile
	// nudges again to force the independent re-observe that refreshes
	// atProvider. Both reconciles must be triggered, not just the forced
	// re-observe.
	if f.nudgeCalls != 2 {
		t.Errorf("expected exactly 2 nudge calls (forcing both reconciles), got %d", f.nudgeCalls)
	}
	if f.waitCalls != 2 {
		t.Errorf("expected exactly 2 wait calls (first reconcile + forced re-observe), got %d", f.waitCalls)
	}
	if bytes.Equal(newSnapshot, snapshot) {
		t.Error("expected the post-patch snapshot to differ from the input snapshot")
	}
	if len(result.SideFx) != 0 {
		t.Errorf("expected no side effects, got %v", result.SideFx)
	}
}

// TestReconcileOnceNudgesBeforeWaiting is a regression test for the case
// TestRunFieldTestExecutesWhenValueDiffers exercises indirectly: called on
// its own, reconcileOnce must not rely on some OTHER trigger (a caller's
// preceding spec patch, in production) having already queued a reconcile.
// If it merely clears conditions and waits, WaitReady blocks until the
// controller's next background poll tick, because a status-only patch is
// filtered out by resource.DesiredStateChanged() and never reaches the
// reconciler. Calling reconcileOnce with nothing else in flight isolates
// that dependency: against the pre-fix body (ClearConditions, then
// WaitReady with no nudge in between) this fails with nudgeCalls == 0.
func TestReconcileOnceNudgesBeforeWaiting(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
	}
	r := newFakeRunner(f)

	if err := r.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if f.nudgeCalls != 1 {
		t.Errorf("expected reconcileOnce to nudge once so WaitReady blocks on the reconcile it just forced, rather than falling back to the provider's background poll tick; got %d nudge calls", f.nudgeCalls)
	}
	if f.waitCalls != 1 {
		t.Errorf("expected exactly 1 wait call, got %d", f.waitCalls)
	}
}

// TestReconcileOnceWaitsThroughLateInitConflictBeforeReturning reproduces
// the race a late-init 409 conflict-and-retry opens: Observe() marks Ready
// True on every successful GET independent of whether that SAME pass went
// on to persist a write successfully, so WaitReady alone can return the
// instant it is called — Ready was already True from before this
// reconcileOnce call even started. Against the pre-fix body (ClearConditions,
// NudgeReconcile, WaitReady, nothing else), this test's fakeCluster is
// NEVER READ at all: WaitReady's kubectl "wait" is faked to succeed
// unconditionally without calling handleGet, so getObjectCalls stays 0 and
// the simulated late-init generation bump (see syncedConflictReads) never
// fires — the "genuine settle" this bug is about is never even observed to
// have happened, let alone waited for.
//
// With the fix, reconcileOnce also polls for the Synced condition to read
// True AT THE resource's CURRENT generation, which this fakeCluster only
// reports on the third read (see syncedConflictReads' doc comment) — so a
// passing run here proves reconcileOnce actually blocked through the
// conflict (read 1), the late-init generation bump (read 2), and the
// genuine settle (read 3), rather than returning the instant Ready read
// True.
func TestReconcileOnceWaitsThroughLateInitConflictBeforeReturning(t *testing.T) {
	f := &fakeCluster{
		forProvider:         map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:          map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:          1,
		kind:                testKindExample,
		name:                testNameExample,
		readyAfterCalls:     1, // Ready reads True from the very first read.
		syncedConflictReads: 1, // one conflicting read, then the bump, then True.
	}
	r := newFakeRunner(f)

	if err := r.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	// Exactly 1 nudge / 1 wait: unchanged from TestReconcileOnceNudgesBeforeWaiting
	// — the fix adds a NEW poll loop, it does not add another nudge or
	// another `kubectl wait` shell-out.
	if f.nudgeCalls != 1 {
		t.Errorf("expected exactly 1 nudge call, got %d", f.nudgeCalls)
	}
	if f.waitCalls != 1 {
		t.Errorf("expected exactly 1 wait call, got %d", f.waitCalls)
	}
	// 3 reads: the conflicting read, the read that observes the late-init
	// bump (still stale), and the read that finally observes Synced=True
	// at the bumped generation. Against the pre-fix body this is 0 — proof
	// that reconcileOnce genuinely blocked on the sequence rather than
	// returning as soon as the already-True Ready condition was checked.
	if f.getObjectCalls != 3 {
		t.Fatalf("getObjectCalls = %d, want 3 — reconcileOnce must poll through the conflict, the late-init bump, and the eventual settle, not return early", f.getObjectCalls)
	}
	// The simulated late-init write bumped the generation by one EXTRA
	// step beyond the starting generation (1 -> 2). A reconcileOnce that
	// returned before this bump was ever observed would leave f.generation
	// at 1.
	if f.generation != 2 {
		t.Errorf("generation = %d, want 2 — the late-init bump must have been observed before reconcileOnce returned", f.generation)
	}
}

// TestApplyPatchAndReconcileAbsorbsLateInitConflictBeforeReturning is the
// end-to-end version of TestReconcileOnceWaitsThroughLateInitConflictBeforeReturning,
// exercising the actual production call path a field test uses:
// applyPatchAndReconcile's real field Patch() (which itself bumps the
// generation once), followed by ITS OWN two forced reconciles. It proves
// the genuine settle — the late-init bump plus the reconcile the watch
// auto-triggers off it — is fully absorbed before applyPatchAndReconcile
// returns to its caller (runFieldTest), rather than leaking into whatever
// runs next (a later poll, or a separate RunConverge call — see
// TestRunConvergePreCheckWaitsThroughLateInitConflictBeforeBaseline for
// that half of the story).
func TestApplyPatchAndReconcileAbsorbsLateInitConflictBeforeReturning(t *testing.T) {
	f := &fakeCluster{
		forProvider:         map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:          map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:          1,
		kind:                testKindExample,
		name:                testNameExample,
		readyAfterCalls:     1,
		syncedConflictReads: 1,
	}
	r := newFakeRunner(f)

	test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
	if err := r.applyPatchAndReconcile(test); err != nil {
		t.Fatalf("applyPatchAndReconcile: %v", err)
	}

	if f.patchCalls != 1 {
		t.Errorf("expected exactly 1 real field patch, got %d", f.patchCalls)
	}
	// 2 nudges / 2 waits: unchanged from TestRunFieldTestExecutesWhenValueDiffers
	// — applyPatchAndReconcile's own two forced reconciles, undisturbed by
	// the fix.
	if f.nudgeCalls != 2 {
		t.Errorf("expected exactly 2 nudge calls, got %d", f.nudgeCalls)
	}
	if f.waitCalls != 2 {
		t.Errorf("expected exactly 2 wait calls, got %d", f.waitCalls)
	}
	// 4 reads: reconcileOnce's first call absorbs the conflict, the bump,
	// and the settle (3 reads, as in TestReconcileOnceWaitsThroughLateInitConflictBeforeReturning);
	// its second call (nudgeAndReconcile) confirms Synced is STILL True at
	// the now-unchanged generation in a single read. Against the pre-fix
	// body this is 0 — the whole sequence would have gone unobserved.
	if f.getObjectCalls != 4 {
		t.Fatalf("getObjectCalls = %d, want 4", f.getObjectCalls)
	}
	// generation 1 -> 2 from the real field patch, then -> 3 from the
	// simulated late-init write — both absorbed before this call returns.
	if f.generation != 3 {
		t.Errorf("generation = %d, want 3 — the field patch's own bump plus the late-init bump must both be observed before applyPatchAndReconcile returns", f.generation)
	}
}

// TestApplyPatchAndReconcileClearNullsSiblingInSamePatch proves the
// end-to-end wiring of manifest.UpdateTest.Clear: applyPatchAndReconcile
// passes t.Clear through to Patch/buildMergePatch, so a switch test sets
// its primary field AND nulls its named sibling(s) in ONE kubectl patch
// call — never two sequential patches that could each independently
// succeed while leaving both union arms set on the backend.
func TestApplyPatchAndReconcileClearNullsSiblingInSamePatch(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{
			"defaultBotSetting": map[string]interface{}{},
		},
		atProvider:      map[string]interface{}{},
		generation:      1,
		kind:            testKindExample,
		name:            testNameExample,
		readyAfterCalls: 1,
	}
	r := newFakeRunner(f)

	test := manifest.UpdateTest{
		Field: "botProtectionSetting",
		Value: map[string]interface{}{},
		Clear: []string{"defaultBotSetting"},
	}
	if err := r.applyPatchAndReconcile(test); err != nil {
		t.Fatalf("applyPatchAndReconcile: %v", err)
	}

	if f.patchCalls != 1 {
		t.Errorf("expected exactly 1 real field patch (the switch must be atomic), got %d", f.patchCalls)
	}
	if _, stillPresent := f.forProvider["defaultBotSetting"]; stillPresent {
		t.Errorf("forProvider still carries defaultBotSetting after the clear-bearing patch, want it removed")
	}
	if _, ok := f.forProvider["botProtectionSetting"]; !ok {
		t.Errorf("forProvider is missing botProtectionSetting, want the primary field's value applied")
	}
}

// TestRunFieldTestNotEvidencedWhenNoUpdateEvent covers the failure mode this
// evidence check exists to catch: a field whose observed value ends up
// matching the target (e.g. because something outside the reconciler wrote
// it) but for which the controller never recorded an update event. Without
// the event-count signal this would be indistinguishable from a genuine
// PASS; with it, it must be downgraded to a distinct failure.
func TestRunFieldTestNotEvidencedWhenNoUpdateEvent(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
		// recordUpdateEvent left false: the patch still "succeeds" (the fake
		// always converges atProvider), but no UpdatedExternalResource event
		// is ever recorded — simulating a reconciler whose Update() path
		// never actually ran.
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if result.Passed {
		t.Fatalf("expected Passed=false when no update event was recorded, got %+v", result)
	}
	if !result.NotEvidenced {
		t.Fatalf("expected NotEvidenced=true, got %+v", result)
	}
	if result.UpdateEvidenced {
		t.Errorf("expected UpdateEvidenced=false, got %+v", result)
	}
	if result.Error == nil {
		t.Fatal("expected a non-nil error explaining the missing evidence")
	}
	const wantSubstr = "update not evidenced"
	if !strings.Contains(result.Error.Error(), wantSubstr) {
		t.Errorf("error = %q, want substring %q", result.Error.Error(), wantSubstr)
	}
}

// TestRunFieldTestZeroCountEventStillEvidencesUpdate confirms the evidence
// check applies the same zero-guard as sumEventOccurrences: an aggregated
// event whose .count field reads 0 (client-go leaves it unset for a single,
// unaggregated occurrence) must still count as one occurrence and evidence
// the update, not be mistaken for "no event happened".
func TestRunFieldTestZeroCountEventStillEvidencesUpdate(t *testing.T) {
	f := &fakeCluster{
		forProvider:        map[string]interface{}{testFieldNotifyDelay: float64(5)},
		atProvider:         map[string]interface{}{testFieldNotifyDelay: float64(5)},
		generation:         1,
		kind:               testKindExample,
		name:               testNameExample,
		recordUpdateEvent:  true,
		emitZeroCountEvent: true,
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{Field: testFieldNotifyDelay, Value: 10}
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if !result.UpdateEvidenced {
		t.Fatalf("expected UpdateEvidenced=true for a zero-count first occurrence, got %+v", result)
	}
	if !result.Passed {
		t.Fatalf("expected Passed=true, got %+v", result)
	}
	if result.NotEvidenced {
		t.Errorf("expected NotEvidenced=false, got %+v", result)
	}
}

// evidenceEventsSource is a minimal fake for the "get events" kubectl call,
// used only by the evidenceOutcome-level tests below. Unlike fakeCluster
// (which models a whole live resource and increments its event count
// synchronously from handlePatch), this models the aggregated event count
// exactly as the reconciler's asynchronous EventRecorder does: the caller
// controls independently of any patch when a LIST call starts observing the
// grown count, so the retry loop's own behaviour — not the surrounding
// patch/reconcile machinery — is what's under test.
type evidenceEventsSource struct {
	// visibleFromCall is the 1-indexed handleGetEvents call number from
	// which the grown count first becomes visible. 0 means never.
	visibleFromCall int
	calls           int
}

func (s *evidenceEventsSource) exec(args []string) (string, error) {
	if len(args) < 2 || args[0] != kubectlGetSubcommand || args[1] != "events" {
		return "", fmt.Errorf("evidenceEventsSource: unexpected exec call: %v", args)
	}
	s.calls++
	list := eventList{}
	if s.visibleFromCall > 0 && s.calls >= s.visibleFromCall {
		// A zero-value InvolvedObject matches the zero-value
		// kind/name/namespace/apiVersion both tests below pass into
		// evidenceOutcome.
		list.Items = append(list.Items, eventItem{Reason: eventReasonUpdated, Count: 1})
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestEvidenceOutcomeRetriesUntilEventVisible is the fix's acceptance case:
// evidenceOutcome must not conclude NOT-EVIDENCED off a single synchronous
// recount when the update event genuinely exists but has not yet been
// indexed by a LIST call — it must keep re-listing within its retry window
// and report evidenced as soon as the count grows, proving the retry
// actually closes the race rather than a fixed sleep merely delaying the
// same single check.
func TestEvidenceOutcomeRetriesUntilEventVisible(t *testing.T) {
	const visibleFromCall = 4 // absent on the first 3 recounts, present from the 4th
	src := &evidenceEventsSource{visibleFromCall: visibleFromCall}
	r := &Runner{
		execFunc:       src.exec,
		sleepFunc:      func(time.Duration) {},
		evidenceWindow: time.Second,
	}

	checked, evidenced, err := r.evidenceOutcome("", "", "", "", 0, nil)
	if err != nil {
		t.Fatalf("evidenceOutcome() error = %v", err)
	}
	if !checked {
		t.Fatal("checked = false, want true")
	}
	if !evidenced {
		t.Fatalf("evidenced = false, want true — the count grows on recount %d, well inside the retry window", visibleFromCall)
	}
	if src.calls < visibleFromCall {
		t.Errorf("evidenceOutcome recounted only %d time(s) before reporting evidenced, want at least %d — it must genuinely retry, not report evidenced without having observed the growth",
			src.calls, visibleFromCall)
	}
}

// TestEvidenceOutcomeReportsNotEvidencedWhenNeverGrows is the retry's
// ceiling case: a count that never grows within the retry window must still
// report NOT-EVIDENCED, proving the retry does not mask a genuine missing
// Update() by retrying forever or by treating "still absent" as success.
func TestEvidenceOutcomeReportsNotEvidencedWhenNeverGrows(t *testing.T) {
	src := &evidenceEventsSource{} // visibleFromCall left 0: never visible
	r := &Runner{
		execFunc:       src.exec,
		sleepFunc:      func(time.Duration) {},
		evidenceWindow: testEvidenceWindow,
	}

	checked, evidenced, err := r.evidenceOutcome("", "", "", "", 0, nil)
	if err != nil {
		t.Fatalf("evidenceOutcome() error = %v", err)
	}
	if !checked {
		t.Fatal("checked = false, want true")
	}
	if evidenced {
		t.Fatal("evidenced = true, want false — the count never grows, so this must not mask a genuine missing Update()")
	}
	if src.calls < 2 {
		t.Errorf("evidenceOutcome recounted only %d time(s), want a genuine retry (>1) before giving up within the window", src.calls)
	}
}

// TestRunFieldTestNoOpUsesExpectOverride confirms the no-op comparison uses
// the patch target value (t.Value), independent of an optional t.Expect
// override used for fields whose observed value differs from what was set.
func TestRunFieldTestNoOpUsesExpectOverride(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldFeatureEnabled: true},
		atProvider:  map[string]interface{}{testFieldFeatureEnabled: true},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{Field: testFieldFeatureEnabled, Value: true, Expect: true}
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if !result.NoOp {
		t.Fatalf("expected NoOp=true, got %+v", result)
	}
	if f.patchCalls != 0 {
		t.Errorf("expected 0 patch calls, got %d", f.patchCalls)
	}
}

// TestReadCurrentValuePrefersSpecForProvider verifies readCurrentValue reads
// spec.forProvider ahead of status.atProvider — the value a merge patch is
// about to overwrite, not the (possibly stale) observed value.
func TestReadCurrentValuePrefersSpecForProvider(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(10)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(999)},
	}
	r := newFakeRunner(f)

	got, err := r.readCurrentValue(testFieldNotifyDelay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "10" {
		t.Errorf("readCurrentValue = %q, want %q (spec.forProvider value)", got, "10")
	}
}

// TestReadCurrentValueFallsBackToAtProvider verifies readCurrentValue falls
// back to status.atProvider when the field is absent from spec.forProvider.
func TestReadCurrentValueFallsBackToAtProvider(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{},
		atProvider:  map[string]interface{}{"computedField": "server-value"},
	}
	r := newFakeRunner(f)

	got, err := r.readCurrentValue("computedField")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "server-value" {
		t.Errorf("readCurrentValue = %q, want %q (status.atProvider fallback)", got, "server-value")
	}
}

// TestNavigateJSONPath covers navigateAtProvider and navigateSpecForProvider
// (both backed by navigateJSONPath), including nested fields, missing
// containers, missing leaves, non-object intermediates, and the
// present-vs-absent distinction the exists return value carries.
func TestNavigateJSONPath(t *testing.T) {
	cases := map[string]struct {
		reason     string
		obj        map[string]interface{}
		field      string
		want       interface{}
		wantExists bool
		wantErr    bool
	}{
		"TopLevelField": {
			reason: "a direct child of atProvider resolves",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"name": "example"},
				},
			},
			field:      "name",
			want:       "example",
			wantExists: true,
		},
		"NestedField": {
			reason: "a dot-path descends through nested objects",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{
						"parent": map[string]interface{}{"child": "value"},
					},
				},
			},
			field:      "parent.child",
			want:       "value",
			wantExists: true,
		},
		"MissingContainer": {
			reason:     "a missing status key returns nil, exists=false, rather than erroring",
			obj:        map[string]interface{}{},
			field:      "name",
			want:       nil,
			wantExists: false,
		},
		"MissingLeaf": {
			reason: "a leaf absent from an otherwise present object returns nil, exists=false",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"other": "x"},
				},
			},
			field:      "name",
			want:       nil,
			wantExists: false,
		},
		"PresentButNullLeaf": {
			reason: "a leaf explicitly set to JSON null is present (exists=true), unlike a leaf that is simply absent",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"name": nil},
				},
			},
			field:      "name",
			want:       nil,
			wantExists: true,
		},
		"NonObjectIntermediate": {
			reason: "descending through a scalar intermediate is an error",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"parent": "scalar"},
				},
			},
			field:   "parent.child",
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, exists, err := navigateAtProvider(tc.obj, tc.field)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: expected error, got nil", tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: got %v, want %v", tc.reason, got, tc.want)
			}
			if exists != tc.wantExists {
				t.Errorf("%s: exists = %v, want %v", tc.reason, exists, tc.wantExists)
			}
		})
	}
}

// TestExternalName covers Runner.ExternalName's two observable states: the
// annotation present with a value, and absent entirely (a resource observed
// before Create has ever populated it).
func TestExternalName(t *testing.T) {
	cases := map[string]struct {
		reason       string
		externalName string
		want         string
	}{
		"AnnotationPresent": {
			reason:       "the live crossplane.io/external-name annotation value is returned verbatim",
			externalName: testObjectTypePrefix + "abc123/example-object",
			want:         testObjectTypePrefix + "abc123/example-object",
		},
		"AnnotationAbsent": {
			reason: "a resource with no external-name annotation yet returns an empty string, not an error",
			want:   "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeCluster{externalName: tc.externalName}
			r := newFakeRunner(f)

			got, err := r.ExternalName()
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: ExternalName() = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// TestCheckExternalNamePrefix proves the identity guard for a resource whose
// backend models two object types behind one kind is a real gate, not a
// check that can only ever pass: the mismatch and empty-name cases below
// must both be classified as failures, not silently accepted. This is
// deliberately a pure-function unit test (no live cluster, no
// deliberately-broken controller build required) — see
// CheckExternalNamePrefix's doc comment for why that is sufficient proof.
func TestCheckExternalNamePrefix(t *testing.T) {
	cases := map[string]struct {
		reason         string
		name           string
		expectedPrefix string
		wantOK         bool
		wantReasonHas  string
	}{
		"MatchingPrefix": {
			reason:         "an external-name that starts with the expected object-type prefix passes",
			name:           testObjectTypePrefix + "abc123/example-object",
			expectedPrefix: testObjectTypePrefix,
			wantOK:         true,
		},
		"WrongObjectType": {
			reason:         "an external-name resolved against the sibling object type must fail the expected type's prefix — this is the exact silent mis-identification hazard the check exists to catch",
			name:           testSiblingObjectTypePrefix + "abc123/example-object",
			expectedPrefix: testObjectTypePrefix,
			wantOK:         false,
			wantReasonHas:  "does not have expected prefix",
		},
		"EmptyExternalName": {
			reason:         "a resource with no resolved external-name yet cannot satisfy any prefix expectation",
			name:           "",
			expectedPrefix: testObjectTypePrefix,
			wantOK:         false,
			wantReasonHas:  "absent or empty",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ok, reason := CheckExternalNamePrefix(tc.name, tc.expectedPrefix)
			if ok != tc.wantOK {
				t.Fatalf("%s: CheckExternalNamePrefix(%q, %q) ok = %v, want %v (reason: %s)",
					tc.reason, tc.name, tc.expectedPrefix, ok, tc.wantOK, reason)
			}
			if !tc.wantOK && !strings.Contains(reason, tc.wantReasonHas) {
				t.Errorf("%s: reason = %q, want substring %q", tc.reason, reason, tc.wantReasonHas)
			}
		})
	}
}

// TestStringifyFieldValue covers the value-to-comparison-string conversion
// used for both status.atProvider and spec.forProvider field reads.
func TestStringifyFieldValue(t *testing.T) {
	cases := map[string]struct {
		reason string
		val    interface{}
		want   string
	}{
		"Nil": {
			reason: "a missing value stringifies to the empty string",
			val:    nil,
			want:   "",
		},
		"String": {
			reason: "strings are returned unquoted",
			val:    "hello",
			want:   "hello",
		},
		"Number": {
			reason: "numbers are returned as canonical JSON",
			val:    float64(10),
			want:   "10",
		},
		"Bool": {
			reason: "booleans are returned as canonical JSON",
			val:    true,
			want:   "true",
		},
		"Map": {
			reason: "maps are returned as canonical JSON, not Go-format",
			val:    map[string]interface{}{"key": "value"},
			want:   `{"key":"value"}`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := stringifyFieldValue(tc.val, "field")
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// manifestWithSequentialFieldTests builds a Manifest whose Tests set the
// same field to n distinct, strictly-increasing values in turn. Reusing one
// field (rather than n distinct fields) keeps the fixture small while still
// guaranteeing every test is a genuine change relative to the one before it
// (never a no-op), because runFieldTest's no-op check compares against
// spec.forProvider, which handlePatch updates in place after every patch.
func manifestWithSequentialFieldTests(n int) *manifest.Manifest {
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	for i := 1; i <= n; i++ {
		m.Tests = append(m.Tests, manifest.UpdateTest{Field: testFieldNotifyDelay, Value: float64(i)})
	}
	return m
}

// TestRunTestsResetsEventBurstBeforeCeiling covers the false-NOT-EVIDENCED
// regression: a resource with more mutable fields than the client-go
// event-spam-filter's ~25-event burst must still report every
// genuinely-converged field as evidenced, because RunTests proactively
// restarts the controller (earning back a fresh burst) before the ceiling is
// reached rather than after events have already been silently dropped.
func TestRunTestsResetsEventBurstBeforeCeiling(t *testing.T) {
	const numFields = eventBurstCeiling + 5 // comfortably past one burst
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		// eventBudget mirrors the real client-go burst ceiling: once a
		// generation's aggregated count reaches it, further same-generation
		// increments are silently dropped, exactly as production
		// eventBurstCeiling is calibrated to stay under.
		eventBudget: eventBurstCeiling,
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(numFields)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != numFields {
		t.Fatalf("got %d results, want %d", len(results), numFields)
	}

	for i, res := range results {
		if res.NotEvidenced {
			t.Errorf("field %d (%s): got NotEvidenced=true, want evidenced (err: %v)", i, res.Field, res.Error)
		}
		if !res.Passed {
			t.Errorf("field %d (%s): got Passed=false, want true (err: %v)", i, res.Field, res.Error)
		}
	}

	if f.restartCalls == 0 {
		t.Error("expected at least one controller restart before the burst ceiling was reached")
	}
	// numFields exceeds one ceiling's worth of attempts, so at least one
	// generation besides the first must have accumulated events — proof the
	// reset actually happened before fields ran out of budget, not merely
	// that restart() was called with no effect on the outcome.
	if len(f.generations) < 2 {
		t.Errorf("expected events to span at least 2 controller generations, got %d: %v", len(f.generations), f.generations)
	}
}

// TestRunTestsMidLoopResetAccountsForMultiEventAttempts is the regression
// covering a mid-run undercount that the ceiling-crossing test above does
// not exercise: attemptsSinceReset assumes exactly one update event per
// field-test attempt, incrementing by 1 regardless of how many events the
// patch actually produced. Measured live on ServicePolicyRule, several
// attempts produced 3 events each (the aggregated count climbed
// 12 → 15 → 18 → 21 → 24), so the real burst was already exhausted while
// attemptsSinceReset was still only at 17 — short of eventBurstCeiling —
// and the mid-loop reset never fired for the fields run in between. Those
// fields came back NOT-EVIDENCED with no EvidenceUntrusted flag: the false
// failure was silent, not merely degraded.
//
// eventsPerPatch: 3 with only numFields: 10 reproduces the same shape at a
// much smaller scale: three events per attempt crosses eventBurstCeiling
// (20) by the 8th field, while attemptsSinceReset (incrementing by 1 per
// attempt) would not reach the ceiling until the 21st — so a run this
// short can only pass if RunTests resets on the REAL measured count, not
// on attemptsSinceReset alone.
func TestRunTestsMidLoopResetAccountsForMultiEventAttempts(t *testing.T) {
	const numFields = 10
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		eventsPerPatch:    3,
		eventBudget:       eventBurstCeiling,
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(numFields)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != numFields {
		t.Fatalf("got %d results, want %d", len(results), numFields)
	}

	for i, res := range results {
		if res.NotEvidenced {
			t.Errorf("field %d (%s): got NotEvidenced=true, want evidenced — a mid-loop reset driven by the real event count, not attemptsSinceReset alone, should have earned a fresh burst before the real ceiling was crossed (err: %v)", i, res.Field, res.Error)
		}
		if !res.Passed {
			t.Errorf("field %d (%s): got Passed=false, want true (err: %v)", i, res.Field, res.Error)
		}
	}

	// attemptsSinceReset alone never reaches eventBurstCeiling across only
	// 10 fields, so any restart recorded here is proof the trigger came
	// from the real measured count, not the 1-per-attempt estimate.
	if f.restartCalls == 0 {
		t.Error("expected a mid-run controller restart driven by the real event count crossing the ceiling before attemptsSinceReset ever would")
	}
}

// TestRunTestsEarnsBurstBeforeFirstFieldWhenAlreadyAtCeiling is ticket
// 6bb473df's regression: creation-time settling (late-init corrections, an
// oneof default settling, a multi-round convergence from empty to a
// complex desired state) can leave an object's event spam-filter burst
// already at or past eventBurstCeiling before RunTests issues its own
// first patch. Measured live: provider-f5xc's HttpLoadbalancer, freshly
// created with six newly-populated slice fields, arrived at its first
// field test with 32 pre-existing update events already recorded against
// it — past the ceiling below. attemptsSinceReset only counts patches THIS
// run issues, so without a pre-run check the first field test's own
// entirely legitimate Update() event is silently dropped by the exhausted
// burst rather than delayed, and no amount of read-side retrying
// (evidenceRetryWindow) can recover a write that never landed — the
// instrumented reproduction showed the aggregated count frozen at exactly
// its pre-existing value for the whole retry window, on every field.
// RunTests must reset the burst BEFORE the first patch when the object
// arrives already at the ceiling, not only after eventBurstCeiling patches
// accumulate within the run itself.
func TestRunTestsEarnsBurstBeforeFirstFieldWhenAlreadyAtCeiling(t *testing.T) {
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		eventBudget:       eventBurstCeiling,
		// generations pre-seeded AT the ceiling — simulating settling that
		// happened entirely before RunTests was ever called. This mirrors
		// what was measured live: the very first eventsBefore read already
		// returns eventBurstCeiling, not a low number climbing from 0.
		generations: []int32{eventBurstCeiling},
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(1)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].NotEvidenced {
		t.Errorf("field 0 (%s): got NotEvidenced=true, want evidenced — a pre-run reset should have earned a fresh burst before this patch (err: %v)", results[0].Field, results[0].Error)
	}
	if !results[0].Passed {
		t.Errorf("field 0 (%s): got Passed=false, want true (err: %v)", results[0].Field, results[0].Error)
	}
	if f.restartCalls == 0 {
		t.Error("expected a controller restart BEFORE the first field test, since the object arrived already at the burst ceiling")
	}
}

// TestRunTestsSkipsPreRunResetWhenBelowCeiling is
// TestRunTestsEarnsBurstBeforeFirstFieldWhenAlreadyAtCeiling's boundary
// counterpart: an object that arrives with pre-existing events but still
// comfortably under the ceiling must not pay a restart it does not need —
// the pre-run check is a targeted response to a specific measured defect,
// not a blanket "always reset first" policy.
func TestRunTestsSkipsPreRunResetWhenBelowCeiling(t *testing.T) {
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		eventBudget:       eventBurstCeiling,
		// One short of the ceiling — must NOT trigger the pre-run reset.
		generations: []int32{eventBurstCeiling - 1},
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(1)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].NotEvidenced {
		t.Errorf("field 0 (%s): got NotEvidenced=true, want evidenced (err: %v)", results[0].Field, results[0].Error)
	}
	if f.restartCalls != 0 {
		t.Errorf("expected no restart when pre-existing events (%d) are below the ceiling (%d), got %d restart(s)", eventBurstCeiling-1, eventBurstCeiling, f.restartCalls)
	}
}

// TestRunTestsWithoutRestartWiringStillDetectsGenuineNonEvidence is the
// counterpart regression case: the burst-reset machinery must never become
// vacuously permissive. A controller that truly never emits update events
// (recordUpdateEvent left false, so no generation ever accumulates anything
// for resetEventBurst to rescue) must still fail with NotEvidenced, restart
// or no restart.
func TestRunTestsWithoutRestartWiringStillDetectsGenuineNonEvidence(t *testing.T) {
	const numFields = eventBurstCeiling + 3
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:  map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
		// recordUpdateEvent left false: no field test's patch ever produces
		// an update event, simulating a reconciler whose Update() path
		// never runs at all.
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(numFields)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}

	for i, res := range results {
		if !res.NotEvidenced {
			t.Errorf("field %d (%s): got NotEvidenced=false, want true (a controller with no Update() events must still fail)", i, res.Field)
		}
		if res.Passed {
			t.Errorf("field %d (%s): got Passed=true, want false", i, res.Field)
		}
	}
}

// TestRunTestsAssertUnchangedGatesOnSilentWipe is the acceptance case for
// the assert-unchanged directive (see manifest.Manifest.AssertUnchanged):
// a manifest declares a field that must survive the whole run, the backend
// (simulated by fakeCluster.silentWipeField) resets it on every write as a
// side effect of an unrelated patch, and RunTests must report a GATING
// violation — not merely a diagnostic — attributing it to the field test
// whose patch was in flight when the drift first appeared.
func TestRunTestsAssertUnchangedGatesOnSilentWipe(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider: map[string]interface{}{
			testFieldNotifyDelay: float64(0),
			"legacyRuleList":     []interface{}{"rule-a"},
		},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		// Every real field patch silently resets legacyRuleList to an empty
		// list — mirroring a backend that defaults an omitted union member
		// on every write, regardless of which field the request touched.
		silentWipeField: "legacyRuleList",
		silentWipeValue: []interface{}{},
	}
	r := newFakeRunner(f)

	m := &manifest.Manifest{
		Kind: testKindExample, Name: testNameExample,
		Tests:           []manifest.UpdateTest{{Field: testFieldNotifyDelay, Value: float64(1)}},
		AssertUnchanged: []string{"legacyRuleList"},
	}

	results, violations, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected the (unrelated) field test itself to pass, got %+v", results)
	}

	if len(violations) != 1 {
		t.Fatalf("got %d assert-unchanged violations, want 1: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Field != "legacyRuleList" {
		t.Errorf("violation Field = %q, want %q", v.Field, "legacyRuleList")
	}
	if v.Baseline != `["rule-a"]` {
		t.Errorf("violation Baseline = %q, want %q", v.Baseline, `["rule-a"]`)
	}
	if v.Observed != "[]" {
		t.Errorf("violation Observed = %q, want %q", v.Observed, "[]")
	}
	if v.AfterField != testFieldNotifyDelay {
		t.Errorf("violation AfterField = %q, want %q (the field whose patch triggered the wipe)", v.AfterField, testFieldNotifyDelay)
	}
}

// TestRunTestsAssertUnchangedPassesWhenFieldHolds is the negative
// counterpart: a manifest that declares an assert-unchanged field whose
// value genuinely survives the whole run reports zero violations, proving
// the directive does not fire on its own — only on an actual observed
// drift.
func TestRunTestsAssertUnchangedPassesWhenFieldHolds(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider: map[string]interface{}{
			testFieldNotifyDelay: float64(0),
			"legacyRuleList":     []interface{}{"rule-a"},
		},
		generation: 1,
		kind:       testKindExample,
		name:       testNameExample,
		// No silentWipeField: the backend behaves correctly.
	}
	r := newFakeRunner(f)

	m := &manifest.Manifest{
		Kind: testKindExample, Name: testNameExample,
		Tests:           []manifest.UpdateTest{{Field: testFieldNotifyDelay, Value: float64(1)}},
		AssertUnchanged: []string{"legacyRuleList"},
	}

	_, violations, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("got %d assert-unchanged violations, want 0: %+v", len(violations), violations)
	}
}

// TestRunTestsAssertUnchangedReportsDriftOnceAcrossMultipleFieldTests
// confirms a wiped field is reported exactly once even though it stays
// wiped for the remainder of the run — the drift is attributed to the
// FIRST field test whose patch exposed it, not to every subsequent one.
func TestRunTestsAssertUnchangedReportsDriftOnceAcrossMultipleFieldTests(t *testing.T) {
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider: map[string]interface{}{
			testFieldNotifyDelay: float64(0),
			"legacyRuleList":     []interface{}{"rule-a"},
		},
		generation:      1,
		kind:            testKindExample,
		name:            testNameExample,
		silentWipeField: "legacyRuleList",
		silentWipeValue: []interface{}{},
	}
	r := newFakeRunner(f)

	m := &manifest.Manifest{
		Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{
			{Field: testFieldNotifyDelay, Value: float64(1)},
			{Field: testFieldFeatureEnabled, Value: true},
		},
		AssertUnchanged: []string{"legacyRuleList"},
	}

	_, violations, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want exactly 1 (reported once, not once per field test): %+v", len(violations), violations)
	}
	if violations[0].AfterField != testFieldNotifyDelay {
		t.Errorf("violation attributed to %q, want %q (the first field test whose patch exposed the drift)",
			violations[0].AfterField, testFieldNotifyDelay)
	}
}

// TestRunTestsBelowCeilingNeverRestarts confirms the proactive reset is
// scoped to the burst ceiling: a run whose non-skipped, non-no-op field
// count never reaches eventBurstCeiling must never pay the restart cost.
func TestRunTestsBelowCeilingNeverRestarts(t *testing.T) {
	const numFields = eventBurstCeiling - 1
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f)

	m := manifestWithSequentialFieldTests(numFields)
	if _, _, err := r.RunTests(m); err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}

	if f.restartCalls != 0 {
		t.Errorf("expected 0 restarts for a run below the burst ceiling, got %d", f.restartCalls)
	}
}

// TestRunTestsRestartFailureDoesNotAbortRun confirms a failed burst reset
// degrades gracefully: the run continues (later fields may lose evidence,
// but the whole test suite must not abort) rather than the tool crashing or
// silently swallowing the rest of the manifest. It also asserts the
// degraded-state signal a reviewer depends on: every field tested from the
// point of the failed reset onward must come back EvidenceUntrusted (not a
// clean, silently-trusted PASS or NotEvidenced), while fields tested before
// the failure are unaffected. This is what stops "0 not-evidenced" from
// ever printing as a clean verdict once a reset has failed mid-run — the
// caller rendering the summary must fail any run containing one.
func TestRunTestsRestartFailureDoesNotAbortRun(t *testing.T) {
	const numFields = eventBurstCeiling + 2
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		eventBudget:       eventBurstCeiling,
	}
	r := newFakeRunner(f)
	r.restartFunc = func() error {
		return fmt.Errorf("simulated rollout failure")
	}

	m := manifestWithSequentialFieldTests(numFields)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != numFields {
		t.Fatalf("got %d results, want %d — a failed restart must not abort the run", len(results), numFields)
	}

	// The proactive reset attempt fires immediately before the field at
	// index eventBurstCeiling (the (eventBurstCeiling+1)th field). Every
	// field before that index ran while the burst was still trusted; every
	// field from that index onward ran after the failed reset.
	for i, res := range results {
		wantUntrusted := i >= eventBurstCeiling
		if res.EvidenceUntrusted != wantUntrusted {
			t.Errorf("field %d (%s): got EvidenceUntrusted=%v, want %v", i, res.Field, res.EvidenceUntrusted, wantUntrusted)
		}
	}

	untrusted := 0
	for _, res := range results {
		if res.EvidenceUntrusted {
			untrusted++
		}
	}
	wantUntrustedCount := numFields - eventBurstCeiling
	if untrusted != wantUntrustedCount {
		t.Errorf("got %d EvidenceUntrusted results, want %d", untrusted, wantUntrustedCount)
	}
}

// TestRunTestsAmbiguousProviderDeploymentDegradesToUntrusted joins the two
// halves of the burst-reset contract: an ambiguous controller-Deployment
// lookup (two provider packages installed, no override) must surface as a
// reset FAILURE, and RunTests must respond by marking every subsequent
// result EvidenceUntrusted rather than reporting a clean run. Before
// resolveControllerDeploymentName learned to refuse to guess, this run
// restarted an arbitrary other provider's controller, reported success, and
// left the untrusted evidence looking pristine.
func TestRunTestsAmbiguousProviderDeploymentDegradesToUntrusted(t *testing.T) {
	t.Setenv(providerDeploymentEnvVar, "")

	const numFields = eventBurstCeiling + 2
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldNotifyDelay: float64(0)},
		atProvider:        map[string]interface{}{testFieldNotifyDelay: float64(0)},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
		eventBudget:       eventBurstCeiling,
		providerPods:      []string{testProviderDeployment, testOtherProviderDeployment},
	}
	r := newFakeRunner(f)
	// Exercise the REAL restart path (kubectl argv through execFunc), not
	// the restartFunc seam — the defect lives in deployment resolution.
	r.restartFunc = nil

	m := manifestWithSequentialFieldTests(numFields)
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != numFields {
		t.Fatalf("got %d results, want %d — an unresolvable restart must not abort the run", len(results), numFields)
	}

	for i, res := range results {
		wantUntrusted := i >= eventBurstCeiling
		if res.EvidenceUntrusted != wantUntrusted {
			t.Errorf("field %d (%s): got EvidenceUntrusted=%v, want %v", i, res.Field, res.EvidenceUntrusted, wantUntrusted)
		}
	}
	if f.rolloutCalls != 0 {
		t.Errorf("got %d rollout calls, want 0 — an ambiguous lookup must never restart an arbitrary provider's controller", f.rolloutCalls)
	}
}

// TestShortenTimeout covers shortenTimeout's parsing, division, flooring,
// and fallback-on-unparseable-input behaviour directly — it is a pure
// function with no cluster dependency.
func TestShortenTimeout(t *testing.T) {
	cases := map[string]struct {
		reason   string
		original string
		divisor  int
		floor    time.Duration
		want     string
	}{
		"DividesCleanly": {
			reason:   "a value comfortably above the floor after division is used as-is",
			original: "120s",
			divisor:  4,
			floor:    15 * time.Second,
			want:     "30s",
		},
		"FloorsWhenDivisionGoesBelowIt": {
			reason:   "a short base timeout divides below the floor and is clamped up to it",
			original: "20s",
			divisor:  4,
			floor:    15 * time.Second,
			want:     "15s",
		},
		"UnparseableFallsBackToDefaultPollInterval": {
			reason:   "an unparseable original falls back to defaultPollInterval before dividing, rather than producing a zero or unbounded window",
			original: "not-a-duration",
			divisor:  4,
			floor:    15 * time.Second,
			want:     (defaultPollInterval / 4).String(),
		},
		"ZeroFallsBackToDefaultPollInterval": {
			reason:   "a zero-valued original is treated the same as unparseable — dividing zero would otherwise silently produce a zero window",
			original: "0s",
			divisor:  4,
			floor:    15 * time.Second,
			want:     (defaultPollInterval / 4).String(),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := shortenTimeout(tc.original, tc.divisor, tc.floor)
			if got != tc.want {
				t.Errorf("%s: shortenTimeout(%q, %d, %s) = %q, want %q", tc.reason, tc.original, tc.divisor, tc.floor, got, tc.want)
			}
		})
	}
}

// TestClassifyKnownDefect covers classifyKnownDefect's verdict inversion in
// isolation, without a live cluster: non-convergence (Passed=false, backed
// by trustworthy evidence one way or the other) is credited as the entry's
// expected outcome; convergence (Passed=true) is flagged KnownDefectConverged;
// and NoOp/EvidenceUntrusted/NotEvidenced results are all left untouched
// because none of the three proves the defect still holds — NotEvidenced in
// particular means the value DID reach its target, so crediting it as
// non-convergence would be the exact false verdict this function exists to
// prevent (see classifyKnownDefect's own doc comment).
func TestClassifyKnownDefect(t *testing.T) {
	const ticketID = "e9ce03ee-920d-46f5-9aa3-120228b196fb"

	cases := map[string]struct {
		reason          string
		in              TestResult
		wantKnownDefect string
		wantConverged   bool
	}{
		"NonConvergenceViaMismatchIsCredited": {
			reason:          "a plain value mismatch is the entry's expected outcome",
			in:              TestResult{Field: "useTls", Passed: false},
			wantKnownDefect: ticketID,
			wantConverged:   false,
		},
		"NotEvidencedIsLeftUntouched": {
			reason:          "the value converged, but with no evidence Update() produced it — the same 'cannot trust this either way' reasoning as EvidenceUntrusted, so it must NOT be credited as expected non-convergence",
			in:              TestResult{Field: "useTls", Passed: false, NotEvidenced: true},
			wantKnownDefect: "",
			wantConverged:   false,
		},
		"NonConvergenceViaErrorIsCredited": {
			reason:          "a reconcile-timeout error is still Passed=false — still credited",
			in:              TestResult{Field: "useTls", Passed: false, Error: fmt.Errorf("waiting for Synced: timed out")},
			wantKnownDefect: ticketID,
			wantConverged:   false,
		},
		"ConvergenceIsFlaggedHardFailure": {
			reason:          "the field actually reached its target with trustworthy evidence — the defect appears fixed",
			in:              TestResult{Field: "useTls", Passed: true},
			wantKnownDefect: ticketID,
			wantConverged:   true,
		},
		"NoOpIsLeftUntouched": {
			reason:          "a no-op patch never exercised the broken path at all — it says nothing about the defect",
			in:              TestResult{Field: "useTls", NoOp: true, Passed: false},
			wantKnownDefect: "",
			wantConverged:   false,
		},
		"EvidenceUntrustedIsLeftUntouched": {
			reason:          "untrusted evidence cannot prove or disprove convergence either way",
			in:              TestResult{Field: "useTls", EvidenceUntrusted: true, Passed: true},
			wantKnownDefect: "",
			wantConverged:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := classifyKnownDefect(tc.in, ticketID)
			if got.KnownDefect != tc.wantKnownDefect {
				t.Errorf("%s: KnownDefect = %q, want %q", tc.reason, got.KnownDefect, tc.wantKnownDefect)
			}
			if got.KnownDefectConverged != tc.wantConverged {
				t.Errorf("%s: KnownDefectConverged = %v, want %v", tc.reason, got.KnownDefectConverged, tc.wantConverged)
			}
		})
	}
}

// TestRunTestsKnownDefectNonConvergenceDoesNotFailTheRun is the end-to-end
// regression test for the exact bug this classification exists to prevent:
// a KnownDefect entry whose patch converges in value but is never evidenced
// (recordUpdateEvent left false, the fakeCluster default) must NOT be
// credited as the entry's expected non-convergence. The field's value DID
// reach its target — only the proof that Update() produced it is missing —
// so classifyKnownDefect must leave KnownDefect empty and let the result
// report through its own NOT-EVIDENCED verdict instead (main.go's
// printResults, not covered here). RunTests itself still returns no error
// either way: the run-level pass/fail decision belongs to printResults/
// cmdRun, never to RunTests. It also proves r.timeout is restored to its
// original value once the shortened-timeout call returns, so a later
// ordinary field test in the same run is not left running under the
// KnownDefect entry's narrowed window.
func TestRunTestsKnownDefectNonConvergenceDoesNotFailTheRun(t *testing.T) {
	const ticketID = "e9ce03ee-920d-46f5-9aa3-120228b196fb"
	f := &fakeCluster{
		forProvider: map[string]interface{}{testFieldFeatureEnabled: false},
		atProvider:  map[string]interface{}{testFieldFeatureEnabled: false},
		generation:  1,
		kind:        testKindExample,
		name:        testNameExample,
		// recordUpdateEvent left false: the value converges (handlePatch
		// always mirrors forProvider into atProvider) but no update event
		// is ever recorded, so the evidence check downgrades Passed to
		// NotEvidenced — this is the "converges + no update event"
		// scenario that used to pass quietly as a credited known defect.
	}
	r := newFakeRunner(f)
	originalTimeout := r.timeout

	m := &manifest.Manifest{
		Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{
			{Field: testFieldFeatureEnabled, Value: true, KnownDefect: ticketID},
		},
	}
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	res := results[0]
	if res.KnownDefect != "" {
		t.Errorf("KnownDefect = %q, want empty — a converged-but-unevidenced result must not be credited as expected non-convergence", res.KnownDefect)
	}
	if !res.NotEvidenced {
		t.Error("NotEvidenced = false, want true — the value converged but no update event was ever recorded")
	}
	if res.KnownDefectConverged {
		t.Error("KnownDefectConverged = true, want false — convergence was never trustworthily evidenced, so this must not be reported as the defect appearing fixed either")
	}
	if r.timeout != originalTimeout {
		t.Errorf("r.timeout after RunTests = %q, want it restored to %q", r.timeout, originalTimeout)
	}
}

// TestRunTestsKnownDefectConvergenceFailsTheRun is
// TestRunTestsKnownDefectNonConvergenceDoesNotFailTheRun's counterpart: a
// KnownDefect entry whose field DOES converge with trustworthy evidence
// (recordUpdateEvent: true) must be reported as KnownDefectConverged — the
// self-retiring half of the mechanism. RunTests itself still returns no
// error: the decision to fail the overall command on this belongs to
// printResults/cmdRun (main.go), which is where the run-level exit code is
// decided; RunTests only reports results.
func TestRunTestsKnownDefectConvergenceFailsTheRun(t *testing.T) {
	const ticketID = "e9ce03ee-920d-46f5-9aa3-120228b196fb"
	f := &fakeCluster{
		forProvider:       map[string]interface{}{testFieldFeatureEnabled: false},
		atProvider:        map[string]interface{}{testFieldFeatureEnabled: false},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f)

	m := &manifest.Manifest{
		Kind: testKindExample, Name: testNameExample,
		Tests: []manifest.UpdateTest{
			{Field: testFieldFeatureEnabled, Value: true, KnownDefect: ticketID},
		},
	}
	results, _, err := r.RunTests(m)
	if err != nil {
		t.Fatalf("RunTests: unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	res := results[0]
	if res.KnownDefect != ticketID {
		t.Errorf("KnownDefect = %q, want %q", res.KnownDefect, ticketID)
	}
	if !res.KnownDefectConverged {
		t.Error("KnownDefectConverged = false, want true — the field genuinely converged with evidence")
	}
	if !res.Passed {
		t.Error("Passed = false, want true — the underlying field test itself passed; the run-level verdict inversion happens in main.go's printResults")
	}
}

// TestRestartControllerDeploymentResolvesViaPodLabel exercises
// restartControllerDeployment's actual kubectl argv through execFunc,
// simulating a fake kubectl that mirrors a live Crossplane v2.2.1 cluster:
// `kubectl get deploy -l pkg.crossplane.io/revision` never matches anything
// (the label is absent from the Deployment's own metadata.labels), while
// `kubectl get pods -l pkg.crossplane.io/revision` resolves the controller
// Pod and its pkg.crossplane.io/revision label carries the exact Deployment
// name. This is the regression test for the bug the restartFunc-injected
// tests could never catch, because they bypass restartControllerDeployment
// entirely.
func TestRestartControllerDeploymentResolvesViaPodLabel(t *testing.T) {
	t.Setenv(providerDeploymentEnvVar, "")

	var gotArgs [][]string
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			gotArgs = append(gotArgs, append([]string(nil), args...))
			switch {
			case len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "deploy":
				// A selector against the Deployment's own metadata.labels
				// never matches on a live cluster — mirror kubectl's
				// real "index out of bounds" failure for an empty list
				// rather than returning an empty string, so a caller that
				// mistakenly still queries "get deploy -l ..." is caught
				// by an error, not a silently-empty name.
				return "", fmt.Errorf("array index out of bounds: index 0, length 0")
			case len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods":
				return testProviderDeployment + "\n", nil
			case args[0] == "rollout" && args[1] == "restart":
				return "", nil
			case args[0] == "rollout" && args[1] == "status":
				return "", nil
			default:
				return "", fmt.Errorf("unexpected kubectl invocation: %v", args)
			}
		},
	}

	if err := r.restartControllerDeployment(); err != nil {
		t.Fatalf("restartControllerDeployment: unexpected error: %v", err)
	}

	var sawPodSelector, sawDeploymentLabelSelector, sawRolloutRestart, sawRolloutStatus bool
	for _, args := range gotArgs {
		if len(args) < 2 {
			continue
		}
		switch {
		case args[0] == kubectlGetSubcommand && args[1] == "pods":
			sawPodSelector = true
			if !containsArg(args, providerDeploymentSelector) {
				t.Errorf("get pods argv missing selector %q: %v", providerDeploymentSelector, args)
			}
		case args[0] == kubectlGetSubcommand && args[1] == "deploy":
			sawDeploymentLabelSelector = true
		case args[0] == "rollout" && args[1] == "restart":
			sawRolloutRestart = true
			if !containsArg(args, "deploy/"+testProviderDeployment) {
				t.Errorf("rollout restart argv missing target %q: %v", "deploy/"+testProviderDeployment, args)
			}
		case args[0] == "rollout" && args[1] == "status":
			sawRolloutStatus = true
			if !containsArg(args, "deploy/"+testProviderDeployment) {
				t.Errorf("rollout status argv missing target %q: %v", "deploy/"+testProviderDeployment, args)
			}
		}
	}
	if !sawPodSelector {
		t.Error("expected restartControllerDeployment to resolve the Deployment name via `kubectl get pods -l ...`")
	}
	if sawDeploymentLabelSelector {
		t.Error("restartControllerDeployment must not query `kubectl get deploy -l ...` — that selector never matches on a live cluster (see providerDeploymentSelector)")
	}
	if !sawRolloutRestart {
		t.Error("expected a `kubectl rollout restart deploy/<name>` call")
	}
	if !sawRolloutStatus {
		t.Error("expected a `kubectl rollout status deploy/<name>` call")
	}
}

// TestRestartControllerDeploymentPodResolutionFailure asserts that a
// resolution failure (no matching Pod — the exact live symptom the
// underlying bug produced) is surfaced as an error rather than silently
// treated as a no-op restart.
func TestRestartControllerDeploymentPodResolutionFailure(t *testing.T) {
	t.Setenv(providerDeploymentEnvVar, "")

	r := &Runner{
		execFunc: func(args []string) (string, error) {
			if len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods" {
				return "", fmt.Errorf("array index out of bounds: index 0, length 0")
			}
			return "", fmt.Errorf("unexpected kubectl invocation: %v", args)
		},
	}

	err := r.restartControllerDeployment()
	if err == nil {
		t.Fatal("expected an error when no controller Pod can be resolved, got nil")
	}
	if !strings.Contains(err.Error(), "resolving provider deployment") {
		t.Errorf("error %q does not indicate deployment resolution failed", err.Error())
	}
}

// TestResolveControllerDeploymentName pins the controller-selection
// contract. The pod selector matches EVERY installed provider package's
// controller Pod, so a cluster running more than one provider offers no way
// to tell which revision belongs to the provider under test. Taking the
// first item silently restarts an unrelated controller: the burst reset
// then does nothing, every later evidence check is quietly degraded to
// UNTRUSTED, and nothing in the output says why. Refusing to guess — and
// naming both the candidates and the override — is strictly better.
func TestResolveControllerDeploymentName(t *testing.T) {
	cases := map[string]struct {
		reason      string
		pods        []string
		override    string
		want        string
		wantErr     bool
		wantErrHas  []string
		wantPodCall bool
	}{
		"SingleProviderInstalled": {
			reason:      "exactly one matching Pod is unambiguous — resolve it, as before",
			pods:        []string{testProviderDeployment},
			want:        testProviderDeployment,
			wantPodCall: true,
		},
		"SeveralPodsOfOneDeployment": {
			reason:      "a scaled-out or mid-rollout controller has several Pods carrying the SAME revision label — that is one deployment, not an ambiguity",
			pods:        []string{testProviderDeployment, testProviderDeployment},
			want:        testProviderDeployment,
			wantPodCall: true,
		},
		"TwoProvidersInstalledIsAmbiguous": {
			reason:      "two provider packages, no override: refuse to guess and name every candidate plus the override variable",
			pods:        []string{testProviderDeployment, testOtherProviderDeployment},
			wantErr:     true,
			wantErrHas:  []string{testProviderDeployment, testOtherProviderDeployment, providerDeploymentEnvVar},
			wantPodCall: true,
		},
		"NoProviderPods": {
			reason:      "an empty match is a resolution failure, not an empty deployment name",
			pods:        []string{},
			wantErr:     true,
			wantErrHas:  []string{providerDeploymentSelector},
			wantPodCall: true,
		},
		"OverrideWinsOverAmbiguity": {
			reason:      "an explicit override always wins and skips the lookup entirely, which is what makes a multi-provider cluster testable at all",
			pods:        []string{testProviderDeployment, testOtherProviderDeployment},
			override:    testOtherProviderDeployment,
			want:        testOtherProviderDeployment,
			wantPodCall: false,
		},
		"OverrideWinsOverSinglePod": {
			reason:      "the override is unconditional: it is not a fallback consulted only when the lookup is ambiguous",
			pods:        []string{testProviderDeployment},
			override:    testOtherProviderDeployment,
			want:        testOtherProviderDeployment,
			wantPodCall: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(providerDeploymentEnvVar, tc.override)

			f := &fakeCluster{providerPods: tc.pods}
			r := newFakeRunner(f)
			var sawPodCall bool
			r.execFunc = func(args []string) (string, error) {
				if len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods" {
					sawPodCall = true
				}
				return f.exec(args)
			}

			got, err := r.resolveControllerDeploymentName()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: expected an error, got %q", tc.reason, got)
				}
				for _, want := range tc.wantErrHas {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: error %q does not mention %q", tc.reason, err.Error(), want)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", tc.reason, err)
				}
				if got != tc.want {
					t.Errorf("%s: resolveControllerDeploymentName() = %q, want %q", tc.reason, got, tc.want)
				}
			}
			if sawPodCall != tc.wantPodCall {
				t.Errorf("%s: pod lookup issued = %v, want %v", tc.reason, sawPodCall, tc.wantPodCall)
			}
		})
	}
}

// TestParseControllerPodIdentities pins the parser's tolerance contract: a
// line missing its timestamp component, or carrying one that fails RFC3339
// parsing, reports a zero CreatedAt rather than an error — read by every
// caller as "very old". This is what keeps resolveControllerPodIdentity
// from erroring on a shape it cannot make sense of, and (deliberately) what
// keeps every pre-existing fakeCluster-backed test — whose "get pods"
// stand-in has never emitted a creationTimestamp — resolving an
// already-settled identity without any test-side changes.
func TestParseControllerPodIdentities(t *testing.T) {
	fixedTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := map[string]struct {
		reason string
		in     string
		want   []controllerPodIdentity
	}{
		"Empty": {
			reason: "no output at all is zero identities, not an error",
			in:     "",
			want:   nil,
		},
		"BlankLinesIgnored": {
			reason: "blank lines between entries (kubectl's own trailing newline, or an empty range iteration) contribute nothing",
			in:     "\n\npod-a\t2026-01-02T03:04:05Z\n\n",
			want:   []controllerPodIdentity{{Name: "pod-a", CreatedAt: fixedTime}},
		},
		"SingleValidEntry": {
			reason: "a well-formed name+RFC3339 pair parses exactly",
			in:     "pod-a\t2026-01-02T03:04:05Z\n",
			want:   []controllerPodIdentity{{Name: "pod-a", CreatedAt: fixedTime}},
		},
		"MultipleEntries": {
			reason: "one entry per Pod, in the order the range emitted them",
			in:     "pod-a\t2026-01-02T03:04:05Z\npod-b\t2026-01-02T03:05:00Z\n",
			want: []controllerPodIdentity{
				{Name: "pod-a", CreatedAt: fixedTime},
				{Name: "pod-b", CreatedAt: fixedTime.Add(55 * time.Second)},
			},
		},
		"MissingTimestampComponent": {
			reason: "a name with no tab at all (the shape every pre-existing fakeCluster get-pods stand-in emits) reports a zero CreatedAt rather than erroring",
			in:     "pod-a\n",
			want:   []controllerPodIdentity{{Name: "pod-a"}},
		},
		"UnparsableTimestamp": {
			reason: "a timestamp that fails RFC3339 parsing degrades to zero CreatedAt rather than propagating a parse error the caller has no way to act on",
			in:     "pod-a\tnot-a-timestamp\n",
			want:   []controllerPodIdentity{{Name: "pod-a"}},
		},
		"WhitespacePaddedLines": {
			reason: "leading/trailing whitespace around a line is trimmed before splitting",
			in:     "  pod-a\t2026-01-02T03:04:05Z  \n",
			want:   []controllerPodIdentity{{Name: "pod-a", CreatedAt: fixedTime}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseControllerPodIdentities(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: parseControllerPodIdentities(%q) = %+v, want %+v", tc.reason, tc.in, got, tc.want)
			}
		})
	}
}

// TestLatestControllerPodIdentity pins the "pick the currently running Pod"
// contract: among several entries (a rollout can briefly report the old
// Pod mid-termination alongside the new one), the one with the GREATEST
// CreatedAt is the one currently running.
func TestLatestControllerPodIdentity(t *testing.T) {
	older := controllerPodIdentity{Name: "pod-old", CreatedAt: time.Unix(100, 0)}
	newer := controllerPodIdentity{Name: "pod-new", CreatedAt: time.Unix(200, 0)}

	cases := map[string]struct {
		reason string
		in     []controllerPodIdentity
		want   controllerPodIdentity
		wantOK bool
	}{
		"Empty": {
			reason: "no entries at all is a resolution failure, not a zero-value identity",
			in:     nil,
			wantOK: false,
		},
		"SingleEntry": {
			reason: "one entry is unambiguously the answer",
			in:     []controllerPodIdentity{older},
			want:   older,
			wantOK: true,
		},
		"NewestWinsRegardlessOfOrder": {
			reason: "a rollout can report the old Pod either before or after the new one — the NEWEST CreatedAt wins either way",
			in:     []controllerPodIdentity{older, newer},
			want:   newer,
			wantOK: true,
		},
		"NewestWinsReversedOrder": {
			reason: "same as above with the list order reversed, proving the result does not depend on input order",
			in:     []controllerPodIdentity{newer, older},
			want:   newer,
			wantOK: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := latestControllerPodIdentity(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("%s: found = %v, want %v", tc.reason, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("%s: latestControllerPodIdentity() = %+v, want %+v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestResolveControllerPodIdentityLive exercises the real kubectl argv
// resolveControllerPodIdentityLive issues: it must resolve the Deployment
// name first (exactly as restartControllerDeployment does), then scope its
// Pod lookup to THAT Deployment's revision label value specifically —
// never the bare selector every installed provider's Pods also match — and
// parse the newest entry out of the result.
func TestResolveControllerPodIdentityLive(t *testing.T) {
	t.Setenv(providerDeploymentEnvVar, "")

	var gotPodArgs []string
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			switch {
			case len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "deploy":
				return "", fmt.Errorf("array index out of bounds: index 0, length 0")
			case len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods" && containsArg(args, "-o") && strings.Contains(args[len(args)-1], "labels"):
				// resolveControllerDeploymentName's own lookup.
				return testProviderDeployment + "\n", nil
			case len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods":
				gotPodArgs = args
				return "provider-example-7c9d4f6b7-abcde\t2026-01-02T03:04:05Z\nprovider-example-7c9d4f6b7-fghij\t2026-01-02T03:05:00Z\n", nil
			default:
				return "", fmt.Errorf("unexpected kubectl invocation: %v", args)
			}
		},
	}

	got, err := r.resolveControllerPodIdentityLive()
	if err != nil {
		t.Fatalf("resolveControllerPodIdentityLive: unexpected error: %v", err)
	}
	want := controllerPodIdentity{Name: "provider-example-7c9d4f6b7-fghij", CreatedAt: time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)}
	if got != want {
		t.Errorf("resolveControllerPodIdentityLive() = %+v, want %+v (the newer of the two Pods)", got, want)
	}
	if gotPodArgs == nil {
		t.Fatal("expected an identity-lookup `kubectl get pods` call")
	}
	if !containsArg(gotPodArgs, providerDeploymentSelector+"="+testProviderDeployment) {
		t.Errorf("identity lookup argv missing selector scoped to the resolved Deployment (%s=%s): %v",
			providerDeploymentSelector, testProviderDeployment, gotPodArgs)
	}
}

// TestResolveControllerPodIdentityLivePropagatesDeploymentResolutionFailure
// asserts that a Deployment-resolution failure (no matching Pod at all) is
// surfaced as an error naming that step, rather than silently reporting a
// zero-value identity.
func TestResolveControllerPodIdentityLivePropagatesDeploymentResolutionFailure(t *testing.T) {
	t.Setenv(providerDeploymentEnvVar, "")

	r := &Runner{
		execFunc: func(args []string) (string, error) {
			if len(args) >= 2 && args[0] == kubectlGetSubcommand && args[1] == "pods" {
				return "", fmt.Errorf("array index out of bounds: index 0, length 0")
			}
			return "", fmt.Errorf("unexpected kubectl invocation: %v", args)
		},
	}

	_, err := r.resolveControllerPodIdentityLive()
	if err == nil {
		t.Fatal("expected an error when the Deployment cannot be resolved, got nil")
	}
	if !strings.Contains(err.Error(), "resolving provider deployment") {
		t.Errorf("error %q does not indicate Deployment resolution failed", err.Error())
	}
}

// TestWaitControllerPodSettled pins the bounded-poll contract: a Pod
// already older than the threshold settles on the very first read with no
// sleep, and a Pod that never ages past the threshold reports settled=false
// once the timeout elapses rather than blocking forever.
func TestWaitControllerPodSettled(t *testing.T) {
	t.Run("AlreadySettledNoSleep", func(t *testing.T) {
		var slept int
		r := &Runner{
			podIdentityFunc: func() (controllerPodIdentity, error) {
				return controllerPodIdentity{Name: "pod-a", CreatedAt: time.Now().Add(-time.Hour)}, nil
			},
			sleepFunc: func(time.Duration) { slept++ },
		}

		identity, settled, err := r.waitControllerPodSettled(15*time.Second, time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !settled {
			t.Fatal("expected settled=true for a Pod already older than the threshold")
		}
		if identity.Name != "pod-a" {
			t.Errorf("identity.Name = %q, want %q", identity.Name, "pod-a")
		}
		if slept != 0 {
			t.Errorf("expected zero sleeps when already settled, got %d", slept)
		}
	})

	t.Run("NeverSettlesWithinTimeout", func(t *testing.T) {
		var calls int
		r := &Runner{
			podIdentityFunc: func() (controllerPodIdentity, error) {
				calls++
				return controllerPodIdentity{Name: "pod-a", CreatedAt: time.Now()}, nil
			},
			sleepFunc: func(time.Duration) {},
		}

		identity, settled, err := r.waitControllerPodSettled(time.Hour, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if settled {
			t.Fatal("expected settled=false: the Pod's age never reaches a 1-hour threshold within a 10ms timeout")
		}
		if identity.Name != "pod-a" {
			t.Errorf("identity.Name = %q, want %q", identity.Name, "pod-a")
		}
		if calls < 2 {
			t.Errorf("expected the loop to poll more than once before the timeout, got %d call(s)", calls)
		}
	})

	t.Run("IdentityErrorPropagates", func(t *testing.T) {
		wantErr := fmt.Errorf("boom")
		r := &Runner{
			podIdentityFunc: func() (controllerPodIdentity, error) {
				return controllerPodIdentity{}, wantErr
			},
		}

		_, settled, err := r.waitControllerPodSettled(15*time.Second, time.Second)
		if err == nil {
			t.Fatal("expected the identity error to propagate")
		}
		if settled {
			t.Error("settled must be false when the identity read itself failed")
		}
	})
}

// TestSelectResourceName pins the multi-document resolution contract.
// `kubectl get -f <manifest> -o name` prints one line PER DOCUMENT, and the
// manifest parser selects the annotated document — so the runner must
// select the line that corresponds to it. Taking the whole output verbatim
// turns every later kubectl call into one issued against a multi-line
// garbage name as soon as the manifest ships a companion object.
func TestSelectResourceName(t *testing.T) {
	const (
		crLine        = testResourceIdentifier
		secretLine    = "secret/" + testNameExample + "-credentials"
		sameNameLine  = "secret/" + testNameExample
		otherKindLine = "otherresource.example.crossplane.io/" + testNameExample
	)

	cases := map[string]struct {
		reason  string
		out     string
		kind    string
		name    string
		want    string
		wantErr bool
	}{
		"SingleDocument": {
			reason: "the unchanged single-document case: the only line is the resource",
			out:    crLine + "\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   crLine,
		},
		"CompanionObjectListedFirst": {
			reason: "a companion Secret is routinely written before the managed resource — the CR's line must still be the one selected",
			out:    secretLine + "\n" + crLine + "\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   crLine,
		},
		"CompanionObjectSharesTheName": {
			reason: "a companion object named identically to the CR is common (a Secret holding its credentials); the kind is what breaks the tie",
			out:    sameNameLine + "\n" + crLine + "\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   crLine,
		},
		"BlankLinesIgnored": {
			reason: "a trailing separator or blank line must not be mistaken for a candidate",
			out:    "\n" + crLine + "\n\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   crLine,
		},
		"PluralResourceSegment": {
			reason: "kubectl renders the type segment as the singular or the plural of the Kind depending on the resource and version; both must match",
			out:    "secret/" + testNameExample + "\n" + "exampleresources.example.crossplane.io/" + testNameExample + "\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   "exampleresources.example.crossplane.io/" + testNameExample,
		},
		"NoLineMatchesTheName": {
			reason:  "an output holding only unrelated objects is a resolution failure, not a name to pass on",
			out:     secretLine + "\n",
			kind:    testKindExample,
			name:    testNameExample,
			wantErr: true,
		},
		"EmptyOutput": {
			reason:  "an empty output is a resolution failure",
			out:     "\n",
			kind:    testKindExample,
			name:    testNameExample,
			wantErr: true,
		},
		"AmbiguousEvenAfterKind": {
			reason:  "two same-named objects of the same kind in different groups cannot be told apart — refuse rather than pick one",
			out:     crLine + "\n" + "exampleresource.other.crossplane.io/" + testNameExample + "\n",
			kind:    testKindExample,
			name:    testNameExample,
			wantErr: true,
		},
		"NameMatchesButKindDoesNot": {
			reason: "a single name match is accepted on the name alone: kubectl's type rendering is not stable enough to reject on, and there is nothing to disambiguate",
			out:    otherKindLine + "\n",
			kind:   testKindExample,
			name:   testNameExample,
			want:   otherKindLine,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := selectResourceName(tc.out, tc.kind, tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: expected an error, got %q", tc.reason, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: selectResourceName() = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// TestResolveResourceMultiDocumentSelectsMatchingName drives the fix
// through the Runner: when `kubectl get -f <manifest> -o name` prints
// several lines (a companion object plus the CR under test), the cached
// resourceName must be the CR's line alone — never the whole multi-line
// output, which every subsequent kubectl call would then be issued against.
func TestResolveResourceMultiDocumentSelectsMatchingName(t *testing.T) {
	f := &fakeCluster{
		resourceLines: "secret/" + testNameExample + "-credentials\n" + testResourceIdentifier + "\n",
	}
	r := newFakeRunner(f)

	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	if err := r.ResolveResource(m); err != nil {
		t.Fatalf("ResolveResource: unexpected error: %v", err)
	}
	if r.resourceName != testResourceIdentifier {
		t.Errorf("resourceName = %q, want the CR's own identifier %q", r.resourceName, testResourceIdentifier)
	}
}

// TestResolveResourceNoMatchingDocumentErrors confirms an unresolvable
// output fails loudly instead of being cached and carried into every later
// kubectl call.
func TestResolveResourceNoMatchingDocumentErrors(t *testing.T) {
	f := &fakeCluster{resourceLines: "secret/unrelated-object\n"}
	r := newFakeRunner(f)

	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}
	err := r.ResolveResource(m)
	if err == nil {
		t.Fatal("expected an error when no line matches the manifest's name")
	}
	if !strings.Contains(err.Error(), testNameExample) {
		t.Errorf("error %q does not name the resource it failed to resolve", err.Error())
	}
}

// TestResolveResourceRecordsNamespace confirms the resolved namespace is
// carried onto the Runner, so every later kubectl call is namespace-scoped.
func TestResolveResourceRecordsNamespace(t *testing.T) {
	f := &fakeCluster{}
	r := newFakeRunner(f)

	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample, Namespace: "team-a"}
	if err := r.ResolveResource(m); err != nil {
		t.Fatalf("ResolveResource: unexpected error: %v", err)
	}
	if r.namespace != "team-a" {
		t.Errorf("namespace = %q, want %q", r.namespace, "team-a")
	}
}

// TestBuildMergePatch covers the dot-path-to-JSON-merge-patch conversion,
// including the nested case where each segment becomes a wrapping object,
// and the "clear" case where OTHER top-level forProvider siblings are
// nulled in the SAME patch object as the primary field's value.
func TestBuildMergePatch(t *testing.T) {
	cases := map[string]struct {
		reason string
		field  string
		value  interface{}
		clear  []string
		want   string
	}{
		"TopLevelField": {
			reason: "a single segment patches one key under spec.forProvider",
			field:  testFieldNotifyDelay,
			value:  10,
			want:   `{"spec":{"forProvider":{"notifyDelay":10}}}`,
		},
		"NestedField": {
			reason: "a dot path becomes nested objects, not a literal dotted key",
			field:  "parent.child",
			value:  "value",
			want:   `{"spec":{"forProvider":{"parent":{"child":"value"}}}}`,
		},
		"ClearSingleSibling": {
			reason: "a top-level field with one clear entry nulls that sibling in the SAME patch object",
			field:  "botProtectionSetting",
			value:  map[string]interface{}{},
			clear:  []string{"defaultBotSetting"},
			want:   `{"spec":{"forProvider":{"botProtectionSetting":{},"defaultBotSetting":null}}}`,
		},
		"ClearMultipleSiblings": {
			reason: "every named sibling is nulled, not just the first",
			field:  "armA",
			value:  "x",
			clear:  []string{"armB", "armC"},
			want:   `{"spec":{"forProvider":{"armA":"x","armB":null,"armC":null}}}`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := buildMergePatch(tc.field, tc.value, tc.clear)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: buildMergePatch() = %s, want %s", tc.reason, got, tc.want)
			}
		})
	}
}

// TestBuildMergePatchRejectsUnsupportedClearShapes proves the three shapes
// buildMergePatch cannot honour are rejected outright, at patch-build time,
// rather than silently producing a merge patch that nulls the wrong object
// or undoes the very value it just set. Nested (dotted) field paired with a
// non-empty clear is explicitly OUT OF SCOPE — sibling-clearing at a
// non-root nesting level is not supported — so it is one of the rejected
// shapes here rather than a shape that "falls out for free".
func TestBuildMergePatchRejectsUnsupportedClearShapes(t *testing.T) {
	cases := map[string]struct {
		reason string
		field  string
		clear  []string
	}{
		"NestedFieldWithClear": {
			reason: "sibling-clearing at a non-root nesting level is not supported: a dotted field's " +
				"\"sibling\" would land next to the nested field's own parent object, not next to its value",
			field: "parent.child",
			clear: []string{"otherTopLevel"},
		},
		"DottedClearEntry": {
			reason: "clear only names a top-level spec.forProvider field, mirroring ignore-fields' own dot rejection",
			field:  "botProtectionSetting",
			clear:  []string{"nested.sibling"},
		},
		"ClearNamesFieldItself": {
			reason: "clear must name OTHER siblings; naming field itself would null the value the patch just set",
			field:  "botProtectionSetting",
			clear:  []string{"botProtectionSetting"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildMergePatch(tc.field, "value", tc.clear)
			if err == nil {
				t.Fatalf("%s: expected an error, got none", tc.reason)
			}
		})
	}
}

// containsArg reports whether want appears anywhere in args.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestCompareFieldValue covers compareFieldValue directly: with an empty
// ignoreKeys it must behave exactly like jsonEqual (every existing caller's
// contract), and with a non-empty ignoreKeys it must strip the named
// top-level keys from BOTH sides before comparing, tolerating a side that
// is not a JSON object (a still-converging read, or a genuinely
// non-map-typed field) by simply not matching rather than erroring.
func TestCompareFieldValue(t *testing.T) {
	cases := map[string]struct {
		reason     string
		expected   interface{}
		actual     string
		ignoreKeys []string
		want       bool
	}{
		"EmptyIgnoreKeysScalarMatch": {
			reason:   "no ignoreKeys means this is exactly jsonEqual for a scalar",
			expected: "hello",
			actual:   "hello",
			want:     true,
		},
		"EmptyIgnoreKeysMapMismatch": {
			reason:     "with no ignoreKeys, an extra actual key is an ordinary mismatch",
			expected:   map[string]interface{}{"a": "1"},
			actual:     `{"a":"1","ownerStamp":"xyz"}`,
			ignoreKeys: nil,
			want:       false,
		},
		"IgnoredKeyStrippedFromActual": {
			reason:     "a key named in ignoreKeys is removed from actual before comparing, so an unpredictable provider-injected value never has to be named",
			expected:   map[string]interface{}{"a": "1"},
			actual:     `{"a":"1","ownerStamp":"xyz-unpredictable"}`,
			ignoreKeys: []string{"ownerStamp"},
			want:       true,
		},
		"IgnoredKeyStrippedFromBothSides": {
			reason:     "stripping is symmetric — an ignored key present on the expected side too is also removed",
			expected:   map[string]interface{}{"a": "1", "ownerStamp": "whatever-the-author-guessed"},
			actual:     `{"a":"1","ownerStamp":"xyz-unpredictable"}`,
			ignoreKeys: []string{"ownerStamp"},
			want:       true,
		},
		"NonIgnoredMismatchStillFails": {
			reason:     "ignoreKeys only exempts the named keys — every other key still has to match",
			expected:   map[string]interface{}{"a": "1"},
			actual:     `{"a":"2","ownerStamp":"xyz"}`,
			ignoreKeys: []string{"ownerStamp"},
			want:       false,
		},
		"ActualNotYetAnObjectDuringConvergence": {
			reason:     "a still-converging field (e.g. empty string before the first Observe) never satisfies the comparison, but must not panic or error",
			expected:   map[string]interface{}{"a": "1"},
			actual:     "",
			ignoreKeys: []string{"ownerStamp"},
			want:       false,
		},
		"ExpectedNotAnObject": {
			reason:     "ignoreKeys set on a scalar-typed comparison has nothing to strip on the expected side; it simply never matches an object actual",
			expected:   "hello",
			actual:     `{"a":"1"}`,
			ignoreKeys: []string{"ownerStamp"},
			want:       false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := compareFieldValue(tc.expected, tc.actual, tc.ignoreKeys)
			if got != tc.want {
				t.Errorf("%s: compareFieldValue(%v, %q, %v) = %v, want %v",
					tc.reason, tc.expected, tc.actual, tc.ignoreKeys, got, tc.want)
			}
		})
	}
}

// TestJSONEqualNilExpectationSatisfiedByAbsentActual is the regression pin
// for the self-tombstone convergence defect: an explicit `value: null`
// entry (manifest.UpdateTest.ValueExplicit) carries an expected value of Go
// nil, and once the merge patch has actually cleared the field,
// ReadField/stringifyFieldValue can only ever report "" for it — the same
// string a genuinely-absent field produces (see navigateJSONPath's own doc
// comment for why that collapse is deliberate). Before this fix, jsonEqual
// demanded the literal string "null", which ReadField can never produce, so
// a null expectation could never converge and the field test failed by
// timeout precisely BECAUSE the tombstone worked. The `expected == nil, actual
// == ""` case is the only one this test exists to prove; every other
// combination is an ordinary mismatch and must stay one.
func TestJSONEqualNilExpectationSatisfiedByAbsentActual(t *testing.T) {
	cases := map[string]struct {
		reason   string
		expected interface{}
		actual   string
		want     bool
	}{
		"NilExpectedAbsentActualConverges": {
			reason:   "the defect this test exists to close: a cleared field must satisfy its null expectation",
			expected: nil,
			actual:   "",
			want:     true,
		},
		"NilExpectedNonEmptyActualStillMismatches": {
			reason:   "a null expectation must not vacuously match a field that has not converged yet",
			expected: nil,
			actual:   "still-here",
			want:     false,
		},
		"NilExpectedLiteralNullStringStillMismatches": {
			reason:   "ReadField never produces the 4-byte string \"null\" — a caller that somehow got it is not the case this fix targets",
			expected: nil,
			actual:   "null",
			want:     false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := jsonEqual(tc.expected, tc.actual); got != tc.want {
				t.Errorf("%s: jsonEqual(%#v, %q) = %v, want %v", tc.reason, tc.expected, tc.actual, got, tc.want)
			}
			// compareFieldValue with no ignoreKeys must agree exactly —
			// the contract TestCompareFieldValue's own doc comment states.
			if got := compareFieldValue(tc.expected, tc.actual, nil); got != tc.want {
				t.Errorf("%s: compareFieldValue(%#v, %q, nil) = %v, want %v", tc.reason, tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

// TestRunFieldTestIgnoreMapKeysProviderInjectedMember is the end-to-end
// proof this mechanism exists for: a map-typed field (extAttrs) whose live
// status.atProvider value carries BOTH the keys the manifest manages and a
// stable member the PROVIDER itself writes (an identity stamp) — modelled
// here the same way infobloxnios's identity.OwnExtAttrs mirrors one into
// atProvider without ever appearing in spec.forProvider.
//
// One patch exercises all three of add, update and null-tombstone removal
// in a single merge — add: newKey, update: existingKey, remove: removeMe —
// and "expect:" (via IgnoreMapKeys) never has to name, let alone predict,
// the provider-injected ownerStamp member. Without IgnoreMapKeys this same
// entry could never pass: the whole-map comparison would have to match
// ownerStamp's live value exactly, and that value is set up here to look
// exactly as unpredictable as a real metadata.uid-derived stamp would be —
// this test never hardcodes it into the expectation.
func TestRunFieldTestIgnoreMapKeysProviderInjectedMember(t *testing.T) {
	const ownerStampValue = "stamp-derived-from-a-uid-the-manifest-cannot-know-in-advance"

	f := &fakeCluster{
		forProvider: map[string]interface{}{
			"extAttrs": map[string]interface{}{
				"existingKey": "orig",
				"removeMe":    "toBeGone",
			},
		},
		atProvider: map[string]interface{}{
			"extAttrs": map[string]interface{}{
				"existingKey": "orig",
				"removeMe":    "toBeGone",
				"ownerStamp":  ownerStampValue,
			},
		},
		generation:        1,
		kind:              testKindExample,
		name:              testNameExample,
		recordUpdateEvent: true,
	}
	r := newFakeRunner(f)
	snapshot, err := json.Marshal(f.atProvider)
	if err != nil {
		t.Fatalf("marshalling snapshot: %v", err)
	}

	test := manifest.UpdateTest{
		Field: "extAttrs",
		Value: map[string]interface{}{
			"existingKey": "updated", // update
			"removeMe":    nil,       // null-tombstone removal
			"newKey":      "added",   // add
		},
		Expect: map[string]interface{}{
			"existingKey": "updated",
			"newKey":      "added",
			// removeMe and ownerStamp are both correctly absent: removeMe
			// because it was removed, ownerStamp because IgnoreMapKeys
			// exempts it from the comparison entirely.
		},
		IgnoreMapKeys: []string{"ownerStamp"},
	}

	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample, "", "")

	if !result.Passed {
		t.Fatalf("expected Passed=true, got %+v (error: %v)", result, result.Error)
	}
	if result.NoOp {
		t.Fatal("expected NoOp=false — the patch changes existingKey, removes removeMe and adds newKey")
	}

	gotExtAttrs, ok := f.atProvider["extAttrs"].(map[string]interface{})
	if !ok {
		t.Fatalf("f.atProvider[extAttrs] = %v, want a map", f.atProvider["extAttrs"])
	}
	if gotExtAttrs["ownerStamp"] != ownerStampValue {
		t.Errorf("ownerStamp = %v, want it left untouched at %q — the provider-injected member must survive the patch unmentioned",
			gotExtAttrs["ownerStamp"], ownerStampValue)
	}
	if _, stillPresent := gotExtAttrs["removeMe"]; stillPresent {
		t.Error("removeMe is still present after a null-tombstone patch")
	}
	if gotExtAttrs["existingKey"] != "updated" || gotExtAttrs["newKey"] != "added" {
		t.Errorf("gotExtAttrs = %v, want existingKey=updated and newKey=added", gotExtAttrs)
	}
}

// TestRunFieldTestIgnoreMapKeysAbsentStillRequiresExactMatch is the negative
// control for TestRunFieldTestIgnoreMapKeysProviderInjectedMember: the exact
// same expected-vs-actual pair, but without ignoreMapKeys naming the
// provider-injected key, must NOT compare equal — proving the whole-map
// comparison really was unsatisfiable before this mechanism, not merely
// untested.
//
// This is checked at the compareFieldValue level rather than through a live
// runFieldTest: pollField's retry sleep is a fixed real-wall-clock interval
// unrelated to Runner.timeout, so reproducing a genuine poll-to-timeout
// failure here would cost several real seconds for no additional coverage —
// runFieldTest's own comparison is exactly compareFieldValue, exercised
// directly by TestCompareFieldValue's "EmptyIgnoreKeysMapMismatch" case
// against this identical expected/actual pair.
func TestRunFieldTestIgnoreMapKeysAbsentStillRequiresExactMatch(t *testing.T) {
	expected := map[string]interface{}{"existingKey": "updated"}
	actual := `{"existingKey":"updated","ownerStamp":"unpredictable-value"}`

	if compareFieldValue(expected, actual, nil) {
		t.Fatal("expected compareFieldValue to fail without ignoreMapKeys naming ownerStamp")
	}
	if !compareFieldValue(expected, actual, []string{"ownerStamp"}) {
		t.Fatal("expected compareFieldValue to pass once ignoreMapKeys names ownerStamp")
	}
}

// outputSpecOf returns the value of a kubectl argv's -o flag, or "" when
// there is none.
func outputSpecOf(args []string) string {
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestExecCapturesKubectlStderr exercises the REAL exec() path — r.kubectl
// pointed at a fake script on disk — rather than the execFunc test seam
// every other test in this file uses. execFunc short-circuits ahead of the
// exec.Command construction this bug lives in (Go's os/exec only populates
// ExitError.Stderr when the caller leaves cmd.Stderr nil), so it is the one
// test that can prove or disprove the fix.
//
// It asserts two things a fix must hold simultaneously: the failing
// command's stderr text reaches the returned error, AND it still reaches
// os.Stderr live (the tee, not a switch from one destination to the other).
func TestExecCapturesKubectlStderr(t *testing.T) {
	const sentinel = "timed out waiting for the condition"

	script := writeFakeKubectlStderrScript(t, sentinel)
	r := &Runner{kubectl: script}

	streamed := captureOSStderr(t, func() {
		_, err := r.exec("get", "widget")
		if err == nil {
			t.Fatal("exec() returned a nil error for a non-zero exit")
		}
		if !strings.Contains(err.Error(), sentinel) {
			t.Errorf("exec() error = %q, want it to contain %q", err.Error(), sentinel)
		}
	})

	if !strings.Contains(streamed, sentinel) {
		t.Errorf("live-streamed stderr = %q, want it to contain %q", streamed, sentinel)
	}
}

// writeFakeKubectlStderrScript writes an executable shell script to a
// t.TempDir() that writes sentinel to stderr and exits 1, standing in for a
// failing kubectl invocation. It returns the script's path.
func writeFakeKubectlStderrScript(t *testing.T, sentinel string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "fake-kubectl")
	body := fmt.Sprintf("#!/bin/sh\necho %q 1>&2\nexit 1\n", sentinel)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture, needs to be executable
		t.Fatalf("write fake kubectl script: %v", err)
	}
	return path
}

// captureOSStderr redirects os.Stderr to an in-memory pipe for the duration
// of fn, restores it afterward, and returns everything written during fn.
func captureOSStderr(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = orig

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}
