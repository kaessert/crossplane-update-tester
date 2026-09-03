// Package runner drives the update tester's live-cluster checks through
// kubectl: per-field update tests (RunTests), the post-create convergence
// check (RunConverge), and the ref-less identity-resolve recovery check
// (RunResolveRecover).
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kaessert/crossplane-update-tester/internal/differ"
	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// Runner executes update tests against a live cluster using kubectl.
type Runner struct {
	kubectl      string
	manifestPath string
	// resourceName is the cached `kubectl get -o name` identifier of the
	// object under test, e.g.
	// "exampleresource.example.crossplane.io/example-resource".
	resourceName string
	namespace    string
	timeout      string
	// pollInterval is the provider's poll interval, as declared by the
	// caller. It is not a cadence this Runner itself polls on — it is the
	// scale against which an observed convergence duration is judged (see
	// slowObserveThreshold). Zero means "not declared", which reads as
	// defaultPollInterval.
	pollInterval time.Duration

	// execFunc, when set, overrides the kubectl invocation used by exec().
	// Tests inject a fake here to simulate kubectl output without a live
	// cluster; production code leaves it nil and exec() shells out for real.
	execFunc func(args []string) (string, error)

	// restartFunc, when set, overrides restartControllerDeployment as the
	// mechanism resetEventBurst uses to earn back a fresh event-spam-filter
	// burst (see eventBurstCeiling). Tests inject a fake here; production
	// code leaves it nil and resetEventBurst restarts the controller
	// Deployment for real through the client-go backend.
	restartFunc func() error

	// podIdentityFunc, when set, overrides resolveControllerPodIdentity as
	// the mechanism convergeArm/convergeAssert use to read the running
	// provider controller Pod's identity (see controllerPodIdentity). Tests
	// inject a fake sequence here; production code leaves it nil and
	// resolveControllerPodIdentity shells out to kubectl for real.
	podIdentityFunc func() (controllerPodIdentity, error)

	// sleepFunc, when set, overrides the wait between iterations of a
	// bounded poll loop (waitReady, waitGenerationSettled, evidenceOutcome's
	// retry). Tests inject a no-op or near-instant stand-in so every poll
	// iteration's logic — not just its first pass — runs without spending
	// real wall-clock time; production code leaves it nil and sleep() calls
	// time.Sleep for real.
	sleepFunc func(time.Duration)

	// evidenceWindow, when set (> 0), overrides evidenceRetryWindow — the
	// bounded window evidenceOutcome retries the post-patch event recount
	// before concluding an update was never evidenced. Tests set a small
	// value here so both the eventually-evidenced and the
	// genuinely-never-evidenced cases resolve near-instantly instead of
	// each spending evidenceRetryWindow's real 10 seconds; production code
	// leaves it zero and evidenceOutcome falls back to evidenceRetryWindow.
	evidenceWindow time.Duration

	// podSettleThreshold and podSettleTimeout, when set (> 0), override
	// controllerPodSettleThreshold and controllerPodSettleTimeout
	// respectively — the age a provider controller Pod must reach before
	// convergeArm takes its baseline, and how long it waits for that.
	// Tests set small values here so both the already-settled and the
	// never-settles-within-timeout cases resolve near-instantly instead of
	// spending the real 120s window; production code leaves both zero and
	// convergeArm falls back to the package constants.
	podSettleThreshold time.Duration
	podSettleTimeout   time.Duration

	// burstCeiling, when > 0, overrides defaultEventBurstCeiling as the
	// number of Update()-triggering field tests attempted before
	// proactively restarting the controller to earn back a fresh
	// event-spam-filter burst (see eventBurstCeiling and
	// defaultEventBurstCeiling). Populated by NewRunner from
	// UPDATE_TESTER_EVENT_BURST_CEILING; zero or negative means "use the
	// default", read through the eventBurstCeiling accessor rather than
	// this field directly.
	burstCeiling int

	// kubeClientsetOnce, kubeClientset and kubeClientsetErr memoize the
	// client-go Clientset the client-go KubeClient backend resolves from
	// the ambient kubeconfig (see goClientset) — built at most once per
	// Runner, since ListEventsForObject is called once per event read
	// rather than once per Runner.
	kubeClientsetOnce sync.Once
	kubeClientset     kubernetes.Interface
	kubeClientsetErr  error

	// kubeClientsetFunc, when set, overrides goClientset as the mechanism
	// the client-go KubeClient backend uses to obtain a Clientset. Tests
	// inject a fake clientset constructor here; production code leaves it
	// nil and goClientset builds one from the ambient kubeconfig.
	kubeClientsetFunc func() (kubernetes.Interface, error)

	// kubeDynamicOnce, kubeDynamic and kubeDynamicErr memoize the dynamic
	// client the client-go KubeClient backend uses to read a resource with
	// no compiled-in Go type — a provider CR, exactly like the managed
	// resource GetObjectJSON's own call site reads (see goDynamicClient).
	kubeDynamicOnce sync.Once
	kubeDynamic     dynamic.Interface
	kubeDynamicErr  error

	// kubeDynamicFunc, when set, overrides goDynamicClient the same way
	// kubeClientsetFunc overrides goClientset. Tests inject a fake dynamic
	// client constructor here; production code leaves it nil and
	// goDynamicClient builds one from the ambient kubeconfig.
	kubeDynamicFunc func() (dynamic.Interface, error)

	// restMapperOnce, kubeRESTMapper and kubeRESTMapperErr memoize the
	// discovery-backed RESTMapper the client-go KubeClient backend uses to
	// resolve a kubectl `-o name` resource/group pair into a full
	// GroupVersionResource — built at most once per Runner, since the
	// discovery round trip behind it is the ~16ms-per-call cost
	// GetObjectJSON's migration exists to remove (see restMapper).
	restMapperOnce    sync.Once
	kubeRESTMapper    meta.RESTMapper
	kubeRESTMapperErr error

	// restMapperFunc, when set, overrides the discovery-based construction
	// restMapper() otherwise performs. Unlike kubeClientsetFunc/
	// kubeDynamicFunc, this override is invoked FROM INSIDE restMapperOnce
	// rather than as a bypass — see restMapper's doc comment for why.
	restMapperFunc func() (meta.RESTMapper, error)

	// kubeBackendLogOnce guards the one-line record of which KubeClient
	// backend this Runner resolved to (see kube()) so a full catalog run
	// — which calls kube() on the order of a thousand times for event
	// reads alone — emits that record exactly once, not once per call.
	kubeBackendLogOnce sync.Once

	// podLogStreamFunc, when set, overrides podLogStream as the mechanism
	// the client-go KubeClient backend uses to open one pod/container's
	// log stream for ProviderLogs. Tests inject a fake here: the built-in
	// fake Clientset's GetLogs always returns identical canned content
	// with a 200 status for every pod, so it cannot express either
	// distinct per-pod content or a mid-stream failure. Production code
	// leaves it nil and podLogStream opens a real client-go log stream.
	podLogStreamFunc func(namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error)

	// rolloutStatusPollInterval, when > 0, overrides
	// defaultRolloutStatusPollInterval — the cadence RolloutStatus's
	// client-go backend re-Gets the target Deployment while waiting for
	// its rollout to finish. Tests set a near-instant value here so a
	// not-ready-then-ready transition resolves without spending the real
	// default interval; production code leaves it zero and RolloutStatus
	// falls back to defaultRolloutStatusPollInterval.
	rolloutStatusPollInterval time.Duration
}

// sleep waits d, calling sleepFunc instead of time.Sleep when a test has
// overridden it. Every bounded poll loop in this package waits through this
// method rather than calling time.Sleep directly, so one override makes all
// of them fast under test.
func (r *Runner) sleep(d time.Duration) {
	if r.sleepFunc != nil {
		r.sleepFunc(d)
		return
	}
	time.Sleep(d)
}

// NewRunner creates a Runner for the given manifest file.
func NewRunner(manifestPath string, timeout int) *Runner {
	kubectl := os.Getenv("KUBECTL")
	if kubectl == "" {
		kubectl = "kubectl"
	}
	// A parse failure here is deliberately NOT an error: this knob is read
	// deep inside a hook subprocess tree, and failing an E2E run over a
	// typo in an env var costs more than silently running at the
	// calibrated default. burstCeiling stays 0 ("use the default") for an
	// unset, empty, unparseable, zero or negative value; the
	// eventBurstCeiling accessor is what actually applies the fallback.
	var burstCeiling int
	if v := os.Getenv(eventBurstCeilingEnvVar); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			burstCeiling = parsed
		}
	}
	return &Runner{
		kubectl:      kubectl,
		manifestPath: manifestPath,
		timeout:      fmt.Sprintf("%ds", timeout),
		burstCeiling: burstCeiling,
	}
}

// WithPollInterval declares the provider's poll interval and returns the
// Runner, so it can be chained onto NewRunner by the one caller that has a
// poll interval to declare.
//
// It is a setter rather than a NewRunner parameter because it changes
// nothing about what the Runner waits for: it only calibrates the
// slow-observe threshold (see slowObserveThreshold). Every other command
// constructs a Runner that has no opinion on the provider's poll cadence,
// and would have to pass a zero through a wider constructor to say so.
// A zero or negative value leaves defaultPollInterval in force.
func (r *Runner) WithPollInterval(d time.Duration) *Runner {
	r.pollInterval = d
	return r
}

