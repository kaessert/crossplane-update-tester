package validator

import (
	"reflect"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestCheckGuaranteedNoOp covers the flag/no-flag cases the ticket
// requires, table-driven against synthetic manifests.
func TestCheckGuaranteedNoOp(t *testing.T) {
	cases := map[string]struct {
		reason      string
		forProvider map[string]interface{}
		tests       []manifest.UpdateTest
		wantFields  []string // nil means no finding at all
	}{
		"ScalarValueEqualsCreateTimeFlagged": {
			reason: "value: repeats the exact scalar already in spec.forProvider — the patch changes nothing",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "original"},
			},
			wantFields: []string{"description"},
		},
		"ScalarValueDifferentNotFlagged": {
			reason: "value: genuinely differs from the create-time value — an ordinary, exercisable entry",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "updated"},
			},
			wantFields: nil,
		},
		"ObjectValueEqualsCreateTimeFlagged": {
			reason: "the real enableApiDiscovery shape: value: is byte-for-byte the same nested object already committed at creation",
			forProvider: map[string]interface{}{
				"enableApiDiscovery": map[string]interface{}{
					"apiCrawler": map[string]interface{}{
						"apiCrawlerConfig": map[string]interface{}{
							"domains": []interface{}{"example.crossplane.test"},
						},
					},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "enableApiDiscovery",
					Value: map[string]interface{}{
						"apiCrawler": map[string]interface{}{
							"apiCrawlerConfig": map[string]interface{}{
								"domains": []interface{}{"example.crossplane.test"},
							},
						},
					},
				},
			},
			wantFields: []string{"enableApiDiscovery"},
		},
		"ObjectValueDifferentNotFlagged": {
			reason: "the object value: patches a genuinely different shape than create time",
			forProvider: map[string]interface{}{
				"httpHealthCheck": map[string]interface{}{"path": "/healthz"},
			},
			tests: []manifest.UpdateTest{
				{Field: "httpHealthCheck", Value: map[string]interface{}{"path": "/healthz-updated"}},
			},
			wantFields: nil,
		},
		"SkipEntryNotFlagged": {
			reason: "a skip: entry is never flagged, even though its value would otherwise deep-equal create time",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Skip: "not exercised", Value: "original"},
			},
			wantFields: nil,
		},
		"FieldAbsentFromForProviderNotFlagged": {
			reason: "nothing to compare against — a field never committed at creation cannot be a repeat of anything",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "otherField", Value: "anything"},
			},
			wantFields: nil,
		},
		"NilForProviderNotFlagged": {
			reason: "a manifest with no spec.forProvider at all never panics and never flags",
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "anything"},
			},
			wantFields: nil,
		},
		"KnownDefectEntryAlsoFlagged": {
			reason: "runner.go's pre-patch no-op guard runs unconditionally for a knownDefect entry too — runFieldTest knows nothing about KnownDefect",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "original", KnownDefect: "TICKET-1234"},
			},
			wantFields: []string{"description"},
		},
		"DottedFieldPathResolves": {
			reason: "a nested field path navigates m.ForProvider the same way navigateForProvider does elsewhere in this package",
			forProvider: map[string]interface{}{
				"syslog": map[string]interface{}{
					"tlsServer": map[string]interface{}{"serverName": "syslog.example.com"},
				},
			},
			tests: []manifest.UpdateTest{
				{Field: "syslog.tlsServer", Value: map[string]interface{}{"serverName": "syslog.example.com"}},
			},
			wantFields: []string{"syslog.tlsServer"},
		},
		"PrecedingEntrySameTopLevelFieldGuardsLaterEntry": {
			reason: "an earlier non-skipped entry already targeted this exact field — it may have changed the live value by the time this entry's patch is built, so this check declines to guess",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "updated"},
				{Field: "description", Value: "original"},
			},
			wantFields: nil,
		},
		"PrecedingEntryClearListGuardsLaterEntry": {
			reason: "an earlier entry's clear: list names this field's top-level name — it may have nulled or otherwise changed it, so this check declines to guess",
			forProvider: map[string]interface{}{
				"allowList": map[string]interface{}{"customList": []interface{}{"alice"}},
			},
			tests: []manifest.UpdateTest{
				{
					// denyList is absent from create-time forProvider, so
					// this entry is an ordinary, genuinely exercised patch
					// — never itself a candidate finding — and its clear:
					// list is what this case is actually testing.
					Field: "denyList",
					Value: map[string]interface{}{"customList": []interface{}{"mallory"}},
					Clear: []string{"allowList"},
				},
				{Field: "allowList", Value: map[string]interface{}{"customList": []interface{}{"alice"}}},
			},
			wantFields: nil,
		},
		"PrecedingSkipEntryDoesNotGuard": {
			reason: "a skip: entry never patches anything, so it must not guard a later entry targeting the same field",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Skip: "not exercised here", Value: "updated"},
				{Field: "description", Value: "original"},
			},
			wantFields: []string{"description"},
		},
		"MultipleIndependentFlags": {
			reason: "two unrelated fields can each independently be a guaranteed no-op in the same manifest",
			forProvider: map[string]interface{}{
				"a": "same",
				"b": "same",
			},
			tests: []manifest.UpdateTest{
				{Field: "a", Value: "same"},
				{Field: "b", Value: "same"},
			},
			wantFields: []string{"a", "b"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{ForProvider: tc.forProvider, Tests: tc.tests}
			findings := CheckGuaranteedNoOp(m)

			if tc.wantFields == nil {
				if len(findings) != 0 {
					t.Fatalf("%s: got %d findings, want 0: %+v", tc.reason, len(findings), findings)
				}
				return
			}

			if len(findings) != len(tc.wantFields) {
				t.Fatalf("%s: got %d findings, want %d: %+v", tc.reason, len(findings), len(tc.wantFields), findings)
			}
			for i, f := range findings {
				if f.Field != tc.wantFields[i] {
					t.Errorf("%s: findings[%d].Field = %q, want %q", tc.reason, i, f.Field, tc.wantFields[i])
				}
			}
		})
	}
}

