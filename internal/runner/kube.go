package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
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

	// WaitForCondition blocks until condition — kubectl wait's own
	// "condition=<type>[=<status>]" syntax, e.g. "condition=Ready" or
	// "condition=Ready=False" — is satisfied or timeout elapses. The
	// condition status defaults to "True" when omitted, exactly as
	// kubectl's own default does.
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

	// ControllerRevisionLabels returns the unique, non-empty
	// providerDeploymentSelector ("pkg.crossplane.io/revision") label
	// values carried by every Pod currently matching selector in
	// namespace, in first-seen order. resolveControllerDeploymentName
	// reduces this to exactly one Deployment name — see
	// providerDeploymentSelector's own doc comment for why a Pod's label
	// is read rather than the Deployment object itself.
	ControllerRevisionLabels(namespace, selector string) ([]string, error)

	// ControllerPodIdentities returns the name and creation time of every
	// Pod currently matching selector in namespace.
	// resolveControllerPodIdentityLive reduces this to whichever entry was
	// created most recently — the Pod actually running now, since an
	// older entry seen during a rollout can only be one mid-termination.
	ControllerPodIdentities(namespace, selector string) ([]controllerPodIdentity, error)

	// RolloutRestart and RolloutStatus back restartControllerDeployment,
	// the mechanism resetEventBurst uses to discard a provider
	// controller's in-process event-spam-filter state. Every provider's
	// own event-burst-size flip has now landed fleet-wide, so production
	// code no longer reaches either method — but they are kept here,
	// deliberately unported, because the smoke-test harness asserts on
	// the exact kubectl argv they issue and drives the compiled binary as
	// a subprocess with no seam to substitute a fake backend in their
	// place. They are retained solely as that harness's subject — no
	// longer a production rollback hatch — and go away only once that
	// harness itself is rewritten to stop depending on an exec-forced
	// kubectl transcript (a fake API server or equivalent).
	RolloutRestart(namespace, target string) (string, error)
	RolloutStatus(namespace, target, timeout string) (string, error)
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

// parseSinceSeconds converts ProviderLogs' since argument — kubectl --since
// syntax, e.g. "30s" — into the whole-second count
// corev1.PodLogOptions.SinceSeconds takes. The sole caller
// (countUpdateLogCalls) always builds this as an integer-second Go duration
// string, so truncating a fractional remainder here never loses precision
// for any caller this project has today.
func parseSinceSeconds(since string) (int64, error) {
	dur, err := time.ParseDuration(since)
	if err != nil {
		return 0, fmt.Errorf("parsing since duration %q: %w", since, err)
	}
	return int64(dur.Seconds()), nil
}

func (c *execKubeClient) RolloutRestart(namespace, target string) (string, error) {
	return c.r.exec("rollout", "restart", target, "-n", namespace)
}

func (c *execKubeClient) RolloutStatus(namespace, target, timeout string) (string, error) {
	return c.r.exec("rollout", "status", target, "-n", namespace, "--timeout="+timeout)
}

// getPodsJSONPath issues `kubectl get pods -n <namespace> -l <selector> -o
// <jsonPath>` — the exec backend's shared argv-building block for
// ControllerRevisionLabels and ControllerPodIdentities below. It is
// deliberately NOT part of the KubeClient interface: the client-go
// backend needs no JSONPath template at all, so promoting this to a full
// interface method (as the pre-port GetPodsJSONPath was) would expose a
// shape only one backend can honour. It survives, unexported, purely as
// this backend's own building block, and is what keeps both methods'
// kubectl argv byte-identical to what this project issued before the
// pod-identity lookup moved off it.
func (c *execKubeClient) getPodsJSONPath(namespace, selector, jsonPath string) (string, error) {
	return c.r.exec("get", "pods", "-n", namespace, "-l", selector, "-o", jsonPath)
}

