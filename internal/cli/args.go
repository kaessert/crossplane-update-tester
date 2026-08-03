// Package cli holds command-line argument handling shared by the
// update-tester subcommands.
package cli

import (
	"flag"
	"strings"
)

// ReorderArgs moves flag tokens (and their values, when the flag consumes one)
// ahead of positional arguments, so flag.FlagSet.Parse finds every flag
// regardless of where it appears on the command line.
//
// Why this exists: Go's flag package stops scanning at the first non-flag
// argument by design (see the flag.FlagSet.Parse documentation). Everything
// after that point lands in fs.Args() untouched — no error, no warning. So an
// invocation like
//
//	update-tester run manifest.yaml --timeout 600
//
// silently DROPS --timeout: the tool runs with its built-in default and the
// operator is left staring at a timeout failure that names a duration they
// never asked for. The failure is invisible at the call site, which makes it
// expensive to diagnose; downstream Makefiles and hooks end up carrying
// apologetic comments about flag ordering instead. Reordering here makes the
// parser accept flags and positionals in any order, so callers do not have to
// know or encode the rule.
//
// The FlagSet is required, not optional: it is the only thing that can tell a
// recognized flag from an unrecognized one, and a value-taking flag from a
// boolean one. Both distinctions are load-bearing (see below).
//
// Rules applied to each token:
//
//   - "--" is a positional terminator in the conventional CLI grammar, never a
//     flag name. It is kept in the positional stream so it is not hoisted
//     ahead of the arguments it is meant to terminate.
//   - "--flag=value" already carries its value in one token, so no following
//     argument is consumed.
//   - A boolean flag (one whose Value implements IsBoolFlag() bool returning
//     true) does not consume the following argument. Without this check,
//     "--verbose manifest.yaml" would swallow the manifest path as the flag's
//     value and the command would be left with no positional arguments at all.
//   - An unrecognized flag is moved but consumes nothing, and the token after
//     it stays positional. Guessing at its arity risks eating a real argument;
//     leaving it intact lets fs.Parse report "flag provided but not defined"
//     with the user's own spelling, which is the more useful error.
//
// Relative order is preserved within the flag stream and within the positional
// stream; only the two streams are separated. Input is not modified — a new
// slice is returned. A nil or empty args yields an empty result.
func ReorderArgs(fs *flag.FlagSet, args []string) []string {
	var flagTokens, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Not a flag: too short to be one ("", "-"), no leading dash, or the
		// "--" positional terminator.
		if len(a) < 2 || a[0] != '-' || a == "--" {
			positional = append(positional, a)
			continue
		}

		flagTokens = append(flagTokens, a)

		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			// "--flag=value" form already carries its value.
			continue
		}

		f := fs.Lookup(name)
		if f == nil {
			// Unrecognized flag — leave the next token alone and let
			// fs.Parse report the error.
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			// Bool flags take no separate value argument.
			continue
		}

		// Value-taking flag: pull its value along so the pair stays adjacent.
		if i+1 < len(args) {
			flagTokens = append(flagTokens, args[i+1])
			i++
		}
	}

	return append(flagTokens, positional...)
}
