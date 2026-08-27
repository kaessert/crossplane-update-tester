package roundtrip

import (
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// containerClearFixtureCRD declares one of each shape under
// spec.forProvider: a scalar (name), a list (tags), a free-form map
// (labels), a bare empty-object marker with no additionalProperties
// (selector — must NOT be reported, it has no keys to ever clear), a
// nested object WITH declared properties (network.cidr, network.subnets)
// — the subnets leaf beneath it is a container leaf in its own right, but
// network itself is never a leaf (DiffReport descends into it) — and a
// free-form map declared via x-kubernetes-preserve-unknown-fields: true
// with NO additionalProperties key at all (helmValues — the raw-passthrough
// shape Crossplane/upjet-style codegen emits, e.g. provider-helm's `values`
// or provider-vclustercli's `helmValues`).
const containerClearFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: widgets.crossplane.io
  names:
    kind: Widget
    plural: widgets
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  name:
                    type: string
                  tags:
                    type: array
                    items:
                      type: string
                  labels:
                    type: object
                    additionalProperties:
                      type: string
                  selector:
                    type: object
                  network:
                    type: object
                    properties:
                      cidr:
                        type: string
                      subnets:
                        type: object
                        additionalProperties:
                          type: string
                  helmValues:
                    type: object
                    x-kubernetes-preserve-unknown-fields: true
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

func decodeCRD(t *testing.T, y string) map[string]interface{} {
	t.Helper()
	var crd map[string]interface{}
	if err := yaml.Unmarshal([]byte(y), &crd); err != nil {
		t.Fatalf("decoding fixture CRD: %v", err)
	}
	return crd
}

// TestDeclaredContainerLeaves confirms exactly the container-typed leaves
// are reported — no scalars, no bare empty-object markers, no
// object-with-properties ancestor — and that a nested container leaf
// (network.subnets) is found at its own leaf path, alongside a leaf
// declared via x-kubernetes-preserve-unknown-fields: true instead of
// additionalProperties (helmValues).
func TestDeclaredContainerLeaves(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)

	leaves, err := DeclaredContainerLeaves(crd)
	if err != nil {
		t.Fatalf("DeclaredContainerLeaves: %v", err)
	}

	got := make(map[string]Shape, len(leaves))
	for _, l := range leaves {
		got[l.Path] = l.Shape
	}
	want := map[string]Shape{
		"tags":            ShapeList,
		"labels":          ShapeMap,
		"network.subnets": ShapeMap,
		"helmValues":      ShapeMap,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredContainerLeaves = %+v, want %+v", got, want)
	}
}

// TestDeclaredContainerLeavesPreserveUnknownFieldsWithNoAdditionalProperties
// is the dedicated regression pin for the gap this fix closes: a
// "type: object" node carrying x-kubernetes-preserve-unknown-fields: true
// and NO additionalProperties key at all — the shape Crossplane/upjet-style
// codegen emits for a raw-passthrough field (provider-helm's `values`,
// provider-vclustercli's `helmValues`) — must be reported as a free-form
// map leaf, exactly like an additionalProperties-shaped node.
func TestDeclaredContainerLeavesPreserveUnknownFieldsWithNoAdditionalProperties(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)

	leaves, err := DeclaredContainerLeaves(crd)
	if err != nil {
		t.Fatalf("DeclaredContainerLeaves: %v", err)
	}

	var found *ContainerLeaf
	for i := range leaves {
		if leaves[i].Path == "helmValues" {
			found = &leaves[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("helmValues (x-kubernetes-preserve-unknown-fields: true, no additionalProperties) not found in %+v", leaves)
	}
	if found.Shape != ShapeMap {
		t.Errorf("helmValues.Shape = %v, want ShapeMap", found.Shape)
	}
}

// TestContainerClearCoverageWholeFieldTombstoneViaClearList confirms an
// entry whose Clear list names a container leaf credits that leaf as
// covered — the union-swap mechanism, folded into the SAME merge patch as
// the entry's own field.
func TestContainerClearCoverageWholeFieldTombstoneViaClearList(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", Clear: []string{"tags"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["tags"].Covered {
		t.Errorf("tags not covered by clear:, findings = %+v", findings)
	}
	if byPath["labels"].Covered {
		t.Errorf("labels reported covered with no entry naming it: %+v", byPath["labels"])
	}
	if byPath["network.subnets"].Covered {
		t.Errorf("network.subnets reported covered with no entry naming it: %+v", byPath["network.subnets"])
	}
}

