package roundtrip

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// ineligibleFixtureCRD models the shapes REASON 1 and REASON 2 exist to
// recognise, each measured against a real fleet CRD before being reduced
// to this fixture:
//
//   - vpcSelector: the exact schema controller-tools generates for
//     xpv1.Selector (matchControllerRef/matchLabels/policy) — measured
//     against provider-vultr's blockstorages CRD (snapshotIdSelector).
//     Its OWN container leaf is vpcSelector.matchLabels — a free-form map
//     one level below the Selector object itself, the same way
//     network.subnets sits one level below "network" in
//     containerClearFixtureCRD.
//   - vpcRefs: the exact schema controller-tools generates for a
//     namespaced *Refs field (items carrying name/namespace/policy) —
//     measured against provider-vultr's loadbalancers CRD
//     (instancesRefs).
//   - mirroredSelector: schema-identical to vpcSelector, but its
//     matchLabels leaf is ALSO declared under status.atProvider — the
//     cross-check case: a field that matches the Selector shape but IS
//     mirrored back must never be excluded on shape alone.
//   - tags: an ordinary list, kept as the eligible control matching
//     containerClearFixtureCRD.
//   - subscriptions: a plain list REQUIRED by a ROOT x-kubernetes-validations
//     rule whenever managementPolicies intersects its own schema default
//     — measured verbatim against provider-tailscale's Webhook CRD
//     (its "subscriptions is required..." rule).
//   - deleteOnlyList: a plain list gated ONLY on the literal 'Delete',
//     which is NOT a member of managementPolicies' own default (['*'])
//     — must stay ELIGIBLE, because under the object's resting state the
//     rule's own guard is satisfied (i.e. NOT required) — a control for
//     "no eligible sibling is deliberately not a third reason" and for
//     "under the EFFECTIVE (default) managementPolicies", not any policy
//     that could theoretically apply later.
//   - specAnchoredRequired: schema-identical obligation to subscriptions,
//     but declared on the "spec" schema node instead of the CRD root
//     (self refers to spec, not the whole object) — the second anchor
//     requiredByManagementPolicies must also recognise.
const ineligibleFixtureCRD = `apiVersion: apiextensions.k8s.io/v1
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
        x-kubernetes-validations:
        - message: subscriptions is required once managementPolicies includes '*', 'Create', or 'Update'
          rule: "!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.subscriptions)"
        - message: deleteOnlyList is required only while managementPolicies includes 'Delete'
          rule: "!has(self.spec) || !has(self.spec.managementPolicies) || !('Delete' in self.spec.managementPolicies) || has(self.spec.forProvider.deleteOnlyList)"
        properties:
          spec:
            type: object
            x-kubernetes-validations:
            - message: specAnchoredRequired is required once managementPolicies includes '*', 'Create', or 'Update'
              rule: "!has(self.managementPolicies) || !('*' in self.managementPolicies || 'Create' in self.managementPolicies || 'Update' in self.managementPolicies) || has(self.forProvider.specAnchoredRequired)"
            properties:
              managementPolicies:
                type: array
                default: ["*"]
                items:
                  type: string
              forProvider:
                type: object
                properties:
                  tags:
                    type: array
                    items:
                      type: string
                  subscriptions:
                    type: array
                    items:
                      type: string
                  deleteOnlyList:
                    type: array
                    items:
                      type: string
                  specAnchoredRequired:
                    type: array
                    items:
                      type: string
                  vpcSelector:
                    type: object
                    properties:
                      matchControllerRef:
                        type: boolean
                      matchLabels:
                        type: object
                        additionalProperties:
                          type: string
                      policy:
                        type: object
                        properties:
                          resolution:
                            type: string
                          resolve:
                            type: string
                  vpcRefs:
                    type: array
                    items:
                      type: object
                      required: ["name"]
                      properties:
                        name:
                          type: string
                        namespace:
                          type: string
                        policy:
                          type: object
                          properties:
                            resolution:
                              type: string
                            resolve:
                              type: string
                  mirroredSelector:
                    type: object
                    properties:
                      matchLabels:
                        type: object
                        additionalProperties:
                          type: string
                      policy:
                        type: object
                        properties:
                          resolution:
                            type: string
                          resolve:
                            type: string
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  mirroredSelector:
                    type: object
                    properties:
                      matchLabels:
                        type: object
                        additionalProperties:
                          type: string
`

