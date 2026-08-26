package roundtrip

import "sort"

// The cell-denominator replaces the must-test set's own field-by-field
// waiver bookkeeping with a coarser unit: the CELL a field occupies,
// identified by (roundtrip classification, container shape, direction).
// One tested representative per cell credits every sibling that shares the
// same cell mechanically — no annotation, no prose waiver — for the one
// classification where that collapse is sound (ClassEqual; see
// CreditCells' own doc comment for why the other must-test classifications
// are never credited this way).
//
// Two axes come from a live DiffReport row (Classification, Shape);
// the third — Direction — is NOT observable from a row at all. A row is
// always the result of a create/update having already happened, so every
// row-derived cell is a DirectionSet cell by construction. The clear
// (removal) direction is a distinct coverage obligation, checked
// separately against a manifest's own test entries rather than against a
// live row — see ContainerClearCoverage.

// Shape is a cell's container-shape axis, derived from the JSON type of
// whichever of a Row's SpecValue/MirrorValue is populated — no CRD
// re-walk needed, matching how the fleet cell census itself derived it.
type Shape string

// The three shapes a live JSON value can take. There is no separate
// "object-with-declared-properties" shape here: DiffReport never produces
// a Row for such a node in the first place (it descends into it instead —
// see DiffReport's own doc comment on what counts as a leaf), so every Row
// this package ever sees is already scalar, list, or a free-form map.
const (
	ShapeScalar Shape = "scalar"
	ShapeList   Shape = "list"
	ShapeMap    Shape = "map"
)

// Direction is a cell's third axis: whether the cell represents the
// ordinary set/observe direction (every row-derived cell) or the
// clear/removal direction (container-typed fields only — see
// ContainerClearCoverage).
type Direction string

const (
	// DirectionSet is the ordinary set/observe direction — every
	// row-derived cell (see GroupCells).
	DirectionSet Direction = "set"
	// DirectionClear is the clear/removal direction, tracked only for
	// container-typed fields — see ContainerClearCoverage.
	DirectionClear Direction = "clear"
)

// CellKey identifies one cell. Classification is empty and meaningless for
// a DirectionClear key — a clear-direction obligation is about whether
// removal was ever exercised at all, not about what a set-direction
// observation classified the field as — so ClassNA is used there instead
// of leaving the field's normal classification vocabulary to imply
// something it does not mean.
type CellKey struct {
	Classification string
	Shape          Shape
	Direction      Direction
}

// ClassNA is CellKey.Classification's value for a DirectionClear cell,
// where a set-direction classification does not apply.
const ClassNA = "n/a"

// ShapeOf derives a Row's container shape from whichever of SpecValue or
// MirrorValue is actually populated, preferring the spec side since that
// is the value the manifest itself authored. A row with neither side
// populated cannot occur (DiffReport's own classify() never returns ok for
// that case), so falling through to ShapeScalar here is defensive, not a
// path any real row reaches.
func ShapeOf(r Row) Shape {
	v := r.SpecValue
	found := r.SpecFound
	if !found {
		v = r.MirrorValue
		found = r.MirrorFound
	}
	if !found {
		return ShapeScalar
	}
	switch v.(type) {
	case []interface{}:
		return ShapeList
	case map[string]interface{}:
		return ShapeMap
	default:
		return ShapeScalar
	}
}

// GroupCells partitions rows into the SET-direction cells they occupy.
// present-in-mirror-absent-from-spec is excluded by construction — it is
// not a spec field at all (see DiffReport's own doc comment), so grouping
// it would count read-only atProvider-only surface as testable cell
// membership.
func GroupCells(rows []Row) map[CellKey][]Row {
	out := make(map[CellKey][]Row)
	for _, r := range rows {
		if r.Classification == ClassPresentInMirrorAbsentFromSpec {
			continue
		}
		key := CellKey{Classification: r.Classification, Shape: ShapeOf(r), Direction: DirectionSet}
		out[key] = append(out[key], r)
	}
	return out
}

// sortedPaths extracts and sorts every row's Path — the deterministic
// member ordering every rotation and report function in this package
// builds on.
func sortedPaths(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Path
	}
	sort.Strings(out)
	return out
}
