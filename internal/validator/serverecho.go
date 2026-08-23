package validator

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// ServerEchoedExpectationFinding records a single update-test entry whose
// effective expectation omits a member the resource's own controller
// declares SERVER-ECHOED: a value the backend fills in on every element the
// caller never set one for, regardless of what the generated struct's json
// tag says. It is a distinct trap from IncompleteExpectationFinding, which
// only reasons about members WITHOUT "omitempty" — a member the controller
// clears with a normalizer is, by construction, declared WITH "omitempty"
// (the desired side never encodes the server's default explicitly), so
// IncompleteExpectationFinding structurally cannot see this class at all.
type ServerEchoedExpectationFinding struct {
	// Field is the manifest field name from the update-test entry (the
	// "field:" value).
	Field string
	// Keys are the sorted server-echoed member JSON name(s) absent from
	// the effective expectation, found anywhere in the value's nested
	// object tree (not only at the top level) — see
	// walkServerEchoedMembers.
	Keys []string
}

// ServerEchoedMember is one (struct shape, field) pair a resource's
// controller declares server-echoed: it registers a go-cmp Transformer that
// clears the field before comparing, because the backend always returns it
// — even when the caller never set it — and the desired side leaves it
// absent (omitempty never encodes the zero value explicitly), so a verbatim
// comparison would report a permanent diff and the controller would call
// Update() every reconcile on an otherwise idle resource.
type ServerEchoedMember struct {
	// StructFieldNames is the sorted set of every Go field name the
	// controller's own hand-written wire-shape struct declares (e.g.
	// every field of an "apiHeaderMatcherType"-shaped struct the
	// controller normalizes). It has no naming relationship to the
	// CRD-generated struct that mirrors it — the generator and the
	// controller name their structs independently — so matching by field
	// name SET rather than by type name is what lets
	// matchServerEchoedMembers connect the two without a hand-maintained
	// map between them.
	StructFieldNames []string
	// EchoedField is the Go field name the controller's normalizer
	// clears, e.g. "InvertMatch".
	EchoedField string
}

// reTransformerCall matches a go-cmp Transformer registration naming the
// clearing function directly, e.g.
//
//	var normalizeHeaderMatcherInvertMatch = cmp.Transformer("normalizeHeaderMatcherInvertMatch", clearServerDefaultedInvertMatch)
//
// Group 1 is the clearing function's name.
var reTransformerCall = regexp.MustCompile(`cmp\.Transformer\(\s*"[^"]*"\s*,\s*(\w+)\s*\)`)

// reFuncStart matches a top-level function definition line, capturing the
// function name (group 1) and its first parameter's declared type (group
// 2), with any leading "*" stripped. It intentionally matches only a
// single-parameter, single-line signature — every clearing function this
// tool has been written against takes exactly the value it normalizes and
// returns the same shape (see clearServerDefaultedDescription and
// clearServerDefaultedInvertMatch in provider-f5xc's httploadbalancer
// controller) — so a signature that does not match this shape is simply not
// resolved, the same conservative bar the rest of this package holds to.
var reFuncStart = regexp.MustCompile(`^func\s+(\w+)\s*\(\s*\w+\s+\*?(\w+)\s*\)`)

// reZeroAssign matches a WHOLE trimmed source line that assigns a
// server-default zero value to exactly one field of exactly one local
// variable, e.g. "out.Description = nil" or "h.InvertMatch = nil". It
// requires the line contain nothing else — no trailing "&&", no leading
// "if" — specifically so it never matches a comparison line such as
// "if m.Description == nil" (a single "=" preceded by nothing and followed
// by nothing but the zero-value token, anchored start to end, is never
// satisfied by a "==" or "!=" comparison).
var reZeroAssign = regexp.MustCompile(`^\w+\.(\w+)\s*=\s*(?:nil|""|false|0)$`)

