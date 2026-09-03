package runner

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// runtimeScheme returns a bare apimachinery scheme, the same starting point
// TestGetObjectJSONResolvesGVRAndReadsViaDynamicClient and its siblings use
// (dynamicfake.NewSimpleDynamicClient derives GVR<->ListKind registration
// from the seeded objects, not from this scheme). A named helper avoids
// colliding the package name with the stdlib "runtime" package this file
// also needs for the goroutine-leak check below.
func runtimeScheme() *runtime.Scheme { return runtime.NewScheme() }

// newTestUnstructuredExampleWithConditions builds a minimal ExampleResource
// carrying exactly the status.conditions entries passed in, which is all
// waitConditionMet and WaitForCondition read.
func newTestUnstructuredExampleWithConditions(namespace, name string, conditions ...map[string]interface{}) *unstructured.Unstructured {
	metadata := map[string]interface{}{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	conds := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		conds = append(conds, c)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testGVRGroup + "/" + testGVRVersion,
		"kind":       testKindExample,
		"metadata":   metadata,
		"status": map[string]interface{}{
			"conditions": conds,
		},
	}}
}

// waitCondition builds one status.conditions entry in the shape
// waitConditionMet reads: a "type" and a "status", nothing else.
func waitCondition(conditionType, status string) map[string]interface{} {
	return map[string]interface{}{"type": conditionType, "status": status}
}

// newWaitTestRunner builds a Runner wired for the WaitForCondition tests
// below: a memoized fakeRESTMapper resolving testGetObjectName to testGVR,
// and dynClient as the dynamic client the client-go backend reads and
// watches against — mirroring newTestPatchRunner's role for the patch
// tests.
func newWaitTestRunner(dynClient dynamic.Interface) *Runner {
	return &Runner{
		restMapperFunc:  func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) { return dynClient, nil },
	}
}

// TestParseWaitCondition proves the condition string is genuinely PARSED —
// not hard-coded to "Ready" — for every syntax kubectl wait's own
// "--for=condition=<type>[=<status>]" flag accepts, plus the two malformed
// shapes rejected outright.
func TestParseWaitCondition(t *testing.T) {
	cases := map[string]struct {
		reason     string
		condition  string
		wantType   string
		wantStatus string
		wantErr    bool
	}{
		"bare condition defaults status to True": {
			reason:     "every caller today passes condition=Ready with no explicit status, and True is kubectl's own default",
			condition:  "condition=Ready",
			wantType:   "Ready",
			wantStatus: "True",
		},
		"explicit status is honoured": {
			reason:     "kubectl wait accepts an explicit condition-value after a second =",
			condition:  "condition=Ready=False",
			wantType:   "Ready",
			wantStatus: "False",
		},
		"a non-Ready condition type is parsed, not hard-coded": {
			reason:     "WaitForCondition takes the condition as a parameter; an implementation ignoring it would still pass every existing Ready-only caller",
			condition:  "condition=Synced",
			wantType:   "Synced",
			wantStatus: "True",
		},
		"missing the condition= prefix is an error": {
			reason:    "only the condition=... form is supported by this migration; delete/create/jsonpath are not routed through WaitForCondition",
			condition: "delete",
			wantErr:   true,
		},
		"empty condition type is an error": {
			reason:    "a bare 'condition=' names no type to wait on",
			condition: "condition=",
			wantErr:   true,
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			gotType, gotStatus, err := parseWaitCondition(tc.condition)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: parseWaitCondition(%q) error = nil, want non-nil", tc.reason, tc.condition)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if gotType != tc.wantType || gotStatus != tc.wantStatus {
				t.Errorf("%s: parseWaitCondition(%q) = (%q, %q), want (%q, %q)",
					tc.reason, tc.condition, gotType, gotStatus, tc.wantType, tc.wantStatus)
			}
		})
	}
}

