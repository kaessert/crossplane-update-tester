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
// "type: object" node with NO declared properties that declares arbitrary
// member keys are allowed — additionalProperties-shaped or
// x-kubernetes-preserve-unknown-fields-shaped. A "type: object" node that DOES
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
// clear-direction coverage — one of THREE states, never two: covered,
// uncovered, or ineligible. Ineligible is distinct from uncovered: it
// means the leaf's removal direction cannot be exercised AT ALL (see
// IneligibilityReason), so it is excluded from the container-clear
// denominator entirely rather than counted as a gap to close. Covered is
// always false when Ineligible is true — ContainerClearCoverage refuses to
// produce a finding that is both (see its own doc comment).
//
// REPORT-ONLY, by construction: this type carries no field a caller could
// fold into a process exit code without writing new code to do so, and no
// function in this file returns a bool or error that means "fail" for the
// covered/uncovered split. Six of the fleet's seven providers measure zero
// clear-direction coverage today; enforcing this would break every one of
// their E2E runs the moment it shipped. Flipping this from advisory to
// enforcing is a distinct, deliberate, later act — not a side effect of
// this type existing.
type ContainerClearFinding struct {
	Path    string
	Shape   Shape
	Covered bool
	// Ineligible is true when this leaf's removal direction can never be
	// exercised — see IneligibilityReason. An ineligible leaf is excluded
	// from the container-clear denominator (see containerLeafSummary) but
	// is still reported here, with Reason set, so an exclusion is always
	// visible and auditable rather than silently dropped.
	Ineligible bool
	// Reason explains why Ineligible is true. Empty when Ineligible is
	// false.
	Reason IneligibilityReason
	Detail string
}

