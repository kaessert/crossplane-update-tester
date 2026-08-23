package validator

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FindTypesFile resolves a provider's generated Go types file by IDENTITY —
// the file under root/apis/<scope>/ that declares "type <Kind>Parameters
// struct" — instead of guessing its path from the resource's name. Provider
// apis/ layouts vary (some flatten every Kind's types into a single
// apis/<scope>/v1alpha1/ package, others nest one directory per resource,
// and generated filenames pluralize irregularly), so a path built from the
// manifest's own name is a guess that can silently resolve to the wrong
// file. Scanning for the struct declaration itself is not: it can only
// succeed against the file that actually declares it, or fail loudly.
//
// Scope is derived from apiVersion's group — the segment before the first
// "/" — never from the manifest's filename: a group ending in
// ".m.crossplane.io" resolves to apis/namespaced, every other group to
// apis/cluster. This mirrors the one scope-resolution rule every dual-scope
// provider in the fleet already follows for its own tooling. Deriving scope
// from the filename instead has already produced one masked defect (a
// bundled prerequisite manifest whose filename did not match its own
// scope), so it is deliberately not accepted here even as a fallback.
//
// Both zero matches and more than one match are reported as errors naming
// the Kind, the resolved scope, and the directory searched. Silently
// falling back to a wrong-but-existing path is exactly the failure mode
// this replaces, so neither case is resolved by guessing.
func FindTypesFile(root, apiVersion, kind string) (string, error) {
	scope := ScopeForAPIVersion(apiVersion)
	dir := filepath.Join(root, "apis", scope)
	structName := kind + "Parameters"

	matches, err := findStructDeclarations(dir, structName)
	if err != nil {
		return "", fmt.Errorf("searching %s for \"type %s struct\": %w", dir, structName, err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no file under %s declares %q (kind %q, scope %q)", dir, "type "+structName+" struct", kind, scope)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple files under %s declare %q (kind %q, scope %q): %s",
			dir, "type "+structName+" struct", kind, scope, strings.Join(matches, ", "))
	}
}

// ScopeForAPIVersion derives a resource's Crossplane scope directory
// ("cluster" or "namespaced") from its manifest's apiVersion GROUP — the
// segment before the first "/" — never from the version or from any other
// surface. A group ending in ".m.crossplane.io" is the namespaced variant;
// every other group is cluster-scoped. This is the one place that mapping
// is made, so every caller that needs it (FindTypesFile today) resolves
// scope identically.
func ScopeForAPIVersion(apiVersion string) string {
	group := apiVersion
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		group = apiVersion[:idx]
	}
	if strings.HasSuffix(group, ".m.crossplane.io") {
		return "namespaced"
	}
	return "cluster"
}

// findStructDeclarations walks dir for .go files (excluding _test.go) that
// declare "type <structName> struct", returning every match sorted for a
// deterministic error message when more than one is found. A missing dir
// (the common shape of "this provider has no apis/<scope> at all", or a
// typo'd --root) is reported as zero matches rather than a walk error, so
// the caller's message stays about the missing declaration, not the
// directory.
func findStructDeclarations(dir, structName string) ([]string, error) {
	var matches []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		declares, err := fileDeclaresStruct(path, structName)
		if err != nil {
			return err
		}
		if declares {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// fileDeclaresStruct reports whether the Go source file at path declares
// "type <structName> struct" using the same reAnyStruct pattern
// ParseStructFields uses to locate a target struct, so a file this function
// says "declares X" is the exact file ParseStructFields can actually parse
// X from.
func fileDeclaresStruct(path, structName string) (bool, error) {
	// #nosec G304 -- path comes from filepath.Walk over a directory this
	// same call resolved, not attacker-controlled input.
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if m := reAnyStruct.FindStringSubmatch(scanner.Text()); m != nil && m[1] == structName {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	return false, nil
}
