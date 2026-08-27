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

	statusTested              = "tested"
	statusSkipped             = "skipped"
	statusSkippedUnstructured = "skipped-unstructured"
	statusImmutable           = "immutable"
	statusRefPlumbing         = "reference-plumbing"
	statusMissing             = "MISSING"

	fieldArmA = "botProtectionSetting"
	fieldArmB = "defaultBotSetting"
	fieldArmC = "thirdArmSetting"

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
			{Field: fieldName, Skip: manifest.LegacySkip("renaming changes external identity")},
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
		fieldName:          statusSkippedUnstructured,
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

// TestValidateManifestKnownDefectCountsAsCovered pins the coverage-gate
// proof cc2f1edb's test plan asked for and never implemented: a manifest
// whose only entry for a mutable field is a knownDefect entry (value: set,
// no skip:) reports that field as fully covered, exactly like an ordinary
// entry — update-test.validate must not demand a separate value: entry on
// top of the one the knownDefect entry already carries, and a provider
// adopting the token must not red-line on validate.
func TestValidateManifestKnownDefectCountsAsCovered(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Comment", JSONName: fieldComment},
	}
	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{Field: fieldComment, Value: "Updated by update-tester", KnownDefect: "e9ce03ee-920d-46f5-9aa3-120228b196fb"},
		},
	}

	result := ValidateManifest(m, fields)

	if !result.AllGood {
		t.Fatalf("expected AllGood=true — a knownDefect entry with a value: still runs the test and covers the field; got fields: %+v", result.Fields)
	}
	statusByName := statusMap(result)
	if got := statusByName[fieldComment]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q — a knownDefect entry is credited the same way an ordinary entry is", fieldComment, got, statusTested)
	}
}

// TestValidateManifestClearCreditsSwitchSiblings proves (AC a): a
// non-skipped entry's clear: list credits every named sibling, in addition
// to its own field, under a status distinct from "tested" — never folded
// into it, per the ruling this ticket implements.
func TestValidateManifestClearCreditsSwitchSiblings(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
		{GoName: "DefaultBotSetting", JSONName: fieldArmB},
		{GoName: "ThirdArmSetting", JSONName: fieldArmC},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{
				Field: fieldArmA,
				Value: map[string]interface{}{},
				Clear: []string{fieldArmB, fieldArmC},
			},
		},
	}

	result := ValidateManifest(m, fields)

	if !result.AllGood {
		t.Fatalf("expected AllGood=true (all three arms covered — one direct, two via clear:); got fields: %+v", result.Fields)
	}

	statusByName := statusMap(result)
	if got := statusByName[fieldArmA]; got != statusTested {
		t.Errorf("field %q (the entry's own field): status = %q, want %q", fieldArmA, got, statusTested)
	}
	for _, sibling := range []string{fieldArmB, fieldArmC} {
		if got := statusByName[sibling]; got != clearCreditStatus {
			t.Errorf("field %q (named only in clear:): status = %q, want %q (distinct from %q)", sibling, got, clearCreditStatus, statusTested)
		}
	}
	if clearCreditStatus == statusTested {
		t.Fatalf("clearCreditStatus must be distinct from %q", statusTested)
	}
}

// TestValidateManifestClearNeverDowngradesDirectEntry proves that a sibling
// named in one entry's clear: list, but ALSO independently covered by its
// own direct "field:" entry elsewhere in the same manifest, keeps its own
// (stronger) status — clear: credit only fills a gap, it never downgrades
// real direct coverage. Checked in both Tests-slice orderings, since
// ValidateManifest must not depend on which entry appears first.
func TestValidateManifestClearNeverDowngradesDirectEntry(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
		{GoName: "DefaultBotSetting", JSONName: fieldArmB},
	}

	directEntry := manifest.UpdateTest{Field: fieldArmB, Value: "direct-value"}
	switchEntry := manifest.UpdateTest{
		Field: fieldArmA,
		Value: map[string]interface{}{},
		Clear: []string{fieldArmB},
	}

	orderings := map[string][]manifest.UpdateTest{
		"DirectFirst": {directEntry, switchEntry},
		"SwitchFirst": {switchEntry, directEntry},
	}

	for name, tests := range orderings {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{Kind: kindWidget, Tests: tests}
			result := ValidateManifest(m, fields)

			if !result.AllGood {
				t.Fatalf("expected AllGood=true; got fields: %+v", result.Fields)
			}
			statusByName := statusMap(result)
			if got := statusByName[fieldArmB]; got != statusTested {
				t.Errorf("field %q: status = %q, want %q (its own direct entry must win regardless of ordering)", fieldArmB, got, statusTested)
			}
		})
	}
}

