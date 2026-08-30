package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/restmapper"
	ktesting "k8s.io/client-go/testing"
)

// TestKubeClientArgvEquivalence captures the exact kubectl argv the
// exec-backed KubeClient produces for every operation the seam covers, and
// asserts it against the literal argv the pre-seam code built inline at
// each of the 15 call sites it replaced (runner.go's ResolveResource,
// ClearConditions, NudgeReconcile, Patch, WaitReady, GetObject,
// restartControllerDeployment, resolveControllerDeploymentName,
// resolveControllerPodIdentityLive; converge.go's countUpdateLogCalls and
// countEventsByReason; resolve.go's pause, stripExternalName and unpause).
// This is the mechanical proof that the seam is behaviour-identical, not an
// assertion in prose.
func TestKubeClientArgvEquivalence(t *testing.T) {
	const (
		ns        = "test-namespace"
		name      = "exampleresource.example.crossplane.io/example-resource"
		manifest  = "/tmp/example-resource.yaml"
		patchJSON = `{"spec":{"forProvider":{"field":"value"}}}`
		condition = "condition=Ready"
		timeout   = "300s"
		selector  = "pkg.crossplane.io/revision"
		podSel    = "pkg.crossplane.io/revision=release-name"
		jsonPath1 = `jsonpath={range .items[*]}{.metadata.labels.pkg\.crossplane\.io/revision}{"\n"}{end}`
		jsonPath2 = `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}`
		target    = "deploy/release-name"
		since     = "30s"
	)

	tests := map[string]struct {
		reason   string
		call     func(c KubeClient) (string, error)
		wantArgs []string
	}{
		"GetObjectJSON no namespace": {
			reason:   "GetObject called `kubectl get <name> -o json` with no -n flag for a cluster-scoped resource",
			call:     func(c KubeClient) (string, error) { return c.GetObjectJSON("", name) },
			wantArgs: []string{"get", name, "-o", "json"},
		},
		"GetObjectJSON namespaced": {
			reason:   "GetObject appended -n <namespace> AFTER the rest of the argv for a namespaced resource",
			call:     func(c KubeClient) (string, error) { return c.GetObjectJSON(ns, name) },
			wantArgs: []string{"get", name, "-o", "json", "-n", ns},
		},
		"ResolveManifestName no namespace": {
			reason:   "ResolveResource called `kubectl get -f <manifest> -o name`, always before r.namespace is set",
			call:     func(c KubeClient) (string, error) { return c.ResolveManifestName("", manifest) },
			wantArgs: []string{"get", "-f", manifest, "-o", "name"},
		},
		"ResolveManifestName namespaced": {
			reason:   "the manifest-name lookup scopes to -n exactly like every other namespace-aware operation",
			call:     func(c KubeClient) (string, error) { return c.ResolveManifestName(ns, manifest) },
			wantArgs: []string{"get", "-f", manifest, "-o", "name", "-n", ns},
		},
		"PatchMerge": {
			reason:   "NudgeReconcile, Patch, pause, stripExternalName and unpause all built this exact shape, differing only in the patch body",
			call:     func(c KubeClient) (string, error) { return c.PatchMerge(ns, name, patchJSON) },
			wantArgs: []string{"patch", name, "--type=merge", "-p", patchJSON, "-n", ns},
		},
		"PatchMergeStatus": {
			reason: "ClearConditions patched the status subresource with --subresource=status ahead of --type=merge",
			call: func(c KubeClient) (string, error) {
				return c.PatchMergeStatus(ns, name, `{"status":{"conditions":[]}}`)
			},
			wantArgs: []string{"patch", name, "--subresource=status", "--type=merge", "-p", `{"status":{"conditions":[]}}`, "-n", ns},
		},
		"WaitForCondition": {
			reason:   "WaitReady always waited on condition=Ready with a --timeout flag",
			call:     func(c KubeClient) (string, error) { return c.WaitForCondition(ns, name, condition, timeout) },
			wantArgs: []string{"wait", name, "--for=condition=Ready", "--timeout=300s", "-n", ns},
		},
		"ListEventsForObject namespaced": {
			reason: "countEventsByReason's exec backend lists events across every namespace, narrowed by a field selector on involvedObject, and never carries a -n flag",
			call:   func(c KubeClient) (string, error) { return c.ListEventsForObject(testKindExample, name, ns) },
			wantArgs: []string{"get", "events", "--all-namespaces", "-o", "json", "--field-selector",
				"involvedObject.kind=" + testKindExample + ",involvedObject.name=" + name + ",involvedObject.namespace=" + ns},
		},
		"ListEventsForObject cluster-scoped": {
			reason: "a cluster-scoped resource's field selector carries an explicit EMPTY involvedObject.namespace term, never an omitted one — omitting it would match ANY namespace, including a namespaced sibling's",
			call:   func(c KubeClient) (string, error) { return c.ListEventsForObject(testKindExample, name, "") },
			wantArgs: []string{"get", "events", "--all-namespaces", "-o", "json", "--field-selector",
				"involvedObject.kind=" + testKindExample + ",involvedObject.name=" + name + ",involvedObject.namespace="},
		},
		"ProviderLogs": {
			reason: "countUpdateLogCalls always sent --tail=-1 ahead of --since, scoped to the PROVIDER namespace by an explicit -n rather than the resource's own namespace",
			call: func(c KubeClient) (string, error) {
				return c.ProviderLogs(providerDeploymentNamespace, selector, since)
			},
			wantArgs: []string{"logs", "-n", providerDeploymentNamespace, "-l", selector, "--tail=-1", "--since=30s"},
		},
		"RolloutRestart": {
			reason:   "restartControllerDeployment issued rollout restart with the target BEFORE -n, never after",
			call:     func(c KubeClient) (string, error) { return c.RolloutRestart(providerDeploymentNamespace, target) },
			wantArgs: []string{"rollout", "restart", target, "-n", providerDeploymentNamespace},
		},
		"RolloutStatus": {
			reason: "restartControllerDeployment's rollout status wait carried --timeout AFTER -n",
			call: func(c KubeClient) (string, error) {
				return c.RolloutStatus(providerDeploymentNamespace, target, timeout)
			},
			wantArgs: []string{"rollout", "status", target, "-n", providerDeploymentNamespace, "--timeout=300s"},
		},
		"GetPodsJSONPath deployment-name lookup shape": {
			reason: "resolveControllerDeploymentName listed pods by the bare revision-label selector",
			call: func(c KubeClient) (string, error) {
				return c.GetPodsJSONPath(providerDeploymentNamespace, selector, jsonPath1)
			},
			wantArgs: []string{"get", "pods", "-n", providerDeploymentNamespace, "-l", selector, "-o", jsonPath1},
		},
		"GetPodsJSONPath pod-identity lookup shape": {
			reason: "resolveControllerPodIdentityLive listed pods by the same selector pinned to one deployment's revision value",
			call: func(c KubeClient) (string, error) {
				return c.GetPodsJSONPath(providerDeploymentNamespace, podSel, jsonPath2)
			},
			wantArgs: []string{"get", "pods", "-n", providerDeploymentNamespace, "-l", podSel, "-o", jsonPath2},
		},
	}

	for tn, tc := range tests {
		t.Run(tn, func(t *testing.T) {
			var gotArgs []string
			r := &Runner{execFunc: func(args []string) (string, error) {
				gotArgs = args
				return "", nil
			}}
			if _, err := tc.call(r.kube()); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Errorf("%s:\n got  %#v\n want %#v", tc.reason, gotArgs, tc.wantArgs)
			}
		})
	}
}

