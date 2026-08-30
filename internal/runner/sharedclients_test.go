package runner

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

// countingSharedClientsBuilders returns a sharedClientsBuilders whose four
// steps each increment their own counter and otherwise return a working
// fake, so a test can drive real concurrent callers through Apply and
// prove each step ran exactly once — this IS the structural guard the
// review's mutation test targets: moving any one of these calls inside a
// per-worker loop must make its counter equal the worker count instead of
// staying at 1.
func countingSharedClientsBuilders() (b sharedClientsBuilders, counts *[4]*int32) {
	var restConfigCalls, clientsetCalls, dynamicCalls, discoveryCalls int32
	b = sharedClientsBuilders{
		restConfig: func() (*rest.Config, error) {
			atomic.AddInt32(&restConfigCalls, 1)
			return &rest.Config{Host: "https://example.invalid"}, nil
		},
		clientset: func(*rest.Config) (kubernetes.Interface, error) {
			atomic.AddInt32(&clientsetCalls, 1)
			return fake.NewSimpleClientset(), nil
		},
		dynamic: func(*rest.Config) (dynamic.Interface, error) {
			atomic.AddInt32(&dynamicCalls, 1)
			return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), nil
		},
		discovery: func(*rest.Config) (discovery.DiscoveryInterface, error) {
			atomic.AddInt32(&discoveryCalls, 1)
			return &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}}, nil
		},
	}
	return b, &[4]*int32{&restConfigCalls, &clientsetCalls, &dynamicCalls, &discoveryCalls}
}

// TestNewSharedClientsBuildsExactlyOnceRegardlessOfWorkerCount is the
// structural guard the review's own test plan calls out by name: eight
// workers, each with their OWN Runner pointed at the same SharedClients via
// Apply, must resolve their client-go handles by returning the ONE shared
// instance rather than each triggering a fresh construction. A mutation
// that instead built a SharedClients (or called any of its four
// constructor steps) once per worker turns every counter below from 1 into
// (at least) the worker count.
func TestNewSharedClientsBuildsExactlyOnceRegardlessOfWorkerCount(t *testing.T) {
	builders, counts := countingSharedClientsBuilders()

	sc, err := newSharedClients(builders)
	if err != nil {
		t.Fatalf("newSharedClients: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Runner{}
			sc.Apply(r)
			if _, err := r.goClientset(); err != nil {
				t.Error(err)
			}
			if _, err := r.goDynamicClient(); err != nil {
				t.Error(err)
			}
			if _, err := r.restMapper(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	names := []string{"restConfig", "clientset", "dynamic", "discovery"}
	for i, c := range counts {
		if got := atomic.LoadInt32(c); got != 1 {
			t.Errorf("%s builder invoked %d time(s) across %d workers, want exactly 1", names[i], got, workers)
		}
	}
}

// TestSharedClientsApplyBypassesEachRunnersOwnConstruction proves the
// OTHER half of the guarantee: not just "built once", but that a worker's
// OWN Runner never even attempts its own kubeconfig-backed construction —
// Apply must fully replace kubeClientsetFunc/kubeDynamicFunc/restMapperFunc
// rather than merely seeding a default a Runner might still override or
// fall back past.
func TestSharedClientsApplyBypassesEachRunnersOwnConstruction(t *testing.T) {
	builders, _ := countingSharedClientsBuilders()
	sc, err := newSharedClients(builders)
	if err != nil {
		t.Fatalf("newSharedClients: %v", err)
	}

	r := &Runner{}
	sc.Apply(r)

	cs, err := r.goClientset()
	if err != nil {
		t.Fatalf("goClientset: %v", err)
	}
	if cs != sc.clientset {
		t.Error("Runner.goClientset() did not return the SharedClients instance")
	}
	dyn, err := r.goDynamicClient()
	if err != nil {
		t.Fatalf("goDynamicClient: %v", err)
	}
	if dyn != sc.dynamic {
		t.Error("Runner.goDynamicClient() did not return the SharedClients instance")
	}
	rm, err := r.restMapper()
	if err != nil {
		t.Fatalf("restMapper: %v", err)
	}
	if rm != sc.restMapper {
		t.Error("Runner.restMapper() did not return the SharedClients instance")
	}
}

// TestNewSharedClientsPropagatesRestConfigFailure proves a kubeconfig
// resolution failure surfaces as an error rather than a nil SharedClients
// silently used later.
func TestNewSharedClientsPropagatesRestConfigFailure(t *testing.T) {
	builders, _ := countingSharedClientsBuilders()
	builders.restConfig = func() (*rest.Config, error) { return nil, errors.New("boom") }

	if _, err := newSharedClients(builders); err == nil {
		t.Fatal("newSharedClients returned no error for a failing restConfig builder")
	}
}