// ResolveResource uses kubectl to resolve the full resource name from the
// manifest file.
//
// `kubectl get -f <manifest> -o name` prints one line PER DOCUMENT in the
// manifest, and a Crossplane example manifest routinely ships a companion
// object (a Secret, a ProviderConfig) alongside the managed resource under
// test. Treating the whole trimmed output as the resource name therefore
// yields a multi-line string as soon as the manifest holds more than one
// document, and every subsequent kubectl call is issued against that
// garbage. The manifest parser selects the annotated document, so this must
// match it: the line whose object name — and, when several documents share
// a name, whose type — corresponds to the parsed Kind/Name is selected, and
// an output that cannot be resolved unambiguously is an error rather than a
// guess.
func (r *Runner) ResolveResource(m *manifest.Manifest) error {
	out, err := r.kube().ResolveManifestName(r.namespace, r.manifestPath)
	if err != nil {
		return fmt.Errorf("resolving resource from manifest: %w", err)
	}

	name, err := selectResourceName(out, m.Kind, m.Name)
	if err != nil {
		return fmt.Errorf("resolving resource from manifest %s: %w", r.manifestPath, err)
	}

	r.resourceName = name
	r.namespace = m.Namespace
	return nil
}

// selectResourceName picks the `kubectl get -o name` line that identifies
// the manifest document under test.
//
// Lines are matched on the bare object name first, because that is what the
// manifest parser keys on. A name collision between documents is normal
// (a managed resource and its companion Secret are routinely named alike),
// so the kind breaks the tie. An output that still cannot be narrowed to
// exactly one line is reported as an error: any pick made here is silently
// carried by every later kubectl call in the run, so a wrong pick would
// report results for an object nobody asked about.
func selectResourceName(out, kind, name string) (string, error) {
	var byName []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if objectNameOf(line) == name {
			byName = append(byName, line)
		}
	}

	switch len(byName) {
	case 0:
		return "", fmt.Errorf("kubectl returned no object named %q (kind %s); output was %q",
			name, kind, strings.TrimSpace(out))
	case 1:
		return byName[0], nil
	}

	var byKind []string
	for _, candidate := range byName {
		if resourceTypeMatchesKind(resourceTypeOf(candidate), kind) {
			byKind = append(byKind, candidate)
		}
	}
	if len(byKind) == 1 {
		return byKind[0], nil
	}
	return "", fmt.Errorf("kubectl returned %d objects named %q (%s) and none of them uniquely matches kind %q",
		len(byName), name, strings.Join(byName, ", "), kind)
}

// objectNameOf extracts the bare object name from a "type/name" resource
// identifier as returned by `kubectl get -o name`.
func objectNameOf(resourceName string) string {
	if idx := strings.LastIndex(resourceName, "/"); idx != -1 {
		return resourceName[idx+1:]
	}
	return resourceName
}

// resourceTypeOf extracts the type portion of a "type/name" resource
// identifier, e.g. "exampleresource.example.crossplane.io" or "secret".
func resourceTypeOf(resourceName string) string {
	if idx := strings.LastIndex(resourceName, "/"); idx != -1 {
		return resourceName[:idx]
	}
	return ""
}

// resourceTypeMatchesKind reports whether a `kubectl get -o name` type
// segment denotes the given Kubernetes Kind. kubectl renders the type as
// "<resource>.<group>" (or a bare "<resource>" for core types), where
// <resource> is the lowercased singular or plural of the Kind depending on
// the type and the kubectl version — so the leading token is compared
// case-insensitively against the Kind and its common plural forms rather
// than for exact equality.
func resourceTypeMatchesKind(resourceType, kind string) bool {
	if kind == "" || resourceType == "" {
		return false
	}
	head := resourceType
	if idx := strings.Index(head, "."); idx != -1 {
		head = head[:idx]
	}
	head = strings.ToLower(head)
	k := strings.ToLower(kind)
	return head == k || head == k+"s" || head == k+"es"
}

// Snapshot reads the current status.atProvider as JSON bytes, for the
// differ to compare against a later one.
//
// It fetches the whole object with `kubectl get -o json` and marshals the
// subtree here, for the same reason ReadField parses the object itself: no
// read in this package depends on how a given kubectl renders a non-scalar
// value. The stakes are highest for this one, because these bytes go
// straight to a JSON parser — a Go-syntax rendering would fail every
// convergence check while PARSING the snapshot rather than reporting a
// verdict, so the check would lose its meaning rather than its formatting.
//
// Marshalling a decoded map sorts its keys, so two snapshots of the same
// observed state are byte-identical and a diff between two different ones
// reads in a stable order. The differ compacts each value before comparing,
// so ordering is not load-bearing for the verdict — only for the reader.
//
// An absent, null or empty status.atProvider reads as an empty object: a
// resource whose observed state has not been populated yet is a normal
// pre-population state, not an error. A status.atProvider that is present
// but is NOT a JSON object is an error rather than a silent empty object —
// every caller treats the snapshot as a field map, so coercing it would
// report "all fields stable" about a status this tool never understood.
func (r *Runner) Snapshot() ([]byte, error) {
	obj, err := r.GetObject()
	if err != nil {
		return nil, fmt.Errorf("reading status.atProvider: %w", err)
	}

	// Navigating with jsonKeyAtProvider as the field (rather than as part
	// of the container) reuses the same descent ReadField uses, and gives
	// the whole subtree instead of one leaf under it.
	val, _, err := navigateJSONPath(obj, []string{jsonKeyStatus}, jsonKeyAtProvider)
	if err != nil {
		return nil, fmt.Errorf("reading status.atProvider: %w", err)
	}
	if val == nil {
		return []byte("{}"), nil
	}
	atProvider, ok := val.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("reading status.atProvider: expected a JSON object, got %T", val)
	}

	data, err := json.Marshal(atProvider)
	if err != nil {
		return nil, fmt.Errorf("marshalling status.atProvider: %w", err)
	}
	return data, nil
}

// ClearConditions clears the status conditions so that a subsequent
// WaitReady blocks until the controller re-establishes them. This is
// the same technique uptest uses in 01-update.yaml.tmpl — it creates
// a reliable signal without timing assumptions.
func (r *Runner) ClearConditions() error {
	_, err := r.kube().PatchMergeStatus(r.namespace, r.resourceName, `{"status":{"conditions":[]}}`)
	if err != nil {
		return fmt.Errorf("clearing conditions: %w", err)
	}
	return nil
}

// updateTesterNudgeAnnotation is patched onto the resource's metadata by
// NudgeReconcile to force an immediate reconcile. It is deliberately not
// under the crossplane.io/ prefix reserved for the runtime's own
// annotations.
const updateTesterNudgeAnnotation = "update-tester.crossplane.io/nudge"

// NudgeReconcile patches a private metadata annotation with a unique value
// to force an immediate controller reconcile.
//
// A pure status-subresource patch (ClearConditions) is not sufficient for
// this: most generated controllers register their watch with
// resource.DesiredStateChanged(), which only reacts to an annotation,
// label, or generation (i.e. spec) change — a status-only write is
// filtered out and never reaches the reconciler. Changing a metadata
// annotation, by contrast, satisfies that predicate and enqueues a
// reconcile through the same path a real spec edit would, without
// touching spec.forProvider (so it cannot itself trigger another
// Update()).
func (r *Runner) NudgeReconcile() error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
		updateTesterNudgeAnnotation, strconv.FormatInt(time.Now().UnixNano(), 10))
	if _, err := r.kube().PatchMerge(r.namespace, r.resourceName, patch); err != nil {
		return fmt.Errorf("nudging reconcile: %w", err)
	}
	return nil
}

// Patch applies a JSON merge patch for the given field and value. clear
// names OTHER top-level spec.forProvider fields to null in the SAME patch,
// and withValues names OTHER top-level spec.forProvider fields to set to an
// explicit, non-null literal value in the SAME patch — see
// manifest.UpdateTest.Clear, manifest.UpdateTest.WithValues and
// buildMergePatch. Pass nil (or an empty slice/map) when the test carries
// no clear or withValues directive.
func (r *Runner) Patch(field string, value interface{}, clear []string, withValues map[string]interface{}) error {
	patchJSON, err := buildMergePatch(field, value, clear, withValues)
	if err != nil {
		return fmt.Errorf("building patch: %w", err)
	}
	_, err = r.kube().PatchMerge(r.namespace, r.resourceName, patchJSON)
	if err != nil {
		return fmt.Errorf("patching %s: %w", field, err)
	}
	return nil
}

// WaitReady waits for the resource to become Ready.
func (r *Runner) WaitReady() error {
	_, err := r.kube().WaitForCondition(r.namespace, r.resourceName, "condition=Ready", r.timeout)
	if err != nil {
		return fmt.Errorf("waiting for Ready: %w", err)
	}
	return nil
}