// ControllerRevisionLabels backs resolveControllerDeploymentName on the
// exec backend: the exact `kubectl get pods -l <selector> -o jsonpath=...`
// invocation this project has always issued for it, reduced client-side by
// uniqueNonEmptyLines to the deduplicated, non-empty label values.
func (c *execKubeClient) ControllerRevisionLabels(namespace, selector string) ([]string, error) {
	out, err := c.getPodsJSONPath(namespace, selector,
		`jsonpath={range .items[*]}{.metadata.labels.pkg\.crossplane\.io/revision}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	return uniqueNonEmptyLines(out), nil
}

// ControllerPodIdentities backs resolveControllerPodIdentityLive on the
// exec backend: the exact `kubectl get pods -l <selector> -o jsonpath=...`
// invocation this project has always issued for it, parsed client-side by
// parseControllerPodIdentities into name/creation-time pairs.
func (c *execKubeClient) ControllerPodIdentities(namespace, selector string) ([]controllerPodIdentity, error) {
	out, err := c.getPodsJSONPath(namespace, selector,
		`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	return parseControllerPodIdentities(out), nil
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

// clientGoKubeClient overrides event listing, resource reads, manifest-name
// resolution, the two merge-patch operations, the Ready wait, the
// controller-log read, and the controller-pod list that backs
// deployment-name resolution and pod-identity reads, with direct client-go
// calls. Only RolloutRestart and RolloutStatus are still promoted from the
// embedded exec backend unchanged — this type exists to migrate one
// operation at a time, not to grow into a second full backend. A shared
// informer or watch cache is deliberately
// NOT used for the event listing, resource read or manifest-name
// resolution overrides: the evidence check those feed is a before/after
// delta on the same object, and a lagging cache would return a stale
// second read and manufacture a false negative — and a manifest resolution
// that consulted a cache could resolve an object as present after its
// deletion had already reached the API server, which is exactly the
// distinction ResolveManifestName exists to get right. The two patch
// overrides write rather than read, so a cache cannot go stale under them,
// but they are still direct dynamic calls for the same reason.
// WaitForCondition is the one exception to "no watch cache reached for
// anywhere in this type": a wait for a future transition is exactly what a
// watch is for, and its own doc comment states the boundary that keeps
// that watch from ever becoming a second, cached source for the direct-read
// overrides above. ProviderLogs opens a fresh log stream per pod on every
// call, exactly like the event/resource-read overrides — nothing here is
// cached either. clientset is a func rather than a stored value so it can
// be resolved lazily (a Runner is often built before a kubeconfig is
// needed) and overridden by tests.
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

// kubeClientQPS and kubeClientBurst raise client-go's client-side rate
// limit above its defaults (rest.DefaultQPS = 5, rest.DefaultBurst = 10).
//
// Those defaults are sized for a controller sharing a production API server
// with many other clients. This tool is a test harness driving a dedicated,
// throwaway E2E control plane on which it is the only significant client,
// and it issues roughly a dozen requests per field test. At 5 QPS a single
// field test spends ~2.4s waiting on tokens: a measured DNSView run (156
// field tests) took 377s at the default against 120s with the limit raised,
// and 256s for the kubectl-per-call exec path this backend replaced. The
// in-process migration made the tool SLOWER until this was set, because
// every `kubectl` fork got a brand-new token bucket and one long-lived
// client does not.
//
// Do NOT express this as "no limit" (a negative QPS, which makes client-go
// skip the limiter entirely). The ceiling is what stops a defect in this
// tool from saturating the API server of the cluster under test and
// surfacing as an unrelated-looking E2E failure somewhere else.
//
// The limiter is per-PROCESS and the post-assert hook runs one process per
// resource, so a parallel drain at -P N raises the aggregate ceiling to
// N*kubeClientQPS. That is the intended behaviour — the value bounds one
// worker, not the run — but it is why the value is deliberately nowhere
// near an API server's own capacity.
const (
	kubeClientQPS   = 50
	kubeClientBurst = 100
)

// kubeRESTConfig resolves the ambient kubeconfig and applies this tool's
// rate limit. Every client-go client in this file is built from it, so the
// limit cannot be set on one client and silently forgotten on another —
// which is exactly how the default slipped through: three separate
// construction sites each resolved their own config and none touched QPS.
func kubeRESTConfig() (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS = kubeClientQPS
	cfg.Burst = kubeClientBurst
	return cfg, nil
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
		cfg, err := kubeRESTConfig()
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
		cfg, err := kubeRESTConfig()
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
		cfg, err := kubeRESTConfig()
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

// resolveManifestGVR resolves gvk — a decoded manifest document's own
// apiVersion/kind, never a kubectl `-o name` string — to a
// GroupVersionResource and reports whether the mapped resource is
// namespace-scoped, via the SAME memoized RESTMapper resourceGVR uses.
// RESTMapping is used here rather than ResourceFor: ResolveManifestName
// starts from a GroupVersionKind, and only RESTMapping's Scope tells a
// namespaced object apart from a cluster-scoped one that happens to share
// a Kind — a distinction manifestDocumentNamespace needs to decide whether
// a document's own (possibly absent) metadata.namespace applies at all.
func (c *clientGoKubeClient) resolveManifestGVR(gvk schema.GroupVersionKind) (gvr schema.GroupVersionResource, namespaced bool, err error) {
	mapper, err := c.r.restMapper()
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("resolving RESTMapper: %w", err)
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("mapping %s to a GroupVersionResource: %w", gvk, err)
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// manifestDocumentNamespace decides which namespace to read a decoded
// manifest document's live object under, mirroring `kubectl get -f
// <manifest>` with no `-n` flag: an explicit caller namespace (kubectl's
// `-n` override) wins when set, then the document's own declared
// metadata.namespace, then "default" — kubectl's own context-default
// fallback for a namespaced object that declares neither. ResolveManifestName's
// sole caller (Runner.ResolveResource) always passes "" here, because it
// runs before the manifest has been resolved at all, so in practice this
// falls through to the document's own namespace (every namespaced example
// this project tests declares one) or, failing that, "default". A
// cluster-scoped object always resolves to "" regardless of what either
// argument carries: the dynamic client's URL builder omits the namespace
// path segment only when this is empty, and a non-empty value for a
// cluster-scoped resource would build a namespaced-shaped request that
// 404s permanently.
func manifestDocumentNamespace(callerNamespace, docNamespace string, namespaced bool) string {
	if !namespaced {
		return ""
	}
	if callerNamespace != "" {
		return callerNamespace
	}
	if docNamespace != "" {
		return docNamespace
	}
	return "default"
}

// manifestResourceIdentifier formats a kubectl `-o name` identifier —
// "<plural-resource>.<group>/<name>" for a non-core resource, bare
// "<plural-resource>/<name>" for the core group — from a resolved GVR and
// object name. This is the exact shape selectResourceName, resourceTypeOf
// and objectNameOf already parse; building it here keeps that contract
// defined in the one place that produces it, rather than left to whichever
// caller assembles it next.
func manifestResourceIdentifier(gvr schema.GroupVersionResource, name string) string {
	if gvr.Group == "" {
		return gvr.Resource + "/" + name
	}
	return gvr.Resource + "." + gvr.Group + "/" + name
}

// ResolveManifestName resolves manifestPath's document(s) to their live
// resource name(s), one line per document in document order, via a direct
// dynamic Get per document — never a YAML decode alone.
//
// `kubectl get -f <manifest>` is a LIVE lookup of every document, not a
// parse of the file. Measured against a real cluster ahead of this
// implementation (a two-document manifest, second object deleted):
// kubectl printed NOTHING on stdout for the whole invocation and exited
// non-zero the moment the second document failed to resolve, even though
// the FIRST document's object still existed — a manifest containing both
// present objects printed both lines, in document order, on a clean exit.
// A decode-and-map implementation that never touches the cluster cannot
// reproduce the failing case: it would print a line for every document
// regardless of whether the object was ever created, changing exactly the
// lines selectResourceName sees, and only on a manifest where some
// document does not exist. This implementation reproduces the measured
// behaviour by construction, not by special-casing it: it Gets each
// document in order and returns the first error immediately, with no
// partial output — there is no separate "accumulate then decide" step for
// a special case to hide in.
func (c *clientGoKubeClient) ResolveManifestName(namespace, manifestPath string) (string, error) {
	// #nosec G304 -- manifestPath is an operator-supplied example manifest
	// path, not attacker-controlled input; the exec backend reads the same
	// path via kubectl.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("reading manifest %s: %w", manifestPath, err)
	}
	dyn, err := c.r.goDynamicClient()
	if err != nil {
		return "", fmt.Errorf("resolving dynamic client: %w", err)
	}

	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var lines []string
	for {
		var doc unstructured.Unstructured
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("parsing manifest %s: %w", manifestPath, err)
		}
		gvk := doc.GroupVersionKind()
		if gvk.Kind == "" {
			// A blank document — a trailing "---" separator, or one with
			// neither apiVersion nor kind — carries nothing to resolve.
			// internal/manifest's own decodeManifestDocs skips exactly
			// this case for the same reason.
			continue
		}
		gvr, namespaced, err := c.resolveManifestGVR(gvk)
		if err != nil {
			return "", fmt.Errorf("resolving document %d (%s) from manifest %s: %w", len(lines)+1, gvk.Kind, manifestPath, err)
		}
		ns := manifestDocumentNamespace(namespace, doc.GetNamespace(), namespaced)
		if _, err := dyn.Resource(gvr).Namespace(ns).Get(context.Background(), doc.GetName(), metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("resolving %s %q from manifest %s via client-go: %w", gvk.Kind, doc.GetName(), manifestPath, err)
		}
		lines = append(lines, manifestResourceIdentifier(gvr, doc.GetName()))
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("manifest %s contains no Kubernetes documents", manifestPath)
	}
	return strings.Join(lines, "\n") + "\n", nil
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

// parseWaitCondition parses WaitForCondition's condition argument in
// kubectl wait's own "condition=<type>[=<status>]" syntax, so an
// implementation that ignores everything but "Ready" cannot silently pass
// every existing caller while breaking the first one that names a
// different condition. The status half defaults to "True" — kubectl's own
// default when it is omitted — and is returned verbatim rather than
// normalised: the case-insensitive comparison kubectl itself performs
// happens once, in waitConditionMet, not here.
func parseWaitCondition(condition string) (conditionType, conditionStatus string, err error) {
	const prefix = "condition="
	if !strings.HasPrefix(condition, prefix) {
		return "", "", fmt.Errorf("parsing wait condition %q: expected the %q prefix", condition, prefix)
	}
	rest := strings.TrimPrefix(condition, prefix)
	conditionType, conditionStatus = rest, "True"
	if idx := strings.Index(rest, "="); idx != -1 {
		conditionType, conditionStatus = rest[:idx], rest[idx+1:]
	}
	if conditionType == "" {
		return "", "", fmt.Errorf("parsing wait condition %q: empty condition type", condition)
	}
	return conditionType, conditionStatus, nil
}

// waitConditionMet reports whether obj carries a status.conditions entry
// whose type and status match conditionType/conditionStatus, compared
// case-insensitively — the same "Unicode simple case folding" comparison
// kubectl wait documents for its own --for=condition matching.
func waitConditionMet(obj *unstructured.Unstructured, conditionType, conditionStatus string) (bool, error) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Errorf("reading status.conditions: %w", err)
	}
	if !found {
		return false, nil
	}
	for _, raw := range conditions {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		entryType, _, _ := unstructured.NestedString(entry, "type")
		if !strings.EqualFold(entryType, conditionType) {
			continue
		}
		entryStatus, _, _ := unstructured.NestedString(entry, "status")
		return strings.EqualFold(entryStatus, conditionStatus), nil
	}
	return false, nil
}

// WaitForCondition blocks until name's status.conditions satisfies
// condition or timeout elapses, using k8s.io/client-go/tools/watch's
// UntilWithSync over a ListWatch scoped to this one object by a
// metadata.name field selector — the SAME primitive kubectl wait's own
// implementation calls internally (k8s.io/kubectl/pkg/cmd/wait's
// getObjAndCheckCondition), built the same way: a List-then-Watch
// ListerWatcher, a store-existence precondition, and a ConditionFunc
// evaluated over the resulting event stream.
//
// This is what makes the already-satisfied case correct without a special
// case for it: UntilWithSync builds an informer, waits for that informer's
// initial List to land in its store, and only then evaluates precondition
// — but the informer's own event handler synthesizes an Added event for
// every object the initial List already contained (see client-go's
// NewIndexerInformerWatcher), so an object that reached the condition
// before this call ever ran satisfies it on that synthetic event, through
// the exact same ConditionFunc a genuine later transition would satisfy.
// The watch itself is seeded from that same informer's reflector, which
// re-lists from the resourceVersion its own initial List observed, so a
// transition landing between the List and the Watch opening is a normal
// watch event rather than a missed one — the guarantee this migration
// needs, provided by the informer rather than hand-built here.
//
// precondition mirrors kubectl's own: once the store has synced, an empty
// store means the object does not exist, and that is reported as an
// immediate, non-nil error rather than a block until timeout — matching
// kubectl wait's own fail-fast behaviour for a resource that is absent.
//
// The Watch this opens is never a source for anything but this wait:
// ListEventsForObject and GetObjectJSON, this type's other two read
// overrides, still take direct, uncached calls — see clientGoKubeClient's
// own doc comment for that boundary.
func (c *clientGoKubeClient) WaitForCondition(namespace, name, condition, timeout string) (string, error) {
	conditionType, conditionStatus, err := parseWaitCondition(condition)
	if err != nil {
		return "", err
	}
	dur, err := time.ParseDuration(timeout)
	if err != nil {
		return "", fmt.Errorf("parsing wait timeout %q: %w", timeout, err)
	}
	gvr, err := c.resourceGVR(name)
	if err != nil {
		return "", err
	}
	dyn, err := c.r.goDynamicClient()
	if err != nil {
		return "", fmt.Errorf("resolving dynamic client: %w", err)
	}
	objName := objectNameOf(name)
	res := dyn.Resource(gvr).Namespace(namespace)

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	fieldSelector := fields.OneTermEqualSelector("metadata.name", objName).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = fieldSelector
			return res.List(ctx, opts)
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = fieldSelector
			return res.Watch(ctx, opts)
		},
	}

	precondition := func(store cache.Store) (bool, error) {
		if len(store.List()) == 0 {
			return true, apierrors.NewNotFound(gvr.GroupResource(), objName)
		}
		return false, nil
	}

	condFn := func(event watch.Event) (bool, error) {
		switch event.Type {
		case watch.Error:
			// Matches kubectl wait's own ConditionalWait.isConditionMet:
			// keep waiting rather than fail. The server is expected to
			// close the watch immediately after an Error event, and
			// UntilWithSync recovers by relisting.
			return false, nil
		case watch.Deleted:
			// Also matches kubectl: a delete does not itself satisfy or
			// fail the wait. It chains back to a relist, which the
			// precondition above turns into a NotFound once the store is
			// actually empty.
			return false, nil
		}
		obj, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			return false, nil
		}
		return waitConditionMet(obj, conditionType, conditionStatus)
	}

	event, err := watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, precondition, watchtools.ConditionFunc(condFn))
	if err != nil {
		if wait.Interrupted(err) {
			return "", fmt.Errorf("timed out after %s waiting for %s on %s: %w", timeout, condition, name, err)
		}
		return "", fmt.Errorf("waiting for %s on %s via client-go: %w", condition, name, err)
	}
	if event == nil || event.Object == nil {
		return "", fmt.Errorf("waiting for %s on %s via client-go: condition function returned no object", condition, name)
	}
	data, err := json.Marshal(event.Object)
	if err != nil {
		return "", fmt.Errorf("marshalling client-go wait result: %w", err)
	}
	return string(data), nil
}

