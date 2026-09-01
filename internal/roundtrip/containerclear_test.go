package roundtrip

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
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

// dispositionFixtureCRD extends containerClearFixtureCRD's shapes with one
// CEL-immutable leaf (immutableTags), so a disposition test can confirm an
// INELIGIBLE leaf never carries a disposition report even when its own
// skip: entry declares one — Disposition is defined only for the
// uncovered/eligible state (see ContainerClearFinding.Disposition's own
// doc comment).
const dispositionFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
                  immutableTags:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableTags is immutable
                      rule: "self == oldSelf"
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

// TestContainerClearCoverageReportsDispositionOnUncoveredLeaf confirms an
// uncovered, eligible leaf whose own skip: entry declares a disposition:
// carries that exact value in the finding — the report-only surface this
// ticket adds.
func TestContainerClearCoverageReportsDispositionOnUncoveredLeaf(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{
				Field: "tags",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipVendorDefect,
					Evidence:    "observed a 400",
					Disposition: manifest.DispositionOneLivePatch,
				},
			},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	tags := byPath["tags"]
	if tags.Covered {
		t.Fatalf("tags reported covered — this test needs it uncovered to exercise the disposition report: %+v", tags)
	}
	if tags.Disposition != manifest.DispositionOneLivePatch {
		t.Errorf("tags.Disposition = %q, want %q", tags.Disposition, manifest.DispositionOneLivePatch)
	}
}

// TestContainerClearCoverageUncoveredLeafWithNoDispositionReportsEmpty
// confirms an uncovered leaf whose skip: entry authors NO disposition: key
// reports an EMPTY Disposition — not defaulted to any of the four tiers.
func TestContainerClearCoverageUncoveredLeafWithNoDispositionReportsEmpty(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Skip: manifest.SkipInfo{Reason: manifest.SkipWriteOnly}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if got := byPath["tags"].Disposition; got != "" {
		t.Errorf("tags.Disposition = %q, want empty — no disposition: key was authored, so none may be reported", got)
	}
}

// TestContainerClearCoverageUncoveredLeafWithNoEntryReportsEmptyDisposition
// confirms a leaf named by NO entry at all (the ordinary "nobody has
// touched this field yet" state) reports an empty Disposition rather than
// erroring or guessing one.
func TestContainerClearCoverageUncoveredLeafWithNoEntryReportsEmptyDisposition(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{Tests: nil}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if got := byPath["tags"].Disposition; got != "" {
		t.Errorf("tags.Disposition = %q, want empty — no entry at all names this leaf", got)
	}
	if got := byPath["labels"].Disposition; got != "" {
		t.Errorf("labels.Disposition = %q, want empty — no entry at all names this leaf", got)
	}
}

// TestContainerClearCoverageCoveredLeafNeverReportsDisposition confirms a
// leaf that IS covered — even one whose own entry carries a skip: with a
// disposition: authored on it — reports an empty Disposition. Covered is
// not a gap this axis tracks, and the ticket restricts the report to
// uncovered leaves only.
func TestContainerClearCoverageCoveredLeafNeverReportsDisposition(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// "name" clears "tags" as a sibling, covering it via the
			// whole-field tombstone route.
			{Field: "name", Value: "new-name", Clear: []string{"tags"}},
			// tags ALSO carries its own skip: with a disposition — an
			// unusual but valid combination this test uses specifically to
			// prove Covered wins and no disposition is ever surfaced for
			// it.
			{
				Field: "tags",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipWriteOnly,
					Disposition: manifest.DispositionStaticallyProvable,
				},
			},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	tags := byPath["tags"]
	if !tags.Covered {
		t.Fatalf("tags not covered by clear:, findings = %+v", findings)
	}
	if tags.Disposition != "" {
		t.Errorf("tags.Disposition = %q, want empty — a covered leaf never reports a disposition", tags.Disposition)
	}
}

