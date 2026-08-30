package runner

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// testPodRevisionLabel is the label key providerDeploymentSelector names —
// a bare key with no value, which Kubernetes label-selector syntax parses
// as "key exists". newTestPod stamps this label on every pod it builds so
// the fake Clientset's client-side selector filtering (gentype's
// alsoFakeLister.List, which — unlike the underlying ObjectTracker it
// wraps — DOES filter by label selector) actually matches it, exactly like
// a real cluster's pods carrying the label Crossplane's package manager
// places on every provider controller Pod.
const testPodRevisionLabel = "pkg.crossplane.io/revision"

// newTestPod builds a minimal Pod carrying containerNames, labelled so it
// matches providerDeploymentSelector, for seeding a fake Clientset's Pod
// list.
func newTestPod(namespace, name string, containerNames ...string) *corev1.Pod {
	containers := make([]corev1.Container, 0, len(containerNames))
	for _, c := range containerNames {
		containers = append(containers, corev1.Container{Name: c})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{testPodRevisionLabel: name},
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

// nopCloser wraps a strings.Reader so newTestStream can return an
// io.ReadCloser without pulling in io.NopCloser's exact wrapper type,
// keeping the injected fakes below trivially inspectable.
type nopCloser struct {
	*strings.Reader
	closed *bool
}

func (n nopCloser) Close() error {
	if n.closed != nil {
		*n.closed = true
	}
	return nil
}

// newTestStream returns an io.ReadCloser over content, recording into
// *closed when Close is called — the mechanism
// TestClientGoKubeClientProviderLogsClosesEveryStream below uses to prove
// every opened stream is closed on the success path.
func newTestStream(content string, closed *bool) io.ReadCloser {
	return nopCloser{Reader: strings.NewReader(content), closed: closed}
}

// erroringReader always fails its Read, simulating a log stream that dies
// mid-transfer after being opened successfully — distinct from a pod whose
// stream never opens at all (TestClientGoKubeClientProviderLogsOnePodFailureDoesNotLoseOthers
// also covers that case via podLogStreamFunc returning a non-nil error
// directly).
type erroringReader struct {
	closed *bool
}

func (erroringReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated mid-stream read failure")
}

func (r erroringReader) Close() error {
	if r.closed != nil {
		*r.closed = true
	}
	return nil
}

// newTestLogsRunner builds a Runner wired for the ProviderLogs tests below:
// kubeClientsetFunc resolves cs (a fake Clientset seeded with whatever pods
// the caller wants List to return), and no execFunc, so kube() defaults to
// the client-go backend.
func newTestLogsRunner(cs kubernetes.Interface) *Runner {
	return &Runner{kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil }}
}

// lastPodsListAction returns the last ListAction recorded against a fake
// Clientset's Actions() targeting the pods resource, failing the test if
// none was recorded — mirroring lastPatchAction's role in kube_test.go.
func lastPodsListAction(t *testing.T, actions []ktesting.Action) ktesting.ListAction {
	t.Helper()
	var found ktesting.ListAction
	for _, action := range actions {
		if action.GetResource().Resource != "pods" {
			continue
		}
		if a, ok := action.(ktesting.ListAction); ok {
			found = a
		}
	}
	if found == nil {
		t.Fatal("no pods List action recorded against the fake clientset")
	}
	return found
}

// lastPodLogOptions returns the *corev1.PodLogOptions carried by the last
// recorded "log" subresource action against a fake Clientset — the fake
// Pods.GetLogs implementation records this via a GenericActionImpl before
// returning its (always-canned) response, which is what makes it possible
// to inspect the options a call built without a live cluster.
func lastPodLogOptions(t *testing.T, actions []ktesting.Action) *corev1.PodLogOptions {
	t.Helper()
	var found *corev1.PodLogOptions
	for _, action := range actions {
		if action.GetSubresource() != "log" {
			continue
		}
		generic, ok := action.(ktesting.GenericAction)
		if !ok {
			continue
		}
		opts, ok := generic.GetValue().(*corev1.PodLogOptions)
		if !ok {
			continue
		}
		found = opts
	}
	if found == nil {
		t.Fatal("no log subresource action recorded against the fake clientset")
	}
	return found
}

// TestClientGoKubeClientProviderLogsListsPodsBySelector proves the pod List
// call carries the exact selector the caller passed, via the recorded
// action's ListRestrictions — the fake ObjectTracker itself ignores the
// selector when deciding what to return (see newTestPod's doc comment), so
// only inspecting the action proves the selector was actually sent.
func TestClientGoKubeClientProviderLogsListsPodsBySelector(t *testing.T) {
	const selector = "pkg.crossplane.io/revision"
	pod := newTestPod(testNamespaceExample, "controller-abc", "controller")
	cs := fake.NewSimpleClientset(pod)
	r := newTestLogsRunner(cs)

	if _, err := r.kube().ProviderLogs(testNamespaceExample, selector, "30s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	listAction := lastPodsListAction(t, cs.Actions())
	if got := listAction.GetListRestrictions().Labels.String(); got != selector {
		t.Errorf("List label selector = %q, want %q", got, selector)
	}
	if got := listAction.GetNamespace(); got != testNamespaceExample {
		t.Errorf("List namespace = %q, want %q", got, testNamespaceExample)
	}
}

// TestClientGoKubeClientProviderLogsNeverSetsTailLines is the AC's
// load-bearing proof: PodLogOptions.TailLines must stay nil on every call,
// which is what "--tail=-1" means. Manually mutation-tested in a scratch
// copy never committed: setting TailLines to a non-nil *int64 (mirroring
// kubectl's own --tail=10 default for a selector) makes this test fail,
// confirming the assertion is not vacuous — countUpdateLogCalls's own doc
// comment records what that exact default did once measured live: loop
// detection collapsed from firing in every window to firing in two windows
// out of three, silently and with a zero exit.
func TestClientGoKubeClientProviderLogsNeverSetsTailLines(t *testing.T) {
	pod := newTestPod(testNamespaceExample, "controller-abc", "controller")
	cs := fake.NewSimpleClientset(pod)
	r := newTestLogsRunner(cs)

	if _, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "45s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := lastPodLogOptions(t, cs.Actions())
	if opts.TailLines != nil {
		t.Errorf("TailLines = %v, want nil (the client-go equivalent of --tail=-1)", *opts.TailLines)
	}
}

// TestClientGoKubeClientProviderLogsSinceSecondsMatchesCallerWindow proves
// PodLogOptions.SinceSeconds carries the exact integer-second count the
// caller's `since` argument named, computed by countUpdateLogCalls and not
// recomputed here — never a value this method derives independently.
func TestClientGoKubeClientProviderLogsSinceSecondsMatchesCallerWindow(t *testing.T) {
	cases := map[string]struct {
		since string
		want  int64
	}{
		"whole seconds":                {since: "45s", want: 45},
		"minutes converted to seconds": {since: "2m", want: 120},
		"single second":                {since: "1s", want: 1},
	}

	for tn, tc := range cases {
		t.Run(tn, func(t *testing.T) {
			pod := newTestPod(testNamespaceExample, "controller-abc", "controller")
			cs := fake.NewSimpleClientset(pod)
			r := newTestLogsRunner(cs)

			if _, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", tc.since); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			opts := lastPodLogOptions(t, cs.Actions())
			if opts.SinceSeconds == nil {
				t.Fatalf("SinceSeconds = nil, want %d", tc.want)
			}
			if *opts.SinceSeconds != tc.want {
				t.Errorf("SinceSeconds = %d, want %d", *opts.SinceSeconds, tc.want)
			}
		})
	}
}

// TestClientGoKubeClientProviderLogsRejectsUnparseableSince proves a
// malformed `since` argument is reported as an error rather than silently
// defaulting to a zero window, which would make loop detection read an
// empty log and report "observed nothing" instead of failing loudly.
func TestClientGoKubeClientProviderLogsRejectsUnparseableSince(t *testing.T) {
	r := newTestLogsRunner(fake.NewSimpleClientset())
	if _, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "not-a-duration"); err == nil {
		t.Fatal("ProviderLogs() error = nil for an unparseable since argument, want non-nil")
	}
}

// TestClientGoKubeClientProviderLogsDefaultsToFirstContainerForMultiContainerPod
// proves container selection mirrors kubectl's own default-container
// behaviour (source-verified against k8s.io/kubectl's
// logsForObjectWithClient, which defaults to Spec.Containers[0] and merely
// warns to stderr for a multi-container pod rather than refusing): the
// FIRST container in the pod spec is read, never every container and never
// an error.
func TestClientGoKubeClientProviderLogsDefaultsToFirstContainerForMultiContainerPod(t *testing.T) {
	pod := newTestPod(testNamespaceExample, "controller-abc", "first-container", "second-container")
	cs := fake.NewSimpleClientset(pod)

	var gotContainer string
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			gotContainer = container
			return newTestStream("log line\n", nil), nil
		},
	}

	if _, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContainer != "first-container" {
		t.Errorf("container = %q, want %q (the pod's first container)", gotContainer, "first-container")
	}
}

