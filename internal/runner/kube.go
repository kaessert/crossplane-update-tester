package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
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
	// GetObjectJSON returns the full resource, rendered as JSON.
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

// kubeBackendEnvVar selects which backend serves the event-list operation.
// "exec" forces every KubeClient operation, including events, onto the exec
// backend — the rollback path if the client-go path misbehaves on a
// provider this migration's own live run did not cover. Any other value,
// including unset, routes event listing through the client-go backend and
// leaves every other operation on the exec backend.
const kubeBackendEnvVar = "UPDATE_TESTER_KUBE_BACKEND"

// clientGoKubeClient overrides ONLY event listing with a direct,
// field-selected client-go List call. Every other KubeClient operation is
// promoted from the embedded exec backend unchanged — this type exists to
// migrate exactly one operation at a time, not to grow into a second full
// backend. A shared informer or watch cache is deliberately NOT used here:
// the evidence check this feeds is a before/after delta on the same
// object, and a lagging cache would return a stale second read and
// manufacture a false negative. clientset is a func rather than a stored
// value so it can be resolved lazily (a Runner is often built before a
// kubeconfig is needed) and overridden by tests.
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
//  3. Otherwise: the client-go backend for event listing (the default,
//     production behaviour), built from kubeClientsetFunc when a test has
//     set one, or from the ambient kubeconfig otherwise. Every other
//     operation still delegates to the embedded exec backend.
func (r *Runner) kube() KubeClient {
	exec := &execKubeClient{r: r}
	if os.Getenv(kubeBackendEnvVar) == "exec" {
		return exec
	}
	if r.execFunc != nil && r.kubeClientsetFunc == nil {
		return exec
	}
	return &clientGoKubeClient{execKubeClient: exec, clientset: r.goClientset}
}
