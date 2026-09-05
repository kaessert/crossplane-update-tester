// Package manifest parses Crossplane example manifests and the update-test
// annotations that drive the tester's checks.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kaessert/crossplane-update-tester/sidecar"
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

// ReadyConditionsKey names the SAME annotation uptest itself reads to learn
// which status condition(s) mean a resource is ready — it is uptest's
// annotation, not one this project defines, and this constant exists only so
// a converge check can honour the SAME declaration uptest's own per-resource
// assertion already honours, rather than inventing a second one. Comma
// separated, e.g. "Synced" or "Synced,Ready" — see EffectiveReadyConditions
// for how an absent or blank value defaults.
const ReadyConditionsKey = "uptest.upbound.io/conditions"

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
	// WithValues names OTHER top-level spec.forProvider fields that must
	// carry an explicit, non-null literal value in the SAME merge patch
	// that sets Field's own Value.
	//
	// It exists for two fields that are coupled on the BACKEND rather than
	// merely in the CRD schema: a derived field cannot be set to a target
	// value unless the field it derives from is ALSO given a real value in
	// the same patch, because the backend re-derives the former from
	// whatever the latter currently holds on every write that does not
	// also carry an explicit value for it. Clear cannot express this: it
	// only ever writes a literal JSON null for a sibling, and a null on an
	// optional field is dropped from the outgoing request by omitempty
	// rather than read as "set this to its zero value" — exactly the "no
	// change" outcome a genuine backend-level clear must avoid. WithValues
	// is the additive route for a sibling that needs a real, non-null
	// value (including the empty string or an empty list) instead.
	//
	// A field named in WithValues must not also appear in Clear — the two
	// disagree about what the same sibling should end up holding in the
	// same patch, so combining them is rejected at parse time rather than
	// left to whichever key a map happens to iterate last — see
	// ValidateWithValues and runner.buildMergePatch for how this map is
	// consumed.
	WithValues map[string]interface{} `yaml:"withValues"`
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
	// IgnoreListElementKeys names per-element member keys to exclude, on
	// BOTH sides and from EVERY element, from the equality check `run`
	// performs between Expect (or Value, when Expect is unset) and the
	// live status.atProvider value — see runner.compareFieldValue for the
	// comparison itself.
	//
	// It exists for the list-shaped counterpart of IgnoreMapKeys: a
	// list-of-objects field whose live elements each carry a member the
	// PROVIDER itself assigns per element — a server-generated per-rule
	// ID, for example — alongside the keys the manifest actually manages.
	// IgnoreMapKeys cannot reach this: it strips a key from the top level
	// of a map-shaped comparison, but a list's own elements are one level
	// beneath that, so a provider-assigned per-element key would still
	// force Expect to predict a value that does not exist until the
	// element is created on the backend, and can therefore never appear
	// in a static example manifest. Naming the member here lets Expect
	// describe only the keys the test actually manages for each element;
	// only the comparison ignores the named per-element keys.
	//
	// Requires Expect or Value to resolve to a JSON array of objects; see
	// runner.compareFieldValue for what happens when it does not.
	IgnoreListElementKeys []string `yaml:"ignoreListElementKeys"`
	// ValueExplicit reports whether the "value:" key was present in the
	// entry's own YAML mapping at all, regardless of what it decoded to.
	//
	// It exists to break a genuine ambiguity: `value: null`, a bare
	// `value:` with nothing after the colon, and the "value:" key being
	// entirely ABSENT from the mapping all decode Value to the identical
	// Go nil — yaml.v3 gives an interface{} target no way to tell "the key
	// was there and explicitly null" from "the key was never written" once
	// decoding has happened. That distinction matters here specifically:
	// an explicit `value: null` is a legitimate whole-field-tombstone
	// entry (see ValidateClear and roundtrip.ContainerClearCoverage's
	// self-tombstone case for the container leaf this unlocks — a
	// top-level container field with no sibling to host a clear: list),
	// while a genuinely omitted value: is simply an incomplete entry with
	// nothing to test. Populated by UnmarshalYAML; not itself part of the
	// YAML schema (no yaml tag) and not settable by an author.
	ValueExplicit bool `yaml:"-"`
}

