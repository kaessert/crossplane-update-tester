package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

// apiHandler routes every request this server needs to answer. It is a
// single hand-rolled router rather than net/http.ServeMux's pattern
// matching: the path shapes below differ only by which segments are
// literal ("namespaces", "status") versus which fixture kind occupies
// them, and walking the segments once here is easier to reason about than
// relying on wildcard-versus-literal precedence for a handful of routes
// that are never going to grow.
type apiHandler struct {
	state *serverState
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	switch parts[0] {
	case "api":
		h.serveCoreV1(w, r, parts[1:])
		return
	case "apis":
		h.serveCustom(w, r, parts[1:])
		return
	}
	log.Printf("faketestserver: unhandled path %s %s", r.Method, r.URL.String())
	http.NotFound(w, r)
}

// ─── /api/v1/... (typed clientset: pods, events) ───────────────────────────

func (h *apiHandler) serveCoreV1(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, legacyAPIVersionsDoc())
	case len(rest) == 1 && rest[0] == "v1":
		writeJSON(w, http.StatusOK, coreV1ResourceListDoc())
	case len(rest) >= 3 && rest[0] == "v1" && rest[1] == "namespaces" && rest[3] == "pods":
		h.serveCoreV1Namespaced(w, r, rest[2], rest[4:])
	case len(rest) >= 2 && rest[0] == "v1" && rest[1] == "events":
		h.serveEvents(w, r)
	default:
		log.Printf("faketestserver: unhandled core v1 path %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (h *apiHandler) serveCoreV1Namespaced(w http.ResponseWriter, r *http.Request, namespace string, tail []string) {
	switch len(tail) {
	case 0:
		h.servePodsList(w, r, namespace)
	case 2:
		if tail[1] == "log" {
			// Pod logs: this fake's controller is always quiet — no
			// Update() line is ever emitted — so every log read returns
			// empty content. countUpdateLogCalls treats that as "zero
			// calls observed", exactly the well-behaved-backend model
			// this server exists to serve; the loop-detection scenarios
			// that DO need non-empty log content are internal/runner unit
			// tests against a fake podLogStreamFunc, not this server.
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *apiHandler) servePodsList(w http.ResponseWriter, r *http.Request, namespace string) {
	sel := r.URL.Query().Get("labelSelector")
	matches := true
	if sel != "" {
		selector, err := labelSelectorMatches(sel, map[string]string{"pkg.crossplane.io/revision": h.state.podName})
		if err != nil {
			writeStatusError(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("parsing labelSelector: %v", err))
			return
		}
		matches = selector
	}

	list := &corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
	if matches {
		list.Items = []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:              h.state.podName,
				Namespace:         namespace,
				CreationTimestamp: metav1.NewTime(h.state.podCreated),
				Labels:            map[string]string{"pkg.crossplane.io/revision": h.state.podName},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "provider"}},
			},
		}}
	}
	writeJSONValue(w, http.StatusOK, list)
}

func (h *apiHandler) serveEvents(w http.ResponseWriter, r *http.Request) {
	// Field-selector narrowing is intentionally NOT applied server-side:
	// this server always returns the full aggregated event list, and
	// internal/runner's own sumEventOccurrencesByReason re-filters by
	// kind/name/namespace/apiVersion-group client-side regardless of what
	// a backend's server-side selector already narrowed — see that
	// function's doc comment. Returning everything keeps this server
	// correct even if a future caller's selector syntax drifts from what
	// this server understands.
	writeJSONValue(w, http.StatusOK, buildEventList(h.state))
}

func buildEventList(state *serverState) *corev1.EventList {
	list := &corev1.EventList{TypeMeta: metav1.TypeMeta{Kind: "EventList", APIVersion: "v1"}}
	for _, o := range state.all() {
		o.mu.Lock()
		kind, apiVersion, name, namespace := o.kind, o.apiVersion, o.name, o.namespace
		evUpdate, evCreate := o.evUpdate, o.evCreate
		o.mu.Unlock()

		if evUpdate > 0 {
			list.Items = append(list.Items, corev1.Event{
				Reason:         "UpdatedExternalResource",
				Count:          int32(evUpdate), //nolint:gosec // evUpdate is a small in-process counter, never attacker-controlled
				InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: namespace, APIVersion: apiVersion},
			})
		}
		if evCreate > 0 {
			list.Items = append(list.Items, corev1.Event{
				Reason:         "CreatedExternalResource",
				Count:          int32(evCreate), //nolint:gosec // evCreate is a small in-process counter, never attacker-controlled
				InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: namespace, APIVersion: apiVersion},
			})
		}
	}
	return list
}

// labelSelectorMatches reports whether obj's labels satisfy sel, using the
// same k8s.io/apimachinery/pkg/labels selector syntax a real apiserver
// parses.
func labelSelectorMatches(sel string, obj map[string]string) (bool, error) {
	selector, err := labels.Parse(sel)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(obj)), nil
}

// ─── /apis/<group>/<version>/... (discovery + dynamic CRUD/watch) ─────────

