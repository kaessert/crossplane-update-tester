package roundtrip

import "regexp"

// reCELImmutable matches a CEL "self == oldSelf" comparison inside an
// x-kubernetes-validations rule string — the CRD-side counterpart of
// validator's own field-declaration regex, matched against the exact same
// pattern so a field is classified immutable identically whether the check
// reads generated Go source or the CRD schema generated from it.
//
// It deliberately does NOT match the sibling "!has(oldSelf.x) || has(self.x)"
// rule shape a parent object also carries — that rule guards one named
// field against being REMOVED once set; it says nothing about the CURRENT
// schema node being immutable, and matching it would misclassify every
// field in an object as immutable merely because one sibling happens to
// carry its own remove-guard.
var reCELImmutable = regexp.MustCompile(`self\s*==\s*oldSelf`)

// immutablePaths walks an OpenAPI object schema node (the same
// map[string]interface{}/[]interface{}/scalar shape leafPaths already
// operates on) and returns the set of LEAF field paths — dot-separated,
// matching Row.Path exactly — that are CEL-immutable: either the leaf's own
// schema node carries an x-kubernetes-validations rule matching
// reCELImmutable, or an ancestor object node does.
//
// The inheritance step matters: a marker such as "capabilities is immutable
// after creation" is declared once, on the "capabilities" object node
// itself, and covers every field nested beneath it — but DiffReport's rows
// are always reported at the LEAF path, never at an ancestor's own path (an
// object node with declared properties is never itself a leaf — see
// leafPaths), so a marker that stopped at the ancestor would never match a
// single Row.Path. Pushing it down to every leaf it covers is what makes it
// actually usable against rows.
func immutablePaths(schema interface{}) map[string]bool {
	out := make(map[string]bool)
	collectImmutablePaths(schema, "", false, out)
	return out
}

// collectImmutablePaths performs the walk immutablePaths documents,
// mirroring leafPaths' own traversal rules (an object schema node descends
// only when it declares an explicit "type: object" AND a non-empty
// "properties" map; anything else — a scalar, an array, an untyped node, or
// an empty object marker — is a leaf) so the two functions agree on exactly
// which paths are leaves.
func collectImmutablePaths(schema interface{}, prefix string, inherited bool, out map[string]bool) {
	m, ok := schema.(map[string]interface{})
	if !ok {
		if prefix != "" && inherited {
			out[prefix] = true
		}
		return
	}

	immutable := inherited || nodeHasImmutableMarker(m)

	typ, _ := m["type"].(string)
	props, hasProps := m["properties"].(map[string]interface{})
	if typ == "object" && hasProps && len(props) > 0 {
		for name, sub := range props {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			collectImmutablePaths(sub, path, immutable, out)
		}
		return
	}

	if prefix != "" && immutable {
		out[prefix] = true
	}
}

// nodeHasImmutableMarker reports whether schema node m itself carries an
// x-kubernetes-validations rule matching reCELImmutable.
func nodeHasImmutableMarker(m map[string]interface{}) bool {
	validations, _ := m["x-kubernetes-validations"].([]interface{})
	for _, v := range validations {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		rule, _ := vm["rule"].(string)
		if reCELImmutable.MatchString(rule) {
			return true
		}
	}
	return false
}
