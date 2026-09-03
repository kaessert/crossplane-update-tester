// Package validator checks that a manifest's update-test annotation covers
// every mutable field of the corresponding generated Go API type.
package validator

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// FieldInfo holds metadata about a struct field parsed from Go source.
type FieldInfo struct {
	GoName    string
	JSONName  string
	Immutable bool
	// GoType is the field's declared Go type, exactly as written in source
	// (e.g. "*string", "[]FooBar", "map[string]string",
	// "*xpv2.NamespacedReference"). It is used to resolve the corresponding
	// generated Observation struct for object/list-of-object fields — see
	// structElemType and CheckObservability in observability.go.
	GoType string
	// Omitempty reports whether the field's json tag carries the
	// "omitempty" option. A field WITHOUT omitempty — most commonly a
	// +nullable marker's generated companion — always marshals, as an
	// explicit null when unset. resolveObservationFields in
	// observability.go uses this to find Observation struct members an
	// update-test expectation cannot safely omit; see
	// CheckIncompleteExpectations in incompleteexpect.go.
	Omitempty bool
}

// ValidationResult holds the outcome of validating a manifest against types.
type ValidationResult struct {
	Kind    string
	Fields  []FieldValidation
	AllGood bool
}

// FieldValidation holds the status of a single field in validation.
type FieldValidation struct {
	JSONName string
	Status   string // "tested", "skipped", "skipped-unstructured", "immutable", "reference-plumbing", "MISSING", "tested-via-switch", "clear-target-unknown", "withValues-target-unknown", "guarded-assert-unchanged"
}

// clearCreditStatus is the status a field is credited under when the ONLY
// coverage it has is being named in another entry's "clear:" list — nulled
// as a side effect of a sibling union arm being switched in, never
// independently set or asserted itself. Deliberately distinct from
// "tested": a cleared arm is proven clearable, never proven settable, and
// folding the two together would let a single switch test that clears six
// sibling arms report all six as having had their own value tested. See
// ValidateManifest.
const clearCreditStatus = "tested-via-switch"

// legacySkipStatus is the status a field is credited under when its
// "skip:" entry is the pre-migration free-prose string form rather than the
// structured mapping form (see manifest.SkipInfo.Legacy). It is deliberately
// distinct from "skipped" — the field still counts as covered, but a legacy
// entry carries no reason a machine can check, so keeping it a separate
// status gives each provider its own burn-down count as entries migrate to
// the structured form. See ValidateManifest.
const legacySkipStatus = "skipped-unstructured"

// clearTargetUnknownStatus flags a "clear:" entry naming a field that does
// not exist in the target type's declared struct fields — a typo, or a
// stale reference to a field that was renamed or removed after the entry
// was written. Crediting a name nothing resolves to would let the coverage
// arithmetic drift from the declared struct with no signal at all, so this
// status feeds into the same AllGood/Fields reporting a MISSING field uses
// rather than passing silently. See ValidateManifest.
const clearTargetUnknownStatus = "clear-target-unknown"

// withValuesTargetUnknownStatus flags a "withValues:" entry naming a field
// that does not exist in the target type's declared struct fields — the
// same failure mode clearTargetUnknownStatus guards for "clear:", applied
// to the other directive that writes a sibling in the same merge patch. A
// typo'd or stale key here is silently accepted at parse time, folded into
// the outgoing merge patch, and then pruned by the API server's
// structural-schema pruning: the run reports coverage for a sibling it
// never actually set. Kept as its own status rather than reusing
// clearTargetUnknownStatus so the rendered detail names the directive that
// was actually misused. See ValidateManifest.
const withValuesTargetUnknownStatus = "withValues-target-unknown"

// assertUnchangedCreditStatus is the status a field is credited under when
// its ONLY coverage is being named — as its own top-level field, or as the
// first path segment of a nested status.atProvider path — by the
// manifest's "assert-unchanged:" directive (see manifest.Manifest.
// AssertUnchanged). A field the directive names DIRECTLY cannot also carry
// its own update-test entry (manifest.ParseAnnotation rejects that overlap
// before any cluster is touched), so a manifest author choosing the
// stronger, actively-enforced guard necessarily has no "field:"/"skip:"
// entry left to write for it. Without this credit the field reports
// MISSING, penalising the manifest that guards it more strongly than an
// ordinary skip: would.
// Deliberately distinct from "tested" and "skipped": assert-unchanged
// proves the field never DRIFTS, never that a specific new value can be
// WRITTEN to it, so folding it into either would overstate what was
// actually exercised. See ValidateManifest.
const assertUnchangedCreditStatus = "guarded-assert-unchanged"

