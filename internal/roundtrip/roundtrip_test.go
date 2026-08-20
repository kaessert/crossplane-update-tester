package roundtrip

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mustDecodeCRD parses a CRD YAML fixture into the generic
// map[string]interface{} shape DiffReport and FindCRD both operate on.
func mustDecodeCRD(t *testing.T, doc string) map[string]interface{} {
	t.Helper()
	var crd map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
		t.Fatalf("decoding CRD fixture: %v", err)
	}
	return crd
}

func mustDecodeObject(t *testing.T, doc string) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(doc), &obj); err != nil {
		t.Fatalf("decoding object fixture: %v", err)
	}
	return obj
}

// classificationCounts tallies rows by Classification, for a compact
// assertion against an expected distribution.
func classificationCounts(rows []Row) map[string]int {
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Classification]++
	}
	return counts
}

// rowByPath finds the row for a given field path, failing the test if it is
// not present — every test that calls this asserts on a path it expects
// DiffReport to have reported.
func rowByPath(t *testing.T, rows []Row, path string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no row for path %q; rows: %+v", path, rows)
	return Row{}
}

// ─── parity fixtures ────────────────────────────────────────────────────
//
// Every fixture below is a trimmed, schema-faithful subset of a REAL
// provider-f5xc CRD (package/crds) and a REAL live object captured during
// an actual E2E run (ticket 0a24dc7f's ~/e2e-logs/a49da517-attempt6
// diagnostics — AppFirewall's oneof siblings and HttpLoadbalancer's
// corsPolicy/protectedCookies, the exact shape of the defect this port
// exists to catch). Each one was verified byte-for-byte against
// test/hooks/lib/roundtrip_diff.py's own `diff` mode before being trimmed
// into this file, and the trim preserved every row test/hooks/lib's
// classification produced. See DiffReport's callers below for what each one
// demonstrates.

// appFirewallCRD / appFirewallObject demonstrate present-in-spec-absent-from-
// mirror (a forProvider oneof-selector sibling the backend never echoes back,
// e.g. `allowAllResponseCodes: {}`), present-in-mirror-absent-from-spec (a
// pure server field like `id`, and an array value like `violationsView` that
// has no forProvider counterpart at all), and equal — including through a
// one-level-nested object (`blockingPage.blockingPage`).
const appFirewallCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: f5xc.crossplane.io
  names:
    kind: AppFirewall
    plural: appfirewalls
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  allowAllResponseCodes:
                    nullable: true
                    type: object
                  defaultAnonymization:
                    nullable: true
                    type: object
                  disableAiEnhancements:
                    nullable: true
                    type: object
                  useDefaultBlockingPage:
                    nullable: true
                    type: object
                  description:
                    type: string
                  disable:
                    type: boolean
                  labels:
                    additionalProperties:
                      type: string
                    nullable: true
                    type: object
                  blockingPage:
                    nullable: true
                    properties:
                      blockingPage:
                        type: string
                      responseCode:
                        type: string
                    type: object
                  enableAiEnhancements:
                    nullable: true
                    properties:
                      mitigateHighRiskAction:
                        nullable: true
                        type: object
                    type: object
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  id:
                    type: string
                  violationsView:
                    items:
                      properties:
                        description:
                          type: string
                        enabled:
                          type: boolean
                        enabledByDefault:
                          type: string
                        name:
                          type: string
                        title:
                          type: string
                      type: object
                    nullable: true
                    type: array
                  description:
                    type: string
                  disable:
                    type: boolean
                  labels:
                    additionalProperties:
                      type: string
                    nullable: true
                    type: object
                  blockingPage:
                    nullable: true
                    properties:
                      blockingPage:
                        type: string
                      responseCode:
                        type: string
                    type: object
                  enableAiEnhancements:
                    nullable: true
                    properties:
                      mitigateHighRiskAction:
                        nullable: true
                        type: object
                    type: object
