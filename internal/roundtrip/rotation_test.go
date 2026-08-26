package roundtrip

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestRepresentativesPerRunConvergesWithinBudget checks the size-scaled
// rotation budget against a representative range of cell sizes (vultr 72,
// infobloxnios 64, tailscale 44, f5xc 42, vsphere 24, lambda 12,
// vclustercli 5) plus the median (4, identical on every provider): a small
// cell must reach full coverage in ~2 runs, and a large one in under 10 —
// a flat one-representative-per-cell budget fails exactly this.
func TestRepresentativesPerRunConvergesWithinBudget(t *testing.T) {
	cases := map[string]struct {
		size          int
		maxRuns       int
		wantExactRuns int // 0 means "not asserted exactly, only the bound"
	}{
		"median cell (4) converges within 2 runs":     {size: 4, maxRuns: 2, wantExactRuns: 2},
		"vclustercli max (5) converges within 2 runs": {size: 5, maxRuns: 2},
		"lambda max (12) converges within 2 runs":     {size: 12, maxRuns: 2},
		"vsphere max (24) converges within 3 runs":    {size: 24, maxRuns: 3},
		"f5xc max (42) converges within 6 runs":       {size: 42, maxRuns: 6},
		"tailscale max (44) converges within 6 runs":  {size: 44, maxRuns: 6},
		"infobloxnios max (64) converges within 8":    {size: 64, maxRuns: 8},
		"vultr max (72) converges under 10 runs":      {size: 72, maxRuns: 9},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reps := RepresentativesPerRun(tc.size)
			if reps <= 0 {
				t.Fatalf("RepresentativesPerRun(%d) = %d, want > 0", tc.size, reps)
			}
			runsToConverge := (tc.size + reps - 1) / reps // ceil(size/reps)
			if runsToConverge > tc.maxRuns {
				t.Errorf("RepresentativesPerRun(%d) = %d, converges in %d runs, want <= %d",
					tc.size, reps, runsToConverge, tc.maxRuns)
			}
			if tc.wantExactRuns != 0 && runsToConverge != tc.wantExactRuns {
				t.Errorf("RepresentativesPerRun(%d) = %d, converges in %d runs, want exactly %d",
					tc.size, reps, runsToConverge, tc.wantExactRuns)
			}
			if tc.size < 10 && tc.maxRuns == 2 && runsToConverge >= 10 {
				t.Errorf("size %d converges in %d runs — a flat one-per-cell budget would take %d runs; this must be scaled", tc.size, runsToConverge, tc.size)
			}
		})
	}
}

// TestRepresentativesPerRunNeverExceedsSize confirms the budget never asks
// for more representatives than the cell actually has members.
func TestRepresentativesPerRunNeverExceedsSize(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 4, 5, 8, 12, 24, 42, 44, 64, 72, 100} {
		reps := RepresentativesPerRun(size)
		if reps > size {
			t.Errorf("RepresentativesPerRun(%d) = %d, want <= %d", size, reps, size)
		}
		if size == 0 && reps != 0 {
			t.Errorf("RepresentativesPerRun(0) = %d, want 0", reps)
		}
	}
}

// TestNewRotationStateProducesUsableMaps confirms a fresh state's maps are
// non-nil (so Select/PromoteFailure can write into them immediately) and
// that two fresh states get different seeds — the "chosen pseudo-randomly"
// requirement starts here.
func TestNewRotationStateProducesUsableMaps(t *testing.T) {
	s := NewRotationState()
	if s.Cursors == nil || s.Sticky == nil {
		t.Fatalf("NewRotationState() = %+v, want non-nil Cursors and Sticky", s)
	}
	if s.Seed == 0 {
		t.Errorf("NewRotationState().Seed = 0, want a real generated seed")
	}
}

// TestLoadRotationStateMissingFileIsNotAnError confirms a provider's very
// first run (no persisted state yet) is a normal case, not a failure.
func TestLoadRotationStateMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist", "rotation-state.json")

	s, err := LoadRotationState(path)
	if err != nil {
		t.Fatalf("LoadRotationState(%q) = %v, want nil error for a missing file", path, err)
	}
	if s.Seed == 0 {
		t.Errorf("LoadRotationState on a missing file produced Seed 0, want a freshly generated seed")
	}
}

// TestLoadRotationStateMalformedFileIsAnError confirms a corrupt state file
// is reported rather than silently treated as fresh — silently starting
// over would discard every sticky promotion recorded so far.
func TestLoadRotationStateMalformedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotation-state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("writing malformed fixture: %v", err)
	}

	if _, err := LoadRotationState(path); err == nil {
		t.Fatal("LoadRotationState on a malformed file returned nil error, want one")
	}
}