// TestRunnerKubeUsesExecFunc proves the existing execFunc test seam still
// governs KubeClient's exec backend for a test Runner that has not opted
// into the client-go path: a Runner built as a bare struct literal with
// execFunc set and no kubeClientsetFunc (exactly how every pre-existing
// test in this package constructs one) never shells out to a real kubectl
// binary, and never attempts to build a real client-go client either.
func TestRunnerKubeUsesExecFunc(t *testing.T) {
	called := false
	r := &Runner{execFunc: func(args []string) (string, error) {
		called = true
		return "ok", nil
	}}

	out, err := r.kube().ListEventsForObject(testKindExample, testNameExample, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("execFunc was not invoked; the exec backend bypassed the test seam")
	}
	if out != "ok" {
		t.Fatalf("got %q, want %q", out, "ok")
	}
}

// TestRunnerKubeBackendEnvVarForcesExec proves UPDATE_TESTER_KUBE_BACKEND=exec
// overrides EVEN a Runner that HAS a working client-go override — the escape
// hatch must win over the default routing rule, not merely apply when no
// client-go client is available.
func TestRunnerKubeBackendEnvVarForcesExec(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")

	execCalled := false
	clientsetCalled := false
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			execCalled = true
			return `{"items":[]}`, nil
		},
		kubeClientsetFunc: func() (kubernetes.Interface, error) {
			clientsetCalled = true
			return fake.NewSimpleClientset(), nil
		},
	}

	if _, err := r.kube().ListEventsForObject(testKindExample, testNameExample, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execCalled {
		t.Error("execFunc was not invoked; UPDATE_TESTER_KUBE_BACKEND=exec did not force the exec backend")
	}
	if clientsetCalled {
		t.Error("kubeClientsetFunc was invoked; UPDATE_TESTER_KUBE_BACKEND=exec must bypass the client-go backend entirely")
	}
}

// TestRunnerKubeDefaultsToClientGoForEvents proves the DEFAULT routing (no
// env var, no execFunc) serves ListEventsForObject through the client-go
// backend — "this operation defaults to the Go backend once parity is
// proven" is the routing rule under test, not merely something the escape
// hatch can opt into.
func TestRunnerKubeDefaultsToClientGoForEvents(t *testing.T) {
	clientsetCalled := false
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) {
			clientsetCalled = true
			return fake.NewSimpleClientset(), nil
		},
	}

	if _, err := r.kube().ListEventsForObject(testKindExample, testNameExample, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clientsetCalled {
		t.Fatal("kubeClientsetFunc was not invoked; ListEventsForObject did not default to the client-go backend")
	}
}

// TestEventFieldSelectorIsDeterministic proves eventFieldSelector's output
// does not depend on Go's randomised map iteration order: fields.Set.String()
// sorts its terms, but fields.Set.AsSelector().String() does not, and using
// the wrong one would make this seam's argv-equivalence assertions and the
// client-go FieldSelector both flaky across runs.
func TestEventFieldSelectorIsDeterministic(t *testing.T) {
	want := eventFieldSelector(testKindExample, testNameExample, testNamespaceExample)
	for i := 0; i < 20; i++ {
		if got := eventFieldSelector(testKindExample, testNameExample, testNamespaceExample); got != want {
			t.Fatalf("call %d: eventFieldSelector() = %q, want %q (non-deterministic term order)", i, got, want)
		}
	}
}

// newTestClientGoEvent builds a corev1.Event for seeding a fake clientset,
// mirroring newTestEventItemScoped's fields but as the native client-go
// type ListEventsForObject actually queries. name must be unique within
// namespace for the fake object tracker.
func newTestClientGoEvent(name, namespace, reason string, count int32, kind, involvedName, involvedNamespace, apiVersion string) *corev1.Event {
	objNamespace := namespace
	if objNamespace == "" {
		objNamespace = "crossplane-system"
	}
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: objNamespace},
		Reason:     reason,
		Count:      count,
		InvolvedObject: corev1.ObjectReference{
			Kind:       kind,
			Name:       involvedName,
			Namespace:  involvedNamespace,
			APIVersion: apiVersion,
		},
	}
}

// TestClientGoKubeClientListEventsForObjectSendsFieldSelector proves the
// client-go backend actually issues the SAME field selector the exec
// backend builds (see TestKubeClientArgvEquivalence's ListEventsForObject
// cases), by capturing the fake clientset's recorded List action rather
// than trusting the return value alone — the fake ignores FieldSelector
// when deciding what to return, so only inspecting the action proves the
// selector was actually sent.
func TestClientGoKubeClientListEventsForObjectSendsFieldSelector(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := &Runner{kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil }}

	if _, err := r.kube().ListEventsForObject(testKindExample, testNameExample, testNamespaceExample); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventFieldSelector(testKindExample, testNameExample, testNamespaceExample)
	var gotSelector string
	var found bool
	for _, action := range cs.Actions() {
		listAction, ok := action.(ktesting.ListAction)
		if !ok {
			continue
		}
		found = true
		gotSelector = listAction.GetListRestrictions().Fields.String()
	}
	if !found {
		t.Fatal("no List action recorded against the fake clientset")
	}
	if gotSelector != want {
		t.Errorf("FieldSelector = %q, want %q", gotSelector, want)
	}
}

// TestCountEventsByReasonExecAndClientGoAgreeOnAggregatedCount is the AC's
// load-bearing parity proof: for the SAME cluster state — one aggregated
// Event whose .count is 7, i.e. greater than 1 — the exec backend (parsing
// kubectl's JSON) and the client-go backend (parsing a corev1.EventList
// marshalled back to the same JSON shape) must report the identical
// occurrence count. A backend that counted Items instead of summing .count
// would silently report 1 instead of 7 on whichever path had the bug.
func TestCountEventsByReasonExecAndClientGoAgreeOnAggregatedCount(t *testing.T) {
	const wantCount = 7

	// Exec path: canned kubectl JSON, forced onto the exec backend via the
	// escape hatch so this Runner's default routing cannot mask the
	// comparison.
	t.Setenv(kubeBackendEnvVar, "exec")
	execRunner := &Runner{execFunc: func(args []string) (string, error) {
		list := eventList{Items: []eventItem{
			newTestEventItem(eventReasonUpdated, wantCount, testKindExample, testNameExample),
		}}
		b, err := json.Marshal(list)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}}
	gotExec, err := execRunner.countEventsByReason(testKindExample, testNameExample, "", "", eventReasonUpdated)
	if err != nil {
		t.Fatalf("exec path: countEventsByReason() error = %v", err)
	}

	// client-go path: the same logical event, seeded as a native
	// corev1.Event into a fake clientset — no env var, proving the
	// DEFAULT routing serves this.
	t.Setenv(kubeBackendEnvVar, "")
	cs := fake.NewSimpleClientset(
		newTestClientGoEvent("evt-1", "", eventReasonUpdated, wantCount, testKindExample, testNameExample, "", ""),
	)
	goRunner := &Runner{kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil }}
	gotGo, err := goRunner.countEventsByReason(testKindExample, testNameExample, "", "", eventReasonUpdated)
	if err != nil {
		t.Fatalf("client-go path: countEventsByReason() error = %v", err)
	}

	if gotExec != wantCount {
		t.Errorf("exec path: countEventsByReason() = %d, want %d", gotExec, wantCount)
	}
	if gotGo != wantCount {
		t.Errorf("client-go path: countEventsByReason() = %d, want %d", gotGo, wantCount)
	}
	if gotExec != gotGo {
		t.Errorf("exec and client-go paths disagree for identical cluster state: exec=%d, client-go=%d", gotExec, gotGo)
	}
}