// UnmarshalYAML implements yaml.Unmarshaler for UpdateTest so
// ValueExplicit can be derived from the raw mapping node — see its doc
// comment for why that requires looking at the node directly rather than
// at the decoded Value.
func (t *UpdateTest) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("update-test entry must be a mapping, got YAML node kind %d", node.Kind)
	}
	// A "knownDefect:" key is a parse-time error, not a silently ignored
	// unknown key: the mechanism it named has been removed, and letting an
	// existing manifest keep parsing while its entry quietly stopped
	// enforcing anything it used to would trade a loud stopgap for an
	// invisible one. Record what was observed in skip: (reason
	// vendor-defect or fixture-missing, with evidence: describing the
	// observation) if the field genuinely cannot be tested, or withValues:
	// if it can be tested once a coupled sibling field also carries a
	// literal value.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "knownDefect" {
			return fmt.Errorf(
				"update-test entry carries a \"knownDefect:\" key, which no longer exists — use skip: with reason "+
					"vendor-defect or fixture-missing, recording what was observed in evidence:, or withValues: if "+
					"the test can be expressed once a coupled sibling field also carries a literal value in the "+
					"same patch; valid skip: reasons are %s",
				validSkipReasonList())
		}
	}
	// A distinct named type, not UpdateTest itself: decoding into
	// UpdateTest directly here would recurse into this same method
	// forever, since yaml.v3 uses the Unmarshaler interface at every
	// level it finds it implemented.
	type rawUpdateTest UpdateTest
	var raw rawUpdateTest
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*t = UpdateTest(raw)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "value" {
			t.ValueExplicit = true
			break
		}
	}
	return nil
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
	// because of an observed vendor/backend defect, recorded in Evidence.
	// Not resolvable offline — Evidence is checked for presence.
	SkipVendorDefect SkipReason = "vendor-defect"
	// SkipFixtureMissing marks a field that cannot be tested because the
	// fixture data it needs does not exist yet, recorded in Evidence.
	// Not resolvable offline — Evidence is checked for presence.
	SkipFixtureMissing SkipReason = "fixture-missing"
	// SkipWriteOnly marks a field with no readable counterpart to assert
	// against. Accepted here with no companion key, unlike the other four
	// structured reasons: the tool's own full-mirror convention for
	// atProvider means an atProvider counterpart exists in the generated
	// type by construction, so telling a genuinely write-only field apart
	// from one whose counterpart was simply never named cannot be done
	// from the schema alone — it requires comparing against a live
	// roundtrip row. That resolution happens at run time, in
	// roundtrip.DenominatorReport, against the field's own row: a
	// present-in-spec-absent-from-mirror row confirms the claim, any
	// other row (or no row at all) rejects it. Parse-time validation here
	// only checks that the reason is well-formed; it is not the
	// resolution.
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

// Disposition is the closed set of evidence-tier dispositions a structured
// "skip:" entry may declare alongside its Reason, via the optional
// "disposition:" key — see SkipInfo. It answers a different question than
// Reason does: Reason names WHY no test exists for the field; Disposition
// names HOW that reason could be — or already has been — checked, so a
// tool can tell a claim that is re-checkable by machine from one that is
// only a human's word.
//
// The four values are the evidence tiers a fleet-wide reading of every
// "skip:" waiver's own claim shape converged on. Deliberately closed, like
// SkipReason: an open-ended free-text disposition could not be told apart
// from a guess, which is exactly the failure this axis exists to end.
type Disposition string

const (
	// DispositionStaticallyProvable marks a reason decidable from the repo
	// alone — a CRD schema validation rule on the field's own declaration, a
	// structural fact of the resource's own controller source (a field a
	// request or response type never carries), or the example manifest's
	// own data — with no cluster and no live call, re-checkable by a tool
	// on every run for as long as the cited artifact exists.
	DispositionStaticallyProvable Disposition = "statically-provable"
	// DispositionOneLivePatch marks a reason that is a claim about backend
	// runtime behaviour — a status code, or whether a value takes live
	// effect — not decidable from the repo alone, but resolvable by firing
	// ONE request that has no lasting consequence: a rejection leaves state
	// unchanged, and an acceptance is undoable by a further,
	// similarly-priced request. This includes a claim whose evidence was
	// already gathered and is recorded in the reason's own prose — the
	// disposition records the CLASS of evidence the claim rests on, not
	// whether it happens to be already paid for.
	DispositionOneLivePatch Disposition = "one-live-patch"
	// DispositionDeclaredExclusion marks a reason where firing the
	// one-live-patch-shaped probe is ITSELF the irreversible or destructive
	// act, or damages state shared with other runs, so no mechanical check
	// can ever confirm it. It is the only disposition that encodes a
	// standing human commitment rather than a re-checkable claim, and it
	// requires DeclaredBy and Reconfirm for exactly that reason — see
	// validateSkipInfo.
	DispositionDeclaredExclusion Disposition = "declared-exclusion"
	// DispositionDefect marks a reason the repo's own artifacts contradict,
	// or that names nothing checkable at all. Recording it is a defect
	// finding, not a legitimate skip.
	DispositionDefect Disposition = "defect"
)

