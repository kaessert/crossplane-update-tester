package runner

import (
	"encoding/json"
	"fmt"
)

// UnchangedAssertion records a manifest-declared assert-unchanged field (see
// manifest.Manifest.AssertUnchanged) whose live value drifted from its
// pre-run baseline at some point during a `run`. Every UnchangedAssertion
// RunTests returns is a GATING failure: proof that some field test's patch —
// named here as AfterField — caused the backend to silently change a field
// nobody asked it to touch, most commonly a backend defaulting an omitted
// union member on every write.
type UnchangedAssertion struct {
	// Field is the dot-separated status.atProvider path the manifest named.
	Field string
	// Baseline is the field's value as read from the very first snapshot
	// taken in the run, before any field test had patched anything.
	Baseline string
	// Observed is the field's value at the point the drift was first
	// detected.
	Observed string
	// AfterField names the update-test field whose patch was applied
	// immediately before the drift was first observed — the single most
	// useful fact for an operator chasing a silent-wipe report, since it
	// points straight at the unrelated write that triggered it.
	AfterField string
}

// readAssertUnchangedBaselines reads the current value of every named
// status.atProvider field from a Runner.Snapshot() result, for use as the
// run's baseline before any field test patches anything. Returns nil, nil
// when fields is empty, so a manifest that declares no assert-unchanged
// fields pays no cost beyond the check.
func readAssertUnchangedBaselines(snapshot []byte, fields []string) (map[string]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	baselines := make(map[string]string, len(fields))
	for _, f := range fields {
		v, err := readSnapshotField(snapshot, f)
		if err != nil {
			return nil, fmt.Errorf("reading assert-unchanged baseline for %q: %w", f, err)
		}
		baselines[f] = v
	}
	return baselines, nil
}

// readSnapshotField navigates a Runner.Snapshot() result (the
// status.atProvider subtree, already unwrapped) to a dot-separated field
// path and stringifies the value found there using the same rules
// Runner.ReadField uses for a live read, so a baseline captured here and a
// later observation captured the same way are directly comparable.
func readSnapshotField(snapshot []byte, field string) (string, error) {
	var atProvider map[string]interface{}
	if err := json.Unmarshal(snapshot, &atProvider); err != nil {
		return "", fmt.Errorf("parsing snapshot: %w", err)
	}
	// The snapshot IS the atProvider subtree already (Snapshot marshals it
	// directly), so there is no container to descend through first — unlike
	// navigateAtProvider, which starts from the whole resource object.
	val, err := navigateJSONPath(atProvider, nil, field)
	if err != nil {
		return "", err
	}
	return stringifyFieldValue(val, field)
}

// checkAssertUnchanged compares the current value of every not-yet-violated
// assert-unchanged field against its baseline, using a snapshot taken
// immediately after one field test's patch and reconcile. Every field whose
// value has moved is reported exactly once — marked in violated so it is
// never reported again even if it keeps differing from the baseline on
// every subsequent field test — and attributed to afterField, the update-test
// field whose patch was applied just before this snapshot was taken.
//
// A read failure for one field does not stop the others from being checked;
// it is returned as err (the first one encountered) so the caller can
// surface it without losing whatever violations were found on the fields
// that could be read.
func checkAssertUnchanged(snapshot []byte, fields []string, baselines map[string]string, violated map[string]bool, afterField string) ([]UnchangedAssertion, error) {
	var out []UnchangedAssertion
	var firstErr error
	for _, f := range fields {
		if violated[f] {
			continue
		}
		cur, err := readSnapshotField(snapshot, f)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reading %q: %w", f, err)
			}
			continue
		}
		if cur != baselines[f] {
			violated[f] = true
			out = append(out, UnchangedAssertion{
				Field:      f,
				Baseline:   baselines[f],
				Observed:   cur,
				AfterField: afterField,
			})
		}
	}
	return out, firstErr
}