func ineligibleFindingsByPath(t *testing.T, crd map[string]interface{}) map[string]ContainerClearFinding {
	t.Helper()
	findings, err := ContainerClearCoverage(crd, &manifest.Manifest{})
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	return findingsByPath(findings)
}

// TestContainerClearCoverageReferenceResolutionSelector confirms a
// Selector's nested matchLabels leaf is reported ineligible with REASON 1,
// derived purely from the schema shape of its enclosing vpcSelector node.
func TestContainerClearCoverageReferenceResolutionSelector(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["vpcSelector.matchLabels"]
	if !ok {
		t.Fatalf("vpcSelector.matchLabels not found in findings: %+v", byPath)
	}
	if !f.Ineligible {
		t.Errorf("vpcSelector.matchLabels not marked Ineligible: %+v", f)
	}
	if f.Covered {
		t.Errorf("vpcSelector.matchLabels marked BOTH ineligible and covered: %+v", f)
	}
	if f.Reason != ReasonReferenceResolution {
		t.Errorf("vpcSelector.matchLabels.Reason = %q, want %q", f.Reason, ReasonReferenceResolution)
	}
}

// TestContainerClearCoverageReferenceResolutionRefsList confirms a *Refs
// list leaf whose items are Reference-shaped is reported ineligible with
// REASON 1.
func TestContainerClearCoverageReferenceResolutionRefsList(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["vpcRefs"]
	if !ok {
		t.Fatalf("vpcRefs not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Reason != ReasonReferenceResolution {
		t.Errorf("vpcRefs = %+v, want Ineligible=true Reason=%q", f, ReasonReferenceResolution)
	}
}

// TestContainerClearCoverageReferenceResolutionCrossCheckedAgainstMirror
// confirms the cross-check: a leaf that matches the Selector shape exactly
// but IS mirrored in status.atProvider must NOT be excluded — a genuinely
// backend-reaching field is never discarded on shape alone.
func TestContainerClearCoverageReferenceResolutionCrossCheckedAgainstMirror(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["mirroredSelector.matchLabels"]
	if !ok {
		t.Fatalf("mirroredSelector.matchLabels not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("mirroredSelector.matchLabels marked Ineligible despite being present in status.atProvider: %+v", f)
	}
}

// TestContainerClearCoverageOrdinaryListStaysEligible confirms an ordinary
// list leaf with no Selector/Reference shape and no CEL requirement is
// never marked ineligible.
func TestContainerClearCoverageOrdinaryListStaysEligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	if f := byPath["tags"]; f.Ineligible {
		t.Errorf("tags marked Ineligible, want an ordinary eligible leaf: %+v", f)
	}
}

// TestContainerClearCoverageRequiredByManagementPoliciesRootAnchored
// confirms REASON 2 at the CRD root anchor (self.spec.forProvider.<leaf>) —
// the shape measured verbatim against provider-tailscale.
func TestContainerClearCoverageRequiredByManagementPoliciesRootAnchored(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["subscriptions"]
	if !ok {
		t.Fatalf("subscriptions not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Reason != ReasonRequiredByCEL {
		t.Errorf("subscriptions = %+v, want Ineligible=true Reason=%q", f, ReasonRequiredByCEL)
	}
}

// TestContainerClearCoverageRequiredByManagementPoliciesSpecAnchored
// confirms REASON 2 at the "spec" node anchor (self.forProvider.<leaf>).
func TestContainerClearCoverageRequiredByManagementPoliciesSpecAnchored(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["specAnchoredRequired"]
	if !ok {
		t.Fatalf("specAnchoredRequired not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Reason != ReasonRequiredByCEL {
		t.Errorf("specAnchoredRequired = %+v, want Ineligible=true Reason=%q", f, ReasonRequiredByCEL)
	}
}

// TestContainerClearCoverageManagementPolicyGuardNotSatisfiedByDefault
// confirms a leaf gated only on a literal ('Delete') that is NOT a member
// of managementPolicies' own schema default (['*']) is NOT ineligible —
// under the object's resting, unedited state the guard's negation is
// true, so the OR is already satisfied without needing the leaf at all.
func TestContainerClearCoverageManagementPolicyGuardNotSatisfiedByDefault(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	if f := byPath["deleteOnlyList"]; f.Ineligible {
		t.Errorf("deleteOnlyList marked Ineligible, but its guard ('Delete') is not in the default managementPolicies (['*']): %+v", f)
	}
}

// TestContainerClearCoverageIneligibleExcludedFromSummaryDenominator
// confirms containerLeafSummary removes every ineligible leaf from BOTH
// the numerator and the denominator, and surfaces the ineligible count —
// the acceptance criterion this whole ticket exists to satisfy: the
// denominator must not demand coverage for a leaf that cannot be
// exercised.
func TestContainerClearCoverageIneligibleExcludedFromSummaryDenominator(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	findings, err := ContainerClearCoverage(crd, &manifest.Manifest{})
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}

	var ineligibleCount, eligibleCount int
	for _, f := range findings {
		if f.Ineligible {
			ineligibleCount++
		} else {
			eligibleCount++
		}
	}
	if ineligibleCount == 0 {
		t.Fatalf("fixture produced zero ineligible findings, test is vacuous: %+v", findings)
	}

	summary := containerLeafSummary(findings)
	if !strings.Contains(summary, "0/"+strconv.Itoa(eligibleCount)) {
		t.Errorf("containerLeafSummary = %q, want denominator %d (eligible leaves only, ineligible excluded)", summary, eligibleCount)
	}
	if !strings.Contains(summary, strconv.Itoa(ineligibleCount)+" ineligible") {
		t.Errorf("containerLeafSummary = %q, want it to name the ineligible count %d", summary, ineligibleCount)
	}
}

// TestContainerClearCoverageEveryIneligibleFindingCarriesAReason confirms
// no ineligible finding is ever silently reported without a reason a
// reader can audit.
func TestContainerClearCoverageEveryIneligibleFindingCarriesAReason(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	findings, err := ContainerClearCoverage(crd, &manifest.Manifest{})
	if err != nil {
		t.Fatalf("ContainerClearCoverage: %v", err)
	}
	for _, f := range findings {
		if f.Ineligible && f.Reason == "" {
			t.Errorf("finding %+v is Ineligible with an empty Reason", f)
		}
		if !f.Ineligible && f.Reason != "" {
			t.Errorf("finding %+v is not Ineligible but carries Reason %q", f, f.Reason)
		}
	}
}

// TestContainerClearCoverageContradictionBetweenIneligibleAndCovered
// confirms the guard against a leaf being classified BOTH ineligible and
// covered: an existing manifest entry that (incorrectly, or because the
// predicate is wrong) exercises removal on a leaf this run's schema-derived
// predicate says can never be exercised must surface as an error, never be
// silently resolved one way or the other.
func TestContainerClearCoverageContradictionBetweenIneligibleAndCovered(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// subscriptions is REQUIRED by REASON 2 above, but this entry
			// claims to have exercised its removal direction anyway.
			{Field: "subscriptions", Value: nil, ValueExplicit: true},
		},
	}

	if _, err := ContainerClearCoverage(crd, m); err == nil {
		t.Error("ContainerClearCoverage returned nil error for a leaf classified both ineligible and covered, want a contradiction error")
	}
}