// TestContainerClearCoverageAncestorTombstoneClearsNestedLeaf confirms the
// ancestor-walk case: an entry whose Clear list names an OBJECT ancestor
// several levels above a container leaf (never the leaf's own exact path)
// still credits that leaf as covered, because an RFC-7386 merge-patch null
// on the ancestor removes the whole subtree beneath it — the
// ServicePolicy/allowList shape measured live on provider-f5xc. Regression
// guard for the exact-path case (network.subnets is NOT named directly,
// only "network" is) plus a control leaf (tags) that stays uncovered since
// nothing names it or any of its ancestors.
func TestContainerClearCoverageAncestorTombstoneClearsNestedLeaf(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", Clear: []string{"network"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["network.subnets"].Covered {
		t.Errorf("network.subnets not covered by ancestor tombstone on network, findings = %+v", findings)
	}
	if byPath["tags"].Covered {
		t.Errorf("tags reported covered with no entry naming it or any ancestor: %+v", byPath["tags"])
	}
	if byPath["labels"].Covered {
		t.Errorf("labels reported covered with no entry naming it or any ancestor: %+v", byPath["labels"])
	}
}

// TestContainerClearCoverageAncestorWalkIsSegmentExact confirms the
// ancestor walk matches whole dotted-path SEGMENTS only, never a bare
// string prefix: a Clear entry naming "net" must NOT be treated as an
// ancestor of "network.subnets" just because "net" is a textual prefix of
// "network".
func TestContainerClearCoverageAncestorWalkIsSegmentExact(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", Clear: []string{"net"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	if byPath := findingsByPath(findings); byPath["network.subnets"].Covered {
		t.Errorf("network.subnets reported covered by a bare string-prefix match on \"net\": %+v", byPath["network.subnets"])
	}
}

// TestContainerClearCoveragePerKeyRemoval confirms an entry directly
// testing a map-typed leaf, whose own Value nulls one of its member keys,
// credits that leaf as covered via the per-key removal shape.
func TestContainerClearCoveragePerKeyRemoval(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "labels", Value: map[string]interface{}{"a": "1", "b": nil}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["labels"].Covered {
		t.Errorf("labels not covered by per-key removal, findings = %+v", findings)
	}
	if byPath["tags"].Covered {
		t.Errorf("tags reported covered with no entry naming it: %+v", byPath["tags"])
	}
}

// TestContainerClearCoverageOmittedKeyIsNotRemoval confirms the exact
// blind spot this check exists to close: an add-only value (every key
// present, nothing nulled) does NOT count as removal coverage — a JSON
// merge patch treats an omitted key as "leave alone", never as "remove".
func TestContainerClearCoverageOmittedKeyIsNotRemoval(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "labels", Value: map[string]interface{}{"a": "1", "b": "2"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	if findingsByPath(findings)["labels"].Covered {
		t.Error("an add-only value (no nulled member) was credited as clear-direction coverage")
	}
}

// TestContainerClearCoverageZeroCoverageStillProducesFindingsNoError is the
// pin the ticket requires: a manifest with NO clear-direction coverage at
// all — the measured state of six of the fleet's seven providers — still
// produces a clean, error-free, findings-only result. Nothing about this
// function's signature (no bool, no process-exit-relevant return) lets a
// caller turn this into a non-zero exit code without writing new code to
// do so.
func TestContainerClearCoverageZeroCoverageStillProducesFindingsNoError(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{Tests: nil} // no update-test entries at all

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage with zero coverage returned an error: %v — this must be report-only", err)
	}
	if len(findings) != 4 {
		t.Fatalf("got %d findings, want 4 (one per declared container leaf)", len(findings))
	}
	for _, f := range findings {
		if f.Covered {
			t.Errorf("finding %+v is Covered=true with no test entries at all", f)
		}
	}
	if got := containerLeafSummary(findings); got != "0/4 container leaves carry clear-direction coverage" {
		t.Errorf("containerLeafSummary = %q, want the 0/4 tally", got)
	}
}

