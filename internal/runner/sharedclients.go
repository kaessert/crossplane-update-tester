package runner

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// SharedClients is ONE client-go client set — a Clientset, a dynamic
// client, and a discovery-backed RESTMapper, all built from ONE *rest.Config
// — meant to be shared by every worker Runner in a batch run instead of
// each building its own.
//
// This is the structural fix batch mode exists to deliver: the exec-per-
// process parallelism it replaces gave every worker its own kubeconfig
// load, its own client-go Clientset/dynamic client/discovery cache, and its
// own client-side token bucket, so N workers meant N of everything — an
// accident of the process-per-worker design, not something anyone chose.
// Building exactly one of each here and handing it to every worker's Runner
// via Apply is what makes "N workers, one client set" a property a test can
// assert on, rather than a convention every future call site has to
// remember.
type SharedClients struct {
	clientset  kubernetes.Interface
	dynamic    dynamic.Interface
	restMapper meta.RESTMapper
	throttle   *ThrottleTracker
}

// Throttle returns the ThrottleTracker observing every request issued by
// any client this SharedClients built. Every worker's Runner is pointed at
// the SAME SharedClients (see Apply), so a 429 seen on behalf of any one
// fixture is visible here regardless of which worker triggered it — the
// signal RunBatch's adaptive concurrency reduction consumes.
func (sc *SharedClients) Throttle() *ThrottleTracker { return sc.throttle }

// sharedClientsBuilders is the full set of construction steps
// NewSharedClients performs, factored out so a test can substitute
// call-counting stand-ins for every one of them without a real kubeconfig
// or a live API server — the seam a structural "built exactly once
// regardless of --parallel N" test depends on. Production code always uses
// defaultSharedClientsBuilders.
type sharedClientsBuilders struct {
	restConfig func() (*rest.Config, error)
	clientset  func(*rest.Config) (kubernetes.Interface, error)
	dynamic    func(*rest.Config) (dynamic.Interface, error)
	discovery  func(*rest.Config) (discovery.DiscoveryInterface, error)
}

var defaultSharedClientsBuilders = sharedClientsBuilders{
	restConfig: kubeRESTConfig,
	clientset: func(cfg *rest.Config) (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(cfg)
	},
	dynamic: func(cfg *rest.Config) (dynamic.Interface, error) {
		return dynamic.NewForConfig(cfg)
	},
	discovery: func(cfg *rest.Config) (discovery.DiscoveryInterface, error) {
		return discovery.NewDiscoveryClientForConfig(cfg)
	},
}

// NewSharedClients resolves the ambient kubeconfig and builds the Clientset,
// dynamic client and RESTMapper this batch run will share, wrapping the
// REST config's transport with a ThrottleTracker so every request any of
// the three issues — and so every request any worker's Runner issues, once
// pointed at this SharedClients via Apply — reports through the same
// rate-limit signal. Called exactly once per batch run; see RunBatch's doc
// comment for why that "once" is the caller's responsibility, not
// something this function enforces on its own.
func NewSharedClients() (*SharedClients, error) {
	return newSharedClients(defaultSharedClientsBuilders)
}

// newSharedClients is NewSharedClients' testable core: everything above the
// three constructor calls is real, but which functions perform those calls
// is parameterised so a test can prove "invoked exactly once" without a
// live cluster. See sharedClientsBuilders.
func newSharedClients(b sharedClientsBuilders) (*SharedClients, error) {
	cfg, err := b.restConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving kubeconfig: %w", err)
	}

	tracker := newThrottleTracker()
	base := cfg.WrapTransport
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if base != nil {
			rt = base(rt)
		}
		return &throttleRoundTripper{next: rt, tracker: tracker}
	}

	cs, err := b.clientset(cfg)
	if err != nil {
		return nil, fmt.Errorf("building shared client-go clientset: %w", err)
	}
	dyn, err := b.dynamic(cfg)
	if err != nil {
		return nil, fmt.Errorf("building shared dynamic client: %w", err)
	}
	dc, err := b.discovery(cfg)
	if err != nil {
		return nil, fmt.Errorf("building shared discovery client: %w", err)
	}

	return &SharedClients{
		clientset:  cs,
		dynamic:    dyn,
		restMapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
		throttle:   tracker,
	}, nil
}

// Apply points r's own memoized client-go overrides (kubeClientsetFunc,
// kubeDynamicFunc, restMapperFunc — the exact seam tests already use to
// inject a fake client-go client into a Runner) at this SharedClients'
// instances, so r never resolves a kubeconfig, never builds its own
// Clientset/dynamic client/discovery client, and never runs its own
// discovery round trip: every one of those already happened once, in
// NewSharedClients, for the whole batch.
func (sc *SharedClients) Apply(r *Runner) {
	r.kubeClientsetFunc = func() (kubernetes.Interface, error) { return sc.clientset, nil }
	r.kubeDynamicFunc = func() (dynamic.Interface, error) { return sc.dynamic, nil }
	r.restMapperFunc = func() (meta.RESTMapper, error) { return sc.restMapper, nil }
}