// TestClientGoKubeClientProviderLogsConcatenatesMultiplePods proves every
// pod selector matches contributes its content to the result — the SET of
// lines UT-KUBE-SEAM's multi-ReplicaSet measurement requires, not just the
// first pod found.
func TestClientGoKubeClientProviderLogsConcatenatesMultiplePods(t *testing.T) {
	podA := newTestPod(testNamespaceExample, "controller-aaa", "controller")
	podB := newTestPod(testNamespaceExample, "controller-bbb", "controller")
	cs := fake.NewSimpleClientset(podA, podB)

	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			return newTestStream(fmt.Sprintf("line from %s\n", podName), nil), nil
		},
	}

	out, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"line from controller-aaa", "line from controller-bbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q — one pod's lines were lost", out, want)
		}
	}
}

// TestClientGoKubeClientProviderLogsOnePodFailureDoesNotLoseOthers is the
// AC's load-bearing proof for partial failure: when one matched pod's log
// stream cannot be opened, the OTHER pod's lines must still be returned and
// the call must not itself report an error. This is deliberately NOT what
// the exec backend's Runner.exec wrapper produces today (a non-zero
// `kubectl logs -l` exit discards its entire stdout via os/exec.Output,
// losing every pod's content, not just the failing one's) — that is a
// limitation of shelling out through os/exec, not of kubectl's own
// sequential per-pod log consumption, which this backend has no reason to
// inherit now that it reads each pod's stream directly.
func TestClientGoKubeClientProviderLogsOnePodFailureDoesNotLoseOthers(t *testing.T) {
	podOK := newTestPod(testNamespaceExample, "controller-ok", "controller")
	podBad := newTestPod(testNamespaceExample, "controller-bad", "controller")
	cs := fake.NewSimpleClientset(podOK, podBad)

	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			if podName == "controller-bad" {
				return nil, errors.New("simulated: pod evicted mid-request")
			}
			return newTestStream("line from controller-ok\n", nil), nil
		},
	}

	out, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("unexpected error: %v — a single failing pod must not fail the whole call while another pod succeeded", err)
	}
	if !strings.Contains(out, "line from controller-ok") {
		t.Errorf("output %q lost the surviving pod's lines", out)
	}
}

