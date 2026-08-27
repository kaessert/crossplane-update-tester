package roundtrip

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// ContainerLeaf names one declared container-typed forProvider leaf — a
// schema node whose OWN JSON value is authored as a list or a free-form
// map, at the exact same leaf granularity DiffReport itself reports rows
// at (see leafPaths' own doc comment for what counts as a leaf; this walk
// deliberately agrees with it rather than re-deriving its own notion of
// "leaf").
type ContainerLeaf struct {
	Path  string
	Shape Shape // always ShapeList or ShapeMap
}

// DeclaredContainerLeaves walks crd's served spec.forProvider schema and
// returns every container-typed leaf: a "type: array" node, or a
// "type: object" node with NO declared properties (a free-form map —
// additionalProperties-shaped, x-kubernetes-preserve-unknown-fields-shaped,
// or a bare `{type: object}` marker). A "type: object" node that DOES
// declare properties is never itself a leaf (DiffReport descends into it
// instead, exactly as leafPaths does), so it is correctly excluded here
// too. A bare object marker with neither an additionalProperties key nor
// x-kubernetes-preserve-unknown-fields: true (the shape generated for an
// empty oneof selector struct) is also excluded: it has no member keys a
// clear direction could ever remove, so it is not a container-clear
// obligation at all.
func DeclaredContainerLeaves(crd map[string]interface{}) ([]ContainerLeaf, error) {
	schema, err := servedSchema(crd)
	if err != nil {
		return nil, err
	}
	fpSchema, err := fieldSchema(schema, "spec", "forProvider")
	if err != nil {
		return nil, fmt.Errorf("locating spec.forProvider schema: %w", err)
	}

	var out []ContainerLeaf
	collectContainerLeaves(fpSchema, "", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func collectContainerLeaves(schema interface{}, prefix string, out *[]ContainerLeaf) {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return
	}

	typ, _ := m["type"].(string)
	props, hasProps := m["properties"].(map[string]interface{})
	if typ == "object" && hasProps && len(props) > 0 {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			collectContainerLeaves(props[name], path, out)
		}
		return
	}

	if prefix == "" {
		// The root spec.forProvider node itself is never a container leaf.
		return
	}

	switch typ {
	case "array":
		*out = append(*out, ContainerLeaf{Path: prefix, Shape: ShapeList})
	case "object":
		_, hasAdditional := m["additionalProperties"]
		preservesUnknown, _ := m["x-kubernetes-preserve-unknown-fields"].(bool)
		if hasAdditional || preservesUnknown {
			*out = append(*out, ContainerLeaf{Path: prefix, Shape: ShapeMap})
		}
	}
}

// ContainerClearFinding records one declared container-typed leaf's
// clear-direction coverage.
//
// REPORT-ONLY, by construction: this type carries no field a caller could
// fold into a process exit code without writing new code to do so, and no
// function in this file returns a bool or error that means "fail". Six of
// the fleet's seven providers measure zero clear-direction coverage today;
// enforcing this would break every one of their E2E runs the moment it
// shipped. Flipping this from advisory to enforcing is a distinct,
// deliberate, later act — not a side effect of this type existing.
type ContainerClearFinding struct {
	Path    string
	Shape   Shape
	Covered bool
	Detail  string
}