`

const appFirewallObject = `{
  "apiVersion": "f5xc.crossplane.io/v1alpha1",
  "kind": "AppFirewall",
  "metadata": { "name": "example-app-firewall" },
  "spec": {
    "forProvider": {
      "allowAllResponseCodes": {},
      "defaultAnonymization": {},
      "disableAiEnhancements": {},
      "useDefaultBlockingPage": {},
      "description": "Updated App Firewall managed by Crossplane",
      "disable": true,
      "labels": { "team": "platform" },
      "blockingPage": {
        "blockingPage": "https://www.example.com/blocked.html",
        "responseCode": "Forbidden"
      },
      "enableAiEnhancements": { "mitigateHighRiskAction": {} }
    }
  },
  "status": {
    "atProvider": {
      "id": "example-app-firewall",
      "violationsView": [
        {
          "description": "Excessive or irregular whitespace is used in requests, potentially to evade security mechanisms.",
          "enabled": true,
          "enabledByDefault": "Yes",
          "name": "VIOL_EVASION_APACHE_WHITESPACE",
          "title": "Apache whitespace"
        },
        {
          "description": "An HTTP request specifies an unsupported or unrecognized HTTP version.",
          "enabled": true,
          "enabledByDefault": "Yes",
          "name": "VIOL_HTTP_PROTOCOL_BAD_HTTP_VERSION",
          "title": "Bad HTTP version"
        }
      ],
      "description": "Updated App Firewall managed by Crossplane",
      "disable": true,
      "labels": { "team": "platform" },
      "blockingPage": {
        "blockingPage": "https://www.example.com/blocked.html",
        "responseCode": "Forbidden"
      },
      "enableAiEnhancements": { "mitigateHighRiskAction": {} }
    }
  }
}`

// TestDiffReportMatchesPythonAppFirewall pins the classification for every
// path test/hooks/lib/roundtrip_diff.py's own `diff` mode produced against
// the unabridged CRD and the unabridged captured object (verified by hand
// before trimming — see the fixture comment above).
func TestDiffReportMatchesPythonAppFirewall(t *testing.T) {
	crd := mustDecodeCRD(t, appFirewallCRD)
	obj := mustDecodeObject(t, appFirewallObject)

	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport: %v", err)
	}

	wantCounts := map[string]int{
		ClassEqual:                         6,
		ClassPresentInSpecAbsentFromMirror: 4,
		ClassPresentInMirrorAbsentFromSpec: 2,
	}
	if got := classificationCounts(rows); !reflect.DeepEqual(got, wantCounts) {
		t.Errorf("classification tally = %+v, want %+v\nrows: %+v", got, wantCounts, rows)
	}
	if len(rows) != 12 {
		t.Errorf("len(rows) = %d, want 12 (matches the Python reference run)", len(rows))
	}

	wantClass := map[string]string{
		"allowAllResponseCodes":     ClassPresentInSpecAbsentFromMirror,
		"defaultAnonymization":      ClassPresentInSpecAbsentFromMirror,
		"disableAiEnhancements":     ClassPresentInSpecAbsentFromMirror,
		"useDefaultBlockingPage":    ClassPresentInSpecAbsentFromMirror,
		"id":                        ClassPresentInMirrorAbsentFromSpec,
		"violationsView":            ClassPresentInMirrorAbsentFromSpec,
		"description":               ClassEqual,
		"disable":                   ClassEqual,
		"labels":                    ClassEqual,
		"blockingPage.blockingPage": ClassEqual,
		"blockingPage.responseCode": ClassEqual,
		"enableAiEnhancements.mitigateHighRiskAction": ClassEqual,
	}
	for path, want := range wantClass {
		if got := rowByPath(t, rows, path).Classification; got != want {
			t.Errorf("path %q classification = %q, want %q", path, got, want)
		}
	}

	// present-in-spec-absent-from-mirror is the classification that
	// distinguished a genuine defect from a harness artifact on
	// cb60fcbd attempts 5 and 7 — assert its value shape explicitly, not
	// only its label.
	allowAll := rowByPath(t, rows, "allowAllResponseCodes")
	if !allowAll.SpecFound || allowAll.MirrorFound {
		t.Errorf("allowAllResponseCodes: SpecFound=%v MirrorFound=%v, want true/false", allowAll.SpecFound, allowAll.MirrorFound)
	}
}

// httpLoadbalancerCRD / httpLoadbalancerObject demonstrate defaulted-by-server
// (corsPolicy.allowHeaders/disabled/exposeHeaders — the backend echoes a
// value the client never sent) and value-changed on an ARRAY field
// (protectedCookies): the CRD schema declares protectedCookies as
// `type: array`, so its element object is never descended into and the
// whole array is compared as one leaf value. This is the actual
// defect shape bb2827fb fixed — the nested
// `disableTamperingProtection` the backend adds to one array element
// changes the whole `protectedCookies` array, not a
// "protectedCookies[].disableTamperingProtection" sub-path.
const httpLoadbalancerCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: f5xc.crossplane.io
  names:
    kind: HttpLoadbalancer
    plural: httploadbalancers
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  corsPolicy:
                    nullable: true
                    properties:
                      allowCredentials:
                        type: boolean
                      allowHeaders:
                        type: string
                      allowMethods:
                        type: string
                      allowOrigin:
                        items:
                          type: string
                        nullable: true
                        type: array
                      disabled:
                        type: boolean
                      exposeHeaders:
                        type: string
                      maximumAge:
                        type: integer
                    type: object
                  protectedCookies:
                    items:
                      properties:
                        addHttponly:
                          nullable: true
                          type: object
                        addSecure:
                          nullable: true
                          type: object
                        disableTamperingProtection:
                          nullable: true
                          type: object
                        name:
                          type: string
                      type: object
                    nullable: true
                    type: array
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  corsPolicy:
                    nullable: true
                    properties:
                      allowCredentials:
                        type: boolean
                      allowHeaders:
                        type: string
                      allowMethods:
                        type: string
                      allowOrigin:
                        items:
                          type: string
                        nullable: true
                        type: array
                      disabled:
                        type: boolean
                      exposeHeaders:
                        type: string
                      maximumAge:
                        type: integer
                    type: object
                  protectedCookies:
                    items:
                      properties:
                        addHttponly:
                          nullable: true
                          type: object
                        addSecure:
                          nullable: true
                          type: object
                        disableTamperingProtection:
                          nullable: true
                          type: object
                        name:
                          type: string
                      type: object
                    nullable: true
                    type: array
`