// TestCountEventsByReasonClientGoDoesNotCrossMatchNamespacedSibling proves
// the client-go path preserves the invariant that a cluster-scoped resource
// matches only events whose involvedObject carries no namespace, even
// though the fake clientset (like a defensive real backend would not be
// relied upon to do this alone) returns every seeded event regardless of
// the field selector — the client-side re-check in
// sumEventOccurrencesByReason is what actually enforces the invariant, and
// this test proves it still fires on events sourced from client-go.
func TestCountEventsByReasonClientGoDoesNotCrossMatchNamespacedSibling(t *testing.T) {
	const (
		clusterScopedCount = 3
		namespacedCount    = 40
	)
	cs := fake.NewSimpleClientset(
		newTestClientGoEvent("evt-cluster", "", eventReasonUpdated, clusterScopedCount,
			testKindExample, testNameExample, "", testAPIVersionClusterScoped),
		newTestClientGoEvent("evt-namespaced", testNamespaceExample, eventReasonUpdated, namespacedCount,
			testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced),
	)
	r := &Runner{kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil }}

	gotClusterScoped, err := r.countEventsByReason(testKindExample, testNameExample, "", testAPIVersionClusterScoped, eventReasonUpdated)
	if err != nil {
		t.Fatalf("cluster-scoped: countEventsByReason() error = %v", err)
	}
	if gotClusterScoped != clusterScopedCount {
		t.Errorf("cluster-scoped: countEventsByReason() = %d, want %d (must not include the namespaced sibling's %d)",
			gotClusterScoped, clusterScopedCount, namespacedCount)
	}

	gotNamespaced, err := r.countEventsByReason(testKindExample, testNameExample, testNamespaceExample, testAPIVersionNamespaced, eventReasonUpdated)
	if err != nil {
		t.Fatalf("namespaced: countEventsByReason() error = %v", err)
	}
	if gotNamespaced != namespacedCount {
		t.Errorf("namespaced: countEventsByReason() = %d, want %d (must not include the cluster-scoped sibling's %d)",
			gotNamespaced, namespacedCount, clusterScopedCount)
	}
}

// TestKubeBackendSelectionLineDiffersBetweenForcedAndDefault is the parity
// proof's own precondition: a forced-exec Runner's recorded backend line
// must be distinguishable from a default Runner's, or the record cannot
// answer the question it exists for — which backend actually served a
// given run.
func TestKubeBackendSelectionLineDiffersBetweenForcedAndDefault(t *testing.T) {
	forced := kubeBackendSelectionLine(true, false)
	fallback := kubeBackendSelectionLine(false, true)
	deflt := kubeBackendSelectionLine(false, false)

	if forced == deflt {
		t.Fatalf("forced-exec and default lines are identical: %q", forced)
	}
	if forced == fallback {
		t.Fatalf("forced-exec and test-harness-fallback lines are identical: %q", forced)
	}
	if fallback == deflt {
		t.Fatalf("test-harness-fallback and default lines are identical: %q", fallback)
	}
	if !strings.Contains(forced, kubeBackendEnvVar) {
		t.Errorf("forced-exec line = %q, want it to name %s", forced, kubeBackendEnvVar)
	}
}

// TestKubeLogsBackendSelectionOncePerRunner proves kube() records its
// backend choice to stderr exactly once per Runner, no matter how many
// times kube() is subsequently called — a full catalog run calls it on the
// order of a thousand times for event reads alone (see
// kubeBackendLogOnce), and the review that motivated this record needs the
// line to be a single reliable marker, not one buried in a thousand
// duplicates.
func TestKubeLogsBackendSelectionOncePerRunner(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")
	r := &Runner{execFunc: func(args []string) (string, error) { return "", nil }}

	out := captureOSStderr(t, func() {
		for i := 0; i < 5; i++ {
			if _, err := r.kube().ListEventsForObject(testKindExample, testNameExample, ""); err != nil {
				t.Fatalf("call %d: unexpected error: %v", i, err)
			}
		}
	})

	if got := strings.Count(out, "kube backend:"); got != 1 {
		t.Fatalf("kube backend line appeared %d time(s) across 5 kube() calls on one Runner, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "forced by "+kubeBackendEnvVar+"=exec") {
		t.Errorf("logged line = %q, want it to say the choice was forced by %s", out, kubeBackendEnvVar)
	}
}

// TestKubeLogsClientGoBackendByDefault proves the default (unforced,
// non-test-fallback) Runner's recorded line names the client-go backend,
// mirroring TestRunnerKubeDefaultsToClientGoForEvents but asserting on the
// record rather than on which func got called.
func TestKubeLogsClientGoBackendByDefault(t *testing.T) {
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil },
	}

	out := captureOSStderr(t, func() {
		if _, err := r.kube().ListEventsForObject(testKindExample, testNameExample, ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "client-go") {
		t.Errorf("logged line = %q, want it to name the client-go backend", out)
	}
	if strings.Contains(out, "forced by") {
		t.Errorf("logged line = %q, an unforced default Runner must not claim to be forced", out)
	}
}

// testGVRGroup, testGVRVersion and testGVRResource are the group, version
// and properly pluralized resource name GetObjectJSON's client-go backend
// resolves testKindExample's kubectl `-o name` type segment to — distinct
// from the generic "name" fixture the argv-equivalence tests above use,
// which is never resolved against a real RESTMapper.
const (
	testGVRGroup    = "example.crossplane.io"
	testGVRVersion  = "v1alpha1"
	testGVRResource = "exampleresources"
)

// testGetObjectName is the kubectl `-o name` identifier GetObjectJSON's
// tests below resolve through a RESTMapper into testGVR.
var testGetObjectName = testGVRResource + "." + testGVRGroup + "/" + testNameExample

// testGVR is the fully-resolved GroupVersionResource testGetObjectName maps
// to, shared by every test below that needs a working RESTMapper stand-in.
var testGVR = schema.GroupVersionResource{Group: testGVRGroup, Version: testGVRVersion, Resource: testGVRResource}

// newTestUnstructuredExample builds an unstructured object matching
// testKindExample/testGVRGroup/testGVRVersion, for seeding a fake dynamic
// client. namespace == "" builds a cluster-scoped object.
//
// status.atProvider deliberately carries more than one scalar field — a
// nested object, a list and a null-valued key alongside atProviderField —
// so a decoded-map comparison against it actually exercises a non-scalar
// decode. A fixture with only a top-level string cannot distinguish a
// correct decode from one that silently drops a null key or flattens a
// nested value, which is exactly the shape every container-clear update
// test in this package depends on being decoded faithfully.
func newTestUnstructuredExample(namespace, name, atProviderField string) *unstructured.Unstructured {
	metadata := map[string]interface{}{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testGVRGroup + "/" + testGVRVersion,
		"kind":       testKindExample,
		"metadata":   metadata,
		"status": map[string]interface{}{
			"atProvider": map[string]interface{}{
				"field":    atProviderField,
				"tags":     []interface{}{"tag-a", "tag-b"},
				"nested":   map[string]interface{}{"inner": "inner-value"},
				"nullable": nil,
			},
		},
	}}
}

// fakeRESTMapper is a minimal meta.RESTMapper stand-in for tests that only
// exercise ResourceFor — the sole method resourceGVR calls. Every other
// method panics: a test that reaches one has exercised a resolution path
// this migration does not use.
type fakeRESTMapper struct {
	gvr schema.GroupVersionResource
	err error
}

func (m *fakeRESTMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return m.gvr, m.err
}

func (m *fakeRESTMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	panic("KindFor: not used by the GetObjectJSON migration")
}

func (m *fakeRESTMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	panic("KindsFor: not used by the GetObjectJSON migration")
}

func (m *fakeRESTMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	panic("ResourcesFor: not used by the GetObjectJSON migration")
}

