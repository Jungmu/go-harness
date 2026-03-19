package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

func TestManagerPrepareAfterRunAndCleanup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	manager := NewManager(root, "", config.HooksConfig{
		AfterCreate: `printf created > created.txt`,
		BeforeRun:   `printf before > before.txt`,
		AfterRun:    `printf after > after.txt`,
		Timeout:     time.Second,
	}, logger)

	workspace, err := manager.Prepare(context.Background(), domain.Issue{Identifier: "ABC/123"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !workspace.CreatedNow {
		t.Fatalf("CreatedNow = false, want true")
	}
	if filepath.Base(workspace.Path) != "ABC_123" {
		t.Fatalf("workspace path = %q", workspace.Path)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "created.txt")); err != nil {
		t.Fatalf("created.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "before.txt")); err != nil {
		t.Fatalf("before.txt missing: %v", err)
	}

	if err := manager.AfterRun(context.Background(), workspace); err != nil {
		t.Fatalf("AfterRun() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "after.txt")); err != nil {
		t.Fatalf("after.txt missing: %v", err)
	}

	if err := manager.Cleanup(context.Background(), workspace); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after cleanup: %v", err)
	}
}

func TestManagerCleanupRejectsSymlinkWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := t.TempDir()
	linkPath := filepath.Join(root, "ABC-1")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	manager := NewManager(root, "", config.HooksConfig{Timeout: time.Second}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	err := manager.Cleanup(context.Background(), domain.Workspace{Path: linkPath, WorkspaceKey: "ABC-1"})
	if err == nil {
		t.Fatal("Cleanup() error = nil, want symlink rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Cleanup() error = %v, want symlink error", err)
	}
}

func TestManagerInjectsWorkflowHookEnv(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("Mkdir(.git) error = %v", err)
	}
	workflowDir := filepath.Join(repoRoot, "bin")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workflowDir) error = %v", err)
	}
	workflowPath := filepath.Join(workflowDir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\n---\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(workflowPath) error = %v", err)
	}

	root := t.TempDir()
	manager := NewManager(root, workflowPath, config.HooksConfig{
		AfterCreate: `printf '%s\n%s\n%s\n%s' "$HARNESS_SOURCE_REPO" "$GO_HARNESS_SOURCE_REPO" "$HARNESS_WORKFLOW_PATH" "$HARNESS_WORKFLOW_DIR" > hook-env.txt`,
		Timeout:     time.Second,
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	workspace, err := manager.Prepare(context.Background(), domain.Issue{Identifier: "JON-66"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(workspace.Path, "hook-env.txt"))
	if err != nil {
		t.Fatalf("ReadFile(hook-env.txt) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 {
		t.Fatalf("hook env lines = %d, want 4", len(lines))
	}
	if lines[0] != repoRoot {
		t.Fatalf("HARNESS_SOURCE_REPO = %q, want %q", lines[0], repoRoot)
	}
	if lines[1] != repoRoot {
		t.Fatalf("GO_HARNESS_SOURCE_REPO = %q, want %q", lines[1], repoRoot)
	}
	if lines[2] != workflowPath {
		t.Fatalf("HARNESS_WORKFLOW_PATH = %q, want %q", lines[2], workflowPath)
	}
	if lines[3] != workflowDir {
		t.Fatalf("HARNESS_WORKFLOW_DIR = %q, want %q", lines[3], workflowDir)
	}
}

func TestManagerPrepareRemovesWorkspaceAfterAfterCreateFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manager := NewManager(root, "", config.HooksConfig{
		AfterCreate: `echo broken >&2; exit 1`,
		Timeout:     time.Second,
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	_, err := manager.Prepare(context.Background(), domain.Issue{Identifier: "JON-66"})
	if err == nil {
		t.Fatal("Prepare() error = nil, want after_create failure")
	}
	if !strings.Contains(err.Error(), "after_create hook failed") {
		t.Fatalf("Prepare() error = %v, want after_create failure", err)
	}

	workspacePath := filepath.Join(root, "JON-66")
	if _, statErr := os.Stat(workspacePath); !os.IsNotExist(statErr) {
		t.Fatalf("workspace exists after after_create failure: %v", statErr)
	}
}