// TestClientGoKubeClientProviderLogsMidStreamReadFailureDoesNotLoseOthers
// covers the OTHER half of "one unreadable pod": a stream that opens
// successfully but fails during Read, rather than failing to open at all.
func TestClientGoKubeClientProviderLogsMidStreamReadFailureDoesNotLoseOthers(t *testing.T) {
	podOK := newTestPod(testNamespaceExample, "controller-ok", "controller")
	podBad := newTestPod(testNamespaceExample, "controller-bad", "controller")
	cs := fake.NewSimpleClientset(podOK, podBad)

	var badClosed bool
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			if podName == "controller-bad" {
				return erroringReader{closed: &badClosed}, nil
			}
			return newTestStream("line from controller-ok\n", nil), nil
		},
	}

	out, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "line from controller-ok") {
		t.Errorf("output %q lost the surviving pod's lines", out)
	}
	if !badClosed {
		t.Error("the mid-stream-failing pod's stream was never closed")
	}
}

// TestClientGoKubeClientProviderLogsAllPodsFailingReturnsError proves the
// one case that DOES surface as an error: every matched pod failed, leaving
// nothing observed — the same "nothing to attribute a call count to"
// outcome the exec backend already produces when its single kubectl
// invocation fails outright.
func TestClientGoKubeClientProviderLogsAllPodsFailingReturnsError(t *testing.T) {
	podA := newTestPod(testNamespaceExample, "controller-a", "controller")
	podB := newTestPod(testNamespaceExample, "controller-b", "controller")
	cs := fake.NewSimpleClientset(podA, podB)

	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			return nil, fmt.Errorf("simulated failure for %s", podName)
		},
	}

	out, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s")
	if err == nil {
		t.Fatal("ProviderLogs() error = nil when every matched pod failed, want non-nil")
	}
	if out != "" {
		t.Errorf("output = %q, want empty when every matched pod failed", out)
	}
}