func (m *fakeRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	panic("RESTMapping: not used by the GetObjectJSON migration")
}

func (m *fakeRESTMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	panic("RESTMappings: not used by the GetObjectJSON migration")
}

func (m *fakeRESTMapper) ResourceSingularizer(string) (string, error) {
	panic("ResourceSingularizer: not used by the GetObjectJSON migration")
}

// TestGetObjectJSONResolvesGVRAndReadsViaDynamicClient proves the DEFAULT
// routing (no env var, no execFunc) serves GetObjectJSON through the
// client-go backend: the kubectl `-o name` identifier is resolved to a GVR
// via the RESTMapper, and the dynamic Get is issued against the BARE object
// name (never the "type/name" string, which is not a valid API object
// name) scoped to the caller's namespace exactly — empty for cluster-scoped,
// never a guessed default.
func TestGetObjectJSONResolvesGVRAndReadsViaDynamicClient(t *testing.T) {
	cases := map[string]struct {
		reason    string
		namespace string
	}{
		"cluster-scoped": {
			reason:    "a cluster-scoped read passes namespace through as the empty string",
			namespace: "",
		},
		"namespaced": {
			reason:    "a namespaced read scopes the dynamic Get to the caller's namespace",
			namespace: testNamespaceExample,
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			obj := newTestUnstructuredExample(tc.namespace, testNameExample, "resolved-via-client-go")
			dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)

			r := &Runner{
				restMapperFunc:  func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
				kubeDynamicFunc: func() (dynamic.Interface, error) { return dynClient, nil },
			}

			out, err := r.kube().GetObjectJSON(tc.namespace, testGetObjectName)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}

			var got map[string]interface{}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("%s: decoding GetObjectJSON output: %v", tc.reason, err)
			}
			metadata, _ := got["metadata"].(map[string]interface{})
			if metadata["name"] != testNameExample {
				t.Errorf("%s: metadata.name = %v, want %q", tc.reason, metadata["name"], testNameExample)
			}

			var getAction ktesting.GetAction
			for _, action := range dynClient.Actions() {
				if a, ok := action.(ktesting.GetAction); ok {
					getAction = a
				}
			}
			if getAction == nil {
				t.Fatalf("%s: no Get action recorded against the fake dynamic client", tc.reason)
			}
			if getAction.GetName() != testNameExample {
				t.Errorf("%s: dynamic Get name = %q, want the BARE object name %q, not the type/name identifier",
					tc.reason, getAction.GetName(), testNameExample)
			}
			if getAction.GetNamespace() != tc.namespace {
				t.Errorf("%s: dynamic Get namespace = %q, want %q", tc.reason, getAction.GetNamespace(), tc.namespace)
			}
			if getAction.GetResource() != testGVR {
				t.Errorf("%s: dynamic Get GVR = %#v, want %#v", tc.reason, getAction.GetResource(), testGVR)
			}
		})
	}
}

// TestGetObjectJSONExecAndClientGoAgreeOnDecodedMap is the AC's load-bearing
// parity proof: for the SAME logical object, the exec backend (parsing
// canned kubectl JSON) and the client-go backend (parsing a dynamic Get's
// unstructured result, marshalled back to JSON) must decode to the
// IDENTICAL map[string]interface{} — never compared as bytes, since
// Runner.GetObject unmarshals immediately and field ordering carries no
// meaning to any caller.
func TestGetObjectJSONExecAndClientGoAgreeOnDecodedMap(t *testing.T) {
	const execJSON = `{"apiVersion":"example.crossplane.io/v1alpha1","kind":"ExampleResource","metadata":{"name":"example-resource","namespace":"default"},"status":{"atProvider":{"field":"parity-value","tags":["tag-a","tag-b"],"nested":{"inner":"inner-value"},"nullable":null}}}`

	// Exec path: canned kubectl JSON, forced onto the exec backend via the
	// escape hatch so this Runner's default routing cannot mask the
	// comparison.
	t.Setenv(kubeBackendEnvVar, "exec")
	execRunner := &Runner{execFunc: func(args []string) (string, error) { return execJSON, nil }}
	execOut, err := execRunner.kube().GetObjectJSON(testNamespaceExample, testGetObjectName)
	if err != nil {
		t.Fatalf("exec path: unexpected error: %v", err)
	}
	var execDecoded map[string]interface{}
	if err := json.Unmarshal([]byte(execOut), &execDecoded); err != nil {
		t.Fatalf("exec path: decoding: %v", err)
	}

	// client-go path: the same logical object, seeded into a fake dynamic
	// client — no env var, proving the DEFAULT routing serves this.
	t.Setenv(kubeBackendEnvVar, "")
	obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "parity-value")
	goRunner := &Runner{
		restMapperFunc: func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj), nil
		},
	}
	goOut, err := goRunner.kube().GetObjectJSON(testNamespaceExample, testGetObjectName)
	if err != nil {
		t.Fatalf("client-go path: unexpected error: %v", err)
	}
	var goDecoded map[string]interface{}
	if err := json.Unmarshal([]byte(goOut), &goDecoded); err != nil {
		t.Fatalf("client-go path: decoding: %v", err)
	}

	if !reflect.DeepEqual(execDecoded, goDecoded) {
		t.Errorf("exec and client-go paths disagree for the same logical object:\n exec     = %#v\n client-go = %#v", execDecoded, goDecoded)
	}
}

// TestGetObjectJSONSurfacesNotFoundAsError proves a not-found read reaches
// the caller as a non-nil error — exactly how kubectl's non-zero exit on a
// missing resource reaches Runner.GetObject's callers today. No caller
// inspects the error's text or type for this operation (unlike
// ResolveManifestName's disambiguation logic), so "returns a non-nil error"
// is the entire preserved contract.
func TestGetObjectJSONSurfacesNotFoundAsError(t *testing.T) {
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dynClient.PrependReactor("get", testGVRResource, func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: testGVRGroup, Resource: testGVRResource}, testNameExample)
	})

	r := &Runner{
		restMapperFunc:  func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) { return dynClient, nil },
	}

	if _, err := r.kube().GetObjectJSON(testNamespaceExample, testGetObjectName); err == nil {
		t.Fatal("GetObjectJSON() error = nil for a not-found read, want a non-nil error")
	}
}

// TestRunnerKubeBackendEnvVarForcesExecForGetObjectJSONSpecifically proves
// UPDATE_TESTER_KUBE_BACKEND=exec forces GetObjectJSON specifically back
// onto the exec backend, mirroring
// TestRunnerKubeBackendEnvVarForcesExec but for the operation this ticket
// migrates rather than event listing.
func TestRunnerKubeBackendEnvVarForcesExecForGetObjectJSONSpecifically(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")

	execCalled := false
	mapperCalled := false
	dynamicCalled := false
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			execCalled = true
			return `{"status":{}}`, nil
		},
		restMapperFunc: func() (meta.RESTMapper, error) {
			mapperCalled = true
			return &fakeRESTMapper{gvr: testGVR}, nil
		},
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			dynamicCalled = true
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), nil
		},
	}

	if _, err := r.kube().GetObjectJSON(testNamespaceExample, testGetObjectName); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execCalled {
		t.Error("execFunc was not invoked; UPDATE_TESTER_KUBE_BACKEND=exec did not force the exec backend for GetObjectJSON")
	}
	if mapperCalled || dynamicCalled {
		t.Error("restMapperFunc/kubeDynamicFunc were invoked; UPDATE_TESTER_KUBE_BACKEND=exec must bypass the client-go backend entirely")
	}
}

