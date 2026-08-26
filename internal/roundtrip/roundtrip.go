// Package roundtrip answers a question source cannot: whether a field
// round-trips faithfully through a provider's backend. The backend may
// default a field the caller never sent, canonicalise a value, or silently
// drop something the client submitted — none of that is knowable from the
// generated types, only by observation. This package walks a resource's CRD
// OpenAPI schema to enumerate every declared spec.forProvider field, then
// classifies each one against a live object's spec.forProvider value and its
// status.atProvider mirror.
//
// Everything here is read-only and pure: DiffReport takes an already-parsed
// CRD and an already-fetched object and never touches a cluster. Callers
// that need the live object (the roundtrip-diff subcommand, and
// converge-all's inline findings) fetch it themselves and hand it in.
package roundtrip

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Five classifications, applied per field path. A field never populated on
// either side is not reported at all — there is nothing to observe yet.
const (
	// ClassEqual: present on both sides, same value.
	ClassEqual = "equal"
	// ClassValueChanged: present on both sides, different value.
	ClassValueChanged = "value-changed"
	// ClassPresentInSpecAbsentFromMirror: the client set it; the mirror
	// never reports it — a potential mirror gap.
	ClassPresentInSpecAbsentFromMirror = "present-in-spec-absent-from-mirror"
	// ClassDefaultedByServer: the client never set it; the backend
	// supplied a value anyway.
	ClassDefaultedByServer = "defaulted-by-server"
	// ClassPresentInMirrorAbsentFromSpec: the field exists only in the
	// atProvider schema (no forProvider counterpart at all), and the
	// mirror actually reports a value for it.
	ClassPresentInMirrorAbsentFromSpec = "present-in-mirror-absent-from-spec"
)

// Row records one field path's classification against a single live object.
//
// SpecFound/MirrorFound distinguish "absent" from "present but zero" —
// false, 0, "", and {} are all legitimate values a caller may have set, and
// collapsing them to Go's zero value for the absent case would make a
// genuinely-set zero indistinguishable from a field nobody ever touched.
type Row struct {
	Path           string
	Classification string
	SpecValue      interface{}
	SpecFound      bool
	MirrorValue    interface{}
	MirrorFound    bool
	// Immutable reports whether Path carries a CEL "self == oldSelf"
	// validation marker — on its own schema node, or inherited from an
	// ancestor object node whose whole subtree is marked immutable — in the
	// served CRD's spec.forProvider schema. Always false for a
	// present-in-mirror-absent-from-spec row: that classification has no
	// forProvider counterpart to carry a marker at all. See immutablePaths.
	Immutable bool
}