// TestValidateManifestClearSkippedEntryGrantsNoCredit confirms the credit
// pass mirrors the direct-entry pass's own skip handling: a "skip:" entry's
// clear: list must not silently grant coverage credit to fields nobody
// actually exercised.
func TestValidateManifestClearSkippedEntryGrantsNoCredit(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
		{GoName: "DefaultBotSetting", JSONName: fieldArmB},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{Field: fieldArmA, Skip: manifest.LegacySkip("not switchable in this backend"), Clear: []string{fieldArmB}},
		},
	}

	result := ValidateManifest(m, fields)

	statusByName := statusMap(result)
	if got := statusByName[fieldArmA]; got != statusSkippedUnstructured {
		t.Errorf("field %q: status = %q, want %q", fieldArmA, got, statusSkippedUnstructured)
	}
	if got := statusByName[fieldArmB]; got != statusMissing {
		t.Errorf("field %q: status = %q, want %q — a skip: entry's clear: list must grant no credit", fieldArmB, got, statusMissing)
	}
	if result.AllGood {
		t.Fatal("expected AllGood=false — fieldArmB is genuinely uncovered")
	}
}

// TestValidateManifestClearTargetUnknownField proves (AC b): a clear: entry
// naming a field that is NOT itself a declared struct field on the target
// type does not silently pass. The chosen failure mode is explicit and
// mechanical: it is reported as a distinct FieldValidation entry under
// clearTargetUnknownStatus and flips AllGood to false — the same reporting
// channel a MISSING field already uses — rather than a hard error return,
// so a single validation run still surfaces every problem in the manifest
// at once instead of stopping at the first one.
func TestValidateManifestClearTargetUnknownField(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{
				Field: fieldArmA,
				Value: map[string]interface{}{},
				Clear: []string{"noSuchField"},
			},
		},
	}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatal("expected AllGood=false — clear: names a field absent from the struct entirely")
	}

	statusByName := statusMap(result)
	if got := statusByName["noSuchField"]; got != clearTargetUnknownStatus {
		t.Errorf(`status["noSuchField"] = %q, want %q`, got, clearTargetUnknownStatus)
	}
	// The entry's own field must still be credited independently — an
	// invalid sibling in someone else's clear: list must not poison an
	// otherwise-valid direct entry.
	if got := statusByName[fieldArmA]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q", fieldArmA, got, statusTested)
	}
}

// TestValidateManifestWithValuesTargetUnknownField is withValues:'s
// counterpart to TestValidateManifestClearTargetUnknownField above (ticket
// cf604fcf-24aa-4ba9-8cfa-2f1d906d882b, site 1): a withValues: entry naming
// a field that is NOT itself a declared struct field on the target type
// must not silently pass. Today it is accepted at parse time, folded into
// the merge patch, and then pruned by the API server's structural-schema
// pruning — a wrong answer, never surfaced. It must instead fail the same
// way an unknown clear: target does: a distinct FieldValidation entry and
// AllGood=false.
func TestValidateManifestWithValuesTargetUnknownField(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{
				Field:      fieldArmA,
				Value:      "new-value",
				WithValues: map[string]interface{}{"noSuchField": "x"},
			},
		},
	}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatal("expected AllGood=false — withValues: names a field absent from the struct entirely")
	}

	statusByName := statusMap(result)
	if got := statusByName["noSuchField"]; got != withValuesTargetUnknownStatus {
		t.Errorf(`status["noSuchField"] = %q, want %q`, got, withValuesTargetUnknownStatus)
	}
	// The entry's own field must still be credited independently — an
	// invalid sibling in someone else's withValues: map must not poison an
	// otherwise-valid direct entry.
	if got := statusByName[fieldArmA]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q", fieldArmA, got, statusTested)
	}
}

