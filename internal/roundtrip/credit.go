package roundtrip

// CellCredit is the crediting outcome for one `equal` cell: some of its
// members are this run's chosen representatives (always tested);
// everything else is credited mechanically by cell membership — no
// annotation, no prose waiver required.
type CellCredit struct {
	Key CellKey
	// Members lists every field path occupying this cell, sorted.
	Members []string
	// Representatives are this run's chosen subset — see RotationState.Select.
	Representatives []string
	// Credited are members NOT chosen this run; they are credited by
	// membership rather than tested.
	Credited []string
	// Sticky are members permanently promoted into every run's
	// representative set (a subset of Representatives).
	Sticky []string
}

// CreditCells applies representative crediting to cells and advances
// state's rotation cursors. Only an `equal` cell collapses to a
// representative — value-changed, defaulted-by-server and
// present-in-spec-absent-from-mirror members are returned untouched in
// nonEqual, because each of those must be tested and reported
// INDIVIDUALLY: crediting them by membership would hide a genuine backend
// deviation behind whichever member happened to be observed, which is
// exactly the failure mode this tool exists to catch.
// present-in-mirror-absent-from-spec never appears in cells at all (see
// GroupCells), and no cell here ever has Direction != DirectionSet, since
// GroupCells never produces one.
func CreditCells(cells map[CellKey][]Row, state *RotationState) (credits []CellCredit, nonEqual map[CellKey][]Row) {
	nonEqual = make(map[CellKey][]Row)
	for key, rows := range cells {
		if key.Classification != ClassEqual {
			nonEqual[key] = rows
			continue
		}

		members := sortedPaths(rows)
		reps, sticky := state.Select(key, members)

		repSet := make(map[string]bool, len(reps))
		for _, m := range reps {
			repSet[m] = true
		}
		credited := make([]string, 0, len(members))
		for _, m := range members {
			if !repSet[m] {
				credited = append(credited, m)
			}
		}

		credits = append(credits, CellCredit{
			Key:             key,
			Members:         members,
			Representatives: reps,
			Credited:        credited,
			Sticky:          sticky,
		})
	}
	return credits, nonEqual
}
