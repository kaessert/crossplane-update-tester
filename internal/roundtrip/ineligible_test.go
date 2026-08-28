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
//   - namespacedVpcSelector: the exact schema controller-tools generates
//     for a NAMESPACED xpv1.Selector — schema-identical to vpcSelector but
//     carrying a fourth property, "namespace" — measured against
//     provider-vultr's namespaced instances CRD (attachVpcSelector). Its
//     OWN container leaf is namespacedVpcSelector.matchLabels, and it must
//     be classified ReasonReferenceResolution exactly like vpcSelector's:
//     the namespaced twin of a cluster-scoped selector is reference-
//     resolution plumbing either way.
//   - tags: an ordinary list, kept as the eligible control matching
//     containerClearFixtureCRD.
//   - backendConfigRefs: the OVER-EXCLUSION counter-fixture — a list field
//     whose JSON name ends in "Refs" exactly like vpcRefs, but whose items
//     are shaped nothing like crossplane-runtime's generated Reference
//     (id/kind instead of name/namespace/policy). A genuine backend
//     field that happens to end in "Refs" must stay ELIGIBLE: REASON 1 is
//     keyed on the item schema's SHAPE, never the field's name.
//   - weirdSelector: the OVER-EXCLUSION counter-fixture for REASON 1's
//     other shape — an object field whose JSON name ends in "Selector"
//     exactly like vpcSelector, but whose only property ("mode") is not
//     drawn from {matchControllerRef, matchLabels, policy} at all. Its own
//     container leaf (weirdSelector.mode, a free-form map) must stay
//     ELIGIBLE for the same reason.
//   - subscriptions: a plain list REQUIRED by a ROOT x-kubernetes-validations
//     rule whenever managementPolicies intersects its own schema default
//     — measured verbatim against provider-tailscale's Webhook CRD
//     (its "subscriptions is required..." rule) — but with NO minItems and
//     no size() guard of its own, so `value: []` still clears it: this
//     must stay ELIGIBLE, the over-exclusion this fixture pins.
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
//     requiredByManagementPolicies must also recognise. Also has no
//     minItems, so it too stays ELIGIBLE.
//   - requiredMinItemsList: CEL-required exactly like subscriptions, but
//     its own schema ALSO declares minItems: 1 — measured verbatim against
//     provider-f5xc's BgpAsnSet.asNumbers. `value: []` is rejected by
//     minItems itself, so this is the genuine ineligible case.
//   - requiredSizeGuardList: CEL-required like subscriptions, with no
//     minItems, but a SECOND root CEL rule independently requires
//     `.size() > 0` on it — the other route to closing the `value: []`
//     clear, with no minItems fleet example measured (kept as a unit-only
//     control since the fleet has none today).
//   - requiredMap: a free-form MAP leaf required by a root CEL rule — the
//     fleet has zero such leaves today, but the case is unit-tested
//     anyway: `value: {}` is an RFC-7386 no-op, so a MAP leaf has no
//     escape route the way a LIST leaf's `value: []` does.
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
        - message: requiredMinItemsList is required once managementPolicies includes '*', 'Create', or 'Update'
          rule: "!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.requiredMinItemsList)"
        - message: requiredSizeGuardList is required once managementPolicies includes '*', 'Create', or 'Update'
          rule: "!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.requiredSizeGuardList)"
        - message: requiredSizeGuardList must never be emptied, independent of managementPolicies
          rule: "self.spec.forProvider.requiredSizeGuardList.size() > 0"
        - message: requiredMap is required once managementPolicies includes '*', 'Create', or 'Update'
          rule: "!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.requiredMap)"
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
                  backendConfigRefs:
                    type: array
                    items:
                      type: object
                      required: ["id", "kind"]
                      properties:
                        id:
                          type: string
                        kind:
                          type: string
                  weirdSelector:
                    type: object
                    properties:
                      mode:
                        type: object
                        additionalProperties:
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
                  requiredMinItemsList:
                    type: array
                    minItems: 1
                    items:
                      type: string
                  requiredSizeGuardList:
                    type: array
                    items:
                      type: string
                  requiredMap:
                    type: object
                    additionalProperties:
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
                  namespacedVpcSelector:
                    type: object
                    properties:
                      matchControllerRef:
                        type: boolean
                      matchLabels:
                        type: object
                        additionalProperties:
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
                  immutableTags:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: immutableTags is immutable
                      rule: "self == oldSelf"
                  immutableGroup:
                    type: object
                    x-kubernetes-validations:
                    - message: immutableGroup is immutable after creation
                      rule: "self == oldSelf"
                    properties:
                      members:
                        type: array
                        items:
                          type: string
                  conditionalImmutableList:
                    type: array
                    items:
                      type: string
                    x-kubernetes-validations:
                    - message: conditionalImmutableList is immutable once set
                      rule: "size(oldSelf) == 0 || self == oldSelf"
                  guardedGroup:
                    type: object
                    x-kubernetes-validations:
                    - message: someField must not be removed once set, but may otherwise change
                      rule: "!has(oldSelf.someField) || has(self.someField)"
                    properties:
                      someField:
                        type: array
                        items:
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

