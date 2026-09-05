package sidecar

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLoadAbsentSidecar confirms the un-migrated state every manifest
// starts in: no "<manifest>.yaml.uptest" file beside it, Load returns
// (nil, nil) rather than an error.
func TestLoadAbsentSidecar(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "widget.yaml")
	if err := os.WriteFile(manifest, []byte("apiVersion: v1\nkind: Secret\n"), 0o600); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	f, err := Load(manifest)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if f != nil {
		t.Fatalf("Load() = %+v, want nil", f)
	}
}

// TestPathFor confirms the extension is ".yaml.uptest", never
// ".uptest.yaml" — the ordering that keeps a sidecar out of every
// "examples/**/*.yaml" glob.
func TestPathFor(t *testing.T) {
	got := PathFor("examples/instance/instance.yaml")
	want := "examples/instance/instance.yaml.uptest"
	if got != want {
		t.Fatalf("PathFor() = %q, want %q", got, want)
	}
}

// TestParse covers Parse's own, target-independent checks: selector
// splitting, value rendering, and the four failure modes that need no
// manifest to detect (missing for:, unknown directive, duplicate
// selector, a templated narrowing directive).
func TestParse(t *testing.T) {
	cases := map[string]struct {
		reason  string
		yaml    string
		want    *File
		wantErr string
	}{
		"CoreGroupTwoSegmentFor": {
			reason: "a core-group kind's for: has only two segments (\"v1/Secret\") and must still split into " +
				"apiVersion \"v1\", kind \"Secret\" rather than failing or mis-splitting",
			yaml: "for: v1/Secret\nuptest.upbound.io/timeout: \"600\"\n",
			want: &File{Docs: []Doc{{
				For: "v1/Secret", APIVersion: "v1", Kind: "Secret",
				Annotations: map[string]string{"uptest.upbound.io/timeout": "600"},
			}}},
		},
		"ThreeSegmentFor": {
			reason: "a grouped kind's for: splits at its LAST slash, so the group itself may contain slashes-free " +
				"dots without confusing the split",
			yaml: "for: instance.lambda.crossplane.io/v1alpha1/Instance\nuptest.upbound.io/timeout: \"1200\"\n",
			want: &File{Docs: []Doc{{
				For:        "instance.lambda.crossplane.io/v1alpha1/Instance",
				APIVersion: "instance.lambda.crossplane.io/v1alpha1",
				Kind:       "Instance",
				Annotations: map[string]string{
					"uptest.upbound.io/timeout": "1200",
				},
			}}},
		},
		"NativeIntRendersToString": {
			reason: "a native (unquoted) int value renders to the plain string an annotation must hold",
			yaml:   "for: v1/ConfigMap\nuptest.upbound.io/timeout: 1200\n",
			want: &File{Docs: []Doc{{
				For: "v1/ConfigMap", APIVersion: "v1", Kind: "ConfigMap",
				Annotations: map[string]string{"uptest.upbound.io/timeout": "1200"},
			}}},
		},
		"NativeListRendersToYAMLText": {
			reason: "a native (non-string) YAML list re-marshals to the equivalent YAML text an inline block " +
				"scalar would have held",
			yaml: "for: v1/ConfigMap\ncrossplane.io/update-test:\n  - field: name\n    value: renamed\n",
			want: &File{Docs: []Doc{{
				For: "v1/ConfigMap", APIVersion: "v1", Kind: "ConfigMap",
				Annotations: map[string]string{"crossplane.io/update-test": "- field: name\n  value: renamed"},
			}}},
		},
		"BlockScalarRoundTripsByteForByte": {
			reason: "a block-scalar string value is carried through unchanged — no reformatting, no reindentation",
			yaml: "for: v1/ConfigMap\ncrossplane.io/update-test: |\n  - field: name\n    value: crossplane-test-instance-renamed\n" +
				"  - field: comment\n    value: \"second line\"\n",
			want: &File{Docs: []Doc{{
				For: "v1/ConfigMap", APIVersion: "v1", Kind: "ConfigMap",
				Annotations: map[string]string{
					"crossplane.io/update-test": "- field: name\n  value: crossplane-test-instance-renamed\n" +
						"- field: comment\n  value: \"second line\"\n",
				},
			}}},
		},
		"MultipleDocumentsOneFilePerTarget": {
			reason: "a sidecar for a multi-document manifest declares one document per target, in file order",
			yaml: "for: v1/Secret\nname: creds\nuptest.upbound.io/timeout: \"60\"\n---\n" +
				"for: widget.example.crossplane.io/v1alpha1/Widget\nuptest.upbound.io/timeout: \"600\"\n",
			want: &File{Docs: []Doc{
				{For: "v1/Secret", APIVersion: "v1", Kind: "Secret", Name: "creds",
					Annotations: map[string]string{"uptest.upbound.io/timeout": "60"}},
				{For: "widget.example.crossplane.io/v1alpha1/Widget", APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget",
					Annotations: map[string]string{"uptest.upbound.io/timeout": "600"}},
			}},
		},
		"BlankTrailingDocumentSkipped": {
			reason: "a stream ending in a trailing \"---\" decodes a trailing blank document, which must be " +
				"skipped rather than reported as a malformed Doc",
			yaml: "for: v1/Secret\nuptest.upbound.io/timeout: \"60\"\n---\n",
			want: &File{Docs: []Doc{
				{For: "v1/Secret", APIVersion: "v1", Kind: "Secret",
					Annotations: map[string]string{"uptest.upbound.io/timeout": "60"}},
			}},
		},
		"MissingFor": {
			reason:  "for: is mandatory on every document",
			yaml:    "uptest.upbound.io/timeout: \"60\"\n",
			wantErr: "missing mandatory for:",
		},
		"UnknownDirective": {
			reason:  "a top-level key that is neither a recognised directive nor a namespaced annotation key is an error, not a silent no-op",
			yaml:    "for: v1/Secret\ntimeout: \"60\"\n",
			wantErr: "unknown directive",
		},
		"DuplicateSelector": {
			reason:  "two documents declaring the identical selector can never resolve to two different targets",
			yaml:    "for: v1/Secret\nname: creds\nuptest.upbound.io/timeout: \"60\"\n---\nfor: v1/Secret\nname: creds\nuptest.upbound.io/timeout: \"90\"\n",
			wantErr: "duplicate selector",
		},
		"TemplatedNameRejected": {
			reason:  "a name: carrying a ${...} placeholder can never match the raw, un-templated manifest text update-tester reads",
			yaml:    "for: v1/Secret\nname: \"${Rand.String(10)}-creds\"\nuptest.upbound.io/timeout: \"60\"\n",
			wantErr: "templating placeholder",
		},
		"TemplatedNamespaceRejected": {
			reason:  "the same templating guard applies to namespace:",
			yaml:    "for: widget.example.crossplane.io/v1alpha1/Widget\nnamespace: \"${data.namespace}\"\nuptest.upbound.io/timeout: \"60\"\n",
			wantErr: "templating placeholder",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Parse([]byte(tc.yaml))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("%s: Parse() error = nil, want containing %q", tc.reason, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s: Parse() error = %q, want containing %q", tc.reason, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: Parse() error = %v", tc.reason, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: Parse() = %+v, want %+v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestConflicts exercises the exported Conflicts check directly, beyond
// what TestParse's DuplicateSelector case already confirms Parse itself
// catches — a File with no duplicate is reported clean.
func TestConflicts(t *testing.T) {
	cases := map[string]struct {
		reason  string
		file    *File
		wantErr string
	}{
		"NilFile": {
			reason: "a nil File has nothing to conflict",
			file:   nil,
		},
		"NoDuplicates": {
			reason: "distinct selectors never conflict",
			file: &File{Docs: []Doc{
				{For: "v1/Secret", Name: "a"},
				{For: "v1/Secret", Name: "b"},
			}},
		},
		"DuplicateAcrossNamespaceToo": {
			reason:  "the duplicate key includes namespace, so two selectors differing only by name or namespace are NOT reported as duplicates of each other",
			file:    &File{Docs: []Doc{{For: "v1/Secret", Name: "a", Namespace: "x"}, {For: "v1/Secret", Name: "a", Namespace: "x"}}},
			wantErr: "duplicate selector",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Conflicts(tc.file)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("%s: Conflicts() error = %v, want nil", tc.reason, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: Conflicts() error = %v, want containing %q", tc.reason, err, tc.wantErr)
			}
		})
	}
}

// TestResolve covers everything that needs the manifest's real objects to
// evaluate: ambiguity (with and without narrowing), the two redundant-
// narrowing rejections, a genuine single-field narrowing, the ".m."
// scope-confusion hint, and two differently-spelled selectors that resolve
// to the same target.
func TestResolve(t *testing.T) {
	widgetV1 := ObjectID{APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Name: "one"}
	widgetV2 := ObjectID{APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Name: "two"}
	widgetNSDefault := ObjectID{APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Name: "one", Namespace: "default"}
	widgetNSOther := ObjectID{APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Name: "one", Namespace: "other"}
	secret := ObjectID{APIVersion: "v1", Kind: "Secret", Name: "creds"}
	namespacedWidget := ObjectID{APIVersion: "widget.example.m.crossplane.io/v1alpha1", Kind: "Widget", Name: "one", Namespace: "default"}
	secretA := ObjectID{APIVersion: "v1", Kind: "Secret", Name: "a", Namespace: "ns1"}
	secretB := ObjectID{APIVersion: "v1", Kind: "Secret", Name: "b", Namespace: "ns2"}
	secretC := ObjectID{APIVersion: "v1", Kind: "Secret", Name: "c", Namespace: "ns2"}

	cases := map[string]struct {
		reason  string
		file    *File
		targets []ObjectID
		want    map[int]map[string]string
		wantErr string
	}{
		"NilFile": {
			reason:  "a nil File (no sidecar) resolves to nothing",
			file:    nil,
			targets: []ObjectID{secret},
			want:    nil,
		},
		"UniqueByGVKAlone": {
			reason:  "a for: that already selects one target needs no narrowing",
			file:    &File{Docs: []Doc{{For: "v1/Secret", APIVersion: "v1", Kind: "Secret", Annotations: map[string]string{"a": "1"}}}},
			targets: []ObjectID{secret, widgetV1},
			want:    map[int]map[string]string{0: {"a": "1"}},
		},
		"AmbiguousNamesCandidates": {
			reason: "for: matching more than one target, with no narrowing supplied, errors naming every candidate",
			file: &File{Docs: []Doc{
				{For: "widget.example.crossplane.io/v1alpha1/Widget", APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget"},
			}},
			targets: []ObjectID{widgetV1, widgetV2},
			wantErr: "ambiguous, matches one, two",
		},
		"NamespaceAloneNarrowsWhenItGenuinelyNarrows": {
			reason:  "namespace: alone is legal, and applied, when it narrows an ambiguous for: down to one target",
			file:    &File{Docs: []Doc{{For: "widget.example.crossplane.io/v1alpha1/Widget", APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Namespace: "other", Annotations: map[string]string{"a": "1"}}}},
			targets: []ObjectID{widgetNSDefault, widgetNSOther},
			want:    map[int]map[string]string{1: {"a": "1"}},
		},
		"RedundantNameRejected": {
			reason: "a name: that changes nothing (for: already unique) is rejected rather than silently accepted",
			file: &File{Docs: []Doc{
				{For: "v1/Secret", APIVersion: "v1", Kind: "Secret", Name: "creds"},
			}},
			targets: []ObjectID{secret, widgetV1},
			wantErr: "redundant",
		},
		"RedundantNamespaceRejected": {
			reason: "a namespace: that changes nothing (for: already unique) is rejected the same way",
			file: &File{Docs: []Doc{
				{For: "widget.example.crossplane.io/v1alpha1/Widget", APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget", Namespace: "default"},
			}},
			targets: []ObjectID{widgetNSDefault, secret},
			wantErr: "redundant",
		},
		"NoMatchNamesMGroup": {
			reason: "a cluster-scoped selector against a manifest that only has the .m. namespaced variant names that group in its error, rather than a bare \"matches no object\"",
			file: &File{Docs: []Doc{
				{For: "widget.example.crossplane.io/v1alpha1/Widget", APIVersion: "widget.example.crossplane.io/v1alpha1", Kind: "Widget"},
			}},
			targets: []ObjectID{namespacedWidget},
			wantErr: "widget.example.m.crossplane.io/v1alpha1",
		},
		"NoMatchNoHintWhenNoAlternateScopeExists": {
			reason:  "a selector matching nothing gets a plain error when there is no .m./non-.m. sibling to suggest",
			file:    &File{Docs: []Doc{{For: "v1/Secret", APIVersion: "v1", Kind: "Secret"}}},
			targets: []ObjectID{widgetV1},
			wantErr: "matches no object",
		},
		"DifferentlySpelledSelectorsClaimingOneTargetRejected": {
			reason: "name: a and namespace: ns1 are spelled differently but both resolve to the same object (a/ns1); " +
				"neither redundancy guard fires because each Doc sets only one narrowing directive, so this must be " +
				"caught as its own conflict rather than silently merged, leaving b and c with no annotations",
			file: &File{Docs: []Doc{
				{For: "v1/Secret", APIVersion: "v1", Kind: "Secret", Name: "a", Annotations: map[string]string{"uptest.upbound.io/timeout": "111"}},
				{For: "v1/Secret", APIVersion: "v1", Kind: "Secret", Namespace: "ns1", Annotations: map[string]string{"uptest.upbound.io/conditions": "Ready"}},
			}},
			targets: []ObjectID{secretA, secretB, secretC},
			wantErr: "both resolve to the same object (ns1/a)",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(tc.file, tc.targets)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("%s: Resolve() error = nil, want containing %q", tc.reason, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s: Resolve() error = %q, want containing %q", tc.reason, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: Resolve() error = %v", tc.reason, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: Resolve() = %+v, want %+v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestLoadReadsSidecarFile is an end-to-end check of Load against a real
// file on disk, confirming it delegates to Parse correctly rather than
// only ever being exercised through Parse directly.
func TestLoadReadsSidecarFile(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "widget.yaml")
	if err := os.WriteFile(manifest, []byte("apiVersion: widget.example.crossplane.io/v1alpha1\nkind: Widget\n"), 0o600); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	sidecarPath := PathFor(manifest)
	if err := os.WriteFile(sidecarPath, []byte("for: widget.example.crossplane.io/v1alpha1/Widget\nuptest.upbound.io/timeout: \"600\"\n"), 0o600); err != nil {
		t.Fatalf("writing sidecar: %v", err)
	}

	f, err := Load(manifest)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if f == nil || len(f.Docs) != 1 || f.Docs[0].Annotations["uptest.upbound.io/timeout"] != "600" {
		t.Fatalf("Load() = %+v, want one Doc with timeout 600", f)
	}
}
