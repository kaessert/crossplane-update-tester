package roundtrip

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// IneligibilityReason names why a declared container leaf's removal
// direction can never be exercised at all — see classifyIneligibility's own
// doc comment for what each is derived from. Four causes produce
// it: crossplane-runtime's own reference-resolution plumbing
// (ReasonReferenceResolution), a CEL "self == oldSelf" immutability rule on
// the leaf or an enclosing ancestor (ReasonCELImmutable), a LIST or MAP
// leaf whose own schema closes its clear route, and the leaf carrying no
// value anywhere on THIS manifest under test at all
// (ReasonAbsentFromManifest). The third cause renders as one of three
// concrete reasons depending on the leaf's own shape and schema:
//
//   - a free-form MAP leaf required by an x-kubernetes-validations rule
//     under the object's default managementPolicies (ReasonRequiredByCELMap)
//     — nulling it is rejected by that rule's has() guard, and `value: {}`
//     is an RFC-7386 no-op, so the presence rule alone is enough;
//   - a LIST leaf has a removal route a MAP leaf does not: `value: []` is
//     RFC-7386 wholesale replacement (the same route selfTombstoned already
//     credits). Whether that route itself is closed — by the leaf's own
//     `minItems > 0` or a second CEL rule requiring `<path>.size() > 0` /
//     `size(<path>) > 0`, declared at the CRD root, the spec node, the
//     forProvider node or any object node between forProvider and the leaf —
//     is checked independently of any presence rule: it answers a separate
//     question (can the list ever be emptied at all) from whether it is
//     also required to be present, and it is sufficient ON ITS OWN to make
//     the leaf ineligible, whether or not a presence rule also applies.
//     The reason text differs by which guards are in force: both a
//     presence rule AND a closed empty-list route (listRequiredByCELReason)
//     name the presence rule's has() guard alongside the empty-list
//     blocker; a closed empty-list route with NO presence rule
//     (listNeverEmptyReason) names only the empty-list blocker, and says so
//     — an explicit whole-field tombstone is not rejected by that blocker
//     and still validates at admission, but the blocker is a standing,
//     schema-level statement that the list itself must never be emptied. A
//     LIST leaf whose empty-list route stays open is not ineligible at all,
//     whether or not it is CEL-required: it is left out of the map
//     entirely, exactly like any other eligible leaf. A size() rule led by
//     `!has(self.X)` / `self.X != 'V'` guard disjuncts is conditional: it
//     closes the route only on a manifest whose forProvider satisfies every
//     guard (listConditionallyEmptyReason names the condition), and a
//     manifest that does not satisfy it keeps the leaf eligible.
//
// The first three reasons are derived from crd's own schema — the same
// answer for every manifest of the Kind — with one exception: a list leaf
// closed by a conditional size() rule is evaluated against the manifest's
// own forProvider (see above). ReasonAbsentFromManifest is the odd one out,
// and deliberately so: it is the only reason that reads the manifest under
// test's own tests (m.Tests) and presence data rather than the schema
// alone, because the question it answers — "does THIS
// object ever carry a value here" — has no schema-only answer. That is
// sound rather than a layering violation: ContainerClearCoverage's own
// coverage half (coverageFor) already reads exactly the same manifest data
// to decide whether a leaf's removal was exercised; this reason reads it to
// decide whether the leaf's PRESENCE was ever exercised, the strictly prior
// question.
type IneligibilityReason string

