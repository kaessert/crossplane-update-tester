// Package residual walks a directory of Crossplane example manifests and
// reports the repo-scope cell-denominator residual: every skip: entry that
// declares an evidence-tier disposition (see manifest.Disposition),
// enumerated per field per fixture rather than as a single count.
//
// Every ad-hoc script written to take this measurement by hand has parsed
// the crossplane.io/update-test annotation itself, using a bare YAML load
// wrapped in a blanket "on error, skip this fixture". The annotation body
// is a YAML list optionally preceded by directive lines (converge-skip:,
// assert-unchanged:, ignore-fields:), which is NOT valid as a single plain
// YAML document — a mapping key cannot be a sibling of a top-level
// sequence — so a bare load raises on every fixture that uses the
// directive-prefixed form, and "skip it" drops the fixture from the
// numerator and the denominator in the same step. The result stays
// internally consistent and looks correct while silently under-counting
// both sides.
//
// Scan closes that gap by going through manifest.Parse — the SAME parser
// every other subcommand in this tool uses, directive lines and all — so a
// caller here can never diverge from what the tool itself considers a
// valid annotation, and a fixture that fails to parse is reported by name
// instead of disappearing.
package residual

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// Row is one disposition-carrying skip: entry: a single field, in a single
// fixture, whose skip: block declares an evidence-tier disposition. A
// legacy free-prose skip, or a structured skip with no disposition: key at
// all, is not a Row here — those are tracked by the tool's other coverage
// surfaces (validator's offline checks, roundtrip's own must-test
// denominator); this package reports only the evidence-tier axis a
// disposition: key declares.
type Row struct {
	// Fixture is the manifest file path, relative to the walked root when
	// the file lives under it (the common case), else the path as given.
	Fixture string
	// Field is the update-test entry's own "field:" leaf path.
	Field string
	// Disposition is the entry's declared evidence tier.
	Disposition manifest.Disposition
}

// ParseFailure names one fixture whose YAML failed to parse through
// manifest.Parse even though its raw text carries the annotation key —
// see Scan's own doc comment for why this must never be silently folded
// into "no annotation".
type ParseFailure struct {
	// Path is the fixture's path, exactly as reported by Scan's walk (not
	// relativized), so a caller can open the exact file that failed.
	Path string
	// Err is the underlying manifest.Parse error.
	Err error
}

// Counts is the reported denominator pair. FixturesWithAnnotation is a
// TEXTUAL count, independent of whether the annotation went on to parse —
// it can be answered even for a fixture whose YAML is broken.
// ParsedOK is however many of those fixtures actually parsed clean
// through manifest.Parse. The two are reported side by side deliberately:
// a residual count with no denominator beside it is unfalsifiable, and a
// caller that only ever consulted manifest.Parse could never observe a
// fixture whose YAML fails to decode at all — it would just disappear.
type Counts struct {
	FixturesWithAnnotation int
	ParsedOK               int
}

// Result is everything one Scan call over one root produces.
type Result struct {
	Counts   Counts
	Rows     []Row
	Failures []ParseFailure
}

// Dispositions is the reporting order every grouped rendering in this
// package uses — the same order manifest's own closed set is declared in,
// so a reader who already knows that ordering from a skip: validation
// error sees the identical order here.
var Dispositions = []manifest.Disposition{
	manifest.DispositionStaticallyProvable,
	manifest.DispositionOneLivePatch,
	manifest.DispositionDeclaredExclusion,
	manifest.DispositionDefect,
}

// dispositionRank returns Dispositions' index for d, or len(Dispositions)
// for any value outside the closed set — defensive only: manifest's own
// parse-time validation already rejects an unrecognized disposition:
// value before a Row carrying it could ever reach this package.
func dispositionRank(d manifest.Disposition) int {
	for i, known := range Dispositions {
		if known == d {
			return i
		}
	}
	return len(Dispositions)
}