// ProviderLogs reads controller log lines for every pod selector matches in
// namespace, covering the last `since` duration, via a direct client-go log
// stream per pod — never a shared cache, and never a `--tail` limit.
// PodLogOptions.TailLines is left nil unconditionally: kubectl defaults
// --tail to 10 whenever a selector is used, and countUpdateLogCalls's own
// doc comment records what that default did once measured live — loop
// detection firing in every window collapsed to firing in two windows out
// of three, silently and with a zero exit. Leaving TailLines nil is what
// "--tail=-1" means, and is the one thing this method must never change.
//
// Container selection mirrors the exec backend's own invocation (a bare
// `kubectl logs -l <selector>`, no `--container` flag): the pod's FIRST
// container is read, matching kubectl's own default-container behaviour —
// kubectl warns to stderr and proceeds for a multi-container pod rather
// than refusing, so this does too, with nothing to warn to since no caller
// reads this backend's stderr.
//
// A pod whose log stream cannot be opened or fully read is recorded and
// skipped rather than aborting the whole call, so one unreadable pod never
// costs the others their lines: kubectl's own sequential log consumption
// already streams each matched pod's content to stdout as it goes, and it
// is only the exec backend's os/exec wrapper — which discards a command's
// entire stdout the moment its exit code is non-zero — that turns one
// failing pod into zero bytes of evidence for every pod. This backend reads
// each pod's stream directly and has no reason to inherit that limitation.
// Only when EVERY matched pod failed is the call itself reported as an
// error, matching the one outcome the exec backend can still produce today:
// nothing observed, nothing to attribute a call count to.
func (c *clientGoKubeClient) ProviderLogs(namespace, selector, since string) (string, error) {
	sinceSeconds, err := parseSinceSeconds(since)
	if err != nil {
		return "", err
	}
	cs, err := c.clientset()
	if err != nil {
		return "", fmt.Errorf("resolving client-go client: %w", err)
	}
	pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("listing pods for %q via client-go: %w", selector, err)
	}

	var out strings.Builder
	var failures []error
	for _, pod := range pods.Items {
		if err := c.appendPodLog(&out, cs, namespace, pod, sinceSeconds); err != nil {
			failures = append(failures, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
	}
	if len(pods.Items) > 0 && len(failures) == len(pods.Items) {
		return "", fmt.Errorf("reading controller log via client-go: every matched pod failed: %w", errors.Join(failures...))
	}
	return out.String(), nil
}

// appendPodLog opens and fully reads one pod's log stream and writes its
// content to out. The stream is closed on every exit path — including a
// Read that fails partway through — so a mid-stream error on one pod can
// never leak its connection into the next pod's read.
func (c *clientGoKubeClient) appendPodLog(out *strings.Builder, cs kubernetes.Interface, namespace string, pod corev1.Pod, sinceSeconds int64) error {
	container, err := firstContainerName(pod)
	if err != nil {
		return err
	}
	stream, err := c.r.podLogStream(cs, namespace, pod.Name, container, sinceSeconds)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}
	defer stream.Close() //nolint:errcheck // the read below is what determines success; a close error on an already-fully-read stream carries nothing actionable
	data, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("reading log stream: %w", err)
	}
	out.Write(data)
	return nil
}

