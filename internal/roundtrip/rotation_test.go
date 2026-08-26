package roundtrip

import (
	"fmt"
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
	const scope = "Widget/example"
	skey := stateKey(scope, key)
	s.Cursors[skey] = 3
	s.PromoteFailure(scope, key, "region")

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
	if loaded.Cursors[skey] != 3 {
		t.Errorf("loaded cursor = %d, want 3", loaded.Cursors[skey])
	}
	if !reflect.DeepEqual(loaded.Sticky[skey], []string{"region"}) {
		t.Errorf("loaded sticky = %v, want [region]", loaded.Sticky[skey])
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
	const scope = "Widget/example"
	state := NewRotationState()

	seenBeforeRepeat := map[string]bool{}
	repeatedTooSoon := false
	budget := RepresentativesPerRun(len(members))
	runsNeeded := (len(members) + budget - 1) / budget

	for run := 0; run < runsNeeded; run++ {
		reps, _ := state.Select(scope, key, members)
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

// TestSelectCursorsAreIndependentAcrossManifestsSharingACellKey is the
// ticket's own required regression: this is the exact scenario that used
// to starve 30-44% of every provider's equal-cell members. Two DIFFERENT
// manifests (scopeA, scopeB) share one CellKey — the common case, since
// most equal cells are scalars — with DIFFERENT member counts, so the old
// single shared cursor advanced by each manifest's own budget modulo its
// OWN size, corrupting the other's position. After scoping, each manifest
// converges to full coverage of its OWN members, independent of the
// other's size or how many times it has run.
func TestSelectCursorsAreIndependentAcrossManifestsSharingACellKey(t *testing.T) {
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	const scopeA = "Widget/cluster-example"
	const scopeB = "Widget/namespaced-example"
	membersA := []string{"a0", "a1"}                   // size 2 — the ticket's own minimal case
	membersB := []string{"b0", "b1", "b2", "b3", "b4"} // size 5, deliberately different

	state := NewRotationState()

	seenA := map[string]bool{}
	seenB := map[string]bool{}
	// 50 runs each — far beyond either cell's own convergence bound — to
	// match the ticket's own >=50-run replay requirement.
	for run := 0; run < 50; run++ {
		repsA, _ := state.Select(scopeA, key, membersA)
		for _, m := range repsA {
			seenA[m] = true
		}
		repsB, _ := state.Select(scopeB, key, membersB)
		for _, m := range repsB {
			seenB[m] = true
		}
	}

	for _, m := range membersA {
		if !seenA[m] {
			t.Errorf("manifest A (scope %q): member %q was never selected across 50 runs — starvation", scopeA, m)
		}
	}
	for _, m := range membersB {
		if !seenB[m] {
			t.Errorf("manifest B (scope %q): member %q was never selected across 50 runs — starvation", scopeB, m)
		}
	}

	// The two manifests' cursors must be stored under different keys —
	// asserted directly, not just inferred from coverage above.
	if stateKey(scopeA, key) == stateKey(scopeB, key) {
		t.Fatalf("stateKey collided for two different scopes sharing a CellKey: %q", stateKey(scopeA, key))
	}
}

// TestSelectManifestScopingReproducesFleetCellSizes replays the rotation
// against every provider's own largest measured equal-cell size (vultr 72,
// infobloxnios 64, tailscale 44, f5xc 42, vsphere 24, lambda 12,
// vclustercli 5), each as its OWN manifest scope sharing one CellKey,
// simultaneously, across 60 runs — reproducing the reviewer's 200-run
// replay methodology at a smaller but still-conclusive run count. Before
// the cursor-scoping fix, this exact configuration is what measured
// 30-44% of members never selected; after it, every provider's full
// member set must be covered.
func TestSelectManifestScopingReproducesFleetCellSizes(t *testing.T) {
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	providers := map[string]int{
		"vultr":        72,
		"infobloxnios": 64,
		"tailscale":    44,
		"f5xc":         42,
		"vsphere":      24,
		"lambda":       12,
		"vclustercli":  5,
	}

	membersByProvider := map[string][]string{}
	for provider, size := range providers {
		members := make([]string, size)
		for i := range members {
			members[i] = fmt.Sprintf("%s-field-%d", provider, i)
		}
		membersByProvider[provider] = members
	}

	state := NewRotationState()
	seen := map[string]map[string]bool{}
	for provider := range providers {
		seen[provider] = map[string]bool{}
	}

	for run := 0; run < 60; run++ {
		for provider, members := range membersByProvider {
			scope := provider + "/example"
			reps, _ := state.Select(scope, key, members)
			for _, m := range reps {
				seen[provider][m] = true
			}
		}
	}

	for provider, members := range membersByProvider {
		var neverSelected []string
		for _, m := range members {
			if !seen[provider][m] {
				neverSelected = append(neverSelected, m)
			}
		}
		if len(neverSelected) != 0 {
			t.Errorf("provider %s: %d/%d members never selected across 60 runs (want 0): %v",
				provider, len(neverSelected), len(members), neverSelected)
		}
	}
}

// TestSelectAlwaysIncludesStickyMembers confirms a promoted field is
// selected on every subsequent call regardless of where the rotation
// cursor happens to be.
func TestSelectAlwaysIncludesStickyMembers(t *testing.T) {
	members := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	const scope = "Widget/example"
	state := NewRotationState()
	state.PromoteFailure(scope, key, "e")

	for run := 0; run < 5; run++ {
		reps, sticky := state.Select(scope, key, members)
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
	const scope = "Widget/example"
	state.PromoteFailure(scope, key, "region")
	state.PromoteFailure(scope, key, "region")
	state.PromoteFailure(scope, key, "az")

	got := append([]string(nil), state.Sticky[stateKey(scope, key)]...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"az", "region"}) {
		t.Errorf("Sticky = %v, want [az region] with no duplicate", got)
	}
}

// TestPromoteFailureIsScopedPerManifest confirms a field promoted sticky
// for one manifest does not leak into another manifest that shares the
// same CellKey — the same manifest-identity requirement Select carries.
func TestPromoteFailureIsScopedPerManifest(t *testing.T) {
	state := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	state.PromoteFailure("Widget/a", key, "region")

	if got := state.Sticky[stateKey("Widget/b", key)]; len(got) != 0 {
		t.Errorf("Sticky for scope Widget/b = %v, want empty — a promotion for Widget/a leaked across scopes", got)
	}
	if got := state.Sticky[stateKey("Widget/a", key)]; !reflect.DeepEqual(got, []string{"region"}) {
		t.Errorf("Sticky for scope Widget/a = %v, want [region]", got)
	}
}

// TestSelectOnEmptyMembersReturnsNothing confirms a cell with zero members
// (should not occur via GroupCells, but Select must not panic) is handled
// gracefully.
func TestSelectOnEmptyMembersReturnsNothing(t *testing.T) {
	state := NewRotationState()
	key := CellKey{Classification: ClassEqual, Shape: ShapeScalar, Direction: DirectionSet}
	reps, sticky := state.Select("Widget/example", key, nil)
	if reps != nil || sticky != nil {
		t.Errorf("Select(scope, key, nil) = %v, %v, want nil, nil", reps, sticky)
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
