package manifest

import (
	"reflect"
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

// TestParseBytesClearField proves a "clear:" key on an update-test entry
// parses onto UpdateTest.Clear, and that a manifest with no "clear:" key at
// all parses with a nil Clear — the fleet's 727 pre-existing entries carry
// no such key, so this is the "additive only" case every one of them must
// keep matching.
func TestParseBytesClearField(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   []string
	}{
		"Absent": {
			reason: "an entry with no clear key parses with a nil Clear, not an error",
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
		"SingleSibling": {
			reason: "a single-element clear list is parsed verbatim",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: botProtectionSetting
        value: {}
        clear: [defaultBotSetting]
`,
			want: []string{"defaultBotSetting"},
		},
		"MultipleSiblings": {
			reason: "every member of the union group is preserved, not just the first",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: armA
        value: "x"
        clear: [armB, armC]
`,
			want: []string{"armB", "armC"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if len(m.Tests) != 1 {
				t.Fatalf("%s: len(Tests) = %d, want 1", tc.reason, len(m.Tests))
			}
			if !stringSlicesEqual(m.Tests[0].Clear, tc.want) {
				t.Errorf("%s: Tests[0].Clear = %#v, want %#v", tc.reason, m.Tests[0].Clear, tc.want)
			}
		})
	}
}

// TestParseAnnotationRejectsUnsupportedClearShapes pins ValidateClear's three
// rejections at the annotation-parsing entry point — before any cluster is
// touched — mirroring the ignore-fields dotted-entry rejection shape.
func TestParseAnnotationRejectsUnsupportedClearShapes(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"NestedFieldWithClear": {
			reason: "sibling-clearing at a non-root nesting level is not supported",
			annotation: `
- field: parent.child
  value: "x"
  clear: [otherTopLevel]
`,
			wantErrSubstr: `clear is only supported for a top-level field; "parent.child" is nested`,
		},
		"DottedClearEntry": {
			reason: "a clear entry naming a nested path is rejected the same way ignore-fields rejects one",
			annotation: `
- field: botProtectionSetting
  value: {}
  clear: [nested.sibling]
`,
			wantErrSubstr: `clear entry "nested.sibling": dot-separated paths are not supported`,
		},
		"ClearNamesFieldItself": {
			reason: "clear must name OTHER siblings, not the field being patched",
			annotation: `
- field: botProtectionSetting
  value: {}
  clear: [botProtectionSetting]
`,
			wantErrSubstr: `clear entry "botProtectionSetting": clear must name OTHER sibling fields`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := ParseAnnotation(tc.annotation)
			if err == nil {
				t.Fatalf("%s: ParseAnnotation() error = nil, want an error", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ParseAnnotation() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseBytesWithValuesField proves a "withValues:" key on an
// update-test entry parses onto UpdateTest.WithValues, and that a manifest
// with no "withValues:" key at all parses with a nil WithValues — mirroring
// TestParseBytesClearField for the additive sibling-literal-value route.
func TestParseBytesWithValuesField(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   map[string]interface{}
	}{
		"Absent": {
			reason: "an entry with no withValues key parses with a nil WithValues, not an error",
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
		"SingleSibling": {
			reason: "a single-entry withValues map is parsed verbatim, carrying a real non-null literal",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: tags
        value: []
        withValues:
          tag: ""
`,
			want: map[string]interface{}{"tag": ""},
		},
		"MultipleSiblings": {
			reason: "every named sibling's literal value is preserved, not just the first",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: armA
        value: "x"
        withValues:
          armB: "y"
          armC: "z"
`,
			want: map[string]interface{}{"armB": "y", "armC": "z"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if len(m.Tests) != 1 {
				t.Fatalf("%s: len(Tests) = %d, want 1", tc.reason, len(m.Tests))
			}
			if !reflect.DeepEqual(m.Tests[0].WithValues, tc.want) {
				t.Errorf("%s: Tests[0].WithValues = %#v, want %#v", tc.reason, m.Tests[0].WithValues, tc.want)
			}
		})
	}
}

// TestParseAnnotationRejectsUnsupportedWithValuesShapes pins
// ValidateWithValues's rejections at the annotation-parsing entry point —
// before any cluster is touched — mirroring
// TestParseAnnotationRejectsUnsupportedClearShapes for the additive
// withValues mechanism, plus the one shape unique to withValues: a sibling
// named in both clear and withValues in the same entry.
func TestParseAnnotationRejectsUnsupportedWithValuesShapes(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"NestedFieldWithWithValues": {
			reason: "sibling-value patching at a non-root nesting level is not supported",
			annotation: `
- field: parent.child
  value: "x"
  withValues:
    otherTopLevel: "y"
`,
			wantErrSubstr: `withValues is only supported for a top-level field; "parent.child" is nested`,
		},
		"DottedWithValuesKey": {
			reason: "a withValues key naming a nested path is rejected the same way clear rejects one",
			annotation: `
- field: tags
  value: []
  withValues:
    nested.sibling: "x"
`,
			wantErrSubstr: `withValues entry "nested.sibling": dot-separated paths are not supported`,
		},
		"WithValuesNamesFieldItself": {
			reason: "withValues must name OTHER siblings, not the field being patched",
			annotation: `
- field: tags
  value: []
  withValues:
    tags: ["x"]
`,
			wantErrSubstr: `withValues entry "tags": withValues must name OTHER sibling fields`,
		},
		"KeyInBothClearAndWithValues": {
			reason: "a sibling cannot be both nulled and given a literal value in the same merge patch",
			annotation: `
- field: tags
  value: []
  clear: [tag]
  withValues:
    tag: ""
`,
			wantErrSubstr: `withValues entry "tag": also named in this entry's clear list`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := ParseAnnotation(tc.annotation)
			if err == nil {
				t.Fatalf("%s: ParseAnnotation() error = nil, want an error", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ParseAnnotation() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationExplicitNullValueIsAcceptedAsSelfTombstone proves the
// mechanism this ticket adds: an entry whose "value:" key is present but
// null (spelled as "null", "~", or a bare "value:" with nothing after the
// colon) parses successfully with ValueExplicit set, rather than tripping
// the "value is required" rejection — the whole-field self-tombstone route
// for a leaf with no sibling top-level field able to host a clear: entry
// (see roundtrip.ContainerClearCoverage's self-tombstone case).
func TestParseAnnotationExplicitNullValueIsAcceptedAsSelfTombstone(t *testing.T) {
	cases := map[string]struct {
		reason     string
		annotation string
	}{
		"NullKeyword": {
			reason: "value: null spells the explicit null out",
			annotation: `
- field: rules
  value: null
`,
		},
		"TildeKeyword": {
			reason: "value: ~ is YAML's other null spelling",
			annotation: `
- field: rules
  value: ~
`,
		},
		"BareColon": {
			reason: "a bare value: with nothing after the colon is also an explicit null in YAML",
			annotation: `
- field: rules
  value:
`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tests, _, _, _, err := ParseAnnotation(tc.annotation)
			if err != nil {
				t.Fatalf("%s: ParseAnnotation() error = %v, want nil", tc.reason, err)
			}
			if len(tests) != 1 {
				t.Fatalf("%s: len(tests) = %d, want 1", tc.reason, len(tests))
			}
			if tests[0].Value != nil {
				t.Errorf("%s: tests[0].Value = %#v, want nil", tc.reason, tests[0].Value)
			}
			if !tests[0].ValueExplicit {
				t.Errorf("%s: tests[0].ValueExplicit = false, want true", tc.reason)
			}
		})
	}
}

// TestParseAnnotationAbsentValueStillRejected is the companion negative
// case: an entry with NO "value:" key at all — the pre-existing rejection —
// must still fail parsing exactly as before. ValueExplicit's addition is
// additive only; it must not loosen this check for the genuinely
// incomplete entry it exists to catch.
func TestParseAnnotationAbsentValueStillRejected(t *testing.T) {
	annotation := `
- field: rules
`
	_, _, _, _, err := ParseAnnotation(annotation)
	if err == nil {
		t.Fatal("ParseAnnotation() error = nil, want an error for a genuinely absent value: key")
	}
	if !strings.Contains(err.Error(), "value is required unless skip is set") {
		t.Errorf("ParseAnnotation() error = %q, want it to contain the value-required message", err.Error())
	}
}

// TestParseAnnotationValueExplicitFalseForOrdinaryEntries confirms
// ValueExplicit does not leak true onto every entry regardless of shape —
// only entries whose value: key is present carry it, exactly as an
// ordinary present-and-non-null value already implies.
func TestParseAnnotationValueExplicitFalseForOrdinaryEntries(t *testing.T) {
	annotation := `
- field: comment
  value: "hello"
`
	tests, _, _, _, err := ParseAnnotation(annotation)
	if err != nil {
		t.Fatalf("ParseAnnotation() error = %v, want nil", err)
	}
	if len(tests) != 1 {
		t.Fatalf("len(tests) = %d, want 1", len(tests))
	}
	if !tests[0].ValueExplicit {
		t.Error("ValueExplicit = false for an entry whose value: key is present and non-null, want true")
	}
	if tests[0].Value != "hello" {
		t.Errorf("Value = %#v, want %q", tests[0].Value, "hello")
	}
}

// TestParseAnnotationRejectsKnownDefectKey pins the removal of the
// knownDefect route: a "knownDefect:" key on an update-test entry is now a
// parse-time error naming the closed skip: reason set and the withValues:
// route, not a silently ignored unknown key. Ordinary entries with no such
// key, including ones that use skip: or withValues: for their own reasons,
// are unaffected.
func TestParseAnnotationRejectsKnownDefectKey(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"KnownDefectKeyIsRejected": {
			reason: "the knownDefect route no longer exists; a manifest still carrying the key must fail loudly rather than silently stop being enforced",
			annotation: `
- field: useTls
  value: true
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: `carries a "knownDefect:" key, which no longer exists`,
		},
		"ErrorNamesTheReplacementRoutes": {
			reason: "the error must point the reader at what to use instead, not just say the key is gone",
			annotation: `
- field: useTls
  value: true
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: "vendor-defect",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := ParseAnnotation(tc.annotation)
			if err == nil {
				t.Fatalf("%s: ParseAnnotation() error = nil, want an error", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ParseAnnotation() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseBytesNoKnownDefectKeyStillWorks proves an ordinary manifest with
// no knownDefect: key anywhere parses exactly as before — the additive-only
// case every pre-existing entry in the fleet must keep matching now that the
// key is a parse error rather than an accepted-and-ignored one.
func TestParseBytesNoKnownDefectKeyStillWorks(t *testing.T) {
	yaml := `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: comment
        value: "updated"
`
	m, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseBytes() error = %v", err)
	}
	if len(m.Tests) != 1 {
		t.Fatalf("len(Tests) = %d, want 1", len(m.Tests))
	}
	if m.Tests[0].Field != "comment" {
		t.Errorf("Tests[0].Field = %q, want %q", m.Tests[0].Field, "comment")
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

// TestParseAnnotationStructuredSkipAccepted pins every structured skip:
// reason ParseAnnotation accepts, and what each one records on the parsed
// SkipInfo.
func TestParseAnnotationStructuredSkipAccepted(t *testing.T) {
	cases := map[string]struct {
		reason     string
		annotation string
		want       SkipInfo
	}{
		"UnionArm": {
			reason: "union-arm carries the sibling field name",
			annotation: `
- field: allowList
  skip:
    reason: union-arm
    sibling: ruleList
`,
			want: SkipInfo{Reason: SkipUnionArm, Sibling: "ruleList"},
		},
		"CoveredElsewhere": {
			reason: "covered-elsewhere carries the by: pointer",
			annotation: `
- field: comment
  skip:
    reason: covered-elsewhere
    by: examples/x/y.yaml#comment
`,
			want: SkipInfo{Reason: SkipCoveredElsewhere, By: "examples/x/y.yaml#comment"},
		},
		"VendorDefect": {
			reason: "vendor-defect carries both evidence: and ticket:",
			annotation: `
- field: dnsVolterraManaged
  skip:
    reason: vendor-defect
    evidence: "HTTP 400 'Change of domain type ... is not supported'"
    ticket: FX-DNS-DELEGATION
`,
			want: SkipInfo{
				Reason:   SkipVendorDefect,
				Evidence: "HTTP 400 'Change of domain type ... is not supported'",
				Ticket:   "FX-DNS-DELEGATION",
			},
		},
		"FixtureMissing": {
			reason: "fixture-missing carries both evidence: and ticket:",
			annotation: `
- field: firewallGroupId
  skip:
    reason: fixture-missing
    evidence: "no fixture backend exposes a second firewall group to move this field between"
    ticket: VU-FW-FIXTURE
`,
			want: SkipInfo{
				Reason:   SkipFixtureMissing,
				Evidence: "no fixture backend exposes a second firewall group to move this field between",
				Ticket:   "VU-FW-FIXTURE",
			},
		},
		"WriteOnly": {
			reason: "write-only carries no companion keys",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
`,
			want: SkipInfo{Reason: SkipWriteOnly},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tests, _, _, _, err := ParseAnnotation(tc.annotation)
			if err != nil {
				t.Fatalf("%s: ParseAnnotation() error = %v", tc.reason, err)
			}
			if len(tests) != 1 {
				t.Fatalf("%s: got %d entries, want 1", tc.reason, len(tests))
			}
			if got := tests[0].Skip; got != tc.want {
				t.Errorf("%s: Skip = %+v, want %+v", tc.reason, got, tc.want)
			}
			if !tests[0].Skip.Present() {
				t.Errorf("%s: Skip.Present() = false, want true", tc.reason)
			}
			if tests[0].Skip.Legacy {
				t.Errorf("%s: Skip.Legacy = true, want false for a structured entry", tc.reason)
			}
		})
	}
}

// TestParseAnnotationStructuredSkipRejectsInvalidShapes pins every
// parse-time rejection a structured skip: entry can trigger: an unknown
// reason, the "immutable" reason specifically, and each reason's own
// required companion keys being absent or malformed.
func TestParseAnnotationStructuredSkipRejectsInvalidShapes(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"UnknownReason": {
			reason: "a reason outside the closed set is rejected naming the valid set",
			annotation: `
- field: comment
  skip:
    reason: not-a-real-reason
`,
			wantErrSubstr: "not one of the valid reasons",
		},
		"ImmutableRejectedByName": {
			reason: "immutable is not a skip: reason — it is derived mechanically from the CEL marker",
			annotation: `
- field: region
  skip:
    reason: immutable
`,
			wantErrSubstr: "self == oldSelf",
		},
		"UnionArmMissingSibling": {
			reason: "union-arm requires a non-empty sibling:",
			annotation: `
- field: allowList
  skip:
    reason: union-arm
`,
			wantErrSubstr: "requires a non-empty sibling:",
		},
		"CoveredElsewhereMissingBy": {
			reason: "covered-elsewhere requires a non-empty by:",
			annotation: `
- field: comment
  skip:
    reason: covered-elsewhere
`,
			wantErrSubstr: "requires a non-empty by:",
		},
		"CoveredElsewhereMalformedBy": {
			reason: "covered-elsewhere's by: must be shaped <path>#<field>",
			annotation: `
- field: comment
  skip:
    reason: covered-elsewhere
    by: examples/x/y.yaml
`,
			wantErrSubstr: "must be shaped",
		},
		"VendorDefectMissingEvidence": {
			reason: "vendor-defect requires both evidence: and ticket:",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    ticket: FX-DNS-DELEGATION
`,
			wantErrSubstr: "requires both evidence: and ticket:",
		},
		"VendorDefectMissingTicket": {
			reason: "vendor-defect requires both evidence: and ticket:",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    evidence: "observed a 400"
`,
			wantErrSubstr: "requires both evidence: and ticket:",
		},
		"FixtureMissingNoTicketOrEvidence": {
			reason: "fixture-missing requires both evidence: and ticket:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
`,
			wantErrSubstr: "requires both evidence: and ticket:",
		},
		"FixtureMissingTicketOnly": {
			reason: "fixture-missing requires evidence: as well as ticket:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
    ticket: VU-FW-FIXTURE
`,
			wantErrSubstr: "requires both evidence: and ticket:",
		},
		"FixtureMissingEvidenceOnly": {
			reason: "fixture-missing requires ticket: as well as evidence:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
    evidence: "no fixture backend exposes a second firewall group to move this field between"
`,
			wantErrSubstr: "requires both evidence: and ticket:",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := ParseAnnotation(tc.annotation)
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: error = %q, want substring %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestSkipInfoUnmarshalYAMLLegacyString confirms the pre-migration bare
// string shape still parses, is credited as Legacy, and an empty string (or
// an absent skip: key) still decodes to the zero value — matching the
// pre-this-ticket behaviour where an empty string meant "no skip declared".
func TestSkipInfoUnmarshalYAMLLegacyString(t *testing.T) {
	tests, _, _, _, err := ParseAnnotation(`
- field: comment
  skip: "not exercised in this example"
- field: owner
  value: "updated-owner"
`)
	if err != nil {
		t.Fatalf("ParseAnnotation() error = %v", err)
	}
	if len(tests) != 2 {
		t.Fatalf("got %d entries, want 2", len(tests))
	}

	commentSkip := tests[0].Skip
	if !commentSkip.Legacy {
		t.Errorf("comment: Skip.Legacy = false, want true for a bare string skip:")
	}
	if commentSkip.LegacyText != "not exercised in this example" {
		t.Errorf("comment: Skip.LegacyText = %q, want %q", commentSkip.LegacyText, "not exercised in this example")
	}
	if !commentSkip.Present() {
		t.Errorf("comment: Skip.Present() = false, want true")
	}

	if got := tests[1].Skip; got.Present() {
		t.Errorf("owner: Skip = %+v, want the zero value (no skip: key present)", got)
	}
}

// TestSkipInfoDescribe pins the human-readable rendering SkipMsg (see
// runner.TestResult) uses for each shape a SkipInfo can hold.
func TestSkipInfoDescribe(t *testing.T) {
	cases := map[string]struct {
		reason string
		skip   SkipInfo
		want   string
	}{
		"Legacy": {
			reason: "the legacy form renders its own free-prose text verbatim",
			skip:   LegacySkip("not exercised in this example"),
			want:   "not exercised in this example",
		},
		"UnionArm": {
			reason: "union-arm names its sibling",
			skip:   SkipInfo{Reason: SkipUnionArm, Sibling: "ruleList"},
			want:   "union-arm (sibling: ruleList)",
		},
		"CoveredElsewhere": {
			reason: "covered-elsewhere names its by: pointer",
			skip:   SkipInfo{Reason: SkipCoveredElsewhere, By: "examples/x/y.yaml#comment"},
			want:   "covered-elsewhere (by: examples/x/y.yaml#comment)",
		},
		"VendorDefect": {
			reason: "vendor-defect names both evidence and ticket",
			skip:   SkipInfo{Reason: SkipVendorDefect, Evidence: "observed a 400", Ticket: "FX-DNS-DELEGATION"},
			want:   "vendor-defect (observed a 400; ticket: FX-DNS-DELEGATION)",
		},
		"FixtureMissing": {
			reason: "fixture-missing names both its evidence and its ticket",
			skip: SkipInfo{
				Reason:   SkipFixtureMissing,
				Evidence: "no fixture backend exposes a second firewall group to move this field between",
				Ticket:   "VU-FW-FIXTURE",
			},
			want: "fixture-missing (no fixture backend exposes a second firewall group to move this field between; ticket: VU-FW-FIXTURE)",
		},
		"WriteOnly": {
			reason: "write-only carries no companion data to render",
			skip:   SkipInfo{Reason: SkipWriteOnly},
			want:   "write-only",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.skip.Describe(); got != tc.want {
				t.Errorf("%s: Describe() = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// TestValidateIgnoreMapKeys pins ValidateIgnoreMapKeys's direct rejections:
// paired with skip, an empty entry, and a repeated entry — plus the
// pass-through cases (absent, and a well-formed non-empty list).
func TestValidateIgnoreMapKeys(t *testing.T) {
	cases := map[string]struct {
		reason        string
		test          UpdateTest
		wantErrSubstr string // empty means ValidateIgnoreMapKeys must return nil
	}{
		"Absent": {
			reason: "no ignoreMapKeys key at all is the common case and must not error",
			test:   UpdateTest{Field: "extAttrs", Value: map[string]interface{}{"a": "1"}},
		},
		"WellFormed": {
			reason: "a well-formed, non-empty list on an ordinary entry passes",
			test: UpdateTest{
				Field:         "extAttrs",
				Value:         map[string]interface{}{"a": "1"},
				IgnoreMapKeys: []string{"ownerStamp"},
			},
		},
		"PairedWithSkip": {
			reason:        "skip already asserts no comparison exists to write, so ignoreMapKeys has nothing left to affect",
			test:          UpdateTest{Field: "extAttrs", Skip: LegacySkip("no test written yet"), IgnoreMapKeys: []string{"ownerStamp"}},
			wantErrSubstr: "ignoreMapKeys is set but skip is also set",
		},
		"EmptyEntry": {
			reason:        "an empty string names no map member key",
			test:          UpdateTest{Field: "extAttrs", Value: map[string]interface{}{"a": "1"}, IgnoreMapKeys: []string{""}},
			wantErrSubstr: "ignoreMapKeys entry is empty",
		},
		"RepeatedEntry": {
			reason:        "the same key named twice is not a second exclusion",
			test:          UpdateTest{Field: "extAttrs", Value: map[string]interface{}{"a": "1"}, IgnoreMapKeys: []string{"ownerStamp", "ownerStamp"}},
			wantErrSubstr: `ignoreMapKeys entry "ownerStamp" is repeated`,
		},
		"CollidesWithExpect": {
			reason: "a key named in both ignoreMapKeys and expect: is stripped from both sides of the comparison " +
				"before it is ever checked, so the field test passes vacuously regardless of the live value",
			test: UpdateTest{
				Field:         "extAttrs",
				Value:         map[string]interface{}{"existingKey": "updated", "ownerStamp": "x"},
				Expect:        map[string]interface{}{"existingKey": "updated"},
				IgnoreMapKeys: []string{"existingKey", "ownerStamp"},
			},
			wantErrSubstr: `ignoreMapKeys entry "existingKey" also appears as a key in this entry's own expect:`,
		},
		"CollidesWithValueNoExpect": {
			reason: "with no expect:, value: is the effective expectation the runner compares against, so a " +
				"collision there is exactly as vacuous as colliding with expect:",
			test: UpdateTest{
				Field:         "extAttrs",
				Value:         map[string]interface{}{"existingKey": "updated"},
				IgnoreMapKeys: []string{"existingKey"},
			},
			wantErrSubstr: `ignoreMapKeys entry "existingKey" also appears as a key in this entry's own value:`,
		},
		"NamedOnlyInIgnoreMapKeys": {
			reason: "the legitimate shape: ignoreMapKeys names the provider-injected member that expect: " +
				"deliberately never mentions",
			test: UpdateTest{
				Field:         "extAttrs",
				Value:         map[string]interface{}{"existingKey": "updated"},
				Expect:        map[string]interface{}{"existingKey": "updated"},
				IgnoreMapKeys: []string{"ownerStamp"},
			},
		},
		"NonObjectExpectation": {
			reason: "a scalar or list expectation has no keys to collide with, so ignoreMapKeys is accepted " +
				"even though it will never match anything on that side",
			test: UpdateTest{
				Field:         "count",
				Value:         "5",
				IgnoreMapKeys: []string{"ownerStamp"},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateIgnoreMapKeys(tc.test)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("%s: ValidateIgnoreMapKeys() error = %v, want nil", tc.reason, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: ValidateIgnoreMapKeys() error = nil, want an error containing %q", tc.reason, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ValidateIgnoreMapKeys() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationIgnoreMapKeys proves an "ignoreMapKeys:" key on an
// update-test entry parses onto UpdateTest.IgnoreMapKeys, that an entry with
// no such key parses with a nil IgnoreMapKeys (the common case every
// pre-existing fleet entry must keep matching), and that ParseAnnotation
// rejects the entry-level combination ValidateIgnoreMapKeys itself rejects
// (skip paired with ignoreMapKeys) at the same entry point every other
// per-entry validation runs through.
func TestParseAnnotationIgnoreMapKeys(t *testing.T) {
	t.Run("Absent", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: comment
  value: "updated"
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v", err)
		}
		if len(tests) != 1 {
			t.Fatalf("len(tests) = %d, want 1", len(tests))
		}
		if tests[0].IgnoreMapKeys != nil {
			t.Errorf("IgnoreMapKeys = %v, want nil", tests[0].IgnoreMapKeys)
		}
	})

	t.Run("Present", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: extAttrs
  value: {NewKey: "v1", ExistingKey: "updated", RemovedKey: null}
  expect: {NewKey: "v1", ExistingKey: "updated"}
  ignoreMapKeys: [OwnerStamp]
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v", err)
		}
		if len(tests) != 1 {
			t.Fatalf("len(tests) = %d, want 1", len(tests))
		}
		want := []string{"OwnerStamp"}
		got := tests[0].IgnoreMapKeys
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("IgnoreMapKeys = %v, want %v", got, want)
		}
	})

	t.Run("RejectsSkipCombination", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: extAttrs
  skip: "no test written yet"
  ignoreMapKeys: [OwnerStamp]
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting skip + ignoreMapKeys")
		}
		wantErrSubstr := "ignoreMapKeys is set but skip is also set"
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})

	// RejectsVacuousSelfCollision reproduces, through the real annotation
	// parse path, the exact expected/actual pair this check exists to catch:
	// existingKey is named in both expect: and ignoreMapKeys, so the
	// comparison would strip it from both sides and report PASS against a
	// live value ("NOT-updated") that never actually converged.
	t.Run("RejectsVacuousSelfCollision", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: extAttrs
  value: {existingKey: "updated", ownerStamp: "x"}
  expect: {existingKey: "updated"}
  ignoreMapKeys: [existingKey, ownerStamp]
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting the existingKey self-collision")
		}
		wantErrSubstr := `ignoreMapKeys entry "existingKey" also appears as a key in this entry's own expect:`
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})
}

// TestValidateIgnoreListElementKeys pins ValidateIgnoreListElementKeys's
// direct rejections: paired with skip, an empty entry, and a repeated
// entry — plus the pass-through cases (absent, and a well-formed non-empty
// list) — mirroring TestValidateIgnoreMapKeys one level deeper: the
// collision scan here looks at each ELEMENT of a list-shaped expectation,
// not at the expectation itself.
func TestValidateIgnoreListElementKeys(t *testing.T) {
	cases := map[string]struct {
		reason        string
		test          UpdateTest
		wantErrSubstr string // empty means ValidateIgnoreListElementKeys must return nil
	}{
		"Absent": {
			reason: "no ignoreListElementKeys key at all is the common case and must not error",
			test:   UpdateTest{Field: "firewallRules", Value: []interface{}{map[string]interface{}{"port": "80"}}},
		},
		"WellFormed": {
			reason: "a well-formed, non-empty list on an ordinary entry passes",
			test: UpdateTest{
				Field:                 "firewallRules",
				Value:                 []interface{}{map[string]interface{}{"port": "80"}},
				IgnoreListElementKeys: []string{"id"},
			},
		},
		"PairedWithSkip": {
			reason:        "skip already asserts no comparison exists to write, so ignoreListElementKeys has nothing left to affect",
			test:          UpdateTest{Field: "firewallRules", Skip: LegacySkip("no test written yet"), IgnoreListElementKeys: []string{"id"}},
			wantErrSubstr: "ignoreListElementKeys is set but skip is also set",
		},
		"EmptyEntry": {
			reason:        "an empty string names no per-element key",
			test:          UpdateTest{Field: "firewallRules", Value: []interface{}{map[string]interface{}{"port": "80"}}, IgnoreListElementKeys: []string{""}},
			wantErrSubstr: "ignoreListElementKeys entry is empty",
		},
		"RepeatedEntry": {
			reason:        "the same key named twice is not a second exclusion",
			test:          UpdateTest{Field: "firewallRules", Value: []interface{}{map[string]interface{}{"port": "80"}}, IgnoreListElementKeys: []string{"id", "id"}},
			wantErrSubstr: `ignoreListElementKeys entry "id" is repeated`,
		},
		"CollidesWithExpect": {
			reason: "a key named in both ignoreListElementKeys and an element of expect: is stripped from every " +
				"element on both sides of the comparison before it is ever checked, so the field test passes " +
				"vacuously regardless of the live value",
			test: UpdateTest{
				Field: "firewallRules",
				Value: []interface{}{map[string]interface{}{"port": "443", "id": "x"}},
				Expect: []interface{}{
					map[string]interface{}{"port": "443", "id": "guessed"},
				},
				IgnoreListElementKeys: []string{"id"},
			},
			wantErrSubstr: `ignoreListElementKeys entry "id" also appears as a key in one of this entry's own expect: elements`,
		},
		"CollidesWithValueNoExpect": {
			reason: "with no expect:, value: is the effective expectation the runner compares against, so a " +
				"collision there is exactly as vacuous as colliding with expect:",
			test: UpdateTest{
				Field:                 "firewallRules",
				Value:                 []interface{}{map[string]interface{}{"port": "443", "id": "x"}},
				IgnoreListElementKeys: []string{"id"},
			},
			wantErrSubstr: `ignoreListElementKeys entry "id" also appears as a key in one of this entry's own value: elements`,
		},
		"CollidesOnOnlyOneOfSeveralElements": {
			reason: "the redundancy scan checks EVERY element, not just the first — a collision on the second " +
				"element is caught exactly the same way",
			test: UpdateTest{
				Field: "firewallRules",
				Value: []interface{}{
					map[string]interface{}{"port": "443"},
					map[string]interface{}{"port": "8080", "id": "x"},
				},
				IgnoreListElementKeys: []string{"id"},
			},
			wantErrSubstr: `ignoreListElementKeys entry "id" also appears as a key in one of this entry's own value: elements`,
		},
		"NamedOnlyInIgnoreListElementKeys": {
			reason: "the legitimate shape: ignoreListElementKeys names the provider-injected per-element member " +
				"that expect: deliberately never mentions",
			test: UpdateTest{
				Field:                 "firewallRules",
				Value:                 []interface{}{map[string]interface{}{"port": "443"}},
				Expect:                []interface{}{map[string]interface{}{"port": "443"}},
				IgnoreListElementKeys: []string{"id"},
			},
		},
		"NonArrayExpectation": {
			reason: "a scalar or map expectation has no elements to collide with, so ignoreListElementKeys is " +
				"accepted even though it will never match anything on that side",
			test: UpdateTest{
				Field:                 "count",
				Value:                 "5",
				IgnoreListElementKeys: []string{"id"},
			},
		},
		"ElementNotAnObjectIsSkipped": {
			reason: "a list-of-scalars has no member keys to collide with — a non-object element is skipped " +
				"rather than treated as a match",
			test: UpdateTest{
				Field:                 "tags",
				Value:                 []interface{}{"a", "b"},
				IgnoreListElementKeys: []string{"id"},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateIgnoreListElementKeys(tc.test)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("%s: ValidateIgnoreListElementKeys() error = %v, want nil", tc.reason, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: ValidateIgnoreListElementKeys() error = nil, want an error containing %q", tc.reason, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ValidateIgnoreListElementKeys() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationIgnoreListElementKeys proves an "ignoreListElementKeys:"
// key on an update-test entry parses onto UpdateTest.IgnoreListElementKeys,
// that an entry with no such key parses with a nil IgnoreListElementKeys
// (the common case every pre-existing fleet entry must keep matching), and
// that ParseAnnotation rejects the entry-level combination
// ValidateIgnoreListElementKeys itself rejects (skip paired with
// ignoreListElementKeys) at the same entry point every other per-entry
// validation runs through.
func TestParseAnnotationIgnoreListElementKeys(t *testing.T) {
	t.Run("Absent", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: comment
  value: "updated"
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v", err)
		}
		if len(tests) != 1 {
			t.Fatalf("len(tests) = %d, want 1", len(tests))
		}
		if tests[0].IgnoreListElementKeys != nil {
			t.Errorf("IgnoreListElementKeys = %v, want nil", tests[0].IgnoreListElementKeys)
		}
	})

	t.Run("Present", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: firewallRules
  value: [{port: 443, protocol: tcp}]
  expect: [{port: 443, protocol: tcp}]
  ignoreListElementKeys: [id]
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v", err)
		}
		if len(tests) != 1 {
			t.Fatalf("len(tests) = %d, want 1", len(tests))
		}
		want := []string{"id"}
		got := tests[0].IgnoreListElementKeys
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("IgnoreListElementKeys = %v, want %v", got, want)
		}
	})

	t.Run("RejectsSkipCombination", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: firewallRules
  skip: "no test written yet"
  ignoreListElementKeys: [id]
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting skip + ignoreListElementKeys")
		}
		wantErrSubstr := "ignoreListElementKeys is set but skip is also set"
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})

	// RejectsVacuousSelfCollision reproduces, through the real annotation
	// parse path, the exact expected/actual pair this check exists to
	// catch: id is named in both an expect: element and
	// ignoreListElementKeys, so the comparison would strip it from both
	// sides and report PASS against a live value that never actually
	// converged.
	t.Run("RejectsVacuousSelfCollision", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: firewallRules
  value: [{port: 443, id: "x"}]
  expect: [{port: 443, id: "guessed"}]
  ignoreListElementKeys: [id]
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting the id self-collision")
		}
		wantErrSubstr := `ignoreListElementKeys entry "id" also appears as a key in one of this entry's own expect: elements`
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})
}