// GetObject reads the full resource as a decoded JSON object.
func (r *Runner) GetObject() (map[string]interface{}, error) {
	out, err := r.kube().GetObjectJSON(r.namespace, r.resourceName)
	if err != nil {
		return nil, fmt.Errorf("reading resource: %w", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		return nil, fmt.Errorf("parsing resource JSON: %w", err)
	}
	return obj, nil
}

// GetGeneration reads metadata.generation from the live resource.
func (r *Runner) GetGeneration() (int64, error) {
	obj, err := r.GetObject()
	if err != nil {
		return 0, err
	}
	return extractGeneration(obj)
}

// externalNameAnnotation is the well-known crossplane-runtime annotation
// that carries a managed resource's resolved external identifier.
const externalNameAnnotation = "crossplane.io/external-name"

// ExternalName reads the crossplane.io/external-name annotation from the
// live resource. Returns an empty string (no error) if the annotation is
// absent — the caller decides whether that is meaningful.
func (r *Runner) ExternalName() (string, error) {
	obj, err := r.GetObject()
	if err != nil {
		return "", fmt.Errorf("reading external-name: %w", err)
	}
	metadata, _ := obj["metadata"].(map[string]interface{})
	annotations, _ := metadata["annotations"].(map[string]interface{})
	name, _ := annotations[externalNameAnnotation].(string)
	return name, nil
}

// ReadField reads a single field from status.atProvider by fetching the whole
// object with `kubectl get -o json` and parsing it here, rather than asking
// kubectl to extract the field with a jsonpath expression. Parsing the object
// ourselves keeps the result independent of how kubectl renders a non-scalar
// jsonpath result, which has changed across kubectl versions: kubectl before
// ~1.21 printed nested objects in Go syntax ("map[key:value]"), while current
// versions JSON-marshal them. Complex types (maps, arrays) come back as
// canonical JSON; scalars (string, number, bool) as their unquoted value.
//
// Snapshot does the same for the whole subtree, so this is the rule for the
// package rather than a local choice: kubectl is asked for objects, and any
// value that is not a scalar is rendered here.
func (r *Runner) ReadField(field string) (string, error) {
	obj, err := r.GetObject()
	if err != nil {
		return "", fmt.Errorf("reading field %s: %w", field, err)
	}

	val, _, err := navigateAtProvider(obj, field)
	if err != nil {
		return "", err
	}
	return stringifyFieldValue(val, field)
}

// readCurrentValue returns a field's value as it stands BEFORE a patch is
// applied, for no-op detection. It prefers spec.forProvider — the value the
// upcoming merge patch would overwrite, which is what determines whether the
// patch actually changes anything the controller can react to. If the field
// is absent from spec (e.g. it is only ever populated from the live backend),
// it falls back to the resource's live observed state (status.atProvider).
func (r *Runner) readCurrentValue(field string) (string, error) {
	obj, err := r.GetObject()
	if err != nil {
		return "", fmt.Errorf("reading resource for no-op check on %s: %w", field, err)
	}

	val, _, err := navigateSpecForProvider(obj, field)
	if err == nil && val != nil {
		return stringifyFieldValue(val, field)
	}

	// Fall back to the live observed state.
	atVal, _, atErr := navigateAtProvider(obj, field)
	if atErr != nil {
		return "", atErr
	}
	return stringifyFieldValue(atVal, field)
}

// stringifyFieldValue converts a decoded-JSON value into the string
// representation used throughout this package for comparisons: strings are
// returned unquoted (consistent with kubectl jsonpath behaviour and how YAML
// annotation values are represented), everything else (numbers, booleans,
// maps, arrays, and nil) is returned as canonical JSON.
func stringifyFieldValue(val interface{}, field string) (string, error) {
	if val == nil {
		return "", nil
	}
	if s, ok := val.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return "", fmt.Errorf("marshalling field %s for comparison: %w", field, err)
	}
	return string(b), nil
}

// jsonKeyAtProvider is the status subfield holding the last-observed backend
// state. jsonKeyStatus is the top-level status field. Both are constants
// (rather than repeated literals) because they are referenced from this
// file, converge.go, and their tests.
const (
	jsonKeyStatus     = "status"
	jsonKeyAtProvider = "atProvider"
)

// navigateAtProvider navigates a resource JSON object to
// status.atProvider.<dot-separated-field> and returns the value found there,
// plus whether the field actually exists (see navigateJSONPath for why the
// distinction matters).
func navigateAtProvider(obj map[string]interface{}, field string) (interface{}, bool, error) {
	return navigateJSONPath(obj, []string{jsonKeyStatus, jsonKeyAtProvider}, field)
}

// navigateSpecForProvider navigates a resource JSON object to
// spec.forProvider.<dot-separated-field> and returns the value found there,
// plus whether the field actually exists (see navigateJSONPath for why the
// distinction matters).
func navigateSpecForProvider(obj map[string]interface{}, field string) (interface{}, bool, error) {
	return navigateJSONPath(obj, []string{"spec", "forProvider"}, field)
}

// navigateJSONPath descends obj through each key in container (e.g.
// ["status", "atProvider"]), then further descends through the
// dot-separated field path under that container. Returns exists=false when
// any segment — container or field — is missing, and an error if a
// non-terminal segment resolves to something other than a JSON object.
//
// The returned bool is what lets a caller tell a genuinely-absent field
// apart from one that is present but holds a JSON null or empty value: both
// stringify to the same "" via stringifyFieldValue, so a caller that only
// ever looked at the value (as this function used to do, returning a bare
// nil for "missing") cannot recover that distinction after the fact. Most
// callers still don't need it — ReadField and readCurrentValue treat both
// cases as "nothing to report" — but readAssertUnchangedBaselines does: a
// silently-absent field there is a typo in the manifest, not a legitimate
// empty baseline, and letting the two collapse into the same "" is exactly
// the defect that let an unresolvable assert-unchanged path pass vacuously.
func navigateJSONPath(obj map[string]interface{}, container []string, field string) (interface{}, bool, error) {
	var curr interface{} = obj
	for _, key := range container {
		m, ok := curr.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("%s is not a JSON object", strings.Join(container, "."))
		}
		v, exists := m[key]
		if !exists {
			return nil, false, nil
		}
		curr = v
	}

	for _, part := range strings.Split(field, ".") {
		m, ok := curr.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("cannot navigate to %q: parent is not a JSON object", part)
		}
		v, exists := m[part]
		if !exists {
			return nil, false, nil
		}
		curr = v
	}
	return curr, true, nil
}

// jsonEqual compares an expected Go value (from a YAML annotation) with an
// actual string returned by ReadField.
//
// For string values, it compares directly because ReadField returns unquoted
// string values — the same representation that YAML gives for string scalars.
//
// For complex types (maps, arrays) and numbers, both expected and actual are
// normalised through JSON (marshal → unmarshal) so that type differences
// introduced by YAML parsing (e.g., int vs float64) do not cause false
// failures. reflect.DeepEqual is used on the normalised values.
//
// Falls back to string comparison when JSON normalisation is not possible.
//
// An explicit `value: null` self-tombstone entry (manifest.UpdateTest.
// ValueExplicit) decodes to a Go nil exactly like an ordinary "no expected
// value" would, so expected == nil here is read as "the field is expected
// to be absent" rather than as the JSON literal "null". Once a merge patch
// has actually cleared the field, ReadField/stringifyFieldValue return ""
// for BOTH "the key is gone entirely" and "the key holds JSON null" — see
// navigateJSONPath's own doc comment for why that collapse happens — and
// neither shape can ever stringify to the 4-byte string "null" that a
// literal JSON comparison would otherwise demand. Comparing expected == nil
// against actual == "" is therefore the correct — and only reachable —
// success condition for a null expectation, not a special case invented
// for it.
func jsonEqual(expected interface{}, actual string) bool {
	// String scalars: compare directly.
	// ReadField strips JSON quotes from strings, so actual == "hello" not `"hello"`.
	if s, ok := expected.(string); ok {
		return s == actual
	}

	if expected == nil {
		return actual == ""
	}

	// Marshal expected to canonical JSON.
	expectedBytes, err := json.Marshal(expected)
	if err != nil {
		// Cannot marshal — fall back to Sprintf comparison.
		return fmt.Sprintf("%v", expected) == actual
	}

	// Normalise both through JSON parsing so that numeric types align
	// (YAML gives int, JSON gives float64 for the same numeric literal).
	var expectedNorm, actualNorm interface{}
	if json.Unmarshal(expectedBytes, &expectedNorm) == nil {
		if json.Unmarshal([]byte(actual), &actualNorm) == nil {
			return reflect.DeepEqual(expectedNorm, actualNorm)
		}
	}

	// Fall back to plain string comparison.
	return string(expectedBytes) == actual
}