// TestValidateManifestWithValuesGrantsNoCoverageCredit pins the asymmetry
// the ticket calls out (site 1, AC bullet 2): a withValues: sibling's
// post-patch value is never asserted by the runner, unlike a clear:
// sibling's proven null, so it must earn NO coverage credit at all — never
// clearCreditStatus, never any status distinct from what the field would
// report with no withValues: mention. A real, declared sibling named only
// in withValues:, with no entry of its own, must still report MISSING.
func TestValidateManifestWithValuesGrantsNoCoverageCredit(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "BotProtectionSetting", JSONName: fieldArmA},
		{GoName: "DefaultBotSetting", JSONName: fieldArmB},
	}

	m := &manifest.Manifest{
		Kind: kindWidget,
		Tests: []manifest.UpdateTest{
			{
				Field:      fieldArmA,
				Value:      "new-value",
				WithValues: map[string]interface{}{fieldArmB: "derived-value"},
			},
		},
	}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatal("expected AllGood=false — fieldArmB is named only in withValues: and earns no credit for it")
	}

	statusByName := statusMap(result)
	if got := statusByName[fieldArmA]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q", fieldArmA, got, statusTested)
	}
	if got := statusByName[fieldArmB]; got != statusMissing {
		t.Errorf("field %q: status = %q, want %q — withValues: must grant no coverage credit (contrast clearCreditStatus)", fieldArmB, got, statusMissing)
	}
	if got := statusByName[fieldArmB]; got == clearCreditStatus {
		t.Errorf("field %q must never be credited under clearCreditStatus via withValues:", fieldArmB)
	}
}

// TestValidateManifestAssertUnchangedCoverage is the acceptance case this
// ticket implements: a field named only by the manifest's top-level
// "assert-unchanged:" directive (never by its own "field:"/"skip:" entry —
// manifest.ParseAnnotation rejects that overlap at parse time) must be
// credited as covered, under a status distinct from "tested"/"skipped", and
// a field covered by neither mechanism must still report MISSING — the
// credit must not widen into "any mention counts".
func TestValidateManifestAssertUnchangedCoverage(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Comment", JSONName: fieldComment},
		{GoName: "Region", JSONName: fieldRegion},
	}

	m := &manifest.Manifest{
		Kind:            kindWidget,
		AssertUnchanged: []string{fieldRegion},
		Tests: []manifest.UpdateTest{
			{Field: fieldComment, Value: "updated"},
		},
	}

	result := ValidateManifest(m, fields)

	if !result.AllGood {
		t.Fatalf("expected AllGood=true — comment is tested, region is guarded by assert-unchanged; got fields: %+v", result.Fields)
	}
	statusByName := statusMap(result)
	if got := statusByName[fieldRegion]; got != assertUnchangedCreditStatus {
		t.Errorf("field %q (named only by assert-unchanged): status = %q, want %q", fieldRegion, got, assertUnchangedCreditStatus)
	}
	if got := statusByName[fieldComment]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q", fieldComment, got, statusTested)
	}
	if assertUnchangedCreditStatus == statusTested || assertUnchangedCreditStatus == statusSkipped {
		t.Fatalf("assertUnchangedCreditStatus must be distinct from %q and %q", statusTested, statusSkipped)
	}
}

// TestValidateManifestAssertUnchangedNestedPathCreditsTopLevelField proves
// the credit resolves a dot-separated status.atProvider path (as f5xc's
// "legacyRuleList.rules" guards a nested member) against the top-level spec
// field named by its first segment, exactly the shape the directive is
// documented to carry.
func TestValidateManifestAssertUnchangedNestedPathCreditsTopLevelField(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "LegacyRuleList", JSONName: "legacyRuleList"},
	}

	m := &manifest.Manifest{
		Kind:            kindWidget,
		AssertUnchanged: []string{"legacyRuleList.rules"},
	}

	result := ValidateManifest(m, fields)

	if !result.AllGood {
		t.Fatalf("expected AllGood=true; got fields: %+v", result.Fields)
	}
	statusByName := statusMap(result)
	if got := statusByName["legacyRuleList"]; got != assertUnchangedCreditStatus {
		t.Errorf(`status["legacyRuleList"] = %q, want %q`, got, assertUnchangedCreditStatus)
	}
}