const (
	// ReasonReferenceResolution reports that the leaf is
	// crossplane-runtime's own cross-resource reference-resolution
	// plumbing — a Selector's matchLabels map, or a *Refs list of
	// Reference/NamespacedReference items — resolved entirely by
	// crossplane-runtime before a request ever reaches the provider. It
	// can never appear in status.atProvider and a clear-direction test
	// against it would only ever exercise kube-apiserver's own
	// merge-patch handling, never anything the provider does.
	ReasonReferenceResolution IneligibilityReason = "reference-resolution field: resolved by crossplane-runtime before the request reaches the provider; never mirrored in status.atProvider"

	// ReasonRequiredByCELMap reports that a free-form MAP leaf is required
	// by an x-kubernetes-validations rule under the default
	// managementPolicies. Nulling it is rejected by that rule's has()
	// guard, and `value: {}` is an RFC-7386 no-op — RFC 7396 recurses into
	// an object-valued patch member and merges it key-by-key against the
	// live object, so an empty map names no member to remove and the live
	// map survives the patch completely unchanged. With both routes
	// closed, no clear-direction test can ever reach the backend.
	ReasonRequiredByCELMap IneligibilityReason = "required by a CEL validation rule under the default managementPolicies; nulling it is rejected by that rule's has() guard, and `value: {}` is an RFC-7386 no-op that leaves every existing member untouched — no clear-direction test can ever reach the backend"

	// ReasonCELImmutable reports that the leaf's own schema node, or an
	// ancestor object node enclosing it, carries an x-kubernetes-validations
	// rule requiring `self == oldSelf` (see immutable.go's reCELImmutable
	// and immutablePaths, reused here rather than re-walked). That is a
	// strictly stronger block than either reason above: those reject only a
	// specific patch shape (a null, or an empty value against a has()
	// guard); this one rejects EVERY mutation of the field, so there is no
	// alternate route left to try — nulling, emptying, or setting any other
	// value are all rejected identically. No clear-direction test can ever
	// reach the backend.
	ReasonCELImmutable IneligibilityReason = "CEL-immutable: an x-kubernetes-validations rule requires self == oldSelf on this leaf or an enclosing ancestor, so admission rejects EVERY mutation of the field — not merely a null or empty-value patch, unlike the has()-guard reasons above — and no clear-direction test can ever reach the backend"

	// ReasonAbsentFromManifest reports that leaf carries no value anywhere
	// on the manifest under test — not in its own spec.forProvider, and
	// not introduced by any TESTED (non-skip) update-test entry either,
	// whether as that entry's own field-value pair or as a literal value
	// named in its withValues: map. There is nothing on the object this
	// fixture creates for a clear-direction test to remove, so no test
	// anyone could write against THIS manifest would ever satisfy the
	// obligation.
	//
	// Unlike the three reasons above, this one is NOT a statement about
	// the Kind's schema — it is a statement about this one fixture. A leaf
	// excluded here may still be eligible, and covered, on a sibling
	// manifest of the same Kind that does populate it (a separate
	// DeclaredContainerLeaves/classifyAbsentFromManifest run against that
	// manifest's own data). It is also assigned only when the leaf's own
	// (Shape, Depth) cell — see CellKey and GroupClearCells — carries no
	// OTHER member with direct coverageFor coverage of its own: a cell
	// credits every eligible member the moment ANY one of them is
	// genuinely tested (one representative covers the whole cell), so
	// stripping an untested sibling out of that cell's eligible set would
	// withdraw a credit the report already grants it, which coverage must
	// never do. An ancestor tombstone that legitimately sweeps a subtree
	// holding no such key on this particular object is a real, credited
	// clear either way — never a contradiction — and
	// ContainerClearCoverage's own ineligible/covered guard is written to
	// never see this reason paired with covered: true at all (see that
	// guard's own comment).
	ReasonAbsentFromManifest IneligibilityReason = "absent from this manifest: neither spec.forProvider nor any tested update-test entry gives this leaf a value on the object under test, so its removal direction cannot be exercised here — a sibling manifest of the same Kind that does populate it may still cover it"
)

// listRequiredByCELReason builds the ineligibility reason for a LIST leaf
// required by an x-kubernetes-validations rule whose own empty-list route
// is ALSO closed. blocker names the actual thing closing it — "minItems: N"
// or a size() CEL rule's own text — so the reason a reader sees is never
// the generic "admission rejects nulling it": nulling was never the only
// route for a list, and it is not what makes THIS leaf different from every
// other CEL-required list leaf that stays eligible.
func listRequiredByCELReason(blocker string) IneligibilityReason {
	return IneligibilityReason(fmt.Sprintf(
		"required by a CEL validation rule under the default managementPolicies; nulling it is rejected by that rule's has() guard, and %s also rejects emptying it to `value: []` — %s, not the has() guard alone, is why this leaf (unlike an ordinary CEL-required list) stays ineligible",
		blocker, blocker))
}

// listNeverEmptyReason builds the ineligibility reason for a LIST leaf
// whose OWN schema closes the `value: []` clear route — via `minItems > 0`
// or a second x-kubernetes-validations rule requiring `<path>.size() > 0` —
// with NO x-kubernetes-validations rule requiring the leaf's presence
// under the default managementPolicies. blocker names the actual thing
// closing the route, exactly as listRequiredByCELReason's own blocker
// does.
//
// Unlike listRequiredByCELReason's case, nothing here rejects an explicit
// whole-field tombstone: with no presence rule, removing the leaf entirely
// (`value: null`, or a sibling/ancestor clear:) still validates at
// admission, because minItems and a size() guard both constrain a PRESENT
// list's own length and say nothing about the leaf being absent. blocker
// is nonetheless a standing, schema-level statement — present on every
// manifest of the Kind, not a per-manifest choice — that the list itself
// must never be emptied, and closing the `value: []` route is sufficient
// on its own: listEmptyRouteBlocked answers a question no presence rule
// ever asked.
func listNeverEmptyReason(blocker string) IneligibilityReason {
	return IneligibilityReason(fmt.Sprintf(
		"%s closes this list's `value: []` clear route on its own; no x-kubernetes-validations rule requires the leaf's presence, so an explicit whole-field tombstone (`value: null`, or a sibling/ancestor clear:) is not rejected by %s and still validates at admission — %s is nonetheless a standing, schema-level statement that the list itself must never be emptied, and closing that route alone is why this leaf is ineligible",
		blocker, blocker, blocker))
}