// Regexes for parsing Go struct fields.
var (
	// Matches a struct field line like:
	//   Name string `json:"name" ...`
	//   Path *string `json:"path,omitempty" ...`
	// Group 1 is the Go field name, group 2 is the raw declared Go type
	// (a single token — Go type literals never contain whitespace), group 3
	// is the JSON tag name, group 4 is whatever follows it inside the same
	// quoted tag value (e.g. ",omitempty", or "" when the tag carries no
	// further options) — addField checks it for the "omitempty" option.
	reStructField = regexp.MustCompile(
		`^\s+(\w+)\s+(\S+).*` + "`" + `.*json:"([^",]+)([^"]*)"` + `.*` + "`",
	)

	// Matches XValidation marker for immutability.
	// Looks for: rule="self == oldSelf" in a comment or marker above the field.
	reImmutable = regexp.MustCompile(`self\s*==\s*oldSelf`)

	// Matches the start of any named struct — used to locate both the
	// target {Kind}Parameters struct and, separately, arbitrary nested
	// struct types (e.g. a {Kind}{X}Observation companion) that
	// CheckObservability resolves by name.
	reAnyStruct = regexp.MustCompile(`^type\s+(\w+)\s+struct\s*\{`)
)

// referencePlumbingSuffixes are the JSON-name suffixes angryjet appends to a
// base value field's name when it generates the companion cross-resource
// reference fields. A scalar base field gets a singular companion pair
// ("owner" + "ownerRef" + "ownerSelector"); a list-typed base field
// (`[]string`) gets a PLURAL "Refs" companion alongside the same singular
// "Selector" — angryjet emits one Selector regardless of base cardinality,
// e.g. "attachVpc" + "attachVpcRefs" + "attachVpcSelector". Both are
// Crossplane reference-resolution machinery, not independent API fields, so
// neither has update semantics of its own to exercise.
var referencePlumbingSuffixes = []string{"Ref", "Refs", "Selector"}

// isReferencePlumbingField reports whether jsonName is a generated
// reference-plumbing field (a "*Ref", "*Refs", or "*Selector" companion to a
// base value field) whose base field is present in fieldSet. Fields are only
// classified as reference plumbing when the matching base field actually
// exists — this way a genuinely missing or renamed base value field is
// still reported as MISSING rather than silently excused.
func isReferencePlumbingField(jsonName string, fieldSet map[string]bool) bool {
	for _, suffix := range referencePlumbingSuffixes {
		if !strings.HasSuffix(jsonName, suffix) {
			continue
		}
		base := strings.TrimSuffix(jsonName, suffix)
		if base != "" && fieldSet[base] {
			return true
		}
	}
	return false
}

// goTypesParser is a small state machine that scans a Go source file line by
// line looking for the named targetStruct, skipping over any other struct
// declarations it encounters along the way (e.g. nested config structs,
// sibling Observation variants). It uses basic regex parsing.
type goTypesParser struct {
	targetStruct string
	fields       []FieldInfo
	found        bool // targetStruct's declaration was located, even if it has zero fields
	inTarget     bool // inside the struct we actually want to parse
	inOther      bool // inside a non-matching struct (skipping)
	braceDepth   int
	prevLines    []string // buffer of preceding comment/marker lines
	done         bool
}

// handleLine processes a single line of source, advancing the parser state.
func (p *goTypesParser) handleLine(line string) {
	if !p.inTarget && !p.inOther {
		p.tryEnterStruct(line)
		return
	}

	// Track brace depth to detect the end of the current struct. Braces are
	// counted only in the code portion of the line — see
	// stripLineComment — so a doc comment quoting unbalanced brace
	// characters (e.g. a snippet of another language embedded in a field's
	// doc comment) cannot be mistaken for the struct's own closing brace.
	codeOnly := stripLineComment(line)
	p.braceDepth += strings.Count(codeOnly, "{") - strings.Count(codeOnly, "}")
	if p.braceDepth <= 0 {
		if p.inTarget {
			// Done parsing the target struct.
			p.done = true
			return
		}
		// Finished skipping a non-target struct; resume searching.
		p.inOther = false
		return
	}

	// Inside a non-target struct — skip everything.
	if p.inOther {
		return
	}

	// Inside the target struct — parse field declarations.
	p.parseFieldLine(line)
}

