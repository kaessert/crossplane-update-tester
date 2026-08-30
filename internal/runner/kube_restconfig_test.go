package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// writeMinimalKubeconfig points KUBECONFIG at a syntactically valid,
// never-contacted cluster so kubeRESTConfig can resolve without a live
// control plane. Nothing here dials the server.
func writeMinimalKubeconfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

// TestKubeRESTConfigRaisesRateLimitAboveClientGoDefaults pins the fix for a
// measured regression: every client-go client was built from a config that
// never set QPS/Burst, so client-go substituted DefaultQPS=5 / DefaultBurst=10
// and throttled the whole tool to five requests a second.
//
// The exec backend this replaced was accidentally immune — each `kubectl`
// invocation is a fresh process with a fresh token bucket — so migrating to
// one long-lived in-process client is precisely what turned a per-process
// limiter into a per-run one. A DNSView E2E measured 377s throttled against
// 120s unthrottled, and 256s for the exec path, i.e. the migration was a
// net LOSS until this was set.
func TestKubeRESTConfigRaisesRateLimitAboveClientGoDefaults(t *testing.T) {
	writeMinimalKubeconfig(t)

	cfg, err := kubeRESTConfig()
	if err != nil {
		t.Fatalf("kubeRESTConfig: %v", err)
	}

	if cfg.QPS != kubeClientQPS {
		t.Errorf("QPS = %v, want %v", cfg.QPS, kubeClientQPS)
	}
	if cfg.Burst != kubeClientBurst {
		t.Errorf("Burst = %v, want %v", cfg.Burst, kubeClientBurst)
	}

	// The defect itself: a zero value is what client-go replaces with its
	// defaults, so "unset" and "deliberately 5" are indistinguishable here.
	if cfg.QPS == 0 || cfg.Burst == 0 {
		t.Fatal("QPS/Burst left at zero — client-go substitutes DefaultQPS=5/DefaultBurst=10, which is the throttle this test exists to prevent")
	}
	if cfg.QPS <= rest.DefaultQPS {
		t.Errorf("QPS = %v does not exceed client-go's DefaultQPS = %v; the regression is back", cfg.QPS, rest.DefaultQPS)
	}
	if cfg.Burst <= rest.DefaultBurst {
		t.Errorf("Burst = %v does not exceed client-go's DefaultBurst = %v", cfg.Burst, rest.DefaultBurst)
	}

	// Guard the other direction. A negative QPS makes client-go skip the
	// limiter entirely, which removes the ceiling that stops a defect in
	// this tool from saturating the API server of the cluster under test.
	if cfg.QPS < 0 {
		t.Error("QPS is negative — that disables client-go's limiter outright; this tool wants a raised ceiling, not no ceiling")
	}
}

// TestEveryClientGoConfigGoesThroughKubeRESTConfig is the check that would
// have caught the original defect, and the one that catches the next client
// added without a rate limit.
//
// The bug was not a wrong value; it was THREE independent construction sites
// (clientset, dynamic, discovery) each resolving their own config, none of
// which set QPS. Asserting the value on one helper proves nothing if a
// fourth site quietly resolves its own. So assert structurally: the
// kubeconfig loader may appear exactly once in this file, inside
// kubeRESTConfig.
func TestEveryClientGoConfigGoesThroughKubeRESTConfig(t *testing.T) {
	src, err := os.ReadFile("kube.go")
	if err != nil {
		t.Fatalf("reading kube.go: %v", err)
	}
	const loader = "NewNonInteractiveDeferredLoadingClientConfig"

	if n := strings.Count(string(src), loader); n != 1 {
		t.Errorf("%s appears %d times in kube.go, want exactly 1 (inside kubeRESTConfig).\n"+
			"Each additional occurrence is a client-go config built without this tool's "+
			"QPS/Burst, which is exactly how the 5 QPS throttle shipped.", loader, n)
	}

	// ...and that the one occurrence really is inside kubeRESTConfig.
	body := regexp.MustCompile(`(?s)func kubeRESTConfig\(\).*?\n}`).Find(src)
	if body == nil {
		t.Fatal("kubeRESTConfig not found in kube.go")
	}
	if !strings.Contains(string(body), loader) {
		t.Errorf("the sole %s call is not inside kubeRESTConfig", loader)
	}
}
