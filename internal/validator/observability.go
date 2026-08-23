package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// ObservabilityFinding records a single update-test entry whose effective
// expectation names a top-level key that the resource's generated
// Observation struct provably excludes by construction — most commonly a
// *Ref/*Selector cross-resource reference field, which the generator's own
// doc comment states is "input-only and carries no observed state".
type ObservabilityFinding struct {
	// Field is the manifest field name from the update-test entry (the
	// "field:" value, matching a top-level JSON name in the Parameters
	// struct).
	Field string
	// Keys are the unobservable top-level key(s) found in the effective
	// expectation, sorted for deterministic output.
	Keys []string
}

// CheckObservability scans tests for entries whose effective expectation —
// "expect:" when present, else "value:", matching the runner's own
// precedence (runner.go's runFieldTest) — is an object, or a list of
// objects, naming a top-level key that the target field's generated
// Observation struct provably excludes.
//
// It is deliberately conservative: the value of this check is that it never
// cries wolf. The following are all left unflagged rather than guessed at,
// per the caller's request:
//   - a "skip:" entry
//   - a dotted field path (e.g. "cookieParams.authHmac.primKeySecretRef") —
//     resolving a nested field's own nested struct is out of scope
//   - a field the Parameters struct does not declare (already reported
//     MISSING elsewhere; not this check's job)
//   - a scalar expectation, or one whose declared Go type is not a locally
//     resolvable struct (a builtin, a map, or a cross-package type such as
//     "xpv2.NamespacedReference" this checker cannot inspect the shape of)
//   - a nested type whose Observation-side shape cannot be resolved at all
//     — neither its "<Type>Observation" companion struct, nor a field of
//     the same name declared on "<kind>Observation" itself (see
//     resolveObservationFieldsForKindField)
//
// A false positive here would push implementers toward blanket "skip:"
// entries, which is the outcome the coverage campaign this check supports
// exists to prevent.
func CheckObservability(typesPath, kind string, paramFields []FieldInfo, tests []manifest.UpdateTest) []ObservabilityFinding {
	byJSON := make(map[string]FieldInfo, len(paramFields))
	for _, f := range paramFields {
		byJSON[f.JSONName] = f
	}

	// Cache resolved Observation struct fields by element type, so a types
	// file with many entries referencing the same nested struct is only
	// parsed (and, on a miss, sibling-file-searched) once.
	obsCache := make(observationFieldCache)

	var findings []ObservabilityFinding
	for _, t := range tests {
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

		obsFields := resolveObservationFieldsForKindField(typesPath, kind, t.Field, elemType, obsCache)
		if obsFields == nil {
			continue
		}
		obsKeys := make(map[string]bool, len(obsFields))
		for _, f := range obsFields {
			obsKeys[f.JSONName] = true
		}

		effective := t.Expect
		if effective == nil {
			effective = t.Value
		}
		if effective == nil {
			continue
		}

		bad := unobservableKeys(effective, obsKeys)
		if len(bad) == 0 {
			continue
		}
		findings = append(findings, ObservabilityFinding{Field: t.Field, Keys: bad})
	}

	return findings
}

// observationFieldCache caches resolveObservationFields results by element
// type, shared across a single CheckObservability or
// CheckIncompleteExpectations call so a types file with many entries
// referencing the same nested struct is only parsed (and, on a miss,
// sibling-file-searched) once. A nil entry (present key, nil value) records
// "tried and unresolvable" so a repeated miss doesn't re-search either.
type observationFieldCache map[string][]FieldInfo