// tryEnterStruct checks whether line opens any named struct and, if so,
// starts tracking it (either as the target struct or one to skip past).
func (p *goTypesParser) tryEnterStruct(line string) {
	m := reAnyStruct.FindStringSubmatch(line)
	if m == nil {
		return
	}
	if m[1] == p.targetStruct {
		// Found the struct we want — start parsing its fields.
		p.inTarget = true
		p.found = true
		p.braceDepth = 1
		p.prevLines = nil
		return
	}
	// Some other struct declaration — track depth so we can skip past it.
	p.inOther = true
	p.braceDepth = 1
}

// parseFieldLine handles a single line while inside the target struct: it
// either records a field declaration, accumulates a preceding comment/marker
// line, or resets the comment buffer.
func (p *goTypesParser) parseFieldLine(line string) {
	matches := reStructField.FindStringSubmatch(line)
	switch {
	case matches != nil:
		p.addField(matches[1], matches[2], matches[3], strings.Contains(matches[4], "omitempty"), line)
	case isCommentOrMarkerLine(line):
		// Accumulate comment/marker lines.
		p.prevLines = append(p.prevLines, line)
	default:
		p.prevLines = nil
	}
}

// stripLineComment returns the portion of line preceding a "//" line
// comment, so brace-depth tracking in handleLine never counts a brace
// character that only appears inside prose — a Go doc comment quoting a
// code or config snippet from another language is free to carry
// unbalanced "{"/"}" without being mistaken for the struct's own closing
// brace. This is comment-line exclusion, not a full Go tokenizer, but it
// does track backtick-quoted struct-tag strings well enough to skip a "//"
// that appears inside one (e.g. a tag carrying a "https://..." default
// value) rather than misreading it as the start of a comment — a
// realistic collision a bare substring search would get wrong. A line
// with no unquoted "//" is returned unchanged.
func stripLineComment(line string) string {
	inTag := false
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '`':
			inTag = !inTag
		case !inTag && line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// isCommentOrMarkerLine reports whether line is a comment or a kubebuilder/
// crossplane marker that should be buffered for immutability detection.
func isCommentOrMarkerLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//") ||
		strings.Contains(line, "+kubebuilder") ||
		strings.Contains(line, "+crossplane")
}

// addField records a parsed field declaration, determining immutability from
// the buffered preceding comment lines or the field line itself.
func (p *goTypesParser) addField(goName, goType, jsonName string, omitempty bool, line string) {
	// Check if any preceding comment lines contain immutability marker.
	immutable := false
	for _, pl := range p.prevLines {
		if reImmutable.MatchString(pl) {
			immutable = true
			break
		}
	}
	// Also check the field line itself (inline markers).
	if reImmutable.MatchString(line) {
		immutable = true
	}

	p.fields = append(p.fields, FieldInfo{
		GoName:    goName,
		JSONName:  jsonName,
		Immutable: immutable,
		GoType:    goType,
		Omitempty: omitempty,
	})
	p.prevLines = nil
}

// ParseGoTypes reads a Go source file and extracts fields from the
// {targetKind}Parameters struct. It skips other structs (e.g. nested config
// structs, Observation variants) until it finds the one matching targetKind.
func ParseGoTypes(path, targetKind string) ([]FieldInfo, error) {
	return ParseStructFields(path, targetKind+"Parameters")
}

// ParseStructFields reads a Go source file and extracts fields from the
// named struct, skipping over every other struct declaration it encounters
// along the way. Unlike ParseGoTypes it takes the exact struct name rather
// than a {Kind} and an implied "Parameters" suffix, so it can also resolve
// an arbitrary nested struct — most importantly a {Kind}{X}Observation
// companion type — which CheckObservability needs to determine whether a
// field's declared shape is observable.
func ParseStructFields(path, structName string) ([]FieldInfo, error) {
	// #nosec G304 -- path is an operator-supplied CLI argument (the
	// generated types file to validate), not attacker-controlled input.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening types file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	p := &goTypesParser{targetStruct: structName}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		p.handleLine(scanner.Text())
		if p.done {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning types file: %w", err)
	}

	if !p.found {
		return nil, fmt.Errorf("no %s struct found in %s", structName, path)
	}

	// The struct was located — a nullary struct (e.g. a zero-payload union
	// arm marker) is a real, resolvable declaration with zero members, not
	// an unresolvable one. Callers distinguish "found" from "unresolved"
	// solely by nil-ness of the returned slice, so a found struct must
	// never yield nil here even when it declares no fields.
	if p.fields == nil {
		p.fields = []FieldInfo{}
	}

	return p.fields, nil
}

