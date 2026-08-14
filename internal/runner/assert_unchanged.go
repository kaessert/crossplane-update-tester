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
//
// A field path that does not resolve on the object is an error here, not a
// silent "" baseline. Without this check, a typo'd path (a stray container
// segment, a renamed field) reads as absent on every snapshot — baseline and
// every later observation alike — so it compares "" to "" for the whole run
// and the guard reports a green checkmark while never having measured
// anything. Rejecting it at baseline time, before any field test has
// patched the resource, turns that silent no-op into a loud, immediate
// parse-time-adjacent failure instead of a false sense of coverage.
func readAssertUnchangedBaselines(snapshot []byte, fields []string) (map[string]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	baselines := make(map[string]string, len(fields))
	for _, f := range fields {
		v, exists, err := readSnapshotField(snapshot, f)
		if err != nil {
			return nil, fmt.Errorf("reading assert-unchanged baseline for %q: %w", f, err)
		}
		if !exists {
			return nil, fmt.Errorf(
				"assert-unchanged field %q: no such field in status.atProvider — check the path for a typo, "+
					"a stray container segment, or a field the backend has not populated yet", f)
		}
		baselines[f] = v
	}
	return baselines, nil
}

// readSnapshotField navigates a Runner.Snapshot() result (the
// status.atProvider subtree, already unwrapped) to a dot-separated field
// path and stringifies the value found there using the same rules
// Runner.ReadField uses for a live read, so a baseline captured here and a
// later observation captured the same way are directly comparable. The
// returned bool reports whether the field actually exists — see
// navigateJSONPath — so a caller that needs to reject a genuinely-absent
// field (readAssertUnchangedBaselines) can tell that apart from one that
// exists but stringifies to the same empty value.
func readSnapshotField(snapshot []byte, field string) (string, bool, error) {
	var atProvider map[string]interface{}
	if err := json.Unmarshal(snapshot, &atProvider); err != nil {
		return "", false, fmt.Errorf("parsing snapshot: %w", err)
	}
	// The snapshot IS the atProvider subtree already (Snapshot marshals it
	// directly), so there is no container to descend through first — unlike
	// navigateAtProvider, which starts from the whole resource object.
	val, exists, err := navigateJSONPath(atProvider, nil, field)
	if err != nil {
		return "", false, err
	}
	s, err := stringifyFieldValue(val, field)
	return s, exists, err
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
		cur, _, err := readSnapshotField(snapshot, f)
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
