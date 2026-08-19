package validator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// Field JSON names and validation statuses reused across the table-driven
// cases below. Declaring them once keeps the package's total literal
// occurrence count under a typical goconst threshold (min-occurrences: 5)
// instead of re-typing the same status/field-name strings validator.go
// already emits.
const (
	fieldName          = "name"
	fieldOwner         = "owner"
	fieldOwnerRef      = "ownerRef"
	fieldOwnerRefs     = "ownerRefs"
	fieldOwnerSelector = "ownerSelector"
	fieldComment       = "comment"
	fieldRegion        = "region"

	statusTested      = "tested"
	statusSkipped     = "skipped"
	statusImmutable   = "immutable"
	statusRefPlumbing = "reference-plumbing"
	statusMissing     = "MISSING"

	kindWidget = "Widget"
)

// TestIsReferencePlumbingField covers the *Ref/*Selector exclusion logic:
// a field is only treated as generated reference-plumbing when its
// same-prefixed base value field is present in the struct's field set.
func TestIsReferencePlumbingField(t *testing.T) {
	cases := map[string]struct {
		reason   string
		jsonName string
		fieldSet map[string]bool
		want     bool
	}{
		"RefWithBaseField": {
			reason:   "ownerRef is reference plumbing when owner exists",
			jsonName: fieldOwnerRef,
			fieldSet: map[string]bool{fieldOwner: true, fieldOwnerRef: true},
			want:     true,
		},
		"RefsWithBaseField": {
			reason:   "ownerRefs (plural, the list-typed base's Ref companion) is reference plumbing when owner exists — angryjet emits a plural Refs slice alongside a singular Selector for []string base fields",
			jsonName: fieldOwnerRefs,
			fieldSet: map[string]bool{fieldOwner: true, fieldOwnerRefs: true},
			want:     true,
		},
		"RefsWithoutBaseField": {
			reason:   "a *Refs field with no matching base value field is not excused — it must be reported MISSING, same as the singular *Ref case",
			jsonName: "danglingRefs",
			fieldSet: map[string]bool{"danglingRefs": true},
			want:     false,
		},
		"SelectorWithBaseField": {
			reason:   "ownerSelector is reference plumbing when owner exists",
			jsonName: fieldOwnerSelector,
			fieldSet: map[string]bool{fieldOwner: true, fieldOwnerSelector: true},
			want:     true,
		},
		"RefWithoutBaseField": {
			reason:   "a *Ref field with no matching base value field is not excused — it must be reported MISSING",
			jsonName: "danglingRef",
			fieldSet: map[string]bool{"danglingRef": true},
			want:     false,
		},
		"SelectorWithoutBaseField": {
			reason:   "a *Selector field with no matching base value field is not excused",
			jsonName: "danglingSelector",
			fieldSet: map[string]bool{"danglingSelector": true},
			want:     false,
		},
		"PlainFieldNoSuffix": {
			reason:   "a field with no Ref/Selector suffix is never reference plumbing",
			jsonName: fieldComment,
			fieldSet: map[string]bool{fieldComment: true},
			want:     false,
		},
		"BareRefSuffixNoBase": {
			reason:   "a field literally named 'Ref' has an empty base and must not match",
			jsonName: "ref",
			fieldSet: map[string]bool{"ref": true, "": true},
			want:     false,
		},
		"BareSelectorSuffixNoBase": {
			reason:   "a field literally named 'Selector' has an empty base and must not match",
			jsonName: "selector",
			fieldSet: map[string]bool{"selector": true, "": true},
			want:     false,
		},
		"CaseSensitiveSuffix": {
			reason:   "suffix matching is case-sensitive — 'ref' (lowercase) is not the 'Ref' suffix",
			jsonName: "ownerref",
			fieldSet: map[string]bool{fieldOwner: true, "ownerref": true},
			want:     false,
		},
		"RefSuffixBaseMissingFromSet": {
			reason:   "the base field name is derived correctly but must actually be present in fieldSet",
			jsonName: fieldOwnerRef,
			fieldSet: map[string]bool{fieldOwnerRef: true, "somethingElse": true},
			want:     false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := isReferencePlumbingField(tc.jsonName, tc.fieldSet)
			if got != tc.want {
				t.Errorf("%s: isReferencePlumbingField(%q, %v) = %v, want %v",
					tc.reason, tc.jsonName, tc.fieldSet, got, tc.want)
			}
		})
	}
}