// ValidateManifest checks that the manifest's update-test annotation covers
// all mutable fields from the Go types.
func ValidateManifest(m *manifest.Manifest, fields []FieldInfo) *ValidationResult {
	// Build a set of tested/skipped fields from the annotation. This pass
	// records only each entry's own "field:" — a second pass below layers
	// clear: credit on top, so that a field's OWN direct entry (whichever
	// order the two appear in m.Tests) always wins over a weaker credit
	// picked up only because some other entry's clear: happened to name it.
	tested := make(map[string]string) // jsonName → "tested", "skipped" or "skipped-unstructured"
	for _, t := range m.Tests {
		switch {
		case t.Skip.Legacy:
			tested[t.Field] = legacySkipStatus
		case t.Skip.Present():
			tested[t.Field] = "skipped"
		default:
			tested[t.Field] = "tested"
		}
	}

	// Build the set of all field JSON names in this struct so reference-
	// plumbing detection can confirm a matching base value field exists,
	// and so a clear: entry can be checked against the type it claims to
	// describe.
	fieldSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldSet[f.JSONName] = true
	}

	result := &ValidationResult{
		Kind:    m.Kind,
		AllGood: true,
	}

	// assert-unchanged coverage credit: a field named by the manifest's
	// "assert-unchanged:" directive (m.AssertUnchanged) can never also
	// carry its own "field:"/"skip:" entry — manifest.ParseAnnotation
	// rejects that overlap at parse time — so a manifest author who
	// chose the stronger, actively-enforced guard has no weaker skip:
	// entry left to earn the ordinary coverage credit. Crediting it here,
	// under its own distinct status, stops that choice from reporting
	// MISSING. Each directive entry is a dot-separated status.atProvider
	// path (e.g. "legacyRuleList.rules"); only the first segment can ever
	// name a spec field this validator knows about, so that is what is
	// checked against fieldSet and credited — a path whose first segment
	// names no declared field credits nothing, exactly like an ordinary
	// MISSING field today. It is credited only when the field has no
	// stronger direct entry of its own, mirroring the clear: credit
	// ordering below. That guard is load-bearing, not merely defensive:
	// the parse-time overlap rejection compares WHOLE names, so only a
	// directly-named field is barred from also carrying its own entry —
	// a nested path's first segment may carry one, and f5xc's
	// secret-policy manifest uses exactly that shape deliberately
	// ("legacyRuleList.rules" guarding a legacyRuleList that keeps its
	// own skip: entry). Its own entry wins.
	for _, a := range m.AssertUnchanged {
		top := a
		if idx := strings.Index(a, "."); idx >= 0 {
			top = a[:idx]
		}
		if !fieldSet[top] {
			continue
		}
		if _, ok := tested[top]; !ok {
			tested[top] = assertUnchangedCreditStatus
		}
	}

	// Group-aware coverage credit: a non-skipped entry's clear: list names
	// sibling union-arm fields that this entry's merge patch nulls in the
	// SAME atomic patch that sets its own field's value. Each named sibling
	// is real coverage — the null was proven to hold, the same way any
	// other value's post-patch state is proven — so it is credited here
	// under clearCreditStatus, not left MISSING. It is credited only when
	// the sibling has no stronger direct entry of its own (see the ordering
	// note above), and a sibling name that resolves to no declared struct
	// field at all is flagged rather than silently ignored.
	for _, t := range m.Tests {
		if t.Skip.Present() {
			continue
		}
		for _, c := range t.Clear {
			if !fieldSet[c] {
				result.Fields = append(result.Fields, FieldValidation{
					JSONName: c,
					Status:   clearTargetUnknownStatus,
				})
				result.AllGood = false
				continue
			}
			if _, ok := tested[c]; !ok {
				tested[c] = clearCreditStatus
			}
		}
	}

	// Unknown-target rejection for "withValues:", mirroring the clear:
	// check above. Deliberately NOT mirrored further: a withValues sibling
	// earns no coverage credit here (contrast the clearCreditStatus
	// assignment above) because its post-patch value is never asserted by
	// the runner — only the null a clear: entry writes is proven to hold.
	// A withValues sibling with no direct entry of its own is left exactly
	// as it already was before this check existed (typically MISSING),
	// which is the correct, unembellished answer to "was this field's
	// value independently tested?".
	for _, t := range m.Tests {
		if t.Skip.Present() {
			continue
		}
		// Sorted so a map with more than one unknown entry reports the
		// same offending keys, in the same order, on every run — map
		// iteration order is randomised by Go (mirrors the sort in
		// manifest.ValidateWithValues).
		siblings := make([]string, 0, len(t.WithValues))
		for sibling := range t.WithValues {
			siblings = append(siblings, sibling)
		}
		sort.Strings(siblings)
		for _, sibling := range siblings {
			if !fieldSet[sibling] {
				result.Fields = append(result.Fields, FieldValidation{
					JSONName: sibling,
					Status:   withValuesTargetUnknownStatus,
				})
				result.AllGood = false
			}
		}
	}

	for _, f := range fields {
		var v FieldValidation
		v.JSONName = f.JSONName

		switch {
		case f.Immutable:
			v.Status = "immutable"
		case isReferencePlumbingField(f.JSONName, fieldSet):
			v.Status = "reference-plumbing"
		default:
			if status, ok := tested[f.JSONName]; ok {
				v.Status = status
			} else {
				v.Status = "MISSING"
				result.AllGood = false
			}
		}
		result.Fields = append(result.Fields, v)
	}

	return result
}

