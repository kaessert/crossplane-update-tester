// Package manifest parses Crossplane example manifests and the update-test
// annotations that drive the tester's checks.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AnnotationKey names the manifest annotation carrying the per-field update
// test list. See ParseAnnotation for the format.
const AnnotationKey = "crossplane.io/update-test"

// ExpectExternalNamePrefixKey names the manifest annotation that declares the
// required prefix of the live resource's crossplane.io/external-name
// annotation.
//
// It exists for resources whose backend models more than one object type
// behind a single Kubernetes kind, where an identity search issued against
// the wrong object type returns zero matches and the reconciler silently
// creates a duplicate while still reporting Ready — a failure invisible to a
// plain Ready assertion. Optional: manifests that do not need this guard omit
// it, and the checks gated on it are skipped.
const ExpectExternalNamePrefixKey = "crossplane.io/expect-external-name-prefix"

// UpdateTest represents a single field update test parsed from the annotation.
type UpdateTest struct {
	Field  string      `yaml:"field"`
	Value  interface{} `yaml:"value"`
	Expect interface{} `yaml:"expect"`
	Skip   string      `yaml:"skip"`
}

// Manifest holds the parsed Kubernetes manifest metadata needed for testing.
type Manifest struct {
	APIVersion   string
	Kind         string
	Name         string
	Namespace    string
	Tests        []UpdateTest
	ConvergeSkip string
	// ExpectExternalNamePrefix is the value of the
	// crossplane.io/expect-external-name-prefix annotation, when present.
	// Empty means the manifest declares no external-name-prefix
	// expectation — see ExpectExternalNamePrefixKey.
	ExpectExternalNamePrefix string
	// AssertUnchanged lists dot-separated status.atProvider field paths that
	// must hold the SAME value for the entire duration of a `run` (the
	// per-field update tests), regardless of which other field is being
	// patched. It is populated from the "assert-unchanged:" directive line
	// in the crossplane.io/update-test annotation — see ParseAnnotation.
	//
	// This exists for a backend that silently defaults an omitted field on
	// every write: a PUT that patches one unrelated field can still cause
	// the backend to reset a field the request never mentioned, and that
	// reset returns the same 200 a genuine update would. A value-only
	// assertion on the field being patched cannot see this, because the
	// field it corrupts is never the one under test. Declaring the
	// vulnerable field here makes the runner check it after every patch in
	// the run and fail the run the moment it moves — see runner.Runner.RunTests.
	AssertUnchanged []string
	// ForProvider is the manifest's own spec.forProvider, decoded as plain
	// JSON-shaped data (map[string]interface{}, []interface{}, and scalars —
	// gopkg.in/yaml.v3 already decodes mapping nodes as map[string]interface{}
	// with string keys, so no further normalisation is needed). It is the
	// create-time value the runner's first Patch() call merges against, and
	// exists so an offline check can simulate that RFC 7386 merge without a
	// live cluster — see validator.CheckMergePatchSiblings. Nil when the
	// manifest has no spec.forProvider at all.
	ForProvider map[string]interface{}
}

// manifestDoc is the intermediate YAML structure for parsing.
type manifestDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		ForProvider map[string]interface{} `yaml:"forProvider"`
	} `yaml:"spec"`
}

// Parse reads a YAML manifest file and extracts metadata and update test
// annotations.
func Parse(path string) (*Manifest, error) {
	// #nosec G304 -- path is an operator-supplied CLI argument (the
	// manifest file to test), not attacker-controlled input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses manifest YAML bytes.
//
// The input may be a multi-document ("---"-separated) YAML stream: Crossplane
// example manifests sometimes ship a companion object (a Secret, a
// ProviderConfig) in the same file as the managed resource under test. Every
// document is decoded and the one carrying the AnnotationKey annotation is
// selected, because the companion document is frequently written first and
// also has a non-empty apiVersion/kind/metadata.name — decoding only the
// leading document would silently test the wrong object and report zero
// update tests.
//
// When no document carries the annotation the first valid document wins,
// which keeps single-document manifests (and multi-document ones that declare
// no update tests) behaving exactly as they did before multi-document support
// existed.
func ParseBytes(data []byte) (*Manifest, error) {
	docs, err := decodeManifestDocs(data)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("manifest missing apiVersion or kind")
	}

	selected := docs[0]
	for _, d := range docs {
		if _, ok := d.Metadata.Annotations[AnnotationKey]; ok {
			selected = d
			break
		}
	}

	return manifestFromDoc(selected)
}

