package runner

import (
	"testing"
)

// TestReadSnapshotField covers navigation of a Runner.Snapshot() result: a
// plain scalar, a nested dot-separated path, a missing field (empty value,
// exists=false, no error), a present-but-empty field (empty value,
// exists=true — the case a bare value comparison cannot tell apart from
// missing), and a snapshot that is not even a JSON object.
func TestReadSnapshotField(t *testing.T) {
	cases := map[string]struct {
		reason     string
		snapshot   string
		field      string
		want       string
		wantExists bool
		wantErr    bool
	}{
		"TopLevelScalar": {
			reason:     "a plain top-level field is stringified the same way ReadField would render it",
			snapshot:   `{"comment":"hello"}`,
			field:      "comment",
			want:       "hello",
			wantExists: true,
		},
		"NestedField": {
			reason:     "a dot-separated path descends through nested objects",
			snapshot:   `{"ruleChoice":{"legacyRuleList":{"rules":["a"]}}}`,
			field:      "ruleChoice.legacyRuleList",
			want:       `{"rules":["a"]}`,
			wantExists: true,
		},
		"MissingField": {
			reason:     "a missing field reads as an empty string with exists=false, not an error — mirrors ReadField/navigateJSONPath",
			snapshot:   `{"comment":"hello"}`,
			field:      "absent",
			want:       "",
			wantExists: false,
		},
		"PresentButEmptyField": {
			reason:     "a field present with an empty-string value reads as exists=true — distinguishable from MissingField even though both stringify to \"\"",
			snapshot:   `{"comment":""}`,
			field:      "comment",
			want:       "",
			wantExists: true,
		},
		"PresentButNullField": {
			reason:     "a field present with a JSON null value also reads as exists=true",
			snapshot:   `{"comment":null}`,
			field:      "comment",
			want:       "",
			wantExists: true,
		},
		"MalformedSnapshot": {
			reason:   "a snapshot that fails to parse as JSON is an error, not a silently empty read",
			snapshot: "not json",
			field:    "comment",
			wantErr:  true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, exists, err := readSnapshotField([]byte(tc.snapshot), tc.field)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: readSnapshotField() error = nil, want an error", tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: readSnapshotField() unexpected error: %v", tc.reason, err)
			}
			if got != tc.want {
				t.Errorf("%s: readSnapshotField() = %q, want %q", tc.reason, got, tc.want)
			}
			if exists != tc.wantExists {
				t.Errorf("%s: readSnapshotField() exists = %v, want %v", tc.reason, exists, tc.wantExists)
			}
		})
	}
}