const httpLoadbalancerObject = `{
  "apiVersion": "f5xc.crossplane.io/v1alpha1",
  "kind": "HttpLoadbalancer",
  "metadata": { "name": "example-http-loadbalancer" },
  "spec": {
    "forProvider": {
      "corsPolicy": {
        "allowCredentials": true,
        "allowMethods": "GET,POST",
        "allowOrigin": ["https://example.crossplane.test"],
        "maximumAge": 3600
      },
      "protectedCookies": [
        { "addHttponly": {}, "addSecure": {}, "name": "session_id" }
      ]
    }
  },
  "status": {
    "atProvider": {
      "corsPolicy": {
        "allowCredentials": true,
        "allowHeaders": "",
        "allowMethods": "GET,POST",
        "allowOrigin": ["https://example.crossplane.test"],
        "disabled": false,
        "exposeHeaders": "",
        "maximumAge": 3600
      },
      "protectedCookies": [
        { "addHttponly": {}, "addSecure": {}, "disableTamperingProtection": {}, "name": "session_id" }
      ]
    }
  }
}`

func TestDiffReportMatchesPythonHttpLoadbalancer(t *testing.T) {
	crd := mustDecodeCRD(t, httpLoadbalancerCRD)
	obj := mustDecodeObject(t, httpLoadbalancerObject)

	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport: %v", err)
	}

	wantCounts := map[string]int{
		ClassEqual:             4,
		ClassDefaultedByServer: 3,
		ClassValueChanged:      1,
	}
	if got := classificationCounts(rows); !reflect.DeepEqual(got, wantCounts) {
		t.Errorf("classification tally = %+v, want %+v\nrows: %+v", got, wantCounts, rows)
	}

	for _, path := range []string{"corsPolicy.allowHeaders", "corsPolicy.disabled", "corsPolicy.exposeHeaders"} {
		r := rowByPath(t, rows, path)
		if r.Classification != ClassDefaultedByServer {
			t.Errorf("path %q classification = %q, want %q", path, r.Classification, ClassDefaultedByServer)
		}
		if r.SpecFound {
			t.Errorf("path %q: SpecFound = true, want false (client never set it)", path)
		}
	}

	// protectedCookies is declared `type: array` in the schema, so it is
	// a LEAF at its own top-level path — never descended into per
	// element — and the nested disableTamperingProtection difference
	// surfaces as a value-changed verdict on the WHOLE array.
	pc := rowByPath(t, rows, "protectedCookies")
	if pc.Classification != ClassValueChanged {
		t.Errorf("protectedCookies classification = %q, want %q", pc.Classification, ClassValueChanged)
	}
	specArr, _ := pc.SpecValue.([]interface{})
	mirrorArr, _ := pc.MirrorValue.([]interface{})
	if len(specArr) != 1 || len(mirrorArr) != 1 {
		t.Fatalf("protectedCookies: want one element on each side, got spec=%v mirror=%v", pc.SpecValue, pc.MirrorValue)
	}
	mirrorElem, _ := mirrorArr[0].(map[string]interface{})
	if _, ok := mirrorElem["disableTamperingProtection"]; !ok {
		t.Errorf("protectedCookies mirror element missing disableTamperingProtection: %v", mirrorElem)
	}
	specElem, _ := specArr[0].(map[string]interface{})
	if _, ok := specElem["disableTamperingProtection"]; ok {
		t.Errorf("protectedCookies spec element unexpectedly has disableTamperingProtection: %v", specElem)
	}
}

