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
	// Skip declares that no test exists to write for Field. It accepts
	// either the legacy free-prose string form or the structured mapping
	// form — see SkipInfo for both shapes and the closed reason set the
	// structured form is restricted to.
	Skip SkipInfo `yaml:"skip"`
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
	// KnownDefect names the ticket ID tracking a real provider defect that
	// makes this field's update path fail. Unlike Skip, a KnownDefect entry
	// IS expressible and IS run: it exists for the field between "the test
	// passes" and "no test exists to write" — an entry that runs, is
	// expected NOT to converge, and says exactly why.
	//
	// It requires Value (a KnownDefect entry with no value is a parse
	// error — see ParseAnnotation), because the whole point is to exercise
	// the real patch path rather than describe one that was never tried.
	// It is mutually exclusive with Skip — the two states already cover
	// "converges" and "no test exists"; this is a distinct third state,
	// not a variant of either.
	//
	// The runner applies the patch exactly as it would for an ordinary
	// entry, but inverts the verdict: non-convergence is the EXPECTED
	// outcome here and does not fail the run, while a field that DOES
	// converge fails the run hard, naming this ticket ID and instructing
	// the reader to delete the token and restore a plain value:/expect:
	// entry. That inversion is what makes the suppression self-retiring —
	// once the underlying defect is fixed, the annotation itself breaks
	// the next run until someone removes it, rather than rotting silently
	// the way a stale Skip reason does.
	KnownDefect string `yaml:"knownDefect"`
	// IgnoreMapKeys names top-level member keys to exclude, on BOTH sides,
	// from the equality check `run` performs between Expect (or Value, when
	// Expect is unset) and the live status.atProvider value — see
	// runner.compareFieldValue for the comparison itself.
	//
	// It exists for a map-typed field whose live value carries a member the
	// PROVIDER itself writes — an identity stamp, a server-managed marker —
	// alongside the keys the manifest actually manages. Without it, an
	// Expect for such a field would have to predict that provider-written
	// value verbatim, and a value derived from something that does not
	// exist until the resource is created (e.g. metadata.uid) can never
	// appear in a static example manifest. Naming the member here lets
	// Expect describe only the keys the test actually manages — add,
	// update, and null-tombstone removal are all still expressed exactly
	// as before; only the comparison ignores the named keys.
	//
	// Requires Expect or Value to resolve to a JSON object; see
	// runner.compareFieldValue for what happens when it does not.
	IgnoreMapKeys []string `yaml:"ignoreMapKeys"`
}

// SkipReason is the closed set of reasons a structured "skip:" entry may
// declare for why no update-test exists to write for a field. Unlike the
// legacy free-prose string form (see SkipInfo.Legacy), a reason code names
// one of a small number of shapes this package — and, for the two reasons
// that need more than the entry's own text, the validator package — can
// check offline.
type SkipReason string

// The closed set of structured skip reasons. An unrecognised value is a
// parse-time error naming this set — see validateSkipInfo.
const (
	// SkipUnionArm marks a field as one alternative of a union modelled as
	// separate top-level *Parameters fields, where Sibling names another
	// arm of the same union. Resolved offline against the target type's
	// own declared fields — see validator.CheckSkipReasons.
	SkipUnionArm SkipReason = "union-arm"
	// SkipCoveredElsewhere marks a field whose value is already exercised
	// by another manifest's own update-test entry, named in By as
	// "<path>#<field>". Resolved offline by loading that manifest and
	// confirming the named entry is itself directly tested — see
	// validator.CheckSkipReasons.
	SkipCoveredElsewhere SkipReason = "covered-elsewhere"
	// SkipVendorDefect marks a field whose update path cannot be tested
	// because of an observed vendor/backend defect, recorded in Evidence
	// and tracked in Ticket. Not resolvable offline — Evidence and Ticket
	// are checked for presence only.
	SkipVendorDefect SkipReason = "vendor-defect"
	// SkipFixtureMissing marks a field that cannot be tested because the
	// fixture data it needs does not exist yet, tracked in Ticket. Not
	// resolvable offline — Ticket is checked for presence only.
	SkipFixtureMissing SkipReason = "fixture-missing"
	// SkipWriteOnly marks a field with no readable counterpart to assert
	// against. Accepted here without further resolution: the tool's own
	// full-mirror convention for atProvider means an atProvider
	// counterpart exists in the generated type by construction, so
	// telling a genuinely write-only field apart from one whose
	// counterpart was simply never named requires comparing against a
	// live roundtrip row, not the schema alone. UTV-TOOL-DENOM resolves
	// this reason against that row; until it lands, a write-only entry
	// here is accepted on the strength of its author's claim, same as
	// vendor-defect and fixture-missing.
	SkipWriteOnly SkipReason = "write-only"
)