// ContainerClearCoverage checks every declared container-typed leaf under
// crd's spec.forProvider schema against m's own Tests. A leaf is covered
// when ANY of:
//
//   - some entry's Clear list names it EXACTLY — the whole-field tombstone
//     shape ({"spec":{"forProvider":{"<leaf>":null}}}), folded into the
//     same merge patch as that entry's own field (see
//     manifest.UpdateTest.Clear and runner.buildMergePatch); or
//   - some entry's WithValues map names it EXACTLY with an empty LIST
//     literal ({"spec":{"forProvider":{"<leaf>":[]}}}) — RFC-7386 treats an
//     empty list as a wholesale replacement at that key, identical in
//     effect to the value: [] self-tombstone case below; only the LIST
//     shape qualifies — a Map-shaped leaf can never be emptied this way,
//     see the empty-MAP note on selfTombstoned. A null literal is NOT a
//     credit route here: WithValues is documented to carry a real,
//     non-null value (manifest.UpdateTest.WithValues), but nothing
//     currently rejects a null one, and such an entry is credited by
//     neither this route nor the Clear route above; or
//   - some entry's Clear list names an ANCESTOR of the leaf's dotted path
//     — the same whole-field tombstone shape, but applied to an object
//     several levels above the leaf ({"spec":{"forProvider":{"<ancestor>":null}}}).
//     RFC-7386 merge-patch semantics remove the ENTIRE subtree under a
//     nulled object member, so a tombstone on "allowList" genuinely clears
//     "allowList.ipPrefixSet" beneath it even though no entry ever names
//     that nested path directly; or
//   - the entry directly testing the leaf itself carries a Value whose
//     top-level member map contains at least one explicit null — the
//     per-key removal shape ({"spec":{"forProvider":{"<leaf>":{"a":"1","b":null}}}}); or
//   - the entry directly testing the leaf itself patches the leaf's OWN
//     value to a tombstone — either an explicit `value: null`
//     (manifest.UpdateTest.ValueExplicit, credited for a leaf with no
//     sibling top-level field able to host a Clear entry naming it) or an
//     empty LIST value (`value: []`) — the one non-null shape RFC-7386
//     still treats as wholesale replacement, because a list is never
//     merged member-by-member. An empty MAP value (`value: {}`) is NOT a
//     tombstone under RFC-7386 and is deliberately never credited here —
//     see selfTombstoned for why.
//
// The per-key-removal and self-tombstone checks stay exact-path-only by
// construction: both only ever look at the entry testing the leaf's own
// field, and have no ancestor-walk analogue — nulling or emptying a
// different field's value cannot remove a descendant of a different leaf.
//
// Neither the exact-path nor the ancestor-tombstone case is inferred
// beyond ordinary merge-patch semantics: a leaf with no entry at all, and
// no ancestor entry, or an entry whose Value simply omits a key rather
// than nulling it, is NOT covered — an RFC-7386 merge patch treats an
// omitted key as "leave alone", never as "remove", which is exactly the
// blind spot this check exists to close.
//
// A leaf classified INELIGIBLE (see IneligibilityReason and
// classifyIneligibility) skips all of the above: its removal direction can
// never be exercised at all, so it is reported with Ineligible set and
// Covered always false — see this function's own contradiction check below
// for what happens when an existing manifest entry disagrees. The
// reference-resolution reason is exempt from that error (see the check's
// own comment): closing a leaf ANOTHER field's ancestor tombstone
// incidentally sweeps up is never rejected by admission — there is no CEL
// rule guarding a reference-resolution field — so it is not evidence the
// predicate is wrong, only that crossplane-runtime already discarded a
// field the manifest happened to also mention.
func ContainerClearCoverage(crd map[string]interface{}, m *manifest.Manifest) ([]ContainerClearFinding, error) {
	leaves, err := DeclaredContainerLeaves(crd)
	if err != nil {
		return nil, err
	}

	ineligible, err := classifyIneligibility(crd, leaves)
	if err != nil {
		return nil, err
	}

	clearedSiblings := make(map[string]bool)
	withValuesEmptyList := make(map[string]bool)
	perKeyNulled := make(map[string]bool)
	selfByField := make(map[string]manifest.UpdateTest, len(m.Tests))
	for _, t := range m.Tests {
		for _, sibling := range t.Clear {
			clearedSiblings[sibling] = true
		}
		for sibling, v := range t.WithValues {
			if l, ok := v.([]interface{}); ok && len(l) == 0 {
				withValuesEmptyList[sibling] = true
			}
		}
		if hasNestedNullMember(t.Value) {
			perKeyNulled[t.Field] = true
		}
		selfByField[t.Field] = t
	}

	findings := make([]ContainerClearFinding, 0, len(leaves))
	for _, leaf := range leaves {
		covered, detail := coverageFor(leaf, clearedSiblings, withValuesEmptyList, perKeyNulled, selfByField)

		reason, isIneligible := ineligible[leaf.Path]
		if !isIneligible {
			findings = append(findings, ContainerClearFinding{Path: leaf.Path, Shape: leaf.Shape, Covered: covered, Detail: detail})
			continue
		}

		if reason == ReasonReferenceResolution {
			// Never a contradiction, covered or not: a reference-resolution
			// field carries no CEL rule guarding it, so nothing a manifest
			// entry does to it — including an ancestor tombstone that
			// incidentally sweeps this leaf up along with a genuinely
			// tested sibling — is ever rejected by admission, and none of
			// it is ever observable at the backend either way. A "covered"
			// signal here is not evidence the predicate disagrees with the
			// manifest; it is crossplane-runtime having already discarded
			// this field regardless of what the merge patch says.
			findings = append(findings, ContainerClearFinding{
				Path: leaf.Path, Shape: leaf.Shape, Ineligible: true, Reason: reason,
				Detail: string(reason),
			})
			continue
		}

		// ReasonRequiredByCELMap, or a LIST leaf whose empty-list route is
		// ALSO closed: the same x-kubernetes-validations rule that makes
		// has() fail on a null patch would reject the EXACT merge patch a
		// "covered" signal here claims succeeded, so a manifest entry
		// disagreeing with this is a contradiction in the predicate
		// itself, not something to silently resolve one way or the other.
		if covered {
			return nil, fmt.Errorf(
				"container leaf %q is classified BOTH ineligible (%s) and covered (%s) — "+
					"the ineligibility predicate and the manifest's own coverage disagree; fix the predicate or the manifest, do not silently prefer one",
				leaf.Path, reason, detail)
		}
		findings = append(findings, ContainerClearFinding{
			Path: leaf.Path, Shape: leaf.Shape, Ineligible: true, Reason: reason,
			Detail: string(reason),
		})
	}
	return findings, nil
}

