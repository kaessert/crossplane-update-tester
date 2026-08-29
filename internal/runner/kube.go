package runner

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

	// ListEventsAllNamespaces returns every Event in the cluster, rendered
	// as JSON.
	ListEventsAllNamespaces() (string, error)

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

func (c *execKubeClient) ListEventsAllNamespaces() (string, error) {
	return c.r.exec("get", "events", "--all-namespaces", "-o", "json")
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

// kube returns the KubeClient backend for this Runner. It is a method
// rather than a field populated at construction so a Runner built as a bare
// struct literal — every existing test constructs one this way, setting
// execFunc directly with no knowledge of KubeClient — still resolves to an
// exec backend wired to that same execFunc/kubectl plumbing.
func (r *Runner) kube() KubeClient {
	return &execKubeClient{r: r}
}