// TestIsSelectorShape and TestIsReferenceItemShape cover the two schema
// predicates in isolation, including near-miss shapes that must NOT match
// so a genuine backend field is never excluded on a coincidental property
// overlap.
func TestIsSelectorShape(t *testing.T) {
	cases := map[string]struct {
		schema map[string]interface{}
		want   bool
	}{
		"genuine Selector shape (matchLabels + matchControllerRef + policy)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchControllerRef": map[string]interface{}{"type": "boolean"},
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"policy": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"resolution": map[string]interface{}{"type": "string"},
							"resolve":    map[string]interface{}{"type": "string"},
						},
					},
				},
			},
			want: true,
		},
		"matchLabels only, no matchControllerRef or policy": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
				},
			},
			want: true,
		},
		"extra property not in the allowed set": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"somethingElse": map[string]interface{}{"type": "string"},
				},
			},
			want: false,
		},
		"matchLabels without additionalProperties (not a free-form map)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchLabels": map[string]interface{}{"type": "object"},
				},
			},
			want: false,
		},
		"empty object": {
			schema: map[string]interface{}{"type": "object"},
			want:   false,
		},
		"policy with unexpected shape": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"policy": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"somethingElse": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
			want: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isSelectorShape(tc.schema); got != tc.want {
				t.Errorf("isSelectorShape(%+v) = %v, want %v", tc.schema, got, tc.want)
			}
		})
	}
}