// listConditionallyEmptyReason builds the ineligibility reason for a LIST
// leaf whose `value: []` route is closed by a GUARDED non-empty rule — one
// that binds only while forProvider satisfies the rule's own condition, which
// blocker names. It is a statement about THIS manifest, not about the Kind:
// a manifest that does not set the condition is admitted with `value: []`
// and keeps the leaf eligible. The rule's body is not read further, so
// nothing is claimed about an explicit whole-field tombstone.
func listConditionallyEmptyReason(blocker string) IneligibilityReason {
	return IneligibilityReason(fmt.Sprintf(
		"%s on this manifest, so admission rejects emptying this list to `value: []`; the rule is conditional and binds only because this manifest's forProvider satisfies that condition",
		blocker))
}

// classifyIneligibility derives, for every leaf in leaves, whether its
// removal direction can ever be exercised against crd's schema at all,
// returning two maps. structural holds the HARD reasons — both of a LIST
// leaf's two clear routes closed (ReasonCELImmutable, ReasonReferenceResolution,
// ReasonRequiredByCELMap, or listRequiredByCELReason), where the admission
// rule a "covered" manifest entry would have to have passed is the SAME
// rule this function already found closes the route: a manifest claiming
// coverage anyway is evidence the predicate or the manifest disagrees, and
// ContainerClearCoverage's own hard-error contradiction check is written
// to treat it that way. markerOnly holds listNeverEmptyReason candidates —
// a LIST leaf whose `value: []` route alone is closed, with NO presence
// rule also in force. Unlike structural, closing only ONE of the two
// routes never rejects an admission-accepted whole-field tombstone
// (`value: null`, or a sibling/ancestor clear:), so a manifest that
// credits one of THESE leaves via that route is not evidence of anything
// wrong — it is a genuine, working clear test. ContainerClearCoverage
// reconciles markerOnly against each leaf's own coverage before finalizing
// ineligibility, exactly as it already reconciles ReasonAbsentFromManifest
// against cellCovered: coverage already earned is never withdrawn.
//
// Both maps are derived from the Kind's schema — the same answer for every
// manifest of that Kind — except that a conditional size() rule is
// evaluated against forProvider, the manifest's own spec.forProvider, and
// binds only where its condition holds. They are re-derived on EVERY call: nothing
// here is a hardcoded list, a per-provider config, or an annotation a
// human must remember to update, so a CRD change that removes the shape or
// the rule puts the leaf back in the denominator automatically on the very
// next run.
//
// The fourth reason, ReasonAbsentFromManifest, is NOT derived here — it
// reads the manifest under test's own data rather than the schema, and
// needs a cell-level coverage lookup only the caller can build (see
// classifyAbsentFromManifest and ContainerClearCoverage's own
// construction).
//
// A leaf matching none of the reasons in IneligibilityReason's own doc
// comment is left out of both returned maps entirely; it is not
// ineligible.
func classifyIneligibility(crd map[string]interface{}, leaves []ContainerLeaf, forProvider map[string]interface{}) (structural, markerOnly map[string]IneligibilityReason, err error) {
	schema, err := servedSchema(crd)
	if err != nil {
		return nil, nil, err
	}
	fpSchema, err := fieldSchema(schema, "spec", "forProvider")
	if err != nil {
		return nil, nil, err
	}
	apSchema, err := fieldSchema(schema, "status", "atProvider")
	if err != nil {
		return nil, nil, err
	}
	apPaths := make(map[string]bool)
	for _, p := range leafPaths(apSchema, "") {
		apPaths[p] = true
	}

	// Reused rather than re-walked: immutablePaths already performs the
	// exact ancestor-inheriting traversal this classification needs, and
	// DiffReport already calls it against this same fpSchema node — a
	// second implementation of the same walk would be a rejection on
	// review even where it computes the identical answer.
	immutable := immutablePaths(fpSchema)

	mpDefault, hasDefault := managementPoliciesDefault(schema)

	structural = make(map[string]IneligibilityReason, len(leaves))
	markerOnly = make(map[string]IneligibilityReason)
	for _, leaf := range leaves {
		if referenceResolutionShape(fpSchema, leaf, apPaths) {
			structural[leaf.Path] = ReasonReferenceResolution
			continue
		}
		if immutable[leaf.Path] {
			structural[leaf.Path] = ReasonCELImmutable
			continue
		}
		requiredByCEL := hasDefault && requiredByManagementPolicies(schema, leaf.Path, mpDefault)
		if requiredByCEL && leaf.Shape == ShapeMap {
			structural[leaf.Path] = ReasonRequiredByCELMap
			continue
		}
		if leaf.Shape != ShapeList {
			continue
		}
		// listEmptyRouteBlocked is consulted here ONCE, in its own branch,
		// for every list leaf — checked independently of requiredByCEL
		// above: whether the list's own `value: []` route is closed
		// answers a SEPARATE question (can it be emptied at all) from
		// whether it is also required to be present, and is sufficient ON
		// ITS OWN to make the leaf a markerOnly candidate. requiredByCEL
		// only changes which map (and which reason text) the leaf lands
		// in, never whether this leaf is checked at all.
		blocker, conditional, blocked := listEmptyRouteBlocked(fpSchema, schema, leaf.Path, forProvider)
		if !blocked {
			continue
		}
		if requiredByCEL {
			// Both routes closed: a hard, structural reason exactly like
			// ReasonRequiredByCELMap above.
			structural[leaf.Path] = listRequiredByCELReason(blocker)
		} else if conditional {
			// Only the `value: []` route is closed, and only while the
			// rule's own condition holds on THIS manifest.
			markerOnly[leaf.Path] = listConditionallyEmptyReason(blocker)
		} else {
			// Only the `value: []` route is closed; the caller reconciles
			// this against actual coverage before treating it as final.
			markerOnly[leaf.Path] = listNeverEmptyReason(blocker)
		}
	}
	return structural, markerOnly, nil
}

