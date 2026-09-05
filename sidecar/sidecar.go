// Package sidecar loads and resolves the annotation sidecar that can sit
// beside a Crossplane example manifest.
//
// Half of every example manifest is test-harness configuration rather than
// documentation a user needs: annotations read by uptest and by
// update-tester, not by Kubernetes. A sidecar moves that configuration out
// of the manifest and into a file beside it, named "<manifest>.yaml.uptest",
// so the manifest itself keeps only what a reader applying it needs.
//
// # Selector grammar
//
// Each document inside a sidecar file selects the manifest object it
// supplies annotations for. The selector is the shortest prefix of the
// Kubernetes object identity (GVK, then name, then namespace) that
// identifies exactly one object in the manifest:
//
//   - for: "<apiVersion>/<Kind>", split at its LAST "/" (so "v1/Secret"
//     works for a core-group kind that carries no domain). Mandatory on
//     every document.
//   - name:, namespace: — required when for: alone is ambiguous, and
//     rejected as redundant when it is not. Both directions are enforced:
//     a directive that is merely PERMITTED when it changes nothing means
//     some sidecars carry one and some do not, and every one that does
//     must be edited whenever the example is renamed.
//
// A selector value must never carry a templating placeholder ("${...}"):
// update-tester reads the manifest's raw, un-templated text, so a name: or
// namespace: copied from a templated field could never match.
//
// # Values
//
// A top-level key containing "/" is a harness annotation; one without is a
// directive (for/name/namespace). An annotation's value may be written as
// plain YAML (a native int, a native list, a block scalar) — Parse renders
// each to the string form the annotation itself must hold: a string scalar
// (plain, quoted or block) is carried through byte for byte, and any other
// YAML value is re-marshalled to its equivalent text.
//
// # Switch, not overlay
//
// A sidecar REPLACES the manifest's harness annotations; it does not
// overlay them. A key live in both files has no defensible precedence, so a
// caller merging a sidecar onto a manifest must treat any inline occurrence
// of a key the sidecar also declares as a hard error rather than picking
// one silently. This package resolves selectors and renders values; that
// switch check belongs to each caller, because only the caller knows which
// keys it itself reads.
package sidecar

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Directive key names recognised at the top level of a sidecar document.
// Any other top-level key must contain "/" — the marker that distinguishes
// a harness annotation from a typo'd directive.
const (
	keyFor       = "for"
	keyName      = "name"
	keyNamespace = "namespace"
)

// clusterGroupSuffix and namespacedGroupSuffix are the two forms a
// Crossplane v2 API group takes for the same resource family: a
// cluster-scoped kind lives in "<...>.crossplane.io", its namespaced
// sibling in "<...>.m.crossplane.io" — the ".m." infix is the ONLY
// difference between the two groups. Used only to produce a more useful
// error message when a selector's scope looks confused; matching itself
// never depends on this.
const (
	clusterGroupSuffix    = ".crossplane.io"
	namespacedGroupSuffix = ".m.crossplane.io"
)

// ObjectID is the Kubernetes object identity a sidecar selector resolves
// against: GVK plus namespace and name. Namespace is empty for a
// cluster-scoped object.
type ObjectID struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// Doc is one selector-plus-annotations document decoded from a sidecar
// file. A sidecar beside a multi-document manifest declares one Doc per
// targeted object, the documents separated the same way the manifest
// itself is ("---"-separated YAML documents).
type Doc struct {
	// For is the raw for: directive value, "<apiVersion>/<Kind>".
	For string
	// APIVersion and Kind are For split at its last "/" — see splitFor.
	APIVersion string
	Kind       string
	// Name and Namespace are the optional narrowing directives, exactly as
	// the file declared them. Parse records only what the file said;
	// Resolve decides whether their presence is required, redundant, or
	// legal given the manifest's real objects.
	Name      string
	Namespace string
	// Annotations are every non-directive top-level key, with its value
	// already rendered to the string an annotation must hold.
	Annotations map[string]string
}

// File is a parsed sidecar: every Doc it declares, in file order.
type File struct {
	Docs []Doc
}

// ConflictError reports a selector that could not be resolved to exactly
// one manifest object: a duplicate selector within one File (Conflicts),
// or — given the manifest's real objects — a selector matching zero or
// more than one object, or carrying a redundant narrowing directive
// (Resolve).
type ConflictError struct {
	Message string
}

// Error implements the error interface.
func (e *ConflictError) Error() string { return e.Message }

// PathFor returns the sidecar path for a manifest at manifestPath. The
// extension is ".yaml.uptest", never ".uptest.yaml": a ".yaml" suffix is
// swept into every `examples/**/*.yaml` glob and would be applied to a
// cluster as if it were a manifest in its own right.
func PathFor(manifestPath string) string {
	return manifestPath + ".uptest"
}