// coverageFor applies the covered/uncovered decision ContainerClearCoverage
// documents to one leaf, independent of whether that leaf turns out to be
// ineligible — kept separate so the ineligible/covered contradiction check
// above can compute both without duplicating this logic.
func coverageFor(leaf ContainerLeaf, clearedSiblings, withValuesEmptyList, perKeyNulled map[string]bool, selfByField map[string]manifest.UpdateTest) (covered bool, detail string) {
	ancestor, ancestorCleared := clearedAncestor(leaf.Path, clearedSiblings)
	self, hasSelf := selfByField[leaf.Path]
	switch {
	case clearedSiblings[leaf.Path]:
		return true, "whole-field tombstone: named in a sibling entry's clear: list"
	case leaf.Shape == ShapeList && withValuesEmptyList[leaf.Path]:
		return true, "whole-field tombstone: named in a sibling entry's withValues: map with an empty list literal (RFC-7386 wholesale replacement, equivalent to a null tombstone)"
	case ancestorCleared:
		return true, fmt.Sprintf("whole-subtree tombstone: ancestor %q named in a sibling entry's clear: list removes this leaf too", ancestor)
	case perKeyNulled[leaf.Path]:
		return true, "per-key removal: this field's own tested value nulls a member key"
	case hasSelf && selfTombstoned(self, leaf.Shape):
		return true, "whole-field tombstone: this field's own tested value is an explicit `value: null` or an empty container, with no sibling field needed to host it"
	default:
		return false, "no clear:, whole-field tombstone, whole-subtree tombstone, per-key removal, or self-tombstone exercises this container leaf's removal direction"
	}
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

// selfTombstoned reports whether entry t's OWN tested value removes the
// WHOLE leaf shape rather than describing a value for it — either an
// explicit `value: null` (manifest.UpdateTest.ValueExplicit; see that
// field's doc comment for why an explicit null cannot be told apart from
// t.Value simply being unset without it) or an empty LIST value
// (`value: []`) on a List-shaped leaf.
//
// Both are equivalent, once applied, to the whole-field tombstone a Clear
// entry on a SIBLING field would produce
// ({"spec":{"forProvider":{"<field>":null}}} or an empty list at that same
// key) — but authored directly on the leaf's own entry, so a leaf with no
// sibling top-level field able to host a Clear entry (see
// manifest.ValidateClear's rejection of clear naming its own field) can
// still earn clear-direction coverage.
//
// An empty LIST is credited here even though it is not byte-identical to a
// null tombstone: RFC 7386 merge-patch semantics apply a non-object VALUE
// — a scalar, or a list — by wholesale replacement at that key, exactly as
// they apply null, because a list is never merged member-by-member. An
// empty MAP value is the opposite case and is deliberately NEVER credited:
// RFC 7396 recurses into an object-valued patch member and merges it
// key-by-key against the live object, so `{"<field>":{}}` names no member
// to remove and the live map survives the patch completely unchanged. The
// only way to remove every member of a Map-shaped leaf under RFC 7386 is
// the explicit-null route above; shape gates the empty-list check so a
// Map-shaped leaf's empty MAP value is never mistaken for it.
func selfTombstoned(t manifest.UpdateTest, shape Shape) bool {
	if t.Value == nil {
		return t.ValueExplicit
	}
	if shape == ShapeList {
		v, ok := t.Value.([]interface{})
		return ok && len(v) == 0
	}
	return false
}

// containerLeafSummary renders a one-line coverage tally, e.g.
// "3/9 container leaves carry clear-direction coverage" — the shape a CLI
// report prints alongside the per-leaf detail. Ineligible leaves are
// excluded from BOTH the numerator and the denominator — they are not
// gaps to close, they cannot be exercised at all — and, when any exist,
// their count is appended so an exclusion is never silently invisible in
// the one-line tally.
func containerLeafSummary(findings []ContainerClearFinding) string {
	covered := 0
	eligible := 0
	ineligible := 0
	for _, f := range findings {
		if f.Ineligible {
			ineligible++
			continue
		}
		eligible++
		if f.Covered {
			covered++
		}
	}
	if ineligible == 0 {
		return fmt.Sprintf("%d/%d container leaves carry clear-direction coverage", covered, eligible)
	}
	return fmt.Sprintf("%d/%d container leaves carry clear-direction coverage (%d ineligible, excluded)", covered, eligible, ineligible)
}
