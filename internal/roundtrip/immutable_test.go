package roundtrip

import (
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNodeHasImmutableMarker exercises the CEL rule-text matcher in
// isolation, including the one shape it must NOT match: the sibling
// "cannot be removed once set" rule a parent object also carries, which
// names oldSelf but does not declare the current node immutable.
func TestNodeHasImmutableMarker(t *testing.T) {
	cases := map[string]struct {
		reason string
		schema string // YAML, decoded before use
		want   bool
	}{
		"NoValidationsAtAll": {
			reason: "a node with no x-kubernetes-validations key at all is mutable",
			schema: `{type: string}`,
			want:   false,
		},
		"SelfEqualsOldSelf": {
			reason: "the exact CEL comparison validator's own reImmutable matches",
			schema: `{type: string, x-kubernetes-validations: [{rule: "self == oldSelf", message: "immutable"}]}`,
			want:   true,
		},
		"SelfEqualsOldSelfNoSpaces": {
			reason: "the regex tolerates the CEL author omitting spaces around ==",
			schema: `{type: string, x-kubernetes-validations: [{rule: "self==oldSelf", message: "immutable"}]}`,
			want:   true,
		},
		"RemoveGuardRuleDoesNotCount": {
			reason: "the sibling \"cannot be removed once set\" shape names oldSelf but is not \"self == oldSelf\" and must not be misread as immutability",
			schema: `{type: object, x-kubernetes-validations: [{rule: "!has(oldSelf.reusable) || has(self.reusable)", message: "reusable cannot be removed once set"}]}`,
			want:   false,
		},
		"MultipleRulesOneMatches": {
			reason: "a node can carry more than one validation rule; only one needs to match",
			schema: `{type: object, x-kubernetes-validations: [{rule: "size(self) > 0"}, {rule: "self == oldSelf"}]}`,
			want:   true,
		},
		"MultipleRulesNoneMatch": {
			reason: "every rule present is checked; none matching means mutable",
			schema: `{type: object, x-kubernetes-validations: [{rule: "size(self) > 0"}, {rule: "self.foo != ''"}]}`,
			want:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var schema map[string]interface{}
			if err := yaml.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatalf("decoding schema: %v", err)
			}
			got := nodeHasImmutableMarker(schema)
			if got != tc.want {
				t.Errorf("%s: nodeHasImmutableMarker() = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestImmutablePaths covers the full-tree walk, including the inheritance
// step that pushes an ancestor's marker down to every leaf beneath it, and
// confirms it agrees with leafPaths on exactly which nodes are leaves (a
// scalar, an array, an untyped node, and an empty object marker are all
// leaves — see TestLeafPaths in roundtrip_test.go for the same rules
// asserted on leafPaths itself).
func TestImmutablePaths(t *testing.T) {
	cases := map[string]struct {
		reason string
		schema string // YAML, decoded before use
		want   []string
	}{
		"NilSchema": {
			reason: "a non-map schema node contributes nothing",
			schema: `null`,
			want:   nil,
		},
		"NoMarkersAnywhere": {
			reason: "an entirely mutable object yields an empty set",
			schema: `
type: object
properties:
  name: {type: string}
  size: {type: integer}
`,
			want: nil,
		},
		"LeafOwnMarker": {
			reason: "a leaf field carrying its own self == oldSelf marker is immutable; an unmarked sibling is not",
			schema: `
type: object
properties:
  name: {type: string}
  region:
    type: string
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
`,
			want: []string{"region"},
		},
		"AncestorMarkerPropagatesToEveryLeafBeneath": {
			reason: "a marker on an object node (e.g. \"capabilities is immutable after creation\") covers every leaf nested under it, mirroring the real tailscale TailnetKey.capabilities shape — an unmarked top-level sibling stays mutable",
			schema: `
type: object
properties:
  capabilities:
    type: object
    x-kubernetes-validations: [{rule: "self == oldSelf", message: "capabilities is immutable after creation"}]
    properties:
      devices:
        type: object
        properties:
          create:
            type: object
            properties:
              ephemeral: {type: boolean}
              reusable: {type: boolean}
  description: {type: string}
`,
			want: []string{"capabilities.devices.create.ephemeral", "capabilities.devices.create.reusable"},
		},
		"ArrayIsALeafAndCanBeImmutable": {
			reason: "an array field is a leaf (its item schema is never descended into, matching leafPaths) and can itself carry an immutability marker",
			schema: `
type: object
properties:
  tags:
    type: array
    items: {type: string}
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
`,
			want: []string{"tags"},
		},
		"EmptyObjectMarkerIsALeafAndCanBeImmutable": {
			reason: "type: object with no properties (an empty oneof-selector struct) is a leaf, not descended into, and can carry its own marker",
			schema: `
type: object
properties:
  selector:
    type: object
    nullable: true
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
`,
			want: []string{"selector"},
		},
		"UntypedNodeIsALeafEvenWithProperties": {
			reason: "a schema node with properties but no explicit type: object is a leaf, matching leafPaths' own untyped-node rule — its marker still applies to itself as a leaf, not descended into",
			schema: `
type: object
properties:
  untyped:
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
    properties:
      inner: {type: string}
`,
			want: []string{"untyped"},
		},
		"MixedImmutableAndMutableSiblings": {
			reason: "immutability is per-field, not all-or-nothing for the whole resource",
			schema: `
type: object
properties:
  ephemeral:
    type: boolean
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
  preauthorized: {type: boolean}
  reusable:
    type: boolean
    x-kubernetes-validations: [{rule: "self == oldSelf"}]
`,
			want: []string{"ephemeral", "reusable"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var schema interface{}
			if err := yaml.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatalf("decoding schema: %v", err)
			}
			gotSet := immutablePaths(schema)
			var got []string
			for p := range gotSet {
				got = append(got, p)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: immutablePaths() = %v, want %v", tc.reason, got, want)
			}
		})
	}
}

// immutableTailnetKeyCRD is a trimmed, schema-faithful subset of the real
// provider-tailscale TailnetKey CRD (both scopes carry this shape) — the
// fleet's worst case measured for this ticket: every declared
// spec.forProvider field is CEL-immutable, each via its own leaf-level
// marker.
const immutableTailnetKeyCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: tailscale.crossplane.io
  names:
    kind: TailnetKey
    plural: tailnetkeys
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
                  description:
                    type: string
                    x-kubernetes-validations: [{rule: "self == oldSelf"}]
                  ephemeral:
                    type: boolean
                    x-kubernetes-validations: [{rule: "self == oldSelf"}]
                  preauthorized:
                    type: boolean
                    x-kubernetes-validations: [{rule: "self == oldSelf"}]
                  reusable:
                    type: boolean
                    x-kubernetes-validations: [{rule: "self == oldSelf"}]
                  tags:
                    type: array
                    items: {type: string}
                    x-kubernetes-validations: [{rule: "self == oldSelf"}]
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  id:
                    type: string
`

// immutableTailnetKeyObject is a live object shape matching
// immutableTailnetKeyCRD: every spec.forProvider field is set, and NONE of
// them is ever mirrored into status.atProvider — the exact
// present-in-spec-absent-from-mirror-forever shape a write-only field
// produces, which is what makes it CEL-immutable in the first place (the
// backend has no way to echo a value it never returns).
const immutableTailnetKeyObject = `{
  "apiVersion": "tailscale.crossplane.io/v1alpha1",
  "kind": "TailnetKey",
  "metadata": { "name": "example-key" },
  "spec": {
    "forProvider": {
      "description": "ci key",
      "ephemeral": true,
      "preauthorized": true,
      "reusable": true,
      "tags": ["tag:ci"]
    }
  },
  "status": {
    "atProvider": {
      "id": "k-example"
    }
  }
}`

// TestDiffReportSetsImmutableOnRows confirms DiffReport itself wires
// immutablePaths' result through to Row.Immutable — the integration point
// DenominatorReport depends on — using a real fleet shape (see the fixture
// comments above) rather than only the unit-level immutablePaths cases.
func TestDiffReportSetsImmutableOnRows(t *testing.T) {
	crd := mustDecodeCRD(t, immutableTailnetKeyCRD)
	obj := mustDecodeObject(t, immutableTailnetKeyObject)

	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport: %v", err)
	}

	for _, path := range []string{"description", "ephemeral", "preauthorized", "reusable", "tags"} {
		row := rowByPath(t, rows, path)
		if !row.Immutable {
			t.Errorf("row %q: Immutable = false, want true (marked self == oldSelf in the fixture CRD)", path)
		}
		if row.Classification != ClassPresentInSpecAbsentFromMirror {
			t.Errorf("row %q: Classification = %q, want %q (never mirrored, by construction)", path, row.Classification, ClassPresentInSpecAbsentFromMirror)
		}
	}
}

// TestDiffReportPresentInMirrorAbsentFromSpecIsNeverImmutable confirms a
// row with no forProvider counterpart at all is never reported immutable —
// there is no spec-side schema node for a marker to live on.
func TestDiffReportPresentInMirrorAbsentFromSpecIsNeverImmutable(t *testing.T) {
	crd := mustDecodeCRD(t, immutableTailnetKeyCRD)
	obj := mustDecodeObject(t, immutableTailnetKeyObject)

	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport: %v", err)
	}

	row := rowByPath(t, rows, "id")
	if row.Classification != ClassPresentInMirrorAbsentFromSpec {
		t.Fatalf("row \"id\": Classification = %q, want %q", row.Classification, ClassPresentInMirrorAbsentFromSpec)
	}
	if row.Immutable {
		t.Errorf("row \"id\": Immutable = true, want false — present-in-mirror-absent-from-spec has no forProvider schema node to carry a marker")
	}
}
