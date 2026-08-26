package roundtrip

import (
	"reflect"
	"testing"
)

// TestShapeOfDerivesFromPopulatedSide confirms shape derivation reads
// whichever of SpecValue/MirrorValue is actually populated, preferring the
// spec side, and never panics on an absent side.
func TestShapeOfDerivesFromPopulatedSide(t *testing.T) {
	cases := map[string]struct {
		row  Row
		want Shape
	}{
		"scalar spec value": {
			row:  Row{SpecFound: true, SpecValue: "x"},
			want: ShapeScalar,
		},
		"list spec value": {
			row:  Row{SpecFound: true, SpecValue: []interface{}{"a", "b"}},
			want: ShapeList,
		},
		"map spec value": {
			row:  Row{SpecFound: true, SpecValue: map[string]interface{}{"a": "1"}},
			want: ShapeMap,
		},
		"falls back to mirror when spec absent": {
			row:  Row{MirrorFound: true, MirrorValue: []interface{}{1, 2, 3}},
			want: ShapeList,
		},
		"prefers spec over mirror when both present": {
			row:  Row{SpecFound: true, SpecValue: "scalar", MirrorFound: true, MirrorValue: map[string]interface{}{"a": 1}},
			want: ShapeScalar,
		},
		"neither side found defaults to scalar defensively": {
			row:  Row{},
			want: ShapeScalar,
		},
		"numeric scalar": {
			row:  Row{SpecFound: true, SpecValue: float64(42)},
			want: ShapeScalar,
		},
		"bool scalar": {
			row:  Row{SpecFound: true, SpecValue: true},
			want: ShapeScalar,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ShapeOf(tc.row); got != tc.want {
				t.Errorf("ShapeOf(%+v) = %q, want %q", tc.row, got, tc.want)
			}
		})
	}
}

// TestGroupCellsExcludesPresentInMirrorAbsentFromSpec confirms the fleet
// census's own corrected arithmetic: that classification is not a spec
// field at all and must never occupy a cell.
func TestGroupCellsExcludesPresentInMirrorAbsentFromSpec(t *testing.T) {
	rows := []Row{
		{Path: "id", Classification: ClassPresentInMirrorAbsentFromSpec, MirrorFound: true, MirrorValue: "srv-1"},
		{Path: "name", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
	}
	cells := GroupCells(rows)

	for key := range cells {
		if key.Classification == ClassPresentInMirrorAbsentFromSpec {
			t.Errorf("GroupCells produced a cell for present-in-mirror-absent-from-spec: %+v", key)
		}
	}
	if len(cells) != 1 {
		t.Fatalf("GroupCells(%+v) produced %d cells, want 1 (only the equal/name row)", rows, len(cells))
	}
}

// TestGroupCellsPartitionsByClassificationAndShape confirms rows land in
// distinct cells whenever either axis differs, and share a cell when both
// match.
func TestGroupCellsPartitionsByClassificationAndShape(t *testing.T) {
	rows := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
		{Path: "b", Classification: ClassEqual, SpecFound: true, SpecValue: "y", MirrorFound: true, MirrorValue: "y"},
		{Path: "tags", Classification: ClassEqual, SpecFound: true, SpecValue: map[string]interface{}{"k": "v"}, MirrorFound: true, MirrorValue: map[string]interface{}{"k": "v"}},
		{Path: "region", Classification: ClassValueChanged, SpecFound: true, SpecValue: "us", MirrorFound: true, MirrorValue: "eu"},
	}
	cells := GroupCells(rows)

	scalarEqual := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	mapEqual := CellKey{Classification: ClassEqual, Shape: ShapeMap, Direction: DirectionSet}
	valueChanged := CellKey{Classification: ClassValueChanged, Shape: ShapeScalar, Direction: DirectionSet}

	if got := sortedPaths(cells[scalarEqual]); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("scalar equal cell members = %v, want [a b]", got)
	}
	if got := sortedPaths(cells[mapEqual]); !reflect.DeepEqual(got, []string{"tags"}) {
		t.Errorf("map equal cell members = %v, want [tags]", got)
	}
	if got := sortedPaths(cells[valueChanged]); !reflect.DeepEqual(got, []string{"region"}) {
		t.Errorf("value-changed cell members = %v, want [region]", got)
	}
	if len(cells) != 3 {
		t.Errorf("GroupCells produced %d cells, want 3", len(cells))
	}
}

// TestGroupCellsEveryDerivedCellIsSetDirection confirms a row-derived cell
// is always DirectionSet — a live row can never itself prove the clear
// direction (see ContainerClearCoverage for that separate obligation).
func TestGroupCellsEveryDerivedCellIsSetDirection(t *testing.T) {
	rows := []Row{
		{Path: "a", Classification: ClassEqual, SpecFound: true, SpecValue: "x", MirrorFound: true, MirrorValue: "x"},
		{Path: "b", Classification: ClassDefaultedByServer, MirrorFound: true, MirrorValue: "y"},
	}
	for key := range GroupCells(rows) {
		if key.Direction != DirectionSet {
			t.Errorf("cell %+v has Direction %q, want %q", key, key.Direction, DirectionSet)
		}
	}
}

// TestCellKeyStringIsStableAndDistinguishesEveryAxis confirms String()
// (RotationState's own map key) differs whenever any one axis differs —
// two cells that collide on their string key would silently share
// rotation cursors and sticky registries.
func TestCellKeyStringIsStableAndDistinguishesEveryAxis(t *testing.T) {
	base := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	variants := []CellKey{
		{Classification: ClassValueChanged, Shape: ShapeScalar, Direction: DirectionSet},
		{Classification: ClassEqual, Shape: ShapeList, Direction: DirectionSet},
		{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionClear},
	}
	seen := map[string]bool{base.String(): true}
	for _, v := range variants {
		s := v.String()
		if seen[s] {
			t.Errorf("CellKey %+v produced a String() colliding with an earlier key: %q", v, s)
		}
		seen[s] = true
	}
	// Same key, called twice, must produce the same string both times —
	// compared against a copy created independently of base, so the
	// compiler cannot fold this into a trivially-true self-comparison.
	independentCopy := CellKey{Classification: base.Classification, Shape: base.Shape, Direction: base.Direction}
	if base.String() != independentCopy.String() {
		t.Errorf("CellKey.String() is not stable: %q != %q", base.String(), independentCopy.String())
	}
}