// Load reads the sidecar beside manifestPath, if one exists. It returns
// (nil, nil) when there is no sidecar file — the un-migrated state every
// example starts in, and it must behave exactly like "no sidecar at all"
// for as long as that manifest stays un-migrated.
func Load(manifestPath string) (*File, error) {
	path := PathFor(manifestPath)
	// #nosec G304 -- path is derived from an operator-supplied manifest
	// path, not attacker-controlled input.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading sidecar %s: %w", path, err)
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing sidecar %s: %w", path, err)
	}
	return f, nil
}

// Parse decodes sidecar YAML bytes into a File. The input may be a
// multi-document ("---"-separated) stream — one document per object the
// sidecar's manifest declares. A blank document (the trailing "---" a
// stream sometimes ends with) is skipped rather than treated as an error.
//
// Parse rejects, at this stage, everything that does not depend on the
// manifest's actual objects: a missing for:, an unrecognised top-level
// key, a templated name:/namespace:, and two documents declaring the
// identical selector (see Conflicts). Everything that DOES depend on the
// manifest's objects — an ambiguous or non-matching selector, a redundant
// name:/namespace: — is Resolve's job, because Parse alone has nothing to
// check it against.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []Doc
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing sidecar YAML: %w", err)
		}
		doc, err := docFromNode(&node)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			continue
		}
		docs = append(docs, *doc)
	}

	f := &File{Docs: docs}
	if err := Conflicts(f); err != nil {
		return nil, err
	}
	return f, nil
}

// docFromNode converts one decoded YAML document node into a Doc, or
// returns (nil, nil) for a blank document.
func docFromNode(node *yaml.Node) (*Doc, error) {
	if node.Kind != yaml.DocumentNode || len(node.Content) != 1 {
		return nil, fmt.Errorf("malformed sidecar document")
	}
	root := node.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		// A blank document — e.g. the trailing "---" a stream is written
		// with. Nothing to select, nothing to report.
		return nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("sidecar document must be a YAML mapping, found %s", root.Tag)
	}

	doc := Doc{Annotations: map[string]string{}}
	var forSeen bool
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valNode := root.Content[i+1]
		key := keyNode.Value

		switch key {
		case keyFor:
			if valNode.Kind != yaml.ScalarNode || valNode.Value == "" {
				return nil, fmt.Errorf("sidecar for: must be a non-empty scalar")
			}
			forSeen = true
			doc.For = valNode.Value
			apiVersion, kind, err := splitFor(doc.For)
			if err != nil {
				return nil, err
			}
			doc.APIVersion = apiVersion
			doc.Kind = kind
		case keyName:
			v, err := narrowingValue(keyName, valNode)
			if err != nil {
				return nil, err
			}
			doc.Name = v
		case keyNamespace:
			v, err := narrowingValue(keyNamespace, valNode)
			if err != nil {
				return nil, err
			}
			doc.Namespace = v
		default:
			if !strings.Contains(key, "/") {
				return nil, fmt.Errorf(
					"sidecar top-level key %q is not a recognised directive (for/name/namespace) and carries no \"/\", "+
						"so it cannot be a harness annotation either — unknown directive", key)
			}
			value, err := renderValue(valNode)
			if err != nil {
				return nil, fmt.Errorf("rendering %s: %w", key, err)
			}
			doc.Annotations[key] = value
		}
	}
	if !forSeen {
		return nil, fmt.Errorf("sidecar document missing mandatory for: directive")
	}
	return &doc, nil
}

// narrowingValue extracts and validates a name:/namespace: directive
// value: it must be a plain scalar, and it must never carry a templating
// placeholder — update-tester reads the manifest's raw, un-templated text,
// so a selector value that only exists after templating can never match.
func narrowingValue(directive string, node *yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("sidecar %s: must be a scalar", directive)
	}
	if strings.Contains(node.Value, "${") {
		return "", fmt.Errorf(
			"sidecar %s: %q contains a templating placeholder (\"${...}\") — update-tester reads the raw manifest "+
				"file, before uptest's templating runs, so a templated %s: can never match",
			directive, node.Value, directive)
	}
	return node.Value, nil
}

