package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifestFile writes content to root/relPath, creating any missing
// parent directories — the on-disk counterpart to ParseBytes's in-memory
// tests, needed here because sidecar.Load only ever looks beside a real
// file path.
func writeManifestFile(t *testing.T, root, relPath, content string) string {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating parent dirs for %s: %v", relPath, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
	return path
}

// TestParseMergesSidecarBeforeDocumentSelection is the proof convention
// requires: a two-document manifest whose Secret is written FIRST and
// whose managed resource carries its update-test annotation only in the
// sidecar. Parse must select the managed resource, not the Secret —
// selection has to see the sidecar-declared annotation, which means the
// merge ran before selection, not after.
func TestParseMergesSidecarBeforeDocumentSelection(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeManifestFile(t, root, "widget.yaml", `apiVersion: v1
kind: Secret
metadata:
  name: widget-creds
---
apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
`)
	writeManifestFile(t, root, "widget.yaml.uptest", `for: widget.example.crossplane.io/v1alpha1/Widget
crossplane.io/update-test: |
  - field: name
    value: renamed-widget
`)

	m, err := Parse(manifestPath)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if m.Kind != "Widget" || m.Name != "example-widget" {
		t.Fatalf("Parse() selected %s %q, want the Widget (the Secret is written first in the file "+
			"and must not be selected merely because the sidecar merge ran too late to matter)", m.Kind, m.Name)
	}
	if len(m.Tests) != 1 || m.Tests[0].Field != "name" {
		t.Fatalf("Parse() Tests = %+v, want the single sidecar-declared update-test entry", m.Tests)
	}
}

// TestParseNoSidecarBehavesAsBefore confirms a manifest with no sidecar
// file parses exactly as it always has — the un-migrated state every
// example is in until it migrates.
func TestParseNoSidecarBehavesAsBefore(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeManifestFile(t, root, "widget.yaml", `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
  annotations:
    crossplane.io/update-test: |
      - field: name
        value: renamed-widget
`)

	m, err := Parse(manifestPath)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(m.Tests) != 1 || m.Tests[0].Field != "name" {
		t.Fatalf("Parse() Tests = %+v, want the single inline update-test entry", m.Tests)
	}
}

// TestParseRejectsInlineOwnedKeyWhenSidecarExists is the switch-not-overlay
// guard: once a sidecar exists beside a manifest, that manifest may not
// also carry any of this tool's own annotation keys inline — a key live in
// both files has no defensible precedence.
func TestParseRejectsInlineOwnedKeyWhenSidecarExists(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeManifestFile(t, root, "widget.yaml", `apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
  annotations:
    crossplane.io/update-test: |
      - field: name
        value: inline-value
`)
	writeManifestFile(t, root, "widget.yaml.uptest", `for: widget.example.crossplane.io/v1alpha1/Widget
uptest.upbound.io/timeout: "600"
`)

	_, err := Parse(manifestPath)
	if err == nil {
		t.Fatalf("Parse() error = nil, want a rejection: the manifest carries crossplane.io/update-test " +
			"inline while a sidecar exists beside it")
	}
	if !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("Parse() error = %q, want it to name the sidecar conflict", err.Error())
	}
}

// TestParseSidecarMultipleTargets covers a sidecar declaring one document
// per object in a multi-document manifest, confirming each set of
// annotations lands on the correct object rather than all merging onto
// the first or the last.
func TestParseSidecarMultipleTargets(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeManifestFile(t, root, "widget.yaml", `apiVersion: v1
kind: Secret
metadata:
  name: widget-creds
---
apiVersion: widget.example.crossplane.io/v1alpha1
kind: Widget
metadata:
  name: example-widget
`)
	writeManifestFile(t, root, "widget.yaml.uptest", `for: v1/Secret
uptest.upbound.io/timeout: "60"
---
for: widget.example.crossplane.io/v1alpha1/Widget
crossplane.io/update-test: |
  - field: name
    value: renamed-widget
crossplane.io/expect-external-name-prefix: "widget/"
`)

	m, err := Parse(manifestPath)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if m.Kind != "Widget" {
		t.Fatalf("Parse() selected kind %q, want Widget (the only document the sidecar gave an update-test to)", m.Kind)
	}
	if m.ExpectExternalNamePrefix != "widget/" {
		t.Fatalf("Parse() ExpectExternalNamePrefix = %q, want %q", m.ExpectExternalNamePrefix, "widget/")
	}
}