// compareFieldValue is jsonEqual, widened to optionally ignore a set of
// top-level map member keys (ignoreMapKeys) and/or a set of per-element
// keys of a list-of-objects (ignoreListElementKeys) on both sides before
// comparing — see manifest.UpdateTest.IgnoreMapKeys and
// manifest.UpdateTest.IgnoreListElementKeys for why each exists.
//
// With both empty it is exactly jsonEqual (same signature, same
// behaviour), so every existing caller and every existing test that never
// sets either directive is unaffected.
//
// With either non-empty, expected and actual are each normalised through
// JSON, the named keys are deleted — ignoreMapKeys from the top level of
// whichever side is a JSON object, ignoreListElementKeys from every element
// of whichever side is a JSON array of objects — and the (possibly
// reduced) results are compared with reflect.DeepEqual. A side whose shape
// does not match what a given directive strips is not a hard error here —
// deleting a key from something that has no keys, or from elements of
// something that is not an array, is a no-op, so a still-converging field
// or a genuinely differently-shaped field simply never satisfies the
// comparison, which surfaces through the ordinary poll-timeout / FAIL path
// with expected and actual both printed, rather than a separate error
// channel. The two directives are independent and both are always applied
// — an entry only ever populates the one matching its own field's shape
// (manifest.ValidateIgnoreMapKeys / ValidateIgnoreListElementKeys validate
// each independently), so applying both unconditionally costs nothing on
// the side that has nothing of that shape to strip.
func compareFieldValue(expected interface{}, actual string, ignoreMapKeys, ignoreListElementKeys []string) bool {
	if len(ignoreMapKeys) == 0 && len(ignoreListElementKeys) == 0 {
		return jsonEqual(expected, actual)
	}

	expectedBytes, err := json.Marshal(expected)
	if err != nil {
		return false
	}
	var expectedNorm, actualNorm interface{}
	if json.Unmarshal(expectedBytes, &expectedNorm) != nil {
		return false
	}
	if json.Unmarshal([]byte(actual), &actualNorm) != nil {
		return false
	}

	stripKeys(expectedNorm, ignoreMapKeys)
	stripKeys(actualNorm, ignoreMapKeys)
	stripListElementKeys(expectedNorm, ignoreListElementKeys)
	stripListElementKeys(actualNorm, ignoreListElementKeys)

	return reflect.DeepEqual(expectedNorm, actualNorm)
}

