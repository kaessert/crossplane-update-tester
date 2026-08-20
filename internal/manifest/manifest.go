// Package manifest parses Crossplane example manifests and the update-test
// annotations that drive the tester's checks.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AnnotationKey names the manifest annotation carrying the per-field update
// test list. See ParseAnnotation for the format.
const AnnotationKey = "crossplane.io/update-test"

// ExpectExternalNamePrefixKey names the manifest annotation that declares the
// required prefix of the live resource's crossplane.io/external-name
// annotation.
//
// It exists for resources whose backend models more than one object type
// behind a single Kubernetes kind, where an identity search issued against
// the wrong object type returns zero matches and the reconciler silently
// creates a duplicate while still reporting Ready — a failure invisible to a
// plain Ready assertion. Optional: manifests that do not need this guard omit
// it, and the checks gated on it are skipped.
const ExpectExternalNamePrefixKey = "crossplane.io/expect-external-name-prefix"

// UpdateTest represents a single field update test parsed from the annotation.
type UpdateTest struct {
	Field  string      `yaml:"field"`
	Value  interface{} `yaml:"value"`
	Expect interface{} `yaml:"expect"`
	Skip   string      `yaml:"skip"`
	// Clear names OTHER top-level spec.forProvider fields that must be
	// nulled in the SAME merge patch that sets Field's own Value.
	//
	// It exists for a union modeled as separate top-level *Parameters
	// fields rather than as alternative values nested inside one field's
	// own object (the latter shape already works: a JSON merge patch
	// naturally nulls a sibling KEY WITHIN the field it is patching). Two
	// sequential single-field patches — one setting the new arm, one
	// nulling the old — would each independently succeed at the
	// Kubernetes level while leaving both arms set on the backend between
	// them, and if the backend enforces mutual exclusivity that races the
	// test against the backend's own validation. Folding every named
	// sibling's null into ONE merge-patch object makes the switch atomic.
	//
	// Every OTHER member of the union group must be named here, not only
	// the one arm being switched away from — see ValidateClear and
	// runner.buildMergePatch for how this list is consumed.
	Clear []string `yaml:"clear"`
}

// Manifest holds the parsed Kubernetes manifest metadata needed for testing.
type Manifest struct {
	APIVersion   string
	Kind         string
	Name         string
	Namespace    string
	Tests        []UpdateTest
	ConvergeSkip string
	// ExpectExternalNamePrefix is the value of the
	// crossplane.io/expect-external-name-prefix annotation, when present.
	// Empty means the manifest declares no external-name-prefix
	// expectation — see ExpectExternalNamePrefixKey.
	ExpectExternalNamePrefix string
	// AssertUnchanged lists dot-separated status.atProvider field paths that
	// must hold the SAME value for the entire duration of a `run` (the
	// per-field update tests), regardless of which other field is being
	// patched. It is populated from the "assert-unchanged:" directive line
	// in the crossplane.io/update-test annotation — see ParseAnnotation.
	//
	// This exists for a backend that silently defaults an omitted field on
	// every write: a PUT that patches one unrelated field can still cause
	// the backend to reset a field the request never mentioned, and that
	// reset returns the same 200 a genuine update would. A value-only
	// assertion on the field being patched cannot see this, because the
	// field it corrupts is never the one under test. Declaring the
	// vulnerable field here makes the runner check it after every patch in
	// the run and fail the run the moment it moves — see runner.Runner.RunTests.
	AssertUnchanged []string
	// IgnoreFields lists TOP-LEVEL status.atProvider field names excluded
	// from a convergence check's snapshot diff for THIS resource only. It is
	// populated from the "ignore-fields:" directive line in the
	// crossplane.io/update-test annotation — see ParseAnnotation.
	//
	// Unlike AssertUnchanged, a dot-separated path is NOT supported here:
	// differ.DiffSnapshotsExcluding matches the exclusion set against the
	// top-level keys of the status.atProvider snapshot only, so a nested
	// path would exclude nothing. Rather than accept that silently,
	// ParseAnnotation rejects a dotted entry outright — every entry that
	// reaches this slice is guaranteed to be a single top-level key.
	//
	// This exists because the fleet-wide `converge-all --ignore-fields` flag
	// is lossy the moment two resources in the same run need different
	// exclusions: a single flag applies its whole set to every target,
	// widening each resource's exclusion to the union of all of them.
	// Declaring the exclusion here keeps it next to the resource it
	// describes, so a field excluded for one manifest is never silently
	// excluded for another — see cmdConvergeAll.
	IgnoreFields []string
	// ForProvider is the manifest's own spec.forProvider, decoded as plain
	// JSON-shaped data (map[string]interface{}, []interface{}, and scalars —
	// gopkg.in/yaml.v3 already decodes mapping nodes as map[string]interface{}
	// with string keys, so no further normalisation is needed). It is the
	// create-time value the runner's first Patch() call merges against, and
	// exists so an offline check can simulate that RFC 7386 merge without a
	// live cluster — see validator.CheckMergePatchSiblings. Nil when the
	// manifest has no spec.forProvider at all.
	ForProvider map[string]interface{}
}

