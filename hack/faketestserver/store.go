// Command faketestserver is an in-memory, HTTP-served stand-in for a
// Kubernetes API server, used ONLY by hack/smoke-test.sh to drive the real
// compiled crossplane-update-tester binary — via an ordinary kubeconfig
// pointed at this process — with no live cluster and no exec-forced
// kubectl transcript. It exists so the smoke test can exercise the
// project's DEFAULT client-go backend end to end (stub module, symlinked
// hook, `go -C`, real binary) instead of the retired exec backend.
//
// It is deliberately NOT a general-purpose fake API server: it serves
// exactly the requests this project's own client-go backend issues against
// the handful of fixture kinds hack/testdata/examples declares (see
// registeredKinds), models a trivially well-behaved backend (a
// spec.forProvider merge patch bumps metadata.generation, is mirrored into
// status.atProvider, and emits one UpdatedExternalResource event; the
// object is always Ready), and does not attempt to be a real apiserver.
// The failure-injection scenarios (drift/stuck/loop/wipe) this project
// used to simulate through hack/testdata/fake-kubectl.sh now live as
// internal/runner unit tests against the client-go fake clientset/dynamic
// client — see internal/runner's own *_test.go files — because those
// scenarios exercise this tool's OWN evidence logic, not the shape of an
// HTTP request, and are far cheaper to assert on on a Go value than on a
// wire transcript.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// gvkInfo describes one fixture kind this server understands, mirroring
// the handful of fake Crossplane managed-resource kinds under
// hack/testdata/examples. Every kind here carries exactly one served
// version, which keeps discovery trivial: a real CRD's version ladder has
// no counterpart in this fixture set.
type gvkInfo struct {
	Group      string
	Version    string
	Kind       string
	Plural     string
	Namespaced bool
}

// registeredKinds is the full set of custom resource kinds this server
// serves discovery and CRUD/watch for. Adding a fixture kind to
// hack/testdata/examples means adding it here too — the server has no
// generic CRD-registration path, deliberately: it exists to serve this
// project's own smoke test, not arbitrary manifests.
var registeredKinds = []gvkInfo{
	{Group: "network.example.crossplane.io", Version: "v1alpha1", Kind: "Network", Plural: "networks", Namespaced: false},
	{Group: "widget.example.crossplane.io", Version: "v1alpha1", Kind: "Widget", Plural: "widgets", Namespaced: true},
	{Group: "dualscope.example.crossplane.io", Version: "v1alpha1", Kind: "DualScope", Plural: "dualscopes", Namespaced: false},
	{Group: "dualscope.m.example.crossplane.io", Version: "v1alpha1", Kind: "DualScope", Plural: "dualscopes", Namespaced: true},
}

// findKindByGroup looks up a registered kind by its group, the only field
// that is always unique across registeredKinds (two dual-scope variants
// share a Kind and a Plural, by design — see hack/testdata/examples/
// dualscope's own comments).
func findKindByGroup(group string) (gvkInfo, bool) {
	for _, k := range registeredKinds {
		if k.Group == group {
			return k, true
		}
	}
	return gvkInfo{}, false
}

// storedObject is one live fixture object: the JSON tree a GET renders,
// plus the bookkeeping fields (external-name, paused, event counters) the
// original hack/testdata/fake-kubectl.sh modelled as separate files under
// its per-resource state directory. See render and patchMerge for the
// exact behaviour this reproduces.
type storedObject struct {
	mu sync.Mutex

	apiVersion string
	kind       string
	name       string
	namespace  string // "" for a cluster-scoped object

	generation int64
	specFP     map[string]interface{} // spec.forProvider
	atFP       map[string]interface{} // status.atProvider

	extName     string
	extNamePrev string
	paused      bool

	evUpdate int
	evCreate int
}

// key is this object's identity in serverState.objects: unique per
// group+namespace+name (Plural/Kind may collide across the dual-scope
// group pair by design, Group never does).
func (o *storedObject) key(group string) string {
	return group + "|" + o.namespace + "|" + o.name
}

