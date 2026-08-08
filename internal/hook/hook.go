// Package hook derives the example-manifest path that a post-assert E2E hook
// should test, from the name it was invoked under.
//
// A provider repository ships a single hook binary and one symlink per
// resource, named post-assert-<slug>.sh. The symlink name — and nothing else —
// tells the hook which manifest to exercise, so the same binary serves every
// resource in every provider without modification. Derive reproduces that
// mapping.
//
// Naming convention:
//
//	post-assert-<resource>.sh            -> examples/<resource>/<resource>.yaml
//	post-assert-<resource>-namespaced.sh -> examples/<resource>/<resource>-namespaced.yaml
//	post-assert-<resource>-ns.sh         -> examples/<resource>/<resource>-namespaced.yaml
//
// A resource slug may itself contain hyphens (for example "compute-instance");
// only a trailing -namespaced or -ns is treated as a scope suffix.
//
// Two of the rules are resolved against the filesystem rather than by string
// manipulation alone (the -ns collision check and the sibling-variant
// fallback); both are documented on Derive.
package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// invocationPrefix is the fixed prefix of every per-resource hook symlink.
const invocationPrefix = "post-assert-"

// examplesDir is the directory, relative to the provider repo root, that holds
// one subdirectory of example manifests per resource.
const examplesDir = "examples"

// Derive resolves the manifest path for an invocation name such as
// "post-assert-network-v6.sh", rooted at the provider repo root.
//
// invocationName is the name the hook was invoked under — the symlink name,
// not the symlink target. It may be a bare name or a path; only its base name
// is used, and a trailing ".sh" is stripped. Whatever remains after the
// "post-assert-" prefix is the slug.
//
// The returned path is always an existing file. If neither the primary
// derivation nor the sibling-variant fallback names a file that exists, Derive
// returns an error listing the invocation, the derived slug and every path it
// tried. That error text is the only diagnostic an operator gets when a hook
// misfires in the middle of an E2E run, so it names the paths rather than just
// reporting failure.
//
// # Why the -ns suffix is checked against the filesystem
//
// "-ns" is shorthand for "-namespaced", but it is also a legitimate ending for
// a resource name in its own right — a DNS NS record resource is naturally
// called "record-ns". Treating the suffix as shorthand unconditionally would
// silently redirect post-assert-record-ns.sh to
// examples/record/record-namespaced.yaml: a different resource's manifest,
// which may well exist, so the hook would pass while testing the wrong thing.
// Derive therefore prefers the literal reading whenever examples/<slug>/
// exists as a directory, and only falls back to the shorthand when it does
// not. The "-namespaced" suffix needs no such guard: no resource is named
// with it.
//
// # Why the sibling-variant fallback is guarded on the primary being missing
//
// One resource directory may hold more than one example pair for the same
// kind — an alternate variant such as examples/network/network-v6.yaml sitting
// beside examples/network/network.yaml, each with its own post-assert symlink.
// The variant's slug ("network-v6") names no directory of its own, so the
// primary derivation misses. The fallback strips the slug's trailing
// hyphenated segments, one at a time, and looks for the same leaf filename
// inside each progressively shorter directory — stopping at the first match.
//
// One strip is enough for a variant like "network-v6" (directory "network"),
// but a variant's own scope suffix can stack more than one segment away from
// its directory: "record-a-namespaced-pc" (a ProviderConfig-scoped namespaced
// variant of "record-a") resolves only after stripping BOTH "-pc" and
// "-namespaced", landing on directory "record-a". The loop tries the nearest
// candidate first, so a closer match always wins over a more distant one even
// when both would resolve — the earlier one-strip behavior is the loop's
// first iteration, unchanged.
//
// The fallback runs only when the primary path does not exist. That guard is
// what keeps it safe: every slug that already resolves directly keeps its
// current target, so the fallback can never reinterpret a working symlink. An
// unguarded version would be ambiguous for any multi-word resource — "foo-bar"
// would have two plausible readings (examples/foo-bar/foo-bar.yaml and
// examples/foo/foo-bar.yaml) with no way to tell which the author meant.
func Derive(root, invocationName string) (string, error) {
	slug := Slug(invocationName)

	resource, primary := primaryPath(root, slug)
	if fileExists(primary) {
		return primary, nil
	}

	tried := []string{primary}
	leaf := filepath.Base(primary)

	// Sibling-variant fallback: same leaf filename, progressively shorter
	// directories formed by stripping one trailing hyphenated segment at a
	// time until a match is found or no segment is left to strip.
	for {
		idx := strings.LastIndex(resource, "-")
		if idx <= 0 {
			break
		}
		resource = resource[:idx]
		alt := filepath.Join(root, examplesDir, resource, leaf)
		if fileExists(alt) {
			return alt, nil
		}
		tried = append(tried, alt)
	}

	return "", fmt.Errorf(
		"manifest not found for invocation %q (slug=%q): tried %s",
		invocationName, slug, strings.Join(quoteAll(tried), ", "))
}

// Resolve returns the manifest path to actually use: the caller-supplied
// override when it is non-empty, otherwise the path derived from
// invocationName by Derive.
//
// The override exists for direct invocation while debugging, where the caller
// names a manifest outside the naming convention entirely. It bypasses all
// derivation, but is still checked for existence so that a typo in the
// override surfaces here — with the path in the message — instead of as an
// obscure failure further down the run.
func Resolve(root, invocationName, override string) (string, error) {
	if override != "" {
		if !fileExists(override) {
			return "", fmt.Errorf("manifest not found: %q (supplied explicitly, no derivation attempted)", override)
		}
		return override, nil
	}
	return Derive(root, invocationName)
}

// Slug strips the directory, the ".sh" extension and the "post-assert-" prefix
// from an invocation name, leaving the resource slug (including any trailing
// scope suffix). An invocation name without the prefix yields the base name
// unchanged, which then fails to resolve with a diagnostic naming it — better
// than silently deriving from a truncated slug.
func Slug(invocationName string) string {
	base := filepath.Base(invocationName)
	base = strings.TrimSuffix(base, ".sh")
	return strings.TrimPrefix(base, invocationPrefix)
}

// primaryPath applies the naming convention to a slug, returning the resource
// name the slug denotes and the manifest path derived from it, before any
// sibling-variant fallback. The -ns branch consults the filesystem; see
// Derive's doc comment for why.
func primaryPath(root, slug string) (resource, path string) {
	switch {
	case strings.HasSuffix(slug, "-namespaced"):
		resource = strings.TrimSuffix(slug, "-namespaced")
		return resource, manifestPath(root, resource, resource+"-namespaced.yaml")

	case strings.HasSuffix(slug, "-ns"):
		// A resource legitimately named "<something>-ns" wins over the "-ns"
		// shorthand for "-namespaced" whenever it has a directory of its own.
		if dirExists(filepath.Join(root, examplesDir, slug)) {
			return slug, manifestPath(root, slug, slug+".yaml")
		}
		resource = strings.TrimSuffix(slug, "-ns")
		return resource, manifestPath(root, resource, resource+"-namespaced.yaml")

	default:
		return slug, manifestPath(root, slug, slug+".yaml")
	}
}

// manifestPath joins the conventional examples/<resource>/<leaf> location.
func manifestPath(root, resource, leaf string) string {
	return filepath.Join(root, examplesDir, resource, leaf)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func quoteAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = fmt.Sprintf("%q", p)
	}
	return out
}
