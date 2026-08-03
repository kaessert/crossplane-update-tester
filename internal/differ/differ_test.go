// differ_test.go covers the top-level snapshot comparison in differ.go: which
// fields are reported as changed between a before/after snapshot, which are
// deliberately suppressed (the target field, an explicitly excluded field, or
// any field whose value did not change), how absent and unpopulated snapshots
// are interpreted, and how the result is ordered and rendered.
package differ

import (
	"encoding/json"
	"strings"
	"testing"
)

// assertChanges compares a diff result against the expected changes, field
// name and both values, in order. Order is part of the contract: the rendered
// output lands in logs and diffs, so it must be reproducible.
func assertChanges(t *testing.T, got, want []FieldChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d changes %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiffSnapshots(t *testing.T) {
	cases := map[string]struct {
		reason        string
		before, after []byte
		target        string
		want          []FieldChange
	}{
		"IdenticalSnapshots": {
			reason: "Nothing moved, so nothing should be reported.",
			before: []byte(`{"alpha":"one","beta":2}`),
			after:  []byte(`{"alpha":"one","beta":2}`),
			target: "alpha",
			want:   nil,
		},
		"BothSnapshotsEmptyObjects": {
			reason: "Two empty objects have no keys to compare.",
			before: []byte(`{}`),
			after:  []byte(`{}`),
			target: "alpha",
			want:   nil,
		},
		"FieldChanged": {
			reason: "A differing value on a non-target field is the side effect the differ exists to catch.",
			before: []byte(`{"alpha":"one","beta":2}`),
			after:  []byte(`{"alpha":"one","beta":3}`),
			target: "alpha",
			want:   []FieldChange{{Field: "beta", OldValue: "2", NewValue: "3"}},
		},
		"FieldAdded": {
			reason: "A key present only in after is a change; its old value is the empty string because absence is not a JSON value.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte(`{"alpha":"one","gamma":"new"}`),
			target: "alpha",
			want:   []FieldChange{{Field: "gamma", OldValue: "", NewValue: `"new"`}},
		},
		"FieldRemoved": {
			reason: "A key present only in before is a change, reported with an empty new value.",
			before: []byte(`{"alpha":"one","gamma":"gone"}`),
			after:  []byte(`{"alpha":"one"}`),
			target: "alpha",
			want:   []FieldChange{{Field: "gamma", OldValue: `"gone"`, NewValue: ""}},
		},
		"FieldSetToJSONNull": {
			reason: "An explicit null is a value, distinct from an absent key, and differs from both a set value and absence.",
			before: []byte(`{"alpha":"one","gamma":"set"}`),
			after:  []byte(`{"alpha":"one","gamma":null}`),
			target: "alpha",
			want:   []FieldChange{{Field: "gamma", OldValue: `"set"`, NewValue: "null"}},
		},
		"TargetFieldChangedIsExcluded": {
			reason: "The target field is the field under test; its change is the expected outcome, not a side effect.",
			before: []byte(`{"alpha":"one","beta":2}`),
			after:  []byte(`{"alpha":"two","beta":2}`),
			target: "alpha",
			want:   nil,
		},
		"TargetFieldExcludedButOthersStillCaught": {
			reason: "Excluding the target must not suppress unrelated fields that moved at the same time.",
			before: []byte(`{"alpha":"one","beta":2,"gamma":"x"}`),
			after:  []byte(`{"alpha":"two","beta":9,"gamma":"y"}`),
			target: "alpha",
			want: []FieldChange{
				{Field: "beta", OldValue: "2", NewValue: "9"},
				{Field: "gamma", OldValue: `"x"`, NewValue: `"y"`},
			},
		},
		"TargetFieldAbsentFromBothSnapshots": {
			reason: "An exclusion that matches no key is harmless and suppresses nothing.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte(`{"alpha":"two"}`),
			target: "nosuchfield",
			want:   []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: `"two"`}},
		},
		"EmptyBeforeTreatedAsEmptyObject": {
			reason: "A resource whose status is not populated yet is a valid before state; every field then reads as added.",
			before: []byte(``),
			after:  []byte(`{"alpha":"one"}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: "", NewValue: `"one"`}},
		},
		"NullBeforeTreatedAsEmptyObject": {
			reason: "A JSON null snapshot is the serialised form of the same unpopulated state and must not be an error.",
			before: []byte(`null`),
			after:  []byte(`{"alpha":"one"}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: "", NewValue: `"one"`}},
		},
		"NullAfterTreatedAsEmptyObject": {
			reason: "The empty/null tolerance is symmetric across both sides.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte(`null`),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: ""}},
		},
		"EmptyAfterTreatedAsEmptyObject": {
			reason: "An empty after snapshot reads as every field having been removed.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte(``),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: ""}},
		},
		"BothSidesEmpty": {
			reason: "Two unpopulated snapshots are equal, not an error.",
			before: []byte(``),
			after:  []byte(``),
			target: "unrelated",
			want:   nil,
		},
		"BothSidesNull": {
			reason: "Two null snapshots are equal, not an error.",
			before: []byte(`null`),
			after:  []byte(`null`),
			target: "unrelated",
			want:   nil,
		},
		"EmptyBeforeAndNullAfter": {
			reason: "Empty and null denote the same unpopulated state, so mixing them yields no changes.",
			before: []byte(``),
			after:  []byte(`null`),
			target: "unrelated",
			want:   nil,
		},
		"WhitespaceOnlyBeforeTreatedAsEmptyObject": {
			reason: "Snapshots captured from command output carry a trailing newline; a blank capture is the unpopulated state, not malformed input.",
			before: []byte("\n"),
			after:  []byte(`{"alpha":"one"}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: "", NewValue: `"one"`}},
		},
		"WhitespaceOnlyAfterTreatedAsEmptyObject": {
			reason: "Same tolerance on the after side.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte("   \t\n"),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: ""}},
		},
		"NullWithTrailingNewlineTreatedAsEmptyObject": {
			reason: "A null capture from a command arrives as \"null\\n\" and must be read as unpopulated.",
			before: []byte("null\n"),
			after:  []byte(`{"alpha":"one"}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "alpha", OldValue: "", NewValue: `"one"`}},
		},
		"SurroundingWhitespaceAroundObject": {
			reason: "Padding around an otherwise valid object is insignificant and must not register as a change.",
			before: []byte("  {\"alpha\":\"one\"}\n"),
			after:  []byte(`{"alpha":"one"}`),
			target: "unrelated",
			want:   nil,
		},
		"NestedValueReformattedOnly": {
			reason: "Insignificant whitespace inside a nested value is not a change; comparing raw bytes would report a false side effect.",
			before: []byte(`{"nested": {"x":1, "y":2}}`),
			after:  []byte(`{"nested":{"x":1,"y":2}}`),
			target: "unrelated",
			want:   nil,
		},
		"NestedValueGenuinelyChanged": {
			reason: "Formatting tolerance must not hide a real change inside a nested value; reported values are canonical.",
			before: []byte(`{"nested": {"x":1, "y":2}}`),
			after:  []byte(`{"nested": {"x":1, "y":3}}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "nested", OldValue: `{"x":1,"y":2}`, NewValue: `{"x":1,"y":3}`}},
		},
		"ArrayValueChanged": {
			reason: "List-valued fields are compared like any other top-level value.",
			before: []byte(`{"items":["a","b"]}`),
			after:  []byte(`{"items":["a","c"]}`),
			target: "unrelated",
			want:   []FieldChange{{Field: "items", OldValue: `["a","b"]`, NewValue: `["a","c"]`}},
		},
		"NestedChangeUnderTargetFieldStillExcluded": {
			reason: "Exclusion is by top-level key, so any change beneath the target is suppressed with it.",
			before: []byte(`{"alpha":{"deep":1},"beta":2}`),
			after:  []byte(`{"alpha":{"deep":99},"beta":2}`),
			target: "alpha",
			want:   nil,
		},
		"EmptyTargetFieldExcludesNothing": {
			reason: "An empty target name cannot match a real key, so all changes are reported.",
			before: []byte(`{"alpha":"one"}`),
			after:  []byte(`{"alpha":"two"}`),
			target: "",
			want:   []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: `"two"`}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DiffSnapshots(tc.before, tc.after, tc.target)
			if err != nil {
				t.Fatalf("DiffSnapshots() unexpected error: %v\nreason: %s", err, tc.reason)
			}
			assertChanges(t, got, tc.want)
		})
	}
}

// TestDiffSnapshotsOrdering pins the sort: results are ordered by field name
// regardless of map iteration order, so the rendered summary is byte-identical
// across runs and a reviewer's diff shows only real movement.
func TestDiffSnapshotsOrdering(t *testing.T) {
	cases := map[string]struct {
		reason        string
		before, after []byte
		want          []string
	}{
		"KeysOutOfOrderInDocument": {
			reason: "Document order must not leak into the result.",
			before: []byte(`{"zulu":1,"alpha":1,"mike":1}`),
			after:  []byte(`{"zulu":2,"alpha":2,"mike":2}`),
			want:   []string{"alpha", "mike", "zulu"},
		},
		"MixOfAddedRemovedAndChanged": {
			reason: "Added, removed and changed fields share one sorted sequence rather than being grouped by kind.",
			before: []byte(`{"delta":1,"bravo":1}`),
			after:  []byte(`{"bravo":2,"charlie":1}`),
			want:   []string{"bravo", "charlie", "delta"},
		},
		"SortIsCaseSensitiveByteOrder": {
			reason: "Ordering follows plain byte comparison, so uppercase names sort before lowercase.",
			before: []byte(`{"beta":1,"Beta":1,"alpha":1}`),
			after:  []byte(`{"beta":2,"Beta":2,"alpha":2}`),
			want:   []string{"Beta", "alpha", "beta"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Repeat: Go randomises map iteration, so a single pass can
			// pass by luck against an unsorted implementation.
			for i := 0; i < 20; i++ {
				got, err := DiffSnapshots(tc.before, tc.after, "unrelated")
				if err != nil {
					t.Fatalf("DiffSnapshots() unexpected error: %v\nreason: %s", err, tc.reason)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("got %d changes %+v, want fields %v", len(got), got, tc.want)
				}
				for j, field := range tc.want {
					if got[j].Field != field {
						t.Fatalf("iteration %d: changes[%d].Field = %q, want %q (full: %+v)\nreason: %s",
							i, j, got[j].Field, field, got, tc.reason)
					}
				}
			}
		})
	}
}

// TestDiffSnapshotsMalformedInput asserts that unparseable input is surfaced
// as an error rather than being downgraded to an empty object. Silently
// treating a broken capture as empty would turn it into a confident and wrong
// "no changes" verdict.
func TestDiffSnapshotsMalformedInput(t *testing.T) {
	valid := []byte(`{"alpha":"one"}`)

	cases := map[string]struct {
		reason        string
		before, after []byte
		wantSide      string
	}{
		"TruncatedObjectBefore": {
			reason:   "An unterminated object is malformed, not empty.",
			before:   []byte(`{"alpha":`),
			after:    valid,
			wantSide: "before",
		},
		"TruncatedObjectAfter": {
			reason:   "The same applies to the after side.",
			before:   valid,
			after:    []byte(`{"alpha":`),
			wantSide: "after",
		},
		"NotJSONBefore": {
			reason:   "Plain text captured instead of JSON must be reported.",
			before:   []byte(`not json at all`),
			after:    valid,
			wantSide: "before",
		},
		"NotJSONAfter": {
			reason:   "Plain text on the after side must be reported.",
			before:   valid,
			after:    []byte(`not json at all`),
			wantSide: "after",
		},
		"ArrayInsteadOfObjectBefore": {
			reason:   "A top-level array is well-formed JSON but not a snapshot object.",
			before:   []byte(`[1,2,3]`),
			after:    valid,
			wantSide: "before",
		},
		"ScalarInsteadOfObjectAfter": {
			reason:   "A bare scalar is not a snapshot object either, and unlike null carries no unpopulated meaning.",
			before:   valid,
			after:    []byte(`42`),
			wantSide: "after",
		},
		"NullLookalikeBefore": {
			reason:   "A token that merely starts like null is malformed and must not slip through the unpopulated shortcut.",
			before:   []byte(`nullish`),
			after:    valid,
			wantSide: "before",
		},
		"BothSidesMalformedReportsBeforeFirst": {
			reason:   "The before side is parsed first, so it is the side named when both are broken.",
			before:   []byte(`{oops`),
			after:    []byte(`{oops`),
			wantSide: "before",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DiffSnapshots(tc.before, tc.after, "alpha")
			if err == nil {
				t.Fatalf("DiffSnapshots() error = nil, want error; got changes %+v\nreason: %s", got, tc.reason)
			}
			if got != nil {
				t.Errorf("DiffSnapshots() changes = %+v, want nil alongside the error", got)
			}
			if want := "parsing " + tc.wantSide + " snapshot"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not identify the offending side, want it to contain %q", err, want)
			}
		})
	}

	t.Run("ExcludingReportsMalformedInputToo", func(t *testing.T) {
		// The exclude-list entry point shares the same parser and must not
		// have a softer failure mode.
		if _, err := DiffSnapshotsExcluding([]byte(`{oops`), valid, []string{"alpha"}); err == nil {
			t.Fatal("DiffSnapshotsExcluding() error = nil, want error for malformed before snapshot")
		}
	})
}

func TestDiffSnapshotsExcluding(t *testing.T) {
	before := []byte(`{"alpha":1,"beta":1,"gamma":1}`)
	after := []byte(`{"alpha":2,"beta":2,"gamma":2}`)

	cases := map[string]struct {
		reason        string
		before, after []byte
		exclude       []string
		want          []FieldChange
	}{
		"NilExcludeList": {
			reason:  "With nothing excluded every changed field is reported.",
			before:  before,
			after:   after,
			exclude: nil,
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "beta", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"EmptyExcludeList": {
			reason:  "An empty slice behaves the same as no slice at all.",
			before:  before,
			after:   after,
			exclude: []string{},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "beta", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"SingleFieldExcluded": {
			reason:  "A named dynamic field is suppressed while the rest are still asserted.",
			before:  before,
			after:   after,
			exclude: []string{"beta"},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"SeveralFieldsExcluded": {
			reason:  "Every entry in the list is applied, not just the first.",
			before:  before,
			after:   after,
			exclude: []string{"alpha", "gamma"},
			want:    []FieldChange{{Field: "beta", OldValue: "1", NewValue: "2"}},
		},
		"AllFieldsExcluded": {
			reason:  "Excluding everything leaves nothing to report.",
			before:  before,
			after:   after,
			exclude: []string{"alpha", "beta", "gamma"},
			want:    nil,
		},
		"NamesSurroundedByWhitespace": {
			reason:  "The list is typically split from a comma-separated flag, so entries arrive padded and must be trimmed.",
			before:  before,
			after:   after,
			exclude: []string{"  alpha", "gamma  ", "\tbeta\n"},
			want:    nil,
		},
		"EmptyAndBlankEntriesIgnored": {
			reason:  "A trailing comma or a doubled separator yields blank entries; they must not become an exclusion for the empty key or discard the real ones.",
			before:  before,
			after:   after,
			exclude: []string{"", "  ", "beta", "\t\n", ""},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"OnlyBlankEntries": {
			reason:  "A list of nothing but blanks excludes nothing at all.",
			before:  before,
			after:   after,
			exclude: []string{"", "   ", "\t"},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "beta", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"DuplicateEntries": {
			reason:  "Repeating a name is harmless.",
			before:  before,
			after:   after,
			exclude: []string{"beta", "beta", " beta "},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"UnknownFieldExcluded": {
			reason:  "Naming a field that is present in neither snapshot suppresses nothing.",
			before:  before,
			after:   after,
			exclude: []string{"nosuchfield"},
			want: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "beta", OldValue: "1", NewValue: "2"},
				{Field: "gamma", OldValue: "1", NewValue: "2"},
			},
		},
		"MatchIsExactNotPrefix": {
			reason:  "Exclusion matches whole key names; a field sharing a prefix with an excluded one is still reported.",
			before:  []byte(`{"alpha":1,"alphabet":1}`),
			after:   []byte(`{"alpha":2,"alphabet":2}`),
			exclude: []string{"alpha"},
			want:    []FieldChange{{Field: "alphabet", OldValue: "1", NewValue: "2"}},
		},
		"ExcludedFieldAddedRatherThanChanged": {
			reason:  "Exclusion also covers appearance and disappearance of the field, not only value changes.",
			before:  []byte(`{"alpha":1}`),
			after:   []byte(`{"alpha":1,"counter":7}`),
			exclude: []string{"counter"},
			want:    nil,
		},
		"UnpopulatedBeforeWithExclusions": {
			reason:  "The unpopulated-snapshot tolerance and the exclude list compose.",
			before:  []byte(`null`),
			after:   []byte(`{"alpha":1,"counter":7}`),
			exclude: []string{"counter"},
			want:    []FieldChange{{Field: "alpha", OldValue: "", NewValue: "1"}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DiffSnapshotsExcluding(tc.before, tc.after, tc.exclude)
			if err != nil {
				t.Fatalf("DiffSnapshotsExcluding() unexpected error: %v\nreason: %s", err, tc.reason)
			}
			assertChanges(t, got, tc.want)
		})
	}
}

func TestFormatChanges(t *testing.T) {
	cases := map[string]struct {
		reason  string
		changes []FieldChange
		want    string
	}{
		"NilChanges": {
			reason:  "A nil result is the success case and gets the stable-state message.",
			changes: nil,
			want:    "all non-target fields stable ✓",
		},
		"EmptyChanges": {
			reason:  "An allocated but empty slice is the same success case.",
			changes: []FieldChange{},
			want:    "all non-target fields stable ✓",
		},
		"SingleChange": {
			reason:  "One change renders as field, old value, new value with no separator noise.",
			changes: []FieldChange{{Field: "alpha", OldValue: `"one"`, NewValue: `"two"`}},
			want:    `alpha: "one" → "two"`,
		},
		"SeveralChanges": {
			reason: "Multiple changes are comma-separated in the order given, which the differ already sorted.",
			changes: []FieldChange{
				{Field: "alpha", OldValue: "1", NewValue: "2"},
				{Field: "beta", OldValue: `"x"`, NewValue: `"y"`},
				{Field: "gamma", OldValue: "true", NewValue: "false"},
			},
			want: `alpha: 1 → 2, beta: "x" → "y", gamma: true → false`,
		},
		"AddedFieldRendersEmptyOldValue": {
			reason:  "An added field has no old value; the gap in the output is what signals that.",
			changes: []FieldChange{{Field: "gamma", OldValue: "", NewValue: `"new"`}},
			want:    `gamma:  → "new"`,
		},
		"RemovedFieldRendersEmptyNewValue": {
			reason:  "A removed field renders with a trailing gap for the same reason.",
			changes: []FieldChange{{Field: "gamma", OldValue: `"gone"`, NewValue: ""}},
			want:    `gamma: "gone" → `,
		},
		"StructuredValues": {
			reason: "Object and array values are rendered as their compacted JSON.",
			changes: []FieldChange{
				{Field: "items", OldValue: `["a"]`, NewValue: `["a","b"]`},
				{Field: "nested", OldValue: `{"x":1}`, NewValue: `{"x":2}`},
			},
			want: `items: ["a"] → ["a","b"], nested: {"x":1} → {"x":2}`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FormatChanges(tc.changes); got != tc.want {
				t.Errorf("FormatChanges() = %q, want %q\nreason: %s", got, tc.want, tc.reason)
			}
		})
	}
}

// TestRawValue exercises the value normaliser directly. It covers the two
// contracts the comparison rests on — absence reads as the empty string, and
// equal values normalise to equal strings — plus the defensive fallback the
// public entry points cannot reach, since every value they hand it came from
// a successful unmarshal and is valid JSON by construction.
func TestRawValue(t *testing.T) {
	cases := map[string]struct {
		reason string
		in     map[string]json.RawMessage
		key    string
		want   string
	}{
		"AbsentKey": {
			reason: "A missing key reads as the empty string, which no valid JSON value can produce, so absence stays distinguishable from any real value.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage(`1`)},
			key:    "beta",
			want:   "",
		},
		"NilMap": {
			reason: "A nil map is a valid unpopulated snapshot and every lookup in it is an absence.",
			in:     nil,
			key:    "alpha",
			want:   "",
		},
		"ScalarLeftIntact": {
			reason: "A scalar has no insignificant whitespace to remove.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage(`"one"`)},
			key:    "alpha",
			want:   `"one"`,
		},
		"ExplicitNullIsAValue": {
			reason: "A stored null normalises to the literal, not to the empty string that marks absence.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage(`null`)},
			key:    "alpha",
			want:   "null",
		},
		"StructuredValueCompacted": {
			reason: "Insignificant whitespace is stripped so two formattings of one value compare equal.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage("{\n  \"x\": 1,\n  \"y\": [1, 2]\n}")},
			key:    "alpha",
			want:   `{"x":1,"y":[1,2]}`,
		},
		"WhitespaceInsideStringPreserved": {
			reason: "Compaction must not reach inside string literals; spaces there are part of the value.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage(`{"x": "a  b"}`)},
			key:    "alpha",
			want:   `{"x":"a  b"}`,
		},
		"InvalidRawFallsBackToLiteralBytes": {
			reason: "If a value ever cannot be compacted, comparing its raw bytes is still better than dropping it; unreachable via the public API but the branch must behave.",
			in:     map[string]json.RawMessage{"alpha": json.RawMessage(`{oops`)},
			key:    "alpha",
			want:   `{oops`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := rawValue(tc.in, tc.key); got != tc.want {
				t.Errorf("rawValue() = %q, want %q\nreason: %s", got, tc.want, tc.reason)
			}
		})
	}
}

// TestFormatChangesFromDiff checks the two halves end to end: what the differ
// produces is what gets rendered, including the sorted order.
func TestFormatChangesFromDiff(t *testing.T) {
	cases := map[string]struct {
		reason        string
		before, after []byte
		target        string
		want          string
	}{
		"StableResource": {
			reason: "Only the target field moved, which is the passing outcome.",
			before: []byte(`{"alpha":"one","beta":2}`),
			after:  []byte(`{"alpha":"two","beta":2}`),
			target: "alpha",
			want:   "all non-target fields stable ✓",
		},
		"SideEffectsRenderedInSortedOrder": {
			reason: "Rendered output follows field-name order regardless of document order.",
			before: []byte(`{"zulu":1,"alpha":1,"target":"x"}`),
			after:  []byte(`{"zulu":2,"alpha":2,"target":"y"}`),
			target: "target",
			want:   "alpha: 1 → 2, zulu: 1 → 2",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			changes, err := DiffSnapshots(tc.before, tc.after, tc.target)
			if err != nil {
				t.Fatalf("DiffSnapshots() unexpected error: %v\nreason: %s", err, tc.reason)
			}
			if got := FormatChanges(changes); got != tc.want {
				t.Errorf("FormatChanges() = %q, want %q\nreason: %s", got, tc.want, tc.reason)
			}
		})
	}
}