// manifestDoc is the intermediate YAML structure for parsing.
type manifestDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		ForProvider map[string]interface{} `yaml:"forProvider"`
	} `yaml:"spec"`
}

// Parse reads a YAML manifest file and extracts metadata and update test
// annotations.
func Parse(path string) (*Manifest, error) {
	// #nosec G304 -- path is an operator-supplied CLI argument (the
	// manifest file to test), not attacker-controlled input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses manifest YAML bytes.
//
// The input may be a multi-document ("---"-separated) YAML stream: Crossplane
// example manifests sometimes ship a companion object (a Secret, a
// ProviderConfig) in the same file as the managed resource under test. Every
// document is decoded and the one carrying the AnnotationKey annotation is
// selected, because the companion document is frequently written first and
// also has a non-empty apiVersion/kind/metadata.name — decoding only the
// leading document would silently test the wrong object and report zero
// update tests.
//
// When no document carries the annotation the first valid document wins,
// which keeps single-document manifests (and multi-document ones that declare
// no update tests) behaving exactly as they did before multi-document support
// existed.
func ParseBytes(data []byte) (*Manifest, error) {
	docs, err := decodeManifestDocs(data)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("manifest missing apiVersion or kind")
	}

	selected := docs[0]
	for _, d := range docs {
		if _, ok := d.Metadata.Annotations[AnnotationKey]; ok {
			selected = d
			break
		}
	}

	return manifestFromDoc(selected)
}