// TestContainerClearCoverageReferenceResolutionNamespacedSelector confirms
// the NAMESPACED twin of a Selector — one carrying the extra "namespace"
// property crossplane-runtime generates for a namespaced managed
// resource — is reported ineligible with REASON 1 exactly like its
// cluster-scoped sibling in
// TestContainerClearCoverageReferenceResolutionSelector above. A future
// edit that narrows isSelectorShape back to the cluster-scoped shape only
// would pass that test and fail this one.
func TestContainerClearCoverageReferenceResolutionNamespacedSelector(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["namespacedVpcSelector.matchLabels"]
	if !ok {
		t.Fatalf("namespacedVpcSelector.matchLabels not found in findings: %+v", byPath)
	}
	if !f.Ineligible {
		t.Errorf("namespacedVpcSelector.matchLabels not marked Ineligible: %+v", f)
	}
	if f.Covered {
		t.Errorf("namespacedVpcSelector.matchLabels marked BOTH ineligible and covered: %+v", f)
	}
	if f.Reason != ReasonReferenceResolution {
		t.Errorf("namespacedVpcSelector.matchLabels.Reason = %q, want %q", f.Reason, ReasonReferenceResolution)
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

// TestContainerClearCoverageOverExclusionRefsNameMatchButShapeMismatch is
// the reviewer's own primary concern, pinned as a regression test: a
// genuine backend-carried container field whose JSON name happens to end
// in "Refs", exactly like a real crossplane-runtime *Refs field, must NOT
// be marked ineligible when its item schema does not match
// Reference/NamespacedReference. A name-suffix implementation would fail
// this test; the schema-shape implementation must pass it.
func TestContainerClearCoverageOverExclusionRefsNameMatchButShapeMismatch(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["backendConfigRefs"]
	if !ok {
		t.Fatalf("backendConfigRefs not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("backendConfigRefs marked Ineligible on NAME alone (ends in \"Refs\") despite its items not matching the Reference shape: %+v", f)
	}
}

// TestContainerClearCoverageOverExclusionSelectorNameMatchButShapeMismatch
// is the Selector-shape analogue of the test above: an object field whose
// JSON name ends in "Selector" but whose properties are not drawn from
// {matchControllerRef, matchLabels, policy} must leave its own container
// leaf (weirdSelector.mode) ELIGIBLE.
func TestContainerClearCoverageOverExclusionSelectorNameMatchButShapeMismatch(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["weirdSelector.mode"]
	if !ok {
		t.Fatalf("weirdSelector.mode not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("weirdSelector.mode marked Ineligible on NAME alone (parent ends in \"Selector\") despite the parent not matching the Selector shape: %+v", f)
	}
}

// TestContainerClearCoverageRequiredByManagementPoliciesRootAnchoredListStaysEligible
// confirms the fix this ticket exists to make: a LIST leaf required by a
// ROOT x-kubernetes-validations rule, with no minItems and no size() guard
// of its own, stays ELIGIBLE — `value: []` still clears it even though
// `value: null` would be rejected by the rule's has() guard. This is the
// exact shape measured against provider-tailscale's Webhook.subscriptions.
func TestContainerClearCoverageRequiredByManagementPoliciesRootAnchoredListStaysEligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["subscriptions"]
	if !ok {
		t.Fatalf("subscriptions not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("subscriptions marked Ineligible, want ELIGIBLE: value: [] still clears a CEL-required list with no minItems: %+v", f)
	}
}

// TestContainerClearCoverageRequiredByManagementPoliciesSpecAnchoredListStaysEligible
// is the spec-anchor analogue: a LIST leaf required via the "spec" node
// anchor, with no minItems, also stays ELIGIBLE.
func TestContainerClearCoverageRequiredByManagementPoliciesSpecAnchoredListStaysEligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["specAnchoredRequired"]
	if !ok {
		t.Fatalf("specAnchoredRequired not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("specAnchoredRequired marked Ineligible, want ELIGIBLE: value: [] still clears a CEL-required list with no minItems: %+v", f)
	}
}

// TestContainerClearCoverageRequiredListWithMinItemsStaysIneligible confirms
// the genuine ineligible case: a LIST leaf required by a CEL rule AND
// carrying its own minItems: 1 has BOTH clear routes closed — nulling by
// the CEL rule's has() guard, emptying by minItems itself — and the reason
// names minItems as the actual blocker, not "admission rejects nulling it".
// Measured verbatim against provider-f5xc's BgpAsnSet.asNumbers.
func TestContainerClearCoverageRequiredListWithMinItemsStaysIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["requiredMinItemsList"]
	if !ok {
		t.Fatalf("requiredMinItemsList not found in findings: %+v", byPath)
	}
	if !f.Ineligible {
		t.Fatalf("requiredMinItemsList not marked Ineligible, want it blocked by minItems: %+v", f)
	}
	if strings.Contains(string(f.Reason), "admission rejects nulling it") {
		t.Errorf("requiredMinItemsList.Reason = %q, must not use the generic pre-fix wording", f.Reason)
	}
	if !strings.Contains(string(f.Reason), "minItems: 1") {
		t.Errorf("requiredMinItemsList.Reason = %q, want it to name minItems: 1 as the actual blocker", f.Reason)
	}
}

// TestContainerClearCoverageRequiredListWithSizeGuardStaysIneligible confirms
// the second blocking route: a LIST leaf required by CEL, with no minItems
// of its own, but a SECOND CEL rule independently requiring `.size() > 0`
// on it. No fleet example exists today (unit-only control per the ticket),
// but the predicate must still recognise it and name the size() guard as
// the actual blocker.
func TestContainerClearCoverageRequiredListWithSizeGuardStaysIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["requiredSizeGuardList"]
	if !ok {
		t.Fatalf("requiredSizeGuardList not found in findings: %+v", byPath)
	}
	if !f.Ineligible {
		t.Fatalf("requiredSizeGuardList not marked Ineligible, want it blocked by its size() guard: %+v", f)
	}
	if strings.Contains(string(f.Reason), "admission rejects nulling it") {
		t.Errorf("requiredSizeGuardList.Reason = %q, must not use the generic pre-fix wording", f.Reason)
	}
	if !strings.Contains(string(f.Reason), "size()") {
		t.Errorf("requiredSizeGuardList.Reason = %q, want it to name the size() guard as the actual blocker", f.Reason)
	}
}

// TestContainerClearCoverageRequiredMapStaysIneligible confirms a MAP leaf
// required by CEL stays ineligible even though the fleet carries zero such
// leaves today: `value: {}` is an RFC-7386 no-op (de02d9df), so a MAP leaf
// has no analogue to a LIST leaf's `value: []` escape route.
func TestContainerClearCoverageRequiredMapStaysIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["requiredMap"]
	if !ok {
		t.Fatalf("requiredMap not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Reason != ReasonRequiredByCELMap {
		t.Errorf("requiredMap = %+v, want Ineligible=true Reason=%q", f, ReasonRequiredByCELMap)
	}
}

// TestContainerClearCoverageCELImmutableDirectNodeIneligible confirms a
// leaf whose OWN schema node carries an x-kubernetes-validations rule
// matching "self == oldSelf" is reported ineligible with ReasonCELImmutable,
// and that its reason text names what makes it distinct from
// ReasonRequiredByCELMap: admission rejects EVERY mutation, not merely a
// null patch. Measured verbatim against provider-lambda's
// Instance.{fileSystemMounts,fileSystemNames,firewallRulesets,sshKeyNames,tags}.
func TestContainerClearCoverageCELImmutableDirectNodeIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["immutableTags"]
	if !ok {
		t.Fatalf("immutableTags not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Covered {
		t.Fatalf("immutableTags = %+v, want Ineligible=true Covered=false", f)
	}
	if f.Reason != ReasonCELImmutable {
		t.Errorf("immutableTags.Reason = %q, want %q", f.Reason, ReasonCELImmutable)
	}
	if !strings.Contains(string(f.Reason), "EVERY mutation") {
		t.Errorf("immutableTags.Reason = %q, want it to state that admission rejects EVERY mutation (not merely a null patch), so a reader can tell it apart from ReasonRequiredByCELMap", f.Reason)
	}
	if f.Reason == ReasonRequiredByCELMap {
		t.Errorf("immutableTags.Reason must not collapse to ReasonRequiredByCELMap — the two are derived from different admission guards")
	}
}

// TestContainerClearCoverageCELImmutableInheritedFromAncestorIneligible
// confirms the ancestor-inheritance case: "immutableGroup" itself carries
// the "self == oldSelf" marker, and it is not itself a declared container
// leaf (it has properties, so DiffReport descends into it) — the marker
// must still reach the nested leaf "immutableGroup.members" beneath it,
// reusing immutable.go's own collectImmutablePaths walk rather than a
// second one. Measured verbatim against provider-f5xc's
// VirtualSite.siteSelector.expressions, inherited from the immutable
// "siteSelector" ancestor.
func TestContainerClearCoverageCELImmutableInheritedFromAncestorIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["immutableGroup.members"]
	if !ok {
		t.Fatalf("immutableGroup.members not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Covered {
		t.Fatalf("immutableGroup.members = %+v, want Ineligible=true Covered=false", f)
	}
	if f.Reason != ReasonCELImmutable {
		t.Errorf("immutableGroup.members.Reason = %q, want %q", f.Reason, ReasonCELImmutable)
	}
}

// TestContainerClearCoverageCELImmutableConditionalFormIneligible confirms
// the "immutable once set" conditional spelling —
// "size(oldSelf) == 0 || self == oldSelf" — is recognised identically to
// the unconditional form: reCELImmutable already matches it (the substring
// "self == oldSelf" appears verbatim), and once a value exists no mutation
// is accepted, so the removal direction is blocked from the moment the
// field is populated — which every example manifest carrying it does at
// create. Measured verbatim against provider-vultr's Instance.sshkeyId.
func TestContainerClearCoverageCELImmutableConditionalFormIneligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["conditionalImmutableList"]
	if !ok {
		t.Fatalf("conditionalImmutableList not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Covered {
		t.Fatalf("conditionalImmutableList = %+v, want Ineligible=true Covered=false", f)
	}
	if f.Reason != ReasonCELImmutable {
		t.Errorf("conditionalImmutableList.Reason = %q, want %q", f.Reason, ReasonCELImmutable)
	}
}

// TestContainerClearCoverageCELImmutableOrdinaryMutableLeafStaysEligible is
// a counter-fixture proving no over-exclusion: "tags" carries no CEL rule
// at all, so it must stay eligible rather than being swept in by an overly
// broad immutability check.
func TestContainerClearCoverageCELImmutableOrdinaryMutableLeafStaysEligible(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f := byPath["tags"]
	if f.Ineligible {
		t.Errorf("tags marked Ineligible (Reason=%q), want an ordinary eligible leaf untouched by CEL-immutability", f.Reason)
	}
}

// TestContainerClearCoverageCELImmutableSiblingRemoveGuardNotMistakenForImmutability
// is the second, deliberate counter-fixture: "guardedGroup" carries the
// "!has(oldSelf.someField) || has(self.someField)" sibling rule shape —
// guarding one NAMED field against being removed once set, not declaring
// the current schema node immutable — which reCELImmutable is documented
// to NOT match. "guardedGroup.someField" beneath it must stay eligible; a
// regex change that started matching this shape would misclassify every
// field in an object carrying its own remove-guard as immutable.
func TestContainerClearCoverageCELImmutableSiblingRemoveGuardNotMistakenForImmutability(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	byPath := ineligibleFindingsByPath(t, crd)

	f, ok := byPath["guardedGroup.someField"]
	if !ok {
		t.Fatalf("guardedGroup.someField not found in findings: %+v", byPath)
	}
	if f.Ineligible {
		t.Errorf("guardedGroup.someField marked Ineligible (Reason=%q), want it to stay eligible — the enclosing rule guards removal of a named sibling field, it does not declare this node immutable", f.Reason)
	}
}

// TestContainerClearCoverageCELImmutableContradictionWithCovered confirms
// the ineligible/covered contradiction guard treats ReasonCELImmutable the
// same as ReasonRequiredByCELMap (an error), not the same as
// ReasonReferenceResolution (silent exclusion): a manifest entry that
// claims to have exercised removal on immutableTags — a leaf admission
// rejects every mutation of — is a genuine contradiction and must surface
// loudly.
func TestContainerClearCoverageCELImmutableContradictionWithCovered(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "immutableTags", Value: nil, ValueExplicit: true},
		},
	}

	if _, err := ContainerClearCoverage(crd, m); err == nil {
		t.Error("ContainerClearCoverage returned nil error for a CEL-immutable leaf classified both ineligible and covered, want a contradiction error")
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
// silently resolved one way or the other. Uses requiredMinItemsList, not
// subscriptions: subscriptions is ELIGIBLE now (no minItems), so it can no
// longer produce this contradiction at all — requiredMinItemsList is a
// leaf that stays genuinely ineligible after this ticket's fix.
func TestContainerClearCoverageContradictionBetweenIneligibleAndCovered(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// requiredMinItemsList is REQUIRED by REASON 2 above (and
			// blocked from the value: [] route by its own minItems: 1),
			// but this entry claims to have exercised its removal anyway.
			{Field: "requiredMinItemsList", Value: nil, ValueExplicit: true},
		},
	}

	if _, err := ContainerClearCoverage(crd, m); err == nil {
		t.Error("ContainerClearCoverage returned nil error for a leaf classified both ineligible and covered, want a contradiction error")
	}
}

