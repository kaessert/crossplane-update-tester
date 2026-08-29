package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeClient is the typed seam over every kubectl operation Runner needs.
// It exists so a future backend (e.g. an in-process client-go client) can
// replace how these operations are carried out without touching any of the
// call sites that use them — only the backend construction changes.
//
// Each method takes the resource's namespace explicitly rather than reading
// it off a shared field, because the backend has no notion of "the current
// Runner" to read it from.
type KubeClient interface {
	// GetObjectJSON returns the full resource, rendered as JSON. name is a
	// kubectl `-o name` identifier — "<plural-resource>[.<group>]/<name>",
	// e.g. "dnsviews.dns.infoblox.crossplane.io/example" for a custom
	// resource or "secrets/example" for a core-group one — never a bare
	// object name.
	GetObjectJSON(namespace, name string) (string, error)

	// ResolveManifestName resolves a manifest FILE to the resource name(s)
	// it declares, one line per document in the manifest. This is
	// distinct from GetObjectJSON: it takes a file path rather than an
	// already-known object name, and a manifest routinely ships more than
	// one document (a companion Secret or ProviderConfig alongside the
	// managed resource under test) — the caller, not this method, picks
	// the line that names the resource under test.
	ResolveManifestName(namespace, manifestPath string) (string, error)

	// PatchMerge applies a JSON merge patch to a resource's main body.
	PatchMerge(namespace, name, patchJSON string) (string, error)

	// PatchMergeStatus applies a JSON merge patch to a resource's status
	// subresource.
	PatchMergeStatus(namespace, name, patchJSON string) (string, error)

	// WaitForCondition blocks until condition (e.g. "condition=Ready") is
	// satisfied or timeout elapses.
	WaitForCondition(namespace, name, condition, timeout string) (string, error)

	// ListEventsForObject returns cluster Events whose involvedObject
	// matches kind, name and namespace, rendered as JSON in the same
	// {"items": [...]} shape a `kubectl get events -o json` response
	// carries. namespace is the TARGET resource's own namespace, not any
	// Event's — pass "" for a cluster-scoped resource, which must match
	// only Events whose involvedObject itself carries no namespace, never
	// "any namespace". A backend narrows the query server-side with a
	// field selector on all three, but callers still verify namespace and
	// apiVersion of every returned Item client-side (see
	// sumEventOccurrencesByReason) — not every event source populates
	// apiVersion, and that re-check is what keeps a cluster-scoped
	// resource from matching a namespaced sibling that shares its Kind and
	// Name even if a backend's server-side narrowing were ever imperfect.
	ListEventsForObject(kind, name, namespace string) (string, error)

	// ProviderLogs returns controller log lines for the pods matched by
	// selector in namespace, covering the last `since` duration (kubectl
	// --since syntax, e.g. "30s").
	ProviderLogs(namespace, selector, since string) (string, error)

	// RolloutRestart, RolloutStatus and GetPodsJSONPath back the
	// event-burst-reset workaround (see restartControllerDeployment and
	// resolveControllerDeploymentName). They are expected to be deleted,
	// not ported to a future client-go backend, once the fleet-wide
	// event-burst-size flip lands everywhere and this workaround is no
	// longer needed — tickets IN-EVENT-BURST-ON (provider-infobloxnios)
	// and FLEET-EVENT-BURST-ON (the remaining providers).
	RolloutRestart(namespace, target string) (string, error)
	RolloutStatus(namespace, target, timeout string) (string, error)
	GetPodsJSONPath(namespace, selector, jsonPath string) (string, error)
}

// execKubeClient is the KubeClient backend that shells out to kubectl. Every
// method builds exactly the argv the pre-seam code built inline at its call
// site and hands it to Runner.exec, which is what actually invokes kubectl
// (or, in tests, the injected execFunc) — so this backend is byte-for-byte
// behaviour-identical to the code it replaces, by construction rather than
// by convention. It is the only KubeClient implementation today.
type execKubeClient struct {
	r *Runner
}

// scoped appends "-n <namespace>" when namespace is non-empty, mirroring
// the namespace-scoping the pre-seam Runner.run helper used to apply.
func (c *execKubeClient) scoped(namespace string, args []string) (string, error) {
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return c.r.exec(args...)
}