// ContainerClearCoverage checks every declared container-typed leaf under
// crd's spec.forProvider schema against m's own Tests. A leaf is covered
// when ANY of:
//
//   - some entry's Clear list names it EXACTLY — the whole-field tombstone
//     shape ({"spec":{"forProvider":{"<leaf>":null}}}), folded into the
//     same merge patch as that entry's own field (see
//     manifest.UpdateTest.Clear and runner.buildMergePatch); or
//   - some entry's Clear list names an ANCESTOR of the leaf's dotted path
//     — the same whole-field tombstone shape, but applied to an object
//     several levels above the leaf ({"spec":{"forProvider":{"<ancestor>":null}}}).
//     RFC-7386 merge-patch semantics remove the ENTIRE subtree under a
//     nulled object member, so a tombstone on "allowList" genuinely clears
//     "allowList.ipPrefixSet" beneath it even though no entry ever names
//     that nested path directly; or
//   - the entry directly testing the leaf itself carries a Value whose
//     top-level member map contains at least one explicit null — the
//     per-key removal shape ({"spec":{"forProvider":{"<leaf>":{"a":"1","b":null}}}}).
//
// The per-key-removal check stays exact-path-only by construction: a
// per-key null only ever targets the field whose own Value carries it, and
// has no ancestor-walk analogue — nulling a member of some OTHER field's
// map value cannot remove a descendant of a different leaf.
//
// Neither the exact-path nor the ancestor-tombstone case is inferred
// beyond ordinary merge-patch semantics: a leaf with no entry at all, and
// no ancestor entry, or an entry whose Value simply omits a key rather
// than nulling it, is NOT covered — an RFC-7386 merge patch treats an
// omitted key as "leave alone", never as "remove", which is exactly the
// blind spot this check exists to close.
func ContainerClearCoverage(crd map[string]interface{}, m *manifest.Manifest) ([]ContainerClearFinding, error) {
	leaves, err := DeclaredContainerLeaves(crd)
	if err != nil {
		return nil, err
	}

	clearedSiblings := make(map[string]bool)
	perKeyNulled := make(map[string]bool)
	for _, t := range m.Tests {
		for _, sibling := range t.Clear {
			clearedSiblings[sibling] = true
		}
		if hasNestedNullMember(t.Value) {
			perKeyNulled[t.Field] = true
		}
	}

	findings := make([]ContainerClearFinding, 0, len(leaves))
	for _, leaf := range leaves {
		ancestor, ancestorCleared := clearedAncestor(leaf.Path, clearedSiblings)
		switch {
		case clearedSiblings[leaf.Path]:
			findings = append(findings, ContainerClearFinding{
				Path: leaf.Path, Shape: leaf.Shape, Covered: true,
				Detail: "whole-field tombstone: named in a sibling entry's clear: list",
			})
		case ancestorCleared:
			findings = append(findings, ContainerClearFinding{
				Path: leaf.Path, Shape: leaf.Shape, Covered: true,
				Detail: fmt.Sprintf("whole-subtree tombstone: ancestor %q named in a sibling entry's clear: list removes this leaf too", ancestor),
			})
		case perKeyNulled[leaf.Path]:
			findings = append(findings, ContainerClearFinding{
				Path: leaf.Path, Shape: leaf.Shape, Covered: true,
				Detail: "per-key removal: this field's own tested value nulls a member key",
			})
		default:
			findings = append(findings, ContainerClearFinding{
				Path: leaf.Path, Shape: leaf.Shape, Covered: false,
				Detail: "no clear:, whole-field tombstone, whole-subtree tombstone, or per-key removal exercises this container leaf's removal direction",
			})
		}
	}
	return findings, nil
}

// clearedAncestor reports whether some strict ancestor of the dotted path
// leafPath is a member of cleared — i.e. some entry's Clear list names an
// object several levels above leafPath, whose merge-patch null tombstone
// (RFC 7386) removes the whole subtree beneath it, leafPath included. It
// walks from leafPath's immediate parent up to its top-level segment,
// returning the first (deepest) ancestor found in cleared. leafPath itself
// is never checked here — the exact-path case is handled by the caller
// before this is reached.
func clearedAncestor(leafPath string, cleared map[string]bool) (string, bool) {
	segments := strings.Split(leafPath, ".")
	for i := len(segments) - 1; i > 0; i-- {
		candidate := strings.Join(segments[:i], ".")
		if cleared[candidate] {
			return candidate, true
		}
	}
	return "", false
}

// hasNestedNullMember reports whether v is a top-level member map (the
// shape a map-typed field's own Value takes) with at least one member
// explicitly mapped to nil.
func hasNestedNullMember(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	for _, mv := range m {
		if mv == nil {
			return true
		}
	}
	return false
}

// containerLeafSummary renders a one-line coverage tally, e.g.
// "3/9 container leaves carry clear-direction coverage" — the shape a CLI
// report prints alongside the per-leaf detail.
func containerLeafSummary(findings []ContainerClearFinding) string {
	covered := 0
	for _, f := range findings {
		if f.Covered {
			covered++
		}
	}
	return fmt.Sprintf("%d/%d container leaves carry clear-direction coverage", covered, len(findings))
}