// TestClientGoKubeClientProviderLogsNoMatchingPodsReturnsEmpty proves an
// empty pod list is not itself an error — matching `kubectl logs -l` on a
// selector with zero matches, which prints "No resources found" to stderr
// and exits 0 with empty stdout rather than failing.
func TestClientGoKubeClientProviderLogsNoMatchingPodsReturnsEmpty(t *testing.T) {
	r := newTestLogsRunner(fake.NewSimpleClientset())

	out, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("unexpected error for a selector matching no pods: %v", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty when no pods matched", out)
	}
}

// TestClientGoKubeClientProviderLogsClosesEveryStream proves every stream
// opened is closed on the success path, across more than one pod — a
// leaked stream per pod, per field test, per resource is exactly the kind
// of connection leak go test -race (run separately over this whole
// package) and this coarse close-tracking check together are meant to
// catch.
func TestClientGoKubeClientProviderLogsClosesEveryStream(t *testing.T) {
	podA := newTestPod(testNamespaceExample, "controller-a", "controller")
	podB := newTestPod(testNamespaceExample, "controller-b", "controller")
	cs := fake.NewSimpleClientset(podA, podB)

	var closedA, closedB bool
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return cs, nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			switch podName {
			case "controller-a":
				return newTestStream("a\n", &closedA), nil
			case "controller-b":
				return newTestStream("b\n", &closedB), nil
			}
			return nil, fmt.Errorf("unexpected pod %s", podName)
		},
	}

	if _, err := r.kube().ProviderLogs(testNamespaceExample, "pkg.crossplane.io/revision", "30s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !closedA {
		t.Error("controller-a's stream was never closed")
	}
	if !closedB {
		t.Error("controller-b's stream was never closed")
	}
}

// TestClientGoKubeClientProviderLogsAndExecAgreeOnAttributedCount is the
// AC's load-bearing parity proof: for the SAME logical log content, the
// exec backend (canned kubectl output) and the client-go backend (canned
// stream content via podLogStreamFunc) must feed countUpdateLogLinesIn to
// the identical (calls, lines) pair. countUpdateLogLinesIn attributes lines
// by content, so this also proves concatenation order is not load-bearing:
// the exec fixture and the client-go fixture are byte-identical here, but
// nothing about this test (or the production code) requires that in
// general — only the resulting SET of matched lines must agree.
func TestClientGoKubeClientProviderLogsAndExecAgreeOnAttributedCount(t *testing.T) {
	const logContent = `{"level":"debug","msg":"Successfully requested update of external resource","request":{"name":"example-resource","namespace":""}}
{"level":"debug","msg":"unrelated line"}
`
	// Exec path: canned kubectl output, forced onto the exec backend via
	// the escape hatch so this Runner's default routing cannot mask the
	// comparison.
	t.Setenv(kubeBackendEnvVar, "exec")
	execRunner := &Runner{execFunc: func(args []string) (string, error) { return logContent, nil }}
	execOut, err := execRunner.kube().ProviderLogs(providerDeploymentNamespace, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("exec path: unexpected error: %v", err)
	}
	execCalls, execLines := countUpdateLogLinesIn(execOut, "example-resource", "", time.Time{})

	// client-go path: the same logical content, served through
	// podLogStreamFunc — no env var, proving the DEFAULT routing serves
	// this.
	t.Setenv(kubeBackendEnvVar, "")
	pod := newTestPod(providerDeploymentNamespace, "controller-abc", "controller")
	goRunner := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) { return fake.NewSimpleClientset(pod), nil },
		podLogStreamFunc: func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
			return newTestStream(logContent, nil), nil
		},
	}
	goOut, err := goRunner.kube().ProviderLogs(providerDeploymentNamespace, "pkg.crossplane.io/revision", "30s")
	if err != nil {
		t.Fatalf("client-go path: unexpected error: %v", err)
	}
	goCalls, goLines := countUpdateLogLinesIn(goOut, "example-resource", "", time.Time{})

	if execCalls != goCalls || execLines != goLines {
		t.Errorf("exec and client-go paths disagree for identical log content: exec=(calls=%d,lines=%d), client-go=(calls=%d,lines=%d)",
			execCalls, execLines, goCalls, goLines)
	}
	if goCalls != 1 || goLines != 2 {
		t.Fatalf("client-go path: countUpdateLogLinesIn() = (calls=%d, lines=%d), want (calls=1, lines=2) — the fixture is not exercising what this test claims to", goCalls, goLines)
	}
}