// stripKeys deletes the named keys from val in place when val is a JSON
// object (map[string]interface{}, the only shape json.Unmarshal produces
// for a YAML/JSON mapping). Any other shape (scalar, array, nil) is left
// untouched — there is nothing to strip a top-level map key from.
func stripKeys(val interface{}, keys []string) {
	m, ok := val.(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range keys {
		delete(m, k)
	}
}

// stripListElementKeys deletes the named keys, in place, from every element
// of val that is itself a JSON object, when val is a JSON array
// ([]interface{}, the only shape json.Unmarshal produces for a YAML/JSON
// sequence). Any other shape (scalar, object, nil) is left untouched, and
// an element of the array that is not itself an object is skipped rather
// than erroring — there is nothing to strip a per-element key from either
// one.
func stripListElementKeys(val interface{}, keys []string) {
	if len(keys) == 0 {
		return
	}
	arr, ok := val.([]interface{})
	if !ok {
		return
	}
	for _, elem := range arr {
		if m, ok := elem.(map[string]interface{}); ok {
			for _, k := range keys {
				delete(m, k)
			}
		}
	}
}

// formatExpected converts an expected annotation value to a human-readable
// display string. For strings, the value is returned directly. For complex
// types (maps, arrays, numbers), it returns the canonical JSON representation.
func formatExpected(val interface{}) string {
	if s, ok := val.(string); ok {
		return s
	}
	b, err := json.Marshal(val)
	if err != nil {
		return fmt.Sprintf("%v", val)
	}
	return string(b)
}

// TestResult holds the outcome of a single field update test.
type TestResult struct {
	Field string
	// Skipped marks a test that was never attempted because the manifest
	// annotation explicitly opted it out (see SkipMsg for the reason).
	Skipped bool
	SkipMsg string
	// NoOp marks a test whose pre-patch value already equalled the target
	// value: the patch below could not have exercised the Update() path, so
	// this is reported as a distinct failure rather than a false PASS.
	NoOp   bool
	Passed bool
	// Before is the field's value as read immediately before the patch was
	// applied (the same read the no-op check uses), valid only when
	// BeforeKnown is true — that read can fail (e.g. the field is absent
	// from both spec.forProvider and status.atProvider before the first
	// write), and an empty string is also a legitimate field value, so a
	// separate flag is needed to tell "read failed" from "read an empty
	// value" apart. Display code uses Before, not Expected, as the left
	// side of a PASS line: Expected and Actual are both the post-update
	// target, so printing "expected → actual" on a successful transition
	// shows the same value on both sides and reads as a no-op that never
	// happened. Before → Actual is what actually changed.
	Before      string
	BeforeKnown bool
	Expected    string
	Actual      string
	Duration    time.Duration
	Error       error
	SideFx      []differ.FieldChange
	// UpdateEvidenced records whether the aggregated count of
	// UpdatedExternalResource/CannotUpdateExternalResource events for this
	// resource increased between the pre-patch baseline and the post-patch
	// check. This is a wall-clock-independent proof that the reconciler's
	// Update() path actually executed — see NotEvidenced for what it means
	// when this is false but the field value still matched.
	UpdateEvidenced bool
	// NotEvidenced marks a test whose observed value matched the target
	// (Passed would otherwise be true) but for which no update event was
	// ever recorded. Convergence timing alone cannot tell "updated
	// promptly, event observed late" apart from "value happened to already
	// match, Update() never ran" — the event count is the deterministic
	// signal, so a match without it is downgraded from PASS to this
	// distinct failure rather than left indistinguishable from a genuine
	// pass.
	NotEvidenced bool
	// SlowObserve marks a PASSing, evidenced field whose total duration met
	// or exceeded slowObserveThreshold — half the provider's declared poll
	// interval, so the annotation reads as "this took poll-cycle-scale
	// time" on any provider rather than "this took 30 seconds". The runner
	// already forces a second, independent reconcile after Update() so
	// status.atProvider is refreshed by a fresh Observe rather than
	// depending on the provider's background poll tick — so this should be
	// rare. When it does happen, it reflects a genuine backend propagation
	// delay rather than an ambiguous result: UpdateEvidenced is already
	// true, so the slow duration is reported as a labelled variant of
	// PASS, not a reason to doubt the verdict.
	SlowObserve bool
	// EvidenceUntrusted marks a field test whose evidence check ran after a
	// resetEventBurst attempt failed earlier in this run. A failed reset
	// leaves the controller's in-process event-spam-filter burst
	// (eventBurstCeiling) unrefreshed, so the aggregated event count this
	// field's evidence check relies on can no longer be trusted to prove
	// OR disprove that Update() ran: a PASS could be masking silently
	// dropped events, and a NOT-EVIDENCED could be a false failure caused
	// by the same drop. RunTests sets this on every non-skipped, non-no-op
	// result from the point of the first reset failure onward, and callers
	// must treat any run containing one as failed — "0 not-evidenced"
	// must never print as a clean PASS when the evidence behind it is
	// unreliable.
	EvidenceUntrusted bool
}

// defaultPollInterval is the provider poll interval assumed when the caller
// declares none. It is the crossplane-runtime default, and the value the
// slow-observe threshold has always been calibrated against — so a Runner
// with no declared interval behaves exactly as it did before the interval
// became an input.
const defaultPollInterval = 60 * time.Second

// slowObserveDivisor turns a poll interval into the slow-observe threshold.
// Half a poll cycle is comfortably above the couple of seconds a forced
// reconcile normally takes, while still well under a full cycle — so a
// field that only converged because the provider's background poll caught
// up is always over the line, and a field that converged on the forced
// reconcile never is.
const slowObserveDivisor = 2

// slowObserveThreshold is the duration at or above which a passing,
// evidenced field test is annotated SlowObserve instead of being reported
// as a plain, fast PASS.
//
// It is derived from the provider's poll interval rather than fixed,
// because that interval is what makes a duration meaningful: the annotation
// exists to say "this field only converged on the scale of a poll cycle",
// and a provider polling every 10s and one polling every 300s disagree by a
// factor of 30 about what that means. A fixed 30s bar would annotate almost
// every pass of the first and never annotate a genuinely poll-scale pass of
// the second — noise in one direction, a missed signal in the other.
func (r *Runner) slowObserveThreshold() time.Duration {
	interval := r.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return interval / slowObserveDivisor
}

// defaultEventBurstCeiling caps the number of Update()-triggering field
// tests this runner attempts against a single controller process before
// proactively restarting it. client-go's EventBroadcaster spam filter
// allows a burst of ~25 identical events per involved object before
// silently DROPPING further ones — not folding them into an existing
// aggregated Event's .count — so a resource with more mutable fields than
// that burst produces false NOT-EVIDENCED results once the burst is spent,
// even though the controller's Update() path ran correctly every time (see
// applyEvidenceCheck and countUpdateEvents). Restarting the controller
// discards that in-memory state — a brand new process starts with a fresh
// burst and, because client-go's event-aggregation cache is also in-memory
// and per-process, emits a brand NEW Event object afterward rather than
// continuing to patch the throttled one. countUpdateEvents already sums
// every matching Event object for the resource regardless of which one
// carries the count, so no change to the counting logic itself is needed —
// only to how often the controller gets a fresh burst. The margin below the
// documented ~25-token ceiling leaves room for a stray incidental event
// (e.g. a transient CannotUpdateExternalResource) without crossing the hard
// limit before the proactive reset fires.
//
// A controlplane running a raised client-go burst has nothing to earn back
// by restarting this often; eventBurstCeiling lets that be tuned per run
// via eventBurstCeilingEnvVar rather than paying a restart every
// defaultEventBurstCeiling fields regardless.
const defaultEventBurstCeiling = 20

// eventBurstCeilingEnvVar names the environment variable that overrides
// defaultEventBurstCeiling — see NewRunner and the eventBurstCeiling
// accessor.
const eventBurstCeilingEnvVar = "UPDATE_TESTER_EVENT_BURST_CEILING"

// eventBurstCeiling returns the effective burst ceiling for this Runner:
// the value read from eventBurstCeilingEnvVar at construction time (see
// NewRunner), falling back to defaultEventBurstCeiling for any
// unset, unparseable, zero or negative value. Every call site that used to
// reference the eventBurstCeiling constant directly now calls this
// accessor instead, so a Runner built by a test helper (which leaves
// burstCeiling at its zero value) behaves exactly as production does.
func (r *Runner) eventBurstCeiling() int {
	if r.burstCeiling > 0 {
		return r.burstCeiling
	}
	return defaultEventBurstCeiling
}

// RunTests executes all update tests from the manifest and returns results,
// plus every manifest-declared assert-unchanged field (see
// manifest.Manifest.AssertUnchanged) that drifted from its pre-run baseline
// at any point during the run — a GATING failure the caller must treat the
// same as a failed field test. See UnchangedAssertion and
// checkAssertUnchanged.
func (r *Runner) RunTests(m *manifest.Manifest) ([]TestResult, []UnchangedAssertion, error) {
	if err := r.ResolveResource(m); err != nil {
		return nil, nil, err
	}

	snapshot, err := r.Snapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("initial snapshot: %w", err)
	}

	baselines, err := readAssertUnchangedBaselines(snapshot, m.AssertUnchanged)
	if err != nil {
		return nil, nil, err
	}
	// violatedFields tracks which assert-unchanged fields have already been
	// reported, so a field that stays wiped for the rest of the run is
	// reported exactly once — attributed to the field test that first moved
	// it — rather than once per remaining field test.
	violatedFields := make(map[string]bool, len(m.AssertUnchanged))
	var violations []UnchangedAssertion

	var results []TestResult
	var attemptsSinceReset int
	// eventBaseline is the real, measured event count as of the last reset
	// (or the run's start, if no reset has happened yet) — see the mid-loop
	// block below for why a real delta against this baseline, not the
	// 1-per-attempt attemptsSinceReset estimate alone, is what decides
	// whether a field test starts with a spent burst. haveEventBaseline is
	// false only when the count could not be measured at all (e.g. RBAC
	// denies listing events), in which case the mid-loop check degrades to
	// attemptsSinceReset exactly as it did before this baseline existed.
	var eventBaseline int
	var haveEventBaseline bool
	// resetFailed goes true the first time resetEventBurst fails and stays
	// true for the rest of the run. A failed reset leaves the controller's
	// in-process event-spam-filter burst unrefreshed, so every evidence
	// check from that point on is running against a burst of unknown
	// remaining capacity — it may still have headroom (evidence stays
	// trustworthy) or may already be silently dropping events (a PASS
	// could be masking a real Update() failure, or a NOT-EVIDENCED could be
	// a false failure). There is no live signal that tells the runner which
	// case it is in, so once trust is lost it is not re-earned by treating
	// a later reset attempt as sufficient on its own — the run-level
	// verdict must reflect that some portion of the evidence is unreliable
	// rather than silently reporting a clean pass.
	var resetFailed bool

	// Creation-time settling can itself exhaust the object's event
	// spam-filter burst before this loop issues its own first patch. A
	// resource whose desired state needs several corrective Update()
	// rounds to reach Ready (late-init discovering server-normalized
	// sub-fields, an oneof default settling, etc.) accumulates those as
	// real UpdatedExternalResource/CannotUpdateExternalResource events
	// against the SAME per-object burst attemptsSinceReset tracks below —
	// and that consumption is invisible to attemptsSinceReset, which only
	// counts patches THIS run issues. Once the burst is spent, a fresh,
	// entirely legitimate Update() event from the first field test is
	// silently dropped by the spam filter rather than delayed, so no
	// retry window on the read side (evidenceRetryWindow) can recover it:
	// the write itself never lands. Measured live (ticket 6bb473df):
	// HttpLoadbalancer's own settling from six newly-populated slice
	// fields left 32 pre-existing update events on the object — already
	// past the ceiling below — and every field test's own patch then
	// reported NOT-EVIDENCED with the aggregated count frozen at 32 for
	// the whole 10s retry window on both fields, while every other
	// resource in the same run (starting from a low, non-ceiling count)
	// evidenced its own patch inside 200ms. Checking the SAME ceiling
	// used mid-run, once, before the first field test, and earning a
	// fresh burst up front when it is already spent closes that gap: the
	// field loop below never becomes aware this happened, it just starts
	// attemptsSinceReset from a controller guaranteed to have full
	// headroom. A failed count here is tolerated exactly like every other
	// count failure in this package — proceed without resetting rather
	// than block the run on a count this method was never guaranteed to
	// get.
	if preExisting, preErr := r.countUpdateEvents(m.Kind, m.Name, m.Namespace, m.APIVersion); preErr == nil {
		eventBaseline = preExisting
		haveEventBaseline = true
		if preExisting >= r.eventBurstCeiling() {
			if err := r.resetEventBurst(); err != nil {
				fmt.Fprintf(os.Stderr, "    warning: resetting controller event burst (pre-run, %d pre-existing events): %v\n", preExisting, err)
				resetFailed = true
			}
			// A restart discards the controller's in-process burst
			// state, not the Event objects already recorded against
			// this resource — countUpdateEvents keeps summing every
			// one of them, old and new, for the rest of the run (see
			// its doc comment). preExisting therefore remains the
			// correct baseline to measure NEW growth against
			// regardless of whether the reset itself succeeded.
		}
	}

	for _, t := range m.Tests {
		if t.Skip.Present() {
			results = append(results, TestResult{
				Field:   t.Field,
				Skipped: true,
				SkipMsg: t.Skip.Describe(),
			})
			continue
		}

		// Proactively earn back a fresh event-spam-filter burst BEFORE it
		// would be exhausted, rather than reacting after the fact — once a
		// burst is spent, the dropped events are gone for good (see
		// eventBurstCeiling), so there is nothing to "catch up" on the next
		// field. attemptsSinceReset alone assumes exactly one event per
		// field-test attempt; measured live (ServicePolicyRule, several
		// attempts producing 3 events each) that assumption undercounts,
		// so the real burst can already be spent while attemptsSinceReset
		// is still well short of eventBurstCeiling — the four fields
		// tested right after that point come back NOT-EVIDENCED with no
		// EvidenceUntrusted flag, because resetEventBurst is never even
		// attempted for them. Re-measuring the actual count here and
		// comparing its growth since the last reset (eventBaseline) to
		// the same ceiling closes that gap; attemptsSinceReset remains
		// the fallback trigger for the one case a real measurement can't
		// cover — countUpdateEvents itself failing (e.g. RBAC denies
		// listing events).
		//
		// A failed reset does not abort the run: attemptsSinceReset
		// still clears so the runner does not retry the restart before
		// every remaining field, and later fields simply lose the
		// burst-avoidance benefit rather than the whole run. The run's
		// reported verdict is degraded instead — see resetFailed and
		// EvidenceUntrusted.
		shouldReset := attemptsSinceReset >= r.eventBurstCeiling()
		current, curErr := r.countUpdateEvents(m.Kind, m.Name, m.Namespace, m.APIVersion)
		if curErr == nil && haveEventBaseline && current-eventBaseline >= r.eventBurstCeiling() {
			shouldReset = true
		}
		if shouldReset {
			if err := r.resetEventBurst(); err != nil {
				fmt.Fprintf(os.Stderr, "    warning: resetting controller event burst: %v\n", err)
				resetFailed = true
			}
			attemptsSinceReset = 0
			// Re-baseline against the measurement taken above — the
			// restart does not change the already-recorded event
			// total (see the pre-loop comment above), so this stays
			// accurate whether or not the reset itself succeeded.
			if curErr == nil {
				eventBaseline = current
				haveEventBaseline = true
			}
		}

		var result TestResult
		result, snapshot = r.runFieldTest(t, snapshot, m.Kind, m.Name, m.Namespace, m.APIVersion)
		// A no-op test never reaches applyPatchAndReconcile, so it never
		// consults the event-evidence check — the burst reset's success or
		// failure is irrelevant to it.
		if resetFailed && !result.NoOp {
			result.EvidenceUntrusted = true
		}
		results = append(results, result)
		// Only a real (non-no-op) patch has a chance of consuming a burst
		// token — a no-op test short-circuits before applyPatchAndReconcile
		// ever runs, so it never emits an update event.
		if !result.NoOp {
			attemptsSinceReset++
		}

		// Assert-unchanged check: a skipped or no-op test never advances
		// snapshot past its pre-loop value, so there is nothing new to
		// check against the baseline either way.
		if !result.Skipped && !result.NoOp {
			newViolations, cerr := checkAssertUnchanged(snapshot, m.AssertUnchanged, baselines, violatedFields, t.Field)
			if cerr != nil && results[len(results)-1].Error == nil {
				results[len(results)-1].Error = fmt.Errorf("checking assert-unchanged fields: %w", cerr)
			}
			violations = append(violations, newViolations...)
		}
	}

	return results, violations, nil
}