// classifyAbsentFromManifest derives ReasonAbsentFromManifest for every
// leaf that is: not already carrying an ineligibility reason from the
// caller (structural — every reason ContainerClearCoverage has finalized
// by this point: classifyIneligibility's structural map PLUS whichever of
// its markerOnly reasons survived that function's own coverage
// reconciliation); and not a member of a
// (Shape, Depth) cell that cellCovered already reports Covered (see
// ContainerClearCoverage's own construction of cellCovered) — stripping
// such a member out of eligibility would WITHDRAW the cell-membership
// credit the container-clear cell report already grants it (one credited
// representative covers every sibling sharing its cell), which coverage
// must never do. Only once neither exemption applies does this check the
// leaf's own presence: absent from m.ForProvider AND absent from every
// TESTED update-test entry's own data (see testEntryDataTree).
func classifyAbsentFromManifest(leaves []ContainerLeaf, m *manifest.Manifest, structural map[string]IneligibilityReason, cellCovered map[CellKey]bool) map[string]IneligibilityReason {
	testData := testEntryDataTree(m)
	out := make(map[string]IneligibilityReason)
	for _, leaf := range leaves {
		if _, already := structural[leaf.Path]; already {
			continue
		}
		key := CellKey{Classification: ClassNA, Shape: leaf.Shape, Direction: DirectionClear, Depth: depthOf(leaf.Path)}
		if cellCovered[key] {
			continue
		}
		if !presentAtPath(m.ForProvider, leaf.Path) && !presentAtPath(testData, leaf.Path) {
			out[leaf.Path] = ReasonAbsentFromManifest
		}
	}
	return out
}