// decodeManifestDocs splits a (possibly multi-document) YAML byte stream and
// decodes each document into a manifestDoc, skipping documents that do not
// look like a Kubernetes object — either blank (a trailing "---" separator
// yields an empty document, which is legal YAML but has nothing to test) or
// missing apiVersion/kind. Skipping rather than erroring is what lets the
// caller report the original "manifest missing apiVersion or kind" for a
// stream that contains no usable document at all.
func decodeManifestDocs(data []byte) ([]manifestDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []manifestDoc
	for {
		var doc manifestDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing manifest YAML: %w", err)
		}
		if doc.APIVersion == "" || doc.Kind == "" {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// manifestFromDoc converts a single decoded manifestDoc into a Manifest,
// parsing its update-test annotation (if present).
func manifestFromDoc(doc manifestDoc) (*Manifest, error) {
	if doc.Metadata.Name == "" {
		return nil, fmt.Errorf("manifest missing metadata.name")
	}

	m := &Manifest{
		APIVersion:  doc.APIVersion,
		Kind:        doc.Kind,
		Name:        doc.Metadata.Name,
		Namespace:   doc.Metadata.Namespace,
		ForProvider: doc.Spec.ForProvider,
	}

	m.ExpectExternalNamePrefix = doc.Metadata.Annotations[ExpectExternalNamePrefixKey]

	annotation, ok := doc.Metadata.Annotations[AnnotationKey]
	if !ok {
		return m, nil
	}

	tests, convergeSkip, assertUnchanged, ignoreFields, err := ParseAnnotation(annotation)
	if err != nil {
		return nil, fmt.Errorf("parsing %s annotation: %w", AnnotationKey, err)
	}
	m.Tests = tests
	m.ConvergeSkip = convergeSkip
	m.AssertUnchanged = assertUnchanged
	m.IgnoreFields = ignoreFields
	return m, nil
}

// ParseAnnotation parses the update-test annotation YAML string into a slice
// of UpdateTest entries, plus three optional top-level directives:
// "converge-skip" (a reason string), "assert-unchanged" (a field-path list)
// and "ignore-fields" (a field-path list).
//
// The annotation format allows all three directives to appear, each on its
// own unindented line, alongside the list of field entries:
//
//	crossplane.io/update-test: |
//	  converge-skip: "atProvider.lastSyncTime changes every observe cycle"
//	  assert-unchanged: legacyRuleList
//	  ignore-fields: latestBackup
//	  - field: name
//	    value: "updated"
//
// None is valid as a single YAML document (a mapping key cannot be a sibling
// of top-level sequence items), so all three directive lines are extracted
// first and the remainder is parsed as a plain YAML sequence.
//
// "assert-unchanged" takes a comma-separated list of dot-separated
// status.atProvider field paths — see Manifest.AssertUnchanged for what the
// runner does with them. A field named there may not also appear as an
// update-test entry's own field: patching a field and asserting it never
// changes are contradictory requests, so that combination is a parse error
// rather than a runtime race between the two.
//
// "ignore-fields" takes a comma-separated list too, but of TOP-LEVEL
// status.atProvider field names rather than dot-separated paths — see
// Manifest.IgnoreFields for what a convergence check does with it. A dotted
// entry is rejected here, at parse time, rather than silently excluding
// nothing: see ValidateIgnoreFields.
func ParseAnnotation(annotation string) ([]UpdateTest, string, []string, []string, error) {
	rest, convergeSkip, assertUnchanged, ignoreFields, err := extractDirectives(annotation)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("parsing directives: %w", err)
	}
	if err := ValidateIgnoreFields(ignoreFields); err != nil {
		return nil, "", nil, nil, err
	}

	rest = strings.TrimSpace(rest)
	var tests []UpdateTest
	if rest != "" {
		if err := yaml.Unmarshal([]byte(rest), &tests); err != nil {
			return nil, "", nil, nil, fmt.Errorf("unmarshalling annotation: %w", err)
		}
	}

	testedFields := make(map[string]bool, len(tests))
	for i, t := range tests {
		if t.Field == "" {
			return nil, "", nil, nil, fmt.Errorf("entry %d: field is required", i)
		}
		if t.Value == nil && t.Skip == "" {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): value is required unless skip is set", i, t.Field)
		}
		if err := ValidateClear(t.Field, t.Clear); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		testedFields[t.Field] = true
	}
	for _, f := range assertUnchanged {
		if testedFields[f] {
			return nil, "", nil, nil, fmt.Errorf(
				"assert-unchanged field %q is also an update-test field; a field cannot be both patched and asserted unchanged in the same run", f)
		}
	}
	return tests, convergeSkip, assertUnchanged, ignoreFields, nil
}

