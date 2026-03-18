package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoaderLoadWithFrontMatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
server:
  port: 8080
---
Hello {{ issue.identifier }}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	definition, err := NewLoader().Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if definition.SourcePath != path {
		t.Fatalf("SourcePath = %q, want %q", definition.SourcePath, path)
	}
	if definition.PromptTemplate != "Hello {{ issue.identifier }}" {
		t.Fatalf("PromptTemplate = %q", definition.PromptTemplate)
	}
	tracker, ok := definition.Config["tracker"].(map[string]any)
	if !ok {
		t.Fatalf("tracker config missing or wrong type: %#v", definition.Config["tracker"])
	}
	if tracker["kind"] != "linear" {
		t.Fatalf("tracker.kind = %#v", tracker["kind"])
	}
}

func TestLoaderMissingFileReturnsTypedError(t *testing.T) {
	t.Parallel()

	_, err := NewLoader().Load(filepath.Join(t.TempDir(), "missing.md"))
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}

	var workflowErr *Error
	if !errors.As(err, &workflowErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if workflowErr.Code != ErrMissingWorkflowFile {
		t.Fatalf("error code = %q, want %q", workflowErr.Code, ErrMissingWorkflowFile)
	}
}

func TestLoaderRejectsNonMapFrontMatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	content := `---
- one
- two
---
body
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewLoader().Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}

	var workflowErr *Error
	if !errors.As(err, &workflowErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if workflowErr.Code != ErrFrontMatterNotAMap {
		t.Fatalf("error code = %q, want %q", workflowErr.Code, ErrFrontMatterNotAMap)
	}
}