// resolveObservationFieldsForKindField resolves the Observation-side struct
// fields for a TOP-LEVEL Parameters field of kind, named jsonName, whose
// Parameters-side element type is elemType. It tries the name-mangled
// "<elemType>Observation" fast path first, via resolveObservationFields —
// correct and unchanged for a generator layout that emits a distinct
// Observation companion struct (e.g. ViewsOriginPoolWithWeightObservation).
//
// When that fails, it falls back to reading the ACTUAL declared Go type of
// the field named jsonName on "<kind>Observation" itself, and resolves
// THAT type's own fields directly — handling a generator layout that
// reuses the identical struct on both sides of a field with no separate
// Observation companion at all (provider-f5xc's PolicyMatcherTypeBasic,
// referenced unchanged from both ServicePolicyRuleParameters.DomainMatcher
// and ServicePolicyRuleObservation.DomainMatcher).
//
// Resolving via the field "<kind>Observation" ACTUALLY declares — rather
// than a blind "fall back to elemType when <elemType>Observation is
// missing" — is what lets a field genuinely ABSENT from "<kind>Observation"
// (never observed at all) stay unresolved instead of incorrectly matching a
// same-named Parameters-only struct that happens to still parse: a plain
// bare-name fallback cannot tell "reused" apart from "not observed at all",
// since both leave "<elemType>Observation" unresolvable.
func resolveObservationFieldsForKindField(typesPath, kind, jsonName, elemType string, cache observationFieldCache) []FieldInfo {
	if fields := resolveObservationFields(typesPath, elemType, cache); fields != nil {
		return fields
	}

	cacheKey := "field-reuse:" + kind + "." + jsonName
	if fields, cached := cache[cacheKey]; cached {
		return fields
	}

	declaredType, ok := resolveKindObservationFieldType(typesPath, kind, jsonName, cache)
	if !ok {
		cache[cacheKey] = nil
		return nil
	}

	fields, err := ParseStructFields(typesPath, declaredType)
	if err != nil {
		fields, err = resolveInSiblingTypesFiles(typesPath, declaredType)
	}
	if err != nil {
		cache[cacheKey] = nil
		return nil
	}

	cache[cacheKey] = fields
	return fields
}

// resolveKindObservationFieldType returns the raw declared struct type name
// (already unwrapped of its "[]"/"*" prefixes via structElemType, e.g.
// "PolicyMatcherTypeBasic") of the field named jsonName on
// "<kind>Observation", via resolveKindObservationFields. A false return
// means either "<kind>Observation" could not be resolved anywhere searched,
// it declares no field named jsonName, or that field's declared type is not
// a locally-resolvable struct — all three report as "cannot determine", the
// same conservative outcome the rest of this package holds to.
func resolveKindObservationFieldType(typesPath, kind, jsonName string, cache observationFieldCache) (string, bool) {
	for _, f := range resolveKindObservationFields(typesPath, kind, cache) {
		if f.JSONName == jsonName {
			return structElemType(f.GoType)
		}
	}
	return "", false
}

// resolveKindObservationFields returns the fields declared on
// "<kind>Observation" — the resource's own top-level Observation struct,
// not a nested one — resolved first in typesPath and, failing that, in every
// other non-test Go source file in the same directory, the same sibling-file
// fallback resolveObservationFields applies to a nested struct. Cached under
// a "kindobs:" key namespace that cannot collide with a plain elemType cache
// entry, since a locally-resolvable struct name never starts with a
// lowercase letter (see stripToStructName).
func resolveKindObservationFields(typesPath, kind string, cache observationFieldCache) []FieldInfo {
	cacheKey := "kindobs:" + kind
	if fields, cached := cache[cacheKey]; cached {
		return fields
	}

	structName := kind + "Observation"
	fields, err := ParseStructFields(typesPath, structName)
	if err != nil {
		fields, err = resolveInSiblingTypesFiles(typesPath, structName)
	}
	if err != nil {
		cache[cacheKey] = nil
		return nil
	}

	cache[cacheKey] = fields
	return fields
}

// resolveObservationFields returns the fields declared on
// "<elemType>Observation", resolved first in typesPath and, failing that, in
// every other non-test Go source file in the same directory — a MIXED flat
// apis/<scope>/v1alpha1/ layout sometimes de-duplicates a generated struct,
// declaring it once in one resource's types file and referencing it from a
// different resource's. A plain single-file lookup can never see that
// struct at all; this fallback does, while keeping the single-file lookup as
// the fast path. A nil return means the struct could not be resolved
// anywhere searched.
func resolveObservationFields(typesPath, elemType string, cache observationFieldCache) []FieldInfo {
	if fields, cached := cache[elemType]; cached {
		return fields
	}

	structName := elemType + "Observation"
	fields, err := ParseStructFields(typesPath, structName)
	if err != nil {
		fields, err = resolveInSiblingTypesFiles(typesPath, structName)
	}
	if err != nil {
		cache[elemType] = nil
		return nil
	}

	cache[elemType] = fields
	return fields
}

