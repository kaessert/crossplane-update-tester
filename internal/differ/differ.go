package differ

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FieldChange records a changed field between two snapshots.
type FieldChange struct {
	Field    string
	OldValue string
	NewValue string
}

// DiffSnapshots compares two JSON object snapshots (top-level keys only) and
// returns fields that changed, excluding the specified target field.
func DiffSnapshots(before, after []byte, targetField string) ([]FieldChange, error) {
	return diffTopLevel(before, after, map[string]bool{targetField: true})
}

// DiffSnapshotsExcluding compares two JSON object snapshots (top-level keys
// only) and returns fields that changed, excluding any field named in
// exclude. Used by the post-create convergence check to allow known-dynamic
// fields (timestamps, counters) to be excluded from the stability assertion
// via --ignore-fields.
func DiffSnapshotsExcluding(before, after []byte, exclude []string) ([]FieldChange, error) {
	excludeSet := make(map[string]bool, len(exclude))
	for _, f := range exclude {
		f = strings.TrimSpace(f)
		if f != "" {
			excludeSet[f] = true
		}
	}
	return diffTopLevel(before, after, excludeSet)
}

// diffTopLevel compares two JSON object snapshots at the top level, skipping
// any field present in exclude.
func diffTopLevel(before, after []byte, exclude map[string]bool) ([]FieldChange, error) {
	beforeMap, err := parseSnapshot(before, "before")
	if err != nil {
		return nil, err
	}
	afterMap, err := parseSnapshot(after, "after")
	if err != nil {
		return nil, err
	}

	// Collect all keys from both maps.
	keys := make(map[string]bool)
	for k := range beforeMap {
		keys[k] = true
	}
	for k := range afterMap {
		keys[k] = true
	}

	var changes []FieldChange
	for k := range keys {
		if exclude[k] {
			continue
		}
		oldVal := rawValue(beforeMap, k)
		newVal := rawValue(afterMap, k)
		if oldVal != newVal {
			changes = append(changes, FieldChange{
				Field:    k,
				OldValue: oldVal,
				NewValue: newVal,
			})
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Field < changes[j].Field
	})
	return changes, nil
}

// parseSnapshot unmarshals one snapshot into its top-level keys.
//
// An empty snapshot, or one holding only JSON null, is deliberately treated
// as an empty object rather than an error: a resource whose status has not
// been populated yet is a legitimate "before" state, and every one of its
// fields should then be reported as added. Surrounding whitespace is trimmed
// before that decision is made, because snapshots are routinely captured from
// command output and arrive with a trailing newline — an unpopulated capture
// is "\n" or "null\n" as often as it is "" or "null".
//
// Anything else that fails to parse is a real error and is returned as one;
// malformed input is never silently downgraded to an empty object, which
// would turn a broken capture into a clean "no changes" result.
func parseSnapshot(data []byte, label string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("parsing %s snapshot: %w", label, err)
	}
	return m, nil
}

// rawValue returns the comparable string form of the value stored under key,
// or the empty string when the key is absent. Absence is unambiguous: no
// valid JSON value encodes to zero bytes.
//
// The value is compacted first. json.RawMessage preserves the exact bytes of
// the original document, including insignificant whitespace inside objects
// and arrays, so comparing raw messages would report `{"a":1, "b":2}` and
// `{"a":1,"b":2}` as a change. Compacting makes the comparison insensitive to
// formatting and also keeps the reported values canonical.
func rawValue(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// FormatChanges returns a human-readable summary of field changes.
func FormatChanges(changes []FieldChange) string {
	if len(changes) == 0 {
		return "all non-target fields stable ✓"
	}
	var lines []string
	for _, c := range changes {
		lines = append(lines, fmt.Sprintf("%s: %s → %s", c.Field, c.OldValue, c.NewValue))
	}
	return strings.Join(lines, ", ")
}
