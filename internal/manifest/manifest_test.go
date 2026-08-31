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

// TestParseBytesReadyConditions covers ParseBytes's handling of the
// uptest.upbound.io/conditions annotation — uptest's own declaration of
// which status condition(s) mean a resource is ready, which
// EffectiveReadyConditions falls back to consulting. Present, comma
// separated with whitespace, single-valued, blank, and absent.
func TestParseBytesReadyConditions(t *testing.T) {
	cases := map[string]struct {
		reason string
		yaml   string
		want   []string
	}{
		"Absent": {
			reason: "manifests without the annotation parse with a nil ReadyConditions — this is the default for every existing example, and EffectiveReadyConditions is what supplies the fallback",
			yaml: `
apiVersion: example.crossplane.io/v1alpha1
kind: Example
metadata:
  name: example
`,
			want: nil,
		},
		"SingleValue": {
			reason: "the common case measured live on provider-f5xc's CodeBaseIntegration — a resource whose sanctioned steady state is Synced but never Ready",
			yaml: `
apiVersion: example.crossplane.io/v1alpha1
kind: Example
metadata:
  name: example
  annotations:
    uptest.upbound.io/conditions: "Synced"
`,
			want: []string{"Synced"},
		},
		"CommaSeparatedWithWhitespace": {
			reason: "uptest's own annotation format tolerates whitespace around each comma-separated entry",
			yaml: `
apiVersion: example.crossplane.io/v1alpha1
kind: Example
metadata:
  name: example
  annotations:
    uptest.upbound.io/conditions: "Synced, Ready"
`,
			want: []string{"Synced", "Ready"},
		},
		"Blank": {
			reason: "an empty annotation value is indistinguishable from an absent one — both fall back to the default in EffectiveReadyConditions",
			yaml: `
apiVersion: example.crossplane.io/v1alpha1
kind: Example
metadata:
  name: example
  annotations:
    uptest.upbound.io/conditions: ""
`,
			want: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("%s: ParseBytes() error = %v", tc.reason, err)
			}
			if !reflect.DeepEqual(m.ReadyConditions, tc.want) {
				t.Errorf("%s: ReadyConditions = %#v, want %#v", tc.reason, m.ReadyConditions, tc.want)
			}
		})
	}
}