// TestCheckGuaranteedNoOpPreFixHTTPLoadbalancerReproduction pins the exact
// real-world defect this check exists for: provider-f5xc's http-loadbalancer
// enableApiDiscovery entry, before the fix, whose value: was byte-for-byte
// the same nested object already committed in spec.forProvider at creation.
func TestCheckGuaranteedNoOpPreFixHTTPLoadbalancerReproduction(t *testing.T) {
	enableAPIDiscovery := map[string]interface{}{
		"apiCrawler": map[string]interface{}{
			"apiCrawlerConfig": map[string]interface{}{
				"domains": []interface{}{
					map[string]interface{}{
						"domain": "example.crossplane.test",
						"simpleLogin": map[string]interface{}{
							"user": "example-crawler-user",
							"passwordSecretRef": map[string]interface{}{
								"name":      "crossplane-uptest-http-loadbalancer-crawler-password",
								"namespace": "default",
								"key":       "password",
							},
						},
					},
				},
			},
		},
	}

	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"enableApiDiscovery": enableAPIDiscovery,
		},
		Tests: []manifest.UpdateTest{
			{Field: "enableApiDiscovery", Value: enableAPIDiscovery},
		},
	}

	findings := CheckGuaranteedNoOp(m)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1 (the pre-fix shape): %+v", len(findings), findings)
	}
	if findings[0].Field != "enableApiDiscovery" {
		t.Errorf("Field = %q, want enableApiDiscovery", findings[0].Field)
	}
}

// TestCheckGuaranteedNoOpPostFixHTTPLoadbalancerNotFlagged pins the fixed
// shape: enableApiDiscovery is skip: on the branch this defect was fixed on,
// so it must never be flagged, whatever its value: would have deep-equalled.
func TestCheckGuaranteedNoOpPostFixHTTPLoadbalancerNotFlagged(t *testing.T) {
	enableAPIDiscovery := map[string]interface{}{
		"apiCrawler": map[string]interface{}{
			"apiCrawlerConfig": map[string]interface{}{
				"domains": []interface{}{"example.crossplane.test"},
			},
		},
	}

	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"enableApiDiscovery": enableAPIDiscovery,
		},
		Tests: []manifest.UpdateTest{
			{
				Field: "enableApiDiscovery",
				Skip:  "live-confirmed side effect of the domains field, which runs first in field order",
				Value: enableAPIDiscovery,
			},
		},
	}

	findings := CheckGuaranteedNoOp(m)
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0 for a skip: entry: %+v", len(findings), findings)
	}
}