// providerDeploymentNamespace is where the Crossplane package manager
// deploys every provider's controller Deployment.
const providerDeploymentNamespace = "crossplane-system"

// providerDeploymentSelector matches the controller Pod for an installed
// provider package revision — the same label the provider build/deploy flow
// already waits on before running any E2E test. It does NOT match the
// Deployment object itself: Crossplane's package manager places this label
// on the Deployment's spec.selector.matchLabels and
// spec.template.metadata.labels, never on the Deployment's own
// metadata.labels, so `kubectl get deploy -l pkg.crossplane.io/revision`
// always returns zero results (verified live against a Crossplane v2.2.1
// cluster). The label IS present on the Pod's metadata.labels, which is
// what restartControllerDeployment selects against.
const providerDeploymentSelector = "pkg.crossplane.io/revision"

// providerDeploymentEnvVar names the environment variable that pins which
// provider controller Deployment resetEventBurst restarts. It is required
// on a cluster running more than one Crossplane provider package, where the
// pod-label lookup cannot tell which revision belongs to the provider under
// test — see resolveControllerDeploymentName.
const providerDeploymentEnvVar = "UPDATE_TESTER_PROVIDER_DEPLOYMENT"

// controllerRestartTimeout bounds how long resetEventBurst waits for the
// restarted controller Deployment to report a ready replica.
const controllerRestartTimeout = 180 * time.Second

// resetEventBurst restarts the provider's controller Deployment to discard
// its in-process event-spam-filter and event-aggregation-cache state (see
// eventBurstCeiling), then waits for a fresh replica to become ready.
// Restarting mid-run is safe: crossplane-runtime's managed reconciler holds
// no state that this tool depends on across restarts — every reconcile the
// runner cares about is driven explicitly (ClearConditions + WaitReady, or
// NudgeReconcile) rather than inferred from anything the previous process
// remembered.
func (r *Runner) resetEventBurst() error {
	if r.restartFunc != nil {
		return r.restartFunc()
	}
	return r.restartControllerDeployment()
}

// restartControllerDeployment resolves the running provider's controller
// Deployment and issues a rolling restart, then blocks until the rollout
// reports ready.
func (r *Runner) restartControllerDeployment() error {
	name, err := r.resolveControllerDeploymentName()
	if err != nil {
		return fmt.Errorf("resolving provider deployment: %w", err)
	}
	target := "deploy/" + name

	if _, err := r.kube().RolloutRestart(providerDeploymentNamespace, target); err != nil {
		return fmt.Errorf("restarting %s: %w", target, err)
	}
	if _, err := r.kube().RolloutStatus(providerDeploymentNamespace, target,
		controllerRestartTimeout.String()); err != nil {
		return fmt.Errorf("waiting for %s rollout: %w", target, err)
	}
	return nil
}

// resolveControllerDeploymentName finds the provider controller Deployment's
// name by reading it off its Pod rather than the Deployment object itself
// (see providerDeploymentSelector for why the Deployment can't be selected
// directly). Crossplane names the Deployment after the installed package
// revision, and stamps that exact name onto the Pod's
// pkg.crossplane.io/revision label — so reading the label's value off the
// (correctly selectable) Pod recovers the Deployment name without walking
// ownerReferences through a ReplicaSet.
//
// The selector matches the controller Pod of EVERY installed provider
// package, not just the one under test, so the lookup must enumerate all of
// them instead of taking the first item: on a cluster with more than one
// provider installed, picking arbitrarily restarts some other provider's
// controller and the burst reset this exists to perform silently does
// nothing — which then degrades every subsequent evidence check to
// UNTRUSTED without ever saying why. When the label value is ambiguous the
// operator is asked to disambiguate via providerDeploymentEnvVar rather
// than the runner guessing; an explicit override always wins and skips the
// lookup entirely.
func (r *Runner) resolveControllerDeploymentName() (string, error) {
	if override := strings.TrimSpace(os.Getenv(providerDeploymentEnvVar)); override != "" {
		return override, nil
	}

	names, err := r.kube().ControllerRevisionLabels(providerDeploymentNamespace, providerDeploymentSelector)
	if err != nil {
		return "", err
	}

	switch len(names) {
	case 0:
		return "", fmt.Errorf("no pod found with label %s in namespace %s",
			providerDeploymentSelector, providerDeploymentNamespace)
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf(
			"%d provider controller deployments match label %s in namespace %s (%s) — set %s to the one under test",
			len(names), providerDeploymentSelector, providerDeploymentNamespace,
			strings.Join(names, ", "), providerDeploymentEnvVar)
	}
}

// uniqueNonEmptyLines splits kubectl output into trimmed, non-empty lines,
// preserving order and dropping duplicates. Deduplication matters because
// one Deployment can have several Pods (a scaled-out or mid-rollout
// controller), and every one of them carries the same revision label —
// which is one deployment, not an ambiguity.
func uniqueNonEmptyLines(out string) []string {
	seen := make(map[string]bool)
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return lines
}

// controllerPodIdentity captures the observable identity of the provider
// controller Pod actually running at the instant it was read: its own
// Pod name (NOT the Deployment name resolveControllerDeploymentName
// returns — a rolling restart replaces the Pod object while the
// Deployment's name stays fixed) and when that Pod was created.
//
// Two reads whose Name differs mean the Pod was replaced between them —
// by resetEventBurst, an OOM kill, an operator action, a package
// re-install, or anything else. convergeArm and convergeAssert
// deliberately never ask which cause it was; the cluster fact that the
// process changed is all either of them needs.
type controllerPodIdentity struct {
	Name      string
	CreatedAt time.Time
}

// resolveControllerPodIdentity reports the provider controller Pod's
// current identity, deferring to podIdentityFunc when a test has set one.
func (r *Runner) resolveControllerPodIdentity() (controllerPodIdentity, error) {
	if r.podIdentityFunc != nil {
		return r.podIdentityFunc()
	}
	return r.resolveControllerPodIdentityLive()
}

// resolveControllerPodIdentityLive resolves the provider controller
// Deployment exactly as restartControllerDeployment does, then reads the
// name and creation time of the Pod(s) currently running under it. A
// rollout can briefly show more than one Pod (the old one mid-termination
// alongside the new one); the entry with the greatest CreatedAt is the one
// currently running, so that is the one reported — see
// latestControllerPodIdentity.
func (r *Runner) resolveControllerPodIdentityLive() (controllerPodIdentity, error) {
	name, err := r.resolveControllerDeploymentName()
	if err != nil {
		return controllerPodIdentity{}, fmt.Errorf("resolving provider deployment: %w", err)
	}

	identities, err := r.kube().ControllerPodIdentities(providerDeploymentNamespace, providerDeploymentSelector+"="+name)
	if err != nil {
		return controllerPodIdentity{}, fmt.Errorf("listing controller pods for deployment %s: %w", name, err)
	}

	identity, found := latestControllerPodIdentity(identities)
	if !found {
		return controllerPodIdentity{}, fmt.Errorf("no controller pod found for deployment %s in namespace %s", name, providerDeploymentNamespace)
	}
	return identity, nil
}

// parseControllerPodIdentities parses the "<pod-name>\t<creationTimestamp>"
// lines resolveControllerPodIdentityLive's kubectl query produces, one per
// Pod. A line whose timestamp component is missing or fails to parse as
// RFC3339 (the format kubectl's jsonpath always emits for a
// metav1.Time) reports a zero CreatedAt rather than an error — read by
// every caller as "very old", the correct conservative default for a
// shape this parser cannot otherwise make sense of.
func parseControllerPodIdentities(out string) []controllerPodIdentity {
	var identities []controllerPodIdentity
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		id := controllerPodIdentity{Name: parts[0]}
		if len(parts) == 2 {
			if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])); err == nil {
				id.CreatedAt = ts
			}
		}
		identities = append(identities, id)
	}
	return identities
}

// latestControllerPodIdentity returns whichever identity carries the
// greatest CreatedAt — the Pod currently running, since an older entry can
// only be one mid-termination during a rollout (see
// resolveControllerPodIdentityLive). found is false for an empty input.
func latestControllerPodIdentity(identities []controllerPodIdentity) (identity controllerPodIdentity, found bool) {
	if len(identities) == 0 {
		return controllerPodIdentity{}, false
	}
	latest := identities[0]
	for _, id := range identities[1:] {
		if id.CreatedAt.After(latest.CreatedAt) {
			latest = id
		}
	}
	return latest, true
}

