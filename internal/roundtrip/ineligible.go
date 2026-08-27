package roundtrip

import (
	"regexp"
	"strings"
)

// IneligibilityReason names one of the two structural reasons a declared
// container leaf's removal direction can never be exercised at all — see
// classifyIneligibility's own doc comment for what each is derived from.
// There are deliberately only two: "no eligible sibling to hang clear: on"
// is NOT a third reason (see ContainerClearFinding's own doc comment) —
// that is a tooling gap the self-tombstone routes already close, not a
// structural impossibility.
type IneligibilityReason string

const (
	// ReasonReferenceResolution reports that the leaf is
	// crossplane-runtime's own cross-resource reference-resolution
	// plumbing — a Selector's matchLabels map, or a *Refs list of
	// Reference/NamespacedReference items — resolved entirely by
	// crossplane-runtime before a request ever reaches the provider. It
	// can never appear in status.atProvider and a clear-direction test
	// against it would only ever exercise kube-apiserver's own
	// merge-patch handling, never anything the provider does.
	ReasonReferenceResolution IneligibilityReason = "reference-resolution field: resolved by crossplane-runtime before the request reaches the provider; never mirrored in status.atProvider"
	// ReasonRequiredByCEL reports that an x-kubernetes-validations rule
	// requires this leaf whenever the object's managementPolicies is at
	// its schema default. Nulling the leaf is rejected by admission
	// before the provider ever sees the patch, so no clear-direction test
	// can ever reach the backend.
	ReasonRequiredByCEL IneligibilityReason = "required by a CEL validation rule under the default managementPolicies; admission rejects nulling it"
)

// classifyIneligibility derives, for every leaf in leaves, whether its
// removal direction can ever be exercised against crd's schema at all.
// Re-derived from the schema on EVERY call — nothing here is a hardcoded
// list, a per-provider config, or an annotation a human must remember to
// update, so a CRD change that removes the shape or the rule puts the leaf
// back in the denominator automatically on the very next run.
//
// Only two reasons are ever produced (see IneligibilityReason); a leaf
// matching neither is left out of the returned map entirely; it is not
// ineligible.
func classifyIneligibility(crd map[string]interface{}, leaves []ContainerLeaf) (map[string]IneligibilityReason, error) {
	schema, err := servedSchema(crd)
	if err != nil {
		return nil, err
	}
	fpSchema, err := fieldSchema(schema, "spec", "forProvider")
	if err != nil {
		return nil, err
	}
	apSchema, err := fieldSchema(schema, "status", "atProvider")
	if err != nil {
		return nil, err
	}
	apPaths := make(map[string]bool)
	for _, p := range leafPaths(apSchema, "") {
		apPaths[p] = true
	}

	mpDefault, hasDefault := managementPoliciesDefault(schema)

	out := make(map[string]IneligibilityReason, len(leaves))
	for _, leaf := range leaves {
		if referenceResolutionShape(fpSchema, leaf, apPaths) {
			out[leaf.Path] = ReasonReferenceResolution
			continue
		}
		if hasDefault && requiredByManagementPolicies(schema, leaf.Path, mpDefault) {
			out[leaf.Path] = ReasonRequiredByCEL
		}
	}
	return out, nil
}