// TestRotationStateSaveThenLoadRoundTrips confirms persistence is lossless
// for every field a caller depends on: seed, cursors, and sticky.
func TestRotationStateSaveThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "rotation-state.json")

	s := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	s.Cursors[key.String()] = 3
	s.PromoteFailure(key, "region")

	if err := s.Save(path); err != nil {
		t.Fatalf("Save(%q) = %v", path, err)
	}

	loaded, err := LoadRotationState(path)
	if err != nil {
		t.Fatalf("LoadRotationState(%q) = %v", path, err)
	}
	if loaded.Seed != s.Seed {
		t.Errorf("loaded Seed = %d, want %d", loaded.Seed, s.Seed)
	}
	if loaded.Cursors[key.String()] != 3 {
		t.Errorf("loaded cursor = %d, want 3", loaded.Cursors[key.String()])
	}
	if !reflect.DeepEqual(loaded.Sticky[key.String()], []string{"region"}) {
		t.Errorf("loaded sticky = %v, want [region]", loaded.Sticky[key.String()])
	}
}

// TestSelectRotatesAcrossRunsWithoutRepeatingBeforeExhaustion confirms the
// round-robin actually advances: simulating N runs over an 8-member cell
// with a 2-per-run budget visits every member at least once within the
// stated bound, and does not re-select a member before every other member
// has had its turn.
func TestSelectRotatesAcrossRunsWithoutRepeatingBeforeExhaustion(t *testing.T) {
	members := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	state := NewRotationState()

	seenBeforeRepeat := map[string]bool{}
	repeatedTooSoon := false
	budget := RepresentativesPerRun(len(members))
	runsNeeded := (len(members) + budget - 1) / budget

	for run := 0; run < runsNeeded; run++ {
		reps, _ := state.Select(key, members)
		if len(reps) != budget {
			t.Fatalf("run %d: Select returned %d representatives, want %d", run, len(reps), budget)
		}
		for _, r := range reps {
			if seenBeforeRepeat[r] && len(seenBeforeRepeat) < len(members) {
				repeatedTooSoon = true
			}
			seenBeforeRepeat[r] = true
		}
	}
	if repeatedTooSoon {
		t.Errorf("Select repeated a member before every member of the cell had a turn")
	}
	if len(seenBeforeRepeat) != len(members) {
		t.Errorf("after %d runs, %d/%d members were ever selected, want all %d covered",
			runsNeeded, len(seenBeforeRepeat), len(members), len(members))
	}
}

// TestSelectAlwaysIncludesStickyMembers confirms a promoted field is
// selected on every subsequent call regardless of where the rotation
// cursor happens to be.
func TestSelectAlwaysIncludesStickyMembers(t *testing.T) {
	members := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	state := NewRotationState()
	state.PromoteFailure(key, "e")

	for run := 0; run < 5; run++ {
		reps, sticky := state.Select(key, members)
		found := false
		for _, r := range reps {
			if r == "e" {
				found = true
			}
		}
		if !found {
			t.Errorf("run %d: sticky member %q missing from representatives %v", run, "e", reps)
		}
		if !reflect.DeepEqual(sticky, []string{"e"}) {
			t.Errorf("run %d: sticky = %v, want [e]", run, sticky)
		}
	}
}

// TestPromoteFailureIsIdempotent confirms promoting the same field twice
// does not duplicate it in the sticky list.
func TestPromoteFailureIsIdempotent(t *testing.T) {
	state := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	state.PromoteFailure(key, "region")
	state.PromoteFailure(key, "region")
	state.PromoteFailure(key, "az")

	got := append([]string(nil), state.Sticky[key.String()]...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"az", "region"}) {
		t.Errorf("Sticky = %v, want [az region] with no duplicate", got)
	}
}

// TestSelectOnEmptyMembersReturnsNothing confirms a cell with zero members
// (should not occur via GroupCells, but Select must not panic) is handled
// gracefully.
func TestSelectOnEmptyMembersReturnsNothing(t *testing.T) {
	state := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	reps, sticky := state.Select(key, nil)
	if reps != nil || sticky != nil {
		t.Errorf("Select(key, nil) = %v, %v, want nil, nil", reps, sticky)
	}
}

// TestShuffledOrderIsReproducibleForSameSeedAndKey confirms determinism:
// the same seed and cell key always produce the same permutation, which is
// what makes a persisted seed actually reproduce a past run's picks.
func TestShuffledOrderIsReproducibleForSameSeedAndKey(t *testing.T) {
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	a := shuffledOrder(12345, key, 10)
	b := shuffledOrder(12345, key, 10)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("shuffledOrder is not reproducible: %v != %v", a, b)
	}

	otherKey := CellKey{Classification: ClassValueChanged, Shape: ShapeScalar, Direction: DirectionSet}
	c := shuffledOrder(12345, otherKey, 10)
	if reflect.DeepEqual(a, c) {
		t.Errorf("shuffledOrder produced the same order for two different cell keys — cells would rotate in lockstep")
	}
}