func (c *execKubeClient) GetObjectJSON(namespace, name string) (string, error) {
	return c.scoped(namespace, []string{"get", name, "-o", "json"})
}

func (c *execKubeClient) ResolveManifestName(namespace, manifestPath string) (string, error) {
	return c.scoped(namespace, []string{"get", "-f", manifestPath, "-o", "name"})
}

func (c *execKubeClient) PatchMerge(namespace, name, patchJSON string) (string, error) {
	return c.scoped(namespace, []string{"patch", name, "--type=merge", "-p", patchJSON})
}

func (c *execKubeClient) PatchMergeStatus(namespace, name, patchJSON string) (string, error) {
	return c.scoped(namespace, []string{"patch", name, "--subresource=status", "--type=merge", "-p", patchJSON})
}

func (c *execKubeClient) WaitForCondition(namespace, name, condition, timeout string) (string, error) {
	return c.scoped(namespace, []string{"wait", name, "--for=" + condition, "--timeout=" + timeout})
}

func (c *execKubeClient) ListEventsForObject(kind, name, namespace string) (string, error) {
	return c.r.exec("get", "events", "--all-namespaces", "-o", "json",
		"--field-selector", eventFieldSelector(kind, name, namespace))
}

func (c *execKubeClient) ProviderLogs(namespace, selector, since string) (string, error) {
	return c.r.exec("logs", "-n", namespace, "-l", selector, "--tail=-1", "--since="+since)
}

func (c *execKubeClient) RolloutRestart(namespace, target string) (string, error) {
	return c.r.exec("rollout", "restart", target, "-n", namespace)
}

func (c *execKubeClient) RolloutStatus(namespace, target, timeout string) (string, error) {
	return c.r.exec("rollout", "status", target, "-n", namespace, "--timeout="+timeout)
}

func (c *execKubeClient) GetPodsJSONPath(namespace, selector, jsonPath string) (string, error) {
	return c.r.exec("get", "pods", "-n", namespace, "-l", selector, "-o", jsonPath)
}

// eventFieldSelector builds the involvedObject field selector every
// KubeClient backend uses to narrow an events List server-side, shared so
// the exec and client-go backends build byte-identical selector strings.
// namespace is included unconditionally, even when empty, so a
// cluster-scoped resource's selector explicitly requires an empty
// involvedObject.namespace rather than omitting the term and matching any
// namespace. fields.Set.String() (not Set.AsSelector().String()) is used
// deliberately: it sorts its terms before joining them, so the returned
// string is deterministic across calls — AsSelector's term order comes from
// a map iteration and is not.
func eventFieldSelector(kind, name, namespace string) string {
	return fields.Set{
		"involvedObject.kind":      kind,
		"involvedObject.name":      name,
		"involvedObject.namespace": namespace,
	}.String()
}

// kubeBackendEnvVar selects which backend serves the operations that have
// been migrated to client-go so far. "exec" forces every KubeClient
// operation, migrated ones included, onto the exec backend — the rollback
// path if the client-go path misbehaves on a provider this migration's own
// live run did not cover. Any other value, including unset, routes the
// migrated operations through the client-go backend and leaves every other
// operation on the exec backend. kubeBackendSelectionLine below is the
// single authoritative statement of which operations that is; this comment
// deliberately does not repeat the list, because a second copy goes stale
// on the next slice of the migration and has already done so once.
const kubeBackendEnvVar = "UPDATE_TESTER_KUBE_BACKEND"

// clientGoKubeClient overrides event listing, resource reads and the two
// merge-patch operations with direct client-go calls. Every other KubeClient
// operation is promoted from the embedded exec backend unchanged — this
// type exists to migrate one operation at a time, not to grow into a second
// full backend. A shared informer or watch cache is deliberately NOT used
// for any override: the evidence check event listing and resource reads
// feed is a before/after delta on the same object, and a lagging cache
// would return a stale second read and manufacture a false negative: the
// two patch overrides write rather than read, so a cache cannot go stale
// under them, but they are still direct dynamic calls for the same reason
// — no informer or watch cache is reached for anywhere in this type.
// clientset is a func rather than a stored value so it can be resolved
// lazily (a Runner is often built before a kubeconfig is needed) and
// overridden by tests.
type clientGoKubeClient struct {
	*execKubeClient
	clientset func() (kubernetes.Interface, error)
}