// dispositions is the closed set of valid Disposition values, in the order
// they are listed in a parse-time "not a valid disposition" error.
var dispositions = []Disposition{
	DispositionStaticallyProvable, DispositionOneLivePatch, DispositionDeclaredExclusion, DispositionDefect,
}

// validDispositionList renders dispositions as a comma-separated string for
// a parse-time error message.
func validDispositionList() string {
	out := make([]string, len(dispositions))
	for i, d := range dispositions {
		out[i] = string(d)
	}
	return strings.Join(out, ", ")
}

// SkipInfo is a field's "skip:" declaration — the zero value means no
// skip: key was present at all (see Present).
//
// Three shapes parse into it. The legacy shape is a bare string — a
// free-prose reason with nothing to check — and is recorded as
// Legacy/LegacyText, credited under a status distinct from the structured
// form's (see validator's "covered (skipped, unstructured)" status) so the
// fleet's existing free-prose entries keep working while carrying their own
// burn-down count. The structured shape is a mapping with a Reason from the
// closed SkipReason set above, plus whatever companion keys that reason
// requires. The third shape is a mapping carrying a legacy: key instead of
// reason: — the same free prose as the bare-string shape, decoded to the
// same Legacy/LegacyText fields, but able to additionally carry
// disposition: (and, when disposition: is declared-exclusion,
// declared-by:/reconfirm:) alongside it:
//
//	skip:
//	  legacy: "the original free-prose text, verbatim"
//	  disposition: statically-provable
//
// legacy: and reason: are alternatives, never both on the same entry — see
// UnmarshalYAML and validateSkipInfo for the accepted shapes and their
// required keys.
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
	// path untestable. Required whenever Reason is SkipVendorDefect or
	// SkipFixtureMissing.
	Evidence string
	// Disposition is the optional evidence-tier disposition declared
	// alongside Reason — see Disposition. Empty when no disposition: key
	// was authored; an absent disposition is reported as absent, never
	// guessed from Reason or Evidence — see ContainerClearFinding.
	Disposition Disposition
	// DeclaredBy names the human who made the standing declaration that
	// this entry's reason cannot be mechanically re-checked. Required, and
	// meaningful, only when Disposition is DispositionDeclaredExclusion.
	DeclaredBy string
	// Reconfirm is a re-confirmation cadence for a
	// DispositionDeclaredExclusion entry — free-form text (a date, or an
	// interval) naming when the declaration must be revisited. This
	// package does not parse or enforce it as a schedule, only requires it
	// be non-empty when Disposition is DispositionDeclaredExclusion.
	Reconfirm string
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
	var base string
	switch {
	case s.Legacy:
		base = s.LegacyText
	case s.Reason == SkipUnionArm:
		base = fmt.Sprintf("union-arm (sibling: %s)", s.Sibling)
	case s.Reason == SkipCoveredElsewhere:
		base = fmt.Sprintf("covered-elsewhere (by: %s)", s.By)
	case s.Reason == SkipVendorDefect:
		base = describeEvidence("vendor-defect", s.Evidence)
	case s.Reason == SkipFixtureMissing:
		base = describeEvidence("fixture-missing", s.Evidence)
	case s.Reason == SkipWriteOnly:
		base = "write-only"
	default:
		base = string(s.Reason)
	}
	// Disposition is optional and orthogonal to Reason — appended only when
	// authored, so every pre-existing Describe() rendering (no disposition:
	// key in the source) is byte-for-byte unchanged.
	if s.Disposition == "" {
		return base
	}
	return fmt.Sprintf("%s [disposition: %s]", base, s.Disposition)
}

// describeEvidence renders the common "<reason> (<evidence>)" shape
// SkipVendorDefect and SkipFixtureMissing share.
func describeEvidence(reason, evidence string) string {
	return fmt.Sprintf("%s (%s)", reason, evidence)
}

