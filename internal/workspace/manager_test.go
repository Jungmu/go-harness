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
	manager := NewManager(root, config.HooksConfig{
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

	manager := NewManager(root, config.HooksConfig{Timeout: time.Second}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	err := manager.Cleanup(context.Background(), domain.Workspace{Path: linkPath, WorkspaceKey: "ABC-1"})
	if err == nil {
		t.Fatal("Cleanup() error = nil, want symlink rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Cleanup() error = %v, want symlink error", err)
	}
}
