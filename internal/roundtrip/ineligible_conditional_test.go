package roundtrip

import (
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// guardedRule is the shape measured on provider-github's ActionsSecretAccess
// and DependabotSecretAccess forProvider node: the list is required, and
// non-empty, only while visibility is 'selected'.
const guardedRule = "!has(self.visibility) || self.visibility != 'selected' || (has(self.selectedRepositories) && size(self.selectedRepositories) > 0)"

func ruleList(rule string) []interface{} {
	return []interface{}{map[string]interface{}{"rule": rule, "message": "m"}}
}

func strProp() map[string]interface{} { return map[string]interface{}{"type": "string"} }

func listProp() map[string]interface{} {
	return map[string]interface{}{"type": "array", "items": strProp()}
}

// sizeGuardCRD builds a Widget CRD whose forProvider declares visibility,
// selectedRepositories, other (a second list) and nested{visibility,
// selectedRepositories}. rules is attached at the node named by anchor:
// "root", "spec", "forProvider" or "nested".
func sizeGuardCRD(anchor, rule string) map[string]interface{} {
	nested := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"visibility":           strProp(),
			"selectedRepositories": listProp(),
		},
	}
	forProvider := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"visibility":           strProp(),
			"selectedRepositories": listProp(),
			"other":                listProp(),
			"nested":               nested,
		},
	}
	spec := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"forProvider": forProvider,
		},
	}
	root := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"spec": spec,
			"status": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"atProvider": map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	switch anchor {
	case "root":
		root["x-kubernetes-validations"] = ruleList(rule)
	case "spec":
		spec["x-kubernetes-validations"] = ruleList(rule)
	case "forProvider":
		forProvider["x-kubernetes-validations"] = ruleList(rule)
	case "nested":
		nested["x-kubernetes-validations"] = ruleList(rule)
	}
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"versions": []interface{}{map[string]interface{}{
				"name":   "v1alpha1",
				"served": true,
				"schema": map[string]interface{}{"openAPIV3Schema": root},
			}},
		},
	}
}