// TestDeclaredContainerLeavesMissingSchemaIsAnError confirms a
// structurally invalid CRD is reported as an error rather than silently
// returning zero leaves — indistinguishable from "genuinely no
// container-typed fields".
func TestDeclaredContainerLeavesMissingSchemaIsAnError(t *testing.T) {
	if _, err := DeclaredContainerLeaves(map[string]interface{}{}); err == nil {
		t.Error("DeclaredContainerLeaves(empty CRD) returned nil error, want one")
	}
}

// TestContainerClearCoverageSelfTombstoneExplicitNull confirms the gap this
// fix closes: a leaf with NO other top-level sibling to host a `clear:`
// entry earns coverage from an explicit `value: null` on its OWN entry —
// GlobalFirewallRuleset.rules from the ticket that filed this fix, modelled
// here as the sole-top-level-field "tags" leaf on a manifest with no other
// tested field at all.
func TestContainerClearCoverageSelfTombstoneExplicitNull(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Value: nil, ValueExplicit: true},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["tags"].Covered {
		t.Errorf("tags not covered by an explicit value: null self-tombstone, findings = %+v", findings)
	}
	if byPath["labels"].Covered {
		t.Errorf("labels reported covered with no entry naming it: %+v", byPath["labels"])
	}
}

// TestContainerClearCoverageSelfTombstoneEmptyContainer confirms the second
// route: an empty LIST value (`value: []`) on a List leaf's own entry, with
// NO ValueExplicit and no clear: list at all, credits that leaf — the shape
// provider-infobloxnios' zone-forward.yaml already uses as a workaround for
// the same underlying gap. It also pins the companion negative: an empty
// MAP value (`value: {}`) on a Map leaf's own entry is NOT credited, since
// an RFC-7386 merge patch recurses into an object value and merges it
// member-by-member rather than replacing it, so `{}` removes nothing from
// a populated live map.
func TestContainerClearCoverageSelfTombstoneEmptyContainer(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Value: []interface{}{}},
			{Field: "labels", Value: map[string]interface{}{}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["tags"].Covered {
		t.Errorf("tags not covered by an empty-list self-tombstone, findings = %+v", findings)
	}
	if byPath["labels"].Covered {
		t.Errorf("labels reported covered by an empty-map value, but RFC-7386 merges an object value member-by-member and removes nothing: %+v", byPath["labels"])
	}
}