// firstContainerName returns pod's first container name — the same
// container kubectl logs selects by default when no --container flag is
// given, whether the pod carries one container or several.
func firstContainerName(pod corev1.Pod) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s carries no containers", pod.Name)
	}
	return pod.Spec.Containers[0].Name, nil
}

// podLogStream opens one pod/container's log stream — the mechanism
// ProviderLogs' client-go backend reads through. podLogStreamFunc, when
// set, overrides this for tests: the built-in fake Clientset's GetLogs
// always returns identical canned content with a 200 status for every pod
// and cannot express either distinct per-pod content or a mid-stream
// failure, so a test proving either needs this seam instead.
func (r *Runner) podLogStream(cs kubernetes.Interface, namespace, podName, container string, sinceSeconds int64) (io.ReadCloser, error) {
	if r.podLogStreamFunc != nil {
		return r.podLogStreamFunc(namespace, podName, container, sinceSeconds)
	}
	opts := &corev1.PodLogOptions{Container: container, SinceSeconds: &sinceSeconds}
	return cs.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(context.Background())
}

// listControllerPods lists Pods matching selector in namespace via a
// direct client-go call — the shared building block behind
// ControllerRevisionLabels and ControllerPodIdentities on this backend.
// Unlike the exec backend's getPodsJSONPath, no JSONPath template is
// built or parsed on this side at all: each override below reads the
// field(s) it needs directly off the typed corev1.Pod list this returns.
func (c *clientGoKubeClient) listControllerPods(namespace, selector string) ([]corev1.Pod, error) {
	cs, err := c.clientset()
	if err != nil {
		return nil, fmt.Errorf("resolving client-go client: %w", err)
	}
	pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing pods for %q via client-go: %w", selector, err)
	}
	return pods.Items, nil
}

