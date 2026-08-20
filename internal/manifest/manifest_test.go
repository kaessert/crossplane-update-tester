package manifest

import (
	"strings"
	"testing"
)

// testObjectTypePrefix is a sample object-type prefix used by the
// expect-external-name-prefix tests.
const testObjectTypePrefix = "ipv6network/"

// TestParseBytesExpectExternalNamePrefix covers ParseBytes's
// handling of the optional crossplane.io/expect-external-name-prefix
// annotation: present, present alongside a crossplane.io/update-test block,
// and absent (the common case for every manifest that predates this
// annotation).
func TestParseBytesExpectExternalNamePrefix(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   string
	}{
		"Present": {
			reason: "the annotation value is extracted verbatim",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network-v6
  annotations:
    crossplane.io/expect-external-name-prefix: "ipv6network/"
`,
			want: testObjectTypePrefix,
		},
		"PresentAlongsideUpdateTest": {
			reason: "the prefix annotation and the update-test annotation are independent — parsing one must not consume or interfere with the other",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network-v6
  annotations:
    crossplane.io/expect-external-name-prefix: "ipv6network/"
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
`,
			want: testObjectTypePrefix,
		},
		"Absent": {
			reason: "manifests without the annotation parse with an empty ExpectExternalNamePrefix, not an error — this is the default for every existing example",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
`,
			want: "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if m.ExpectExternalNamePrefix != tc.want {
				t.Errorf("%s: ExpectExternalNamePrefix = %q, want %q", tc.reason, m.ExpectExternalNamePrefix, tc.want)
			}
		})
	}
}

// TestParseBytesExpectExternalNamePrefixAlongsideTests confirms that
// when both annotations are present, the update-test annotation's own
// parsing (Tests, ConvergeSkip) still succeeds unaffected.
func TestParseBytesExpectExternalNamePrefixAlongsideTests(t *testing.T) {
	yaml := `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network-v6
  annotations:
    crossplane.io/expect-external-name-prefix: "ipv6network/"
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
`
	m, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseBytes() error = %v", err)
	}
	if len(m.Tests) != 1 {
		t.Fatalf("expected 1 update test, got %d: %+v", len(m.Tests), m.Tests)
	}
	if m.Tests[0].Field != "comment" {
		t.Errorf("Tests[0].Field = %q, want %q", m.Tests[0].Field, "comment")
	}
}

// TestParseBytesMultiDocumentSelectsAnnotatedDoc covers ParseBytes's document
// selection rule for multi-document ("---"-separated) manifests. A companion
// object (a Secret, a ProviderConfig) shipped in the same file as the managed
// resource has its own non-empty apiVersion/kind/metadata.name, so taking the
// first document unconditionally would test the wrong object and silently
// report zero update tests. The document carrying the update-test annotation
// wins regardless of its position; when none carries it, the first valid
// document is used so annotation-free manifests behave as they always have.
func TestParseBytesMultiDocumentSelectsAnnotatedDoc(t *testing.T) {
	cases := map[string]struct {
		reason        string
		yaml          string
		wantKind      string
		wantName      string
		wantTestCount int
	}{
		"AnnotatedDocIsSecond": {
			reason: "the annotated document is selected even though a valid companion document precedes it",
			yaml: `apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
  namespace: crossplane-system
---
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
`,
			wantKind:      "Network",
			wantName:      "example-network",
			wantTestCount: 1,
		},
		"AnnotatedDocIsFirst": {
			reason: "position is irrelevant — an annotated leading document is still selected over the trailing companion",
			yaml: `apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
---
apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
`,
			wantKind:      "Network",
			wantName:      "example-network",
			wantTestCount: 1,
		},
		"NoAnnotationUsesFirstDoc": {
			reason: "with no annotation anywhere there is nothing to select on, so the first valid document wins — preserving pre-multi-document behaviour",
			yaml: `apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
---
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
`,
			wantKind:      "Secret",
			wantName:      "example-network-credentials",
			wantTestCount: 0,
		},
		"SingleDocument": {
			reason: "a plain single-document manifest is a one-element stream and must parse exactly as before",
			yaml: `apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
`,
			wantKind:      "Network",
			wantName:      "example-network",
			wantTestCount: 1,
		},
		"SkipsTrailingBlankDoc": {
			reason: "a trailing \"---\" produces an empty document that must be skipped rather than selected or treated as a parse error",
			yaml: `apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
---
`,
			wantKind:      "Network",
			wantName:      "example-network",
			wantTestCount: 0,
		},
		"SkipsLeadingBlankAndCommentOnlyDocs": {
			reason: "a leading separator and a comment-only document carry no apiVersion/kind and must not shadow the real resource",
			yaml: `---
# nothing to see here
---
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
`,
			wantKind:      "Network",
			wantName:      "example-network",
			wantTestCount: 1,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if m.Kind != tc.wantKind {
				t.Errorf("%s: Kind = %q, want %q", tc.reason, m.Kind, tc.wantKind)
			}
			if m.Name != tc.wantName {
				t.Errorf("%s: Name = %q, want %q", tc.reason, m.Name, tc.wantName)
			}
			if len(m.Tests) != tc.wantTestCount {
				t.Errorf("%s: len(Tests) = %d, want %d (%+v)", tc.reason, len(m.Tests), tc.wantTestCount, m.Tests)
			}
		})
	}
}

// TestParseBytesMultiDocumentReadsSelectedDocAnnotations verifies that every
// field derived from annotations comes from the *selected* document, not from
// whichever document happened to be first. A companion document carrying its
// own crossplane.io/expect-external-name-prefix must not leak into the parsed
// Manifest.
func TestParseBytesMultiDocumentReadsSelectedDocAnnotations(t *testing.T) {
	yaml := `apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
  annotations:
    crossplane.io/expect-external-name-prefix: "wrong/"
---
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network-v6
  namespace: default
  annotations:
    crossplane.io/expect-external-name-prefix: "ipv6network/"
    crossplane.io/update-test: |
      converge-skip: "status field churns every observe cycle"
      - field: comment
        value: "Updated by update-tester"
`
	m, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseBytes() error = %v", err)
	}
	if m.ExpectExternalNamePrefix != testObjectTypePrefix {
		t.Errorf("ExpectExternalNamePrefix = %q, want %q", m.ExpectExternalNamePrefix, testObjectTypePrefix)
	}
	if m.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", m.Namespace, "default")
	}
	if m.ConvergeSkip != "status field churns every observe cycle" {
		t.Errorf("ConvergeSkip = %q, want the selected document's reason", m.ConvergeSkip)
	}
}

// TestParseBytesInvalidDocuments covers the error paths, which must keep
// their exact pre-multi-document messages: callers and tests match on them.
// Documents lacking apiVersion/kind are skipped during decoding, so a stream
// made up entirely of such documents surfaces as "missing apiVersion or kind"
// rather than as a nil-manifest success.
func TestParseBytesInvalidDocuments(t *testing.T) {
	cases := map[string]struct {
		reason  string
		yaml    string
		wantErr string
	}{
		"Empty": {
			reason:  "an empty stream yields no documents at all",
			yaml:    "",
			wantErr: "manifest missing apiVersion or kind",
		},
		"SeparatorsOnly": {
			reason:  "a stream of blank documents yields no selectable document",
			yaml:    "---\n---\n",
			wantErr: "manifest missing apiVersion or kind",
		},
		"MissingKind": {
			reason:  "a document without kind is not a Kubernetes object and is skipped, leaving nothing to select",
			yaml:    "apiVersion: network.example.crossplane.io/v1alpha1\nmetadata:\n  name: example-network\n",
			wantErr: "manifest missing apiVersion or kind",
		},
		"MissingName": {
			reason:  "the selected document is validated for metadata.name after selection",
			yaml:    "apiVersion: network.example.crossplane.io/v1alpha1\nkind: Network\n",
			wantErr: "manifest missing metadata.name",
		},
		"MalformedYAML": {
			reason:  "a decode failure is reported rather than silently skipped, so typos are not mistaken for a blank document",
			yaml:    "apiVersion: network.example.crossplane.io/v1alpha1\n\tkind: Network\n",
			wantErr: "parsing manifest YAML",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s: ParseBytes() error = nil, want %q", tc.reason, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: ParseBytes() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestParseBytesAssertUnchanged covers the "assert-unchanged:" directive:
// absent (the default for every manifest that predates it), a single field,
// several comma-separated fields with incidental whitespace, and alongside
// converge-skip and the field-entry list in the same annotation.
func TestParseBytesAssertUnchanged(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   []string
	}{
		"Absent": {
			reason: "a manifest without the directive parses with a nil AssertUnchanged, not an error",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "updated"
`,
			want: nil,
		},
		"SingleField": {
			reason: "a single dot-separated field path is extracted verbatim",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      assert-unchanged: ruleChoice.legacyRuleList
      - field: comment
        value: "updated"
`,
			want: []string{"ruleChoice.legacyRuleList"},
		},
		"MultipleFieldsWithWhitespace": {
			reason: "a comma-separated list is split, and surrounding whitespace on each entry is trimmed",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      assert-unchanged: ruleChoice.legacyRuleList,  otherField , thirdField
      - field: comment
        value: "updated"
`,
			want: []string{"ruleChoice.legacyRuleList", "otherField", "thirdField"},
		},
		"AlongsideConvergeSkipAndFieldEntries": {
			reason: "both top-level directives extract independently and the field-entry list still parses",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      converge-skip: "status field churns every observe cycle"
      assert-unchanged: ruleChoice.legacyRuleList
      - field: comment
        value: "updated"
`,
			want: []string{"ruleChoice.legacyRuleList"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if !stringSlicesEqual(m.AssertUnchanged, tc.want) {
				t.Errorf("%s: AssertUnchanged = %#v, want %#v", tc.reason, m.AssertUnchanged, tc.want)
			}
		})
	}
}

// TestParseBytesIgnoreFields covers the "ignore-fields:" directive: absent
// (the default for every manifest that predates it), a single field, several
// comma-separated fields with incidental whitespace, and alongside the other
// two top-level directives and the field-entry list in the same annotation.
func TestParseBytesIgnoreFields(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   []string
	}{
		"Absent": {
			reason: "a manifest without the directive parses with a nil IgnoreFields, not an error",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "updated"
`,
			want: nil,
		},
		"SingleField": {
			reason: "a single field name is extracted verbatim",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      ignore-fields: latestBackup
      - field: comment
        value: "updated"
`,
			want: []string{"latestBackup"},
		},
		"MultipleFieldsWithWhitespace": {
			reason: "a comma-separated list is split, and surrounding whitespace on each entry is trimmed",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      ignore-fields: ruleCount,  dateModified
      - field: comment
        value: "updated"
`,
			want: []string{"ruleCount", "dateModified"},
		},
		"AlongsideConvergeSkipAssertUnchangedAndFieldEntries": {
			reason: "all three top-level directives extract independently and the field-entry list still parses",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      converge-skip: "status field churns every observe cycle"
      assert-unchanged: ruleChoice.legacyRuleList
      ignore-fields: kvm,powerStatus,serverStatus
      - field: comment
        value: "updated"
`,
			want: []string{"kvm", "powerStatus", "serverStatus"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if !stringSlicesEqual(m.IgnoreFields, tc.want) {
				t.Errorf("%s: IgnoreFields = %#v, want %#v", tc.reason, m.IgnoreFields, tc.want)
			}
		})
	}
}

// TestParseBytesIgnoreFieldsRejectsDottedEntry pins the fix for the silent
// no-op: before this, "ignore-fields: ruleChoice.legacyRuleList" parsed
// cleanly, reached ConvergeOptions.IgnoreFields, matched no top-level key in
// differ.DiffSnapshotsExcluding, and let the convergence check fail on drift
// the operator believed they had excluded — with no diagnostic anywhere.
// Rejecting the dotted entry at parse time turns that into a loud,
// immediate, actionable error instead.
func TestParseBytesIgnoreFieldsRejectsDottedEntry(t *testing.T) {
	cases := map[string]struct {
		reason        string
		yaml          string
		wantErrSubstr string
	}{
		"SingleDottedEntry": {
			reason: "a nested path in ignore-fields is rejected, naming the offending entry",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      ignore-fields: ruleChoice.legacyRuleList
      - field: comment
        value: "updated"
`,
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
		"DottedEntryAmongValidOnes": {
			reason: "one bad entry in a comma-separated list is still caught, even alongside otherwise-valid top-level names",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      ignore-fields: latestBackup,ruleChoice.legacyRuleList,kvm
      - field: comment
        value: "updated"
`,
			wantErrSubstr: `ignore-fields entry "ruleChoice.legacyRuleList"`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s: ParseBytes() error = nil, want an error rejecting the dotted ignore-fields entry", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ParseBytes() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationAssertUnchangedRejectsOverlapWithTestedField pins the
// parse-time guard: a field cannot be both an update-test entry's own field
// (patched) and an assert-unchanged field (asserted to never move) in the
// same run — that pairing can never be satisfied and is rejected before any
// cluster is touched, rather than surfacing as a confusing runtime failure
// on the very first field test.
func TestParseAnnotationAssertUnchangedRejectsOverlapWithTestedField(t *testing.T) {
	annotation := `
assert-unchanged: comment
- field: comment
  value: "updated"
`
	_, _, _, _, err := ParseAnnotation(annotation)
	if err == nil {
		t.Fatal("ParseAnnotation() error = nil, want an error rejecting the overlapping field")
	}
	wantSubstr := `assert-unchanged field "comment" is also an update-test field`
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantSubstr)
	}
}

// stringSlicesEqual compares two string slices for equality, treating nil
// and empty as distinct only when one is nil and the other is non-nil with
// elements — a plain reflect.DeepEqual would report nil != []string{} as
// unequal even when both mean "no fields declared", which is not the
// distinction these tests care about testing.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseBytesForProvider verifies that spec.forProvider is decoded onto
// Manifest.ForProvider as plain map[string]interface{}/[]interface{} data —
// the shape validator.CheckMergePatchSiblings needs to simulate an RFC 7386
// merge without a live cluster.
func TestParseBytesForProvider(t *testing.T) {
	cases := map[string]struct {
		reason   string
		yaml     string
		wantKeys []string // top-level keys expected in m.ForProvider, nil means m.ForProvider must be nil
	}{
		"NestedObjectAndScalar": {
			reason: "a nested object field and a scalar field both decode as plain map data with string keys",
			yaml: `apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
spec:
  forProvider:
    interval: 15
    httpHealthCheck:
      path: /healthz
      useOriginServerName: {}
`,
			wantKeys: []string{"interval", "httpHealthCheck"},
		},
		"NoSpec": {
			reason: "a manifest with no spec at all (e.g. a companion Secret document) decodes to a nil ForProvider, not an error",
			yaml: `apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
`,
			wantKeys: nil,
		},
		"SpecWithoutForProvider": {
			reason: "a spec that never mentions forProvider also decodes to a nil ForProvider",
			yaml: `apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
spec:
  providerConfigRef:
    name: default
`,
			wantKeys: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if tc.wantKeys == nil {
				if m.ForProvider != nil {
					t.Errorf("%s: ForProvider = %#v, want nil", tc.reason, m.ForProvider)
				}
				return
			}
			for _, k := range tc.wantKeys {
				if _, ok := m.ForProvider[k]; !ok {
					t.Errorf("%s: ForProvider missing key %q: %#v", tc.reason, k, m.ForProvider)
				}
			}
		})
	}
}

// TestParseBytesForProviderMultiDocumentSelectsAnnotatedDoc verifies that
// ForProvider, like every other field, comes from the SELECTED document (the
// one carrying the update-test annotation) rather than a leading companion
// document — a companion Secret has no spec.forProvider at all, so picking
// the wrong document would silently report a nil ForProvider for a manifest
// that actually declares one.
func TestParseBytesForProviderMultiDocumentSelectsAnnotatedDoc(t *testing.T) {
	yaml := `apiVersion: v1
kind: Secret
metadata:
  name: example-network-credentials
---
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester"
spec:
  forProvider:
    comment: "original"
`
	m, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseBytes() error = %v", err)
	}
	if got, want := m.ForProvider["comment"], "original"; got != want {
		t.Errorf("ForProvider[comment] = %v, want %v", got, want)
	}
}
