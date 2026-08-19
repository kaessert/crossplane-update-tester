// Package validator checks that a manifest's update-test annotation covers
// every mutable field of the corresponding generated Go API type.
package validator

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
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
	Status   string // "tested", "skipped", "immutable", "reference-plumbing", "MISSING"
}

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

	// Track brace depth to detect the end of the current struct.
	p.braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
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

	if len(p.fields) == 0 {
		return nil, fmt.Errorf("no %s struct found in %s", structName, path)
	}

	return p.fields, nil
}

// ValidateManifest checks that the manifest's update-test annotation covers
// all mutable fields from the Go types.
func ValidateManifest(m *manifest.Manifest, fields []FieldInfo) *ValidationResult {
	// Build a set of tested/skipped fields from the annotation.
	tested := make(map[string]string) // jsonName → "tested" or "skipped"
	for _, t := range m.Tests {
		if t.Skip != "" {
			tested[t.Field] = "skipped"
		} else {
			tested[t.Field] = "tested"
		}
	}

	// Build the set of all field JSON names in this struct so reference-
	// plumbing detection can confirm a matching base value field exists.
	fieldSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldSet[f.JSONName] = true
	}

	result := &ValidationResult{
		Kind:    m.Kind,
		AllGood: true,
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

// PrintValidation outputs the validation result to stdout.
func PrintValidation(r *ValidationResult) {
	structName := r.Kind + "Parameters"
	fmt.Printf("Validating %s manifest against %s\n", r.Kind, structName)

	for _, f := range r.Fields {
		var icon string
		var detail string
		switch f.Status {
		case "tested":
			icon = "✓"
			detail = "covered (tested)"
		case "skipped":
			icon = "✓"
			detail = "covered (skipped)"
		case "immutable":
			icon = "✓"
			detail = "immutable (excluded)"
		case "reference-plumbing":
			icon = "✓"
			detail = "reference plumbing (excluded)"
		case "MISSING":
			icon = "✗"
			detail = "MISSING — not covered by update-test annotation"
		}
		fmt.Printf("  %s %s: %s\n", icon, f.JSONName, detail)
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