// presentAtPath reports whether root carries a value at dotted (a
// container leaf's own dot-joined path), navigating root's nested
// map[string]interface{} structure exactly the way DeclaredContainerLeaves
// itself walks a schema tree. A key counts as present the moment it is its
// own explicit member — an empty list, an empty map, or an explicit null
// are all PRESENT: RFC 7386 (and manifest.UpdateTest.ValueExplicit's own
// convention) treats "the key was written" as a materially different fact
// from "the key was never mentioned at all", regardless of what value it
// was written to. Absent the moment any segment along the way is missing,
// or a non-terminal segment resolves to something other than a nested
// object — a container leaf can never be reached through a scalar or a
// list ancestor.
func presentAtPath(root map[string]interface{}, dotted string) bool {
	cur := root
	segments := strings.Split(dotted, ".")
	for i, seg := range segments {
		if cur == nil {
			return false
		}
		v, ok := cur[seg]
		if !ok {
			return false
		}
		if i == len(segments)-1 {
			return true
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// testEntryDataTree assembles a synthetic presence tree from every TESTED
// (non-skip) entry in m.Tests — never a skip: entry, which authors no test
// at all and so introduces no data of its own — merging each entry's own
// Field/Value pair, plus every sibling literal named in its withValues:
// map, at the dotted path each names. presentAtPath walks the result
// exactly as it walks a manifest's own spec.forProvider, so a leaf whose
// value is introduced for the very first time by the update-test
// annotation itself — never by the base spec — is still found present
// here: the worked case this exists to keep eligible is
// FixedAddress.options, absent from fixed-address-allocate.yaml's own
// spec.forProvider but carrying a genuine, non-empty value: list on its
// own `field: options` entry.
func testEntryDataTree(m *manifest.Manifest) map[string]interface{} {
	tree := map[string]interface{}{}
	if m == nil {
		return tree
	}
	for _, t := range m.Tests {
		if t.Skip.Present() {
			continue
		}
		if t.Field != "" {
			setAtPath(tree, t.Field, t.Value)
		}
		for sibling, v := range t.WithValues {
			setAtPath(tree, sibling, v)
		}
	}
	return tree
}

// setAtPath writes value into tree at dotted, creating intermediate
// map[string]interface{} nodes as needed. Mirrors presentAtPath's own
// segment-by-segment navigation exactly, so a path this function writes is
// always the same path presentAtPath finds.
func setAtPath(tree map[string]interface{}, dotted string, value interface{}) {
	segments := strings.Split(dotted, ".")
	cur := tree
	for i, seg := range segments {
		if i == len(segments)-1 {
			cur[seg] = value
			return
		}
		next, ok := cur[seg].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[seg] = next
		}
		cur = next
	}
}

// listEmptyRouteBlocked reports whether leafPath's OWN `value: []` clear
// route is closed on the manifest under test — by its schema declaring
// `minItems > 0`, or by an x-kubernetes-validations rule requiring the list
// to be non-empty via size() (see sizeNonEmptyGuard) — and, when closed,
// names the actual blocker for the reason string. conditional is true when
// the blocking rule is a guarded one (leading `!has(self.X)` /
// `self.X != 'V'` disjuncts) that binds only because forProvider satisfies
// its condition; the blocker then names that condition. Checked
// independently of requiredByManagementPolicies: this is a SEPARATE
// question (can the list be emptied at all) from whether it is required to
// be present in the first place.
func listEmptyRouteBlocked(fpSchema, schema map[string]interface{}, leafPath string, forProvider map[string]interface{}) (blocker string, conditional, blocked bool) {
	if leafSchema, ok := schemaAtPath(fpSchema, leafPath); ok {
		if n, hasMinItems := minItemsGuard(leafSchema); hasMinItems && n > 0 {
			return fmt.Sprintf("minItems: %d", n), false, true
		}
	}
	if hit, ok := sizeNonEmptyGuard(schema, fpSchema, leafPath, forProvider); ok {
		if hit.condition != "" {
			return fmt.Sprintf("a CEL rule requiring %s while %s", hit.clause, hit.condition), true, true
		}
		return fmt.Sprintf("a CEL rule requiring %s", hit.clause), false, true
	}
	return "", false, false
}

// minItemsGuard reads m's own "minItems" — tolerating both the float64
// gojson/yaml.v3 ordinarily decodes JSON numbers to, and a plain int, so
// this works identically whether the schema arrived via encoding/json or
// gopkg.in/yaml.v3.
func minItemsGuard(m map[string]interface{}) (int, bool) {
	switch v := m["minItems"].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

// sizeGuardPattern matches a CEL rule's OWN non-empty clause for wantPath —
// the method form "<path>.size() > 0" / "<path>.size() >= 1" or the function
// form "size(<path>) > 0" / "size(<path>) >= 1", the ordinary spellings of
// "this list must not be empty" — anchored to wantPath so a size() guard on
// a DIFFERENT field is never mistaken for one guarding this leaf.
func sizeGuardPattern(wantPath string) *regexp.Regexp {
	q := regexp.QuoteMeta(wantPath)
	const cmp = `\s*(?:>\s*0|>=\s*1)\b`
	return regexp.MustCompile(q + `\.size\(\)` + cmp + `|\bsize\(\s*` + q + `\s*\)` + cmp)
}

// sizeAnchor describes one schema node whose x-kubernetes-validations may
// carry a non-empty rule for a leaf, and how `self` resolves there: celSelf
// is the CEL path prefix of forProvider-relative paths (so a leaf's rule
// text is celSelf + the leaf's path below the anchor), and fpPrefix is the
// dotted forProvider-relative path of the anchor node itself ("" at
// forProvider and above), prepended to a guard path to look it up in the
// manifest's forProvider. A guard path not under celSelf cannot be
// evaluated against forProvider at all.
type sizeAnchor struct {
	validations []interface{}
	celSelf     string
	fpPrefix    string
}

// sizeGuardHit is a matched non-empty clause, verbatim, plus the manifest
// condition under which the rule binds ("" when the rule is unconditional).
type sizeGuardHit struct {
	clause    string
	condition string
}

// sizeNonEmptyGuard scans every node on leafPath's route for an
// x-kubernetes-validations rule requiring leafPath's own list to be
// non-empty via size() — a SEPARATE rule from the one
// requiredByManagementPolicies matches, which only ever checks has(). The
// anchors are the CRD root (`self.spec.forProvider.<leaf>`), the spec node
// (`self.forProvider.<leaf>`), the forProvider node (`self.<leaf>`) and
// every object node between forProvider and the leaf (`self.<path below
// that node>`), because a structural-schema rule may be declared wherever
// `self` resolves the path it inspects.
func sizeNonEmptyGuard(schema, fpSchema map[string]interface{}, leafPath string, forProvider map[string]interface{}) (sizeGuardHit, bool) {
	var anchors []sizeAnchor
	if v, ok := schema["x-kubernetes-validations"].([]interface{}); ok {
		anchors = append(anchors, sizeAnchor{validations: v, celSelf: "self.spec.forProvider."})
	}
	if specSchema, err := fieldSchema(schema, "spec"); err == nil {
		if v, ok := specSchema["x-kubernetes-validations"].([]interface{}); ok {
			anchors = append(anchors, sizeAnchor{validations: v, celSelf: "self.forProvider."})
		}
	}
	if v, ok := fpSchema["x-kubernetes-validations"].([]interface{}); ok {
		anchors = append(anchors, sizeAnchor{validations: v, celSelf: "self."})
	}
	segments := strings.Split(leafPath, ".")
	for i := 1; i < len(segments); i++ {
		rel := strings.Join(segments[:i], ".")
		node, ok := schemaAtPath(fpSchema, rel)
		if !ok {
			break
		}
		if v, ok := node["x-kubernetes-validations"].([]interface{}); ok {
			anchors = append(anchors, sizeAnchor{validations: v, celSelf: "self.", fpPrefix: rel + "."})
		}
	}
	for _, a := range anchors {
		want := a.celSelf + strings.TrimPrefix(leafPath, a.fpPrefix)
		if hit, ok := sizeGuardRule(a, want, forProvider); ok {
			return hit, true
		}
	}
	return sizeGuardHit{}, false
}

// sizeGuardRule scans a.validations for a rule carrying a non-empty clause
// for wantPath (see sizeGuardPattern), returning that clause verbatim for
// the reason string.
//
// A rule whose clause sits in its LAST top-level OR-disjunct is evaluated
// against forProvider: every leading disjunct of the recognised guard
// shapes — `!has(self.X)` and `self.X != 'V'` — is reduced to true or
// false on this manifest, and the rule binds only when ALL of them are
// false (the clause is then all that stands between the object and
// admission). A leading disjunct that evaluates true means the rule does
// not bind here and the leaf is left alone; the condition under which it
// does bind is returned in the hit. A rule without leading disjuncts, with
// a leading disjunct of any other shape, or with the clause anywhere but
// last is treated as unconditional.
func sizeGuardRule(a sizeAnchor, wantPath string, forProvider map[string]interface{}) (sizeGuardHit, bool) {
	pattern := sizeGuardPattern(wantPath)
	for _, v := range a.validations {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		rule, _ := vm["rule"].(string)
		clause := pattern.FindString(rule)
		if clause == "" {
			continue
		}
		disjuncts := splitTopLevelOr(rule)
		last := len(disjuncts) - 1
		if last < 1 || !pattern.MatchString(disjuncts[last]) {
			return sizeGuardHit{clause: clause}, true
		}
		var conds []string
		binds, evaluable := true, true
		for _, d := range disjuncts[:last] {
			holds, cond, known := evalGuardDisjunct(d, a, forProvider)
			if !known {
				evaluable = false
				break
			}
			if holds {
				binds = false
				break
			}
			conds = append(conds, cond)
		}
		switch {
		case !evaluable:
			return sizeGuardHit{clause: clause}, true
		case binds:
			return sizeGuardHit{clause: clause, condition: joinConditions(conds)}, true
		}
	}
	return sizeGuardHit{}, false
}

var (
	reNotHasDisjunct = regexp.MustCompile(`^!\s*has\(\s*(self(?:\.[A-Za-z0-9_]+)+)\s*\)$`)
	reNotEqDisjunct  = regexp.MustCompile(`^(self(?:\.[A-Za-z0-9_]+)+)\s*!=\s*(?:'([^']*)'|"([^"]*)")$`)
)

// evalGuardDisjunct reduces one leading OR-disjunct of a guarded rule to its
// value on forProvider. holds is the disjunct's value, cond the plain-words
// condition that makes it FALSE (the rule binding), and known is false when
// the disjunct is not one of the two recognised shapes, names a path outside
// forProvider, or compares a non-string value.
//
//   - `!has(self.X)` is false exactly when X is present;
//   - `self.X != 'V'` is false exactly when X is present and equals V; with X
//     absent it is treated as true (the object cannot satisfy the condition).
func evalGuardDisjunct(d string, a sizeAnchor, forProvider map[string]interface{}) (holds bool, cond string, known bool) {
	d = stripOuterParens(strings.TrimSpace(d))
	if m := reNotHasDisjunct.FindStringSubmatch(d); m != nil {
		fpPath, ok := guardFPPath(m[1], a)
		if !ok {
			return false, "", false
		}
		_, present := valueAtPath(forProvider, fpPath)
		return !present, "forProvider." + fpPath + " is set", true
	}
	if m := reNotEqDisjunct.FindStringSubmatch(d); m != nil {
		fpPath, ok := guardFPPath(m[1], a)
		if !ok {
			return false, "", false
		}
		want := m[2]
		if m[3] != "" {
			want = m[3]
		}
		cond = "forProvider." + fpPath + " == '" + want + "'"
		v, present := valueAtPath(forProvider, fpPath)
		if !present {
			return true, cond, true
		}
		s, isString := v.(string)
		if !isString {
			return false, "", false
		}
		return s != want, cond, true
	}
	return false, "", false
}

// guardFPPath maps a CEL path rooted at `self` in a rule declared on anchor a
// to its dotted path inside forProvider, reporting false when the path does
// not resolve under forProvider.
func guardFPPath(celPath string, a sizeAnchor) (string, bool) {
	rest := celPath + "."
	if !strings.HasPrefix(rest, a.celSelf) {
		return "", false
	}
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, a.celSelf), ".")
	if rest == "" {
		return "", false
	}
	return a.fpPrefix + rest, true
}

// joinConditions renders a rule's binding conditions in plain words,
// dropping a "<path> is set" that an equality on the same path already
// implies.
func joinConditions(conds []string) string {
	var out []string
	for _, c := range conds {
		if path, ok := strings.CutSuffix(c, " is set"); ok {
			implied := false
			for _, other := range conds {
				if strings.HasPrefix(other, path+" == ") {
					implied = true
					break
				}
			}
			if implied {
				continue
			}
		}
		out = append(out, c)
	}
	return strings.Join(out, " and ")
}

// splitTopLevelOr splits rule on `||` operators outside parentheses and
// quoted string literals, returning the trimmed disjuncts in order.
func splitTopLevelOr(rule string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == '|' && depth == 0 && i+1 < len(rule) && rule[i+1] == '|':
			out = append(out, strings.TrimSpace(rule[start:i]))
			i++
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(rule[start:]))
}