// statusIconDetail pairs one FieldValidation.Status value with the console
// icon and human-readable detail line PrintValidation renders for it.
type statusIconDetail struct {
	Status string
	Icon   string
	Detail string
}

// statusOrder is the single declared list of every status
// FieldValidation.Status can hold, in the order PrintValidation renders
// them. A status can reach the console only by having an entry here, so
// there is no second switch statement that can fall out of step with it —
// and KnownStatuses lets a test assert this list against README.md's
// `validate` status documentation without hand-listing the statuses again.
// See main_test.go TestValidateStatusesDocumented.
var statusOrder = []statusIconDetail{
	{Status: "tested", Icon: "✓", Detail: "covered (tested)"},
	{Status: "skipped", Icon: "✓", Detail: "covered (skipped)"},
	{Status: legacySkipStatus, Icon: "✓", Detail: "covered (skipped, unstructured)"},
	{Status: "immutable", Icon: "✓", Detail: "immutable (excluded)"},
	{Status: "reference-plumbing", Icon: "✓", Detail: "reference plumbing (excluded)"},
	{Status: clearCreditStatus, Icon: "✓", Detail: "covered (nulled by a sibling entry's clear: — proven clearable, not independently value-tested)"},
	{Status: assertUnchangedCreditStatus, Icon: "⊙", Detail: "guarded (assert-unchanged) — proven never to drift, not independently value-tested"},
	{Status: "MISSING", Icon: "✗", Detail: "MISSING — not covered by update-test annotation"},
	{Status: clearTargetUnknownStatus, Icon: "✗", Detail: "INVALID — named in a clear: list but not a declared field on this type"},
	{Status: withValuesTargetUnknownStatus, Icon: "✗", Detail: "INVALID — named in a withValues: map but not a declared field on this type"},
}

// KnownStatuses returns every status FieldValidation.Status can hold, in
// statusOrder's declared order. Exported so a test outside this package can
// assert README.md documents exactly this set, deriving the comparison from
// the code instead of hand-listing the statuses a second time.
func KnownStatuses() []string {
	out := make([]string, len(statusOrder))
	for i, sd := range statusOrder {
		out[i] = sd.Status
	}
	return out
}

// PrintValidation outputs the validation result to stdout.
func PrintValidation(r *ValidationResult) {
	structName := r.Kind + "Parameters"
	fmt.Printf("Validating %s manifest against %s\n", r.Kind, structName)

	details := make(map[string]statusIconDetail, len(statusOrder))
	for _, sd := range statusOrder {
		details[sd.Status] = sd
	}

	for _, f := range r.Fields {
		// Zero value (empty icon and detail) for a status absent from
		// statusOrder, matching the original switch's implicit no-op when
		// no case matched — ValidateManifest never assigns one, so this is
		// unreached in practice.
		d := details[f.Status]
		fmt.Printf("  %s %s: %s\n", d.Icon, f.JSONName, d.Detail)
	}

	fmt.Println()
	if r.AllGood {
		fmt.Println("All mutable fields covered.")
	} else {
		fmt.Println("FAIL: some mutable fields are not covered.")
	}
}

// PrintObservability outputs any ObservabilityFinding results to stdout. It
// is a distinct diagnostic block from PrintValidation's MISSING report: a
// field can be fully "covered" by an update-test entry (a value exists for
// every mutable field) while that entry's expectation still names a key
// that can never appear in status.atProvider — coverage and observability
// are different properties, and this prints the second one separately so
// the two are never conflated in the output.
func PrintObservability(findings []ObservabilityFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Unobservable expectations:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: UNOBSERVABLE — key(s) %s excluded from the generated Observation struct by construction (input-only reference field); add expect: with the resolved value\n",
			f.Field, strings.Join(f.Keys, ", "))
	}
	fmt.Println()
	fmt.Println("FAIL: some update-test expectations are structurally unobservable in atProvider.")
}