// TestContainerClearCoverageSizeGuardAnchorsAndForms drives the non-empty
// rule recognition across every anchor a rule can sit on and both spellings
// of the clause, with the condition-evaluation outcomes for a guarded rule.
func TestContainerClearCoverageSizeGuardAnchorsAndForms(t *testing.T) {
	selected := map[string]interface{}{"visibility": "selected"}
	cases := map[string]struct {
		reason string
		anchor string
		rule   string
		// leaf is the path under test; fp is the manifest's forProvider.
		leaf string
		fp   map[string]interface{}
		// wantIneligible is the expected classification of leaf.
		wantIneligible bool
		// wantInReason are substrings the reason must carry.
		wantInReason []string
		// wantNotInReason are substrings the reason must not carry.
		wantNotInReason []string
	}{
		"FunctionFormAtForProviderNode": {
			reason: "function form size(<path>) > 0 on an unguarded forProvider-node rule",
			anchor: "forProvider", rule: "size(self.selectedRepositories) > 0",
			leaf: "selectedRepositories", wantIneligible: true,
			wantInReason:    []string{"size(self.selectedRepositories) > 0"},
			wantNotInReason: []string{"while"},
		},
		"FunctionFormGreaterEqualOne": {
			reason: "function form >= 1 spelling",
			anchor: "forProvider", rule: "size( self.selectedRepositories ) >= 1",
			leaf: "selectedRepositories", wantIneligible: true,
			wantInReason: []string{"size( self.selectedRepositories ) >= 1"},
		},
		"MethodFormAtForProviderNode": {
			reason: "method form <path>.size() > 0 on a forProvider-node rule",
			anchor: "forProvider", rule: "self.selectedRepositories.size() > 0",
			leaf: "selectedRepositories", wantIneligible: true,
			wantInReason: []string{"self.selectedRepositories.size() > 0"},
		},
		"FunctionFormAtRoot": {
			reason: "function form at the CRD root anchor",
			anchor: "root", rule: "size(self.spec.forProvider.selectedRepositories) > 0",
			leaf: "selectedRepositories", wantIneligible: true,
			wantInReason: []string{"size(self.spec.forProvider.selectedRepositories) > 0"},
		},
		"FunctionFormAtSpec": {
			reason: "function form at the spec anchor",
			anchor: "spec", rule: "size(self.forProvider.selectedRepositories) > 0",
			leaf: "selectedRepositories", wantIneligible: true,
			wantInReason: []string{"size(self.forProvider.selectedRepositories) > 0"},
		},
		"MethodFormAtNestedAncestor": {
			reason: "method form on an object node between forProvider and the leaf",
			anchor: "nested", rule: "self.selectedRepositories.size() > 0",
			leaf: "nested.selectedRepositories", wantIneligible: true,
			wantInReason: []string{"self.selectedRepositories.size() > 0"},
		},
		"FunctionFormAtNestedAncestor": {
			reason: "function form on an object node between forProvider and the leaf",
			anchor: "nested", rule: "size(self.selectedRepositories) >= 1",
			leaf: "nested.selectedRepositories", wantIneligible: true,
			wantInReason: []string{"size(self.selectedRepositories) >= 1"},
		},
		"GuardOnDifferentFieldDoesNotMatch": {
			reason: "a size() guard on a DIFFERENT field never guards this leaf",
			anchor: "forProvider", rule: "size(self.other) > 0",
			leaf: "selectedRepositories", wantIneligible: false,
		},
		"MethodGuardOnDifferentFieldDoesNotMatch": {
			reason: "method form on a different field never guards this leaf",
			anchor: "forProvider", rule: "self.other.size() > 0",
			leaf: "selectedRepositories", wantIneligible: false,
		},
		"NestedGuardDoesNotGuardTopLevelSibling": {
			reason: "a rule on the nested node names nested.selectedRepositories, not the top-level leaf of the same name",
			anchor: "nested", rule: "size(self.selectedRepositories) > 0",
			leaf: "selectedRepositories", wantIneligible: false,
		},
		"NestedLeafNotGuardedByForProviderNodeRuleOnTopLevelName": {
			reason: "a forProvider-node rule on the top-level name does not guard the nested leaf",
			anchor: "forProvider", rule: "size(self.selectedRepositories) > 0",
			leaf: "nested.selectedRepositories", wantIneligible: false,
		},
		"ConditionalConditionMet": {
			reason: "guarded rule, visibility == 'selected' on this manifest: the rule binds",
			anchor: "forProvider", rule: guardedRule,
			leaf: "selectedRepositories", fp: selected, wantIneligible: true,
			wantInReason:    []string{"size(self.selectedRepositories) > 0", "forProvider.visibility == 'selected'", "conditional"},
			wantNotInReason: []string{"standing, schema-level statement"},
		},
		"ConditionalConditionNotMet": {
			reason: "guarded rule, visibility == 'all': value: [] is admitted, leaf stays eligible",
			anchor: "forProvider", rule: guardedRule,
			leaf: "selectedRepositories", fp: map[string]interface{}{"visibility": "all"}, wantIneligible: false,
		},
		"ConditionalVisibilityAbsent": {
			reason: "guarded rule, visibility unset: value: [] is admitted, leaf stays eligible",
			anchor: "forProvider", rule: guardedRule,
			leaf: "selectedRepositories", wantIneligible: false,
		},
		"ConditionalMethodFormConditionMet": {
			reason: "guarded rule written in method form",
			anchor: "forProvider",
			rule:   "!has(self.visibility) || self.visibility != 'selected' || self.selectedRepositories.size() > 0",
			leaf:   "selectedRepositories", fp: selected, wantIneligible: true,
			wantInReason: []string{"forProvider.visibility == 'selected'"},
		},
		"ConditionalAtRootConditionMet": {
			reason: "guarded rule at the CRD root: guard paths resolve under self.spec.forProvider",
			anchor: "root",
			rule:   "!has(self.spec.forProvider.visibility) || self.spec.forProvider.visibility != 'selected' || size(self.spec.forProvider.selectedRepositories) > 0",
			leaf:   "selectedRepositories", fp: selected, wantIneligible: true,
			wantInReason: []string{"forProvider.visibility == 'selected'"},
		},
		"ConditionalAtRootConditionNotMet": {
			reason: "guarded rule at the CRD root with the condition unmet",
			anchor: "root",
			rule:   "!has(self.spec.forProvider.visibility) || self.spec.forProvider.visibility != 'selected' || size(self.spec.forProvider.selectedRepositories) > 0",
			leaf:   "selectedRepositories", fp: map[string]interface{}{"visibility": "all"}, wantIneligible: false,
		},
		"ConditionalAtSpecConditionMet": {
			reason: "guarded rule on the spec node",
			anchor: "spec",
			rule:   "!has(self.forProvider.visibility) || self.forProvider.visibility != 'selected' || size(self.forProvider.selectedRepositories) > 0",
			leaf:   "selectedRepositories", fp: selected, wantIneligible: true,
			wantInReason: []string{"forProvider.visibility == 'selected'"},
		},
		"ConditionalNestedAncestorConditionMet": {
			reason: "guarded rule on a nested object: guard paths resolve below that node",
			anchor: "nested", rule: guardedRule,
			leaf: "nested.selectedRepositories",
			fp:   map[string]interface{}{"nested": map[string]interface{}{"visibility": "selected"}}, wantIneligible: true,
			wantInReason: []string{"forProvider.nested.visibility == 'selected'"},
		},
		"ConditionalNestedAncestorConditionOnWrongLevel": {
			reason: "the top-level visibility does not satisfy a rule whose self is the nested node",
			anchor: "nested", rule: guardedRule,
			leaf: "nested.selectedRepositories", fp: selected, wantIneligible: false,
		},
		"UnrecognisedLeadingDisjunctStaysUnconditional": {
			reason: "a leading disjunct of another shape cannot be evaluated: legacy unconditional match",
			anchor: "forProvider",
			rule:   "self.other.size() == 0 || size(self.selectedRepositories) > 0",
			leaf:   "selectedRepositories", wantIneligible: true,
			wantInReason:    []string{"size(self.selectedRepositories) > 0"},
			wantNotInReason: []string{"while"},
		},
		"GuardPathOutsideForProviderStaysUnconditional": {
			reason: "a guard reading a path outside forProvider cannot be evaluated",
			anchor: "spec",
			rule:   "!has(self.paused) || self.paused != 'yes' || size(self.forProvider.selectedRepositories) > 0",
			leaf:   "selectedRepositories", wantIneligible: true,
			wantNotInReason: []string{"while"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			crd := sizeGuardCRD(tc.anchor, tc.rule)
			// Every leaf is present on the manifest so the only reason
			// available is the one under test, never absent-from-manifest.
			fp := map[string]interface{}{
				"selectedRepositories": []interface{}{"a"},
				"other":                []interface{}{"a"},
				"nested":               map[string]interface{}{"selectedRepositories": []interface{}{"a"}},
			}
			for k, v := range tc.fp {
				if sub, ok := v.(map[string]interface{}); ok && k == "nested" {
					nested := fp["nested"].(map[string]interface{})
					for nk, nv := range sub {
						nested[nk] = nv
					}
					continue
				}
				fp[k] = v
			}
			findings, err := ContainerClearCoverage(crd, &manifest.Manifest{ForProvider: fp})
			if err != nil {
				t.Fatalf("%s: ContainerClearCoverage: %v", tc.reason, err)
			}
			f, ok := findingsByPath(findings)[tc.leaf]
			if !ok {
				t.Fatalf("%s: %s not found in findings: %+v", tc.reason, tc.leaf, findings)
			}
			if f.Ineligible != tc.wantIneligible {
				t.Fatalf("%s: %s Ineligible = %v, want %v (reason %q)", tc.reason, tc.leaf, f.Ineligible, tc.wantIneligible, f.Reason)
			}
			for _, want := range tc.wantInReason {
				if !strings.Contains(string(f.Reason), want) {
					t.Errorf("%s: reason %q does not contain %q", tc.reason, f.Reason, want)
				}
			}
			for _, bad := range tc.wantNotInReason {
				if strings.Contains(string(f.Reason), bad) {
					t.Errorf("%s: reason %q must not contain %q", tc.reason, f.Reason, bad)
				}
			}
		})
	}
}