// stripOuterParens removes parentheses that enclose all of s.
func stripOuterParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		enclosing := true
		for i := 0; i < len(s)-1; i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					enclosing = false
				}
			}
			if !enclosing {
				break
			}
		}
		if !enclosing {
			return s
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// valueAtPath returns the value root carries at dotted, navigating nested
// maps exactly as presentAtPath does.
func valueAtPath(root map[string]interface{}, dotted string) (interface{}, bool) {
	var cur interface{} = root
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// referenceResolutionShape reports whether leaf is, by its SCHEMA SHAPE
// alone, crossplane-runtime's own reference-resolution plumbing — never by
// matching leaf.Path's name against a "Ref"/"Refs"/"Selector" suffix. A
// name-suffix match would silently exclude a genuine backend field that
// happens to end in one of those strings; the shape check below can only
// ever match the exact structure controller-tools generates for
// xpv1.Selector and xpv1.Reference/NamespacedReference.
//
// Two shapes are recognised, mirroring how DeclaredContainerLeaves itself
// finds a container leaf:
//
//   - a Map-shaped leaf whose immediate parent schema node is a
//     Selector — the "matchLabels" member DeclaredContainerLeaves reports
//     as a leaf in its own right, because the enclosing Selector object
//     descends into its properties like any other object node;
//   - a List-shaped leaf whose own "items" schema is a Reference /
//     NamespacedReference.
//
// leaf.Path is cross-checked against apPaths (status.atProvider's own leaf
// paths): a field genuinely mirrored back by the provider is never
// excluded here even if its schema happens to match one of the shapes
// above, because that mirroring is itself proof the field reaches the
// backend. Measured against provider-vultr and provider-vsphere: zero of
// the fields this function matches are ever present in status.atProvider,
// so the cross-check has so far never had occasion to veto a match — but
// it stays in force rather than being trusted away.
func referenceResolutionShape(fpSchema map[string]interface{}, leaf ContainerLeaf, apPaths map[string]bool) bool {
	if apPaths[leaf.Path] {
		return false
	}

	switch leaf.Shape {
	case ShapeMap:
		parent := parentPath(leaf.Path)
		if parent == "" {
			return false
		}
		parentSchema, ok := schemaAtPath(fpSchema, parent)
		if !ok {
			return false
		}
		return isSelectorShape(parentSchema)
	case ShapeList:
		leafSchema, ok := schemaAtPath(fpSchema, leaf.Path)
		if !ok {
			return false
		}
		items, _ := leafSchema["items"].(map[string]interface{})
		if items == nil {
			return false
		}
		return isReferenceItemShape(items)
	default:
		return false
	}
}

// parentPath returns path's immediate dotted-path ancestor, or "" when
// path has no ancestor (a top-level field).
func parentPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// schemaAtPath navigates root's "properties" tree along dotted, reusing
// fieldSchema's own navigation so this file agrees exactly with how
// DeclaredContainerLeaves and DiffReport resolve a dotted leaf path back
// to its schema node.
func schemaAtPath(root map[string]interface{}, dotted string) (map[string]interface{}, bool) {
	if dotted == "" {
		return root, true
	}
	node, err := fieldSchema(root, strings.Split(dotted, ".")...)
	if err != nil {
		return nil, false
	}
	return node, true
}

// isSelectorShape reports whether m is shaped exactly like
// crossplane-runtime's generated xpv1.Selector — both the cluster-scoped
// shape (matchControllerRef, matchLabels, policy) and the namespaced shape,
// which carries one additional member, "namespace": that is the only
// difference between the two generated forms (mirroring
// isReferenceItemShape's own name/namespace pair below), and both are
// reference-resolution plumbing either way. Properties are drawn ONLY from
// {matchControllerRef, matchLabels, namespace, policy}, with at least one
// of matchControllerRef/matchLabels present, matchLabels (when present)
// itself a free-form string map, namespace (when present) itself
// string-typed, and policy (when present) itself a resolution/resolve-shaped
// object. Every condition is checked against the node's own structure —
// nothing here reads the enclosing field's JSON name.
func isSelectorShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"matchControllerRef": true, "matchLabels": true, "namespace": true, "policy": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}

	mlRaw, hasML := props["matchLabels"]
	_, hasMCR := props["matchControllerRef"]
	if !hasML && !hasMCR {
		return false
	}
	if hasML {
		ml, ok := mlRaw.(map[string]interface{})
		if !ok {
			return false
		}
		typ, _ := ml["type"].(string)
		_, hasAdd := ml["additionalProperties"]
		if typ != "object" || !hasAdd {
			return false
		}
	}
	if nsRaw, hasNS := props["namespace"]; hasNS {
		ns, ok := nsRaw.(map[string]interface{})
		if !ok {
			return false
		}
		typ, _ := ns["type"].(string)
		if typ != "string" {
			return false
		}
	}
	if policyRaw, hasPolicy := props["policy"]; hasPolicy {
		policySchema, ok := policyRaw.(map[string]interface{})
		if !ok || !isPolicyShape(policySchema) {
			return false
		}
	}
	return true
}