// skipReasons is the closed set of valid SkipReason values, in the order
// they are listed in a parse-time "not a valid reason" error.
var skipReasons = []SkipReason{
	SkipUnionArm, SkipCoveredElsewhere, SkipVendorDefect, SkipFixtureMissing, SkipWriteOnly,
}

// validSkipReasonList renders skipReasons as a comma-separated string for a
// parse-time error message.
func validSkipReasonList() string {
	out := make([]string, len(skipReasons))
	for i, r := range skipReasons {
		out[i] = string(r)
	}
	return strings.Join(out, ", ")
}

// SkipInfo is a field's "skip:" declaration — the zero value means no
// skip: key was present at all (see Present).
//
// Two shapes parse into it. The legacy shape is a bare string — a free-prose
// reason with nothing to check — and is recorded as Legacy/LegacyText,
// credited under a status distinct from the structured form's (see
// validator's "covered (skipped, unstructured)" status) so the fleet's
// existing free-prose entries keep working while carrying their own
// burn-down count. The structured shape is a mapping with a Reason from the
// closed SkipReason set above, plus whatever companion keys that reason
// requires — see UnmarshalYAML and validateSkipInfo for the accepted shapes
// and their required keys.
type SkipInfo struct {
	// Reason is the structured reason code. Empty when Legacy is true.
	Reason SkipReason
	// Sibling names another field in the same Parameters struct that this
	// field is a union alternative to. Set only when Reason is
	// SkipUnionArm.
	Sibling string
	// By names the manifest and field that already exercises this
	// field's value, shaped "<path>#<field>". Set only when Reason is
	// SkipCoveredElsewhere.
	By string
	// Evidence records what was observed that makes this field's update
	// path untestable. Set only when Reason is SkipVendorDefect.
	Evidence string
	// Ticket names the ticket tracking why this field cannot be tested
	// yet. Set when Reason is SkipVendorDefect or SkipFixtureMissing.
	Ticket string
	// Legacy is true when this value was parsed from a bare free-prose
	// string rather than a structured mapping.
	Legacy bool
	// LegacyText is the original free-prose string, set only when Legacy
	// is true.
	LegacyText string
}

// LegacySkip builds the pre-migration free-prose form of a SkipInfo. YAML
// unmarshalling reaches the same shape on its own (see
// SkipInfo.UnmarshalYAML) for a manifest's own "skip:" string; this
// constructor exists for Go code building an UpdateTest literal directly —
// tests, mainly — without going through the YAML annotation parser.
func LegacySkip(reason string) SkipInfo {
	return SkipInfo{Legacy: true, LegacyText: reason}
}

// Present reports whether a "skip:" key was declared at all for this entry.
// The zero value (no skip: key present in the source at all) reports false.
func (s SkipInfo) Present() bool {
	return s.Legacy || s.Reason != ""
}

// Describe renders a short, human-readable string for display in `run`
// output (see runner.TestResult.SkipMsg): the legacy free-prose text
// verbatim, or a rendering of the structured reason and whatever companion
// fields it carries.
func (s SkipInfo) Describe() string {
	switch {
	case s.Legacy:
		return s.LegacyText
	case s.Reason == SkipUnionArm:
		return fmt.Sprintf("union-arm (sibling: %s)", s.Sibling)
	case s.Reason == SkipCoveredElsewhere:
		return fmt.Sprintf("covered-elsewhere (by: %s)", s.By)
	case s.Reason == SkipVendorDefect:
		return fmt.Sprintf("vendor-defect (%s; ticket: %s)", s.Evidence, s.Ticket)
	case s.Reason == SkipFixtureMissing:
		return fmt.Sprintf("fixture-missing (ticket: %s)", s.Ticket)
	case s.Reason == SkipWriteOnly:
		return "write-only"
	default:
		return string(s.Reason)
	}
}