// runFieldTest executes a single (non-skipped) update test: it patches the
// field, waits for the controller to reconcile, forces a second independent
// reconcile so status.atProvider reflects a fresh Observe rather than a
// stale one, polls status.atProvider for the expected value, checks for
// positive event evidence that Update() actually ran, and runs the
// differential assertion against the prior snapshot. kind, name, namespace
// and apiVersion identify the resource for the event-evidence lookup — see
// countUpdateEvents for why all four matter, not just kind/name. It returns
// the test result and the snapshot to use for the next test (unchanged from
// the input snapshot if the test aborted early).
func (r *Runner) runFieldTest(t manifest.UpdateTest, snapshot []byte, kind, name, namespace, apiVersion string) (TestResult, []byte) {
	start := time.Now()
	result := TestResult{Field: t.Field}

	// Determine expected value for comparison and display.
	// Use JSON equality (jsonEqual) to handle complex types (maps, arrays)
	// where fmt.Sprintf("%v") produces Go-format strings (map[key:val])
	// that don't match the JSON returned by kubectl.
	expectedVal := t.Value
	if t.Expect != nil {
		expectedVal = t.Expect
	}
	expected := formatExpected(expectedVal)

	// No-op detection: read the field's current value BEFORE patching. A
	// merge patch that repeats the value already in spec.forProvider makes
	// no change for the API server to persist, so metadata.generation never
	// bumps and the controller's Update() path is never invoked. Left
	// undetected, the poll below would simply re-observe the value that was
	// already there and report a false PASS — indistinguishable from a
	// controller with no Update() implementation at all. Report this as a
	// failure instead, distinct from both PASS and SKIP, so the stale test
	// value gets fixed.
	before, beforeErr := r.readCurrentValue(t.Field)
	if beforeErr == nil {
		result.Before = before
		result.BeforeKnown = true
	}
	if beforeErr == nil && jsonEqual(t.Value, before) {
		result.NoOp = true
		result.Expected = expected
		result.Actual = before
		result.Error = fmt.Errorf("no-op: %s already equals %s — patch cannot exercise the update path",
			t.Field, formatExpected(t.Value))
		result.Duration = time.Since(start)
		return result, snapshot
	}

	// Evidence baseline: count update-related events BEFORE patching, so a
	// later delta proves whether Update() executed — a signal that does not
	// depend on wall-clock convergence timing. A failure here does not abort
	// the field test (the value-based assertions below are still useful);
	// it only disables the evidence check for this field. countUpdateEvents
	// sums each Event's aggregated .Count field (with a zero-guard treating
	// an absent/zero .Count as one occurrence) — not a raw Item count.
	eventsBefore, eventsBeforeErr := r.countUpdateEvents(kind, name, namespace, apiVersion)

	if err := r.applyPatchAndReconcile(t); err != nil {
		result.Error = err
		result.Duration = time.Since(start)
		return result, snapshot
	}

	actual, err := r.pollField(t.Field, expectedVal, start, t.IgnoreMapKeys, t.IgnoreListElementKeys)
	if err != nil {
		result.Error = err
	}

	result.Expected = expected
	result.Actual = actual
	result.Passed = compareFieldValue(expectedVal, actual, t.IgnoreMapKeys, t.IgnoreListElementKeys)
	result.Duration = time.Since(start)

	// Evidence check: did the aggregated update-event count actually grow?
	// A failure to count (e.g. RBAC denies listing events) leaves checked
	// false without downgrading the result — the evidence check could not
	// run either way, which is different from having run and come back
	// empty.
	r.applyEvidenceCheck(&result, kind, name, namespace, apiVersion, t.Field, eventsBefore, eventsBeforeErr)

	if result.Passed && result.Duration >= r.slowObserveThreshold() {
		result.SlowObserve = true
	}

	// Differential assertion
	newSnapshot, err := r.Snapshot()
	if err != nil {
		result.Error = fmt.Errorf("post-update snapshot: %w", err)
		return result, snapshot
	}

	// For nested fields, use only the top-level key for diff exclusion.
	topField := t.Field
	if idx := strings.Index(t.Field, "."); idx != -1 {
		topField = t.Field[:idx]
	}

	changes, err := differ.DiffSnapshots(snapshot, newSnapshot, topField)
	if err != nil {
		result.Error = fmt.Errorf("diff: %w", err)
		return result, snapshot
	}
	result.SideFx = changes

	return result, newSnapshot
}

// applyPatchAndReconcile patches the target field, then drives the
// controller through two independent reconciles. The first is the reconcile
// in which Update() actually runs — but its own Observe() ran BEFORE
// Update(), so status.atProvider is still stale when it completes. The
// second is forced purely to obtain a fresh Observe of the now-updated
// external resource, so atProvider does not depend on the provider's
// background poll tick to refresh (which can be a full poll interval away).
// Both reconciles are triggered by NudgeReconcile rather than relying on
// ClearConditions alone: most generated controllers watch with
// resource.DesiredStateChanged(), which reacts only to an annotation,
// label, or generation (spec) change, so a status-only patch on its own
// would be filtered out and never reach the reconciler — leaving either
// reconcile waiting on the same background poll tick this is meant to
// avoid.
func (r *Runner) applyPatchAndReconcile(t manifest.UpdateTest) error {
	if err := r.Patch(t.Field, t.Value, t.Clear, t.WithValues); err != nil {
		return err
	}
	if err := r.reconcileOnce(); err != nil {
		return err
	}
	return r.nudgeAndReconcile()
}

// reconcileOnce clears status conditions, THEN nudges the controller, THEN
// waits for Ready, THEN waits for Synced — in that order, always — so the
// caller can block on the NEXT reconcile's OUTCOME rather than the stale
// conditions already present.
//
// Clearing conditions does not by itself trigger that next reconcile: most
// generated controllers watch with resource.DesiredStateChanged(), which
// reacts only to an annotation, label, or generation (spec) change, so the
// status-only clear is filtered out and never reaches the reconciler on its
// own. A caller's preceding Patch() usually does trigger a reconcile
// through that same watch filter, but there is no guarantee it wins the
// race against ClearConditions: when the spec-patch reconcile has already
// landed by the time ClearConditions runs, the clear wipes the Ready it
// just set and nothing is left to set it again until the provider's
// background poll tick. The explicit NudgeReconcile call closes that gap.
//
// Clearing BEFORE nudging matters independently of that gap: NudgeReconcile's
// annotation patch can trigger a reconcile that completes (Observe + status
// write) within milliseconds — often faster than this process can issue its
// own next kubectl call. Clearing conditions after the nudge would then have
// a real chance of wiping the fresh Ready condition the nudge just produced,
// forcing WaitReady to fall back on the provider's background poll tick —
// exactly the failure mode this whole sequence exists to avoid. Clearing
// first guarantees the clear has already landed before anything can set a
// new condition, so nothing after it can re-clear a fresh result.
//
// WaitReady alone is not enough to prove the reconcile this call forced has
// actually finished: Observe() marks Ready True on every successful GET,
// independent of whether that SAME pass went on to persist a write
// successfully, so a late-init 409 conflict-and-retry can leave Ready
// already True (left over from an earlier pass) while the reconciler is
// still mid-retry — Synced reads False (ReconcileError) the whole time,
// but WaitReady has no way to see a condition type it never asks about.
// waitSynced closes that gap by also requiring the Synced condition to
// read True AT THE RESOURCE'S CURRENT generation, which is what actually
// proves the pass that ran for THIS generation succeeded rather than one
// that is still retrying, or one that succeeded for a generation already
// superseded by a late-init write made in between.
func (r *Runner) reconcileOnce() error {
	if err := r.ClearConditions(); err != nil {
		return err
	}
	if err := r.NudgeReconcile(); err != nil {
		return err
	}
	if err := r.WaitReady(); err != nil {
		return err
	}
	timeout, _ := time.ParseDuration(r.timeout)
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	synced, status, gen, obsGen, err := r.waitSynced(timeout)
	if err != nil {
		return fmt.Errorf("waiting for Synced: %w", err)
	}
	if !synced {
		return fmt.Errorf("waiting for Synced: timed out after %s — Synced=%q at observedGeneration=%d, resource now at generation %d",
			timeout, status, obsGen, gen)
	}
	return nil
}

// nudgeAndReconcile is reconcileOnce under the name applyPatchAndReconcile
// uses for the second of its two forced reconciles (see that comment for why
// two are needed). The two names exist to make each call site self-
// documenting; the ordering rationale itself lives once, on reconcileOnce
// above, so it cannot drift out of sync between the two callers again.
func (r *Runner) nudgeAndReconcile() error {
	return r.reconcileOnce()
}

// applyEvidenceCheck runs the event-based update-evidence check and updates
// result in place: it always sets UpdateEvidenced, downgrades a value-match
// PASS to NotEvidenced when the aggregated event count never grew (a value
// match without an event is not proof Update() ran — see evidenceOutcome),
// and records a counting error without overwriting one already present.
func (r *Runner) applyEvidenceCheck(result *TestResult, kind, name, namespace, apiVersion, field string, eventsBefore int, eventsBeforeErr error) {
	checked, evidenced, err := r.evidenceOutcome(kind, name, namespace, apiVersion, eventsBefore, eventsBeforeErr)
	result.UpdateEvidenced = evidenced
	if err != nil && result.Error == nil {
		result.Error = err
	}
	if !checked || !result.Passed || evidenced {
		return
	}
	result.Passed = false
	result.NotEvidenced = true
	if result.Error == nil {
		result.Error = fmt.Errorf("update not evidenced: no %s/%s event recorded for %s",
			eventReasonUpdated, eventReasonCannotUpdate, field)
	}
}