func (h *apiHandler) serveCustom(w http.ResponseWriter, r *http.Request, rest []string) {
	switch len(rest) {
	case 0:
		writeJSON(w, http.StatusOK, apiGroupListDoc())
		return
	case 1:
		k, ok := findKindByGroup(rest[0])
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, apiGroupDoc(k))
		return
	}

	group, version := rest[0], rest[1]
	k, ok := findKindByGroup(group)
	if !ok || k.Version != version {
		http.NotFound(w, r)
		return
	}
	tail := rest[2:]

	if len(tail) == 0 {
		writeJSON(w, http.StatusOK, apiResourceListDoc(k))
		return
	}

	namespace := ""
	if tail[0] == "namespaces" {
		if len(tail) < 3 {
			http.NotFound(w, r)
			return
		}
		namespace = tail[1]
		tail = tail[2:]
	}

	if len(tail) == 0 || tail[0] != k.Plural {
		http.NotFound(w, r)
		return
	}
	tail = tail[1:]

	switch len(tail) {
	case 0:
		h.serveCollection(w, r, k, namespace)
	case 1:
		h.serveObject(w, r, k, namespace, tail[0])
	case 2:
		if tail[1] != "status" {
			http.NotFound(w, r)
			return
		}
		h.serveStatus(w, r, k, namespace, tail[0])
	default:
		http.NotFound(w, r)
	}
}

// serveCollection answers both the plain LIST a reflector's initial read
// issues and the WATCH it opens afterward, filtered by the
// metadata.name field selector every caller in this project sets (see
// clientGoKubeClient.WaitForCondition and its ListWatch). Because this
// fake's objects are always Ready the instant they exist, a watch needs no
// live push at all: it emits one synthetic ADDED event per object the
// initial list would have returned, then blocks until the client
// disconnects — exactly the semantics client-go's own informer synthesises
// from a plain list when nothing further ever changes.
func (h *apiHandler) serveCollection(w http.ResponseWriter, r *http.Request, k gvkInfo, namespace string) {
	name := fieldSelectorName(r.URL.Query().Get("fieldSelector"))

	var matched []*storedObject
	for _, o := range h.state.listByGroup(k.Group) {
		if o.namespace != namespace {
			continue
		}
		if name != "" && o.name != name {
			continue
		}
		matched = append(matched, o)
	}

	if r.URL.Query().Get("watch") == "true" {
		h.streamWatch(w, r, matched)
		return
	}

	list := map[string]interface{}{
		"apiVersion": k.Group + "/" + k.Version,
		"kind":       k.Kind + "List",
		"metadata":   map[string]interface{}{"resourceVersion": "1"},
	}
	items := make([]interface{}, 0, len(matched))
	for _, o := range matched {
		items = append(items, o.render())
	}
	list["items"] = items
	writeJSON(w, http.StatusOK, list)
}

// streamWatch writes one ADDED event per object in matched, flushes, and
// then blocks until the request's context is done (the client closing the
// connection, or its own timeout). See serveCollection's doc comment for
// why no further event is ever needed.
func (h *apiHandler) streamWatch(w http.ResponseWriter, r *http.Request, matched []*storedObject) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	for _, o := range matched {
		event := map[string]interface{}{"type": "ADDED", "object": o.render()}
		if err := enc.Encode(event); err != nil {
			return
		}
	}
	flusher.Flush()

	<-r.Context().Done()
}

func (h *apiHandler) serveObject(w http.ResponseWriter, r *http.Request, k gvkInfo, namespace, name string) {
	o := h.state.lookup(k.Group, namespace, name)
	if o == nil {
		writeStatusError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", k.Kind, name))
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, o.render())
	case http.MethodPatch:
		body, err := readJSONObject(r)
		if err != nil {
			writeStatusError(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		o.patchMerge(body, h.state.uppercaseFields)
		writeJSON(w, http.StatusOK, o.render())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveStatus answers the status subresource. Every status patch this
// project issues (ClearConditions) is a deliberate no-op here — see
// storedObject.render's doc comment — so this only ever needs to return
// the current rendered object, on both GET and PATCH.
func (h *apiHandler) serveStatus(w http.ResponseWriter, r *http.Request, k gvkInfo, namespace, name string) {
	o := h.state.lookup(k.Group, namespace, name)
	if o == nil {
		writeStatusError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", k.Kind, name))
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodPatch:
		if r.Method == http.MethodPatch {
			if _, err := readJSONObject(r); err != nil {
				writeStatusError(w, http.StatusBadRequest, "BadRequest", err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, o.render())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── shared helpers ─────────────────────────────────────────────────────

// fieldSelectorName extracts metadata.name's value out of a fieldSelector
// query string — the only term this project's own client-go backend ever
// sets on a resource collection watch/list (see WaitForCondition). Returns
// "" (matching every object) when absent or the term is not present.
func fieldSelectorName(raw string) string {
	if raw == "" {
		return ""
	}
	sel, err := fields.ParseSelector(raw)
	if err != nil {
		return ""
	}
	return sel.Requirements()[0].Value
}

func readJSONObject(r *http.Request) (map[string]interface{}, error) {
	defer r.Body.Close() //nolint:errcheck // best-effort close on a request body we've already fully consumed or errored out of
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decoding request body: %w", err)
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("faketestserver: encoding response: %v", err)
	}
}

func writeJSONValue(w http.ResponseWriter, status int, v interface{}) {
	writeJSON(w, status, v)
}

// writeStatusError writes a Kubernetes-shaped Status error body — the same
// shape apierrors.FromObject/NewNotFound expects to parse back out of a
// non-2xx response, so a caller inspecting apierrors.IsNotFound(err) on
// this server's 404 gets the same answer it would from a real apiserver.
func writeStatusError(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, map[string]interface{}{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    message,
		"reason":     reason,
		"code":       code,
	})
}

// serverReadTimeout/serverWriteTimeout are deliberately generous: a watch
// response is held open for the lifetime of a caller's own --timeout, and
// this server has no independent reason to cut that shorter.
const (
	serverReadTimeout  = 5 * time.Second
	serverWriteTimeout = 10 * time.Minute
)
