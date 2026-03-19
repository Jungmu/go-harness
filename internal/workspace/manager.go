package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

type Manager struct {
	root       string
	sourcePath string
	hooks      config.HooksConfig
	logger     *slog.Logger
}

func NewManager(root, sourcePath string, hooks config.HooksConfig, logger *slog.Logger) *Manager {
	return &Manager{
		root:       root,
		sourcePath: sourcePath,
		hooks:      hooks,
		logger:     logger,
	}
}

func (m *Manager) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	workspace := domain.Workspace{
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
		Path:         filepath.Join(m.root, domain.SanitizeWorkspaceKey(issue.Identifier)),
	}

	if err := m.ensureSafePath(workspace.Path); err != nil {
		return domain.Workspace{}, err
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return domain.Workspace{}, err
	}
	if err := m.validateExistingWorkspacePath(m.root, true); err != nil {
		return domain.Workspace{}, err
	}

	info, err := os.Lstat(workspace.Path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return domain.Workspace{}, fmt.Errorf("workspace path is a symlink: %s", workspace.Path)
	case err == nil && !info.IsDir():
		return domain.Workspace{}, fmt.Errorf("workspace path is not a directory: %s", workspace.Path)
	case os.IsNotExist(err):
		if err := os.MkdirAll(workspace.Path, 0o755); err != nil {
			return domain.Workspace{}, err
		}
		workspace.CreatedNow = true
	case err != nil:
		return domain.Workspace{}, err
	}

	if workspace.CreatedNow {
		if err := m.runHook(ctx, workspace.Path, m.hooks.AfterCreate); err != nil {
			m.cleanupFailedPrepare(workspace.Path)
			return domain.Workspace{}, fmt.Errorf("after_create hook failed: %w", err)
		}
	}
	if err := m.runHook(ctx, workspace.Path, m.hooks.BeforeRun); err != nil {
		return domain.Workspace{}, fmt.Errorf("before_run hook failed: %w", err)
	}

	return workspace, nil
}

func (m *Manager) AfterRun(ctx context.Context, workspace domain.Workspace) error {
	if workspace.Path == "" {
		return nil
	}
	return m.runHook(ctx, workspace.Path, m.hooks.AfterRun)
}

func (m *Manager) Cleanup(ctx context.Context, workspace domain.Workspace) error {
	if workspace.Path == "" {
		return nil
	}
	if err := m.ensureSafePath(workspace.Path); err != nil {
		return err
	}
	if err := m.validateExistingWorkspacePath(workspace.Path, false); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := m.runHook(ctx, workspace.Path, m.hooks.BeforeRemove); err != nil {
		return fmt.Errorf("before_remove hook failed: %w", err)
	}
	return os.RemoveAll(workspace.Path)
}

func (m *Manager) runHook(ctx context.Context, cwd, script string) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}

	hookCtx, cancel := context.WithTimeout(ctx, m.hooks.Timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "bash", "-lc", script)
	cmd.Dir = cwd
	cmd.Env = m.hookEnv()

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(combined.String())
		if output != "" {
			output = truncate(output, 1024)
		}
		return fmt.Errorf("%w: %s", err, output)
	}

	return nil
}

func (m *Manager) cleanupFailedPrepare(path string) {
	if err := m.ensureSafePath(path); err != nil {
		return
	}
	if err := os.RemoveAll(path); err != nil && m.logger != nil {
		m.logger.Warn("failed to remove partially prepared workspace", slog.String("workspace", path), slog.Any("error", err))
	}
}

func (m *Manager) hookEnv() []string {
	env := append([]string{}, os.Environ()...)
	if strings.TrimSpace(m.sourcePath) == "" {
		return env
	}

	workflowPath := filepath.Clean(m.sourcePath)
	workflowDir := filepath.Dir(workflowPath)
	env = upsertEnv(env, "HARNESS_WORKFLOW_PATH", workflowPath)
	env = upsertEnv(env, "HARNESS_WORKFLOW_DIR", workflowDir)

	if repoRoot, ok := discoverSourceRepoRoot(workflowPath); ok {
		env = upsertEnv(env, "HARNESS_SOURCE_REPO", repoRoot)
		// Keep the old variable available so previously copied local workflows do not break.
		env = upsertEnv(env, "GO_HARNESS_SOURCE_REPO", repoRoot)
	}

	return env
}

func discoverSourceRepoRoot(workflowPath string) (string, bool) {
	if strings.TrimSpace(workflowPath) == "" {
		return "", false
	}

	dir := filepath.Dir(filepath.Clean(workflowPath))
	for {
		gitPath := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			if info.IsDir() || info.Mode().IsRegular() {
				return dir, true
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func (m *Manager) ensureSafePath(path string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(m.root))
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	if pathAbs == rootAbs {
		return fmt.Errorf("workspace path must not equal workspace root: %s", pathAbs)
	}
	if !strings.HasPrefix(pathAbs+string(os.PathSeparator), rootAbs+string(os.PathSeparator)) {
		return fmt.Errorf("workspace path escapes root: %s", pathAbs)
	}
	return nil
}

func (m *Manager) validateExistingWorkspacePath(path string, allowRoot bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace path is a symlink: %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace path is not a directory: %s", path)
	}
	if !allowRoot && filepath.Clean(path) == filepath.Clean(m.root) {
		return fmt.Errorf("workspace path must not equal workspace root: %s", path)
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