// ControllerRevisionLabels is listControllerPods narrowed to the field
// resolveControllerDeploymentName needs: the unique, non-empty
// providerDeploymentSelector label values, in first-seen order — the same
// reduction the exec backend's jsonpath template performs, done here as a
// plain Go map read instead.
func (c *clientGoKubeClient) ControllerRevisionLabels(namespace, selector string) ([]string, error) {
	pods, err := c.listControllerPods(namespace, selector)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var labels []string
	for _, pod := range pods {
		v := pod.Labels[providerDeploymentSelector]
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		labels = append(labels, v)
	}
	return labels, nil
}

// ControllerPodIdentities is listControllerPods narrowed to the fields
// resolveControllerPodIdentityLive needs: each matching Pod's own name and
// creation time, read directly off its metadata rather than parsed out of
// a jsonpath-formatted string.
func (c *clientGoKubeClient) ControllerPodIdentities(namespace, selector string) ([]controllerPodIdentity, error) {
	pods, err := c.listControllerPods(namespace, selector)
	if err != nil {
		return nil, err
	}
	identities := make([]controllerPodIdentity, 0, len(pods))
	for _, pod := range pods {
		identities = append(identities, controllerPodIdentity{Name: pod.Name, CreatedAt: pod.CreationTimestamp.Time})
	}
	return identities, nil
}