// evidenceRetryWindow bounds how long evidenceOutcome retries the
// post-patch event recount before concluding an update was never
// evidenced. The status write WaitReady confirms is synchronous, but the
// matching Event object's visibility to a LIST call is not: the event
// recorder queues and the API server indexes it separately, so there is no
// guarantee the event already exists the instant WaitReady returns. A
// single synchronous recount loses that race often enough to report
// NOT-EVIDENCED on a value that updated correctly. The window stays well
// under the per-field --timeout budget (120s default), so a genuinely
// missing update still fails within a small fraction of it.
const evidenceRetryWindow = 10 * time.Second

// evidenceRetryInterval is how often evidenceOutcome re-lists events while
// inside evidenceRetryWindow.
const evidenceRetryInterval = 2 * time.Second

// evidenceOutcome counts update-related events for (kind, name, namespace,
// apiVersion) and reports whether the aggregated count grew relative to
// eventsBefore — proof that Update() executed, independent of wall-clock
// convergence timing. checked is false when the count could not be
// established (the pre-patch baseline errored, or a post-patch recount
// errored); in that case evidenced is meaningless and err explains what
// went wrong, but the caller should not treat the absence of a count as
// absence of an update.
//
// The post-patch count is retried for up to evidenceRetryWindow rather than
// read once: the event that proves Update() ran may not yet be visible to a
// LIST call the instant this is first called (see evidenceRetryWindow). The
// retry stops the moment the count grows, so an event that is already
// visible resolves exactly as fast as before this window existed — the
// window only ever delays a call that would otherwise have reported a false
// NOT-EVIDENCED.
//
// Every recount is logged to stderr with its attempt number, the observed
// count and the elapsed time since the loop started. A reproduction that
// ends in NOT-EVIDENCED is otherwise a single opaque outcome — there is no
// record of whether the count sat unmoving for the whole window (pointing at
// the involvedObject match itself) or was still climbing when the deadline
// hit (pointing at the window being too short). The log line turns that
// question into something a rerun answers directly instead of by
// reconstructing it from `kubectl get events` timestamps captured after the
// cluster is already gone.
func (r *Runner) evidenceOutcome(kind, name, namespace, apiVersion string, eventsBefore int, eventsBeforeErr error) (checked, evidenced bool, err error) {
	if eventsBeforeErr != nil {
		return false, false, fmt.Errorf("counting update events before patch: %w", eventsBeforeErr)
	}

	window := r.evidenceWindow
	if window <= 0 {
		window = evidenceRetryWindow
	}
	start := time.Now()
	deadline := start.Add(window)
	attempt := 0
	for {
		attempt++
		eventsAfter, afterErr := r.countUpdateEvents(kind, name, namespace, apiVersion)
		if afterErr != nil {
			return false, false, fmt.Errorf("counting update events after patch: %w", afterErr)
		}
		fmt.Fprintf(os.Stderr, "    evidence: %s/%s (ns=%q) attempt %d: count=%d (before=%d), elapsed=%s\n",
			kind, name, namespace, attempt, eventsAfter, eventsBefore, time.Since(start).Round(time.Millisecond))
		if eventsAfter > eventsBefore {
			return true, true, nil
		}
		if time.Now().After(deadline) {
			return true, false, nil
		}
		r.sleep(evidenceRetryInterval)
	}
}

// pollField polls status.atProvider for the given field until it matches
// expectedVal or the runner's timeout elapses, whichever comes first.
//
// ClearConditions + WaitReady ensures the controller has run at least one
// reconcile, but atProvider may still be stale: the controller sets Ready
// after Update() but before the next Observe() cycle writes fresh data to
// atProvider. Polling covers the gap between the first Ready
// re-establishment and the subsequent Observe that actually refreshes
// atProvider.
func (r *Runner) pollField(field string, expectedVal interface{}, start time.Time, ignoreMapKeys, ignoreListElementKeys []string) (string, error) {
	// readRetryInterval is how often THIS TOOL re-reads the resource while
	// waiting. It is unrelated to the provider's poll interval (see
	// Runner.pollInterval and slowObserveThreshold), which is how often the
	// controller re-observes the backend — naming the two alike would invite
	// a later reader to "unify" two independent knobs.
	const readRetryInterval = 5 * time.Second
	timeoutDur, _ := time.ParseDuration(r.timeout)
	if timeoutDur == 0 {
		timeoutDur = 120 * time.Second
	}
	deadline := start.Add(timeoutDur)

	for {
		actual, err := r.ReadField(field)
		if err != nil {
			return actual, err
		}
		if compareFieldValue(expectedVal, actual, ignoreMapKeys, ignoreListElementKeys) {
			return actual, nil
		}
		if time.Now().After(deadline) {
			return actual, nil
		}
		fmt.Fprintf(os.Stderr, "    poll: %s = %q (want %q), retrying in %s...\n",
			field, actual, formatExpected(expectedVal), readRetryInterval)
		time.Sleep(readRetryInterval)
	}
}

// exec runs the configured kubectl binary with args and returns stdout. If
// execFunc is set (tests only), it is used instead of shelling out.
//
// Stderr is captured into a buffer rather than handed to cmd.Stderr =
// os.Stderr directly: os/exec's ExitError only populates its own Stderr
// field when the caller has left cmd.Stderr nil, so claiming the stream for
// live display silently zeroes out the text a failing invocation's wrapped
// error would otherwise carry. Teeing to os.Stderr preserves the live
// display and keeps the captured copy for the returned error.
func (r *Runner) exec(args ...string) (string, error) {
	if r.execFunc != nil {
		return r.execFunc(args)
	}
	// #nosec G204 -- r.kubectl is a controlled config value (default
	// "kubectl", overridable only via the KUBECTL env var), not
	// attacker-controlled input.
	cmd := exec.CommandContext(context.Background(), r.kubectl, args...)
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%w: %s", err, stderr.String())
		}
		return "", err
	}
	return string(out), nil
}

// CheckExternalNamePrefix classifies a live external-name annotation value
// against the prefix a manifest declares it must have (via the
// crossplane.io/expect-external-name-prefix annotation). It is a pure
// function — no kubectl calls — so the negative path (mismatch, empty
// name) is exercised by a plain unit test rather than requiring a live
// cluster or a deliberately broken controller build to prove the check can
// actually fail. The CLI wraps it to check a live resource's ExternalName().
func CheckExternalNamePrefix(name, expectedPrefix string) (ok bool, reason string) {
	if name == "" {
		return false, "external-name annotation is absent or empty"
	}
	if !strings.HasPrefix(name, expectedPrefix) {
		return false, fmt.Sprintf("external-name %q does not have expected prefix %q", name, expectedPrefix)
	}
	return true, ""
}

// buildMergePatch constructs a JSON merge patch for a dot-separated field
// path under spec.forProvider, optionally nulling one or more sibling
// top-level forProvider fields (clear) and/or setting one or more sibling
// top-level forProvider fields to an explicit, non-null literal value
// (withValues) in the SAME patch object — see manifest.UpdateTest.Clear and
// manifest.UpdateTest.WithValues for why a standalone patch of the primary
// field cannot also touch a sibling: a JSON merge patch (RFC 7386) only
// reaches keys the patch object itself names, so a second sequential patch
// would be required, and the two are not atomic together. clear and
// withValues are each validated through manifest.ValidateClear and
// manifest.ValidateWithValues before any patch is built, so a dotted field
// paired with either, a dotted entry in either, a clear/withValues entry
// equal to field itself, or a sibling named in both never reaches the
// map-building below.
func buildMergePatch(field string, value interface{}, clear []string, withValues map[string]interface{}) (string, error) {
	if err := manifest.ValidateClear(field, clear); err != nil {
		return "", err
	}
	if err := manifest.ValidateWithValues(field, withValues, clear); err != nil {
		return "", err
	}

	parts := strings.Split(field, ".")

	// Build the innermost value and wrap outward.
	var inner = value
	for i := len(parts) - 1; i >= 0; i-- {
		inner = map[string]interface{}{parts[i]: inner}
	}

	if len(clear) > 0 || len(withValues) > 0 {
		// ValidateClear/ValidateWithValues already confirmed field has no
		// dot whenever either is non-empty, so inner is exactly the
		// single-iteration map built above — the top-level
		// spec.forProvider object itself, safe to add sibling entries
		// into directly.
		forProvider, ok := inner.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("internal error: field %q produced a non-map patch body", field)
		}
		for _, sibling := range clear {
			forProvider[sibling] = nil
		}
		for sibling, v := range withValues {
			forProvider[sibling] = v
		}
		inner = forProvider
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"forProvider": inner,
		},
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