// TestContainerClearCoverageIneligibleLeafNeverReportsDisposition confirms
// an INELIGIBLE leaf (CEL-immutable: its removal direction can never be
// exercised at all) reports an empty Disposition even when its own skip:
// entry declares one — Ineligible and Disposition are mutually exclusive
// report axes, and this pins that Ineligible wins.
func TestContainerClearCoverageIneligibleLeafNeverReportsDisposition(t *testing.T) {
	crd := decodeCRD(t, dispositionFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{
				Field: "immutableTags",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipWriteOnly,
					Disposition: manifest.DispositionDeclaredExclusion,
					DeclaredBy:  "a human",
					Reconfirm:   "2027-01-01",
				},
			},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	immutableTags := byPath["immutableTags"]
	if !immutableTags.Ineligible {
		t.Fatalf("immutableTags not reported Ineligible, findings = %+v", findings)
	}
	if immutableTags.Disposition != "" {
		t.Errorf("immutableTags.Disposition = %q, want empty — an ineligible leaf never reports a disposition", immutableTags.Disposition)
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

// TestContainerClearCoverageWithValuesEmptyListTombstonesLeaf confirms site
// 3's decision (ticket cf604fcf-24aa-4ba9-8cfa-2f1d906d882b): a sibling
// entry's withValues: map naming a LIST-shaped leaf with an empty list
// literal credits that leaf as covered, exactly like the value: []
// self-tombstone case above — RFC-7386 treats an empty list as wholesale
// replacement regardless of which directive placed it in the merge patch.
func TestContainerClearCoverageWithValuesEmptyListTombstonesLeaf(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", WithValues: map[string]interface{}{"tags": []interface{}{}}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	byPath := findingsByPath(findings)
	if !byPath["tags"].Covered {
		t.Errorf("tags not covered by a sibling entry's withValues: empty-list literal, findings = %+v", findings)
	}
	if byPath["labels"].Covered {
		t.Errorf("labels reported covered with no entry naming it: %+v", byPath["labels"])
	}
}

// TestContainerClearCoverageWithValuesNonEmptyListNotCredited confirms the
// withValues: credit route requires the EMPTY list specifically — a
// non-empty literal is an ordinary write, not a removal-direction proof,
// and must not be credited.
func TestContainerClearCoverageWithValuesNonEmptyListNotCredited(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", WithValues: map[string]interface{}{"tags": []interface{}{"a"}}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	if byPath := findingsByPath(findings); byPath["tags"].Covered {
		t.Errorf("tags reported covered by a non-empty withValues: literal: %+v", byPath["tags"])
	}
}

// TestContainerClearCoverageWithValuesMapShapedNeverCredited confirms the
// shape gate: withValues: can never write a null (manifest.ValidateWithValues
// requires a real, non-null value), and an empty MAP literal is not a
// tombstone under RFC-7386 (it recurses into the object and merges
// key-by-key, removing nothing) — so a MAP-shaped leaf must never be
// credited through withValues:, regardless of what literal is given.
func TestContainerClearCoverageWithValuesMapShapedNeverCredited(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "name", Value: "new-name", WithValues: map[string]interface{}{"labels": map[string]interface{}{}}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	if byPath := findingsByPath(findings); byPath["labels"].Covered {
		t.Errorf("labels (Map-shaped) reported covered via withValues:, but an empty map is never a tombstone under RFC-7386: %+v", byPath["labels"])
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

// ─── cell regrain (2026-08-29 "GRANULARITY RULED" — Rulings 1, 2, 3) ───────

// clearCellFixtureCRD declares TWO leaves at each (shape, depth)
// combination the regrain must split by, so a test can prove a cell's
// membership is exactly (shape, depth) and nothing coarser:
//
//   - tags, aliases   — both List, top-level (share one cell)
//   - labels          — Map, top-level (its own cell; no second top-level
//     map leaf, so grouping tests that need a lone top-level map member
//     use this one)
//   - network.subnets, network.routes — both Map, nested (share one cell)
//   - network.itemsList               — List, nested (its own cell; proves
//     depth splits List leaves too, not only Map ones)
const clearCellFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
                  aliases:
                    type: array
                    items:
                      type: string
                  labels:
                    type: object
                    additionalProperties:
                      type: string
                  network:
                    type: object
                    properties:
                      subnets:
                        type: object
                        additionalProperties:
                          type: string
                      routes:
                        type: object
                        additionalProperties:
                          type: string
                      itemsList:
                        type: array
                        items:
                          type: string
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

// vacuousClearCellFixtureCRD declares TWO CEL-immutable top-level List
// leaves and nothing else List-shaped, so the (list, top) cell they form
// has zero eligible members — the VACUOUS state AC 10(b) requires be
// rendered rather than omitted.
const vacuousClearCellFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
                  immutableA:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableA is immutable
                      rule: "self == oldSelf"
                  immutableB:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableB is immutable
                      rule: "self == oldSelf"
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

// clearCellReportsByKey indexes BuildClearCellReport's own output by
// "<shape>/<depth>" so a test can look up the cell it cares about without
// depending on the slice's own sorted order.
func clearCellReportsByKey(reports []ClearCellReport) map[string]ClearCellReport {
	out := make(map[string]ClearCellReport, len(reports))
	for _, r := range reports {
		out[string(r.Key.Shape)+"/"+string(r.Key.Depth)] = r
	}
	return out
}

// TestGroupClearCellsKeyIsShapeAndDepth confirms RULING 1's cell key
// exactly: two leaves sharing (shape, depth) land in ONE cell regardless of
// name, and two leaves sharing shape but differing in depth land in
// DIFFERENT cells — the depth split applies to List leaves as much as Map
// leaves.
func TestGroupClearCellsKeyIsShapeAndDepth(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	m := &manifest.Manifest{Tests: nil}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	cells := GroupClearCells(findings)
	if len(cells) != 4 {
		t.Fatalf("GroupClearCells produced %d cells, want 4 (list/top, map/top, map/nested, list/nested): %+v", len(cells), cells)
	}

	listTop := CellKey{Classification: ClassNA, Shape: ShapeList, Direction: DirectionClear, Depth: DepthTop}
	if got := sortedClearPaths(cells[listTop]); !reflect.DeepEqual(got, []string{"aliases", "tags"}) {
		t.Errorf("list/top cell members = %v, want [aliases tags]", got)
	}

	mapTop := CellKey{Classification: ClassNA, Shape: ShapeMap, Direction: DirectionClear, Depth: DepthTop}
	if got := sortedClearPaths(cells[mapTop]); !reflect.DeepEqual(got, []string{"labels"}) {
		t.Errorf("map/top cell members = %v, want [labels]", got)
	}

	mapNested := CellKey{Classification: ClassNA, Shape: ShapeMap, Direction: DirectionClear, Depth: DepthNested}
	if got := sortedClearPaths(cells[mapNested]); !reflect.DeepEqual(got, []string{"network.routes", "network.subnets"}) {
		t.Errorf("map/nested cell members = %v, want [network.routes network.subnets]", got)
	}

	listNested := CellKey{Classification: ClassNA, Shape: ShapeList, Direction: DirectionClear, Depth: DepthNested}
	if got := sortedClearPaths(cells[listNested]); !reflect.DeepEqual(got, []string{"network.itemsList"}) {
		t.Errorf("list/nested cell members = %v, want [network.itemsList] — depth must split List leaves too, not only Map leaves", got)
	}
}

func sortedClearPaths(findings []ContainerClearFinding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = f.Path
	}
	sort.Strings(out)
	return out
}

// TestDepthOfDerivesFromDottedPath is depthOf's own table test: no dotted
// ancestor is DepthTop, at least one is DepthNested — including a
// multi-level path, so the check is "any dot", not "exactly one".
func TestDepthOfDerivesFromDottedPath(t *testing.T) {
	cases := map[string]struct {
		path string
		want Depth
	}{
		"top-level, no dot":       {path: "tags", want: DepthTop},
		"one level nested":        {path: "network.subnets", want: DepthNested},
		"two levels nested":       {path: "a.b.c", want: DepthNested},
		"empty path defaults top": {path: "", want: DepthTop},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := depthOf(tc.path); got != tc.want {
				t.Errorf("depthOf(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestClearCellReportCoverageIsExistential confirms RULING 2's existential
// half: a cell with ONE covered eligible member (tags) and one uncovered,
// undisposed sibling (aliases) is nonetheless Covered — coverage does not
// require every member to be tested, only one.
func TestClearCellReportCoverageIsExistential(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// tags self-tombstoned: value: [] on its own entry.
			{Field: "tags", Value: []interface{}{}},
			// aliases carries no entry at all — uncovered, undisposed.
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]

	if !cell.Covered {
		t.Fatalf("list/top cell not Covered despite tags carrying self-tombstone coverage: %+v", cell)
	}
	if cell.Representative != "tags" {
		t.Errorf("Representative = %q, want %q (the one covered member)", cell.Representative, "tags")
	}
	if cell.Route != RouteSelfTombstone {
		t.Errorf("Route = %q, want %q", cell.Route, RouteSelfTombstone)
	}
	if !reflect.DeepEqual(cell.Members, []string{"aliases", "tags"}) {
		t.Errorf("Members = %v, want [aliases tags] — both siblings still occupy the cell", cell.Members)
	}
	if len(cell.UndispositionedMembers) != 0 {
		t.Errorf("UndispositionedMembers = %v, want empty — a covered cell reports no disposition gap", cell.UndispositionedMembers)
	}
}

// TestClearCellReportImpossibilityIsUniversalAsymmetric is the AC's own
// named case: RULING 2's universal half. tags carries a valid disposition;
// aliases carries none. Neither is covered. The cell must NOT read as
// settled — a single disposed member never speaks for its undisposed
// sibling, which is exactly the incentive-gradient failure ("author one
// reason and move on") the ruling exists to close off.
func TestClearCellReportImpossibilityIsUniversalAsymmetric(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{
				Field: "tags",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipVendorDefect,
					Evidence:    "observed a 400 on an empty-list PATCH",
					Disposition: manifest.DispositionOneLivePatch,
				},
			},
			// aliases: no entry at all — no disposition, no coverage.
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]

	if cell.Covered {
		t.Fatalf("list/top cell reported Covered with no member ever tested: %+v", cell)
	}
	if cell.Vacuous {
		t.Fatalf("list/top cell reported Vacuous — both members are eligible: %+v", cell)
	}
	if !reflect.DeepEqual(cell.UndispositionedMembers, []string{"aliases"}) {
		t.Errorf("UndispositionedMembers = %v, want exactly [aliases] — tags' own disposition must never cover its sibling", cell.UndispositionedMembers)
	}
}

// TestClearCellReportImpossibilityIsUniversalBothDisposed is the symmetric
// control for the asymmetric test above: once EVERY eligible member of an
// uncovered cell carries a disposition, UndispositionedMembers is empty.
func TestClearCellReportImpossibilityIsUniversalBothDisposed(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	disposedSkip := manifest.SkipInfo{
		Reason:      manifest.SkipVendorDefect,
		Evidence:    "observed a 400 on an empty-list PATCH",
		Disposition: manifest.DispositionOneLivePatch,
	}
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Skip: disposedSkip},
			{Field: "aliases", Skip: disposedSkip},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]

	if cell.Covered {
		t.Fatalf("list/top cell reported Covered with no member ever tested: %+v", cell)
	}
	if len(cell.UndispositionedMembers) != 0 {
		t.Errorf("UndispositionedMembers = %v, want empty — every eligible member carries a disposition", cell.UndispositionedMembers)
	}
}

// TestClearCellReportVacuousCellRendered confirms AC 10(b): a cell whose
// every member is ineligible is rendered — Members and IneligibleMembers
// populated, Vacuous true — rather than omitted for lack of an eligible
// member.
func TestClearCellReportVacuousCellRendered(t *testing.T) {
	crd := decodeCRD(t, vacuousClearCellFixtureCRD)
	m := &manifest.Manifest{Tests: nil}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	reports := BuildClearCellReport(findings)
	cell := clearCellReportsByKey(reports)["list/top"]

	if !cell.Vacuous {
		t.Fatalf("list/top cell not reported Vacuous despite both members being CEL-immutable: %+v", cell)
	}
	if cell.Covered {
		t.Errorf("a Vacuous cell must never also report Covered: %+v", cell)
	}
	if !reflect.DeepEqual(cell.Members, []string{"immutableA", "immutableB"}) {
		t.Errorf("Members = %v, want [immutableA immutableB] — a vacuous cell still names its membership", cell.Members)
	}
	if !reflect.DeepEqual(cell.IneligibleMembers, []string{"immutableA", "immutableB"}) {
		t.Errorf("IneligibleMembers = %v, want [immutableA immutableB]", cell.IneligibleMembers)
	}
	if len(cell.UndispositionedMembers) != 0 {
		t.Errorf("UndispositionedMembers = %v, want empty — a vacuous cell has no eligible member to demand a disposition from", cell.UndispositionedMembers)
	}

	// The vacuous cell must actually be present in the reported slice, not
	// merely constructible by a caller who already knows to look for it.
	found := false
	for _, r := range reports {
		if r.Key.Shape == ShapeList && r.Key.Depth == DepthTop {
			found = true
		}
	}
	if !found {
		t.Error("BuildClearCellReport omitted the vacuous cell from its output entirely")
	}
}

// TestClearCellReportRepresentativeIsStickyAndDeterministic is RULING 3:
// a clear cell's representative does not rotate. With BOTH tags and
// aliases covered, the alphabetically-first covered member (aliases) is
// chosen every time, across repeated calls — there is no cursor, no seed,
// and no RotationState in this code path at all (contrast
// roundtrip.CreditCells, which takes one), so "sticky" here means simply
// that the same input always yields the same answer, deterministically,
// forever.
func TestClearCellReportRepresentativeIsStickyAndDeterministic(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Value: []interface{}{}},
			{Field: "aliases", Value: []interface{}{}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	// Simulate several successive "runs" (BuildClearCellReport regroups
	// from scratch every call, exactly as roundtrip-verify's own
	// per-invocation report construction does) and confirm the
	// Representative never moves.
	for i := 0; i < 5; i++ {
		cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]
		if !cell.Covered {
			t.Fatalf("run %d: list/top cell not Covered: %+v", i, cell)
		}
		if cell.Representative != "aliases" {
			t.Errorf("run %d: Representative = %q, want %q (alphabetically first covered member, sticky across runs)", i, cell.Representative, "aliases")
		}
		if !reflect.DeepEqual(cell.Members, []string{"aliases", "tags"}) {
			t.Errorf("run %d: Members = %v, want [aliases tags]", i, cell.Members)
		}
	}
}