// UnmarshalYAML implements yaml.Unmarshaler so a "skip:" key accepts either
// its legacy free-prose string form or the structured mapping form — see
// SkipInfo. An explicit `skip: ""` (or an entry with no skip: key at all)
// decodes to the zero value, matching the pre-this-ticket behaviour where an
// empty string meant "no skip declared".
func (s *SkipInfo) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := value.Decode(&raw); err != nil {
			return fmt.Errorf("decoding skip: %w", err)
		}
		if raw == "" {
			*s = SkipInfo{}
			return nil
		}
		*s = LegacySkip(raw)
		return nil
	case yaml.MappingNode:
		var m struct {
			Reason   string `yaml:"reason"`
			Sibling  string `yaml:"sibling"`
			By       string `yaml:"by"`
			Evidence string `yaml:"evidence"`
			Ticket   string `yaml:"ticket"`
		}
		if err := value.Decode(&m); err != nil {
			return fmt.Errorf("decoding skip: %w", err)
		}
		info := SkipInfo{
			Reason:   SkipReason(m.Reason),
			Sibling:  m.Sibling,
			By:       m.By,
			Evidence: m.Evidence,
			Ticket:   m.Ticket,
		}
		if err := validateSkipInfo(info); err != nil {
			return err
		}
		*s = info
		return nil
	default:
		return fmt.Errorf("skip: must be a string or a mapping, got YAML node kind %d", value.Kind)
	}
}