// renderValue renders a YAML value node to the string form a harness
// annotation must hold. A scalar's Value already carries exactly what it
// should — a plain "1200" (native int), a quoted string with its quotes
// resolved away, or a block literal's content with real newlines and no
// reformatting — so it is used unchanged. Anything else (a native list or
// map) is re-marshalled to its equivalent YAML text.
func renderValue(node *yaml.Node) (string, error) {
	if node.Kind == yaml.ScalarNode {
		return node.Value, nil
	}
	out, err := yaml.Marshal(node)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// splitFor splits a for: value at its LAST "/" into apiVersion and kind,
// so a core-group kind ("v1/Secret", two segments) and a grouped kind
// ("lambda.crossplane.io/v1alpha1/Instance", three) both split correctly.
func splitFor(raw string) (apiVersion, kind string, err error) {
	idx := strings.LastIndex(raw, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("sidecar for: %q must be \"<apiVersion>/<Kind>\"", raw)
	}
	return raw[:idx], raw[idx+1:], nil
}

// Conflicts reports a File whose Docs declare the identical selector more
// than once — the same for:/name:/namespace: triple can never resolve to
// two different objects, so this is always a mistake independent of what
// the manifest the sidecar sits beside actually contains.
func Conflicts(f *File) error {
	if f == nil {
		return nil
	}
	seen := make(map[string]bool, len(f.Docs))
	for _, d := range f.Docs {
		key := d.For + "\x00" + d.Name + "\x00" + d.Namespace
		if seen[key] {
			return &ConflictError{Message: fmt.Sprintf(
				"duplicate selector: for: %s name: %q namespace: %q appears more than once in this sidecar",
				d.For, d.Name, d.Namespace)}
		}
		seen[key] = true
	}
	return nil
}

// Resolve matches every Doc in f against targets — the objects decoded
// from the manifest the sidecar sits beside, in file order — and returns
// the annotations each target should receive, keyed by the target's index
// in targets. A target with no matching Doc has no entry in the returned
// map. Resolve on a nil File returns (nil, nil): there is nothing to merge.
//
// Every Doc must resolve to EXACTLY one target, and every target must be
// claimed by at most one Doc:
//
//   - for: alone already selecting exactly one target is an error if
//     name: or namespace: is also present (redundant narrowing).
//   - for: matching no target is an error, naming the alternate ".m." API
//     group when the mismatch looks like a cluster/namespaced scope
//     confusion between two variants of the same kind.
//   - for:, optionally narrowed by name:/namespace:, still matching more
//     than one target is an error naming every remaining candidate.
//   - two Docs with DIFFERENT selectors that each resolve to the same
//     target are an error too — Conflicts only catches the identical-
//     selector case, so this is the one member of the ambiguity family it
//     cannot see (differently-spelled selectors landing on one object).
func Resolve(f *File, targets []ObjectID) (map[int]map[string]string, error) {
	if f == nil {
		return nil, nil
	}
	out := make(map[int]map[string]string, len(f.Docs))
	claimedBy := make(map[int]Doc, len(f.Docs))
	for _, doc := range f.Docs {
		idx, err := resolveOne(doc, targets)
		if err != nil {
			return nil, err
		}
		if prior, claimed := claimedBy[idx]; claimed {
			return nil, &ConflictError{Message: fmt.Sprintf(
				"sidecar selectors for: %s name: %q namespace: %q and for: %s name: %q namespace: %q both resolve to the same object (%s) — remove or narrow one of them",
				prior.For, prior.Name, prior.Namespace, doc.For, doc.Name, doc.Namespace, describeCandidates(targets, []int{idx}))}
		}
		claimedBy[idx] = doc
		if out[idx] == nil {
			out[idx] = make(map[string]string, len(doc.Annotations))
		}
		for k, v := range doc.Annotations {
			out[idx][k] = v
		}
	}
	return out, nil
}

// resolveOne resolves a single Doc's selector against targets, returning
// the index of the exactly-one target it selects.
func resolveOne(doc Doc, targets []ObjectID) (int, error) {
	byGVK := filterCandidates(targets, allIndices(len(targets)), "", "", doc.APIVersion, doc.Kind)
	if len(byGVK) == 0 {
		return -1, &ConflictError{Message: fmt.Sprintf(
			"sidecar selector for: %s matches no object in the manifest%s",
			doc.For, scopeHint(doc, targets))}
	}
	if len(byGVK) == 1 {
		if doc.Name != "" || doc.Namespace != "" {
			return -1, &ConflictError{Message: fmt.Sprintf(
				"sidecar selector for: %s already selects exactly one object; name:/namespace: is redundant and must be removed",
				doc.For)}
		}
		return byGVK[0], nil
	}

	// Ambiguous by GVK alone: name:/namespace: are required to narrow it.
	if doc.Name == "" && doc.Namespace == "" {
		return -1, &ConflictError{Message: fmt.Sprintf(
			"sidecar selector for: %s is ambiguous, matches %s — add name: or namespace: to narrow it",
			doc.For, describeCandidates(targets, byGVK))}
	}

	matched := filterCandidates(targets, byGVK, doc.Name, doc.Namespace, doc.APIVersion, doc.Kind)
	if len(matched) == 0 {
		return -1, &ConflictError{Message: fmt.Sprintf(
			"sidecar selector for: %s name: %q namespace: %q matches no object in the manifest",
			doc.For, doc.Name, doc.Namespace)}
	}
	if len(matched) > 1 {
		return -1, &ConflictError{Message: fmt.Sprintf(
			"sidecar selector for: %s is still ambiguous after narrowing, matches %s",
			doc.For, describeCandidates(targets, matched))}
	}

	if doc.Name != "" && doc.Namespace != "" {
		byNameOnly := filterCandidates(targets, byGVK, doc.Name, "", doc.APIVersion, doc.Kind)
		byNamespaceOnly := filterCandidates(targets, byGVK, "", doc.Namespace, doc.APIVersion, doc.Kind)
		switch {
		case len(byNameOnly) == 1:
			return -1, &ConflictError{Message: fmt.Sprintf(
				"sidecar selector for: %s name: %q already selects exactly one object; namespace: %q is redundant and must be removed",
				doc.For, doc.Name, doc.Namespace)}
		case len(byNamespaceOnly) == 1:
			return -1, &ConflictError{Message: fmt.Sprintf(
				"sidecar selector for: %s namespace: %q already selects exactly one object; name: %q is redundant and must be removed",
				doc.For, doc.Namespace, doc.Name)}
		}
	}

	return matched[0], nil
}

// allIndices returns [0, n).
func allIndices(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// filterCandidates returns the subset of candidateIdx (indices into
// targets) whose GVK matches apiVersion/kind and whose name/namespace
// match name/namespace when those are non-empty.
func filterCandidates(targets []ObjectID, candidateIdx []int, name, namespace, apiVersion, kind string) []int {
	var out []int
	for _, i := range candidateIdx {
		t := targets[i]
		if t.APIVersion != apiVersion || t.Kind != kind {
			continue
		}
		if name != "" && t.Name != name {
			continue
		}
		if namespace != "" && t.Namespace != namespace {
			continue
		}
		out = append(out, i)
	}
	return out
}

// describeCandidates renders a sorted, human-readable list of targets at
// idx, for an ambiguity error — "namespace/name" for a namespaced object,
// bare "name" for a cluster-scoped one.
func describeCandidates(targets []ObjectID, idx []int) string {
	parts := make([]string, 0, len(idx))
	for _, i := range idx {
		t := targets[i]
		if t.Namespace != "" {
			parts = append(parts, fmt.Sprintf("%s/%s", t.Namespace, t.Name))
		} else {
			parts = append(parts, t.Name)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// scopeHint looks for a target of the same Kind whose API group is the
// ".m." counterpart of doc's — the Crossplane v2 cluster/namespaced scope
// split — and if it finds one, names that group in the returned suffix. It
// returns "" when no such target exists, so the caller's message degrades
// to a plain "matches no object" with no unfounded suggestion.
func scopeHint(doc Doc, targets []ObjectID) string {
	group, version, ok := splitGroupVersion(doc.APIVersion)
	if !ok {
		return ""
	}
	altGroup, ok := alternateScopeGroup(group)
	if !ok {
		return ""
	}
	altAPIVersion := altGroup + "/" + version
	for _, t := range targets {
		if t.Kind == doc.Kind && t.APIVersion == altAPIVersion {
			return fmt.Sprintf(
				" (the manifest has apiVersion %s instead — Crossplane v2 cluster-scoped and namespaced "+
					"variants of the same kind live in different API groups, distinguished by the \".m.\" infix)",
				altAPIVersion)
		}
	}
	return ""
}

// splitGroupVersion splits an apiVersion into its group and version at the
// LAST "/", matching splitFor's own convention. ok is false when apiVersion
// carries no "/" at all (a core-group apiVersion like "v1" has no group to
// vary a scope suffix on).
func splitGroupVersion(apiVersion string) (group, version string, ok bool) {
	idx := strings.LastIndex(apiVersion, "/")
	if idx < 0 {
		return "", "", false
	}
	return apiVersion[:idx], apiVersion[idx+1:], true
}

// alternateScopeGroup returns the OTHER scope's API group for group — the
// namespaced ".m." variant of a cluster group, or the cluster variant of a
// namespaced one — and false when group carries neither suffix (a
// core-group kind has no scope-split counterpart).
func alternateScopeGroup(group string) (string, bool) {
	if strings.HasSuffix(group, namespacedGroupSuffix) {
		return strings.TrimSuffix(group, namespacedGroupSuffix) + clusterGroupSuffix, true
	}
	if strings.HasSuffix(group, clusterGroupSuffix) {
		return strings.TrimSuffix(group, clusterGroupSuffix) + namespacedGroupSuffix, true
	}
	return "", false
}