// TestRESTMapperResolvedAtMostOncePerRunner is the AC's memoization proof:
// a Runner backed by a REAL discovery-based RESTMapper (a fake discovery
// client wrapped exactly the way production code wraps one) answers TWO
// GetObjectJSON calls with exactly ONE discovery round trip — proving the
// RESTMapper itself, not merely restMapperFunc's return value, is built at
// most once per Runner. Un-memoized, a second GetObjectJSON call would
// construct a fresh DeferredDiscoveryRESTMapper with a cold cache and
// re-pay the discovery round trip this migration exists to remove.
func TestRESTMapperResolvedAtMostOncePerRunner(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	fakeDisc, ok := fakeClientset.Discovery().(*discoveryfake.FakeDiscovery)
	if !ok {
		t.Fatalf("fake clientset Discovery() is %T, want *discoveryfake.FakeDiscovery", fakeClientset.Discovery())
	}
	fakeDisc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: testGVRGroup + "/" + testGVRVersion,
			APIResources: []metav1.APIResource{
				{Name: testGVRResource, Namespaced: false, Kind: testKindExample, SingularName: "exampleresource"},
			},
		},
	}

	var restMapperBuilds int32
	obj := newTestUnstructuredExample("", testNameExample, "memoized")
	r := &Runner{
		restMapperFunc: func() (meta.RESTMapper, error) {
			atomic.AddInt32(&restMapperBuilds, 1)
			return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDisc)), nil
		},
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj), nil
		},
	}

	if _, err := r.kube().GetObjectJSON("", testGetObjectName); err != nil {
		t.Fatalf("first GetObjectJSON: unexpected error: %v", err)
	}
	if _, err := r.kube().GetObjectJSON("", testGetObjectName); err != nil {
		t.Fatalf("second GetObjectJSON: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&restMapperBuilds); got != 1 {
		t.Errorf("restMapperFunc invoked %d time(s) across 2 GetObjectJSON calls on the same Runner, want exactly 1", got)
	}

	var discoveryRoundTrips int
	for _, action := range fakeClientset.Actions() {
		if action.GetResource().Resource == "group" || action.GetResource().Resource == "resource" {
			discoveryRoundTrips++
		}
	}
	// ServerGroupsAndResources issues exactly 2 recorded actions per round
	// trip (one for "group", one for "resource" — see FakeDiscovery). A
	// second GetObjectJSON call recording MORE than that is a second
	// discovery round trip, the exact defect this test exists to catch.
	if discoveryRoundTrips != 2 {
		t.Errorf("fake discovery client recorded %d discovery action(s) across 2 GetObjectJSON calls, want exactly 2 (one round trip) — a higher count means the RESTMapper was rebuilt per call", discoveryRoundTrips)
	}
}

// TestResourceGVRRejectsNameWithNoTypeSegment proves resourceGVR reports an
// error rather than resolving garbage when handed a bare name with no
// "type/name" structure — a defensive check against a future call site that
// does not go through Runner.resourceName's kubectl `-o name` convention.
func TestResourceGVRRejectsNameWithNoTypeSegment(t *testing.T) {
	c := &clientGoKubeClient{execKubeClient: &execKubeClient{r: &Runner{}}}
	if _, err := c.resourceGVR(testNameExample); err == nil {
		t.Fatal("resourceGVR() error = nil for a name with no type segment, want a non-nil error")
	}
}

// newTestPatchRunner builds a Runner wired for the client-go patch tests
// below: a memoized fakeRESTMapper resolving testGetObjectName to testGVR,
// and dynClient as the dynamic client the client-go backend patches
// against. No execFunc is set, so kube() defaults to the client-go backend
// for every test in this section unless a case explicitly forces exec.
func newTestPatchRunner(dynClient dynamic.Interface) *Runner {
	return &Runner{
		restMapperFunc:  func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) { return dynClient, nil },
	}
}

// lastPatchAction returns the last PatchAction recorded against a fake
// dynamic client's Actions(), failing the test if none was recorded. "Last"
// rather than "only" mirrors how the rest of this file inspects fake
// clientset actions (see TestGetObjectJSONResolvesGVRAndReadsViaDynamicClient).
func lastPatchAction(t *testing.T, actions []ktesting.Action) ktesting.PatchAction {
	t.Helper()
	var found ktesting.PatchAction
	for _, action := range actions {
		if a, ok := action.(ktesting.PatchAction); ok {
			found = a
		}
	}
	if found == nil {
		t.Fatal("no Patch action recorded against the fake dynamic client")
	}
	return found
}

// TestClientGoKubeClientPatchOperationsUseMergePatchType is the AC's
// patch-type proof: both PatchMerge and PatchMergeStatus must issue a JSON
// MERGE patch (RFC 7386) and nothing else. StrategicMergePatchType is not
// supported by custom resources at all; JSONPatchType would interpret the
// patch body as an operation array rather than a merge document. Either
// would silently change what every clear: and per-key-null entry in the
// fleet MEANS while still exiting zero on the easy cases, so this is
// checked directly against the recorded action's patch type rather than
// inferred from a successful call.
func TestClientGoKubeClientPatchOperationsUseMergePatchType(t *testing.T) {
	const patchJSON = `{"spec":{"forProvider":{"field":"value"}}}`

	cases := map[string]struct {
		reason string
		call   func(c KubeClient) (string, error)
	}{
		"PatchMerge": {
			reason: "the main-body patch must be a JSON merge patch",
			call: func(c KubeClient) (string, error) {
				return c.PatchMerge(testNamespaceExample, testGetObjectName, patchJSON)
			},
		},
		"PatchMergeStatus": {
			reason: "the status-subresource patch must ALSO be a JSON merge patch, not just the body patch",
			call: func(c KubeClient) (string, error) {
				return c.PatchMergeStatus(testNamespaceExample, testGetObjectName, `{"status":{"conditions":[]}}`)
			},
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "pre-patch")
			dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
			r := newTestPatchRunner(dynClient)

			if _, err := tc.call(r.kube()); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}

			patchAction := lastPatchAction(t, dynClient.Actions())
			if patchAction.GetPatchType() != types.MergePatchType {
				t.Errorf("%s: patch type = %q, want %q (application/merge-patch+json)",
					tc.reason, patchAction.GetPatchType(), types.MergePatchType)
			}
		})
	}
}

// TestClientGoKubeClientPatchMergeStatusTargetsStatusSubresource is the
// AC's subresource proof: PatchMergeStatus's dynamic Patch call must carry
// "status" as its trailing subresource argument. Omitting it would have
// the API server accept the patch against the MAIN BODY, where
// {"status":{"conditions":[]}} is silently a no-op — ClearConditions (the
// sole caller) would then report success without ever clearing a
// condition, and WaitReady would return immediately instead of blocking on
// the controller re-establishing Ready.
//
// Manually mutation-tested in a scratch copy never pushed: dropping the
// "status" argument from PatchMergeStatus's call to dynamicMergePatch makes
// this test fail with subresource="" instead of "status", confirming the
// assertion is not vacuous.
func TestClientGoKubeClientPatchMergeStatusTargetsStatusSubresource(t *testing.T) {
	obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "pre-patch")
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	r := newTestPatchRunner(dynClient)

	if _, err := r.kube().PatchMergeStatus(testNamespaceExample, testGetObjectName, `{"status":{"conditions":[]}}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patchAction := lastPatchAction(t, dynClient.Actions())
	if got := patchAction.GetSubresource(); got != "status" {
		t.Errorf("PatchMergeStatus subresource = %q, want %q", got, "status")
	}
}

// TestClientGoKubeClientPatchMergeTargetsMainBody proves PatchMerge —
// unlike its PatchMergeStatus sibling, which shares the same
// dynamicMergePatch helper — carries NO subresource, so the helper does not
// leak "status" onto the main-body path.
func TestClientGoKubeClientPatchMergeTargetsMainBody(t *testing.T) {
	obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "pre-patch")
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	r := newTestPatchRunner(dynClient)

	if _, err := r.kube().PatchMerge(testNamespaceExample, testGetObjectName, `{"spec":{"forProvider":{"field":"value"}}}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patchAction := lastPatchAction(t, dynClient.Actions())
	if got := patchAction.GetSubresource(); got != "" {
		t.Errorf("PatchMerge subresource = %q, want empty (main body)", got)
	}
}