// TestBuildClearCellReportOrderIsStable confirms the reported cell order
// is deterministic (sorted by CellKey.String()) across repeated calls,
// independent of Go's randomized map iteration inside GroupClearCells —
// the property BuildClearCellReport's own sort exists to guarantee.
func TestBuildClearCellReportOrderIsStable(t *testing.T) {
	crd := decodeCRD(t, clearCellFixtureCRD)
	m := &manifest.Manifest{Tests: nil}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	var first []string
	for i := 0; i < 10; i++ {
		reports := BuildClearCellReport(findings)
		keys := make([]string, len(reports))
		for j, r := range reports {
			keys[j] = r.Key.String()
		}
		if i == 0 {
			first = keys
			continue
		}
		if !reflect.DeepEqual(keys, first) {
			t.Fatalf("run %d: cell order = %v, want %v (stable across calls)", i, keys, first)
		}
	}
}

// TestClearCellReportRouteNamedForEachMechanism is AC 10(c): every one of
// the five credit routes is both produced by ContainerClearCoverage and
// carried through to the cell report's own Route field, using a
// single-member cell per case so the covered member IS the representative.
func TestClearCellReportRouteNamedForEachMechanism(t *testing.T) {
	crd := decodeCRD(t, containerClearFixtureCRD)

	cases := map[string]struct {
		tests    []manifest.UpdateTest
		wantPath string
		want     ClearRoute
	}{
		"sibling clear:": {
			tests:    []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"tags"}}},
			wantPath: "tags",
			want:     RouteSiblingClear,
		},
		"sibling withValues:": {
			tests:    []manifest.UpdateTest{{Field: "name", Value: "new-name", WithValues: map[string]interface{}{"tags": []interface{}{}}}},
			wantPath: "tags",
			want:     RouteSiblingWithValues,
		},
		"ancestor-tombstone": {
			tests:    []manifest.UpdateTest{{Field: "name", Value: "new-name", Clear: []string{"network"}}},
			wantPath: "network.subnets",
			want:     RouteAncestorTombstone,
		},
		"per-key-null": {
			tests:    []manifest.UpdateTest{{Field: "labels", Value: map[string]interface{}{"a": "1", "b": nil}}},
			wantPath: "labels",
			want:     RoutePerKeyNull,
		},
		"self-tombstone": {
			tests:    []manifest.UpdateTest{{Field: "tags", Value: []interface{}{}}},
			wantPath: "tags",
			want:     RouteSelfTombstone,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{Tests: tc.tests}
			findings, err := ContainerClearCoverage(crd, m)
			if err != nil {
				t.Fatalf("ContainerClearCoverage: %v", err)
			}

			byPath := findingsByPath(findings)
			leaf := byPath[tc.wantPath]
			if !leaf.Covered {
				t.Fatalf("%s not covered: %+v", tc.wantPath, leaf)
			}
			if leaf.Route != tc.want {
				t.Errorf("ContainerClearFinding.Route = %q, want %q", leaf.Route, tc.want)
			}

			// And the SAME route must survive into the cell report's own
			// Representative/Route for the cell this leaf occupies alone.
			depth := depthOf(tc.wantPath)
			cell := clearCellReportsByKey(BuildClearCellReport(findings))[string(leaf.Shape)+"/"+string(depth)]
			if cell.Representative != tc.wantPath {
				t.Errorf("cell Representative = %q, want %q", cell.Representative, tc.wantPath)
			}
			if cell.Route != tc.want {
				t.Errorf("cell Route = %q, want %q", cell.Route, tc.want)
			}
		})
	}
}

