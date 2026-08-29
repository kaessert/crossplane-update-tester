package runner

import (
	"reflect"
	"testing"
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
		"ListEventsAllNamespaces": {
			reason:   "countEventsByReason listed events across every namespace, and never carried a -n flag",
			call:     func(c KubeClient) (string, error) { return c.ListEventsAllNamespaces() },
			wantArgs: []string{"get", "events", "--all-namespaces", "-o", "json"},
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
// governs KubeClient's exec backend: a Runner built as a bare struct
// literal with execFunc set (exactly how every pre-existing test in this
// package constructs one) never shells out to a real kubectl binary.
func TestRunnerKubeUsesExecFunc(t *testing.T) {
	called := false
	r := &Runner{execFunc: func(args []string) (string, error) {
		called = true
		return "ok", nil
	}}

	out, err := r.kube().ListEventsAllNamespaces()
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