// UnmarshalYAML implements yaml.Unmarshaler so a "skip:" key accepts any of
// three shapes — see SkipInfo: the legacy free-prose string, the structured
// mapping keyed on reason:, or the structured mapping keyed on legacy:
// (the same free prose, but able to carry disposition: alongside it). An
// explicit `skip: ""` (or an entry with no skip: key at all) decodes to the
// zero value, matching the pre-this-ticket behaviour where an empty string
// meant "no skip declared".
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
		// A "ticket:" key is a parse-time error, not a silently ignored
		// unknown key: the key was removed because it had zero users
		// fleet-wide and its only guard was a shape heuristic that could
		// not actually confirm a value was externally resolvable. Letting
		// an authored "ticket:" decode and vanish would trade a loud
		// rejection for a silent no-op — the same failure class this
		// removal exists to end.
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "ticket" {
				return fmt.Errorf(
					"skip: mapping carries a \"ticket:\" key, which no longer exists — record what was " +
						"observed in evidence: instead")
			}
		}
		var m struct {
			Reason      string `yaml:"reason"`
			Legacy      string `yaml:"legacy"`
			Sibling     string `yaml:"sibling"`
			By          string `yaml:"by"`
			Evidence    string `yaml:"evidence"`
			Disposition string `yaml:"disposition"`
			DeclaredBy  string `yaml:"declared-by"`
			Reconfirm   string `yaml:"reconfirm"`
		}
		if err := value.Decode(&m); err != nil {
			return fmt.Errorf("decoding skip: %w", err)
		}
		info := SkipInfo{
			Reason:      SkipReason(m.Reason),
			Sibling:     m.Sibling,
			By:          m.By,
			Evidence:    m.Evidence,
			Disposition: Disposition(m.Disposition),
			DeclaredBy:  m.DeclaredBy,
			Reconfirm:   m.Reconfirm,
		}
		if m.Legacy != "" {
			info.Legacy = true
			info.LegacyText = m.Legacy
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

// validateDisposition enforces the closed disposition set and
// declared-exclusion's own required companion keys. disposition: is
// optional and orthogonal to reason:/legacy: — this runs for both the
// structured-reason shape and the legacy-mapping shape. Nothing here ever
// derives a disposition from s.Reason, s.LegacyText or s.Evidence: it is
// authored, or it stays empty (see Disposition's own doc comment).
func validateDisposition(s SkipInfo) error {
	if s.Disposition == "" {
		return nil
	}
	validDisposition := false
	for _, d := range dispositions {
		if s.Disposition == d {
			validDisposition = true
			break
		}
	}
	if !validDisposition {
		return fmt.Errorf("skip: disposition %q is not one of the valid dispositions (%s)", s.Disposition, validDispositionList())
	}
	if s.Disposition == DispositionDeclaredExclusion {
		if s.DeclaredBy == "" || s.Reconfirm == "" {
			return fmt.Errorf("skip: disposition %q requires both declared-by: and reconfirm: to be non-empty", s.Disposition)
		}
	}
	return nil
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
	// legacy: and reason: are alternatives, never a merge — a mapping
	// authoring both leaves last-one-wins ambiguity for every downstream
	// reader (burn-down counts, Describe()) with no signal that it happened.
	if s.Legacy && s.Reason != "" {
		return fmt.Errorf(
			"skip: mapping carries both legacy: and reason: — they are alternatives, not a merge; " +
				"legacy: carries pre-migration free prose forward unchanged, reason: declares a structured " +
				"closed-set reason, and a mapping may author only one of the two")
	}

	// The legacy: shape carries no reason code at all, so none of the
	// reason-set/companion-key checks below apply to it — only the
	// reason-independent disposition: check further down does.
	if !s.Legacy {
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
		case SkipVendorDefect, SkipFixtureMissing:
			if s.Evidence == "" {
				return fmt.Errorf("skip: reason %q requires a non-empty evidence: to be recorded", s.Reason)
			}
		case SkipWriteOnly:
			// No required companion keys — see the SkipWriteOnly doc comment
			// for why resolution is deferred rather than performed here.
		}
	}

	return validateDisposition(s)
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
	// Path is the absolute filesystem path this manifest was parsed from,
	// set by Parse. A manifest built directly (ParseBytes, or a literal
	// &Manifest{} in a test) carries an empty Path — every resolution that
	// consumes Path treats empty the same as "this process's own working
	// directory", which is exactly the old, pre-manifest-relative
	// behaviour.
	//
	// It is stored absolute rather than as the raw CLI argument so that a
	// covered-elsewhere chain (see validator.resolveCoveredElsewhere)
	// resolves identically regardless of which working directory the
	// process was started from, or how many hops deep the chain runs — a
	// relative Path would re-anchor at each hop to whatever the process
	// CWD happened to be, not to the referring manifest's own directory.
	Path string
	// ReadyConditions lists the status condition TYPES that must all read
	// "True" for this resource to be considered ready, parsed from the
	// ReadyConditionsKey annotation (comma separated). Empty means the
	// manifest declares no override — see EffectiveReadyConditions for the
	// default this falls back to.
	//
	// This exists for a resource whose sanctioned steady state is
	// permanently NOT Ready by design (e.g. one that intentionally holds a
	// non-functional credential because a working one cannot be
	// committed): a converge check's readiness-flap comparison must judge
	// such a resource against the SAME condition uptest's own per-resource
	// assertion already honours, or it fails a resource for exhibiting its
	// documented, intended state.
	ReadyConditions []string
}

