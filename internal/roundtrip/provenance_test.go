package roundtrip

import "testing"

// TestParseBackendTypeClosedSetNoFallback confirms the backend type is
// declared, never inferred: anything outside the closed set — including
// empty string — is an error, never a silent default.
func TestParseBackendTypeClosedSetNoFallback(t *testing.T) {
	cases := map[string]struct {
		input   string
		want    BackendType
		wantErr bool
	}{
		"real is accepted":               {input: "real", want: BackendReal},
		"simulator is accepted":          {input: "simulator", want: BackendSimulator},
		"empty string is an error":       {input: "", wantErr: true},
		"unrecognised value is an error": {input: "vcenter", wantErr: true},
		"case-sensitive — Real rejected": {input: "Real", wantErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseBackendType(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseBackendType(%q) = %q, nil, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBackendType(%q) = %v, want nil error", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("ParseBackendType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestNewProvenanceMarksSimulatorSatisfiedOnlyForSimulator confirms the
// restated bool matches Backend exactly, in both directions.
func TestNewProvenanceMarksSimulatorSatisfiedOnlyForSimulator(t *testing.T) {
	real := NewProvenance(BackendReal)
	if real.SimulatorSatisfied {
		t.Errorf("NewProvenance(BackendReal).SimulatorSatisfied = true, want false")
	}

	sim := NewProvenance(BackendSimulator)
	if !sim.SimulatorSatisfied {
		t.Errorf("NewProvenance(BackendSimulator).SimulatorSatisfied = false, want true")
	}
}