// TestSplitTopLevelOr pins the disjunct splitter the guard evaluation rests
// on: only `||` outside parentheses and string literals separates disjuncts.
func TestSplitTopLevelOr(t *testing.T) {
	cases := map[string]struct {
		rule string
		want []string
	}{
		"single":            {rule: "a", want: []string{"a"}},
		"three":             {rule: "!has(self.v) || self.v != 'x' || size(self.l) > 0", want: []string{"!has(self.v)", "self.v != 'x'", "size(self.l) > 0"}},
		"parenthesised":     {rule: "!('a' in self.m || 'b' in self.m) || size(self.l) > 0", want: []string{"!('a' in self.m || 'b' in self.m)", "size(self.l) > 0"}},
		"quotedOperator":    {rule: "self.v != 'a||b' || size(self.l) > 0", want: []string{"self.v != 'a||b'", "size(self.l) > 0"}},
		"singlePipeIgnored": {rule: "a | b", want: []string{"a | b"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := splitTopLevelOr(tc.rule)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("splitTopLevelOr(%q) = %q, want %q", tc.rule, got, tc.want)
			}
		})
	}
}

// TestStripOuterParens covers parentheses that enclose the whole string
// versus ones that merely start and end it.
func TestStripOuterParens(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"none":         {in: "a && b", want: "a && b"},
		"enclosing":    {in: "(a && b)", want: "a && b"},
		"double":       {in: "((a))", want: "a"},
		"notEnclosing": {in: "(a) && (b)", want: "(a) && (b)"},
		"empty":        {in: "", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := stripOuterParens(tc.in); got != tc.want {
				t.Errorf("stripOuterParens(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestValueAtPath covers lookup through nested maps, absence, and a
// non-map intermediate.
func TestValueAtPath(t *testing.T) {
	root := map[string]interface{}{
		"a": map[string]interface{}{"b": "v", "n": nil},
		"s": "scalar",
	}
	cases := map[string]struct {
		path    string
		want    interface{}
		present bool
	}{
		"nested":        {path: "a.b", want: "v", present: true},
		"explicitNull":  {path: "a.n", want: nil, present: true},
		"missingLeaf":   {path: "a.x"},
		"missingTop":    {path: "z"},
		"throughScalar": {path: "s.x"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := valueAtPath(root, tc.path)
			if ok != tc.present || got != tc.want {
				t.Errorf("valueAtPath(%q) = (%v, %v), want (%v, %v)", tc.path, got, ok, tc.want, tc.present)
			}
		})
	}
	if _, ok := valueAtPath(nil, "a"); ok {
		t.Errorf("valueAtPath(nil, a) reported present")
	}
}
