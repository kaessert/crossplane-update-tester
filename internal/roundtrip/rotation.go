package roundtrip

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RepresentativesPerRun implements the size-scaled rotation budget: how
// many representatives of an `equal` cell of the given size a single run
// selects, so that the cell reaches full (exhaustive) coverage within a
// bounded number of runs regardless of how large it is.
//
// The requirement this satisfies: a small cell (e.g. size 4) must reach
// full coverage in ~2 runs, and a large one (e.g. size 72) in under 10. A
// flat one-representative-per-cell budget does not scale to a large cell —
// it would take 72 runs to exhaust a 72-member cell.
//
// The scaling function: target := max(2, ceil(size/8)) is the number of
// runs the budget is built to converge within; reps := ceil(size/target)
// is the resulting per-run budget. Because reps is rounded UP from
// size/target, reps*target is always >= size, so the actual number of
// runs to exhaust the cell (ceil(size/reps)) can never exceed target — the
// bound is structural, not empirical. Worked examples across a representative
// range of cell sizes:
//
//	size  target  reps  runs-to-converge
//	   4       2     2       2
//	   5       2     3       2
//	  12       2     6       2
//	  24       3     8       3
//	  42       6     7       6
//	  44       6     8       6
//	  64       8     8       8
//	  72       9     8       9   (< 10, as required)
func RepresentativesPerRun(size int) int {
	if size <= 0 {
		return 0
	}
	target := size / 8
	if size%8 != 0 {
		target++
	}
	if target < 2 {
		target = 2
	}
	reps := size / target
	if size%target != 0 {
		reps++
	}
	if reps > size {
		reps = size
	}
	return reps
}

// RotationState is the persisted state one rotation decision needs: the
// seed a cell's member order is shuffled from, a per-cell cursor recording
// how far the round-robin has already advanced, and the sticky registry of
// members permanently promoted out of rotation and into every future
// run's representative set.
//
// The seed is generated once, the first time a state file does not yet
// exist (see LoadRotationState), and held STABLE across every subsequent
// run — it is what makes the cursor-based round robin deterministic and
// gives RepresentativesPerRun's convergence bound above a real guarantee
// rather than a probabilistic one: reshuffling the member order on every
// run would make a cursor position meaningless from one run to the next,
// since it would point at a different permutation each time. "Chosen
// pseudo-randomly" describes the ORDER selection is drawn in — a random
// permutation, rather than alphabetical or authoring order, so a field's
// position in the manifest cannot bias which run first exercises it — not
// a claim that the choice re-randomizes every run. Both the seed and the
// resulting per-run selection are recorded here and reported in every
// output this package produces, so a reader can reproduce exactly which
// members a given run picked.
type RotationState struct {
	Seed int64 `json:"seed"`
	// Cursors maps a CellKey's String() to the next unconsumed index into
	// that cell's shuffled member order.
	Cursors map[string]int `json:"cursors"`
	// Sticky maps a CellKey's String() to the sorted field paths permanently
	// promoted into every run's representative set — see PromoteFailure.
	Sticky map[string][]string `json:"sticky"`
}

// String renders a CellKey as the stable identity every report keyed by
// cell uses. It carries no manifest identity — see stateKey for the
// persisted rotation state's own lookup key, which adds one.
func (k CellKey) String() string {
	return fmt.Sprintf("%s|%s|%s", k.Classification, k.Shape, k.Direction)
}

// stateKey combines a manifest scope with a CellKey into the string
// RotationState persists its cursors and sticky registry under.
//
// scope MUST identify the manifest whose own rows produced key and members
// — the tool settles cell membership at per-manifest scope (see GroupCells'
// own doc comment for why), so the rotation cursor that credits those
// members must be scoped identically. Before this, Cursors was keyed by
// CellKey.String() alone: every manifest sharing a (classification, shape,
// direction) triple — the overwhelmingly common case, since most equal
// cells are scalars — advanced the SAME cursor, each modulo its own,
// different member count. That is not a round robin: it is deterministic
// starvation, measured at 30-44% of every provider's equal-cell members
// NEVER selected across 200 replayed runs. Scoping by manifest makes two
// manifests sharing a CellKey mathematically independent: each owns its
// own cursor, so neither's arithmetic can ever land on the other's index
// space.
func stateKey(scope string, key CellKey) string {
	return scope + "\x00" + key.String()
}

