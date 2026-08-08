package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTree materialises a provider repo layout under a fresh temp directory:
// every entry in dirs is created as an empty directory, every entry in files as
// an empty regular file (with its parents). Paths are slash-separated and
// relative to the root.
//
// The two fallbacks in Derive are driven by filesystem existence checks, so the
// tests exercise them against a real tree rather than a stubbed filesystem —
// otherwise the stub, not the code, would define the behaviour under test.
func buildTree(t *testing.T, dirs, files []string) string {
	t.Helper()
	root := t.TempDir()

	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", f, err)
		}
		if err := os.WriteFile(p, []byte("# example manifest\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return root
}

func TestDerive(t *testing.T) {
	cases := []struct {
		name string
		// dirs are created empty; files are created with their parents.
		dirs  []string
		files []string
		// invocation is the name the hook was invoked under.
		invocation string
		// want is the expected result, relative to the temp root.
		want string
	}{
		// ── Ported from run-update-tester_test.sh ────────────────────────
		{
			// TestDeriveManifestPathPlain
			name:       "PlainSlug",
			files:      []string{"examples/compute-instance/compute-instance.yaml"},
			invocation: "post-assert-compute-instance.sh",
			want:       "examples/compute-instance/compute-instance.yaml",
		},
		{
			// TestDeriveManifestPathNamespacedSuffix
			name:       "NamespacedSuffix",
			files:      []string{"examples/compute-instance/compute-instance-namespaced.yaml"},
			invocation: "post-assert-compute-instance-namespaced.sh",
			want:       "examples/compute-instance/compute-instance-namespaced.yaml",
		},
		{
			// TestDeriveManifestPathNsSuffix
			name:       "NsSuffixShorthand",
			files:      []string{"examples/compute-instance/compute-instance-namespaced.yaml"},
			invocation: "post-assert-compute-instance-ns.sh",
			want:       "examples/compute-instance/compute-instance-namespaced.yaml",
		},
		{
			// TestDeriveManifestPathMultiWordResource (first assertion).
			// Resource slugs may themselves contain hyphens; only a trailing
			// -namespaced/-ns is treated as a scope suffix.
			name:       "MultiWordResource",
			files:      []string{"examples/foo-bar-baz/foo-bar-baz.yaml"},
			invocation: "post-assert-foo-bar-baz.sh",
			want:       "examples/foo-bar-baz/foo-bar-baz.yaml",
		},
		{
			// TestDeriveManifestPathMultiWordResource (second assertion).
			name:       "MultiWordResourceNamespaced",
			files:      []string{"examples/foo-bar-baz/foo-bar-baz-namespaced.yaml"},
			invocation: "post-assert-foo-bar-baz-namespaced.sh",
			want:       "examples/foo-bar-baz/foo-bar-baz-namespaced.yaml",
		},

		// ── -ns collision guard ──────────────────────────────────────────
		{
			// A resource legitimately ending in "-ns" owns its directory, so
			// the literal reading wins over the "-namespaced" shorthand.
			name:       "NsSuffixResourceDirectoryWins",
			files:      []string{"examples/record-ns/record-ns.yaml"},
			invocation: "post-assert-record-ns.sh",
			want:       "examples/record-ns/record-ns.yaml",
		},
		{
			// Both readings resolve to an existing file: the literal one must
			// still win, otherwise the hook passes while testing a different
			// resource's manifest.
			name: "NsSuffixResourceDirectoryWinsOverShorthand",
			files: []string{
				"examples/record-ns/record-ns.yaml",
				"examples/record/record-namespaced.yaml",
			},
			invocation: "post-assert-record-ns.sh",
			want:       "examples/record-ns/record-ns.yaml",
		},
		{
			// No directory named after the full slug: the shorthand applies.
			name:       "NsSuffixShorthandWhenNoResourceDirectory",
			files:      []string{"examples/record/record-namespaced.yaml"},
			invocation: "post-assert-record-ns.sh",
			want:       "examples/record/record-namespaced.yaml",
		},
		{
			// The guard tests for a *directory*; a stray file of that name
			// must not be mistaken for one.
			name: "NsSuffixFileNamedLikeResourceDirectoryIgnored",
			files: []string{
				"examples/record-ns",
				"examples/record/record-namespaced.yaml",
			},
			invocation: "post-assert-record-ns.sh",
			want:       "examples/record/record-namespaced.yaml",
		},

		// ── Sibling-variant fallback ─────────────────────────────────────
		{
			// An alternate variant living beside the base manifest: the
			// variant slug names no directory of its own, so the primary
			// derivation misses and the last hyphenated segment is stripped.
			name: "SiblingVariantFallback",
			files: []string{
				"examples/network/network.yaml",
				"examples/network/network-v6.yaml",
			},
			invocation: "post-assert-network-v6.sh",
			want:       "examples/network/network-v6.yaml",
		},
		{
			// The fallback keeps the leaf filename intact, including a
			// -namespaced scope suffix.
			name: "SiblingVariantFallbackNamespaced",
			files: []string{
				"examples/network/network-namespaced.yaml",
				"examples/network/network-v6-namespaced.yaml",
			},
			invocation: "post-assert-network-v6-namespaced.sh",
			want:       "examples/network/network-v6-namespaced.yaml",
		},
		{
			// Guard: the fallback engages only when the primary path is
			// missing, so a slug that already resolves directly keeps its
			// target even when a sibling candidate also exists.
			name: "SiblingVariantFallbackNotUsedWhenPrimaryExists",
			files: []string{
				"examples/network-v6/network-v6.yaml",
				"examples/network/network-v6.yaml",
			},
			invocation: "post-assert-network-v6.sh",
			want:       "examples/network-v6/network-v6.yaml",
		},
		{
			// The fallback strips the last segment first, and stops there
			// when that already resolves: a three-word slug falls back to
			// its two-word parent directory rather than continuing further.
			name: "SiblingVariantFallbackStripsLastSegmentOnly",
			files: []string{
				"examples/foo-bar/foo-bar-baz.yaml",
				"examples/foo/foo-bar-baz.yaml",
			},
			invocation: "post-assert-foo-bar-baz.sh",
			want:       "examples/foo-bar/foo-bar-baz.yaml",
		},
		{
			// Two segments deep: a ProviderConfig-scoped namespaced variant
			// stacks "-namespaced" and "-pc" past its resource directory.
			// Neither single-strip candidate exists, so the loop must try a
			// second strip rather than stopping after the first miss.
			name:       "SiblingVariantFallbackMultiLevel",
			files:      []string{"examples/record-a/record-a-namespaced-pc.yaml"},
			invocation: "post-assert-record-a-namespaced-pc.sh",
			want:       "examples/record-a/record-a-namespaced-pc.yaml",
		},
		{
			// When more than one level would resolve, the nearer directory
			// wins — same guarantee as the single-strip case, generalized.
			name: "SiblingVariantFallbackNearestLevelWinsOverFurther",
			files: []string{
				"examples/foo-bar/foo-bar-baz-qux.yaml",
				"examples/foo/foo-bar-baz-qux.yaml",
			},
			invocation: "post-assert-foo-bar-baz-qux.sh",
			want:       "examples/foo-bar/foo-bar-baz-qux.yaml",
		},
		{
			// The -ns guard runs first: the slug's own directory exists but is
			// empty, so the primary misses and the fallback then looks for the
			// full slug's leaf inside the shorter directory.
			name:       "NsResourceDirectoryEmptyFallsBackToSibling",
			dirs:       []string{"examples/record-ns"},
			files:      []string{"examples/record/record-ns.yaml"},
			invocation: "post-assert-record-ns.sh",
			want:       "examples/record/record-ns.yaml",
		},

		// ── Invocation-name handling ─────────────────────────────────────
		{
			// $0 is typically a path, not a bare name; only the base matters.
			name:       "InvocationNameWithDirectoryComponents",
			files:      []string{"examples/compute-instance/compute-instance.yaml"},
			invocation: "test/hooks/post-assert-compute-instance.sh",
			want:       "examples/compute-instance/compute-instance.yaml",
		},
		{
			name:       "InvocationNameWithoutShExtension",
			files:      []string{"examples/compute-instance/compute-instance.yaml"},
			invocation: "post-assert-compute-instance",
			want:       "examples/compute-instance/compute-instance.yaml",
		},
		{
			name:       "InvocationNameAbsolutePath",
			files:      []string{"examples/compute-instance/compute-instance.yaml"},
			invocation: "/repo/test/hooks/post-assert-compute-instance.sh",
			want:       "examples/compute-instance/compute-instance.yaml",
		},
		{
			// A single-word slug that happens to end in "ns" is not an "-ns"
			// suffix — the suffix is hyphen-delimited.
			name:       "SlugEndingInNsWithoutHyphen",
			files:      []string{"examples/dns/dns.yaml"},
			invocation: "post-assert-dns.sh",
			want:       "examples/dns/dns.yaml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := buildTree(t, tc.dirs, tc.files)

			got, err := Derive(root, tc.invocation)
			if err != nil {
				t.Fatalf("Derive(%q) returned error: %v", tc.invocation, err)
			}
			want := filepath.Join(root, filepath.FromSlash(tc.want))
			if got != want {
				t.Errorf("Derive(%q) = %q, want %q", tc.invocation, got, want)
			}
		})
	}
}