// render builds the JSON-marshalable representation of the object exactly
// as a `GET` (or the synthetic ADDED event a watch opens with) would
// return it: external-name/paused folded into metadata.annotations, and
// status.conditions computed fresh from the current generation rather than
// stored — this fake's "controller" reconciles instantly, exactly like
// fake-kubectl.sh's render_object did, and a ClearConditions status patch
// (see patchStatus) is a deliberate no-op for the same reason.
func (o *storedObject) render() map[string]interface{} {
	o.mu.Lock()
	defer o.mu.Unlock()

	annotations := map[string]interface{}{}
	if o.extName != "" {
		annotations["crossplane.io/external-name"] = o.extName
	}
	if o.paused {
		annotations["crossplane.io/paused"] = "true"
	}
	metadata := map[string]interface{}{
		"name":              o.name,
		"generation":        o.generation,
		"annotations":       annotations,
		"resourceVersion":   strconv.FormatInt(o.generation, 10),
		"uid":               o.name,
		"creationTimestamp": nil,
	}
	if o.namespace != "" {
		metadata["namespace"] = o.namespace
	}

	return map[string]interface{}{
		"apiVersion": o.apiVersion,
		"kind":       o.kind,
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"forProvider": deepCopyMap(o.specFP),
		},
		"status": map[string]interface{}{
			"atProvider": deepCopyMap(o.atFP),
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Available", "observedGeneration": o.generation},
				map[string]interface{}{"type": "Synced", "status": "True", "reason": "ReconcileSuccess", "observedGeneration": o.generation},
			},
		},
	}
}

// patchMerge applies an RFC 7386 merge-patch document to the object's main
// body, reproducing fake-kubectl.sh's cmd_patch dispatch: a
// spec.forProvider change is mirrored into status.atProvider (uppercased
// when the field is named in uppercaseFields, exercising a manifest entry
// whose declared `expect:` differs from its `value:`) and bumps
// metadata.generation and the update-event counter exactly once per call;
// a metadata.annotations change is interpreted for the three annotation
// keys this project's own runner ever writes
// (crossplane.io/paused, crossplane.io/external-name, and the private
// nudge annotation) and never bumps generation, matching real Kubernetes
// behaviour for an annotation-only write.
func (o *storedObject) patchMerge(body map[string]interface{}, uppercaseFields map[string]bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	changed := false
	if specPatch, ok := body["spec"].(map[string]interface{}); ok {
		if fp, ok := specPatch["forProvider"].(map[string]interface{}); ok {
			for field, value := range fp {
				changed = true
				if value == nil {
					delete(o.specFP, field)
					delete(o.atFP, field)
					continue
				}
				o.specFP[field] = value
				stored := value
				if sv, ok := value.(string); ok && uppercaseFields[field] {
					stored = strings.ToUpper(sv)
				}
				o.atFP[field] = stored
			}
		}
	}

	if metaPatch, ok := body["metadata"].(map[string]interface{}); ok {
		if ann, ok := metaPatch["annotations"].(map[string]interface{}); ok {
			for k, v := range ann {
				switch k {
				case "crossplane.io/paused":
					if v == nil {
						o.paused = false
						// Unpausing with no external-name recovers the
						// previously stripped one: the controller
						// resolves identity by searching the backend and
						// finds the object it already created, so no
						// second CreatedExternalResource event — see
						// RunResolveRecover's own doc comment for why
						// this is the property under test.
						if o.extName == "" && o.extNamePrev != "" {
							o.extName = o.extNamePrev
						}
					} else {
						o.paused = true
					}
				case "crossplane.io/external-name":
					if v == nil {
						o.extNamePrev = o.extName
						o.extName = ""
					}
				default:
					// update-tester.crossplane.io/nudge and anything else:
					// an annotation-only write that wakes the controller
					// but changes neither spec nor generation.
				}
			}
		}
	}

	if changed {
		o.generation++
		o.evUpdate++
	}
}

// deepCopyMap returns a copy of m safe to hand to a caller without letting
// it mutate this object's own stored state through the returned value.
func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}