// TestWaitConditionMet covers waitConditionMet's own matching logic in
// isolation from the watch machinery around it: exact match, kubectl's
// documented case-insensitive comparison, a status mismatch, a missing
// condition type, and an object with no status.conditions key at all
// (freshly created, before any status write).
func TestWaitConditionMet(t *testing.T) {
	cases := map[string]struct {
		reason          string
		obj             *unstructured.Unstructured
		conditionType   string
		conditionStatus string
		want            bool
	}{
		"matching type and status": {
			reason:          "the condition this migration exists to detect",
			obj:             newTestUnstructuredExampleWithConditions("", testNameExample, waitCondition("Ready", "True")),
			conditionType:   "Ready",
			conditionStatus: "True",
			want:            true,
		},
		"case-insensitive match": {
			reason:          "kubectl wait compares condition type and status after Unicode simple case folding",
			obj:             newTestUnstructuredExampleWithConditions("", testNameExample, waitCondition("ready", "true")),
			conditionType:   "Ready",
			conditionStatus: "True",
			want:            true,
		},
		"status mismatch": {
			reason:          "a False Ready condition has not met a wait for True",
			obj:             newTestUnstructuredExampleWithConditions("", testNameExample, waitCondition("Ready", "False")),
			conditionType:   "Ready",
			conditionStatus: "True",
			want:            false,
		},
		"condition type not present": {
			reason:          "the condition this wait names is simply absent from status.conditions",
			obj:             newTestUnstructuredExampleWithConditions("", testNameExample, waitCondition("Synced", "True")),
			conditionType:   "Ready",
			conditionStatus: "True",
			want:            false,
		},
		"no status.conditions key at all": {
			reason:          "a freshly created object with no status written yet must not be treated as met",
			obj:             newTestUnstructuredExample("", testNameExample, "x"),
			conditionType:   "Ready",
			conditionStatus: "True",
			want:            false,
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			got, err := waitConditionMet(tc.obj, tc.conditionType, tc.conditionStatus)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: waitConditionMet() = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestClientGoWaitForConditionAlreadySatisfiedReturnsPromptly is the AC's
// load-bearing criterion: a fake whose object is ALREADY Ready, with no
// subsequent event ever delivered, must return promptly rather than block
// until timeout. A bare Watch() with no preceding List passes every test
// where the transition happens after the call opens and fails exactly this
// one — the failure mode that turns every fast-reconciling resource's
// update test into a full-timeout wait. The elapsed-time assertion is what
// makes this mechanical rather than a trust-the-implementation claim: it
// was mutation-tested in a scratch copy (never committed) by swapping the
// UntilWithSync-based implementation for a bare dyn.Watch()-only one, which
// made this exact test time out at the full 10s.
func TestClientGoWaitForConditionAlreadySatisfiedReturnsPromptly(t *testing.T) {
	obj := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "True"))
	dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), obj)
	r := newWaitTestRunner(dynClient)

	const timeout = "10s"
	start := time.Now()
	out, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", timeout)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("WaitForCondition took %s for an object that was ALREADY Ready against a %s timeout that no watch event ever needed to consume — "+
			"a bare Watch() with no preceding List would block for the full timeout here, which is the exact regression this test exists to catch",
			elapsed, timeout)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	metadata, _ := decoded["metadata"].(map[string]interface{})
	if metadata["name"] != testNameExample {
		t.Errorf("metadata.name = %v, want %q", metadata["name"], testNameExample)
	}
}

// TestClientGoWaitForConditionTransitionSatisfiesOnWatchEvent proves the
// ordinary case still works: an object that starts NOT Ready and later
// transitions is caught by the watch this migration opens, not just the
// already-satisfied fast path above.
func TestClientGoWaitForConditionTransitionSatisfiesOnWatchEvent(t *testing.T) {
	notReady := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "False"))
	dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), notReady)
	r := newWaitTestRunner(dynClient)

	ready := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "True"))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		res := dynClient.Resource(testGVR).Namespace(testNamespaceExample)
		// Retries rather than a single timed Update: the watch this test
		// exercises is only registered with the fake tracker once the
		// informer inside WaitForCondition actually opens it, and the fake
		// watcher never replays history — an Update issued before that
		// registration is silently missed. Repeating (idempotently) until
		// the wait itself returns guarantees at least one Update lands
		// after registration, without requiring this test to know exactly
		// when that happened.
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := res.Update(context.Background(), ready.DeepCopy(), metav1.UpdateOptions{}); err != nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	out, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", "5s")
	if err != nil {
		t.Fatalf("unexpected error waiting for a transition that DID happen: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	status, _ := decoded["status"].(map[string]interface{})
	if status == nil {
		t.Fatalf("result carries no status: %v", decoded)
	}
}

// TestClientGoWaitForConditionTimesOutWithNonNilError proves the timeout
// path returns a non-nil error rather than swallowing it: a Go wait that
// returns nil on timeout would convert a hung reconcile into a passing
// field test, reporting coverage the tool never actually observed.
func TestClientGoWaitForConditionTimesOutWithNonNilError(t *testing.T) {
	notReady := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "False"))
	dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), notReady)
	r := newWaitTestRunner(dynClient)

	const timeout = "300ms"
	start := time.Now()
	_, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForCondition() error = nil for a condition that never became True — a swallowed timeout turns a hung reconcile into a passing field test")
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("WaitForCondition returned after %s for a %s timeout on a condition that never transitions — it returned too early", elapsed, timeout)
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitForCondition took %s to report a %s timeout — it must not block substantially longer than the timeout it was given", elapsed, timeout)
	}
	if !wait.Interrupted(errUnwrapToInterrupted(err)) {
		// Not fatal: WaitForCondition wraps the underlying error with
		// fmt.Errorf("%w", ...), so errors.Is still finds it. This is a
		// belt-and-braces check that the returned error really does carry
		// the timeout/interrupted signal, not merely SOME non-nil error.
		t.Errorf("returned error %v does not unwrap to a wait.Interrupted cause", err)
	}
}