// DiffReport classifies every declared spec.forProvider field of crd against
// the live object obj — decoded JSON, the same shape a `kubectl get -o json`
// round trip through encoding/json produces (map[string]interface{} for
// objects, []interface{} for arrays, and JSON scalars).
//
// A leaf field is a schema node that either is not `type: object`, or is
// `type: object` with no declared properties (the shape generated for an
// empty oneof-selector struct, e.g. a bare `{type: object}` marker). Neither
// shape is descended into further — an array's element schema is never
// walked, so a nested field inside an array item is compared only as part
// of the WHOLE array value at the array's own leaf path.
func DiffReport(crd map[string]interface{}, obj map[string]interface{}) ([]Row, error) {
	schema, err := servedSchema(crd)
	if err != nil {
		return nil, err
	}
	fpSchema, err := fieldSchema(schema, "spec", "forProvider")
	if err != nil {
		return nil, fmt.Errorf("locating spec.forProvider schema: %w", err)
	}
	apSchema, err := fieldSchema(schema, "status", "atProvider")
	if err != nil {
		return nil, fmt.Errorf("locating status.atProvider schema: %w", err)
	}

	fpPaths := leafPaths(fpSchema, "")
	sort.Strings(fpPaths)
	apPaths := leafPaths(apSchema, "")
	immutable := immutablePaths(fpSchema)

	specForProvider, _ := getValue(obj, "spec.forProvider")
	mirrorAtProvider, _ := getValue(obj, "status.atProvider")
	spMap, _ := specForProvider.(map[string]interface{})
	if spMap == nil {
		spMap = map[string]interface{}{}
	}
	mrMap, _ := mirrorAtProvider.(map[string]interface{})
	if mrMap == nil {
		mrMap = map[string]interface{}{}
	}

	var rows []Row
	fpSet := make(map[string]bool, len(fpPaths))
	for _, path := range fpPaths {
		fpSet[path] = true
		sv, sf := getValue(spMap, path)
		mv, mf := getValue(mrMap, path)
		cls, ok := classify(sf, sv, mf, mv)
		if !ok {
			continue
		}
		rows = append(rows, Row{
			Path: path, Classification: cls, SpecValue: sv, SpecFound: sf, MirrorValue: mv, MirrorFound: mf,
			Immutable: immutable[path],
		})
	}

	// Fields that exist only in the atProvider schema — no forProvider
	// counterpart at all — are a separate class: the mirror is reporting
	// something the spec was never able to express in the first place.
	mirrorOnly := make([]string, 0, len(apPaths))
	for _, path := range apPaths {
		if !fpSet[path] {
			mirrorOnly = append(mirrorOnly, path)
		}
	}
	sort.Strings(mirrorOnly)
	for _, path := range mirrorOnly {
		mv, mf := getValue(mrMap, path)
		if !mf {
			continue
		}
		rows = append(rows, Row{Path: path, Classification: ClassPresentInMirrorAbsentFromSpec, MirrorValue: mv, MirrorFound: true})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, nil
}

// classify applies the five-way classification described on the package
// doc comment. ok is false when neither side has ever populated the field —
// nothing to report.
func classify(specFound bool, specVal interface{}, mirrorFound bool, mirrorVal interface{}) (classification string, ok bool) {
	switch {
	case specFound && mirrorFound:
		if reflect.DeepEqual(specVal, mirrorVal) {
			return ClassEqual, true
		}
		return ClassValueChanged, true
	case specFound && !mirrorFound:
		return ClassPresentInSpecAbsentFromMirror, true
	case !specFound && mirrorFound:
		return ClassDefaultedByServer, true
	default:
		return "", false
	}
}

// leafPaths yields every leaf field path in an OpenAPI object schema node
// (decoded as the map[string]interface{}/[]interface{}/scalar shape
// gopkg.in/yaml.v3 already produces for a mapping node). See DiffReport's
// doc comment for what counts as a leaf.
func leafPaths(schema interface{}, prefix string) []string {
	m, ok := schema.(map[string]interface{})
	if !ok {
		if prefix != "" {
			return []string{prefix}
		}
		return nil
	}

	typ, _ := m["type"].(string)
	props, hasProps := m["properties"].(map[string]interface{})
	if typ == "object" && hasProps && len(props) > 0 {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)

		var out []string
		for _, name := range names {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			out = append(out, leafPaths(props[name], path)...)
		}
		return out
	}

	if prefix != "" {
		return []string{prefix}
	}
	return nil
}

// getValue resolves a dotted field path against a nested map. The bool
// result is false the moment any path segment is missing or the current
// node is not a map — so a field pruned by the API server (or never set) is
// distinguished from one genuinely set to a zero value like false, 0, or {}.
func getValue(obj map[string]interface{}, path string) (interface{}, bool) {
	var cur interface{} = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, exists := m[part]
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// servedSchema returns the openAPIV3Schema of the served CRD version. Every
// version this fleet ships marks exactly one version served=true; fall back
// to the first version if none does (defensive, not expected in practice).
func servedSchema(crd map[string]interface{}) (map[string]interface{}, error) {
	spec, _ := crd["spec"].(map[string]interface{})
	if spec == nil {
		return nil, fmt.Errorf("CRD has no spec")
	}
	versions, _ := spec["versions"].([]interface{})
	if len(versions) == 0 {
		return nil, fmt.Errorf("CRD has no spec.versions")
	}

	for _, v := range versions {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if served, _ := vm["served"].(bool); served {
			return schemaOf(vm)
		}
	}
	vm, ok := versions[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("CRD spec.versions[0] is not an object")
	}
	return schemaOf(vm)
}

func schemaOf(version map[string]interface{}) (map[string]interface{}, error) {
	schema, _ := version["schema"].(map[string]interface{})
	if schema == nil {
		return nil, fmt.Errorf("CRD version has no schema")
	}
	oapi, ok := schema["openAPIV3Schema"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("CRD version has no schema.openAPIV3Schema")
	}
	return oapi, nil
}

// fieldSchema navigates schema["properties"][path[0]]["properties"][path[1]]...,
// mirroring how diff_report locates spec.forProvider / status.atProvider in
// the served schema.
func fieldSchema(schema map[string]interface{}, path ...string) (map[string]interface{}, error) {
	cur := schema
	for _, p := range path {
		props, _ := cur["properties"].(map[string]interface{})
		if props == nil {
			return nil, fmt.Errorf("no properties map while looking for %q", p)
		}
		next, ok := props[p].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("field %q not found", p)
		}
		cur = next
	}
	return cur, nil
}