// nestedCRD / nestedObject demonstrate a leaf TWO levels below the schema's
// forProvider/atProvider root (l7DdosProtection.mitigationJsChallenge.customPage
// — the exact shape the F5 XC httploadbalancer bug this port exists to catch
// takes), proving leafPaths' recursive descent and not just its first level.
const nestedCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: f5xc.crossplane.io
  names:
    kind: HttpLoadbalancer
    plural: httploadbalancers
  versions:
  - name: v1alpha1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              forProvider:
                type: object
                properties:
                  l7DdosProtection:
                    type: object
                    properties:
                      mitigationJsChallenge:
                        nullable: true
                        properties:
                          cookieExpiry:
                            type: integer
                          customPage:
                            type: string
                          jsScriptDelay:
                            type: integer
                        type: object
          status:
            type: object
            properties:
              atProvider:
                type: object
                properties:
                  l7DdosProtection:
                    type: object
                    properties:
                      mitigationJsChallenge:
                        nullable: true
                        properties:
                          cookieExpiry:
                            type: integer
                          customPage:
                            type: string
                          jsScriptDelay:
                            type: integer
                        type: object
`

const nestedObject = `{
  "apiVersion": "f5xc.crossplane.io/v1alpha1",
  "kind": "HttpLoadbalancer",
  "metadata": { "name": "example-nested" },
  "spec": {
    "forProvider": {
      "l7DdosProtection": {
        "mitigationJsChallenge": {
          "customPage": "PHA+IFBsZWFzZSBXYWl0IDwvcD4=",
          "jsScriptDelay": 2000
        }
      }
    }
  },
  "status": {
    "atProvider": {
      "l7DdosProtection": {
        "mitigationJsChallenge": {
          "customPage": "",
          "jsScriptDelay": 2000
        }
      }
    }
  }
}`

func TestDiffReportDescendsTwoLevelsForNestedPath(t *testing.T) {
	crd := mustDecodeCRD(t, nestedCRD)
	obj := mustDecodeObject(t, nestedObject)

	rows, err := DiffReport(crd, obj)
	if err != nil {
		t.Fatalf("DiffReport: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2; rows: %+v", len(rows), rows)
	}

	changed := rowByPath(t, rows, "l7DdosProtection.mitigationJsChallenge.customPage")
	if changed.Classification != ClassValueChanged {
		t.Errorf("customPage classification = %q, want %q", changed.Classification, ClassValueChanged)
	}
	if changed.SpecValue != "PHA+IFBsZWFzZSBXYWl0IDwvcD4=" || changed.MirrorValue != "" {
		t.Errorf("customPage values = spec=%v mirror=%v, want the base64 payload / empty string", changed.SpecValue, changed.MirrorValue)
	}

	unchanged := rowByPath(t, rows, "l7DdosProtection.mitigationJsChallenge.jsScriptDelay")
	if unchanged.Classification != ClassEqual {
		t.Errorf("jsScriptDelay classification = %q, want %q", unchanged.Classification, ClassEqual)
	}
}

// ─── unit tests for the individual helpers ─────────────────────────────

func TestLeafPaths(t *testing.T) {
	tests := map[string]struct {
		reason string
		schema string // YAML, decoded before use
		prefix string
		want   []string
	}{
		"NilSchema": {
			reason: "a non-map schema node with no prefix yields nothing to report",
			schema: `null`,
			want:   nil,
		},
		"ScalarLeafAtRoot": {
			reason: "a bare scalar type is a leaf; with no prefix it contributes nothing (matches DiffReport's own top-level call convention)",
			schema: `{type: string}`,
			want:   nil,
		},
		"ScalarLeafWithPrefix": {
			reason: "a scalar under a prefix is the leaf itself",
			schema: `{type: string}`,
			prefix: "description",
			want:   []string{"description"},
		},
		"EmptyObjectMarkerIsALeaf": {
			reason: "type: object with no properties (an empty oneof-selector struct) is a leaf, not descended into",
			schema: `{type: object, nullable: true}`,
			prefix: "allowAllResponseCodes",
			want:   []string{"allowAllResponseCodes"},
		},
		"ArrayIsALeafEvenWithItemProperties": {
			reason: "type: array is never descended into, regardless of what its items schema declares",
			schema: `{type: array, items: {type: object, properties: {name: {type: string}}}}`,
			prefix: "protectedCookies",
			want:   []string{"protectedCookies"},
		},
		"AdditionalPropertiesMapIsALeaf": {
			reason: "a map (additionalProperties) has no fixed properties set and is a leaf",
			schema: `{type: object, additionalProperties: {type: string}}`,
			prefix: "labels",
			want:   []string{"labels"},
		},
		"ObjectWithPropertiesDescendsSortedByName": {
			reason: "an object schema WITH declared properties descends into each, sorted",
			schema: `{type: object, properties: {zebra: {type: string}, alpha: {type: string}}}`,
			prefix: "blockingPage",
			want:   []string{"blockingPage.alpha", "blockingPage.zebra"},
		},
		"TwoLevelNestedDescent": {
			reason: "descent recurses through more than one level of object/properties",
			schema: `