// serverState is the whole server's in-memory dataset: every seeded
// fixture object, plus the single fake provider-controller Pod
// ControllerRevisionLabels/ControllerPodIdentities/ProviderLogs resolve
// against.
type serverState struct {
	mu              sync.RWMutex
	objects         map[string]*storedObject // key: gvkInfo.Group + "|" + namespace + "|" + name
	uppercaseFields map[string]bool
	podName         string
	podCreated      time.Time
}

func newServerState(uppercaseFields map[string]bool) *serverState {
	return &serverState{
		objects:         map[string]*storedObject{},
		uppercaseFields: uppercaseFields,
		// Old enough on first read to already clear
		// controllerPodSettleThreshold (15s) — this fake's controller Pod
		// never restarts mid-run, so a fixed identity read at any point
		// during the smoke test is correct.
		podName:    "provider-example-7f4c2b91d0ac",
		podCreated: time.Now().Add(-1 * time.Hour),
	}
}

// seedFromDir walks dir for YAML manifests and materialises one
// storedObject per document whose Kind is registered (see
// registeredKinds), mirroring spec.forProvider into status.atProvider —
// exactly hack/testdata/fake-kubectl.sh's seed_from_manifest, run once at
// startup instead of lazily on first read, because this server (unlike
// that per-invocation script) has no per-request access to the manifest
// path: the request carries only the resolved GVK/namespace/name a
// document was already required to declare.
func (s *serverState) seedFromDir(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		return s.seedFromFile(path)
	})
}

func (s *serverState) seedFromFile(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- fixture path under our own testdata tree
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	dec := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	for {
		var doc unstructured.Unstructured
		if err := dec.Decode(&doc); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				return nil
			}
			// Not every *.yaml under hack/testdata is a Kubernetes
			// manifest a decoder can parse as one (this fixture set has
			// none such today, but a future addition failing loudly here
			// beats silently seeding nothing).
			return nil //nolint:nilerr // best-effort seeding; a genuinely malformed fixture fails the smoke test downstream via a 404, which names the object instead of a decode error naming the wrong file.
		}
		if doc.GetKind() == "" || doc.GetName() == "" {
			continue
		}
		kind, ok := findKindByGroup(doc.GroupVersionKind().Group)
		if !ok || kind.Kind != doc.GetKind() {
			continue
		}
		s.seedObject(kind, &doc)
	}
}

func (s *serverState) seedObject(kind gvkInfo, doc *unstructured.Unstructured) {
	forProvider, _, _ := unstructured.NestedMap(doc.Object, "spec", "forProvider")
	if forProvider == nil {
		forProvider = map[string]interface{}{}
	}
	prefix, _, _ := unstructured.NestedString(doc.Object, "metadata", "annotations", "crossplane.io/expect-external-name-prefix")

	namespace := ""
	if kind.Namespaced {
		namespace = doc.GetNamespace()
		if namespace == "" {
			namespace = "default"
		}
	}

	obj := &storedObject{
		apiVersion: kind.Group + "/" + kind.Version,
		kind:       kind.Kind,
		name:       doc.GetName(),
		namespace:  namespace,
		generation: 1,
		specFP:     deepCopyMap(forProvider),
		atFP:       deepCopyMap(forProvider),
		extName:    prefix + doc.GetName(),
		evCreate:   1,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[obj.key(kind.Group)] = obj
}

// lookup returns the stored object for a group/namespace/name triple, or
// nil if none was seeded — the caller renders that as 404, exactly like a
// live apiserver reading an object that was never created.
func (s *serverState) lookup(group, namespace, name string) *storedObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objects[group+"|"+namespace+"|"+name]
}

// listByGroup returns every stored object for one group, in an unspecified
// order — used by the collection GET/WATCH endpoint, which then narrows by
// namespace and the metadata.name field selector itself.
func (s *serverState) listByGroup(group string) []*storedObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*storedObject
	for k, o := range s.objects {
		if strings.HasPrefix(k, group+"|") {
			out = append(out, o)
		}
	}
	return out
}

// all returns every stored object regardless of group — used to build the
// aggregated Event list.
func (s *serverState) all() []*storedObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*storedObject, 0, len(s.objects))
	for _, o := range s.objects {
		out = append(out, o)
	}
	return out
}
