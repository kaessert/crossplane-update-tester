package roundtrip

import "fmt"

// BackendType is a provider's DECLARED backend classification — declared,
// never inferred. Guessing from a provider name, endpoint, or URL is
// exactly the derived-value failure that produces a wrong answer that
// still renders and still passes review; the closed set below and
// ParseBackendType's refusal to default are what keep that guess out of
// this package.
type BackendType string

const (
	// BackendReal marks a provider whose E2E path runs against a live,
	// non-simulated backend. It is never assumed or defaulted — every
	// provider, including one that is in fact real, must declare it
	// explicitly via --backend (see ParseBackendType).
	BackendReal BackendType = "real"
	// BackendSimulator marks a provider whose ENTIRE E2E path runs against
	// a simulator with no real-backend arm at all (e.g. vcsim) — a cell
	// satisfied this way is a weaker claim than one satisfied against a
	// real backend, and Provenance exists so that weaker claim is visible
	// in every report rather than only in a filing conversation.
	BackendSimulator BackendType = "simulator"
)

// ParseBackendType validates s against the closed set above. There is NO
// fallback: an empty or unrecognised value is always an error, never
// defaulted to BackendReal or any other assumption — declaring the
// backend type is part of onboarding a provider onto the cell-denominator
// checks, not something this package can decide on a caller's behalf.
func ParseBackendType(s string) (BackendType, error) {
	switch BackendType(s) {
	case BackendReal, BackendSimulator:
		return BackendType(s), nil
	default:
		return "", fmt.Errorf(
			"backend type must be declared as %q or %q, got %q — it is never inferred from a provider name, endpoint, or URL",
			BackendReal, BackendSimulator, s)
	}
}

// Provenance records which backend declaration satisfied a report's cells.
// A simulator-backed report means every cell in it was satisfied by a
// simulator-derived classification, not a real backend's — restated as its
// own bool so a reader never has to compare Backend against the constant
// themselves in every downstream consumer.
type Provenance struct {
	Backend            BackendType
	SimulatorSatisfied bool
}

// NewProvenance builds a Provenance from a declared backend type.
func NewProvenance(backend BackendType) Provenance {
	return Provenance{Backend: backend, SimulatorSatisfied: backend == BackendSimulator}
}