func (c *clientGoKubeClient) ListEventsForObject(kind, name, namespace string) (string, error) {
	cs, err := c.clientset()
	if err != nil {
		return "", fmt.Errorf("resolving client-go client: %w", err)
	}
	list, err := cs.CoreV1().Events(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		FieldSelector: eventFieldSelector(kind, name, namespace),
	})
	if err != nil {
		return "", fmt.Errorf("listing events via client-go: %w", err)
	}
	// corev1.EventList's JSON tags (items[].reason, items[].count,
	// items[].involvedObject.{kind,name,namespace,apiVersion}) already
	// match eventList/eventItem's — marshalling straight back to JSON
	// keeps every downstream parser and the exec backend's return shape
	// identical, so sumEventOccurrencesByReason needs no backend-specific
	// branch.
	data, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("marshalling client-go event list: %w", err)
	}
	return string(data), nil
}

// goClientset returns the client-go Clientset the client-go KubeClient
// backend uses, built once from the ambient kubeconfig (the KUBECONFIG env
// var, falling back to ~/.kube/config — the same resolution kubectl itself
// applies) and cached for the lifetime of this Runner. kubeClientsetFunc,
// when set, overrides this for tests, exactly like execFunc overrides
// exec().
func (r *Runner) goClientset() (kubernetes.Interface, error) {
	if r.kubeClientsetFunc != nil {
		return r.kubeClientsetFunc()
	}
	r.kubeClientsetOnce.Do(func() {
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			r.kubeClientsetErr = fmt.Errorf("resolving kubeconfig: %w", err)
			return
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			r.kubeClientsetErr = fmt.Errorf("building client-go clientset: %w", err)
			return
		}
		r.kubeClientset = cs
	})
	return r.kubeClientset, r.kubeClientsetErr
}

// goDynamicClient returns the dynamic client the client-go KubeClient
// backend uses to read objects with no compiled-in Go type — every
// provider CR this tool reads is one of these. Built from the same ambient
// kubeconfig resolution goClientset uses. kubeDynamicFunc, when set,
// overrides this for tests, exactly like kubeClientsetFunc overrides
// goClientset: constructing a dynamic client carries no discovery cost of
// its own, so — unlike restMapper below — there is nothing to prove
// memoized here, and the override bypasses the Once the same way
// kubeClientsetFunc does.
func (r *Runner) goDynamicClient() (dynamic.Interface, error) {
	if r.kubeDynamicFunc != nil {
		return r.kubeDynamicFunc()
	}
	r.kubeDynamicOnce.Do(func() {
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			r.kubeDynamicErr = fmt.Errorf("resolving kubeconfig: %w", err)
			return
		}
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			r.kubeDynamicErr = fmt.Errorf("building dynamic client: %w", err)
			return
		}
		r.kubeDynamic = dyn
	})
	return r.kubeDynamic, r.kubeDynamicErr
}

// restMapper returns the discovery-backed RESTMapper the client-go
// KubeClient backend uses to resolve a kubectl `-o name` resource/group
// pair into a full GroupVersionResource, built at most once per Runner.
//
// Unlike goClientset/goDynamicClient, restMapperFunc (when set) is called
// FROM INSIDE restMapperOnce rather than bypassing it: a discovery round
// trip — not just a kubeconfig read — is the cost this memoization exists
// to remove, so the test proving "resolved at most once" needs the override
// itself gated by the same Once production code runs through, or the test
// would only prove the override function is cheap to call, never that a
// second logical call is actually skipped.
func (r *Runner) restMapper() (meta.RESTMapper, error) {
	r.restMapperOnce.Do(func() {
		if r.restMapperFunc != nil {
			r.kubeRESTMapper, r.kubeRESTMapperErr = r.restMapperFunc()
			return
		}
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			r.kubeRESTMapperErr = fmt.Errorf("resolving kubeconfig for discovery: %w", err)
			return
		}
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			r.kubeRESTMapperErr = fmt.Errorf("building discovery client: %w", err)
			return
		}
		r.kubeRESTMapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	})
	return r.kubeRESTMapper, r.kubeRESTMapperErr
}

