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
// route: an empty container value (`value: []` for a List leaf, `value: {}`
// for a Map leaf) on the leaf's own entry, with NO ValueExplicit and no
// clear: list at all — the shape provider-infobloxnios' zone-forward.yaml
// already uses as a workaround for the same underlying gap.
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
	if !byPath["labels"].Covered {
		t.Errorf("labels not covered by an empty-map self-tombstone, findings = %+v", findings)
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
		"empty map, map shape": {
			test:  manifest.UpdateTest{Value: map[string]interface{}{}},
			shape: ShapeMap,
			want:  true,
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
