package cli

import (
	"flag"
	"slices"
	"testing"
	"time"
)

// newFlagSet builds a FlagSet with a representative mix of flag kinds:
// a value-taking int, a value-taking string, a duration, and a bool.
func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Int("timeout", 120, "")
	fs.String("output", "", "")
	fs.Duration("poll-interval", 60*time.Second, "")
	fs.Bool("verbose", false, "")
	return fs
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("ReorderArgs() = %v, want %v", got, want)
	}
}

// TestReorderArgsFlagAfterPositional is the primary regression test.
// Go's flag package stops scanning at the first non-flag argument, so
// without reordering `run manifest.yaml --timeout 600` silently drops
// --timeout into fs.Args() and leaves the default in effect.
func TestReorderArgsFlagAfterPositional(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "--timeout", "600"})
	assertArgs(t, got, []string{"--timeout", "600", "manifest.yaml"})
}

// TestReorderArgsFlagBeforePositionalIsUnchanged covers the already-correct
// ordering: reordering must be a no-op there, not a reshuffle.
func TestReorderArgsFlagBeforePositionalIsUnchanged(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"--timeout", "600", "manifest.yaml"})
	assertArgs(t, got, []string{"--timeout", "600", "manifest.yaml"})
}

// TestReorderArgsFlagsInterspersed covers flags on both sides of the
// positional argument, with their relative order preserved.
func TestReorderArgsFlagsInterspersed(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{
		"--poll-interval", "90s", "manifest.yaml", "--timeout", "600", "--output", "report.json",
	})
	assertArgs(t, got, []string{
		"--poll-interval", "90s", "--timeout", "600", "--output", "report.json", "manifest.yaml",
	})
}

// TestReorderArgsEqualsFormStaysOneToken verifies "--flag=value" is carried
// as a single token and does not additionally consume the next argument.
func TestReorderArgsEqualsFormStaysOneToken(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "--timeout=600"})
	assertArgs(t, got, []string{"--timeout=600", "manifest.yaml"})
}

// TestReorderArgsEqualsFormWithEmptyValue guards the "--flag=" edge case:
// the value is empty but present, so nothing further may be consumed.
func TestReorderArgsEqualsFormWithEmptyValue(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"--output=", "manifest.yaml"})
	assertArgs(t, got, []string{"--output=", "manifest.yaml"})
}

// TestReorderArgsBoolFlagDoesNotConsumeNext verifies a bool flag is not
// mistaken for a value-taking one. If it were, the manifest path would be
// swallowed as the flag's value and the command would see no positionals.
func TestReorderArgsBoolFlagDoesNotConsumeNext(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"--verbose", "manifest.yaml"})
	assertArgs(t, got, []string{"--verbose", "manifest.yaml"})
}

// TestReorderArgsBoolFlagAfterPositionalDoesNotConsumeNext is the same
// guarantee when the bool flag is hoisted from behind a positional and the
// following token is another positional.
func TestReorderArgsBoolFlagAfterPositionalDoesNotConsumeNext(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "--verbose", "other.yaml"})
	assertArgs(t, got, []string{"--verbose", "manifest.yaml", "other.yaml"})
}

// TestReorderArgsUnrecognizedFlagLeftForParseToReport verifies an unknown
// flag is moved but consumes nothing, so fs.Parse can report it verbatim
// instead of this helper silently swallowing it along with a real argument.
func TestReorderArgsUnrecognizedFlagLeftForParseToReport(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "--bogus", "value"})
	assertArgs(t, got, []string{"--bogus", "manifest.yaml", "value"})
}

// TestReorderArgsUnrecognizedFlagStillReportedByParse pins the payoff of the
// rule above: fs.Parse must still fail on the unknown flag.
func TestReorderArgsUnrecognizedFlagStillReportedByParse(t *testing.T) {
	fs := newFlagSet()
	fs.SetOutput(discard{})
	err := fs.Parse(ReorderArgs(fs, []string{"manifest.yaml", "--bogus"}))
	if err == nil {
		t.Fatal("fs.Parse() = nil, want an error for the undefined flag --bogus")
	}
}