// NewRotationState creates a fresh state with a newly generated seed —
// used the first time a provider has no persisted rotation state at all.
func NewRotationState() RotationState {
	return RotationState{
		Seed:    time.Now().UnixNano(),
		Cursors: map[string]int{},
		Sticky:  map[string][]string{},
	}
}

// LoadRotationState reads a persisted RotationState from path. A missing
// file is not an error — it means this is the first run, and a fresh seed
// is generated and will be persisted by the caller's later Save call.
func LoadRotationState(path string) (RotationState, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is operator-controlled, not attacker input
	if errors.Is(err, os.ErrNotExist) {
		return NewRotationState(), nil
	}
	if err != nil {
		return RotationState{}, fmt.Errorf("reading rotation state %s: %w", path, err)
	}
	var s RotationState
	if err := json.Unmarshal(data, &s); err != nil {
		return RotationState{}, fmt.Errorf("parsing rotation state %s: %w", path, err)
	}
	if s.Cursors == nil {
		s.Cursors = map[string]int{}
	}
	if s.Sticky == nil {
		s.Sticky = map[string][]string{}
	}
	return s, nil
}

// Save persists s to path as indented JSON, creating any missing parent
// directory.
func (s RotationState) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding rotation state: %w", err)
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating rotation state directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing rotation state %s: %w", path, err)
	}
	return nil
}

// shuffledOrder returns a deterministic pseudo-random permutation of
// [0,n), derived from seed combined with key so different cells (and
// different seeds) get independent orders.
func shuffledOrder(seed int64, key CellKey, n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s", seed, key.String()) // hash.Hash.Write never returns an error
	// #nosec G404 -- this is a reproducible rotation schedule, not a
	// cryptographic secret; determinism (same seed+key -> same order) is
	// the whole point.
	rng := rand.New(rand.NewSource(int64(h.Sum64())))
	rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

// Select returns this run's chosen representatives for the equal cell
// identified by key within scope, given its full sorted member list, and
// advances THAT (scope, key) pair's cursor by
// RepresentativesPerRun(len(members)) so the NEXT call for the same scope
// and key (the next run, once persisted and reloaded) continues the round
// robin rather than repeating this run's picks. Every member ever promoted
// to sticky within scope is always included, in addition to (never instead
// of) the rotated budget.
//
// scope MUST identify the manifest members was derived from (see stateKey).
// A second manifest that happens to produce the same CellKey — the common
// case, since most equal cells are scalars — gets its OWN cursor under a
// different scope, so its own budget/n arithmetic can never perturb this
// one's position.
//
// members must already be sorted (GroupCells' own callers sort them via
// sortedPaths) so the shuffled order is reproducible independent of
// whatever order rows happened to arrive in.
func (s *RotationState) Select(scope string, key CellKey, members []string) (representatives, sticky []string) {
	n := len(members)
	if n == 0 {
		return nil, nil
	}
	if s.Cursors == nil {
		s.Cursors = map[string]int{}
	}
	if s.Sticky == nil {
		s.Sticky = map[string][]string{}
	}

	skey := stateKey(scope, key)
	order := shuffledOrder(s.Seed, key, n)
	budget := RepresentativesPerRun(n)
	cursor := s.Cursors[skey]

	picked := make(map[string]bool, budget)
	result := make([]string, 0, budget)
	for i := 0; i < budget; i++ {
		m := members[order[(cursor+i)%n]]
		if !picked[m] {
			picked[m] = true
			result = append(result, m)
		}
	}
	s.Cursors[skey] = (cursor + budget) % n

	sticky = append([]string(nil), s.Sticky[skey]...)
	for _, m := range sticky {
		if !picked[m] {
			picked[m] = true
			result = append(result, m)
		}
	}

	sort.Strings(result)
	return result, sticky
}

// PromoteFailure marks field as permanently sticky within scope and key —
// call this the moment a chosen representative's live test fails, so the
// SAME field is selected on every future run of the SAME manifest without
// depending on the rotation schedule to land on it again by chance.
// Idempotent: promoting an already-sticky field is a no-op. scope MUST
// identify the manifest field was observed to fail against — see stateKey.
func (s *RotationState) PromoteFailure(scope string, key CellKey, field string) {
	if s.Sticky == nil {
		s.Sticky = map[string][]string{}
	}
	skey := stateKey(scope, key)
	for _, f := range s.Sticky[skey] {
		if f == field {
			return
		}
	}
	s.Sticky[skey] = append(s.Sticky[skey], field)
	sort.Strings(s.Sticky[skey])
}
