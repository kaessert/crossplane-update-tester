package validator

import (
	"reflect"
	"sort"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestCheckMergePatchSiblings covers the seven flag/no-flag cases the ticket
// requires, table-driven against a single synthetic manifest whose
// spec.forProvider mirrors the real f5xc shapes this check exists for
// (Healthcheck.httpHealthCheck, VoltshareAdminPolicy.authorRestrictions).
func TestCheckMergePatchSiblings(t *testing.T) {
	cases := map[string]struct {
		reason      string
		forProvider map[string]interface{}
		tests       []manifest.UpdateTest
		wantField   string
		wantKeys    []string // nil means no finding at all
	}{
		"PartialObjectPatchOntoPopulatedObjectFlagged": {
			reason: "value: overwrites only path, leaving useOriginServerName and headers unaddressed — an RFC 7386 merge preserves both verbatim, and jsonEqual's whole-value DeepEqual can never match a value: that only names path",
			forProvider: map[string]interface{}{
				"httpHealthCheck": map[string]interface{}{
					"path":                "/healthz",
					"useOriginServerName": map[string]interface{}{},
					"headers":             map[string]interface{}{},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "httpHealthCheck",
					Value: map[string]interface{}{"path": "/healthz-updated"},
				},
			},
			wantField: "httpHealthCheck",
			wantKeys:  []string{"headers", "useOriginServerName"},
		},
		"MatchingExpectNotFlagged": {
			reason: "an expect: block recording the exact merged shape is the 384310b6 remedy — the check must not fight it",
			forProvider: map[string]interface{}{
				"httpHealthCheck": map[string]interface{}{
					"path":                "/healthz",
					"useOriginServerName": map[string]interface{}{},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "httpHealthCheck",
					Value: map[string]interface{}{"path": "/healthz-updated"},
					Expect: map[string]interface{}{
						"path":                "/healthz-updated",
						"useOriginServerName": map[string]interface{}{},
					},
				},
			},
			wantKeys: nil,
		},
		"ExpectNamingServerBackfilledSiblingNotFlagged": {
			reason: "the real log-receiver tlsServer shape — the runner compares against status.atProvider, not this check's spec-derived merge simulation, so a correct expect: block legitimately records defaultSyslogTlsPort even though no create-time value or merge simulation could ever produce it; presence of the survivor key is enough, the check has no authority to also demand the exact backfilled value",
			forProvider: map[string]interface{}{
				"syslog": map[string]interface{}{
					"endpoint": "syslog.example.com:6514",
					"tlsServer": map[string]interface{}{
						"serverName":   "syslog.example.com",
						"trustedCaUrl": "string:///...",
						"mtlsEnable":   map[string]interface{}{"certificate": "string:///..."},
					},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "syslog",
					Value: map[string]interface{}{"endpoint": "syslog-updated.example.com:6514"},
					Expect: map[string]interface{}{
						"endpoint": "syslog-updated.example.com:6514",
						"tlsServer": map[string]interface{}{
							"serverName":           "syslog.example.com",
							"trustedCaUrl":         "string:///...",
							"mtlsEnable":           map[string]interface{}{"certificate": "string:///..."},
							"defaultSyslogTlsPort": float64(6514),
						},
					},
				},
			},
			wantKeys: nil,
		},
		"ExplicitNullClearingSiblingNotFlagged": {
			reason: "authorRestrictions swaps its create-time allowList member out with an explicit null alongside the incoming denyList member — the a3650f74 remedy",
			forProvider: map[string]interface{}{
				"authorRestrictions": map[string]interface{}{
					"allowList": map[string]interface{}{
						"customList": []interface{}{
							map[string]interface{}{"exactValue": "alice@example.com"},
						},
					},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "authorRestrictions",
					Value: map[string]interface{}{
						"allowList": nil,
						"denyList": map[string]interface{}{
							"customList": []interface{}{
								map[string]interface{}{"exactValue": "mallory@example.com"},
							},
						},
					},
				},
			},
			wantKeys: nil,
		},
		"ObjectPatchedOntoEmptyObjectNotFlagged": {
			reason: "labels/annotations start as {} at create time — there are no sibling keys to survive, matching or not",
			forProvider: map[string]interface{}{
				"labels": map[string]interface{}{},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "labels",
					Value: map[string]interface{}{"team": "platform"},
				},
			},
			wantKeys: nil,
		},
		"ListValueNotFlagged": {
			reason: "RFC 7386 replaces a list wholesale — userRestrictions is a list-valued field and must never be flagged for sibling survival",
			forProvider: map[string]interface{}{
				"userRestrictions": []interface{}{
					map[string]interface{}{"individualUsers": map[string]interface{}{}},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "userRestrictions",
					Value: []interface{}{
						map[string]interface{}{"allTenants": map[string]interface{}{}},
					},
				},
			},
			wantKeys: nil,
		},
		"SkipEntryNotFlagged": {
			reason: "a skip: entry is never flagged, even though its value would otherwise leave a sibling surviving",
			forProvider: map[string]interface{}{
				"httpHealthCheck": map[string]interface{}{
					"path":    "/healthz",
					"headers": map[string]interface{}{},
				},
			},
			tests: []manifest.UpdateTest{
				{
					Field: "httpHealthCheck",
					Skip:  manifest.LegacySkip("not exercised in this example"),
					Value: map[string]interface{}{"path": "/healthz-updated"},
				},
			},
			wantKeys: nil,
		},
		"ScalarNotFlagged": {
			reason: "this check only reasons about object-valued patches",
			forProvider: map[string]interface{}{
				"description": "original",
			},
			tests: []manifest.UpdateTest{
				{Field: "description", Value: "updated"},
			},
			wantKeys: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &manifest.Manifest{ForProvider: tc.forProvider, Tests: tc.tests}
			findings := CheckMergePatchSiblings(m)

			if tc.wantKeys == nil {
				if len(findings) != 0 {
					t.Fatalf("%s: got %d findings, want 0: %+v", tc.reason, len(findings), findings)
				}
				return
			}

			if len(findings) != 1 {
				t.Fatalf("%s: got %d findings, want 1: %+v", tc.reason, len(findings), findings)
			}
			if findings[0].Field != tc.wantField {
				t.Errorf("%s: Field = %q, want %q", tc.reason, findings[0].Field, tc.wantField)
			}
			gotKeys := append([]string(nil), findings[0].Keys...)
			sort.Strings(gotKeys)
			wantKeys := append([]string(nil), tc.wantKeys...)
			sort.Strings(wantKeys)
			if !reflect.DeepEqual(gotKeys, wantKeys) {
				t.Errorf("%s: Keys = %v, want %v", tc.reason, gotKeys, wantKeys)
			}
		})
	}
}

