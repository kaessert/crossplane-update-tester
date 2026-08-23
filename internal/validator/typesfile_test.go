package validator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTypesFileAt writes a minimal, gofmt-shaped Go source file declaring
// "type <structName> struct { ... }" at root/relPath, creating any missing
// parent directories — unlike writeFixture (validator_test.go), which only
// ever writes directly inside t.TempDir() and so cannot exercise the nested
// apis/<scope>/<resourcedir>/v1alpha1/ layout FindTypesFile must resolve.
func writeTypesFileAt(t *testing.T, root, relPath, structName string) string {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating parent dirs for %s: %v", relPath, err)
	}
	src := "package v1alpha1\n\ntype " + structName + " struct {\n\tName *string `json:\"name,omitempty\"`\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
	return path
}

// TestFindTypesFileUniqueMatch covers the two apis/ layouts the fleet
// actually carries: flat (apis/<scope>/v1alpha1/zz_<x>_types.go) and
// nested-per-resource (apis/<scope>/<resourcedir>/v1alpha1/zz_<x>_types.go).
// Identity resolution must succeed on both without being told which shape
// this provider uses.
func TestFindTypesFileUniqueMatch(t *testing.T) {
	cases := map[string]struct {
		reason  string
		relPath string
	}{
		"Flat": {
			reason:  "a single-package apis/<scope>/v1alpha1/ layout (tailscale's shape)",
			relPath: "apis/cluster/v1alpha1/zz_widget_types.go",
		},
		"NestedPerResource": {
			reason:  "one directory per resource under apis/<scope>/ (lambda's shape)",
			relPath: "apis/cluster/widget/v1alpha1/zz_widget_types.go",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			want := writeTypesFileAt(t, root, tc.relPath, "WidgetParameters")

			got, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
			if err != nil {
				t.Fatalf("%s: FindTypesFile: %v", tc.reason, err)
			}
			if got != want {
				t.Errorf("%s: FindTypesFile = %q, want %q", tc.reason, got, want)
			}
		})
	}
}

// TestFindTypesFileScopeFromAPIVersionGroup covers both scope directions and
// confirms scope is read from the apiVersion GROUP, never the version or any
// other surface — the FAC-UTV-SCOPE-AV defect this must not reintroduce.
func TestFindTypesFileScopeFromAPIVersionGroup(t *testing.T) {
	cases := map[string]struct {
		reason     string
		apiVersion string
		relPath    string
	}{
		"ClusterGroup": {
			reason:     "a group with no .m. segment resolves to apis/cluster",
			apiVersion: "widget.crossplane.io/v1alpha1",
			relPath:    "apis/cluster/v1alpha1/zz_widget_types.go",
		},
		"NamespacedGroup": {
			reason:     "a group ending .m.crossplane.io resolves to apis/namespaced",
			apiVersion: "widget.m.crossplane.io/v1alpha1",
			relPath:    "apis/namespaced/v1alpha1/zz_widget_types.go",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			want := writeTypesFileAt(t, root, tc.relPath, "WidgetParameters")

			got, err := FindTypesFile(root, tc.apiVersion, "Widget")
			if err != nil {
				t.Fatalf("%s: FindTypesFile: %v", tc.reason, err)
			}
			if got != want {
				t.Errorf("%s: FindTypesFile = %q, want %q", tc.reason, got, want)
			}
		})
	}
}

// TestScopeForAPIVersionIgnoresVersionSegment pins the exact boundary: only
// the GROUP (everything before the first "/") is consulted, so a version
// segment that happens to contain "m." must never flip the result.
func TestScopeForAPIVersionIgnoresVersionSegment(t *testing.T) {
	got := ScopeForAPIVersion("widget.crossplane.io/m.alpha1")
	if got != "cluster" {
		t.Errorf("ScopeForAPIVersion with .m. only in the version segment = %q, want cluster", got)
	}
}