// isReferenceItemShape reports whether m — the "items" schema of a
// declared List-shaped leaf — is shaped exactly like crossplane-runtime's
// generated xpv1.Reference or xpv1.NamespacedReference: an object whose
// declared properties are drawn ONLY from {name, namespace, policy}, with
// a required string-typed "name" member (namespace, when present, is the
// only difference between the cluster-scoped and namespaced generated
// shapes; both are reference-resolution plumbing either way).
func isReferenceItemShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"name": true, "namespace": true, "policy": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}
	nameSchema, ok := props["name"].(map[string]interface{})
	if !ok {
		return false
	}
	typ, _ := nameSchema["type"].(string)
	if typ != "string" {
		return false
	}
	if policyRaw, hasPolicy := props["policy"]; hasPolicy {
		policySchema, ok := policyRaw.(map[string]interface{})
		if !ok || !isPolicyShape(policySchema) {
			return false
		}
	}
	return true
}

// isPolicyShape reports whether m is shaped like crossplane-runtime's
// generated reference Policy: an object whose declared properties are
// drawn only from {resolution, resolve}.
func isPolicyShape(m map[string]interface{}) bool {
	props, _ := m["properties"].(map[string]interface{})
	if len(props) == 0 {
		return false
	}
	allowed := map[string]bool{"resolution": true, "resolve": true}
	for name := range props {
		if !allowed[name] {
			return false
		}
	}
	return true
}