// TestManifestEffectiveReadyConditions covers the fallback
// EffectiveReadyConditions applies: a manifest's own declaration when
// present, else the single "Ready" default every manifest was implicitly
// judged against before ReadyConditions existed.
func TestManifestEffectiveReadyConditions(t *testing.T) {
	cases := map[string]struct {
		reason string
		m      Manifest
		want   []string
	}{
		"NoDeclarationDefaultsToReady": {
			reason: "a manifest that never declares uptest.upbound.io/conditions must keep judging readiness against \"Ready\" exactly as every manifest did before this field existed",
			m:      Manifest{},
			want:   []string{"Ready"},
		},
		"DeclaredOverrideIsUsedVerbatim": {
			reason: "a declared override replaces the default outright, it is not unioned with it",
			m:      Manifest{ReadyConditions: []string{"Synced"}},
			want:   []string{"Synced"},
		},
		"MultiValueDeclarationIsUsedVerbatim": {
			reason: "a multi-condition declaration is returned as-is for the caller to AND together",
			m:      Manifest{ReadyConditions: []string{"Synced", "Ready"}},
			want:   []string{"Synced", "Ready"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.m.EffectiveReadyConditions()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: EffectiveReadyConditions() = %#v, want %#v", tc.reason, got, tc.want)
			}
		})
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
			reason: "vendor-defect carries both evidence: and an externally resolvable ticket:",
			annotation: `
- field: dnsVolterraManaged
  skip:
    reason: vendor-defect
    evidence: "HTTP 400 'Change of domain type ... is not supported'"
    ticket: https://support.f5.com/csp/case/00482113
`,
			want: SkipInfo{
				Reason:   SkipVendorDefect,
				Evidence: "HTTP 400 'Change of domain type ... is not supported'",
				Ticket:   "https://support.f5.com/csp/case/00482113",
			},
		},
		"VendorDefectEvidenceOnly": {
			reason: "ticket: is optional — vendor-defect parses on evidence: alone",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    evidence: "observed a 400"
`,
			want: SkipInfo{Reason: SkipVendorDefect, Evidence: "observed a 400"},
		},
		"FixtureMissing": {
			reason: "fixture-missing carries both evidence: and an externally resolvable ticket:",
			annotation: `
- field: firewallGroupId
  skip:
    reason: fixture-missing
    evidence: "no fixture backend exposes a second firewall group to move this field between"
    ticket: "8213021"
`,
			want: SkipInfo{
				Reason:   SkipFixtureMissing,
				Evidence: "no fixture backend exposes a second firewall group to move this field between",
				Ticket:   "8213021",
			},
		},
		"FixtureMissingEvidenceOnly": {
			reason: "ticket: is optional — fixture-missing parses on evidence: alone",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
    evidence: "no fixture backend exposes a second firewall group to move this field between"
`,
			want: SkipInfo{
				Reason:   SkipFixtureMissing,
				Evidence: "no fixture backend exposes a second firewall group to move this field between",
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
		"TicketBareInteger": {
			reason: "a bare integer ticket: is a plausible vendor case number and must be accepted",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    evidence: "observed a 400"
    ticket: "482113"
`,
			want: SkipInfo{Reason: SkipVendorDefect, Evidence: "observed a 400", Ticket: "482113"},
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

// TestParseAnnotationDispositionRoundTrips pins each of the four
// disposition: values round-tripping through parse, alongside a reason:
// that itself needs no companion keys (write-only) so the case exercises
// only the disposition axis. declared-exclusion additionally carries the
// declared-by: and reconfirm: keys it requires.
func TestParseAnnotationDispositionRoundTrips(t *testing.T) {
	cases := map[string]struct {
		reason     string
		annotation string
		want       SkipInfo
	}{
		"StaticallyProvable": {
			reason: "statically-provable round-trips with no companion keys of its own",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: statically-provable
`,
			want: SkipInfo{Reason: SkipWriteOnly, Disposition: DispositionStaticallyProvable},
		},
		"OneLivePatch": {
			reason: "one-live-patch round-trips with no companion keys of its own",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: one-live-patch
`,
			want: SkipInfo{Reason: SkipWriteOnly, Disposition: DispositionOneLivePatch},
		},
		"DeclaredExclusion": {
			reason: "declared-exclusion carries both declared-by: and reconfirm:",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: declared-exclusion
    declared-by: a human
    reconfirm: 2027-01-01
`,
			want: SkipInfo{
				Reason:      SkipWriteOnly,
				Disposition: DispositionDeclaredExclusion,
				DeclaredBy:  "a human",
				Reconfirm:   "2027-01-01",
			},
		},
		"Defect": {
			reason: "defect round-trips with no companion keys of its own",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: defect
`,
			want: SkipInfo{Reason: SkipWriteOnly, Disposition: DispositionDefect},
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
		})
	}
}

// TestParseAnnotationDispositionRejectsInvalidShapes pins the disposition
// axis's own parse-time rejections: an unrecognised disposition value named
// in its own error text, and declared-exclusion's two required companion
// keys each rejected individually when absent.
func TestParseAnnotationDispositionRejectsInvalidShapes(t *testing.T) {
	cases := map[string]struct {
		reason        string
		annotation    string
		wantErrSubstr string
	}{
		"UnknownDisposition": {
			reason: "a disposition outside the closed set is rejected NAMING THE OFFENDING VALUE — a dropped value would read as no disposition at all, indistinguishable from an unauthored leaf",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: not-a-real-disposition
`,
			wantErrSubstr: `disposition "not-a-real-disposition" is not one of the valid dispositions`,
		},
		"DeclaredExclusionMissingDeclaredBy": {
			reason: "declared-exclusion requires a non-empty declared-by:",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: declared-exclusion
    reconfirm: 2027-01-01
`,
			wantErrSubstr: "requires both declared-by: and reconfirm:",
		},
		"DeclaredExclusionMissingReconfirm": {
			reason: "declared-exclusion requires a non-empty reconfirm:",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: declared-exclusion
    declared-by: a human
`,
			wantErrSubstr: "requires both declared-by: and reconfirm:",
		},
		"DeclaredExclusionMissingBoth": {
			reason: "declared-exclusion requires both declared-by: and reconfirm: when neither is given",
			annotation: `
- field: privateKey
  skip:
    reason: write-only
    disposition: declared-exclusion
`,
			wantErrSubstr: "requires both declared-by: and reconfirm:",
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

// TestParseAnnotationDispositionNeverInferredFromReason confirms a skip:
// entry with a rich, tier-suggestive reason: (vendor-defect, in a shape
// that reads like a one-live-patch claim) parses with an EMPTY Disposition
// when no disposition: key is authored — pinning that nothing in this
// package pattern-matches reason: or evidence: prose to assign a tier. A
// disposition is authored, or it is absent; it is never guessed.
func TestParseAnnotationDispositionNeverInferredFromReason(t *testing.T) {
	tests, _, _, _, err := ParseAnnotation(`
- field: displayName
  skip:
    reason: vendor-defect
    evidence: "HTTP 409, permanently reserved regardless of response code"
    ticket: IAM-DISPLAYNAME
`)
	if err != nil {
		t.Fatalf("ParseAnnotation() error = %v", err)
	}
	if len(tests) != 1 {
		t.Fatalf("got %d entries, want 1", len(tests))
	}
	if got := tests[0].Skip.Disposition; got != "" {
		t.Errorf("Skip.Disposition = %q, want empty — a disposition is authored via disposition:, never inferred from reason/evidence prose", got)
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
			reason: "vendor-defect requires a non-empty evidence: — ticket: is optional and does not substitute for it",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    ticket: "482113"
`,
			wantErrSubstr: "requires a non-empty evidence:",
		},
		"FixtureMissingNoTicketOrEvidence": {
			reason: "fixture-missing requires a non-empty evidence:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
`,
			wantErrSubstr: "requires a non-empty evidence:",
		},
		"FixtureMissingTicketOnly": {
			reason: "ticket: alone does not substitute for the required evidence:",
			annotation: `
- field: comment
  skip:
    reason: fixture-missing
    ticket: "8213021"
`,
			wantErrSubstr: "requires a non-empty evidence:",
		},
		"TicketUUIDRejected": {
			reason: "a bare UUID is certainly not an externally resolvable reference",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    evidence: "observed a 400"
    ticket: 3f0f55de-1234-4321-89ab-1234567890ab
`,
			wantErrSubstr: "looks like a bare UUID",
		},
		"TicketFactorySlugRejected": {
			reason: "a factory ticket slug is certainly not an externally resolvable reference",
			annotation: `
- field: comment
  skip:
    reason: vendor-defect
    evidence: "observed a 400"
    ticket: FX-DNS-DELEGATION
`,
			wantErrSubstr: "looks like a factory ticket slug",
		},
		"LegacyAndReasonMutuallyExclusive": {
			reason: "legacy: and reason: are alternatives, not a merge",
			annotation: `
- field: comment
  skip:
    legacy: "not exercised in this example"
    reason: write-only
`,
			wantErrSubstr: "carries both legacy: and reason:",
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

// TestSkipInfoUnmarshalYAMLLegacyMapping pins the legacy: mapping shape: the
// same free prose as the bare-string form, but able to additionally carry
// disposition: (and, for declared-exclusion, declared-by:/reconfirm:)
// alongside it — the shape change that decouples authoring a one-word
// evidence tier from re-expressing the prose as a closed-set reason: code.
func TestSkipInfoUnmarshalYAMLLegacyMapping(t *testing.T) {
	cases := map[string]struct {
		reason     string
		annotation string
		want       SkipInfo
	}{
		"LegacyWithDisposition": {
			reason: "legacy: plus disposition: parses with Legacy true and LegacyText set verbatim",
			annotation: `
- field: comment
  skip:
    legacy: "the original free-prose text, verbatim"
    disposition: statically-provable
`,
			want: SkipInfo{
				Legacy:      true,
				LegacyText:  "the original free-prose text, verbatim",
				Disposition: DispositionStaticallyProvable,
			},
		},
		"LegacyAlone": {
			reason: "legacy: with no disposition: parses to exactly what the bare-string scalar form parses to",
			annotation: `
- field: comment
  skip:
    legacy: "not exercised in this example"
`,
			want: SkipInfo{Legacy: true, LegacyText: "not exercised in this example"},
		},
		"LegacyWithDeclaredExclusion": {
			reason: "legacy: carries a declared-exclusion disposition alongside its own required declared-by:/reconfirm:",
			annotation: `
- field: comment
  skip:
    legacy: "firing this probe would delete the only paid-tier tenant fixture"
    disposition: declared-exclusion
    declared-by: platform-team
    reconfirm: "2026-Q3"
`,
			want: SkipInfo{
				Legacy:      true,
				LegacyText:  "firing this probe would delete the only paid-tier tenant fixture",
				Disposition: DispositionDeclaredExclusion,
				DeclaredBy:  "platform-team",
				Reconfirm:   "2026-Q3",
			},
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
			if !tests[0].Skip.Legacy {
				t.Errorf("%s: Skip.Legacy = false, want true for a legacy: mapping entry", tc.reason)
			}
			if !tests[0].Skip.Present() {
				t.Errorf("%s: Skip.Present() = false, want true", tc.reason)
			}
		})
	}
}

// TestSkipInfoUnmarshalYAMLLegacyMappingVerbatimRoundTrip proves the
// legacy: mapping shape is lossless on a REALISTIC waiver string — one
// carrying a colon, an em dash and an embedded double quote, the three
// characters a naive round-trip is most likely to mangle. The legacy:
// mapping form must parse the prose to exactly the same LegacyText the
// bare-string scalar form parses it to, and Describe() must render it
// identically (aside from the disposition suffix the scalar form cannot
// carry at all).
func TestSkipInfoUnmarshalYAMLLegacyMappingVerbatimRoundTrip(t *testing.T) {
	const prose = `Field: "region" cannot be updated — vendor API returns HTTP 409 "immutable field" on any PATCH containing it`

	scalarTests, _, _, _, err := ParseAnnotation(`
- field: region
  skip: 'Field: "region" cannot be updated — vendor API returns HTTP 409 "immutable field" on any PATCH containing it'
`)
	if err != nil {
		t.Fatalf("scalar form: ParseAnnotation() error = %v", err)
	}
	scalarSkip := scalarTests[0].Skip
	if scalarSkip.LegacyText != prose {
		t.Fatalf("scalar form: LegacyText = %q, want %q", scalarSkip.LegacyText, prose)
	}

	mappingTests, _, _, _, err := ParseAnnotation(`
- field: region
  skip:
    legacy: 'Field: "region" cannot be updated — vendor API returns HTTP 409 "immutable field" on any PATCH containing it'
    disposition: statically-provable
`)
	if err != nil {
		t.Fatalf("legacy: mapping form: ParseAnnotation() error = %v", err)
	}
	mappingSkip := mappingTests[0].Skip

	if !mappingSkip.Legacy {
		t.Errorf("legacy: mapping form: Legacy = false, want true")
	}
	if mappingSkip.LegacyText != prose {
		t.Errorf("legacy: mapping form: LegacyText = %q, want %q (verbatim, unmangled)", mappingSkip.LegacyText, prose)
	}
	if mappingSkip.Disposition != DispositionStaticallyProvable {
		t.Errorf("legacy: mapping form: Disposition = %q, want %q", mappingSkip.Disposition, DispositionStaticallyProvable)
	}

	if got, want := scalarSkip.Describe(), prose; got != want {
		t.Errorf("scalar form: Describe() = %q, want %q", got, want)
	}
	if got, want := mappingSkip.Describe(), prose+" [disposition: statically-provable]"; got != want {
		t.Errorf("legacy: mapping form: Describe() = %q, want %q", got, want)
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
			skip:   SkipInfo{Reason: SkipVendorDefect, Evidence: "observed a 400", Ticket: "https://support.f5.com/csp/case/00482113"},
			want:   "vendor-defect (observed a 400; ticket: https://support.f5.com/csp/case/00482113)",
		},
		"VendorDefectNoTicket": {
			reason: "ticket: is optional and omitted entirely from the rendering when empty",
			skip:   SkipInfo{Reason: SkipVendorDefect, Evidence: "observed a 400"},
			want:   "vendor-defect (observed a 400)",
		},
		"FixtureMissing": {
			reason: "fixture-missing names both its evidence and its ticket",
			skip: SkipInfo{
				Reason:   SkipFixtureMissing,
				Evidence: "no fixture backend exposes a second firewall group to move this field between",
				Ticket:   "8213021",
			},
			want: "fixture-missing (no fixture backend exposes a second firewall group to move this field between; ticket: 8213021)",
		},
		"WriteOnly": {
			reason: "write-only carries no companion data to render",
			skip:   SkipInfo{Reason: SkipWriteOnly},
			want:   "write-only",
		},
		"LegacyWithDisposition": {
			reason: "the legacy: mapping form renders its free-prose text verbatim, plus the disposition suffix",
			skip:   SkipInfo{Legacy: true, LegacyText: "not exercised in this example", Disposition: DispositionStaticallyProvable},
			want:   "not exercised in this example [disposition: statically-provable]",
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

// TestValidateFieldEntryMix pins ValidateFieldEntryMix's own rejection and
// acceptance rules directly, without going through YAML parsing: a field
// whose entries mix a skip: entry with a tested one is rejected, while any
// number of tested-only or skip-only entries for one field is accepted —
// including the established multi-tested-entry idiom (a value axis and a
// clear:/withValues: axis tested as separate entries for the same field).
func TestValidateFieldEntryMix(t *testing.T) {
	cases := map[string]struct {
		reason        string
		tests         []UpdateTest
		wantErrSubstr string // empty means ValidateFieldEntryMix must return nil
	}{
		"SingleTestedEntry": {
			reason: "the ordinary, overwhelmingly common shape: one tested entry per field",
			tests: []UpdateTest{
				{Field: "name", Value: "updated"},
			},
		},
		"SingleSkipEntry": {
			reason: "a lone skip: entry has no sibling to conflict with",
			tests: []UpdateTest{
				{Field: "name", Skip: LegacySkip("no test written yet")},
			},
		},
		"MultipleTestedEntriesSameField": {
			reason: "the established idiom: a field's value axis and its clear:/withValues: " +
				"axis tested as two separate TESTED entries — must stay accepted",
			tests: []UpdateTest{
				{Field: "options", Value: map[string]interface{}{"a": "1"}},
				{Field: "options", Value: map[string]interface{}{"a": "1"}, Clear: []string{"otherArm"}},
			},
		},
		"ThreeTestedEntriesSameField": {
			reason: "three tested entries for one field (the fleet's other measured shape) also stays accepted",
			tests: []UpdateTest{
				{Field: "options", Value: "v1"},
				{Field: "options", Value: "v2", Clear: []string{"otherArm"}},
				{Field: "options", Value: "v3", WithValues: map[string]interface{}{"derivedFrom": "v3"}},
			},
		},
		"DifferentFieldsMixed": {
			reason: "a skip: entry on one field and a tested entry on a DIFFERENT field never conflict",
			tests: []UpdateTest{
				{Field: "name", Value: "updated"},
				{Field: "legacyField", Skip: LegacySkip("deprecated")},
			},
		},
		"TestedThenSkipSameField": {
			reason: "the measured defect shape: a skip: entry appended after a tested entry for the " +
				"same field silently downgrades the field's coverage through the last-wins field-keyed map",
			tests: []UpdateTest{
				{Field: "options", Value: "v1"},
				{Field: "options", Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "options" carries both a skip: entry and a tested entry`,
		},
		"SkipThenTestedSameField": {
			reason: "the mix is rejected regardless of which order the two entries appear in",
			tests: []UpdateTest{
				{Field: "options", Skip: LegacySkip("appended by a disposition wave")},
				{Field: "options", Value: "v1"},
			},
			wantErrSubstr: `field "options" carries both a skip: entry and a tested entry`,
		},
		"Empty": {
			reason: "no entries at all is not a mix",
			tests:  nil,
		},
		"SingleEntryValueAndSkipCombined": {
			reason: "the combined-entry trap: one entry carrying BOTH value: and skip: has nothing to " +
				"compare it against — ValidateFieldEntryMix's cross-entry comparison alone would parse " +
				"this clean, then the runner's skip-first check would silently discard the value: half " +
				"and the field would still be reported as covered; this is the shape a dedicated per-entry " +
				"check on top of the cross-entry comparison exists to catch",
			tests: []UpdateTest{
				{Field: "name", Value: "updated", Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "name"'s entry carries both a skip: and a tested assertion`,
		},
		"SingleEntryExpectAndSkipCombined": {
			reason: "expect: alone (no value:) makes the same single entry tested — must also be caught",
			tests: []UpdateTest{
				{Field: "name", Expect: "updated", Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "name"'s entry carries both a skip: and a tested assertion`,
		},
		"SingleEntryClearAndSkipCombined": {
			reason: "clear: alone (no value:/expect:) also makes the entry tested — must also be caught",
			tests: []UpdateTest{
				{Field: "options", Clear: []string{"otherArm"}, Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "options"'s entry carries both a skip: and a tested assertion`,
		},
		"SingleEntryWithValuesAndSkipCombined": {
			reason: "withValues: alone also makes the entry tested — must also be caught",
			tests: []UpdateTest{
				{Field: "options", WithValues: map[string]interface{}{"derivedFrom": "v1"}, Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "options"'s entry carries both a skip: and a tested assertion`,
		},
		"SingleEntryExplicitNullValueAndSkipCombined": {
			reason: "an explicit `value: null` (ValueExplicit true, Value nil) is still a real tested " +
				"assertion — a whole-field tombstone — and must also be caught when paired with skip:",
			tests: []UpdateTest{
				{Field: "name", ValueExplicit: true, Skip: LegacySkip("appended by a disposition wave")},
			},
			wantErrSubstr: `field "name"'s entry carries both a skip: and a tested assertion`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateFieldEntryMix(tc.tests)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("%s: ValidateFieldEntryMix() error = %v, want nil", tc.reason, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: ValidateFieldEntryMix() error = nil, want an error containing %q", tc.reason, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s: ValidateFieldEntryMix() error = %q, want it to contain %q", tc.reason, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestParseAnnotationRejectsSkipTestedMix proves ParseAnnotation itself
// wires ValidateFieldEntryMix in — the real annotation parse path a
// manifest actually goes through, not just the helper in isolation — and
// that the established multi-tested-entry idiom still parses cleanly
// through that same path.
func TestParseAnnotationRejectsSkipTestedMix(t *testing.T) {
	t.Run("RejectsTestedThenSkip", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: options
  value: "v1"
- field: options
  skip:
    reason: fixture-missing
    evidence: "no second fixture to move this field to"
    ticket: TEST-1234
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting the skip/tested mix")
		}
		wantErrSubstr := `field "options" carries both a skip: entry and a tested entry`
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})

	t.Run("AcceptsMultipleTestedEntriesSameField", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: options
  value: {a: "1"}
- field: options
  value: {a: "1"}
  clear: [otherArm]
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v, want nil — multiple TESTED entries for one field is the established idiom", err)
		}
		if len(tests) != 2 {
			t.Fatalf("len(tests) = %d, want 2", len(tests))
		}
	})

	t.Run("AcceptsThreeTestedEntriesSameField", func(t *testing.T) {
		tests, _, _, _, err := ParseAnnotation(`
- field: options
  value: "v1"
- field: options
  value: "v2"
  clear: [otherArm]
- field: options
  value: "v3"
  withValues: {derivedFrom: "v3"}
`)
		if err != nil {
			t.Fatalf("ParseAnnotation() error = %v, want nil", err)
		}
		if len(tests) != 3 {
			t.Fatalf("len(tests) = %d, want 3", len(tests))
		}
	})

	// RejectsCombinedSingleEntry replays the cell-denominator gap through
	// the real YAML parse path: ONE entry carrying both value: and skip:
	// has no sibling entry for ValidateFieldEntryMix's cross-entry
	// comparison to compare it against, so that comparison alone would
	// parse this clean — the runner would then silently skip the field
	// and the value: assertion would never fire. The per-entry check
	// added on top of the cross-entry comparison must catch it here too,
	// not only when built directly as a Go literal.
	t.Run("RejectsCombinedSingleEntry", func(t *testing.T) {
		_, _, _, _, err := ParseAnnotation(`
- field: name
  value: "updated"
  skip:
    reason: fixture-missing
    evidence: "appended by a disposition wave without removing the tested value"
`)
		if err == nil {
			t.Fatal("ParseAnnotation() error = nil, want an error rejecting the single-entry skip/value combination")
		}
		wantErrSubstr := `field "name"'s entry carries both a skip: and a tested assertion`
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("ParseAnnotation() error = %q, want it to contain %q", err.Error(), wantErrSubstr)
		}
	})
}