// TestClientGoKubeClientPatchBytesReachAPIServerUnmodified is the AC's
// load-bearing byte-fidelity proof: a patch body carrying an explicit
// null, an empty list and an empty map must reach the API server as the
// EXACT bytes the caller built — no unmarshal into a Go map and
// re-marshal, which is precisely where RFC-7386 removal semantics (an
// absent key merges, an explicit null removes) collapse: a Go map cannot
// represent "absent" and "present with an explicit null" as different
// states, so a round trip through one silently drops every clear: this
// tool's container-clear rule depends on.
func TestClientGoKubeClientPatchBytesReachAPIServerUnmodified(t *testing.T) {
	const patchJSON = `{"spec":{"forProvider":{"removedField":null,"emptyList":[],"emptyMap":{},"keptField":"kept"}}}`

	cases := map[string]struct {
		reason string
		call   func(c KubeClient) (string, error)
	}{
		"PatchMerge": {
			reason: "main-body patch bytes must pass through untouched",
			call: func(c KubeClient) (string, error) {
				return c.PatchMerge(testNamespaceExample, testGetObjectName, patchJSON)
			},
		},
		"PatchMergeStatus": {
			reason: "status patch bytes must ALSO pass through untouched",
			call: func(c KubeClient) (string, error) {
				return c.PatchMergeStatus(testNamespaceExample, testGetObjectName, patchJSON)
			},
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "pre-patch")
			dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
			r := newTestPatchRunner(dynClient)

			if _, err := tc.call(r.kube()); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}

			patchAction := lastPatchAction(t, dynClient.Actions())
			if got := string(patchAction.GetPatch()); got != patchJSON {
				t.Errorf("%s:\n got  %s\n want %s (byte-for-byte)", tc.reason, got, patchJSON)
			}
		})
	}
}

// TestRunnerKubeBackendEnvVarForcesExecForPatchOperations proves
// UPDATE_TESTER_KUBE_BACKEND=exec forces BOTH PatchMerge and
// PatchMergeStatus back onto the exec backend, mirroring
// TestRunnerKubeBackendEnvVarForcesExecForGetObjectJSONSpecifically for the
// two operations this ticket migrates.
func TestRunnerKubeBackendEnvVarForcesExecForPatchOperations(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")

	cases := map[string]struct {
		reason string
		call   func(c KubeClient) (string, error)
	}{
		"PatchMerge": {
			reason: "the main-body patch must honour the escape hatch",
			call:   func(c KubeClient) (string, error) { return c.PatchMerge(testNamespaceExample, testGetObjectName, `{}`) },
		},
		"PatchMergeStatus": {
			reason: "the status patch must ALSO honour the escape hatch",
			call: func(c KubeClient) (string, error) {
				return c.PatchMergeStatus(testNamespaceExample, testGetObjectName, `{}`)
			},
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			execCalled := false
			mapperCalled := false
			dynamicCalled := false
			r := &Runner{
				execFunc: func(args []string) (string, error) {
					execCalled = true
					return "{}", nil
				},
				restMapperFunc: func() (meta.RESTMapper, error) {
					mapperCalled = true
					return &fakeRESTMapper{gvr: testGVR}, nil
				},
				kubeDynamicFunc: func() (dynamic.Interface, error) {
					dynamicCalled = true
					return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), nil
				},
			}

			if _, err := tc.call(r.kube()); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.reason, err)
			}
			if !execCalled {
				t.Errorf("%s: execFunc was not invoked; UPDATE_TESTER_KUBE_BACKEND=exec did not force the exec backend", tc.reason)
			}
			if mapperCalled || dynamicCalled {
				t.Errorf("%s: restMapperFunc/kubeDynamicFunc were invoked; UPDATE_TESTER_KUBE_BACKEND=exec must bypass the client-go backend entirely", tc.reason)
			}
		})
	}
}

// newRejectingPatchRunner builds a Runner whose client-go patch path always
// fails with an HTTP 400 — the measured rejected-patch failure mode
// (3800e8a7's union-arm-swap mode) — wired with the resourceName/namespace
// every Runner method under test here reads directly rather than taking as
// an argument.
func newRejectingPatchRunner() *Runner {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dc.PrependReactor("patch", testGVRResource, func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewBadRequest("rejected merge patch")
	})
	return &Runner{
		resourceName:    testGetObjectName,
		namespace:       testNamespaceExample,
		restMapperFunc:  func() (meta.RESTMapper, error) { return &fakeRESTMapper{gvr: testGVR}, nil },
		kubeDynamicFunc: func() (dynamic.Interface, error) { return dc, nil },
	}
}

// TestClientGoBackendPatchErrorsReachEveryPatchCaller is the AC's
// caller-behaviour proof: a rejected client-go patch (an HTTP 400) must
// reach Runner.Patch, Runner.NudgeReconcile, Runner.ClearConditions and
// resolve.go's three PatchMerge call sites (pause, stripExternalName,
// unpause) as a non-nil error — exactly how a rejected kubectl patch's
// non-zero exit reaches every one of these callers today. None of them
// inspects the error's text or type, so "non-nil" is the entire preserved
// contract, verified independently for each of the six callers rather than
// asserted once and assumed to generalise.
func TestClientGoBackendPatchErrorsReachEveryPatchCaller(t *testing.T) {
	cases := map[string]struct {
		reason string
		call   func(r *Runner) error
	}{
		"Runner.Patch": {
			reason: "the primary per-field update patch",
			call:   func(r *Runner) error { return r.Patch("field", "value", nil, nil) },
		},
		"Runner.NudgeReconcile": {
			reason: "the metadata-annotation nudge patch",
			call:   func(r *Runner) error { return r.NudgeReconcile() },
		},
		"Runner.ClearConditions": {
			reason: "the status-subresource condition-clear patch",
			call:   func(r *Runner) error { return r.ClearConditions() },
		},
		"resolve.pause": {
			reason: "the paused-annotation patch",
			call:   func(r *Runner) error { return r.pause() },
		},
		"resolve.stripExternalName": {
			reason: "the external-name-removal patch",
			call:   func(r *Runner) error { return r.stripExternalName() },
		},
		"resolve.unpause": {
			reason: "the paused-annotation-removal patch",
			call:   func(r *Runner) error { return r.unpause() },
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			if err := tc.call(newRejectingPatchRunner()); err == nil {
				t.Fatalf("%s (%s): error = nil for a rejected client-go patch, want non-nil", tn, tc.reason)
			}
		})
	}
}

// --- ResolveManifestName (UT-KUBE-GO-MANIFEST) ---
//
// Live measurement recorded ahead of this implementation (see the ticket's
// own result summary for the full transcript): applying a two-document
// manifest and deleting one object, `kubectl get -f <manifest> -o name`
// printed BOTH lines when both objects existed, and printed NOTHING at all
// — not even the still-existing first document's line — once the second
// document's object was deleted, exiting non-zero with
// `Error from server (NotFound): ...`. The tests below hold the
// implementation to that measured, all-or-nothing shape rather than to a
// decode-only guess.