// TestPrintClearCellReportRendersAllThreeRequiredThings confirms the text
// report AC 10 demands: (a) a credited leaf names its representative and
// route, (b) a vacuous cell is printed rather than skipped, and (c) the
// verdict line states TWO numbers — cells and leaves — never one.
func TestPrintClearCellReportRendersAllThreeRequiredThings(t *testing.T) {
	crd := decodeCRD(t, vacuousClearCellFixtureCRD)
	m := &manifest.Manifest{Tests: nil}
	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	reports := BuildClearCellReport(findings)

	var out []string
	PrintClearCellReport(func(format string, args ...interface{}) {
		out = append(out, fmt.Sprintf(format, args...))
	}, reports)
	full := strings.Join(out, "")

	if !strings.Contains(full, "VACUOUS") {
		t.Errorf("report does not render the vacuous cell:\n%s", full)
	}
	if !strings.Contains(full, "cells covered") || !strings.Contains(full, "leaves covered") {
		t.Errorf("verdict line does not state both a cell count and a leaf count:\n%s", full)
	}
}

// mixedClearCellFixtureCRD declares THREE top-level List leaves that all
// land in the SAME (list, top) cell: aliases (eligible, left uncovered and
// undispositioned), immutableC (CEL-immutable, ineligible) and tags
// (eligible, covered by a self-tombstone test entry). This is the one
// shape no fixture before it constructed — every prior ineligible fixture
// was fully vacuous (every member ineligible) — and it is the shape a
// covered cell's member listing must render correctly: crediting tags,
// never immutableC, while still keeping immutableC visible as excluded.
const mixedClearCellFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
                  aliases:
                    type: array
                    items:
                      type: string
                  immutableC:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableC is immutable
                      rule: "self == oldSelf"
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