// managementPoliciesDefault resolves spec.managementPolicies' own schema
// DEFAULT — measured as ['*'] on every provider checked, but read from the
// schema rather than assumed, so a provider that ever ships a different
// default is handled correctly rather than silently mismeasured. The bool
// result is false when the field or its default is absent (a CRD schema
// that, for whatever reason, declares no default at all), in which case
// REASON 2 can never be derived — no default means there is no "resting,
// unedited state" to evaluate the CEL rule against.
func managementPoliciesDefault(schema map[string]interface{}) ([]string, bool) {
	mpSchema, err := fieldSchema(schema, "spec", "managementPolicies")
	if err != nil {
		return nil, false
	}
	raw, ok := mpSchema["default"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// reQuotedLiteral matches every single-quoted string literal in a CEL rule
// — the policy names ('*', 'Create', 'Update', ...) a managementPolicies
// membership guard tests against. Extracted straight from the rule's own
// text, never assumed to be any particular fixed set.
var reQuotedLiteral = regexp.MustCompile(`'([^']+)'`)

// requiredByManagementPolicies reports whether schema carries an
// x-kubernetes-validations rule — at the schema's own root, or on its
// "spec" node — requiring leafPath to be present whenever the object's
// managementPolicies intersects mpDefault. Both anchors are checked
// because a structural-schema CEL rule may be declared wherever `self`
// resolves the path it inspects: at the CRD root (`self.spec.forProvider.
// <leaf>`, `self` is the whole object — the shape measured on every
// provider checked) or on the spec node itself (`self.forProvider.<leaf>`,
// `self` is spec).
func requiredByManagementPolicies(schema map[string]interface{}, leafPath string, mpDefault []string) bool {
	if rootValidations, ok := schema["x-kubernetes-validations"].([]interface{}); ok {
		if leafRequiredByRule(rootValidations, "self.spec.forProvider."+leafPath, mpDefault) {
			return true
		}
	}
	specSchema, err := fieldSchema(schema, "spec")
	if err == nil {
		if specValidations, ok := specSchema["x-kubernetes-validations"].([]interface{}); ok {
			if leafRequiredByRule(specValidations, "self.forProvider."+leafPath, mpDefault) {
				return true
			}
		}
	}
	return false
}

// leafRequiredByRule scans validations (one schema node's own
// x-kubernetes-validations array) for a rule whose final OR-disjunct is
// exactly "has(wantPath)" and whose managementPolicies membership guard —
// parsed straight from the rule's own quoted literals, never assumed —
// shares at least one member with mpDefault. Sharing a member means the
// guard's negation (the disjunct immediately gating the has() clause)
// evaluates false at the object's resting, unedited managementPolicies
// value, so the OR reduces to demanding has(wantPath): the field is
// required right now, not merely under some non-default policy a caller
// might opt into later.
func leafRequiredByRule(validations []interface{}, wantPath string, mpDefault []string) bool {
	wantSuffix := "has(" + wantPath + ")"
	for _, v := range validations {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		rule, _ := vm["rule"].(string)
		if !strings.Contains(rule, "managementPolicies") {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(rule), wantSuffix) {
			continue
		}
		for _, m := range reQuotedLiteral.FindAllStringSubmatch(rule, -1) {
			if containsString(mpDefault, m[1]) {
				return true
			}
		}
	}
	return false
}

// containsString reports whether want is a member of list.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