// referenceResolutionShape reports whether leaf is, by its SCHEMA SHAPE
// alone, crossplane-runtime's own reference-resolution plumbing — never by
// matching leaf.Path's name against a "Ref"/"Refs"/"Selector" suffix. A
// name-suffix match would silently exclude a genuine backend field that
// happens to end in one of those strings; the shape check below can only
// ever match the exact structure controller-tools generates for
// xpv1.Selector and xpv1.Reference/NamespacedReference.
//
// Two shapes are recognised, mirroring how DeclaredContainerLeaves itself
// finds a container leaf:
//
//   - a Map-shaped leaf whose immediate parent schema node is a
//     Selector — the "matchLabels" member DeclaredContainerLeaves reports
//     as a leaf in its own right, because the enclosing Selector object
//     descends into its properties like any other object node;
//   - a List-shaped leaf whose own "items" schema is a Reference /
//     NamespacedReference.
//
// leaf.Path is cross-checked against apPaths (status.atProvider's own leaf
// paths): a field genuinely mirrored back by the provider is never
// excluded here even if its schema happens to match one of the shapes
// above, because that mirroring is itself proof the field reaches the
// backend. Measured against provider-vultr and provider-vsphere: zero of
// the fields this function matches are ever present in status.atProvider,
// so the cross-check has so far never had occasion to veto a match — but
// it stays in force rather than being trusted away.
func referenceResolutionShape(fpSchema map[string]interface{}, leaf ContainerLeaf, apPaths map[string]bool) bool {
	if apPaths[leaf.Path] {
		return false
	}

	switch leaf.Shape {
	case ShapeMap:
		parent := parentPath(leaf.Path)
		if parent == "" {
			return false
		}
		parentSchema, ok := schemaAtPath(fpSchema, parent)
		if !ok {
			return false
		}
		return isSelectorShape(parentSchema)
	case ShapeList:
		leafSchema, ok := schemaAtPath(fpSchema, leaf.Path)
		if !ok {
			return false
		}
		items, _ := leafSchema["items"].(map[string]interface{})
		if items == nil {
			return false
		}
		return isReferenceItemShape(items)
	default:
		return false
	}
}

// parentPath returns path's immediate dotted-path ancestor, or "" when
// path has no ancestor (a top-level field).
func parentPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// schemaAtPath navigates root's "properties" tree along dotted, reusing
// fieldSchema's own navigation so this file agrees exactly with how
// DeclaredContainerLeaves and DiffReport resolve a dotted leaf path back
// to its schema node.
func schemaAtPath(root map[string]interface{}, dotted string) (map[string]interface{}, bool) {
	if dotted == "" {
		return root, true
	}
	node, err := fieldSchema(root, strings.Split(dotted, ".")...)
	if err != nil {
		return nil, false
	}
	return node, true
}

// isSelectorShape reports whether m is shaped exactly like
// crossplane-runtime's generated xpv1.Selector: an object whose declared
// properties are drawn ONLY from {matchControllerRef, matchLabels,
// policy}, with at least one of matchControllerRef/matchLabels present,
// matchLabels (when present) itself a free-form string map, and policy
// (when present) itself a resolution/resolve-shaped object. Every
// condition is checked against the node's own structure — nothing here
// reads the enclosing field's JSON name.
func isSelectorShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"matchControllerRef": true, "matchLabels": true, "policy": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}

	mlRaw, hasML := props["matchLabels"]
	_, hasMCR := props["matchControllerRef"]
	if !hasML && !hasMCR {
		return false
	}
	if hasML {
		ml, ok := mlRaw.(map[string]interface{})
		if !ok {
			return false
		}
		typ, _ := ml["type"].(string)
		_, hasAdd := ml["additionalProperties"]
		if typ != "object" || !hasAdd {
			return false
		}
	}
	if policyRaw, hasPolicy := props["policy"]; hasPolicy {
		policySchema, ok := policyRaw.(map[string]interface{})
		if !ok || !isPolicyShape(policySchema) {
			return false
		}
	}
	return true
}

// isReferenceItemShape reports whether m — the "items" schema of a
// declared List-shaped leaf — is shaped exactly like crossplane-runtime's
// generated xpv1.Reference or xpv1.NamespacedReference: an object whose
// declared properties are drawn ONLY from {name, namespace, policy}, with
// a required string-typed "name" member (namespace, when present, is the
// only difference between the cluster-scoped and namespaced generated
// shapes; both are reference-resolution plumbing either way).
func isReferenceItemShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"name": true, "namespace": true, "policy": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}
	nameSchema, ok := props["name"].(map[string]interface{})
	if !ok {
		return false
	}
	typ, _ := nameSchema["type"].(string)
	if typ != "string" {
		return false
	}
	if policyRaw, hasPolicy := props["policy"]; hasPolicy {
		policySchema, ok := policyRaw.(map[string]interface{})
		if !ok || !isPolicyShape(policySchema) {
			return false
		}
	}
	return true
}