// TestContainerClearCoverageReferenceResolutionAncestorSweepNotAContradiction
// is the f5xc ServicePolicy.allowList.ipPrefixSetRefs regression pin: an
// ancestor clear: tombstone that incidentally sweeps up a REASON-1
// (reference-resolution) descendant is NOT a contradiction, decided
// explicitly by this ticket. A reference-resolution field carries no CEL
// rule guarding it, so nothing about the combined merge patch is ever
// rejected by admission — the descendant being swept up is inert collateral,
// not evidence the predicate disagrees with the manifest.
func TestContainerClearCoverageReferenceResolutionAncestorSweepNotAContradiction(t *testing.T) {
	crd := decodeCRD(t, ineligibleFixtureCRD)
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			// vpcSelector is not itself a container leaf, but nulling it as
			// an ancestor tombstone sweeps up vpcSelector.matchLabels — a
			// REASON-1 (reference-resolution) descendant.
			{Field: "tags", Value: []interface{}{"a"}, Clear: []string{"vpcSelector"}},
		},
	}

	findings, err := ContainerClearCoverage(crd, m)
	if err != nil {
		t.Fatalf("ContainerClearCoverage returned an error for an ancestor tombstone incidentally sweeping up a reference-resolution descendant, want no error: %v", err)
	}
	byPath := findingsByPath(findings)
	f, ok := byPath["vpcSelector.matchLabels"]
	if !ok {
		t.Fatalf("vpcSelector.matchLabels not found in findings: %+v", byPath)
	}
	if !f.Ineligible || f.Covered {
		t.Errorf("vpcSelector.matchLabels = %+v, want Ineligible=true Covered=false even though an ancestor tombstone swept it up", f)
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
		"genuine NAMESPACED Selector shape (matchControllerRef + matchLabels + namespace + policy)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchControllerRef": map[string]interface{}{"type": "boolean"},
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"namespace": map[string]interface{}{"type": "string"},
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
		"namespace present but not string-typed": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"namespace": map[string]interface{}{"type": "integer"},
				},
			},
			want: false,
		},
		"four properties, one outside the allowed set (NOT a generated selector despite the property count matching the namespaced shape)": {
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"matchControllerRef": map[string]interface{}{"type": "boolean"},
					"matchLabels": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					"namespace": map[string]interface{}{"type": "string"},
					"comment":   map[string]interface{}{"type": "string"},
				},
			},
			want: false,
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