// FindCRD scans root/package/crds/*.yaml for the CRD whose spec.group +
// spec.names.kind match apiVersion's group and kind. Returns (nil, "") when
// no matching file is found — including when the directory itself does not
// exist, or a file cannot be parsed — because a caller only ever wants to
// know whether a report can be produced, not why one particular file failed
// to contribute. This is read-only: every file is opened only to inspect its
// group/kind.
func FindCRD(root, apiVersion, kind string) (crd map[string]interface{}, plural string) {
	group := apiVersion
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		group = apiVersion[:idx]
	}

	dir := filepath.Join(root, "package", "crds")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ""
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		// #nosec G304 -- name is one of a directory listing this same
		// function just enumerated, not attacker-controlled input.
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc map[string]interface{}
		if err := yaml.Unmarshal(data, &doc); err != nil || doc == nil {
			continue
		}
		if k, _ := doc["kind"].(string); k != "CustomResourceDefinition" {
			continue
		}
		spec, _ := doc["spec"].(map[string]interface{})
		if spec == nil {
			continue
		}
		specGroup, _ := spec["group"].(string)
		namesMap, _ := spec["names"].(map[string]interface{})
		specKind, _ := namesMap["kind"].(string)
		if specGroup == group && specKind == kind {
			p, _ := namesMap["plural"].(string)
			return doc, p
		}
	}
	return nil, ""
}

// FormatReport renders a full human-readable report, headed by the
// resource's kind/name — the shape the standalone roundtrip-diff subcommand
// prints for each manifest it is given.
func FormatReport(kind, name string, rows []Row) string {
	lines := append([]string{fmt.Sprintf("roundtrip-diff: %s/%s", kind, name)}, FormatFindingsLines(rows)...)
	return strings.Join(lines, "\n")
}

// FormatFindingsLines renders one line per row plus a trailing tally line,
// with NO leading kind/name header — the shape converge-all's summary
// inlines immediately under a failing target's own verdict line, where the
// label is already printed once and a second header would be redundant.
func FormatFindingsLines(rows []Row) []string {
	counts := make(map[string]int, len(rows))
	lines := make([]string, 0, len(rows)+1)
	for _, r := range rows {
		counts[r.Classification]++
		lines = append(lines, fmt.Sprintf("%-38s %s spec=%s mirror=%s",
			r.Classification, r.Path, formatValue(r.SpecFound, r.SpecValue), formatValue(r.MirrorFound, r.MirrorValue)))
	}
	lines = append(lines, fmt.Sprintf("-- %d field(s) classified (%s)", len(rows), summarizeCounts(counts)))
	return lines
}

// formatValue renders a classified value for display. An absent value
// (found == false) always renders as the same literal token regardless of
// classification, so a reader scanning the column can tell "never set" apart
// from a value that happens to marshal to the same text.
func formatValue(found bool, v interface{}) string {
	if !found {
		return "<absent>"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// summarizeCounts renders a classification tally sorted by classification
// name, e.g. "defaulted-by-server=3, equal=16".
func summarizeCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}