// discoverServerEchoedMembers scans every non-test ".go" file directly in
// controllerDir for go-cmp Transformer registrations and resolves each one
// to the (struct field-name set, cleared field) pair CheckServerEchoedExpectations
// matches an Observation-tree struct against. A controller package with no
// such registration returns an empty, non-error result — the feature is
// simply inert for a resource whose controller has nothing of this shape to
// declare.
func discoverServerEchoedMembers(controllerDir string) ([]ServerEchoedMember, error) {
	files, err := controllerGoFiles(controllerDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .go files found in %s", controllerDir)
	}

	fnNames := make(map[string]bool)
	for _, path := range files {
		if err := scanTransformerCalls(path, fnNames); err != nil {
			return nil, err
		}
	}
	if len(fnNames) == 0 {
		return nil, nil
	}

	clearedFields := make(map[string]map[string]bool) // paramType -> set of cleared Go field names
	for _, path := range files {
		if err := scanClearingFuncs(path, fnNames, clearedFields); err != nil {
			return nil, err
		}
	}

	var members []ServerEchoedMember
	for paramType, fields := range clearedFields {
		structFields, err := parseStructFieldsInFiles(files, paramType)
		if err != nil {
			// Unresolvable — the same conservative bar every other check
			// in this package holds to: skip rather than guess.
			continue
		}
		names := make([]string, len(structFields))
		for i, f := range structFields {
			names[i] = f.GoName
		}
		sort.Strings(names)

		fieldNames := make([]string, 0, len(fields))
		for f := range fields {
			fieldNames = append(fieldNames, f)
		}
		sort.Strings(fieldNames)

		for _, f := range fieldNames {
			members = append(members, ServerEchoedMember{StructFieldNames: names, EchoedField: f})
		}
	}
	return members, nil
}

// controllerGoFiles returns every non-test ".go" file directly inside dir,
// sorted for deterministic scanning order.
func controllerGoFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", dir, err)
	}
	var out []string
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// scanTransformerCalls reads path line by line, adding the clearing
// function name of every cmp.Transformer(...) registration found to fnNames.
func scanTransformerCalls(path string, fnNames map[string]bool) error {
	return scanLines(path, func(line string) {
		if m := reTransformerCall.FindStringSubmatch(line); m != nil {
			fnNames[m[1]] = true
		}
	})
}

// scanClearingFuncs reads path looking for the definition of every function
// named in fnNames, and records the Go field name(s) its body clears to a
// server-default zero value (see reZeroAssign) under the function's
// parameter type in clearedFields.
func scanClearingFuncs(path string, fnNames map[string]bool, clearedFields map[string]map[string]bool) error {
	f, err := os.Open(path) // #nosec G304 -- path is derived from controllerGoFiles' own glob, not attacker input.
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var (
		inFunc     bool
		braceDepth int
		paramType  string
	)
	for scanner.Scan() {
		line := scanner.Text()

		if !inFunc {
			m := reFuncStart.FindStringSubmatch(line)
			if m == nil || !fnNames[m[1]] {
				continue
			}
			inFunc = true
			paramType = m[2]
			braceDepth = strings.Count(line, "{") - strings.Count(line, "}")
			if braceDepth <= 0 {
				inFunc = false // single-line body — nothing to scan
			}
			continue
		}

		if m := reZeroAssign.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if clearedFields[paramType] == nil {
				clearedFields[paramType] = make(map[string]bool)
			}
			clearedFields[paramType][m[1]] = true
		}

		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
		if braceDepth <= 0 {
			inFunc = false
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning %s: %w", path, err)
	}
	return nil
}

// scanLines calls fn once per line of path.
func scanLines(path string, fn func(line string)) error {
	f, err := os.Open(path) // #nosec G304 -- path is derived from controllerGoFiles' own glob, not attacker input.
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fn(scanner.Text())
	}
	return scanner.Err()
}