// TestCheckMergePatchSiblingsFieldAbsentFromForProviderNotFlagged confirms a
// field the manifest's spec.forProvider never declares at create time is
// left unflagged rather than guessed at.
func TestCheckMergePatchSiblingsFieldAbsentFromForProviderNotFlagged(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{"description": "original"},
		Tests: []manifest.UpdateTest{
			{Field: "httpHealthCheck", Value: map[string]interface{}{"path": "/healthz-updated"}},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0 for a field absent from spec.forProvider: %+v", len(findings), findings)
	}
}

// TestCheckMergePatchSiblingsCreateTimeValueNotObjectNotFlagged confirms a
// field whose create-time value in spec.forProvider is not itself an object
// (e.g. still nil/server-defaulted) is left unflagged.
func TestCheckMergePatchSiblingsCreateTimeValueNotObjectNotFlagged(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{"httpHealthCheck": "not-an-object"},
		Tests: []manifest.UpdateTest{
			{Field: "httpHealthCheck", Value: map[string]interface{}{"path": "/healthz-updated"}},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0 when the create-time value isn't an object: %+v", len(findings), findings)
	}
}

// TestCheckMergePatchSiblingsNilForProviderNotFlagged confirms a manifest
// with no spec.forProvider at all (m.ForProvider == nil, e.g. the companion
// document in a multi-document stream) never panics and never flags.
func TestCheckMergePatchSiblingsNilForProviderNotFlagged(t *testing.T) {
	m := &manifest.Manifest{
		Tests: []manifest.UpdateTest{
			{Field: "httpHealthCheck", Value: map[string]interface{}{"path": "/healthz-updated"}},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0 for a nil ForProvider: %+v", len(findings), findings)
	}
}

// TestCheckMergePatchSiblingsDottedFieldResolves confirms a dotted field
// path navigates m.ForProvider the same way the runner's own
// navigateSpecForProvider does at patch time.
func TestCheckMergePatchSiblingsDottedFieldResolves(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"syslog": map[string]interface{}{
				"tlsServer": map[string]interface{}{
					"serverName": "syslog.example.com",
					"mtlsEnable": map[string]interface{}{"certificate": "..."},
				},
			},
		},
		Tests: []manifest.UpdateTest{
			{
				Field: "syslog.tlsServer",
				Value: map[string]interface{}{"serverName": "updated.example.com"},
			},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].Field != "syslog.tlsServer" {
		t.Errorf("Field = %q, want syslog.tlsServer", findings[0].Field)
	}
	if len(findings[0].Keys) != 1 || findings[0].Keys[0] != "mtlsEnable" {
		t.Errorf("Keys = %v, want [mtlsEnable]", findings[0].Keys)
	}
}