// TestCheckGuaranteedNoOpDirectiveLineManifestAnalysed confirms a manifest
// whose annotation carries a top-level directive line (assert-unchanged:)
// alongside the field-entry list still has its entries parsed and analysed
// by this check, rather than silently skipped by a parser that chokes on
// the directive line — the exact failure mode 4 of 286 fleet manifests would
// hit with a naive parse.
func TestCheckGuaranteedNoOpDirectiveLineManifestAnalysed(t *testing.T) {
	yamlDoc := `
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network
  annotations:
    crossplane.io/update-test: |
      assert-unchanged: legacyRuleList
      converge-skip: "status field churns every observe cycle"
      - field: description
        value: "same-as-create-time"
spec:
  forProvider:
    description: "same-as-create-time"
`
	m, err := manifest.ParseBytes([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("manifest.ParseBytes() error = %v", err)
	}
	if len(m.Tests) != 1 {
		t.Fatalf("got %d parsed entries, want 1 — the directive lines must not swallow the field-entry list", len(m.Tests))
	}

	findings := CheckGuaranteedNoOp(m)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1 — the entry must be analysed, not silently skipped: %+v", len(findings), findings)
	}
	if findings[0].Field != "description" {
		t.Errorf("Field = %q, want description", findings[0].Field)
	}
}

// TestTopLevelField covers the dotted-path-to-top-level-segment helper in
// isolation.
func TestTopLevelField(t *testing.T) {
	cases := map[string]struct {
		field string
		want  string
	}{
		"TopLevelOnly":    {field: "description", want: "description"},
		"OneDot":          {field: "syslog.tlsServer", want: "syslog"},
		"MultipleDots":    {field: "a.b.c", want: "a"},
		"EmptyString":     {field: "", want: ""},
		"LeadingDot":      {field: ".b", want: ""},
		"TrailingDotOnly": {field: "a.", want: "a"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := topLevelField(tc.field)
			if got != tc.want {
				t.Errorf("topLevelField(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// TestGuaranteedNoOpEqual covers the normalised comparison in isolation,
// including the numeric-type alignment case (YAML int vs a raw float64) the
// JSON round trip exists to handle.
func TestGuaranteedNoOpEqual(t *testing.T) {
	cases := map[string]struct {
		reason string
		a, b   interface{}
		want   bool
	}{
		"IdenticalStrings": {
			reason: "trivial equal case",
			a:      "same", b: "same",
			want: true,
		},
		"DifferentStrings": {
			reason: "trivial unequal case",
			a:      "one", b: "two",
			want: false,
		},
		"IntVsFloat64SameNumericValue": {
			reason: "YAML decodes a bare integer literal as int; a value built up directly in Go might already be float64 — the JSON round trip must treat these as equal",
			a:      5, b: float64(5),
			want: true,
		},
		"IdenticalNestedObjects": {
			reason: "deep object equality after normalisation",
			a:      map[string]interface{}{"x": []interface{}{1, "two"}},
			b:      map[string]interface{}{"x": []interface{}{1, "two"}},
			want:   true,
		},
		"DifferentNestedObjects": {
			reason: "a nested value genuinely differs",
			a:      map[string]interface{}{"x": 1},
			b:      map[string]interface{}{"x": 2},
			want:   false,
		},
		"NilVsNil": {
			reason: "both sides nil",
			a:      nil, b: nil,
			want: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := guaranteedNoOpEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("%s: guaranteedNoOpEqual(%#v, %#v) = %v, want %v", tc.reason, tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestCheckGuaranteedNoOpFindingsIndependentOfDeepEqualOrdering guards
// against a regression where map iteration order over m.Tests could ever
// matter; CheckGuaranteedNoOp iterates m.Tests (a slice), so this mainly
// pins that findings are returned in manifest entry order, not sorted or
// shuffled.
func TestCheckGuaranteedNoOpFindingsIndependentOfDeepEqualOrdering(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"z": "same",
			"a": "same",
		},
		Tests: []manifest.UpdateTest{
			{Field: "z", Value: "same"},
			{Field: "a", Value: "same"},
		},
	}
	findings := CheckGuaranteedNoOp(m)
	want := []string{"z", "a"}
	got := make([]string, len(findings))
	for i, f := range findings {
		got[i] = f.Field
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("field order = %v, want %v (manifest entry order)", got, want)
	}
}