// resolveInSiblingTypesFiles searches every non-test Go source file in the
// same directory as typesPath (excluding typesPath itself — the caller
// already tried it) for structName, returning the first match. Go
// guarantees at most one declaration of a given name per package, so any
// non-"_test.go" ".go" file in the directory is an authoritative place to
// look — a filename convention (e.g. a "zz_"-prefixed or "_types.go"-suffixed
// glob) is narrower than that guarantee and misses declarations in
// differently-named sibling files (e.g. a package-level setup/marker file).
// "_test.go" is excluded because an external "_test" package may legally
// declare a same-named struct that is not part of the production API.
// Errors from the directory read itself, or no sibling declaring structName,
// both report the same "not found" outcome the caller already treats as
// unresolvable.
func resolveInSiblingTypesFiles(typesPath, structName string) ([]FieldInfo, error) {
	dir := filepath.Dir(typesPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s for %s siblings: %w", dir, structName, err)
	}

	self, err := filepath.Abs(typesPath)
	if err != nil {
		self = typesPath
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		sibling := filepath.Join(dir, name)
		if siblingAbs, absErr := filepath.Abs(sibling); absErr == nil && siblingAbs == self {
			continue // already tried by the caller
		}
		if fields, siblingErr := ParseStructFields(sibling, structName); siblingErr == nil {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("no %s struct found in %s or its sibling package files", structName, typesPath)
}

// unobservableKeys returns the sorted, deduplicated set of top-level keys
// across every object in effective (a single object, or a list of objects —
// non-object list elements are skipped) that are absent from obsKeys.
func unobservableKeys(effective interface{}, obsKeys map[string]bool) []string {
	bad := make(map[string]bool)
	for _, obj := range asObjects(effective) {
		for key := range obj {
			if !obsKeys[key] {
				bad[key] = true
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	out := make([]string, 0, len(bad))
	for k := range bad {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// asObjects normalises an effective expectation value into the list of
// top-level objects to check against an Observation struct's field set: a
// single object, or the objects found in a list (a list of scalars yields no
// objects to check, and is silently skipped rather than flagged).
func asObjects(v interface{}) []map[string]interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{val}
	case []interface{}:
		var out []map[string]interface{}
		for _, e := range val {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// structElemType strips the "[]" and "*" wrapper prefixes from a raw
// declared Go type and reports whether the remaining base name looks like a
// locally-defined struct this package can resolve by name in the same types
// file: an unqualified (no ".") name starting with an uppercase ASCII
// letter. Builtin scalars ("string", "bool", "int64", ...) start lowercase
// and so are correctly excluded; cross-package types ("xpv2.Reference",
// "metav1.Time") contain a "." and are Crossplane runtime/reference
// plumbing this checker has no way to inspect the shape of, so they are
// reported as unresolved rather than guessed at; "map[...]..." is excluded
// outright since a map's keys are data, not a fixed struct shape.
func structElemType(goType string) (string, bool) {
	t := strings.TrimSpace(goType)
	for {
		switch {
		case strings.HasPrefix(t, "[]"):
			t = t[2:]
			continue
		case strings.HasPrefix(t, "*"):
			t = t[1:]
			continue
		}
		break
	}
	return stripToStructName(t)
}

// stripToStructName validates a fully-unwrapped type name (no remaining "[]"
// or "*" prefix) as a locally-resolvable struct name.
func stripToStructName(t string) (string, bool) {
	if t == "" || strings.Contains(t, ".") || strings.HasPrefix(t, "map[") {
		return "", false
	}
	if t[0] < 'A' || t[0] > 'Z' {
		return "", false
	}
	return t, true
}