// TestCheckMergePatchSiblingsF5XCHealthcheckReproduction pins the exact
// real-world shape (384310b6): Healthcheck.httpHealthCheck patches only
// path, leaving useOriginServerName/expectedStatusCodes/headers/
// requestHeadersToRemove unaddressed.
func TestCheckMergePatchSiblingsF5XCHealthcheckReproduction(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"httpHealthCheck": map[string]interface{}{
				"path":                   "/healthz",
				"useOriginServerName":    map[string]interface{}{},
				"expectedStatusCodes":    []interface{}{},
				"headers":                map[string]interface{}{},
				"requestHeadersToRemove": []interface{}{},
			},
		},
		Tests: []manifest.UpdateTest{
			{
				Field: "httpHealthCheck",
				Value: map[string]interface{}{"path": "/healthz-updated"},
			},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	want := []string{"expectedStatusCodes", "headers", "requestHeadersToRemove", "useOriginServerName"}
	got := append([]string(nil), findings[0].Keys...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
}

// TestCheckMergePatchSiblingsF5XCVoltshareAdminPolicyReproduction pins the
// exact real-world union shape (a3650f74): authorRestrictions swaps from the
// create-time allowList member to denyList without nulling allowList out, so
// allowList survives as a second, mutually exclusive member.
func TestCheckMergePatchSiblingsF5XCVoltshareAdminPolicyReproduction(t *testing.T) {
	m := &manifest.Manifest{
		ForProvider: map[string]interface{}{
			"authorRestrictions": map[string]interface{}{
				"allowList": map[string]interface{}{
					"customList": []interface{}{
						map[string]interface{}{"exactValue": "alice@example.com"},
					},
				},
			},
		},
		Tests: []manifest.UpdateTest{
			{
				Field: "authorRestrictions",
				Value: map[string]interface{}{
					"denyList": map[string]interface{}{
						"customList": []interface{}{
							map[string]interface{}{"exactValue": "mallory@example.com"},
						},
					},
				},
			},
		},
	}
	findings := CheckMergePatchSiblings(m)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if len(findings[0].Keys) != 1 || findings[0].Keys[0] != "allowList" {
		t.Errorf("Keys = %v, want [allowList]", findings[0].Keys)
	}
}

// TestNavigateForProvider covers the dotted-path navigation helper in
// isolation: multi-level resolution, a missing key, and a path that runs
// into a non-object partway through.
func TestNavigateForProvider(t *testing.T) {
	forProvider := map[string]interface{}{
		"syslog": map[string]interface{}{
			"tlsServer": map[string]interface{}{"serverName": "syslog.example.com"},
		},
		"description": "plain scalar",
	}

	cases := map[string]struct {
		reason string
		field  string
		wantOK bool
		want   interface{}
	}{
		"TopLevel": {
			reason: "a single-segment path resolves directly",
			field:  "description",
			wantOK: true,
			want:   "plain scalar",
		},
		"Nested": {
			reason: "a multi-segment dotted path walks each level",
			field:  "syslog.tlsServer",
			wantOK: true,
			want:   map[string]interface{}{"serverName": "syslog.example.com"},
		},
		"MissingKey": {
			reason: "a key absent at any level resolves to ok=false",
			field:  "syslog.doesNotExist",
			wantOK: false,
		},
		"PathThroughScalar": {
			reason: "continuing to navigate past a scalar resolves to ok=false rather than panicking",
			field:  "description.nested",
			wantOK: false,
		},
		"UnknownTopLevel": {
			reason: "an entirely unknown top-level field resolves to ok=false",
			field:  "doesNotExist",
			wantOK: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := navigateForProvider(forProvider, tc.field)
			if ok != tc.wantOK {
				t.Fatalf("%s: navigateForProvider() ok = %v, want %v", tc.reason, ok, tc.wantOK)
			}
			if tc.wantOK && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: navigateForProvider() = %#v, want %#v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestNavigateForProviderNilForProvider confirms a nil forProvider (a
// manifest with no spec.forProvider at all) resolves to ok=false rather than
// panicking.
func TestNavigateForProviderNilForProvider(t *testing.T) {
	_, ok := navigateForProvider(nil, "anything")
	if ok {
		t.Error("navigateForProvider(nil, ...) ok = true, want false")
	}
}