// resourceGVR resolves a kubectl `-o name` identifier's TYPE segment —
// "<plural-resource>.<group>" for a custom resource, or a bare
// "<plural-resource>" for a core-group one — into a full
// GroupVersionResource via the memoized RESTMapper. The type segment never
// carries a Version, exactly like `kubectl get <type>/<name>` never names
// one either: ResourceFor resolves the served version from discovery the
// same way kubectl's own discovery-backed resolution does, which is
// correct for a custom resource served by an unaggregated CRD — its
// resource/group pair maps to exactly one registered GroupVersionResource,
// with no typed Go client involved anywhere in the resolution.
func (c *clientGoKubeClient) resourceGVR(resourceName string) (schema.GroupVersionResource, error) {
	resourceType := resourceTypeOf(resourceName)
	if resourceType == "" {
		return schema.GroupVersionResource{}, fmt.Errorf(
			"resolving %q: no type segment (expected <resource>[.<group>]/<name>)", resourceName)
	}
	resource, group := resourceType, ""
	if idx := strings.Index(resourceType, "."); idx != -1 {
		resource, group = resourceType[:idx], resourceType[idx+1:]
	}
	mapper, err := c.r.restMapper()
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("resolving RESTMapper: %w", err)
	}
	gvr, err := mapper.ResourceFor(schema.GroupVersionResource{Group: group, Resource: resource})
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("mapping %q to a GroupVersionResource: %w", resourceType, err)
	}
	return gvr, nil
}

// GetObjectJSON reads name (a kubectl `-o name` identifier) with a direct
// dynamic Get — never an informer, a watch cache, or any other client that
// could serve a stale read — and marshals the result straight back to
// JSON. unstructured.Unstructured's JSON encoding is exactly the object's
// own apiVersion/kind/metadata/spec/status shape, the same shape
// GetObject's json.Unmarshal already expects from the exec backend, so no
// caller-visible contract changes. A not-found read or any other API error
// is returned as a non-nil error, unwrapped into no special case: that is
// exactly how a kubectl non-zero exit reaches this method's caller today,
// and every existing caller (GetObject, and everything built on it) already
// treats a non-nil error as failure without inspecting its text.
func (c *clientGoKubeClient) GetObjectJSON(namespace, name string) (string, error) {
	gvr, err := c.resourceGVR(name)
	if err != nil {
		return "", err
	}
	dyn, err := c.r.goDynamicClient()
	if err != nil {
		return "", fmt.Errorf("resolving dynamic client: %w", err)
	}
	obj, err := dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), objectNameOf(name), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting %s via client-go: %w", name, err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshalling client-go object: %w", err)
	}
	return string(data), nil
}

// PatchMerge applies a JSON merge patch to a resource's main body via a
// direct dynamic Patch — RFC 7386 semantics and nothing else.
// types.MergePatchType is used explicitly: StrategicMergePatchType is not
// supported by custom resources at all, and JSONPatchType would interpret
// patchJSON as an operation array rather than a merge document, silently
// misreading every caller's patch. patchJSON reaches the API server exactly
// as the caller built it — no unmarshal into a Go map and re-marshal — which
// is the one thing this method must get right: a map round trip is where an
// explicit `null` (a removal) and an absent key (a no-op) collapse into the
// same Go zero value, and every clear/withValues patch this tool builds
// depends on that distinction surviving. A rejected patch (e.g. an HTTP 400)
// is returned as a non-nil error, unwrapped into no special case — exactly
// how a rejected kubectl patch's non-zero exit reaches every caller today.
func (c *clientGoKubeClient) PatchMerge(namespace, name, patchJSON string) (string, error) {
	return c.dynamicMergePatch(namespace, name, patchJSON)
}

