package validator

import (
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
//   - a nested type whose "<Type>Observation" companion struct cannot be
//     found in typesPath
//
// A false positive here would push implementers toward blanket "skip:"
// entries, which is the outcome the coverage campaign this check supports
// exists to prevent.
func CheckObservability(typesPath string, paramFields []FieldInfo, tests []manifest.UpdateTest) []ObservabilityFinding {
	byJSON := make(map[string]FieldInfo, len(paramFields))
	for _, f := range paramFields {
		byJSON[f.JSONName] = f
	}

	// Cache resolved Observation struct field-name sets by element type, so
	// a types file with many entries referencing the same nested struct is
	// only parsed once. A nil entry (present key, nil value) records "tried
	// and unresolvable" so a repeated miss doesn't re-parse either.
	obsCache := make(map[string]map[string]bool)

	var findings []ObservabilityFinding
	for _, t := range tests {
		if t.Skip != "" || strings.Contains(t.Field, ".") {
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

		obsKeys := resolveObservationKeys(typesPath, elemType, obsCache)
		if obsKeys == nil {
			continue
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

// resolveObservationKeys returns the set of top-level JSON field names
// declared on "<elemType>Observation" in typesPath, consulting/populating
// cache so each element type is parsed at most once. A nil return means the
// struct could not be resolved.
func resolveObservationKeys(typesPath, elemType string, cache map[string]map[string]bool) map[string]bool {
	if keys, cached := cache[elemType]; cached {
		return keys
	}

	fields, err := ParseStructFields(typesPath, elemType+"Observation")
	if err != nil {
		cache[elemType] = nil
		return nil
	}

	keys := make(map[string]bool, len(fields))
	for _, f := range fields {
		keys[f.JSONName] = true
	}
	cache[elemType] = keys
	return keys
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