// TestValidateManifestReferencePlumbingExclusion is the regression test for
// the false positive this exclusion closes: ownerRef/ownerRefs/ownerSelector
// must not be reported MISSING when only the base owner field is covered by
// the update-test annotation. ownerRefs stands in for the plural companion
// angryjet generates for a list-typed base field ([]string), which real
// generated types carry alongside the singular Selector.
func TestValidateManifestReferencePlumbingExclusion(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Name", JSONName: fieldName},
		{GoName: "Owner", JSONName: fieldOwner},
		{GoName: "OwnerRef", JSONName: fieldOwnerRef},
		{GoName: "OwnerRefs", JSONName: fieldOwnerRefs},
		{GoName: "OwnerSelector", JSONName: fieldOwnerSelector},
		{GoName: "Comment", JSONName: fieldComment},
		{GoName: "Region", JSONName: fieldRegion, Immutable: true},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{Field: fieldName, Skip: "renaming changes external identity"},
			{Field: fieldOwner, Value: "updated-owner"},
			{Field: fieldComment, Value: "Updated by uptest"},
		},
	}

	result := ValidateManifest(m, fields)

	if !result.AllGood {
		t.Fatalf("expected AllGood=true (ownerRef/ownerRefs/ownerSelector should be excluded, not MISSING); got fields: %+v", result.Fields)
	}

	statusByName := statusMap(result)

	want := map[string]string{
		fieldName:          statusSkipped,
		fieldOwner:         statusTested,
		fieldOwnerRef:      statusRefPlumbing,
		fieldOwnerRefs:     statusRefPlumbing,
		fieldOwnerSelector: statusRefPlumbing,
		fieldComment:       statusTested,
		fieldRegion:        statusImmutable,
	}
	for field, wantStatus := range want {
		if got := statusByName[field]; got != wantStatus {
			t.Errorf("field %q: status = %q, want %q", field, got, wantStatus)
		}
	}
}

// TestValidateManifestGenuinelyMissingBaseField ensures the exclusion is
// conditional: if the base value field itself is dropped from the struct
// (renamed, removed) — not merely uncovered by the annotation — the Ref/
// Selector pair has no base field in fieldSet and must be scoped normally,
// i.e. reported MISSING like any other uncovered field rather than silently
// excused.
func TestValidateManifestGenuinelyMissingBaseField(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "OwnerRef", JSONName: fieldOwnerRef},
		{GoName: "OwnerSelector", JSONName: fieldOwnerSelector},
	}

	m := &manifest.Manifest{Kind: kindWidget}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatalf("expected AllGood=false — ownerRef/ownerSelector have no matching base field (owner) in the struct, so they must not be excused as reference plumbing")
	}

	for _, f := range result.Fields {
		if f.Status != statusMissing {
			t.Errorf("field %q: status = %q, want %s (no base value field present)", f.JSONName, f.Status, statusMissing)
		}
	}
}

// TestValidateManifestMissingField confirms a genuinely uncovered mutable
// field (no reference-suffix, no annotation entry) still fails validation.
func TestValidateManifestMissingField(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Comment", JSONName: fieldComment},
	}
	m := &manifest.Manifest{Kind: kindWidget}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatal("expected AllGood=false for an uncovered mutable field")
	}
	if len(result.Fields) != 1 || result.Fields[0].Status != statusMissing {
		t.Errorf("got fields %+v, want a single %s comment field", result.Fields, statusMissing)
	}
}

// widgetTypesSrc is a synthetic generated-types fixture containing the
// three-field reference pattern (owner/ownerRef/ownerSelector), an immutable
// field, and a sibling *Parameters struct that must be skipped.
const widgetTypesSrc = `// Code generated by openapi2crd. DO NOT EDIT.
package v1alpha1

// OtherParameters is a decoy struct that must be skipped while scanning
// for WidgetParameters.
type OtherParameters struct {
	Unrelated *string ` + "`json:\"unrelated\"`" + `
}

// WidgetParameters are the configurable fields of a Widget.
type WidgetParameters struct {
	Name *string ` + "`json:\"name\"`" + `
	// +crossplane:generate:reference:type=github.com/example/apis/v1alpha1.Owner
	Owner *string ` + "`json:\"owner\"`" + `
	// +optional
	OwnerRef *xpv1.Reference ` + "`json:\"ownerRef,omitempty\"`" + `
	// +optional
	OwnerSelector *xpv1.Selector ` + "`json:\"ownerSelector,omitempty\"`" + `
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable after creation"
	Region *string ` + "`json:\"region\"`" + `
}
`