// isPolicyShape reports whether m is shaped like crossplane-runtime's
// generated reference Policy: an object whose declared properties are
// drawn only from {resolution, resolve}.
func isPolicyShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"resolution": true, "resolve": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}
	return true
}

// managementPoliciesDefault resolves spec.managementPolicies' own schema
// DEFAULT — measured as ['*'] on every provider checked, but read from the
// schema rather than assumed, so a provider that ever ships a different
// default is handled correctly rather than silently mismeasured. The bool
// result is false when the field or its default is absent (a CRD schema
// that, for whatever reason, declares no default at all), in which case
// REASON 2 can never be derived — no default means there is no "resting,
// unedited state" to evaluate the CEL rule against.
func managementPoliciesDefault(schema map[string]interface{}) ([]string, bool) {
	mpSchema, err := fieldSchema(schema, "spec", "managementPolicies")
	if err != nil {
		return nil, false
	}
	raw, ok := mpSchema["default"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// reQuotedLiteral matches every single-quoted string literal in a CEL rule
// — the policy names ('*', 'Create', 'Update', ...) a managementPolicies
// membership guard tests against. Extracted straight from the rule's own
// text, never assumed to be any particular fixed set.
var reQuotedLiteral = regexp.MustCompile(`'([^']+)'`)

// requiredByManagementPolicies reports whether schema carries an
// x-kubernetes-validations rule — at the schema's own root, or on its
// "spec" node — requiring leafPath to be present whenever the object's
// managementPolicies intersects mpDefault. Both anchors are checked
// because a structural-schema CEL rule may be declared wherever `self`
// resolves the path it inspects: at the CRD root (`self.spec.forProvider.
// <leaf>`, `self` is the whole object — the shape measured on every
// provider checked) or on the spec node itself (`self.forProvider.<leaf>`,
// `self` is spec).
func requiredByManagementPolicies(schema map[string]interface{}, leafPath string, mpDefault []string) bool {
	if rootValidations, ok := schema["x-kubernetes-validations"].([]interface{}); ok {
		if leafRequiredByRule(rootValidations, "self.spec.forProvider."+leafPath, mpDefault) {
			return true
		}
	}
	specSchema, err := fieldSchema(schema, "spec")
	if err == nil {
		if specValidations, ok := specSchema["x-kubernetes-validations"].([]interface{}); ok {
			if leafRequiredByRule(specValidations, "self.forProvider."+leafPath, mpDefault) {
				return true
			}
		}
	}
	return false
}

// leafRequiredByRule scans validations (one schema node's own
// x-kubernetes-validations array) for a rule whose final OR-disjunct is
// exactly "has(wantPath)" and whose managementPolicies membership guard —
// parsed straight from the rule's own quoted literals, never assumed —
// shares at least one member with mpDefault. Sharing a member means the
// guard's negation (the disjunct immediately gating the has() clause)
// evaluates false at the object's resting, unedited managementPolicies
// value, so the OR reduces to demanding has(wantPath): the field is
// required right now, not merely under some non-default policy a caller
// might opt into later.
func leafRequiredByRule(validations []interface{}, wantPath string, mpDefault []string) bool {
	wantSuffix := "has(" + wantPath + ")"
	for _, v := range validations {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		rule, _ := vm["rule"].(string)
		if !strings.Contains(rule, "managementPolicies") {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(rule), wantSuffix) {
			continue
		}
		for _, m := range reQuotedLiteral.FindAllStringSubmatch(rule, -1) {
			if containsString(mpDefault, m[1]) {
				return true
			}
		}
	}
	return false
}

// containsString reports whether want is a member of list.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