// parseStructFieldsInFiles resolves structName in whichever of files
// declares it, trying each in order and returning the first match — the
// same "try every candidate file" shape resolveInSiblingTypesFiles applies
// to generated types files, applied here to hand-written controller files
// instead.
func parseStructFieldsInFiles(files []string, structName string) ([]FieldInfo, error) {
	for _, path := range files {
		if fields, err := ParseStructFields(path, structName); err == nil {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("no %s struct found in any of %d controller file(s)", structName, len(files))
}

// CheckServerEchoedExpectations scans m.Tests for entries whose effective
// expectation — "expect:" when present, else "value:" — omits, anywhere in
// its nested object tree, a member the resource's own controller declares
// server-echoed via a registered go-cmp Transformer (see
// discoverServerEchoedMembers). controllerDir is the resource's controller
// package directory; an empty controllerDir disables this check entirely
// (findings and error both nil) rather than guessing at a path, since a
// wrong-but-existing directory would silently validate against the wrong
// controller's normalizers.
//
// Unlike CheckIncompleteExpectations and CheckObservability, this check
// walks the FULL nested object tree of the effective expectation, not only
// its top level: the F5 XC httploadbalancer case this check exists for
// (examples/http-loadbalancer/http-loadbalancer.yaml, field
// "blockedClients") nests the echoed member three levels deep
// (blockedClients[].httpHeader.headers[].invertMatch), and a member cleared
// by a registered normalizer is, by construction, "omitempty" on the
// generated struct (the desired side never encodes the server's zero-value
// default explicitly) — the exact shape CheckIncompleteExpectations'
// non-omitempty filter excludes by design, and the exact shape
// CheckObservability has no way to flag since the member IS observable, it
// is only unsafe to omit from a value that already carries some of its
// siblings.
func CheckServerEchoedExpectations(typesPath string, paramFields []FieldInfo, m *manifest.Manifest, controllerDir string) ([]ServerEchoedExpectationFinding, error) {
	if controllerDir == "" {
		return nil, nil
	}

	members, err := discoverServerEchoedMembers(controllerDir)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	byJSON := make(map[string]FieldInfo, len(paramFields))
	for _, f := range paramFields {
		byJSON[f.JSONName] = f
	}

	cache := make(observationFieldCache)

	var findings []ServerEchoedExpectationFinding
	for _, t := range m.Tests {
		if t.Skip.Present() || strings.Contains(t.Field, ".") {
			continue
		}

		fi, ok := byJSON[t.Field]
		if !ok {
			continue
		}
		elemType, ok := structElemType(fi.GoType)
		if !ok {
			continue
		}

		effective := effectiveExpectation(t)
		if effective == nil {
			continue
		}

		found := make(map[string]bool)
		walkServerEchoedMembers(typesPath, elemType, effective, members, cache, found)
		if len(found) == 0 {
			continue
		}

		keys := make([]string, 0, len(found))
		for k := range found {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		findings = append(findings, ServerEchoedExpectationFinding{Field: t.Field, Keys: keys})
	}
	return findings, nil
}

// walkServerEchoedMembers recursively checks node — the effective
// expectation value at some point in the field's nested object tree — for a
// missing server-echoed member, resolving elemType's struct shape at each
// level and recursing into every present, locally-resolvable nested struct
// field. It never reports a false positive by construction: a member is
// only flagged when its OWN struct shape (the full Go field-name set
// resolved at that exact tree position) matches a declared server-echoed
// struct shape, so a same-named field belonging to a differently-shaped
// struct (e.g. an unrelated "Description" field on a struct the controller
// never normalizes) is never confused with one that is.
func walkServerEchoedMembers(typesPath, elemType string, node interface{}, members []ServerEchoedMember, cache observationFieldCache, found map[string]bool) {
	fields := resolveTypeFieldsForWalk(typesPath, elemType, cache)
	if fields == nil {
		return
	}

	echoed := matchServerEchoedMembers(fields, members)

	for _, obj := range asObjects(node) {
		for _, f := range fields {
			v, present := obj[f.JSONName]
			if !present {
				if echoed[f.GoName] {
					found[f.JSONName] = true
				}
				continue
			}
			if nestedElem, ok := structElemType(f.GoType); ok {
				walkServerEchoedMembers(typesPath, nestedElem, v, members, cache, found)
			}
		}
	}
}

// matchServerEchoedMembers returns the set of Go field names among
// candidateFields' own struct shape that a declared ServerEchoedMember
// names as its cleared field, for every member whose StructFieldNames is a
// subset of candidateFields' Go field-name set. Subset, not exact equality:
// the generated Observation-side struct is free to carry additional
// CRD-only fields the hand-written wire-shape struct the controller
// normalizes does not — the two are independently authored — so requiring
// every DECLARED field to be present, while tolerating extras, is the
// least-guessy relation that still rejects an unrelated same-named field
// (see the SchemaTlsCertificateType.Description / apiMessageMeta.Description
// case this check was measured against: TlsCertificateType lacks
// apiMessageMeta's "Name" field entirely, so it is never matched).
func matchServerEchoedMembers(candidateFields []FieldInfo, members []ServerEchoedMember) map[string]bool {
	candidateSet := make(map[string]bool, len(candidateFields))
	for _, f := range candidateFields {
		candidateSet[f.GoName] = true
	}

	echoed := make(map[string]bool)
	for _, mem := range members {
		matches := true
		for _, want := range mem.StructFieldNames {
			if !candidateSet[want] {
				matches = false
				break
			}
		}
		if matches {
			echoed[mem.EchoedField] = true
		}
	}
	return echoed
}

// resolveTypeFieldsForWalk resolves the fields for elemType as they would
// appear on the OBSERVED side of the runner's comparison: the generated
// "<elemType>Observation" companion when one exists (the same resolution
// resolveObservationFields performs for a top-level field whose Parameters-
// and Observation-side shapes differ, including its zz_*_types.go sibling
// search), and — when no companion exists anywhere searched — elemType
// itself, directly. The bare-name fallback is required, not optional: a
// mixed apis/ layout sometimes reuses the IDENTICAL struct for both sides of
// a field with no separate companion at all (confirmed on the motivating
// case: HttpLoadbalancerParameters.BlockedClients and its Observation
// counterpart both declare the literal same element type,
// CommonWafSimpleClientSrcRule, with no CommonWafSimpleClientSrcRuleObservation
// anywhere in the package), so without this fallback the walk could never
// reach past the first level for a field shaped that way.
func resolveTypeFieldsForWalk(typesPath, elemType string, cache observationFieldCache) []FieldInfo {
	if fields, cached := cache[elemType]; cached {
		return fields
	}

	fields := resolveObservationFields(typesPath, elemType, cache)
	if fields != nil {
		return fields
	}

	// resolveObservationFields already cached a nil miss for elemType
	// under the "<elemType>Observation" resolution above; overwrite it
	// once the bare-name fallback below settles the real answer.
	rawFields, err := ParseStructFields(typesPath, elemType)
	if err != nil {
		rawFields, err = resolveInSiblingTypesFiles(typesPath, elemType)
	}
	if err != nil {
		cache[elemType] = nil
		return nil
	}

	cache[elemType] = rawFields
	return rawFields
}

// PrintServerEchoedExpectations outputs any ServerEchoedExpectationFinding
// results to stdout, as its own labelled block — never conflated with
// MISSING, UNOBSERVABLE, SIBLING-SURVIVES, or INCOMPLETE-EXPECT, each of
// which names a structurally different reason the same-looking symptom (a
// field test that polls to timeout) can occur.
func PrintServerEchoedExpectations(findings []ServerEchoedExpectationFinding) {
	if len(findings) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Server-echoed expectations:")
	for _, f := range findings {
		fmt.Printf("  ✗ %s: SERVER-ECHOED — key(s) %s are cleared by a registered normalizer because the backend echoes them on every element regardless of what the caller set, and will marshal into atProvider whether or not the underlying field carries omitempty; add expect: naming the server-defaulted value\n",
			f.Field, strings.Join(f.Keys, ", "))
	}
	fmt.Println()
	fmt.Println("FAIL: some update-test expectations omit a member the provider's controller declares server-echoed.")
}