// testCompanionGVRGroup, testCompanionGVRVersion and testCompanionGVRResource
// describe a core-group companion object (a Secret) — distinct from
// testGVRGroup's custom-resource fixture — so a manifest test can exercise
// the "<resource>/<name>" (no dot-group) identifier shape alongside the
// "<resource>.<group>/<name>" shape in the SAME multi-document manifest.
const (
	testCompanionGVRVersion  = "v1"
	testCompanionGVRResource = "secrets"
	testCompanionKind        = "Secret"
)

var testCompanionGVR = schema.GroupVersionResource{Group: "", Version: testCompanionGVRVersion, Resource: testCompanionGVRResource}

// fakeManifestRESTMapper stands in for meta.RESTMapper for
// ResolveManifestName's tests, which resolve via RESTMapping (a
// GroupKind+version) — the opposite of fakeRESTMapper above, which exists
// for the kubectl `-o name` string every OTHER client-go override resolves
// through. Keyed by GroupKind so one fake instance answers for every Kind a
// multi-document manifest test needs (a managed resource plus its
// companion Secret). Every other meta.RESTMapper method panics: a test
// that reaches one has exercised a resolution path this migration does not
// use.
type fakeManifestRESTMapper struct {
	mappings map[schema.GroupKind]*meta.RESTMapping
}

func (m *fakeManifestRESTMapper) RESTMapping(gk schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	mapping, ok := m.mappings[gk]
	if !ok {
		return nil, fmt.Errorf("fakeManifestRESTMapper: no mapping registered for %s", gk)
	}
	return mapping, nil
}

func (m *fakeManifestRESTMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	panic("ResourceFor: not used by the ResolveManifestName migration")
}

func (m *fakeManifestRESTMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	panic("KindFor: not used by the ResolveManifestName migration")
}

func (m *fakeManifestRESTMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	panic("KindsFor: not used by the ResolveManifestName migration")
}

func (m *fakeManifestRESTMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	panic("ResourcesFor: not used by the ResolveManifestName migration")
}

func (m *fakeManifestRESTMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	panic("RESTMappings: not used by the ResolveManifestName migration")
}

func (m *fakeManifestRESTMapper) ResourceSingularizer(string) (string, error) {
	panic("ResourceSingularizer: not used by the ResolveManifestName migration")
}

// exampleResourceMapping and secretMapping are the two GroupKind->RESTMapping
// entries every ResolveManifestName test below needs: the custom resource
// under test (namespaced) and its core-group companion Secret (also
// namespaced, exactly like a real connection-secret companion object).
var (
	exampleResourceMapping = &meta.RESTMapping{
		Resource:         testGVR,
		GroupVersionKind: schema.GroupVersionKind{Group: testGVRGroup, Version: testGVRVersion, Kind: testKindExample},
		Scope:            meta.RESTScopeNamespace,
	}
	secretMapping = &meta.RESTMapping{
		Resource:         testCompanionGVR,
		GroupVersionKind: schema.GroupVersionKind{Group: "", Version: testCompanionGVRVersion, Kind: testCompanionKind},
		Scope:            meta.RESTScopeNamespace,
	}
)

// writeManifestFile joins docs with a YAML document separator and writes
// them to a temp file, returning its path — ResolveManifestName reads a
// real path via os.ReadFile, exactly like the exec backend reads one via
// kubectl, so a test proving its behaviour needs a real file rather than an
// in-memory reader.
func writeManifestFile(t *testing.T, docs ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	content := strings.Join(docs, "\n---\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing manifest fixture: %v", err)
	}
	return path
}

// manifestDocExampleResource and manifestDocSecret build the YAML text for
// the two document shapes every test below composes into a manifest: the
// custom resource under test, and a core-group companion object sharing
// its own name/namespace parameters.
func manifestDocExampleResource(name, namespace string) string {
	return fmt.Sprintf("apiVersion: %s/%s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n",
		testGVRGroup, testGVRVersion, testKindExample, name, namespace)
}

func manifestDocSecret(name, namespace string) string {
	return fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n",
		testCompanionGVRVersion, testCompanionKind, name, namespace)
}

// newManifestRunner builds a Runner wired for ResolveManifestName's
// client-go path: a fakeManifestRESTMapper carrying both GroupKind
// mappings, and a fake dynamic client seeded with objs.
func newManifestRunner(objs ...runtime.Object) *Runner {
	return &Runner{
		restMapperFunc: func() (meta.RESTMapper, error) {
			return &fakeManifestRESTMapper{mappings: map[schema.GroupKind]*meta.RESTMapping{
				{Group: testGVRGroup, Kind: testKindExample}: exampleResourceMapping,
				{Group: "", Kind: testCompanionKind}:         secretMapping,
			}}, nil
		},
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...), nil
		},
	}
}

// TestResolveManifestNameSingleDocument covers the first of
// selectResourceName's own three cases: a manifest holding exactly one
// document. The full output must be exactly the one resolved identifier
// line, newline-terminated exactly like kubectl's own `-o name` output.
func TestResolveManifestNameSingleDocument(t *testing.T) {
	path := writeManifestFile(t, manifestDocExampleResource(testNameExample, testNamespaceExample))
	obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "irrelevant")
	r := newManifestRunner(obj)

	out, err := r.kube().ResolveManifestName("", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := testGVRResource + "." + testGVRGroup + "/" + testNameExample + "\n"
	if out != want {
		t.Errorf("ResolveManifestName() = %q, want %q", out, want)
	}
}

// TestResolveManifestNameManagedResourcePlusCompanion covers
// selectResourceName's second case: a managed resource plus a companion
// Secret in the SAME manifest, at DISTINCT names. Both lines must appear,
// in document order, and the full multi-line string is asserted — not just
// whichever line a caller might pick out.
func TestResolveManifestNameManagedResourcePlusCompanion(t *testing.T) {
	const secretName = "example-resource-connection"
	path := writeManifestFile(t,
		manifestDocExampleResource(testNameExample, testNamespaceExample),
		manifestDocSecret(secretName, testNamespaceExample),
	)
	resource := newTestUnstructuredExample(testNamespaceExample, testNameExample, "irrelevant")
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       testCompanionKind,
		"metadata":   map[string]interface{}{"name": secretName, "namespace": testNamespaceExample},
	}}
	r := newManifestRunner(resource, secret)

	out, err := r.kube().ResolveManifestName("", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := testGVRResource + "." + testGVRGroup + "/" + testNameExample + "\n" +
		testCompanionGVRResource + "/" + secretName + "\n"
	if out != want {
		t.Errorf("ResolveManifestName() = %q, want %q (document order preserved)", out, want)
	}
}

// TestResolveManifestNameSharedNameDisambiguatedByType covers
// selectResourceName's third case: two documents that share a bare object
// NAME, disambiguated only by type. ResolveManifestName itself does no
// disambiguation — selectResourceName does, downstream — but it must
// independently resolve each document to its OWN correctly-typed
// identifier so that downstream disambiguation has two genuinely different
// lines to choose between rather than an accidental duplicate.
func TestResolveManifestNameSharedNameDisambiguatedByType(t *testing.T) {
	const sharedName = "shared-name"
	path := writeManifestFile(t,
		manifestDocExampleResource(sharedName, testNamespaceExample),
		manifestDocSecret(sharedName, testNamespaceExample),
	)
	resource := newTestUnstructuredExample(testNamespaceExample, sharedName, "irrelevant")
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       testCompanionKind,
		"metadata":   map[string]interface{}{"name": sharedName, "namespace": testNamespaceExample},
	}}
	r := newManifestRunner(resource, secret)

	out, err := r.kube().ResolveManifestName("", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := testGVRResource + "." + testGVRGroup + "/" + sharedName + "\n" +
		testCompanionGVRResource + "/" + sharedName + "\n"
	if out != want {
		t.Errorf("ResolveManifestName() = %q, want %q — the two lines must differ by TYPE even though the name is identical", out, want)
	}
}

