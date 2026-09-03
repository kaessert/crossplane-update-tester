package main

// Discovery response builders. These exist solely so
// restmapper.NewDeferredDiscoveryRESTMapper (the exact machinery
// internal/runner's client-go backend uses to resolve a `<plural>.<group>`
// identifier, or a decoded manifest document's GroupVersionKind, to a full
// GroupVersionResource) can do its job against this fake the same way it
// does against a real apiserver: GET /apis, then GET /apis/<group>/<version>
// for every group and version that call reports.

// apiGroupListDoc mirrors k8s.io/apimachinery/pkg/apis/meta/v1.APIGroupList's
// JSON shape closely enough for discovery.ServerGroups to parse it — built
// by hand rather than importing the type, since every field this server
// needs to set is already named identically.
func apiGroupListDoc() map[string]interface{} {
	groups := make([]interface{}, 0, len(registeredKinds))
	seen := map[string]bool{}
	for _, k := range registeredKinds {
		if seen[k.Group] {
			continue
		}
		seen[k.Group] = true
		gv := k.Group + "/" + k.Version
		groups = append(groups, map[string]interface{}{
			"name": k.Group,
			"versions": []interface{}{
				map[string]interface{}{"groupVersion": gv, "version": k.Version},
			},
			"preferredVersion": map[string]interface{}{"groupVersion": gv, "version": k.Version},
		})
	}
	return map[string]interface{}{
		"kind":       "APIGroupList",
		"apiVersion": "v1",
		"groups":     groups,
	}
}

func apiGroupDoc(k gvkInfo) map[string]interface{} {
	gv := k.Group + "/" + k.Version
	return map[string]interface{}{
		"kind":       "APIGroup",
		"apiVersion": "v1",
		"name":       k.Group,
		"versions": []interface{}{
			map[string]interface{}{"groupVersion": gv, "version": k.Version},
		},
		"preferredVersion": map[string]interface{}{"groupVersion": gv, "version": k.Version},
	}
}

// apiResourceListDoc builds the APIResourceList for one group/version: the
// main resource (get/list/watch/patch) plus its status subresource
// (get/patch) — the same pair kubectl and this project's own client-go
// backend expect for any resource with a status subresource.
func apiResourceListDoc(k gvkInfo) map[string]interface{} {
	gv := k.Group + "/" + k.Version
	return map[string]interface{}{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": gv,
		"resources": []interface{}{
			map[string]interface{}{
				"name":       k.Plural,
				"namespaced": k.Namespaced,
				"kind":       k.Kind,
				"verbs":      []interface{}{"get", "list", "patch", "watch"},
			},
			map[string]interface{}{
				"name":       k.Plural + "/status",
				"namespaced": k.Namespaced,
				"kind":       k.Kind,
				"verbs":      []interface{}{"get", "patch"},
			},
		},
	}
}

// legacyAPIVersionsDoc answers GET /api: the core (legacy) group carries
// exactly v1, which is all discovery.ServerGroups needs to see to stop
// treating the legacy group as absent.
func legacyAPIVersionsDoc() map[string]interface{} {
	return map[string]interface{}{
		"kind":       "APIVersions",
		"apiVersion": "v1",
		"versions":   []interface{}{"v1"},
	}
}

// coreV1ResourceListDoc answers GET /api/v1: the two core resources this
// server actually serves (pods, events). Neither the typed clientset nor
// the dynamic/RESTMapper path this project uses ever needs this for a
// well-known core type — kubernetes.NewForConfig hardcodes its REST
// paths — but a discovery round trip that 404s on /api/v1 is worth
// avoiding rather than relying on client-go tolerating it.
func coreV1ResourceListDoc() map[string]interface{} {
	return map[string]interface{}{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": "v1",
		"resources": []interface{}{
			map[string]interface{}{"name": "pods", "namespaced": true, "kind": "Pod", "verbs": []interface{}{"get", "list", "watch"}},
			map[string]interface{}{"name": "pods/log", "namespaced": true, "kind": "Pod", "verbs": []interface{}{"get"}},
			map[string]interface{}{"name": "events", "namespaced": true, "kind": "Event", "verbs": []interface{}{"get", "list", "watch"}},
		},
	}
}