// decodeManifestDocs splits a (possibly multi-document) YAML byte stream and
// decodes each document into a manifestDoc, skipping documents that do not
// look like a Kubernetes object — either blank (a trailing "---" separator
// yields an empty document, which is legal YAML but has nothing to test) or
// missing apiVersion/kind. Skipping rather than erroring is what lets the
// caller report the original "manifest missing apiVersion or kind" for a
// stream that contains no usable document at all.
func decodeManifestDocs(data []byte) ([]manifestDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []manifestDoc
	for {
		var doc manifestDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing manifest YAML: %w", err)
		}
		if doc.APIVersion == "" || doc.Kind == "" {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// manifestFromDoc converts a single decoded manifestDoc into a Manifest,
// parsing its update-test annotation (if present).
func manifestFromDoc(doc manifestDoc) (*Manifest, error) {
	if doc.Metadata.Name == "" {
		return nil, fmt.Errorf("manifest missing metadata.name")
	}

	m := &Manifest{
		APIVersion:  doc.APIVersion,
		Kind:        doc.Kind,
		Name:        doc.Metadata.Name,
		Namespace:   doc.Metadata.Namespace,
		ForProvider: doc.Spec.ForProvider,
	}

	m.ExpectExternalNamePrefix = doc.Metadata.Annotations[ExpectExternalNamePrefixKey]

	annotation, ok := doc.Metadata.Annotations[AnnotationKey]
	if !ok {
		return m, nil
	}

	tests, convergeSkip, assertUnchanged, err := ParseAnnotation(annotation)
	if err != nil {
		return nil, fmt.Errorf("parsing %s annotation: %w", AnnotationKey, err)
	}
	m.Tests = tests
	m.ConvergeSkip = convergeSkip
	m.AssertUnchanged = assertUnchanged
	return m, nil
}

// ParseAnnotation parses the update-test annotation YAML string into a slice
// of UpdateTest entries, plus two optional top-level directives:
// "converge-skip" (a reason string) and "assert-unchanged" (a field-path
// list).
//
// The annotation format allows both directives to appear, each on its own
// unindented line, alongside the list of field entries:
//
//	crossplane.io/update-test: |
//	  converge-skip: "atProvider.lastSyncTime changes every observe cycle"
//	  assert-unchanged: legacyRuleList
//	  - field: name
//	    value: "updated"
//
// Neither is valid as a single YAML document (a mapping key cannot be a
// sibling of top-level sequence items), so both directive lines are
// extracted first and the remainder is parsed as a plain YAML sequence.
//
// "assert-unchanged" takes a comma-separated list of dot-separated
// status.atProvider field paths — see Manifest.AssertUnchanged for what the
// runner does with them. A field named there may not also appear as an
// update-test entry's own field: patching a field and asserting it never
// changes are contradictory requests, so that combination is a parse error
// rather than a runtime race between the two.
func ParseAnnotation(annotation string) ([]UpdateTest, string, []string, error) {
	rest, convergeSkip, assertUnchanged, err := extractDirectives(annotation)
	if err != nil {
		return nil, "", nil, fmt.Errorf("parsing directives: %w", err)
	}

	rest = strings.TrimSpace(rest)
	var tests []UpdateTest
	if rest != "" {
		if err := yaml.Unmarshal([]byte(rest), &tests); err != nil {
			return nil, "", nil, fmt.Errorf("unmarshalling annotation: %w", err)
		}
	}

	testedFields := make(map[string]bool, len(tests))
	for i, t := range tests {
		if t.Field == "" {
			return nil, "", nil, fmt.Errorf("entry %d: field is required", i)
		}
		if t.Value == nil && t.Skip == "" {
			return nil, "", nil, fmt.Errorf("entry %d (%s): value is required unless skip is set", i, t.Field)
		}
		testedFields[t.Field] = true
	}
	for _, f := range assertUnchanged {
		if testedFields[f] {
			return nil, "", nil, fmt.Errorf(
				"assert-unchanged field %q is also an update-test field; a field cannot be both patched and asserted unchanged in the same run", f)
		}
	}
	return tests, convergeSkip, assertUnchanged, nil
}

// extractDirectives scans the annotation text line by line for the two
// top-level (unindented) directive lines — "converge-skip:" and
// "assert-unchanged:" — removes them from the text, and returns the
// remaining text plus what each directive carries (empty/nil when absent).
//
// Both are extracted the same way and for the same reason: neither is valid
// as a sibling of the top-level sequence of field entries in a single YAML
// document, so each is pulled out of the raw text before the remainder is
// parsed as a plain YAML sequence.
func extractDirectives(annotation string) (rest string, convergeSkip string, assertUnchanged []string, err error) {
	lines := strings.Split(annotation, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		switch {
		case indent == 0 && strings.HasPrefix(trimmed, "converge-skip:"):
			var single map[string]string
			if uerr := yaml.Unmarshal([]byte(line), &single); uerr != nil {
				return "", "", nil, fmt.Errorf("parsing converge-skip line %q: %w", line, uerr)
			}
			convergeSkip = single["converge-skip"]
		case indent == 0 && strings.HasPrefix(trimmed, "assert-unchanged:"):
			var single map[string]string
			if uerr := yaml.Unmarshal([]byte(line), &single); uerr != nil {
				return "", "", nil, fmt.Errorf("parsing assert-unchanged line %q: %w", line, uerr)
			}
			assertUnchanged = splitFieldList(single["assert-unchanged"])
		default:
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n"), convergeSkip, assertUnchanged, nil
}

// splitFieldList splits a comma-separated field-path list, trimming
// surrounding whitespace from each entry and dropping empty ones. Returns
// nil for an empty or whitespace-only input, matching the "absent" state
// callers already treat len(...) == 0 as.
func splitFieldList(raw string) []string {
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