func TestIsReferenceItemShape(t *testing.T) {
	cases := map[string]struct {
		schema map[string]interface{}
		want   bool
	}{
		"genuine Reference shape (name + policy)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":   map[string]interface{}{"type": "string"},
					"policy": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"resolution": map[string]interface{}{"type": "string"}}},
				},
			},
			want: true,
		},
		"genuine NamespacedReference shape (name + namespace + policy)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":      map[string]interface{}{"type": "string"},
					"namespace": map[string]interface{}{"type": "string"},
				},
			},
			want: true,
		},
		"missing name": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace": map[string]interface{}{"type": "string"},
				},
			},
			want: false,
		},
		"name is not a string": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "integer"},
				},
			},
			want: false,
		},
		"extra property not in the allowed set": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":    map[string]interface{}{"type": "string"},
					"comment": map[string]interface{}{"type": "string"},
				},
			},
			want: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isReferenceItemShape(tc.schema); got != tc.want {
				t.Errorf("isReferenceItemShape(%+v) = %v, want %v", tc.schema, got, tc.want)
			}
		})
	}
}

// TestManagementPoliciesDefault covers resolution of the schema default in
// isolation, including its absence.
func TestManagementPoliciesDefault(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	schema, err := servedSchema(crd)
	if err != nil {
		t.Fatalf("servedSchema: %v", err)
	}

	got, ok := managementPoliciesDefault(schema)
	if !ok {
		t.Fatal("managementPoliciesDefault ok=false, want true")
	}
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("managementPoliciesDefault = %v, want [\"*\"]", got)
	}

	noDefault := decodeCRD(t, containerClearFixtureCRD)
	schema2, err := servedSchema(noDefault)
	if err != nil {
		t.Fatalf("servedSchema: %v", err)
	}
	if _, ok := managementPoliciesDefault(schema2); ok {
		t.Error("managementPoliciesDefault ok=true for a schema with no managementPolicies field at all, want false")
	}
}

// TestParentPath covers the helper in isolation.
func TestParentPath(t *testing.T) {
	cases := map[string]struct {
		path string
		want string
	}{
		"top-level field has no parent": {path: "tags", want: ""},
		"one level deep":                {path: "vpcSelector.matchLabels", want: "vpcSelector"},
		"two levels deep":               {path: "a.b.c", want: "a.b"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parentPath(tc.path); got != tc.want {
				t.Errorf("parentPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