// extractDirectives scans the annotation text line by line for the three
// top-level (unindented) directive lines — "converge-skip:",
// "assert-unchanged:" and "ignore-fields:" — removes them from the text, and
// returns the remaining text plus what each directive carries (empty/nil
// when absent).
//
// All three are extracted the same way and for the same reason: none is
// valid as a sibling of the top-level sequence of field entries in a single
// YAML document, so each is pulled out of the raw text before the remainder
// is parsed as a plain YAML sequence.
func extractDirectives(annotation string) (rest string, convergeSkip string, assertUnchanged []string, ignoreFields []string, err error) {
	lines := strings.Split(annotation, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		switch {
		case indent == 0 && strings.HasPrefix(trimmed, "converge-skip:"):
			var single map[string]string
			if uerr := yaml.Unmarshal([]byte(line), &single); uerr != nil {
				return "", "", nil, nil, fmt.Errorf("parsing converge-skip line %q: %w", line, uerr)
			}
			convergeSkip = single["converge-skip"]
		case indent == 0 && strings.HasPrefix(trimmed, "assert-unchanged:"):
			var single map[string]string
			if uerr := yaml.Unmarshal([]byte(line), &single); uerr != nil {
				return "", "", nil, nil, fmt.Errorf("parsing assert-unchanged line %q: %w", line, uerr)
			}
			assertUnchanged = splitFieldList(single["assert-unchanged"])
		case indent == 0 && strings.HasPrefix(trimmed, "ignore-fields:"):
			var single map[string]string
			if uerr := yaml.Unmarshal([]byte(line), &single); uerr != nil {
				return "", "", nil, nil, fmt.Errorf("parsing ignore-fields line %q: %w", line, uerr)
			}
			ignoreFields = splitFieldList(single["ignore-fields"])
		default:
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n"), convergeSkip, assertUnchanged, ignoreFields, nil
}

// ValidateIgnoreFields rejects a dot-separated entry in an ignore-fields set
// at parse time, before any cluster is touched. The convergence diff
// (differ.DiffSnapshotsExcluding) matches an exclusion only against a
// TOP-LEVEL status.atProvider key, so a dotted entry like
// "ruleChoice.legacyRuleList" would otherwise parse cleanly, reach
// ConvergeOptions.IgnoreFields, match nothing, and let the check fail on
// drift the operator believed they had excluded — with no diagnostic
// pointing at why. Mirrors the shape of the assert-unchanged baseline
// rejection: name the offending path and say what is wrong with it, rather
// than let it read as a silent no-op.
//
// Exported so every source of an ignore-fields set shares this one check —
// the "ignore-fields:" manifest directive (via ParseAnnotation above), and
// the --ignore-fields flag on both `converge` and `converge-all` (main.go),
// which is also how the UPDATE_TESTER_IGNORE_FIELDS hook environment
// variable reaches validation, since the hook forwards it into `converge`'s
// own flag rather than parsing it separately.
func ValidateIgnoreFields(fields []string) error {
	for _, f := range fields {
		if strings.Contains(f, ".") {
			return fmt.Errorf(
				"ignore-fields entry %q: dot-separated paths are not supported — the convergence diff "+
					"matches only a top-level status.atProvider field name, so a nested path would silently "+
					"exclude nothing; declare the top-level field name instead", f)
		}
	}
	return nil
}

// ValidateClear rejects a clear list that the merge-patch builder cannot
// honour, at parse time, before any cluster is touched.
//
// clear only makes sense when field is ITSELF a top-level spec.forProvider
// key: the null it adds lands as a sibling KEY at the top level of the merge
// patch, alongside field's own value, and a dotted field patches into a
// nested object one level down from there — nulling a "sibling" at the top
// level in that case would not land next to the value being set at all, it
// would land next to the nested field's own PARENT object, a different (and
// currently unsupported) shape. Rather than silently building a patch that
// nulls the wrong object, a dotted field paired with a non-empty clear is
// rejected outright.
//
// Each entry in clear is validated the same way ignore-fields is: a dotted
// entry there is also rejected, since this tool only ever nulls a top-level
// sibling. An entry equal to field itself is rejected too — clear names
// OTHER members of the union, and letting it also equal field would null out
// the very value the patch is meant to set, silently undoing it (map
// iteration order is not guaranteed, so the outcome would be indeterminate
// besides being wrong).
//
// Exported so every source of a clear list shares this one check — the
// "clear:" key on an update-test annotation entry (via ParseAnnotation
// above) and the merge-patch builder that actually shapes the JSON (see
// runner.buildMergePatch).
func ValidateClear(field string, clear []string) error {
	if len(clear) == 0 {
		return nil
	}
	if strings.Contains(field, ".") {
		return fmt.Errorf(
			"clear is only supported for a top-level field; %q is nested — sibling-clearing at a "+
				"non-root nesting level is not supported", field)
	}
	for _, c := range clear {
		if strings.Contains(c, ".") {
			return fmt.Errorf(
				"clear entry %q: dot-separated paths are not supported — clear only names a "+
					"top-level spec.forProvider field", c)
		}
		if c == field {
			return fmt.Errorf(
				"clear entry %q: clear must name OTHER sibling fields, not the field being patched itself", c)
		}
	}
	return nil
}

// splitFieldList splits a comma-separated field-path list, trimming
// surrounding whitespace from each entry and dropping empty ones. Returns
// nil for an empty or whitespace-only input, matching the "absent" state
// callers already treat len(...) == 0 as.
func splitFieldList(raw string) []string {
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