// TestContainerClearCoverageSelfTombstoneShapeMismatchNotCredited confirms
// selfTombstoned checks the CORRECT empty shape for the leaf: an empty MAP
// value on a List-typed leaf (or vice versa) does not type-assert to the
// leaf's own shape, so it must not be credited as a self-tombstone.
func TestContainerClearCoverageSelfTombstoneShapeMismatchNotCredited(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// tags is List-shaped; an empty MAP is the wrong shape.
			{Field: "tags", Value: map[string]interface{}{}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	if byPath := findingsByPath(findings); byPath["tags"].Covered {
		t.Errorf("tags reported covered by an empty value of the WRONG container shape: %+v", byPath["tags"])
	}
}

// TestContainerClearCoverageNonEmptyValueNotSelfTombstoned confirms an
// ordinary non-empty value on the leaf's own entry — the everyday "set a
// new value" test every field starts with — is not mistaken for a
// self-tombstone.
func TestContainerClearCoverageNonEmptyValueNotSelfTombstoned(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Value: []interface{}{"a", "b"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	if byPath := findingsByPath(findings); byPath["tags"].Covered {
		t.Errorf("tags reported covered by an ordinary non-empty value: %+v", byPath["tags"])
	}
}

// TestSelfTombstoned covers the helper in isolation, including the
// ValueExplicit-vs-absent distinction and both container shapes.
func TestSelfTombstoned(t *testing.T) {
	cases := map[string]struct {
		test  manifest.UpdateTest
		shape Shape
		want  bool
	}{
		"explicit null, list shape": {
			test:  manifest.UpdateTest{Value: nil, ValueExplicit: true},
			shape: ShapeList,
			want:  true,
		},
		"explicit null, map shape": {
			test:  manifest.UpdateTest{Value: nil, ValueExplicit: true},
			shape: ShapeMap,
			want:  true,
		},
		"absent value (not explicit)": {
			test:  manifest.UpdateTest{Value: nil, ValueExplicit: false},
			shape: ShapeList,
			want:  false,
		},
		"empty list, list shape": {
			test:  manifest.UpdateTest{Value: []interface{}{}},
			shape: ShapeList,
			want:  true,
		},
		"non-empty list, list shape": {
			test:  manifest.UpdateTest{Value: []interface{}{"a"}},
			shape: ShapeList,
			want:  false,
		},
		"empty map, map shape (not a tombstone under RFC 7386)": {
			test:  manifest.UpdateTest{Value: map[string]interface{}{}},
			shape: ShapeMap,
			want:  false,
		},
		"non-empty map, map shape": {
			test:  manifest.UpdateTest{Value: map[string]interface{}{"a": "1"}},
			shape: ShapeMap,
			want:  false,
		},
		"empty map, list shape (wrong shape)": {
			test:  manifest.UpdateTest{Value: map[string]interface{}{}},
			shape: ShapeList,
			want:  false,
		},
		"empty list, map shape (wrong shape)": {
			test:  manifest.UpdateTest{Value: []interface{}{}},
			shape: ShapeMap,
			want:  false,
		},
		"scalar value": {
			test:  manifest.UpdateTest{Value: "x"},
			shape: ShapeList,
			want:  false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := selfTombstoned(tc.test, tc.shape); got != tc.want {
				t.Errorf("selfTombstoned(%+v, %v) = %v, want %v", tc.test, tc.shape, got, tc.want)
			}
		})
	}
}

// applyMergePatch applies an RFC 7396 JSON Merge Patch to target and
// returns the result, following the RFC's own reference pseudocode
// exactly: a non-object patch value replaces target wholesale; an object
// patch value is merged into target key-by-key, with a null member
// removing the corresponding target key and any other value recursively
// merged. This is a byte-for-byte-equivalent-outcome reimplementation of
// the semantics kube-apiserver applies for `kubectl patch --type=merge`
// (the same semantics runner.buildMergePatch's output is applied under),
// used here to prove what a credited self-tombstone shape actually does to
// a populated live object rather than asserting it in prose.
func applyMergePatch(target, patch interface{}) interface{} {
	patchMap, ok := patch.(map[string]interface{})
	if !ok {
		return patch
	}
	merged := map[string]interface{}{}
	if targetMap, ok := target.(map[string]interface{}); ok {
		for k, v := range targetMap {
			merged[k] = v
		}
	}
	for k, v := range patchMap {
		if v == nil {
			delete(merged, k)
			continue
		}
		merged[k] = applyMergePatch(merged[k], v)
	}
	return merged
}

// TestSelfTombstonedShapesMatchMergePatchSemantics builds the exact
// merge-patch body each self-tombstone-credited (or deliberately
// NOT-credited) shape produces for a leaf, applies RFC 7396 merge-patch
// semantics (applyMergePatch) to a populated live object, and confirms the
// credited/not-credited verdict matches what the patch actually does. This
// is the regression guard for the defect where an empty MAP value was
// credited as removing a leaf's members despite RFC 7396 recursing into
// (rather than replacing) an object-valued patch member, leaving the live
// members completely untouched.
func TestSelfTombstonedShapesMatchMergePatchSemantics(t *testing.T) {
	const leafKey = "leaf"

	cases := map[string]struct {
		shape         Shape
		value         interface{}
		valueExplicit bool
		live          interface{}
	}{
		"explicit null, list-shaped leaf removes the whole key": {
			shape: ShapeList, value: nil, valueExplicit: true,
			live: []interface{}{"a", "b"},
		},
		"explicit null, map-shaped leaf removes the whole key": {
			shape: ShapeMap, value: nil, valueExplicit: true,
			live: map[string]interface{}{"a": "1", "b": "2"},
		},
		"empty list on a list-shaped leaf replaces it wholesale": {
			shape: ShapeList, value: []interface{}{},
			live: []interface{}{"a", "b"},
		},
		"empty map on a map-shaped leaf removes nothing": {
			shape: ShapeMap, value: map[string]interface{}{},
			live: map[string]interface{}{"a": "1", "b": "2"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			test := manifest.UpdateTest{Field: leafKey, Value: tc.value, ValueExplicit: tc.valueExplicit}
			credited := selfTombstoned(test, tc.shape)

			target := map[string]interface{}{leafKey: tc.live}
			patch := map[string]interface{}{leafKey: tc.value}
			merged, ok := applyMergePatch(target, patch).(map[string]interface{})
			if !ok {
				t.Fatalf("applyMergePatch did not return an object: %#v", merged)
			}
			mergedVal, stillPresent := merged[leafKey]

			// The live members are actually gone iff the key is gone
			// entirely (null tombstone) or it is present but now an empty
			// collection (wholesale-replaced by an empty value).
			membersRemoved := !stillPresent
			if stillPresent {
				switch v := mergedVal.(type) {
				case []interface{}:
					membersRemoved = len(v) == 0
				case map[string]interface{}:
					membersRemoved = len(v) == 0
				}
			}

			if credited != membersRemoved {
				t.Errorf("%s: selfTombstoned credited=%v but the RFC-7396 merge patch %s removed the live members (merged=%#v)",
					name, credited, map[bool]string{true: "actually", false: "did not"}[membersRemoved], merged)
			}
		})
	}
}

// findingsByPath indexes ContainerClearFinding by Path for the tests above.
func findingsByPath(findings []ContainerClearFinding) map[string]ContainerClearFinding {
	out := make(map[string]ContainerClearFinding, len(findings))
	for _, f := range findings {
		out[f.Path] = f
	}
	return out
}

// TestHasNestedNullMember covers the helper in isolation, including the
// non-map input shapes a scalar-valued or list-valued entry would pass.
func TestHasNestedNullMember(t *testing.T) {
	cases := map[string]struct {
		value interface{}
		want  bool
	}{
		"map with a null member":   {value: map[string]interface{}{"a": "1", "b": nil}, want: true},
		"map with no null members": {value: map[string]interface{}{"a": "1", "b": "2"}, want: false},
		"empty map":                {value: map[string]interface{}{}, want: false},
		"nil value":                {value: nil, want: false},
		"scalar value":             {value: "x", want: false},
		"list value":               {value: []interface{}{"a", nil}, want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hasNestedNullMember(tc.value); got != tc.want {
				t.Errorf("hasNestedNullMember(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// ensure sort import stays meaningfully used if a future edit trims one
// call site — this test itself exercises sort.Strings via a slice built
// from map iteration to keep assertions order-independent.
func TestDeclaredContainerLeavesPathsAreSorted(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	leaves, err := DeclaredContainerLeaves(crd)
	if err != nil {
		t.Fatalf("DeclaredContainerLeaves: %v", err)
	}
	paths := make([]string, len(leaves))
	for i, l := range leaves {
		paths[i] = l.Path
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(paths, sorted) {
		t.Errorf("DeclaredContainerLeaves paths = %v, not sorted", paths)
	}
}