// TestValidateManifestAssertUnchangedFieldWithNeitherStillMissing pins the
// non-widening bound explicitly: a field named by NEITHER a test entry NOR
// the assert-unchanged directive still reports MISSING.
func TestValidateManifestAssertUnchangedFieldWithNeitherStillMissing(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Comment", JSONName: fieldComment},
		{GoName: "Region", JSONName: fieldRegion},
	}

	m := &manifest.Manifest{
		Kind:            kindWidget,
		AssertUnchanged: []string{fieldRegion},
	}

	result := ValidateManifest(m, fields)

	if result.AllGood {
		t.Fatal("expected AllGood=false — comment is covered by neither a test entry nor assert-unchanged")
	}
	statusByName := statusMap(result)
	if got := statusByName[fieldComment]; got != statusMissing {
		t.Errorf("field %q: status = %q, want %q", fieldComment, got, statusMissing)
	}
	if got := statusByName[fieldRegion]; got != assertUnchangedCreditStatus {
		t.Errorf("field %q: status = %q, want %q", fieldRegion, got, assertUnchangedCreditStatus)
	}
}

// TestValidateManifestAssertUnchangedNeverDowngradesDirectEntry proves the
// credit only fills a gap: a field covered by its own direct test entry
// keeps that entry's status even if some OTHER assert-unchanged path also
// resolves to it (e.g. a stale directive left after the field's own entry
// was added) — never overwritten with the weaker assert-unchanged status.
func TestValidateManifestAssertUnchangedNeverDowngradesDirectEntry(t *testing.T) {
	fields := []FieldInfo{
		{GoName: "Comment", JSONName: fieldComment},
	}

	m := &manifest.Manifest{
		Kind:            kindWidget,
		AssertUnchanged: []string{fieldComment},
		Tests: []manifest.UpdateTest{
			{Field: fieldComment, Value: "updated"},
		},
	}

	result := ValidateManifest(m, fields)

	statusByName := statusMap(result)
	if got := statusByName[fieldComment]; got != statusTested {
		t.Errorf("field %q: status = %q, want %q (its own direct entry must win)", fieldComment, got, statusTested)
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

// TestParseStructFieldsFoundButEmpty pins the "struct declaration seen" vs
// "fields parsed" distinction: a nullary struct (the shape a generated
// zero-payload union-arm marker type takes, "type Empty struct{}") is a
// real, resolvable declaration with zero members, not an unresolvable one.
// Every caller downstream distinguishes "resolved" from "unresolved" solely
// by nil-ness of the returned slice, so a found struct must come back as a
// non-nil, zero-length slice with a nil error — never nil with a nil error,
// which would be indistinguishable from "not found" to those callers.
func TestParseStructFieldsFoundButEmpty(t *testing.T) {
	src := `package v1alpha1

type Empty struct{}
`
	path := writeFixture(t, "zz_setup.go", src)

	fields, err := ParseStructFields(path, "Empty")
	if err != nil {
		t.Fatalf("ParseStructFields: unexpected error: %v", err)
	}
	if fields == nil {
		t.Fatal("fields = nil, want a non-nil, zero-length slice for a found-but-empty struct")
	}
	if len(fields) != 0 {
		t.Errorf("fields = %+v, want zero-length", fields)
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
// across every known status value, including the reference-plumbing status
// and the two clear:-related statuses.
func TestPrintValidationDoesNotPanic(t *testing.T) {
	r := &ValidationResult{
		Kind: kindWidget,
		Fields: []FieldValidation{
			{JSONName: statusTested, Status: statusTested},
			{JSONName: statusSkipped, Status: statusSkipped},
			{JSONName: statusImmutable, Status: statusImmutable},
			{JSONName: "refField", Status: statusRefPlumbing},
			{JSONName: "missing", Status: statusMissing},
			{JSONName: fieldArmB, Status: clearCreditStatus},
			{JSONName: "noSuchField", Status: clearTargetUnknownStatus},
			{JSONName: "noSuchWithValuesField", Status: withValuesTargetUnknownStatus},
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