// TestClearCellReportMixedCellCreditsOnlyEligibleMembers is the ticket's
// own required pin: a single cell carrying one covered eligible member
// (tags), one uncovered eligible member (aliases) and one INELIGIBLE
// member (immutableC) together — the shape every prior ineligible fixture
// skipped by being fully vacuous. It must fail if any render site reverts
// to iterating Members unfiltered.
func TestClearCellReportMixedCellCreditsOnlyEligibleMembers(t *testing.T) {
	crd := decodeCRD(t, mixedClearCellFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "tags", Value: []interface{}{}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]

	if !reflect.DeepEqual(cell.Members, []string{"aliases", "immutableC", "tags"}) {
		t.Fatalf("Members = %v, want [aliases immutableC tags] — the field's own contract holds eligible and ineligible together", cell.Members)
	}
	if !reflect.DeepEqual(cell.IneligibleMembers, []string{"immutableC"}) {
		t.Fatalf("IneligibleMembers = %v, want [immutableC]", cell.IneligibleMembers)
	}
	if !cell.Covered {
		t.Fatalf("cell not Covered despite tags carrying a self-tombstone: %+v", cell)
	}
	if cell.Representative != "tags" {
		t.Errorf("Representative = %q, want %q", cell.Representative, "tags")
	}
	if !reflect.DeepEqual(cell.EligibleMembers(), []string{"aliases", "tags"}) {
		t.Errorf("EligibleMembers() = %v, want [aliases tags] — immutableC subtracted", cell.EligibleMembers())
	}

	var out []string
	PrintClearCellReport(func(format string, args ...interface{}) {
		out = append(out, fmt.Sprintf(format, args...))
	}, []ClearCellReport{cell})
	full := strings.Join(out, "")

	creditLine := ""
	excludedLine := ""
	for _, line := range out {
		if strings.Contains(line, "credits") {
			creditLine = line
		}
		if strings.Contains(line, "excluded (ineligible)") {
			excludedLine = line
		}
	}
	if creditLine == "" {
		t.Fatalf("no credited-member line rendered:\n%s", full)
	}
	if strings.Contains(creditLine, "immutableC") {
		t.Errorf("credited-member line names the INELIGIBLE member as credited: %q", creditLine)
	}
	if !strings.Contains(creditLine, "credits 2 member(s): aliases, tags") {
		t.Errorf("credited-member line = %q, want count 2 and list [aliases tags] — count and list must agree", creditLine)
	}
	if excludedLine == "" || !strings.Contains(excludedLine, "immutableC") {
		t.Errorf("ineligible member immutableC must remain VISIBLE as excluded, not silently dropped from output entirely:\n%s", full)
	}
}