func TestDeriveErrors(t *testing.T) {
	cases := []struct {
		name       string
		dirs       []string
		files      []string
		invocation string
		// wantTried are paths (relative to root) the error must name.
		wantTried []string
		// wantNotTried are paths the error must NOT name.
		wantNotTried []string
		// wantSlug is the slug the error must report.
		wantSlug string
	}{
		{
			name:       "NothingResolves",
			invocation: "post-assert-compute-instance.sh",
			wantSlug:   "compute-instance",
			wantTried: []string{
				"examples/compute-instance/compute-instance.yaml",
				"examples/compute/compute-instance.yaml",
			},
		},
		{
			// A single-segment slug has no hyphen to strip, so the fallback
			// never applies and only one path is reported.
			name:         "SingleSegmentSlugHasNoFallback",
			invocation:   "post-assert-network.sh",
			wantSlug:     "network",
			wantTried:    []string{"examples/network/network.yaml"},
			wantNotTried: []string{"examples//network.yaml"},
		},
		{
			// The shorthand rewrote the slug to a single-segment resource, so
			// there is nothing left to strip and only that path is reported.
			name:       "NsShorthandNothingResolves",
			invocation: "post-assert-record-ns.sh",
			wantSlug:   "record-ns",
			wantTried:  []string{"examples/record/record-namespaced.yaml"},
		},
		{
			// Both the shorthand target and its sibling fallback are reported
			// so an operator can see exactly which spellings were attempted.
			// The fallback strips from the resource AFTER the shorthand was
			// applied, so it is "alpha-beta" that loses its last segment.
			name:       "NsShorthandMultiWordReportsBothCandidates",
			invocation: "post-assert-alpha-beta-ns.sh",
			wantSlug:   "alpha-beta-ns",
			wantTried: []string{
				"examples/alpha-beta/alpha-beta-namespaced.yaml",
				"examples/alpha/alpha-beta-namespaced.yaml",
			},
		},
		{
			// The directory exists but holds no matching manifest: the error
			// must still name both candidates rather than reporting success.
			name:       "ResourceDirectoryWithoutManifest",
			dirs:       []string{"examples/network-v6"},
			invocation: "post-assert-network-v6.sh",
			wantSlug:   "network-v6",
			wantTried: []string{
				"examples/network-v6/network-v6.yaml",
				"examples/network/network-v6.yaml",
			},
		},
		{
			// Every level the multi-strip fallback tried is named, not just
			// the first — an operator debugging a three-word slug needs to
			// see all the directories considered.
			name:       "MultiLevelFallbackReportsEveryCandidate",
			invocation: "post-assert-alpha-beta-gamma.sh",
			wantSlug:   "alpha-beta-gamma",
			wantTried: []string{
				"examples/alpha-beta-gamma/alpha-beta-gamma.yaml",
				"examples/alpha-beta/alpha-beta-gamma.yaml",
				"examples/alpha/alpha-beta-gamma.yaml",
			},
		},
		{
			// A manifest path is a regular file; a directory of that name must
			// not satisfy the check.
			name:       "DirectoryNamedLikeManifestIsNotAMatch",
			dirs:       []string{"examples/network/network.yaml"},
			invocation: "post-assert-network.sh",
			wantSlug:   "network",
			wantTried:  []string{"examples/network/network.yaml"},
		},
		{
			// An invocation name without the post-assert- prefix keeps its
			// base name as the slug and fails loudly, naming it.
			name:       "InvocationWithoutExpectedPrefix",
			invocation: "some-other-hook.sh",
			wantSlug:   "some-other-hook",
			wantTried:  []string{"examples/some-other-hook/some-other-hook.yaml"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := buildTree(t, tc.dirs, tc.files)

			got, err := Derive(root, tc.invocation)
			if err == nil {
				t.Fatalf("Derive(%q) = %q, want error", tc.invocation, got)
			}
			msg := err.Error()

			if !strings.Contains(msg, tc.invocation) {
				t.Errorf("error does not name the invocation %q: %s", tc.invocation, msg)
			}
			if !strings.Contains(msg, tc.wantSlug) {
				t.Errorf("error does not name the slug %q: %s", tc.wantSlug, msg)
			}
			for _, p := range tc.wantTried {
				full := filepath.Join(root, filepath.FromSlash(p))
				if !strings.Contains(msg, full) {
					t.Errorf("error does not name tried path %q: %s", full, msg)
				}
			}
			for _, p := range tc.wantNotTried {
				full := filepath.Join(root, filepath.FromSlash(p))
				if strings.Contains(msg, full) {
					t.Errorf("error names path %q that should not have been tried: %s", full, msg)
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Run("OverrideTakesPrecedenceOverDerivation", func(t *testing.T) {
		root := buildTree(t, nil, []string{
			"examples/compute-instance/compute-instance.yaml",
			"custom/path/manifest.yaml",
		})
		override := filepath.Join(root, "custom", "path", "manifest.yaml")

		got, err := Resolve(root, "post-assert-compute-instance.sh", override)
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		if got != override {
			t.Errorf("Resolve = %q, want the override %q", got, override)
		}
	})

	t.Run("OverrideWinsEvenWhenDerivationWouldFail", func(t *testing.T) {
		root := buildTree(t, nil, []string{"custom/path/manifest.yaml"})
		override := filepath.Join(root, "custom", "path", "manifest.yaml")

		got, err := Resolve(root, "post-assert-does-not-exist.sh", override)
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		if got != override {
			t.Errorf("Resolve = %q, want the override %q", got, override)
		}
	})

	t.Run("EmptyOverrideFallsBackToDerivation", func(t *testing.T) {
		root := buildTree(t, nil, []string{"examples/compute-instance/compute-instance.yaml"})

		got, err := Resolve(root, "post-assert-compute-instance.sh", "")
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		want := filepath.Join(root, "examples", "compute-instance", "compute-instance.yaml")
		if got != want {
			t.Errorf("Resolve = %q, want %q", got, want)
		}
	})

	t.Run("EmptyOverrideFallsBackToNamespacedDerivation", func(t *testing.T) {
		root := buildTree(t, nil, []string{"examples/compute-instance/compute-instance-namespaced.yaml"})

		got, err := Resolve(root, "post-assert-compute-instance-namespaced.sh", "")
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		want := filepath.Join(root, "examples", "compute-instance", "compute-instance-namespaced.yaml")
		if got != want {
			t.Errorf("Resolve = %q, want %q", got, want)
		}
	})

	t.Run("MissingOverrideReportsThePathSupplied", func(t *testing.T) {
		root := buildTree(t, nil, []string{"examples/compute-instance/compute-instance.yaml"})
		override := filepath.Join(root, "custom", "path", "missing.yaml")

		got, err := Resolve(root, "post-assert-compute-instance.sh", override)
		if err == nil {
			t.Fatalf("Resolve = %q, want error for a missing override", got)
		}
		if !strings.Contains(err.Error(), override) {
			t.Errorf("error does not name the override %q: %s", override, err)
		}
	})
}

func TestSlug(t *testing.T) {
	cases := []struct {
		name       string
		invocation string
		want       string
	}{
		{"BareName", "post-assert-network", "network"},
		{"WithShExtension", "post-assert-network.sh", "network"},
		{"WithRelativePath", "test/hooks/post-assert-network.sh", "network"},
		{"WithAbsolutePath", "/repo/test/hooks/post-assert-network-ns.sh", "network-ns"},
		{"ScopeSuffixRetained", "post-assert-network-namespaced.sh", "network-namespaced"},
		{"MultiWordSlug", "post-assert-foo-bar-baz.sh", "foo-bar-baz"},
		{"WithoutPrefixReturnsBaseName", "some-other-hook.sh", "some-other-hook"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slug(tc.invocation); got != tc.want {
				t.Errorf("Slug(%q) = %q, want %q", tc.invocation, got, tc.want)
			}
		})
	}
}