// TestParseGoTypes verifies the Go-source struct scanner against a small
// synthetic types file containing the three-field reference pattern, an
// immutable field, and a sibling *Parameters struct that must be skipped.
func TestParseGoTypes(t *testing.T) {
	path := writeFixture(t, "widget_types.go", widgetTypesSrc)

	fields, err := ParseGoTypes(path, kindWidget)
	if err != nil {
		t.Fatalf("ParseGoTypes: %v", err)
	}

	want := []FieldInfo{
		{GoName: "Name", JSONName: fieldName, GoType: "*string"},
		{GoName: "Owner", JSONName: fieldOwner, GoType: "*string"},
		{GoName: "OwnerRef", JSONName: fieldOwnerRef, GoType: "*xpv1.Reference", Omitempty: true},
		{GoName: "OwnerSelector", JSONName: fieldOwnerSelector, GoType: "*xpv1.Selector", Omitempty: true},
		{GoName: "Region", JSONName: fieldRegion, Immutable: true, GoType: "*string"},
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(fields), len(want), fields)
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("field %d: got %+v, want %+v", i, fields[i], w)
		}
	}
}

// TestParseGoTypesStructNotFound confirms a clear error when the target
// Parameters struct doesn't exist in the file.
func TestParseGoTypesStructNotFound(t *testing.T) {
	src := `package v1alpha1

type OtherParameters struct {
	Name *string ` + "`json:\"name\"`" + `
}
`
	path := writeFixture(t, "types.go", src)

	if _, err := ParseGoTypes(path, kindWidget); err == nil {
		t.Fatal("expected an error when WidgetParameters is not present in the file")
	}
}

// TestParseGoTypesMissingFile confirms a clear error for a nonexistent path.
func TestParseGoTypesMissingFile(t *testing.T) {
	if _, err := ParseGoTypes(filepath.Join(t.TempDir(), "does-not-exist.go"), kindWidget); err == nil {
		t.Fatal("expected an error for a nonexistent types file")
	}
}

// TestParseGoTypesEndToEnd exercises the parser, the manifest annotation
// parser and the exclusion logic together, as the CLI wires them: a manifest
// covering only the base owner field must fully validate against a types
// struct that also declares the generated ownerRef/ownerSelector companions.
func TestParseGoTypesEndToEnd(t *testing.T) {
	typesPath := writeFixture(t, "widget_types.go", widgetTypesSrc)

	manifestSrc := `apiVersion: example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
  annotations:
    crossplane.io/update-test: |
      - field: name
        skip: "renaming changes external identity"
      - field: owner
        value: "updated-owner"
spec:
  forProvider:
    name: example-widget
    owner: original-owner
    region: eu-central-1
`
	manifestPath := writeFixture(t, "widget.yaml", manifestSrc)

	m, err := manifest.Parse(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}

	fields, err := ParseGoTypes(typesPath, m.Kind)
	if err != nil {
		t.Fatalf("ParseGoTypes: %v", err)
	}

	result := ValidateManifest(m, fields)
	if !result.AllGood {
		t.Fatalf("expected the manifest to fully cover WidgetParameters (ownerRef/ownerSelector excluded as reference plumbing); got: %+v", result.Fields)
	}

	statusByName := statusMap(result)
	for _, refField := range []string{fieldOwnerRef, fieldOwnerSelector} {
		if got := statusByName[refField]; got != statusRefPlumbing {
			t.Errorf("field %q: status = %q, want %s", refField, got, statusRefPlumbing)
		}
	}
}

// TestPrintValidationDoesNotPanic is a smoke test for the output formatter
// across every known status value, including the reference-plumbing status.
func TestPrintValidationDoesNotPanic(t *testing.T) {
	r := &ValidationResult{
		Kind: kindWidget,
		Fields: []FieldValidation{
			{JSONName: statusTested, Status: statusTested},
			{JSONName: statusSkipped, Status: statusSkipped},
			{JSONName: statusImmutable, Status: statusImmutable},
			{JSONName: "refField", Status: statusRefPlumbing},
			{JSONName: "missing", Status: statusMissing},
		},
		AllGood: false,
	}
	PrintValidation(r)
}

// writeFixture writes contents to a uniquely named temp directory and returns
// the file path, failing the test if the write does not succeed.
func writeFixture(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", name, err)
	}
	return path
}

// statusMap builds a JSONName → Status lookup from a ValidationResult, used
// by several tests above to assert per-field status without depending on
// slice ordering.
func statusMap(r *ValidationResult) map[string]string {
	m := make(map[string]string, len(r.Fields))
	for _, f := range r.Fields {
		m[f.JSONName] = f.Status
	}
	return m
}