// validateSkipInfo enforces the closed reason set and each reason's own
// required companion keys, at parse time, before any cluster or generated
// type is touched.
//
// It deliberately stops short of the two checks that need context this
// package does not have: SkipUnionArm's sibling being a field actually
// declared on the target Parameters struct, and SkipCoveredElsewhere's by:
// target being itself directly tested. Both run in
// validator.CheckSkipReasons, which has the generated-type field list (for
// the first) and can load another manifest file (for the second).
func validateSkipInfo(s SkipInfo) error {
	if s.Reason == "immutable" {
		return fmt.Errorf(
			"skip: reason \"immutable\" is not a valid reason — immutability is derived mechanically from the " +
				"`self == oldSelf` CEL validation marker on the field's own declaration; add that marker instead " +
				"of declaring skip: here")
	}
	valid := false
	for _, r := range skipReasons {
		if s.Reason == r {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("skip: reason %q is not one of the valid reasons (%s)", s.Reason, validSkipReasonList())
	}

	switch s.Reason {
	case SkipUnionArm:
		if s.Sibling == "" {
			return fmt.Errorf("skip: reason %q requires a non-empty sibling: naming the other union-arm field", s.Reason)
		}
	case SkipCoveredElsewhere:
		if s.By == "" {
			return fmt.Errorf("skip: reason %q requires a non-empty by: shaped \"<path>#<field>\"", s.Reason)
		}
		if !strings.Contains(s.By, "#") {
			return fmt.Errorf("skip: reason %q by: %q must be shaped \"<path>#<field>\"", s.Reason, s.By)
		}
	case SkipVendorDefect:
		if s.Evidence == "" || s.Ticket == "" {
			return fmt.Errorf("skip: reason %q requires both evidence: and ticket: to be non-empty", s.Reason)
		}
	case SkipFixtureMissing:
		if s.Ticket == "" {
			return fmt.Errorf("skip: reason %q requires a non-empty ticket: field", s.Reason)
		}
	case SkipWriteOnly:
		// No required companion keys — see the SkipWriteOnly doc comment
		// for why resolution is deferred rather than performed here.
	}
	return nil
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
		if t.KnownDefect != "" && t.Skip.Present() {
			return nil, "", nil, nil, fmt.Errorf(
				"entry %d (%s): knownDefect and skip are mutually exclusive — skip asserts no test exists to write, "+
					"knownDefect asserts an expressible test fails; an entry cannot be both", i, t.Field)
		}
		if t.Value == nil && !t.Skip.Present() {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): value is required unless skip is set", i, t.Field)
		}
		if t.KnownDefect != "" {
			if err := ValidateKnownDefect(t.KnownDefect); err != nil {
				return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
			}
		}
		if err := ValidateClear(t.Field, t.Clear); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		if err := ValidateIgnoreMapKeys(t); err != nil {
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
	if err := ValidateKnownDefectIgnoreFields(tests, ignoreFields); err != nil {
		return nil, "", nil, nil, err
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

// ValidateIgnoreMapKeys rejects an ignoreMapKeys declaration the runner
// could not act on, at parse time, before any cluster is touched. See
// UpdateTest.IgnoreMapKeys for what the mechanism is for.
//
// It is exported so both sources of an ignoreMapKeys list share this one
// check — the "ignoreMapKeys:" key on an update-test annotation entry (via
// ParseAnnotation above) and any offline validator that wants to check a
// manifest without running it.
func ValidateIgnoreMapKeys(t UpdateTest) error {
	if len(t.IgnoreMapKeys) == 0 {
		return nil
	}
	if t.Skip.Present() {
		return fmt.Errorf(
			"ignoreMapKeys is set but skip is also set — skip asserts no test exists to write, " +
				"so there is no comparison left for ignoreMapKeys to affect; remove one directive")
	}
	seen := make(map[string]bool, len(t.IgnoreMapKeys))
	for _, k := range t.IgnoreMapKeys {
		if k == "" {
			return fmt.Errorf("ignoreMapKeys entry is empty — name the map member key to exclude from comparison")
		}
		if seen[k] {
			return fmt.Errorf("ignoreMapKeys entry %q is repeated", k)
		}
		seen[k] = true
	}
	return nil
}

// knownDefectPlaceholders lists knownDefect values that LOOK non-empty but
// name nothing followable — the exact failure mode this token exists to
// close off (see UpdateTest.KnownDefect): a suppression that survives
// review because it passes every gate a real ticket ID would, while
// pointing nowhere a later reader can search.
var knownDefectPlaceholders = map[string]bool{
	"todo": true, "tbd": true, "fixme": true, "xxx": true,
	"n/a": true, "na": true, "unknown": true, "none": true, "?": true,
}

// minKnownDefectLen is the shortest string ValidateKnownDefect accepts as
// ticket-ID-shaped. It is deliberately loose — pheromone ticket IDs are
// either a UUID or a short custom slug of the filer's choosing, and this
// package has no way to confirm a ticket with that ID actually exists — so
// the bar is simply "long enough that it cannot be a stray character and
// contains no whitespace", not "matches one specific ID format".
const minKnownDefectLen = 6

// ValidateKnownDefect rejects a knownDefect value that cannot be a real
// ticket ID, at parse time, before any cluster is touched: empty, containing
// whitespace (prose describing the defect rather than citing where it is
// tracked), a known placeholder, or too short to plausibly be an ID.
//
// This is what keeps the suppression followable by search instead of by
// reading a comment: a knownDefect entry that passed this check can always
// be traced back to the ticket that explains why the field does not
// converge yet.
func ValidateKnownDefect(ticketID string) error {
	trimmed := strings.TrimSpace(ticketID)
	if trimmed == "" {
		return fmt.Errorf("knownDefect requires a ticket ID; got an empty value")
	}
	if trimmed != ticketID || strings.ContainsAny(trimmed, " \t\n") {
		return fmt.Errorf(
			"knownDefect value %q looks like a prose description, not a ticket ID — "+
				"a ticket ID carries no whitespace; put the reason in a code comment and cite the ticket ID here", ticketID)
	}
	if knownDefectPlaceholders[strings.ToLower(trimmed)] {
		return fmt.Errorf("knownDefect value %q is a placeholder, not a real ticket ID", ticketID)
	}
	if len(trimmed) < minKnownDefectLen {
		return fmt.Errorf(
			"knownDefect value %q is too short to be a ticket ID (need at least %d characters)", ticketID, minKnownDefectLen)
	}
	return nil
}

// ValidateKnownDefectIgnoreFields rejects a knownDefect entry whose field
// also appears (by its top-level name) in the manifest's own ignore-fields
// set, at parse time.
//
// ignore-fields excludes a status.atProvider field from a convergence
// check's snapshot diff entirely — declaring it there says "this field is
// expected to differ and that difference means nothing". knownDefect on the
// same field name says "this field's update path is broken and every run
// proves it stays broken". Both declared on the same manifest for the same
// field is dead config: whichever the author actually intended, the other
// directive silently defeats it — either the convergence diff never looks
// at the field the knownDefect entry is trying to characterise, or the
// knownDefect entry runs unaware that something else has already decided
// this field's drift means nothing. Neither combination is something an
// author would choose on purpose, so it is rejected here rather than left
// to silently pick one behaviour over the other.
func ValidateKnownDefectIgnoreFields(tests []UpdateTest, ignoreFields []string) error {
	ignored := make(map[string]bool, len(ignoreFields))
	for _, f := range ignoreFields {
		ignored[f] = true
	}
	for _, t := range tests {
		if t.KnownDefect == "" {
			continue
		}
		top := t.Field
		if idx := strings.Index(top, "."); idx != -1 {
			top = top[:idx]
		}
		if ignored[top] {
			return fmt.Errorf(
				"field %q is both a knownDefect entry (%s) and named in ignore-fields — this is dead config: "+
					"ignore-fields excludes the field from convergence checking entirely, so the knownDefect "+
					"entry's non-convergence proof is never meaningfully checked against it; remove one directive",
				t.Field, t.KnownDefect)
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