// TestResolveManifestNameFailsWholeCallWhenAnyDocumentMissing is the AC's
// load-bearing proof: it reproduces, against a fake dynamic client, the
// EXACT shape measured live — a manifest whose first document still exists
// and whose second does not. The call must fail outright, with NO partial
// output for the still-existing first document, exactly like the measured
// kubectl run. A decode-and-map implementation that never Gets anything
// would instead return two lines here and pass every OTHER test in this
// file; this is the one test that catches it.
func TestResolveManifestNameFailsWholeCallWhenAnyDocumentMissing(t *testing.T) {
	const missingName = "example-resource-b"
	path := writeManifestFile(t,
		manifestDocExampleResource(testNameExample, testNamespaceExample),
		manifestDocExampleResource(missingName, testNamespaceExample),
	)
	present := newTestUnstructuredExample(testNamespaceExample, testNameExample, "irrelevant")
	// missingName is deliberately never seeded into the fake dynamic
	// client, so a Get against it 404s exactly like the live measurement's
	// deleted second object did.
	r := newManifestRunner(present)

	out, err := r.kube().ResolveManifestName("", path)
	if err == nil {
		t.Fatalf("ResolveManifestName() error = nil for a manifest whose second document does not exist, want non-nil")
	}
	if out != "" {
		t.Errorf("ResolveManifestName() output = %q, want empty — the measured live run printed NOTHING when any document failed, not even the still-existing first document's line", out)
	}
}

// TestResolveManifestNameReusesSharedRESTMapper is the AC's memoization
// proof for THIS operation, mirroring TestRESTMapperResolvedAtMostOncePerRunner
// but across two DIFFERENT operations on the same Runner — ResolveManifestName
// then GetObjectJSON — to prove the RESTMapper introduced for GetObjectJSON
// is reused rather than rebuilt, not merely that ResolveManifestName's own
// two documents share one build.
func TestResolveManifestNameReusesSharedRESTMapper(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	fakeDisc, ok := fakeClientset.Discovery().(*discoveryfake.FakeDiscovery)
	if !ok {
		t.Fatalf("fake clientset Discovery() is %T, want *discoveryfake.FakeDiscovery", fakeClientset.Discovery())
	}
	fakeDisc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: testGVRGroup + "/" + testGVRVersion,
			APIResources: []metav1.APIResource{
				{Name: testGVRResource, Namespaced: true, Kind: testKindExample, SingularName: "exampleresource"},
			},
		},
	}

	var restMapperBuilds int32
	path := writeManifestFile(t, manifestDocExampleResource(testNameExample, testNamespaceExample))
	obj := newTestUnstructuredExample(testNamespaceExample, testNameExample, "memoized")
	r := &Runner{
		restMapperFunc: func() (meta.RESTMapper, error) {
			atomic.AddInt32(&restMapperBuilds, 1)
			return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDisc)), nil
		},
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj), nil
		},
	}

	if _, err := r.kube().ResolveManifestName("", path); err != nil {
		t.Fatalf("ResolveManifestName: unexpected error: %v", err)
	}
	if _, err := r.kube().GetObjectJSON(testNamespaceExample, testGetObjectName); err != nil {
		t.Fatalf("GetObjectJSON: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&restMapperBuilds); got != 1 {
		t.Errorf("restMapperFunc invoked %d time(s) across a ResolveManifestName call and a GetObjectJSON call on the same Runner, want exactly 1 — a higher count means ResolveManifestName built its own RESTMapper instead of reusing the shared one", got)
	}
}

// TestRunnerKubeBackendEnvVarForcesExecForResolveManifestNameSpecifically
// mirrors TestRunnerKubeBackendEnvVarForcesExecForGetObjectJSONSpecifically:
// UPDATE_TESTER_KUBE_BACKEND=exec must force ResolveManifestName
// specifically back onto the exec backend, this operation included.
func TestRunnerKubeBackendEnvVarForcesExecForResolveManifestNameSpecifically(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")

	execCalled := false
	mapperCalled := false
	dynamicCalled := false
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			execCalled = true
			return testGVRResource + "." + testGVRGroup + "/" + testNameExample + "\n", nil
		},
		restMapperFunc: func() (meta.RESTMapper, error) {
			mapperCalled = true
			return &fakeManifestRESTMapper{}, nil
		},
		kubeDynamicFunc: func() (dynamic.Interface, error) {
			dynamicCalled = true
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), nil
		},
	}

	if _, err := r.kube().ResolveManifestName("", "/tmp/does-not-matter.yaml"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execCalled {
		t.Error("execFunc was not invoked; UPDATE_TESTER_KUBE_BACKEND=exec did not force the exec backend for ResolveManifestName")
	}
	if mapperCalled || dynamicCalled {
		t.Error("restMapperFunc/kubeDynamicFunc were invoked; UPDATE_TESTER_KUBE_BACKEND=exec must bypass the client-go backend entirely")
	}
}

// TestManifestDocumentNamespace is a pure-function table test over the
// precedence manifestDocumentNamespace implements: an explicit caller
// namespace, then the document's own, then "default" — but only ever for a
// namespaced resource; a cluster-scoped one always resolves to "".
func TestManifestDocumentNamespace(t *testing.T) {
	cases := map[string]struct {
		reason          string
		callerNamespace string
		docNamespace    string
		namespaced      bool
		want            string
	}{
		"cluster-scoped ignores both": {
			reason:          "a cluster-scoped resource never takes a namespace, however either argument is set",
			callerNamespace: "caller-ns",
			docNamespace:    "doc-ns",
			namespaced:      false,
			want:            "",
		},
		"caller namespace wins": {
			reason:          "an explicit caller namespace is kubectl's -n override and takes precedence",
			callerNamespace: "caller-ns",
			docNamespace:    "doc-ns",
			namespaced:      true,
			want:            "caller-ns",
		},
		"falls back to the document's own namespace": {
			reason:          "no caller override: the document's own declared namespace applies",
			callerNamespace: "",
			docNamespace:    "doc-ns",
			namespaced:      true,
			want:            "doc-ns",
		},
		"falls back to default when neither is set": {
			reason:          "neither caller nor document declares a namespace: kubectl's own context-default fallback",
			callerNamespace: "",
			docNamespace:    "",
			namespaced:      true,
			want:            "default",
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			if got := manifestDocumentNamespace(tc.callerNamespace, tc.docNamespace, tc.namespaced); got != tc.want {
				t.Errorf("%s: manifestDocumentNamespace() = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// TestManifestResourceIdentifier is a pure-function table test over the
// output shape ResolveManifestName's callers (resourceTypeOf, objectNameOf,
// selectResourceName) parse: a dotted group segment for a non-core
// resource, none for the core group.
func TestManifestResourceIdentifier(t *testing.T) {
	cases := map[string]struct {
		reason string
		gvr    schema.GroupVersionResource
		name   string
		want   string
	}{
		"non-core resource carries a dotted group": {
			reason: "a custom resource's kubectl `-o name` identifier always names its group",
			gvr:    testGVR,
			name:   testNameExample,
			want:   testGVRResource + "." + testGVRGroup + "/" + testNameExample,
		},
		"core-group resource carries no group segment": {
			reason: "kubectl never appends a bare dot for the core group",
			gvr:    testCompanionGVR,
			name:   "example-secret",
			want:   testCompanionGVRResource + "/example-secret",
		},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			if got := manifestResourceIdentifier(tc.gvr, tc.name); got != tc.want {
				t.Errorf("%s: manifestResourceIdentifier() = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}