// TestReorderArgsDoubleDashTreatedAsPositional verifies "--" is handled as
// the positional terminator it is, not as a flag named "" that would be
// hoisted ahead of the arguments it terminates.
func TestReorderArgsDoubleDashTreatedAsPositional(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"--", "manifest.yaml"})
	assertArgs(t, got, []string{"--", "manifest.yaml"})
}

// TestReorderArgsSingleDashTreatedAsPositional covers the lone "-" token,
// conventionally meaning stdin.
func TestReorderArgsSingleDashTreatedAsPositional(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"-", "--timeout", "600"})
	assertArgs(t, got, []string{"--timeout", "600", "-"})
}

// TestReorderArgsSingleDashFlagFormRecognized verifies the single-dash
// spelling accepted by the flag package ("-timeout 600") is also handled.
func TestReorderArgsSingleDashFlagFormRecognized(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "-timeout", "600"})
	assertArgs(t, got, []string{"-timeout", "600", "manifest.yaml"})
}

// TestReorderArgsPositionalOrderPreserved verifies the positional stream keeps
// its input order — these are usually paths, where order is meaningful.
func TestReorderArgsPositionalOrderPreserved(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"first.yaml", "--timeout", "600", "second.yaml", "third.yaml"})
	assertArgs(t, got, []string{"--timeout", "600", "first.yaml", "second.yaml", "third.yaml"})
}

// TestReorderArgsValueTakingFlagWithMissingValue verifies a trailing
// value-taking flag with nothing after it does not panic; fs.Parse reports
// the missing value.
func TestReorderArgsValueTakingFlagWithMissingValue(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml", "--timeout"})
	assertArgs(t, got, []string{"--timeout", "manifest.yaml"})
}

// TestReorderArgsNoFlags verifies a flagless invocation is untouched.
func TestReorderArgsNoFlags(t *testing.T) {
	got := ReorderArgs(newFlagSet(), []string{"manifest.yaml"})
	assertArgs(t, got, []string{"manifest.yaml"})
}

// TestReorderArgsEmpty covers empty and nil input.
func TestReorderArgsEmpty(t *testing.T) {
	if got := ReorderArgs(newFlagSet(), nil); len(got) != 0 {
		t.Errorf("ReorderArgs(nil) = %v, want empty", got)
	}
	if got := ReorderArgs(newFlagSet(), []string{}); len(got) != 0 {
		t.Errorf("ReorderArgs([]) = %v, want empty", got)
	}
}

// TestReorderArgsDoesNotMutateInput verifies the caller's slice is left
// intact, since callers commonly hold on to the original os.Args tail.
func TestReorderArgsDoesNotMutateInput(t *testing.T) {
	in := []string{"manifest.yaml", "--timeout", "600"}
	want := []string{"manifest.yaml", "--timeout", "600"}
	ReorderArgs(newFlagSet(), in)
	assertArgs(t, in, want)
}

// TestReorderArgsEndToEndParsesFlagAfterPositional is the end-to-end proof:
// with reordering applied, fs.Parse actually applies a flag that appears
// after the positional argument, and the positional survives.
func TestReorderArgsEndToEndParsesFlagAfterPositional(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	timeout := fs.Int("timeout", 120, "")
	verbose := fs.Bool("verbose", false, "")

	args := []string{"manifest.yaml", "--timeout", "600", "--verbose"}
	if err := fs.Parse(ReorderArgs(fs, args)); err != nil {
		t.Fatalf("fs.Parse() error: %v", err)
	}
	if *timeout != 600 {
		t.Errorf("timeout = %d, want 600 (flag after positional must still be applied)", *timeout)
	}
	if !*verbose {
		t.Error("verbose = false, want true")
	}
	if fs.NArg() != 1 || fs.Arg(0) != "manifest.yaml" {
		t.Errorf("fs.Args() = %v, want [manifest.yaml]", fs.Args())
	}
}

// discard silences FlagSet error output in tests that expect a parse failure.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
