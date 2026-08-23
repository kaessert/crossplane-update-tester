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

// TestParseBytesKnownDefect proves a "knownDefect:" key on an update-test
// entry parses onto UpdateTest.KnownDefect, and that an entry with no such
// key parses with an empty KnownDefect — the additive-only case every
// pre-existing entry in the fleet must keep matching.
func TestParseBytesKnownDefect(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   string
	}{
		"Absent": {
			reason: "an entry with no knownDefect key parses with an empty KnownDefect, not an error",
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
			want: "",
		},
		"Present": {
			reason: "the ticket ID is extracted verbatim",
			yaml: `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      - field: useTls
        value: true
        knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			want: "e9ce03ee-920d-46f5-9aa3-120228b196fb",
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
			if m.Tests[0].KnownDefect != tc.want {
				t.Errorf("%s: Tests[0].KnownDefect = %q, want %q", tc.reason, m.Tests[0].KnownDefect, tc.want)
			}
		})
	}
}

// TestParseAnnotationKnownDefectRejectsInvalidShapes pins every parse-time
// rejection a knownDefect entry can trigger: paired with skip, missing a
// value, and a ticket ID that is prose, a placeholder, or too short — see
// ValidateKnownDefect and the mutual-exclusion/value-required checks in
// ParseAnnotation. An EMPTY knownDefect value is not tested here: YAML
// decodes an omitted key and an explicit `knownDefect: ""` to the identical
// Go zero value, so ParseAnnotation cannot tell them apart and correctly
// treats both as "not a knownDefect entry" rather than invoking
// ValidateKnownDefect at all — see TestValidateKnownDefect for that check's
// own empty-value rejection, exercised directly.
func TestParseAnnotationKnownDefectRejectsInvalidShapes(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"PairedWithSkip": {
			reason: "knownDefect and skip are mutually exclusive — one says no test exists, the other says an expressible test fails",
			annotation: `
- field: useTls
  value: true
  skip: "not ready yet"
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: "knownDefect and skip are mutually exclusive",
		},
		"MissingValue": {
			reason: "a knownDefect entry still has to run, so it needs a value exactly like an ordinary entry",
			annotation: `
- field: useTls
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: "value is required unless skip is set",
		},
		"ProseTicketID": {
			reason: "a knownDefect value with whitespace is a description, not a ticket ID to search for",
			annotation: `
- field: useTls
  value: true
  knownDefect: "fix later once backend supports it"
`,
			wantErrSubstr: "looks like a prose description",
		},
		"PlaceholderTicketID": {
			reason: "a placeholder value passes no gate a real ticket ID would have to",
			annotation: `
- field: useTls
  value: true
  knownDefect: TODO
`,
			wantErrSubstr: "is a placeholder, not a real ticket ID",
		},
		"TooShortTicketID": {
			reason: "a handful of characters is too short to plausibly be a ticket ID",
			annotation: `
- field: useTls
  value: true
  knownDefect: ab1
`,
			wantErrSubstr: "too short to be a ticket ID",
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

// TestValidateKnownDefect exercises ValidateKnownDefect directly, including
// the empty-value case ParseAnnotation can never reach (see
// TestParseAnnotationKnownDefectRejectsInvalidShapes).
func TestValidateKnownDefect(t *testing.T) {
	cases := map[string]struct {
		reason        string
		ticketID      string
		wantErrSubstr string // empty means ValidateKnownDefect must return nil
	}{
		"Empty": {
			reason:        "an empty value cannot be followed back to anything",
			ticketID:      "",
			wantErrSubstr: "knownDefect requires a ticket ID",
		},
		"WhitespaceOnly": {
			reason:        "a whitespace-only value trims to empty",
			ticketID:      "   ",
			wantErrSubstr: "knownDefect requires a ticket ID",
		},
		"Prose": {
			reason:        "a value containing spaces reads as a description, not an ID",
			ticketID:      "fix this later",
			wantErrSubstr: "looks like a prose description",
		},
		"PlaceholderCaseInsensitive": {
			reason:        "placeholder matching is case-insensitive",
			ticketID:      "Todo",
			wantErrSubstr: "is a placeholder, not a real ticket ID",
		},
		"TooShort": {
			reason:        "fewer than 6 characters cannot plausibly be a ticket ID",
			ticketID:      "abcde",
			wantErrSubstr: "too short to be a ticket ID",
		},
		"ValidUUID": {
			reason:   "a full UUID passes",
			ticketID: "e9ce03ee-920d-46f5-9aa3-120228b196fb",
		},
		"ValidShortSlug": {
			reason:   "a short custom slug at the length floor passes",
			ticketID: "e6b026d6",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateKnownDefect(tc.ticketID)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("%s: ValidateKnownDefect(%q) error = %v, want nil", tc.reason, tc.ticketID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: ValidateKnownDefect(%q) error = nil, want an error containing %q", tc.reason, tc.ticketID, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ValidateKnownDefect(%q) error = %q, want it to contain %q", tc.reason, tc.ticketID, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationKnownDefectAcceptsBothTicketIDShapes proves
// ValidateKnownDefect is not tied to one specific ticket-ID format: a full
// UUID and a short custom slug (both real pheromone ticket ID shapes) are
// each accepted.
func TestParseAnnotationKnownDefectAcceptsBothTicketIDShapes(t *testing.T) {
	cases := map[string]struct {
		reason     string
		ticketID   string
		annotation string
	}{
		"UUID": {
			reason:   "a full UUID ticket ID is accepted",
			ticketID: "e9ce03ee-920d-46f5-9aa3-120228b196fb",
		},
		"ShortSlug": {
			reason:   "a short custom ticket ID slug is accepted",
			ticketID: "e6b026d6",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			annotation := "- field: useTls\n  value: true\n  knownDefect: " + tc.ticketID + "\n"
			tests, _, _, _, err := ParseAnnotation(annotation)
			if err != nil {
				t.Fatalf("%s: ParseAnnotation() error = %v", tc.reason, err)
			}
			if len(tests) != 1 || tests[0].KnownDefect != tc.ticketID {
				t.Fatalf("%s: tests = %+v, want one entry with KnownDefect = %q", tc.reason, tests, tc.ticketID)
			}
		})
	}
}

// TestParseAnnotationKnownDefectRejectsIgnoreFieldsOverlap pins the
// dead-config guard: a knownDefect entry naming the same top-level field a
// manifest's own ignore-fields directive also excludes is rejected at parse
// time, before any cluster is touched — see ValidateKnownDefectIgnoreFields.
func TestParseAnnotationKnownDefectRejectsIgnoreFieldsOverlap(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"DirectOverlap": {
			reason: "the same top-level field name in both directives is dead config",
			annotation: `
ignore-fields: useTls
- field: useTls
  value: true
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: `field "useTls" is both a knownDefect entry`,
		},
		"NoOverlapIsFine": {
			reason: "a knownDefect entry and an unrelated ignore-fields entry coexist without error",
			annotation: `
ignore-fields: latestBackup
- field: useTls
  value: true
  knownDefect: e9ce03ee-920d-46f5-9aa3-120228b196fb
`,
			wantErrSubstr: "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := ParseAnnotation(tc.annotation)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("%s: ParseAnnotation() error = %v, want no error", tc.reason, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: ParseAnnotation() error = nil, want an error", tc.reason)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ParseAnnotation() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
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
			reason: "fixture-missing carries the ticket:",
			annotation: `
- field: firewallGroupId
  skip:
    reason: fixture-missing
    ticket: VU-FW-FIXTURE
`,
			want: SkipInfo{Reason: SkipFixtureMissing, Ticket: "VU-FW-FIXTURE"},
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
		"FixtureMissingNoTicket": {
			reason: "fixture-missing requires a non-empty ticket:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
`,
			wantErrSubstr: "requires a non-empty ticket:",
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
			reason: "fixture-missing names its ticket",
			skip:   SkipInfo{Reason: SkipFixtureMissing, Ticket: "VU-FW-FIXTURE"},
			want:   "fixture-missing (ticket: VU-FW-FIXTURE)",
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
