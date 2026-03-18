package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go-harness/internal/workflow"
)

func withUnsetEnv(t *testing.T, key string) {
	t.Helper()

	original, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", key, err)
	}
	t.Cleanup(func() {
		var err error
		if existed {
			err = os.Setenv(key, original)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Fatalf("restore env %q error = %v", key, err)
		}
	})
}

func withExecutableDotEnv(t *testing.T, content string) string {
	t.Helper()

	executablePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	path := filepath.Join(filepath.Dir(executablePath), ".env")

	original, readErr := os.ReadFile(path)
	existed := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadFile(%q) error = %v", path, readErr)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	t.Cleanup(func() {
		var err error
		if existed {
			err = os.WriteFile(path, original, 0o644)
		} else {
			err = os.Remove(path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			t.Fatalf("restore %q error = %v", path, err)
		}
	})

	return path
}

func TestStoreLoadAndValidateAppliesDefaultsAndEnvResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")
	t.Setenv("WORKSPACE_ROOT", filepath.Join(root, "workspaces"))

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
agent:
  max_concurrent_agents_by_state:
    In Progress: 2
---
Handle {{ issue.identifier }}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	if cfg.Tracker.APIKey != "linear-token" {
		t.Fatalf("Tracker.APIKey = %q", cfg.Tracker.APIKey)
	}
	if cfg.Polling.Interval != defaultPollingInterval {
		t.Fatalf("Polling.Interval = %v", cfg.Polling.Interval)
	}
	if cfg.Workspace.Root != filepath.Clean(filepath.Join(root, "workspaces")) {
		t.Fatalf("Workspace.Root = %q", cfg.Workspace.Root)
	}
	if !cfg.IsActiveState("in progress") {
		t.Fatalf("IsActiveState(in progress) = false, want true")
	}
	if cfg.MaxConcurrentForState("in progress") != 2 {
		t.Fatalf("MaxConcurrentForState(in progress) = %d, want 2", cfg.MaxConcurrentForState("in progress"))
	}
}

func TestStoreLoadAndValidateReadsDotEnvFromExecutableDirectory(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")
	withUnsetEnv(t, "WORKSPACE_ROOT")
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nWORKSPACE_ROOT="+filepath.Join(root, "dotenv-workspaces")+"\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
---
Handle {{ issue.identifier }}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	if cfg.Tracker.APIKey != "dotenv-token" {
		t.Fatalf("Tracker.APIKey = %q, want dotenv-token", cfg.Tracker.APIKey)
	}
	if cfg.Workspace.Root != filepath.Join(root, "dotenv-workspaces") {
		t.Fatalf("Workspace.Root = %q, want %q", cfg.Workspace.Root, filepath.Join(root, "dotenv-workspaces"))
	}
}

func TestStoreLoadAndValidatePrefersProcessEnvOverDotEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "process-token")
	t.Setenv("WORKSPACE_ROOT", filepath.Join(root, "process-workspaces"))
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nWORKSPACE_ROOT="+filepath.Join(root, "dotenv-workspaces")+"\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
---
Handle {{ issue.identifier }}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	if cfg.Tracker.APIKey != "process-token" {
		t.Fatalf("Tracker.APIKey = %q, want process-token", cfg.Tracker.APIKey)
	}
	if cfg.Workspace.Root != filepath.Join(root, "process-workspaces") {
		t.Fatalf("Workspace.Root = %q, want %q", cfg.Workspace.Root, filepath.Join(root, "process-workspaces"))
	}
}

func TestStoreLoadAndValidateRejectsMissingProjectSlug(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
---
Prompt
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err == nil {
		t.Fatal("LoadAndValidate() error = nil, want error")
	}

	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if validationErr.Field != "tracker.project_slug" {
		t.Fatalf("ValidationError.Field = %q, want tracker.project_slug", validationErr.Field)
	}
}

func TestStoreReloadIfChangedKeepsLastKnownGoodOnInvalidConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	valid := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
polling:
  interval_ms: 25
---
Prompt
`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(workflow.NewLoader())
	cfg, err := store.LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	invalid := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
---
Prompt
`
	if err := os.WriteFile(path, []byte(invalid), 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded, changed, err := store.ReloadIfChanged()
	if err == nil {
		t.Fatal("ReloadIfChanged() error = nil, want error")
	}
	if changed {
		t.Fatal("ReloadIfChanged() changed = true, want false on invalid reload")
	}
	if reloaded.Tracker.ProjectSlug != cfg.Tracker.ProjectSlug {
		t.Fatalf("ProjectSlug changed after invalid reload: got %q want %q", reloaded.Tracker.ProjectSlug, cfg.Tracker.ProjectSlug)
	}
	if store.DispatchValidationError() == nil {
		t.Fatal("DispatchValidationError() = nil, want error after invalid reload")
	}

	time.Sleep(20 * time.Millisecond)
	validAgain := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST2
polling:
  interval_ms: 50
---
Prompt v2
`
	if err := os.WriteFile(path, []byte(validAgain), 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded, changed, err = store.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged() error = %v", err)
	}
	if !changed {
		t.Fatal("ReloadIfChanged() changed = false, want true")
	}
	if reloaded.Tracker.ProjectSlug != "TEST2" {
		t.Fatalf("ProjectSlug = %q, want TEST2", reloaded.Tracker.ProjectSlug)
	}
	if reloaded.Polling.Interval != 50*time.Millisecond {
		t.Fatalf("Polling.Interval = %v, want 50ms", reloaded.Polling.Interval)
	}
	if store.DispatchValidationError() != nil {
		t.Fatalf("DispatchValidationError() = %v, want nil after valid reload", store.DispatchValidationError())
	}
}

func TestStoreReloadIfChangedReloadsWhenExecutableDotEnvChanges(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
---
Prompt
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token-1\n")

	store := NewStore(workflow.NewLoader())
	cfg, err := store.LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}
	if cfg.Tracker.APIKey != "dotenv-token-1" {
		t.Fatalf("Tracker.APIKey = %q, want dotenv-token-1", cfg.Tracker.APIKey)
	}

	time.Sleep(20 * time.Millisecond)
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token-2\n")

	reloaded, changed, err := store.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged() error = %v", err)
	}
	if !changed {
		t.Fatal("ReloadIfChanged() changed = false, want true after .env change")
	}
	if reloaded.Tracker.APIKey != "dotenv-token-2" {
		t.Fatalf("Tracker.APIKey = %q, want dotenv-token-2", reloaded.Tracker.APIKey)
	}
}