// errUnwrapToInterrupted is a one-line indirection so the timeout test
// above can call wait.Interrupted directly on WaitForCondition's returned
// error without re-importing errors.Is logic here — wait.Interrupted
// already does the unwrapping itself.
func errUnwrapToInterrupted(err error) error { return err }

// TestClientGoWaitForConditionAbsentObjectErrorsImmediately proves an
// object that was never created is reported as a non-nil error promptly —
// matching kubectl wait's own fail-fast behaviour for a missing resource —
// rather than blocking until the full timeout elapses.
func TestClientGoWaitForConditionAbsentObjectErrorsImmediately(t *testing.T) {
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtimeScheme(),
		map[schema.GroupVersionResource]string{testGVR: testKindExample + "List"})
	r := newWaitTestRunner(dynClient)

	const timeout = "10s"
	start := time.Now()
	_, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForCondition() error = nil for an object that was never created, want a non-nil error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("WaitForCondition took %s to report an absent object against a %s timeout — "+
			"kubectl wait fails fast on a missing resource rather than blocking until timeout, and this took most of it", elapsed, timeout)
	}
}

// TestClientGoWaitForConditionTracksNamedConditionNotJustReady proves the
// condition parameter is actually tracked end to end, not merely parsed in
// isolation (TestParseWaitCondition) while the watch loop still checks
// "Ready" regardless: an object carrying only a Synced condition (no Ready
// condition at all) satisfies a wait for "condition=Synced" but would never
// satisfy one hard-coded to Ready.
func TestClientGoWaitForConditionTracksNamedConditionNotJustReady(t *testing.T) {
	obj := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Synced", "True"))
	dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), obj)
	r := newWaitTestRunner(dynClient)

	if _, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Synced", "5s"); err != nil {
		t.Fatalf("waiting for the Synced condition that IS present: unexpected error: %v", err)
	}
}

// TestRunnerKubeDefaultsToClientGoForWaitForCondition proves
// WaitForCondition is served through the client-go backend.
func TestRunnerKubeDefaultsToClientGoForWaitForCondition(t *testing.T) {
	obj := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "True"))
	dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), obj)

	dynamicCalled := false
	r := &Runner{
		restMapperFunc: func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			dynamicCalled = true
			return dynClient, nil
		},
	}

	if _, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", "5s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dynamicCalled {
		t.Fatal("kubeDynamicFunc was not invoked; WaitForCondition did not default to the client-go backend")
	}
}

// TestWaitForConditionLeavesNoWatchOpenAfterReturning is a coarse
// goroutine-leak signal covering both the already-satisfied fast path and
// the timeout path: a leaked watch per call (no leaked watch is ever
// closed) would show as unbounded goroutine growth across 25 calls. The
// tolerance is generous because the Go runtime's own background goroutines
// are not under this test's control — this is a leak DETECTOR, not an
// exact accounting, and go test -race (run separately over this whole
// package) is the stronger guarantee against a genuine data race on the
// watch's own teardown.
func TestWaitForConditionLeavesNoWatchOpenAfterReturning(t *testing.T) {
	readyObj := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "True"))
	notReadyObj := newTestUnstructuredExampleWithConditions(testNamespaceExample, testNameExample, waitCondition("Ready", "False"))

	goruntime.GC()
	base := goruntime.NumGoroutine()

	for i := 0; i < 20; i++ {
		dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), readyObj)
		r := newWaitTestRunner(dynClient)
		if _, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", "5s"); err != nil {
			t.Fatalf("already-satisfied call %d: unexpected error: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		dynClient := dynamicfake.NewSimpleDynamicClient(runtimeScheme(), notReadyObj)
		r := newWaitTestRunner(dynClient)
		if _, err := r.kube().WaitForCondition(testNamespaceExample, testGetObjectName, "condition=Ready", "100ms"); err == nil {
			t.Fatalf("timeout call %d: error = nil, want non-nil", i)
		}
	}

	const tolerance = 5
	var after int
	for attempt := 0; attempt < 20; attempt++ {
		goruntime.GC()
		time.Sleep(20 * time.Millisecond)
		after = goruntime.NumGoroutine()
		if after <= base+tolerance {
			break
		}
	}
	if after > base+tolerance {
		t.Errorf("goroutine count grew from %d to %d after 25 WaitForCondition calls (satisfied and timed-out) — a leaked watch per call would show here", base, after)
	}
}