// PatchMergeStatus is PatchMerge's status-subresource counterpart: the same
// RFC 7386 merge-patch semantics, with "status" passed as the dynamic
// client's trailing subresource argument so the patch lands on the STATUS
// subresource rather than the main body. Omitting that argument is the one
// mistake this method exists to prevent: the API server would accept
// {"status":{"conditions":[]}} against the main body and silently do
// nothing there, so ClearConditions — the sole caller — would return
// success while never actually clearing a condition, and WaitReady would
// then return immediately instead of blocking on the controller
// re-establishing Ready.
func (c *clientGoKubeClient) PatchMergeStatus(namespace, name, patchJSON string) (string, error) {
	return c.dynamicMergePatch(namespace, name, patchJSON, "status")
}

// dynamicMergePatch is PatchMerge and PatchMergeStatus's shared
// implementation: resolve name's GVR via the memoized RESTMapper, then issue
// a direct dynamic Patch with patchJSON's bytes passed through untouched.
// subresources is empty for the main body and "status" for the status
// subresource — the same dynamic.ResourceInterface.Patch signature both
// callers share, differing only in that one trailing argument.
func (c *clientGoKubeClient) dynamicMergePatch(namespace, name, patchJSON string, subresources ...string) (string, error) {
	gvr, err := c.resourceGVR(name)
	if err != nil {
		return "", err
	}
	dyn, err := c.r.goDynamicClient()
	if err != nil {
		return "", fmt.Errorf("resolving dynamic client: %w", err)
	}
	obj, err := dyn.Resource(gvr).Namespace(namespace).Patch(context.Background(), objectNameOf(name),
		types.MergePatchType, []byte(patchJSON), metav1.PatchOptions{}, subresources...)
	if err != nil {
		return "", fmt.Errorf("patching %s via client-go: %w", name, err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshalling client-go patch result: %w", err)
	}
	return string(data), nil
}

// kube returns the KubeClient backend for this Runner. It is a method
// rather than a field populated at construction so a Runner built as a bare
// struct literal — every existing test constructs one this way, setting
// execFunc directly with no knowledge of KubeClient — still resolves to a
// working backend. Selection order:
//
//  1. kubeBackendEnvVar set to "exec" forces the exec backend for every
//     operation.
//  2. execFunc set with no kubeClientsetFunc override: this is a test
//     Runner exercising behaviour through an injected exec fake that
//     predates this migration. Routing it through a real client-go client
//     would silently bypass that fake — no kubeconfig exists under test —
//     rather than exercising it, so it stays on the exec backend it was
//     already wired for.
//  3. Otherwise: the client-go backend for event listing, resource reads
//     and the two merge-patch operations (the default, production
//     behaviour), built from kubeClientsetFunc/kubeDynamicFunc/restMapperFunc
//     when a test has set them, or from the ambient kubeconfig otherwise.
//     Every other operation still delegates to the embedded exec backend.
//
// Whichever branch is taken, the choice is recorded to stderr exactly once
// per Runner (see kubeBackendLogOnce) — a parity run comparing the exec and
// client-go backends is otherwise unverifiable from its own output: a run
// where the env var never reached the process produces artifacts
// indistinguishable from one where it did.
func (r *Runner) kube() KubeClient {
	exec := &execKubeClient{r: r}
	forcedExec := os.Getenv(kubeBackendEnvVar) == "exec"
	execFallback := !forcedExec && r.execFunc != nil && r.kubeClientsetFunc == nil
	r.kubeBackendLogOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "    "+kubeBackendSelectionLine(forcedExec, execFallback))
	})
	if forcedExec || execFallback {
		return exec
	}
	return &clientGoKubeClient{execKubeClient: exec, clientset: r.goClientset}
}

// kubeBackendSelectionLine formats the one-line record kube() emits, naming
// which operation(s) each backend serves and whether kubeBackendEnvVar
// forced the choice. It is a standalone function, rather than inlined into
// kube(), so a unit test can assert on its output directly instead of
// parsing captured stderr text out of a live Runner.
func kubeBackendSelectionLine(forcedExec, execFallback bool) string {
	switch {
	case forcedExec:
		return fmt.Sprintf("kube backend: exec (all operations — forced by %s=exec)", kubeBackendEnvVar)
	case execFallback:
		return "kube backend: exec (all operations — test harness has no client-go override)"
	default:
		return "kube backend: client-go (event list, resource read, patch), exec (all other operations)"
	}
}
