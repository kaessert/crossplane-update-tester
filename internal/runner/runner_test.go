package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// the kind/name passed into runFieldTest.
	kind string
	name string

	// resourceLines, when non-empty, overrides what
	// `kubectl get -f <manifest> -o name` prints — one line per manifest
	// document. Left empty, a single line naming the resource under test is
	// returned.
	resourceLines string

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
	metadata := map[string]interface{}{"generation": f.generation}
	if f.externalName != "" {
		metadata["annotations"] = map[string]interface{}{
			externalNameAnnotation: f.externalName,
		}
	}
	status := map[string]interface{}{jsonKeyAtProvider: f.atProvider}
	if cond, ok := f.readyCondition(); ok {
		status["conditions"] = []interface{}{cond}
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

	f.generation++
	f.patchCalls++
	if f.recordUpdateEvent {
		if len(f.generations) == 0 {
			f.generations = append(f.generations, 0)
		}
		idx := len(f.generations) - 1
		if f.eventBudget <= 0 || f.generations[idx] < f.eventBudget {
			f.generations[idx]++
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

func newFakeRunner(f *fakeCluster) *Runner {
	return &Runner{
		resourceName: testResourceIdentifier,
		timeout:      "5s",
		execFunc:     f.exec,
		restartFunc:  f.restart,
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
			result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
	result, newSnapshot := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
	result, newSnapshot := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
	if f.nudgeCalls != 1 {
		t.Errorf("expected exactly 1 nudge call (forcing the second reconcile), got %d", f.nudgeCalls)
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
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
	result, _ := r.runFieldTest(test, snapshot, testKindExample, testNameExample)

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
// containers, missing leaves, and non-object intermediates.
func TestNavigateJSONPath(t *testing.T) {
	cases := map[string]struct {
		reason  string
		obj     map[string]interface{}
		field   string
		want    interface{}
		wantErr bool
	}{
		"TopLevelField": {
			reason: "a direct child of atProvider resolves",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"name": "example"},
				},
			},
			field: "name",
			want:  "example",
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
			field: "parent.child",
			want:  "value",
		},
		"MissingContainer": {
			reason: "a missing status key returns nil, nil rather than erroring",
			obj:    map[string]interface{}{},
			field:  "name",
			want:   nil,
		},
		"MissingLeaf": {
			reason: "a leaf absent from an otherwise present object returns nil, nil",
			obj: map[string]interface{}{
				jsonKeyStatus: map[string]interface{}{
					jsonKeyAtProvider: map[string]interface{}{"other": "x"},
				},
			},
			field: "name",
			want:  nil,
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
			got, err := navigateAtProvider(tc.obj, tc.field)
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
	results, err := r.RunTests(m)
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
	results, err := r.RunTests(m)
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
	if _, err := r.RunTests(m); err != nil {
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
	results, err := r.RunTests(m)
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
	results, err := r.RunTests(m)
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
// including the nested case where each segment becomes a wrapping object.
func TestBuildMergePatch(t *testing.T) {
	cases := map[string]struct {
		reason string
		field  string
		value  interface{}
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
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := buildMergePatch(tc.field, tc.value)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: buildMergePatch() = %s, want %s", tc.reason, got, tc.want)
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