// Scan walks root for every *.yaml/*.yml file, finds the ones whose raw
// text carries a crossplane.io/update-test annotation key line, and
// parses each through manifest.Parse. Returned rows and failures are
// sorted for deterministic output: rows by (disposition, fixture, field),
// failures by path.
//
// A walk failure (root does not exist, a directory it cannot read) is
// returned as an error; a single fixture's own parse failure is never
// fatal to the scan — it is recorded in Result.Failures and the walk
// continues, so one broken fixture cannot hide every other fixture's
// count.
func Scan(root string) (Result, error) {
	var res Result

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking %s: %w", path, walkErr)
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		// #nosec G304 -- path comes from filepath.WalkDir over a
		// directory this same call resolved, not attacker-controlled
		// input.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}
		if !hasAnnotationKeyLine(data) {
			return nil
		}
		res.Counts.FixturesWithAnnotation++

		m, parseErr := manifest.Parse(path)
		if parseErr != nil {
			res.Failures = append(res.Failures, ParseFailure{Path: path, Err: parseErr})
			return nil
		}
		res.Counts.ParsedOK++

		fixture := path
		if rel, relErr := filepath.Rel(root, path); relErr == nil && !strings.HasPrefix(rel, "..") {
			fixture = rel
		}
		for _, t := range m.Tests {
			if !t.Skip.Present() || t.Skip.Disposition == "" {
				continue
			}
			res.Rows = append(res.Rows, Row{Fixture: fixture, Field: t.Field, Disposition: t.Skip.Disposition})
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i], res.Rows[j]
		if a.Disposition != b.Disposition {
			return dispositionRank(a.Disposition) < dispositionRank(b.Disposition)
		}
		if a.Fixture != b.Fixture {
			return a.Fixture < b.Fixture
		}
		return a.Field < b.Field
	})
	sort.Slice(res.Failures, func(i, j int) bool { return res.Failures[i].Path < res.Failures[j].Path })

	return res, nil
}

// hasAnnotationKeyLine reports whether raw manifest text carries a line
// that assigns the update-test annotation key — optionally indented,
// exactly as it appears under metadata.annotations in every real fixture
// — as opposed to a prose comment that merely mentions the key in passing
// (which never carries the colon immediately after "update-test"; that
// colon is the one thing checked here). This textual, parser-independent
// check is what makes FixturesWithAnnotation and ParsedOK genuinely
// independent numbers: manifest.Parse cannot even be asked the question
// for a file whose YAML fails to decode at all, so a pair that consulted
// manifest.Parse alone could never observe the failure this scan exists
// to catch.
func hasAnnotationKeyLine(data []byte) bool {
	key := manifest.AnnotationKey + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), key) {
			return true
		}
	}
	return false
}

// printfTo writes a report line and discards the write error: every caller
// here is printing a human-readable report to stdout (or, under test, to a
// buffer), and there is no recovery a caller could attempt if that write
// failed.
func printfTo(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// PrintReport renders Counts as a pair, every parse failure by name, and
// every row grouped by disposition, then by fixture, then by field —
// never as a bare total. declared-exclusion is rendered as its own group,
// distinct from the other three dispositions, because a standing human
// declaration is not the same kind of evidence as a re-checkable one, and
// a reader must be able to see which is which without recomputing it.
func PrintReport(w io.Writer, res Result) {
	printfTo(w, "fixtures_with_annotation=%d parsed_ok=%d\n", res.Counts.FixturesWithAnnotation, res.Counts.ParsedOK)

	if len(res.Failures) > 0 {
		printfTo(w, "\n%d fixture(s) failed to parse (never treated as zero rows):\n", len(res.Failures))
		for _, f := range res.Failures {
			printfTo(w, "  \u2717 %s: %v\n", f.Path, f.Err)
		}
	}

	grouped := make(map[manifest.Disposition][]Row, len(Dispositions))
	for _, r := range res.Rows {
		grouped[r.Disposition] = append(grouped[r.Disposition], r)
	}

	printfTo(w, "\n%d disposition row(s):\n", len(res.Rows))
	for _, d := range Dispositions {
		rows := grouped[d]
		if len(rows) == 0 {
			continue
		}
		printfTo(w, "\ndisposition: %s (%d)\n", d, len(rows))
		for _, r := range rows {
			printfTo(w, "  %s: %s\n", r.Fixture, r.Field)
		}
	}
}