// TestReadAssertUnchangedBaselines covers the empty-list short-circuit (no
// snapshot read at all) and reading several fields' baseline values in one
// pass.
func TestReadAssertUnchangedBaselines(t *testing.T) {
	snapshot := []byte(`{"legacyRuleList":["a"],"otherField":"x"}`)

	t.Run("EmptyFieldListReturnsNilWithoutError", func(t *testing.T) {
		got, err := readAssertUnchangedBaselines(snapshot, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %#v, want nil", got)
		}
	})

	t.Run("ReadsEveryDeclaredField", func(t *testing.T) {
		got, err := readAssertUnchangedBaselines(snapshot, []string{"legacyRuleList", "otherField"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"legacyRuleList": `["a"]`, "otherField": "x"}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("baselines[%q] = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("MalformedSnapshotIsAnError", func(t *testing.T) {
		if _, err := readAssertUnchangedBaselines([]byte("not json"), []string{"legacyRuleList"}); err == nil {
			t.Fatal("expected an error reading the baseline from a malformed snapshot")
		}
	})

	// A field path that does not resolve on the object used to read as an
	// empty-string baseline (indistinguishable from a legitimately empty
	// field, and indistinguishable from every later observation of the
	// same absent path), so the assertion could never fail no matter what
	// the backend actually did. It must be rejected up front, before any
	// field test runs.
	t.Run("UnresolvablePathIsAnError", func(t *testing.T) {
		_, err := readAssertUnchangedBaselines(snapshot, []string{"ruleChoice.legacyRuleList"})
		if err == nil {
			t.Fatal("expected an error for a field path that does not exist on the snapshot, got nil")
		}
	})

	// A field that is genuinely present but happens to hold an empty-string
	// value is a different case from UnresolvablePathIsAnError above: the
	// path resolves, the value is simply empty. It must be accepted as a
	// legitimate baseline, not rejected as "no such field" — rejecting it
	// would turn every guarded field that happens to start empty into a
	// false positive.
	t.Run("PresentButEmptyFieldIsAcceptedAsABaseline", func(t *testing.T) {
		empty := []byte(`{"legacyRuleList":"","otherField":"x"}`)
		got, err := readAssertUnchangedBaselines(empty, []string{"legacyRuleList"})
		if err != nil {
			t.Fatalf("unexpected error for a present-but-empty field: %v", err)
		}
		if v, ok := got["legacyRuleList"]; !ok || v != "" {
			t.Errorf("baselines[%q] = %q (present=%v), want \"\" (present=true)", "legacyRuleList", v, ok)
		}
	})

	// Accepting an empty baseline is only useful if it still gates: a field
	// that starts empty and later moves off empty is exactly the silent
	// drift assert-unchanged exists to catch. This exercises the baseline
	// reader and checkAssertUnchanged together, the same composition the
	// runner drives in practice.
	t.Run("EmptyBaselineStillGatesWhenFieldMovesOffEmpty", func(t *testing.T) {
		empty := []byte(`{"legacyRuleList":"","otherField":"x"}`)
		baselines, err := readAssertUnchangedBaselines(empty, []string{"legacyRuleList"})
		if err != nil {
			t.Fatalf("unexpected error reading baseline: %v", err)
		}

		drifted := []byte(`{"legacyRuleList":"rule-x","otherField":"x"}`)
		violated := map[string]bool{}
		got, err := checkAssertUnchanged(drifted, []string{"legacyRuleList"}, baselines, violated, "algo")
		if err != nil {
			t.Fatalf("unexpected error checking drift: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d violations, want 1: %+v", len(got), got)
		}
		v := got[0]
		if v.Field != "legacyRuleList" || v.Baseline != "" || v.Observed != "rule-x" || v.AfterField != "algo" {
			t.Errorf("got %+v, want Field=legacyRuleList Baseline=\"\" Observed=rule-x AfterField=algo", v)
		}
		if !violated["legacyRuleList"] {
			t.Error("expected checkAssertUnchanged to mark the field as violated")
		}
	})
}

// TestCheckAssertUnchanged covers checkAssertUnchanged directly: a field
// that held its baseline reports nothing, a field that moved is reported
// exactly once and attributed to the field test that was in flight, and a
// field already marked violated is never reported a second time even if it
// keeps differing.
func TestCheckAssertUnchanged(t *testing.T) {
	baselines := map[string]string{"legacyRuleList": `["a"]`, "stable": "x"}

	t.Run("NoDriftReportsNothing", func(t *testing.T) {
		snapshot := []byte(`{"legacyRuleList":["a"],"stable":"x"}`)
		violated := map[string]bool{}
		got, err := checkAssertUnchanged(snapshot, []string{"legacyRuleList", "stable"}, baselines, violated, "comment")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d violations, want 0: %+v", len(got), got)
		}
	})

	t.Run("DriftIsReportedAndAttributedToTheTriggeringField", func(t *testing.T) {
		snapshot := []byte(`{"legacyRuleList":[],"stable":"x"}`)
		violated := map[string]bool{}
		got, err := checkAssertUnchanged(snapshot, []string{"legacyRuleList", "stable"}, baselines, violated, "comment")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d violations, want 1: %+v", len(got), got)
		}
		v := got[0]
		if v.Field != "legacyRuleList" || v.Baseline != `["a"]` || v.Observed != "[]" || v.AfterField != "comment" {
			t.Errorf("got %+v, want Field=legacyRuleList Baseline=[\"a\"] Observed=[] AfterField=comment", v)
		}
		if !violated["legacyRuleList"] {
			t.Error("expected checkAssertUnchanged to mark the field as violated")
		}
	})

	t.Run("AlreadyViolatedFieldIsNeverReportedAgain", func(t *testing.T) {
		snapshot := []byte(`{"legacyRuleList":["still-different"],"stable":"x"}`)
		violated := map[string]bool{"legacyRuleList": true}
		got, err := checkAssertUnchanged(snapshot, []string{"legacyRuleList", "stable"}, baselines, violated, "name")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d violations, want 0 (already reported once): %+v", len(got), got)
		}
	})
}