// TestRunnerKubeBackendEnvVarForcesExecForProviderLogsSpecifically proves
// UPDATE_TESTER_KUBE_BACKEND=exec forces ProviderLogs specifically back
// onto the exec backend, mirroring the same escape-hatch proof already in
// place for every other migrated operation.
func TestRunnerKubeBackendEnvVarForcesExecForProviderLogsSpecifically(t *testing.T) {
	t.Setenv(kubeBackendEnvVar, "exec")

	execCalled := false
	clientsetCalled := false
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			execCalled = true
			return "", nil
		},
		kubeClientsetFunc: func() (kubernetes.Interface, error) {
			clientsetCalled = true
			return fake.NewSimpleClientset(), nil
		},
	}

	if _, err := r.kube().ProviderLogs(providerDeploymentNamespace, "pkg.crossplane.io/revision", "30s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execCalled {
		t.Error("execFunc was not invoked; UPDATE_TESTER_KUBE_BACKEND=exec did not force the exec backend for ProviderLogs")
	}
	if clientsetCalled {
		t.Error("kubeClientsetFunc was invoked; UPDATE_TESTER_KUBE_BACKEND=exec must bypass the client-go backend entirely")
	}
}

// TestRunnerKubeDefaultsToClientGoForProviderLogs proves the DEFAULT
// routing (no env var, no execFunc) serves ProviderLogs through the
// client-go backend, mirroring TestRunnerKubeDefaultsToClientGoForEvents
// but for the operation this ticket migrates.
func TestRunnerKubeDefaultsToClientGoForProviderLogs(t *testing.T) {
	clientsetCalled := false
	r := &Runner{
		kubeClientsetFunc: func() (kubernetes.Interface, error) {
			clientsetCalled = true
			return fake.NewSimpleClientset(), nil
		},
	}

	if _, err := r.kube().ProviderLogs(providerDeploymentNamespace, "pkg.crossplane.io/revision", "30s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clientsetCalled {
		t.Fatal("kubeClientsetFunc was not invoked; ProviderLogs did not default to the client-go backend")
	}
}

// TestFirstContainerNameRejectsZeroContainerPod proves a pod with no
// containers at all (a defensive case kubectl itself never actually hits,
// since the API server rejects a PodSpec with an empty container list) is
// reported as an error rather than resolved to an empty container name,
// which the API would reject anyway with a less useful message.
func TestFirstContainerNameRejectsZeroContainerPod(t *testing.T) {
	pod := *newTestPod(testNamespaceExample, "empty-pod")
	if _, err := firstContainerName(pod); err == nil {
		t.Fatal("firstContainerName() error = nil for a pod with zero containers, want non-nil")
	}
}

// TestParseSinceSecondsRejectsUnparseableInput proves parseSinceSeconds
// reports an error rather than silently truncating to zero.
func TestParseSinceSecondsRejectsUnparseableInput(t *testing.T) {
	if _, err := parseSinceSeconds("not-a-duration"); err == nil {
		t.Fatal("parseSinceSeconds() error = nil for an unparseable duration, want non-nil")
	}
}