// defaultReadyCondition is the condition type every converge check honours
// when a manifest declares no ReadyConditionsKey override — the behaviour
// every manifest had before that annotation was read here at all.
const defaultReadyCondition = "Ready"

// EffectiveReadyConditions returns the condition types that determine this
// manifest's readiness for a converge check: its own declared
// ReadyConditions when present, else the single default "Ready" condition.
// A caller never needs to special-case the empty declaration itself.
func (m *Manifest) EffectiveReadyConditions() []string {
	if len(m.ReadyConditions) > 0 {
		return m.ReadyConditions
	}
	return []string{defaultReadyCondition}
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

// Parse reads a YAML manifest file, merges its sidecar (if one exists at
// sidecar.PathFor(path)) onto the decoded documents, and extracts metadata
// and update test annotations.
//
// The sidecar merge happens BEFORE document selection, deliberately: on a
// migrated manifest, AnnotationKey no longer lives in the manifest text at
// all — it lives in the sidecar — and selection picks the document
// carrying it. Selecting first and merging after would silently select
// whichever document the manifest's own (companion) annotations happen to
// carry, which is frequently not the managed resource under test.
func Parse(path string) (*Manifest, error) {
	// #nosec G304 -- path is an operator-supplied CLI argument (the
	// manifest file to test), not attacker-controlled input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	sc, err := sidecar.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading sidecar for %s: %w", path, err)
	}
	m, err := parseDocs(data, sc, path)
	if err != nil {
		return nil, err
	}
	// Absolute so every later resolution anchored on m.Path (directly, or
	// via filepath.Dir) is immune to a working-directory change that
	// happens after this call returns — see the Path field's own doc
	// comment for why that matters for a covered-elsewhere chain.
	if abs, absErr := filepath.Abs(path); absErr == nil {
		m.Path = abs
	} else {
		m.Path = path
	}
	return m, nil
}

// ParseBytes parses manifest YAML bytes with no sidecar. A caller handing
// bytes directly rather than a filesystem path (every existing caller, and
// every test) has no sidecar to look up — this keeps that caller's
// behaviour exactly as it was before sidecar support existed.
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
	return parseDocs(data, nil, "")
}

// ownedSidecarKeys are the manifest annotation keys THIS tool reads. A
// sidecar may declare other tools' keys too (uptest's own annotations,
// e.g. uptest.upbound.io/timeout) — mergeSidecarDocs copies those onto the
// selected document's annotations exactly the same way, but only these
// three participate in the switch-not-overlay hard error below, because
// this tool has no business rejecting a manifest over a key it never
// reads — that is uptest's own annotation set to police.
var ownedSidecarKeys = []string{AnnotationKey, ExpectExternalNamePrefixKey, ReadyConditionsKey}