// uncoveredDispositionedMixedCellFixtureCRD declares THREE top-level List
// leaves landing in the SAME (list, top) cell: aliases and labels (both
// eligible, uncovered, each carrying an authored skip: disposition) and
// immutableC (CEL-immutable, ineligible). Unlike
// mixedClearCellFixtureCRD, NONE of the eligible members is covered — the
// cell as a whole is uncovered, but every eligible member is
// dispositioned, which is the one shape that reaches the
// "uncovered, every eligible member dispositioned" render line rather
// than either the covered line or the "undispositioned member(s)" line.
const uncoveredDispositionedMixedCellFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
                  aliases:
                    type: array
                    items:
                      type: string
                  labels:
                    type: array
                    items:
                      type: string
                  immutableC:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableC is immutable
                      rule: "self == oldSelf"
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  name:
                    type: string
`

// TestClearCellReportUncoveredCellWithIneligibleMemberDispositionedRendersOnlyEligible
// is the render-site-2 pin `TestClearCellReportMixedCellCreditsOnlyEligibleMembers`
// does not reach: a cell that is UNCOVERED (no member earns clear-direction
// credit), non-vacuous (it carries eligible members), and every eligible
// member is dispositioned via skip: — the one shape that walks the
// "uncovered, every eligible member dispositioned" branch. It must fail if
// that branch reverts to iterating Members unfiltered, which would name
// the ineligible immutableC as dispositioned even though skip: was never
// authored on it.
func TestClearCellReportUncoveredCellWithIneligibleMemberDispositionedRendersOnlyEligible(t *testing.T) {
	crd := decodeCRD(t, uncoveredDispositionedMixedCellFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{
				Field: "aliases",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipVendorDefect,
					Evidence:    "observed a 400",
					Disposition: manifest.DispositionOneLivePatch,
				},
			},
			{
				Field: "labels",
				Skip: manifest.SkipInfo{
					Reason:      manifest.SkipWriteOnly,
					Disposition: manifest.DispositionStaticallyProvable,
				},
			},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	cell := clearCellReportsByKey(BuildClearCellReport(findings))["list/top"]

	if cell.Vacuous {
		t.Fatalf("cell reported Vacuous — it carries eligible members: %+v", cell)
	}
	if cell.Covered {
		t.Fatalf("cell reported Covered — no member in this fixture earns clear-direction credit: %+v", cell)
	}
	if len(cell.UndispositionedMembers) != 0 {
		t.Fatalf("UndispositionedMembers = %v, want empty — both eligible members carry an authored skip: disposition", cell.UndispositionedMembers)
	}
	if !reflect.DeepEqual(cell.IneligibleMembers, []string{"immutableC"}) {
		t.Fatalf("IneligibleMembers = %v, want [immutableC]", cell.IneligibleMembers)
	}
	if !reflect.DeepEqual(cell.EligibleMembers(), []string{"aliases", "labels"}) {
		t.Fatalf("EligibleMembers() = %v, want [aliases labels] — immutableC subtracted", cell.EligibleMembers())
	}

	var out []string
	PrintClearCellReport(func(format string, args ...interface{}) {
		out = append(out, fmt.Sprintf(format, args...))
	}, []ClearCellReport{cell})
	full := strings.Join(out, "")

	dispositionedLine := ""
	excludedLine := ""
	for _, line := range out {
		if strings.Contains(line, "every eligible member dispositioned") {
			dispositionedLine = line
		}
		if strings.Contains(line, "excluded (ineligible)") {
			excludedLine = line
		}
	}
	if dispositionedLine == "" {
		t.Fatalf("no 'every eligible member dispositioned' line rendered:\n%s", full)
	}
	if strings.Contains(dispositionedLine, "immutableC") {
		t.Errorf("dispositioned-member line names the INELIGIBLE member as dispositioned: %q", dispositionedLine)
	}
	if !strings.Contains(dispositionedLine, "every eligible member dispositioned: aliases, labels") {
		t.Errorf("dispositioned-member line = %q, want the list [aliases labels] and nothing else", dispositionedLine)
	}
	if excludedLine == "" || !strings.Contains(excludedLine, "immutableC") {
		t.Errorf("ineligible member immutableC must remain VISIBLE as excluded, not silently dropped from output entirely:\n%s", full)
	}
}