type: object
properties:
  mitigationJsChallenge:
    type: object
    properties:
      customPage: {type: string}
      jsScriptDelay: {type: integer}
`,
			prefix: "l7DdosProtection",
			want:   []string{"l7DdosProtection.mitigationJsChallenge.customPage", "l7DdosProtection.mitigationJsChallenge.jsScriptDelay"},
		},
		"ObjectTypeMissingIsALeafEvenWithProperties": {
			reason: "leafPaths requires an EXPLICIT type: object to descend — a schema node with properties but no type keyword is a leaf, matching the Python reference's schema.get(\"type\") == \"object\" check",
			schema: `{properties: {inner: {type: string}}}`,
			prefix: "untyped",
			want:   []string{"untyped"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var schema interface{}
			if err := yaml.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatalf("decoding schema: %v", err)
			}
			got := leafPaths(schema, tc.prefix)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: leafPaths() = %v, want %v", tc.reason, got, want)
			}
		})
	}
}

func TestGetValue(t *testing.T) {
	obj := map[string]interface{}{
		"a": map[string]interface{}{
			"b": map[string]interface{}{
				"c": "leaf",
			},
			"zero": false,
		},
		"top": "value",
	}

	tests := map[string]struct {
		reason    string
		path      string
		wantValue interface{}
		wantFound bool
	}{
		"TopLevelPresent": {
			reason:    "a single-segment path resolves a top-level key",
			path:      "top",
			wantValue: "value",
			wantFound: true,
		},
		"NestedPresent": {
			reason:    "a multi-segment path descends through nested maps",
			path:      "a.b.c",
			wantValue: "leaf",
			wantFound: true,
		},
		"ZeroValueIsFound": {
			reason:    "a field genuinely set to its zero value (false) is FOUND, not treated as absent",
			path:      "a.zero",
			wantValue: false,
			wantFound: true,
		},
		"MissingTopLevelKey": {
			reason:    "an absent key at any level reports found=false, not a zero value",
			path:      "missing",
			wantValue: nil,
			wantFound: false,
		},
		"MissingNestedKey": {
			path:      "a.b.missing",
			wantValue: nil,
			wantFound: false,
		},
		"IntermediateNodeNotAMap": {
			reason:    "descending through a non-map intermediate (top is a string, not a map) reports absent rather than panicking",
			path:      "top.anything",
			wantValue: nil,
			wantFound: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			v, found := getValue(obj, tc.path)
			if found != tc.wantFound {
				t.Errorf("%s: found = %v, want %v", tc.reason, found, tc.wantFound)
			}
			if found && !reflect.DeepEqual(v, tc.wantValue) {
				t.Errorf("%s: value = %v, want %v", tc.reason, v, tc.wantValue)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]struct {
		reason      string
		specFound   bool
		specVal     interface{}
		mirrorFound bool
		mirrorVal   interface{}
		wantClass   string
		wantOK      bool
	}{
		"BothFoundEqual": {
			specFound: true, specVal: "x", mirrorFound: true, mirrorVal: "x",
			wantClass: ClassEqual, wantOK: true,
		},
		"BothFoundDifferent": {
			specFound: true, specVal: "x", mirrorFound: true, mirrorVal: "y",
			wantClass: ClassValueChanged, wantOK: true,
		},
		"OnlySpecFound": {
			reason:    "the client set it; the mirror never reports it",
			specFound: true, specVal: map[string]interface{}{},
			wantClass: ClassPresentInSpecAbsentFromMirror, wantOK: true,
		},
		"OnlyMirrorFound": {
			reason:      "the client never set it; the backend supplied a value anyway",
			mirrorFound: true, mirrorVal: "server-value",
			wantClass: ClassDefaultedByServer, wantOK: true,
		},
		"NeitherFound": {
			reason: "nothing to observe yet — not reported at all",
			wantOK: false,
		},
		"BothFoundEqualZeroValues": {
			reason:    "a shared zero value (false) on both sides is still equal, not mistaken for absence",
			specFound: true, specVal: false, mirrorFound: true, mirrorVal: false,
			wantClass: ClassEqual, wantOK: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gotClass, gotOK := classify(tc.specFound, tc.specVal, tc.mirrorFound, tc.mirrorVal)
			if gotOK != tc.wantOK {
				t.Fatalf("%s: ok = %v, want %v", tc.reason, gotOK, tc.wantOK)
			}
			if gotOK && gotClass != tc.wantClass {
				t.Errorf("%s: classification = %q, want %q", tc.reason, gotClass, tc.wantClass)
			}
		})
	}
}

func TestFindCRD(t *testing.T) {
	dir := t.TempDir()
	crdsDir := filepath.Join(dir, "package", "crds")
	if err := os.MkdirAll(crdsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(crdsDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	writeFile("f5xc.crossplane.io_appfirewalls.yaml", appFirewallCRD)
	writeFile("malformed.yaml", "not: [valid: yaml: at: all")
	writeFile("not-a-crd.yaml", "apiVersion: v1\nkind: ConfigMap\n")
	writeFile("ignored.txt", "not a yaml file at all")

	t.Run("MatchByGroupAndKind", func(t *testing.T) {
		crd, plural := FindCRD(dir, "f5xc.crossplane.io/v1alpha1", "AppFirewall")
		if crd == nil {
			t.Fatal("FindCRD returned nil, want a match")
		}
		if plural != "appfirewalls" {
			t.Errorf("plural = %q, want %q", plural, "appfirewalls")
		}
	})

	t.Run("NoMatchingKind", func(t *testing.T) {
		crd, _ := FindCRD(dir, "f5xc.crossplane.io/v1alpha1", "NoSuchKind")
		if crd != nil {
			t.Errorf("FindCRD = %v, want nil for an unmatched kind", crd)
		}
	})

	t.Run("NoMatchingGroup", func(t *testing.T) {
		crd, _ := FindCRD(dir, "other.example.io/v1", "AppFirewall")
		if crd != nil {
			t.Errorf("FindCRD = %v, want nil for an unmatched group", crd)
		}
	})

	t.Run("MalformedFileIsSkippedNotFatal", func(t *testing.T) {
		// The malformed.yaml and not-a-crd.yaml fixtures sit alongside
		// the real match in the same directory; a successful match
		// proves they were skipped rather than aborting the scan.
		crd, _ := FindCRD(dir, "f5xc.crossplane.io/v1alpha1", "AppFirewall")
		if crd == nil {
			t.Fatal("FindCRD returned nil; a malformed sibling file must not abort the scan")
		}
	})

	t.Run("MissingCRDsDirectory", func(t *testing.T) {
		crd, plural := FindCRD(t.TempDir(), "f5xc.crossplane.io/v1alpha1", "AppFirewall")
		if crd != nil || plural != "" {
			t.Errorf("FindCRD = (%v, %q), want (nil, \"\") for a root with no package/crds at all", crd, plural)
		}
	})

	t.Run("APIVersionWithoutSlash", func(t *testing.T) {
		// A defensive case: an apiVersion with no "/" is its own group
		// (core/v1-shaped), and must not panic on the split.
		crd, _ := FindCRD(dir, "onlygroup", "AppFirewall")
		if crd != nil {
			t.Errorf("FindCRD = %v, want nil", crd)
		}
	})
}

func TestFormatReportAndFindingsLines(t *testing.T) {
	rows := []Row{
		{Path: "allowAllResponseCodes", Classification: ClassPresentInSpecAbsentFromMirror, SpecValue: map[string]interface{}{}, SpecFound: true},
		{Path: "id", Classification: ClassPresentInMirrorAbsentFromSpec, MirrorValue: "example", MirrorFound: true},
		{Path: "disable", Classification: ClassEqual, SpecValue: true, SpecFound: true, MirrorValue: true, MirrorFound: true},
	}

	t.Run("FormatReportHasKindNameHeader", func(t *testing.T) {
		got := FormatReport("AppFirewall", "example-app-firewall", rows)
		if !strings.HasPrefix(got, "roundtrip-diff: AppFirewall/example-app-firewall\n") {
			t.Errorf("FormatReport does not start with the expected header:\n%s", got)
		}
		for _, want := range []string{"allowAllResponseCodes", "present-in-mirror-absent-from-spec", "-- 3 field(s) classified"} {
			if !strings.Contains(got, want) {
				t.Errorf("FormatReport output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("FormatFindingsLinesHasNoHeader", func(t *testing.T) {
		lines := FormatFindingsLines(rows)
		if len(lines) != len(rows)+1 {
			t.Fatalf("len(lines) = %d, want %d (one per row plus a tally line)", len(lines), len(rows)+1)
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "roundtrip-diff:") {
				t.Errorf("FormatFindingsLines must carry no kind/name header, got line %q", l)
			}
		}
		tally := lines[len(lines)-1]
		if !strings.Contains(tally, "3 field(s) classified") {
			t.Errorf("tally line = %q, want it to report 3 fields classified", tally)
		}
	})

	t.Run("EmptyRowsStillProducesATallyLine", func(t *testing.T) {
		lines := FormatFindingsLines(nil)
		if len(lines) != 1 {
			t.Fatalf("len(lines) = %d, want 1 (just the tally)", len(lines))
		}
		if !strings.Contains(lines[0], "0 field(s) classified (none)") {
			t.Errorf("tally line = %q, want the zero-fields/none form", lines[0])
		}
	})
}

func TestFormatValueDistinguishesAbsentFromAnyPresentValue(t *testing.T) {
	// The absent token must never collide with a value that legitimately
	// marshals to similar text (e.g. an empty string), or a reader could
	// not tell "field never set" apart from "field set to the value that
	// happens to render the same way".
	if got := formatValue(false, nil); got != "<absent>" {
		t.Errorf("formatValue(false, nil) = %q, want the absent token", got)
	}
	if got := formatValue(true, ""); got == "<absent>" {
		t.Errorf("formatValue(true, \"\") must not render as the absent token, got %q", got)
	}
}