// kube returns the KubeClient backend for this Runner. It is a method
// rather than a field populated at construction so a Runner built as a bare
// struct literal resolves to a working backend with no separate wiring
// step. Selection order:
//
//  1. kubeBackendEnvVar set to "exec" forces the exec backend for every
//     operation — the differential diagnostic that separates "the tool
//     changed" from "the provider changed" on a run whose client-go path
//     misbehaves in a way this migration's own runs did not cover.
//  2. Otherwise: the client-go backend for event listing, resource reads,
//     manifest-name resolution, the two merge-patch operations, the Ready
//     wait, the controller-log read, and the controller-pod list that
//     backs deployment-name resolution and pod-identity reads (the
//     default, production behaviour), built from
//     kubeClientsetFunc/kubeDynamicFunc/restMapperFunc when a test has set
//     them, or from the ambient kubeconfig otherwise. Only
//     RolloutRestart/RolloutStatus still delegate to the embedded exec
//     backend unconditionally — see their own doc comment on the
//     KubeClient interface for why they are not part of this default.
//
// A test Runner built as a bare struct literal takes exactly this same
// path: there is no longer a separate branch for "a test harness with no
// client-go override", so every test either wires kubeClientsetFunc/
// kubeDynamicFunc/restMapperFunc/podLogStreamFunc — even if only to a fake
// client-go client that itself delegates to a hand-rolled simulation — or
// forces kubeBackendEnvVar to exercise the exec backend deliberately.
//
// Whichever branch is taken, the choice is recorded to stderr exactly once
// per Runner (see kubeBackendLogOnce) — a parity run comparing the exec and
// client-go backends is otherwise unverifiable from its own output: a run
// where the env var never reached the process produces artifacts
// indistinguishable from one where it did.
func (r *Runner) kube() KubeClient {
	exec := &execKubeClient{r: r}
	forcedExec := os.Getenv(kubeBackendEnvVar) == "exec"
	r.kubeBackendLogOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "    "+kubeBackendSelectionLine(forcedExec))
	})
	if forcedExec {
		return exec
	}
	return &clientGoKubeClient{execKubeClient: exec, clientset: r.goClientset}
}

// kubeBackendSelectionLine formats the one-line record kube() emits, naming
// which operation(s) each backend serves and whether kubeBackendEnvVar
// forced the choice. It is a standalone function, rather than inlined into
// kube(), so a unit test can assert on its output directly instead of
// parsing captured stderr text out of a live Runner.
func kubeBackendSelectionLine(forcedExec bool) string {
	if forcedExec {
		return fmt.Sprintf("kube backend: exec (all operations — forced by %s=exec)", kubeBackendEnvVar)
	}
	return "kube backend: client-go (event list, resource read, manifest resolve, patch, wait, log, controller pod list), exec (rollout restart/status only)"
}