// parseDocs is the shared implementation behind Parse and ParseBytes:
// decode every document, merge sc onto them (a nil sc is a no-op — see
// sidecar.Resolve), select one, and convert it.
func parseDocs(data []byte, sc *sidecar.File, path string) (*Manifest, error) {
	docs, err := decodeManifestDocs(data)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("manifest missing apiVersion or kind")
	}

	if err := mergeSidecarDocs(docs, sc, path); err != nil {
		return nil, err
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

// mergeSidecarDocs resolves sc against docs (in place) and merges each
// resolved annotation set onto its target document — before the caller
// selects one for AnnotationKey, so a sidecar-declared update-test
// annotation is visible to that selection exactly as an inline one always
// was. A nil sc is a no-op: the un-migrated state every manifest starts in.
//
// A sidecar REPLACES the manifest's harness annotations rather than
// overlaying them: if sc is non-nil, no document in data may carry any of
// ownedSidecarKeys inline — a key live in both files has no defensible
// precedence, so that combination is a hard error rather than a silent
// pick of one side.
func mergeSidecarDocs(docs []manifestDoc, sc *sidecar.File, path string) error {
	if sc == nil {
		return nil
	}

	targets := make([]sidecar.ObjectID, len(docs))
	for i, d := range docs {
		targets[i] = sidecar.ObjectID{
			APIVersion: d.APIVersion,
			Kind:       d.Kind,
			Namespace:  d.Metadata.Namespace,
			Name:       d.Metadata.Name,
		}
	}
	resolved, err := sidecar.Resolve(sc, targets)
	if err != nil {
		return fmt.Errorf("resolving sidecar for %s: %w", path, err)
	}

	for i := range docs {
		for _, key := range ownedSidecarKeys {
			if _, inline := docs[i].Metadata.Annotations[key]; inline {
				return fmt.Errorf(
					"%s: annotation %q is set inline while a sidecar exists at %s — a sidecar replaces the "+
						"manifest's harness annotations rather than overlaying them; remove the inline key or "+
						"delete the sidecar",
					path, key, sidecar.PathFor(path))
			}
		}
		anns := resolved[i]
		if len(anns) == 0 {
			continue
		}
		if docs[i].Metadata.Annotations == nil {
			docs[i].Metadata.Annotations = make(map[string]string, len(anns))
		}
		for k, v := range anns {
			docs[i].Metadata.Annotations[k] = v
		}
	}
	return nil
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

// parseReadyConditions splits ReadyConditionsKey's comma-separated value
// into condition types, trimming surrounding whitespace and dropping empty
// entries — the same shape uptest's own consumer of this annotation
// tolerates. An absent annotation, or one that is empty or all-whitespace,
// returns nil, which EffectiveReadyConditions then defaults from.
func parseReadyConditions(raw string) []string {
	var out []string
	for _, c := range strings.Split(raw, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
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
	m.ReadyConditions = parseReadyConditions(doc.Metadata.Annotations[ReadyConditionsKey])

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
		if t.Value == nil && !t.ValueExplicit && !t.Skip.Present() {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): value is required unless skip is set", i, t.Field)
		}
		if err := ValidateClear(t.Field, t.Clear); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		if err := ValidateWithValues(t.Field, t.WithValues, t.Clear); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		if err := ValidateIgnoreMapKeys(t); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		if err := ValidateIgnoreListElementKeys(t); err != nil {
			return nil, "", nil, nil, fmt.Errorf("entry %d (%s): %w", i, t.Field, err)
		}
		testedFields[t.Field] = true
	}
	if err := ValidateFieldEntryMix(tests); err != nil {
		return nil, "", nil, nil, err
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
// This rejection stays unconditional even for a Kind whose spec.forProvider
// declares no OTHER top-level field to host a sibling clear: entry against —
// it does not relax into accepting c == field just because no alternative
// exists. An entry that wants to null field itself uses a plain explicit
// `value: null` instead (see UpdateTest.ValueExplicit): that is a different,
// additive route to the same whole-field-tombstone merge-patch body, entirely
// outside the clear: mechanism this function guards.
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

// ValidateWithValues rejects a withValues map the merge-patch builder
// cannot honour, at parse time, before any cluster is touched.
//
// withValues follows the same top-level-only restriction ValidateClear
// enforces on clear, and for the identical reason: the literal it adds
// lands as a sibling KEY at the top level of the merge patch, alongside
// field's own value, so field itself must be a top-level spec.forProvider
// key for that sibling placement to mean what it says.
//
// Each key in withValues is validated the same way each entry in clear is:
// a dotted key is rejected, since this tool only ever patches a top-level
// sibling; a key equal to field itself is rejected too, for the same
// reason ValidateClear rejects it — it would silently overwrite the very
// value the patch is meant to set. A key that also appears in clear is
// rejected as well: the two directives would disagree about what that one
// sibling should hold in the same patch (a literal value from withValues,
// a null from clear), and nothing about a map's iteration order should
// decide which one wins.
//
// Exported so every source of a withValues map shares this one check — the
// "withValues:" key on an update-test annotation entry (via ParseAnnotation
// above) and the merge-patch builder that actually shapes the JSON (see
// runner.buildMergePatch).
func ValidateWithValues(field string, withValues map[string]interface{}, clear []string) error {
	if len(withValues) == 0 {
		return nil
	}
	if strings.Contains(field, ".") {
		return fmt.Errorf(
			"withValues is only supported for a top-level field; %q is nested — sibling-value patching at a "+
				"non-root nesting level is not supported", field)
	}
	clearSet := make(map[string]bool, len(clear))
	for _, c := range clear {
		clearSet[c] = true
	}
	// Sorted so a map with more than one invalid entry reports the SAME
	// offending key on every run — map iteration order is randomised by
	// Go, and an error message that changes from run to run for identical
	// input is its own small bug.
	keys := make([]string, 0, len(withValues))
	for k := range withValues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, sibling := range keys {
		if strings.Contains(sibling, ".") {
			return fmt.Errorf(
				"withValues entry %q: dot-separated paths are not supported — withValues only names a "+
					"top-level spec.forProvider field", sibling)
		}
		if sibling == field {
			return fmt.Errorf(
				"withValues entry %q: withValues must name OTHER sibling fields, not the field being patched itself", sibling)
		}
		if clearSet[sibling] {
			return fmt.Errorf(
				"withValues entry %q: also named in this entry's clear list — a sibling cannot be both nulled "+
					"and given a literal value in the same merge patch; remove it from one list", sibling)
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

	// The comparison strips every ignoreMapKeys key from BOTH the expected
	// and the actual side (runner.compareFieldValue), so a key named here
	// that the entry's own effective expectation ALSO names is deleted
	// before the comparison ever looks at it — the field test then reports
	// PASS regardless of what the live value actually is. The mechanism's
	// entire purpose is to let expect: (or value:, when expect: is unset)
	// omit a provider-injected member it could never predict; naming that
	// same member inside expect:/value: AND in ignoreMapKeys is always an
	// authoring error, never a legitimate shape — there is nothing left to
	// ignore that the author has not also (uselessly) tried to assert.
	effective := t.Expect
	source := "expect"
	if effective == nil {
		effective = t.Value
		source = "value"
	}
	if effObj, ok := effective.(map[string]interface{}); ok {
		for _, k := range t.IgnoreMapKeys {
			if _, present := effObj[k]; present {
				return fmt.Errorf(
					"ignoreMapKeys entry %q also appears as a key in this entry's own %s: — "+
						"the comparison strips ignoreMapKeys keys from BOTH sides before comparing, so this "+
						"key would be deleted from %s: too and the test would pass without ever checking it; "+
						"remove the key from ignoreMapKeys (it is redundant — %s: already covers it) or "+
						"remove it from %s: (if it is really a provider-injected member the manifest cannot predict)",
					k, source, source, source, source)
			}
		}
	}
	return nil
}

// ValidateIgnoreListElementKeys rejects an ignoreListElementKeys declaration
// the runner could not act on, at parse time, before any cluster is
// touched. See UpdateTest.IgnoreListElementKeys for what the mechanism is
// for.
//
// It mirrors ValidateIgnoreMapKeys's checks — empty/duplicate entries,
// mutual exclusion with skip, and the same-name authoring-error scan — but
// the scan reaches one level deeper: the field this mechanism targets is
// LIST-shaped, so "the entry's own effective expectation" is a JSON array,
// and a name is redundant the moment ANY element of that array already
// carries it, since the comparison (runner.compareFieldValue) strips the
// key from every element, not from the array itself.
//
// It is exported so both sources of an ignoreListElementKeys list share
// this one check — the "ignoreListElementKeys:" key on an update-test
// annotation entry (via ParseAnnotation above) and any offline validator
// that wants to check a manifest without running it.
func ValidateIgnoreListElementKeys(t UpdateTest) error {
	if len(t.IgnoreListElementKeys) == 0 {
		return nil
	}
	if t.Skip.Present() {
		return fmt.Errorf(
			"ignoreListElementKeys is set but skip is also set — skip asserts no test exists to write, " +
				"so there is no comparison left for ignoreListElementKeys to affect; remove one directive")
	}
	seen := make(map[string]bool, len(t.IgnoreListElementKeys))
	for _, k := range t.IgnoreListElementKeys {
		if k == "" {
			return fmt.Errorf("ignoreListElementKeys entry is empty — name the per-element key to exclude from comparison")
		}
		if seen[k] {
			return fmt.Errorf("ignoreListElementKeys entry %q is repeated", k)
		}
		seen[k] = true
	}

	// Same authoring-error scan as ValidateIgnoreMapKeys, one level
	// deeper: the comparison strips every ignoreListElementKeys key from
	// EVERY element on BOTH sides (runner.compareFieldValue), so a key
	// named here that an element of the entry's own effective expectation
	// ALSO names is deleted before the comparison ever looks at it — the
	// field test then reports PASS regardless of what the live value
	// actually is for that element.
	effective := t.Expect
	source := "expect"
	if effective == nil {
		effective = t.Value
		source = "value"
	}
	if effArr, ok := effective.([]interface{}); ok {
		for _, elem := range effArr {
			effObj, ok := elem.(map[string]interface{})
			if !ok {
				continue
			}
			for _, k := range t.IgnoreListElementKeys {
				if _, present := effObj[k]; present {
					return fmt.Errorf(
						"ignoreListElementKeys entry %q also appears as a key in one of this entry's own %s: elements — "+
							"the comparison strips ignoreListElementKeys keys from every element on BOTH sides before "+
							"comparing, so this key would be deleted from %s: too and the test would pass without ever "+
							"checking it; remove the key from ignoreListElementKeys (it is redundant — %s: already covers "+
							"it) or remove it from %s: (if it is really a provider-injected per-element member the "+
							"manifest cannot predict)",
						k, source, source, source, source)
				}
			}
		}
	}
	return nil
}

// entryCarriesTestedContent reports whether a single UpdateTest entry
// asserts a real value/expect/clear/withValues, regardless of whether the
// SAME entry also carries a skip:. It exists to catch the combined shape
// ValidateFieldEntryMix's cross-entry comparison cannot see — see that
// function's doc comment.
func entryCarriesTestedContent(t UpdateTest) bool {
	return t.Value != nil || t.ValueExplicit || t.Expect != nil || len(t.Clear) > 0 || len(t.WithValues) > 0
}

// ValidateFieldEntryMix rejects a field whose entries in one update-test
// block mix a skip: entry with a tested entry (an entry that carries no
// skip:, asserting a real value/expect/clear/withValues instead) — whether
// the two live in separate entries for the same field, or BOTH are written
// into one single entry.
//
// A field legitimately carrying SEVERAL tested entries in one block is the
// established idiom — testing its own value axis and its clear:/withValues:
// axis as separate entries — and is never rejected here. What is rejected
// is the OTHER combination: every consumer that resolves a field's coverage
// status (the mutable-field coverage report, and the container-clear
// disposition lookup) does it through a map keyed on field name, built by
// iterating m.Tests in order with each entry overwriting the last. When
// every entry for a field is tested, last-wins is harmless — the status is
// "tested" whichever entry happens to win. When a skip: entry sits beside a
// tested one, last-wins silently downgrades (or upgrades) the field's
// reported coverage depending on which entry was authored last, with
// nothing anywhere reporting that a test result was discarded.
//
// The single-entry combined shape is checked first and separately, because
// it cannot be caught by comparing entries against each other: with only
// one entry for the field there is nothing to compare it to, so it would
// otherwise parse clean. It is also the more dangerous of the two shapes —
// the per-field execution loop checks skip: first and moves on to the next
// field before the tested half of the SAME entry is ever considered, so the
// entry would silently run as a no-op skip while the tested assertion never
// fires, and every coverage consumer still reports the field as tested.
//
// Exported so both parse-time sources of a tests slice (ParseAnnotation
// above, and any offline validator that wants to check a slice it built
// itself) share this one check.
func ValidateFieldEntryMix(tests []UpdateTest) error {
	for _, t := range tests {
		if t.Skip.Present() && entryCarriesTestedContent(t) {
			return fmt.Errorf(
				"field %q's entry carries both a skip: and a tested assertion (value:/expect:/clear:/withValues:) "+
					"in the SAME entry — the runner checks skip: first and moves on before the tested half is ever "+
					"considered, so this entry would silently execute as a no-op skip, the tested assertion would "+
					"never fire, and the field would still be reported as covered; split the two into separate "+
					"entries, or remove whichever of skip:/the tested assertion does not belong",
				t.Field)
		}
	}

	kindByField := make(map[string]string, len(tests))
	for _, t := range tests {
		kind := "tested"
		if t.Skip.Present() {
			kind = "skip"
		}
		prev, seen := kindByField[t.Field]
		if !seen {
			kindByField[t.Field] = kind
			continue
		}
		if prev != kind {
			return fmt.Errorf(
				"field %q carries both a skip: entry and a tested entry (value:/expect:/clear:/withValues:) "+
					"in the same %s block — a field's coverage status is resolved through a map keyed on "+
					"field name with last-entry-wins semantics, so the skip: entry would silently discard "+
					"the tested entry's coverage (or the reverse); remove the skip: entry if the field is "+
					"genuinely tested, or remove the tested entry if it is not",
				t.Field, AnnotationKey)
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
