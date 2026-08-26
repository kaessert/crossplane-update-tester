package roundtrip

import (
	"reflect"
	"sort"
	"testing"
)

// TestCreditCellsAppliesRepresentativeCreditingToEqualCellsOnly is the
// ticket's own required arm: value-changed, defaulted-by-server and
// present-in-spec-absent-from-mirror must be returned UNTOUCHED in
// nonEqual — never collapsed to a representative — because the fleet
// census's counting rule requires each to be tested and reported
// individually.
func TestCreditCellsAppliesRepresentativeCreditingToEqualCellsOnly(t *testing.T) {
	rows := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
		{Path: "b", Classification: ClassEqual, SpecFound: true, SpecValue: "y", MirrorFound: true, MirrorValue: "y"},
		{Path: "region", Classification: ClassValueChanged, SpecFound: true, SpecValue: "us", MirrorFound: true, MirrorValue: "eu"},
		{Path: "az", Classification: ClassDefaultedByServer, MirrorFound: true, MirrorValue: "az-1"},
		{Path: "secret", Classification: ClassPresentInSpecAbsentFromMirror, SpecFound: true, SpecValue: "s"},
	}
	cells := GroupCells(rows)
	state := NewRotationState()

	credits, nonEqual := CreditCells(cells, &state, "Widget/example")

	if len(credits) != 1 {
		t.Fatalf("CreditCells produced %d credits, want 1 (only the equal/scalar cell)", len(credits))
	}
	if credits[0].Key.Classification != ClassEqual {
		t.Errorf("credited cell classification = %q, want %q", credits[0].Key.Classification, ClassEqual)
	}
	if !reflect.DeepEqual(credits[0].Members, []string{"a", "b"}) {
		t.Errorf("credited cell members = %v, want [a b]", credits[0].Members)
	}

	wantNonEqualClasses := map[string]bool{
		ClassValueChanged:                  false,
		ClassDefaultedByServer:             false,
		ClassPresentInSpecAbsentFromMirror: false,
	}
	for key := range nonEqual {
		if _, ok := wantNonEqualClasses[key.Classification]; !ok {
			t.Errorf("unexpected classification in nonEqual: %q", key.Classification)
			continue
		}
		wantNonEqualClasses[key.Classification] = true
	}
	for class, seen := range wantNonEqualClasses {
		if !seen {
			t.Errorf("nonEqual is missing classification %q — it must be returned untouched, never credited", class)
		}
	}
}

// TestCreditCellsRepresentativesPlusCreditedCoverEveryMember confirms no
// member is lost or duplicated between Representatives and Credited.
func TestCreditCellsRepresentativesPlusCreditedCoverEveryMember(t *testing.T) {
	rows := make([]Row, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, Row{
			Path: string(rune('a' + i)), Classification: ClassEqual,
			SpecFound: true, SpecValue: i, MirrorFound: true, MirrorValue: i,
		})
	}
	cells := GroupCells(rows)
	state := NewRotationState()

	credits, _ := CreditCells(cells, &state, "Widget/example")
	if len(credits) != 1 {
		t.Fatalf("got %d credits, want 1", len(credits))
	}
	c := credits[0]

	all := append(append([]string(nil), c.Representatives...), c.Credited...)
	sort.Strings(all)
	if !reflect.DeepEqual(all, c.Members) {
		t.Errorf("Representatives+Credited = %v, want exactly Members = %v", all, c.Members)
	}

	seen := map[string]int{}
	for _, m := range all {
		seen[m]++
	}
	for m, n := range seen {
		if n != 1 {
			t.Errorf("member %q appears %d times across Representatives+Credited, want exactly 1", m, n)
		}
	}
}

// TestCreditCellsStickyMembersSurfaceOnTheCredit confirms a promoted
// member is visible on CellCredit.Sticky, not only buried in the rotation
// state.
func TestCreditCellsStickyMembersSurfaceOnTheCredit(t *testing.T) {
	rows := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 1},
		{Path: "b", Classification: ClassEqual, SpecFound: true, SpecValue: 2, MirrorFound: true, MirrorValue: 2},
	}
	cells := GroupCells(rows)
	state := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	const scope = "Widget/example"
	state.PromoteFailure(scope, key, "b")

	credits, _ := CreditCells(cells, &state, scope)
	if len(credits) != 1 {
		t.Fatalf("got %d credits, want 1", len(credits))
	}
	if !reflect.DeepEqual(credits[0].Sticky, []string{"b"}) {
		t.Errorf("Sticky = %v, want [b]", credits[0].Sticky)
	}
}

// TestCreditCellsEmptyInputProducesNoCredits confirms the zero-cells edge
// case is handled without panicking.
func TestCreditCellsEmptyInputProducesNoCredits(t *testing.T) {
	state := NewRotationState()
	credits, nonEqual := CreditCells(map[CellKey][]Row{}, &state, "Widget/example")
	if len(credits) != 0 || len(nonEqual) != 0 {
		t.Errorf("CreditCells(empty) = %v, %v, want both empty", credits, nonEqual)
	}
}

// TestCreditCellsScopesRotationPerManifest confirms two manifests that
// produce the SAME CellKey each get their own, independent rotation
// cursor through CreditCells — the fix threaded from GroupCells' settled
// per-manifest scope through to RotationState.Select.
func TestCreditCellsScopesRotationPerManifest(t *testing.T) {
	rowsA := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 1},
		{Path: "b", Classification: ClassEqual, SpecFound: true, SpecValue: 2, MirrorFound: true, MirrorValue: 2},
	}
	rowsB := []Row{
		{Path: "x", Classification: ClassEqual, SpecFound: true, SpecValue: 1, MirrorFound: true, MirrorValue: 1},
		{Path: "y", Classification: ClassEqual, SpecFound: true, SpecValue: 2, MirrorFound: true, MirrorValue: 2},
		{Path: "z", Classification: ClassEqual, SpecFound: true, SpecValue: 3, MirrorFound: true, MirrorValue: 3},
	}
	state := NewRotationState()

	seenA := map[string]bool{}
	seenB := map[string]bool{}
	for run := 0; run < 10; run++ {
		creditsA, _ := CreditCells(GroupCells(rowsA), &state, "Widget/a")
		for _, c := range creditsA {
			for _, r := range c.Representatives {
				seenA[r] = true
			}
		}
		creditsB, _ := CreditCells(GroupCells(rowsB), &state, "Widget/b")
		for _, c := range creditsB {
			for _, r := range c.Representatives {
				seenB[r] = true
			}
		}
	}

	for _, p := range []string{"a", "b"} {
		if !seenA[p] {
			t.Errorf("manifest Widget/a: member %q never selected as a representative across 10 runs", p)
		}
	}
	for _, p := range []string{"x", "y", "z"} {
		if !seenB[p] {
			t.Errorf("manifest Widget/b: member %q never selected as a representative across 10 runs", p)
		}
	}
}