// TestFindTypesFileZeroMatches confirms the loud, named failure for the
// common typo/never-generated case: no file anywhere under apis/<scope>/
// declares the target struct. The message must name the Kind, the resolved
// scope and the directory searched so the failure is actionable without
// re-running with -v.
func TestFindTypesFileZeroMatches(t *testing.T) {
	root := t.TempDir()
	// A types file exists, but for a different Kind — proves this is a
	// genuine "not found" and not an empty-directory special case.
	writeTypesFileAt(t, root, "apis/cluster/v1alpha1/zz_other_types.go", "OtherParameters")

	_, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
	if err == nil {
		t.Fatal("expected an error when no file declares WidgetParameters")
	}
	for _, want := range []string{"Widget", "cluster", filepath.Join(root, "apis", "cluster")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("FindTypesFile zero-match error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// TestFindTypesFileZeroMatchesMissingScopeDir confirms a --root that has no
// apis/<scope> directory at all (a typo'd --root, or a provider that has not
// generated this scope) fails the same way as a present-but-empty
// directory — never a raw "no such file or directory" from the walk.
func TestFindTypesFileZeroMatchesMissingScopeDir(t *testing.T) {
	root := t.TempDir() // no apis/ subtree at all

	_, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
	if err == nil {
		t.Fatal("expected an error when apis/cluster does not exist")
	}
	if !strings.Contains(err.Error(), "Widget") {
		t.Errorf("FindTypesFile missing-scope-dir error = %q, want it to mention Widget", err.Error())
	}
}

// TestFindTypesFileMultipleMatches confirms an ambiguous resolution fails
// loudly and names every candidate, rather than silently picking one (e.g.
// lexical-first) — a wrong-but-plausible answer here is worse than no
// answer, since ParseGoTypes would happily parse whichever one is returned.
func TestFindTypesFileMultipleMatches(t *testing.T) {
	root := t.TempDir()
	first := writeTypesFileAt(t, root, "apis/cluster/v1alpha1/zz_widget_types.go", "WidgetParameters")
	second := writeTypesFileAt(t, root, "apis/cluster/widget/v1alpha1/zz_widget_types.go", "WidgetParameters")

	_, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
	if err == nil {
		t.Fatal("expected an error when two files declare WidgetParameters")
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("FindTypesFile multi-match error = %q, want it to name %q", err.Error(), want)
		}
	}
}

// TestFindTypesFileDoesNotMatchSuffixedStructName confirms the discovery
// match is exact, not a substring: a struct whose name merely ENDS with
// "WidgetParameters" (e.g. a "BarWidgetParameters" sibling type) must not be
// mistaken for the target.
func TestFindTypesFileDoesNotMatchSuffixedStructName(t *testing.T) {
	root := t.TempDir()
	writeTypesFileAt(t, root, "apis/cluster/v1alpha1/zz_other_types.go", "BarWidgetParameters")

	_, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
	if err == nil {
		t.Fatal("expected an error: only a suffix-matching struct name exists, not an exact WidgetParameters")
	}
}

// TestFindTypesFileIgnoresTestFiles confirms a _test.go file declaring the
// same struct name (e.g. a table-driven test's local fixture type) is never
// treated as a candidate.
func TestFindTypesFileIgnoresTestFiles(t *testing.T) {
	root := t.TempDir()
	want := writeTypesFileAt(t, root, "apis/cluster/v1alpha1/zz_widget_types.go", "WidgetParameters")
	writeTypesFileAt(t, root, "apis/cluster/v1alpha1/zz_widget_types_test.go", "WidgetParameters")

	got, err := FindTypesFile(root, "widget.crossplane.io/v1alpha1", "Widget")
	if err != nil {
		t.Fatalf("FindTypesFile: %v", err)
	}
	if got != want {
		t.Errorf("FindTypesFile = %q, want %q (the non-test file)", got, want)
	}
}
