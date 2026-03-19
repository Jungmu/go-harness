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

func withExecutableWorkflow(t *testing.T, content string) string {
	t.Helper()

	executablePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	path := filepath.Join(filepath.Dir(executablePath), "WORKFLOW.md")

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

func withWorkingDir(t *testing.T, dir string) {
	t.Helper()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q) error = %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore cwd error = %v", err)
		}
	})
}

func assertSameFile(t *testing.T, gotPath, wantPath string) {
	t.Helper()

	gotInfo, err := os.Stat(gotPath)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", gotPath, err)
	}
	wantInfo, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", wantPath, err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("paths do not reference the same file: got %q want %q", gotPath, wantPath)
	}
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
	if cfg.Logging.Level != defaultLogLevel {
		t.Fatalf("Logging.Level = %q, want %q", cfg.Logging.Level, defaultLogLevel)
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
	withUnsetEnv(t, "PROJECT_SLUG")
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
	if !cfg.Environment.DotEnvPresent {
		t.Fatal("Environment.DotEnvPresent = false, want true")
	}
	if cfg.Environment.DotEnvPath == "" {
		t.Fatal("Environment.DotEnvPath = empty, want executable .env path")
	}
	if len(cfg.Environment.Entries) != 2 {
		t.Fatalf("Environment.Entries = %#v, want 2 tracked entries", cfg.Environment.Entries)
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
	if cfg.Environment.Entries[0].Name != "LINEAR_API_KEY" || cfg.Environment.Entries[0].Source != "process" || cfg.Environment.Entries[0].Value != "<redacted>" {
		t.Fatalf("Environment entry for LINEAR_API_KEY = %#v", cfg.Environment.Entries[0])
	}
	if cfg.Environment.Entries[1].Name != "WORKSPACE_ROOT" || cfg.Environment.Entries[1].Source != "process" || cfg.Environment.Entries[1].Value != filepath.Join(root, "process-workspaces") {
		t.Fatalf("Environment entry for WORKSPACE_ROOT = %#v", cfg.Environment.Entries[1])
	}
}

func TestStoreLoadAndValidateTracksDotEnvEntriesWithRedaction(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")
	withUnsetEnv(t, "WORKSPACE_ROOT")
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nWORKSPACE_ROOT="+filepath.Join(root, "dotenv-workspaces")+"\nUNUSED_FLAG=1\n")

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

	if len(cfg.Environment.Entries) != 3 {
		t.Fatalf("Environment.Entries = %#v, want 3 entries including unused .env key", cfg.Environment.Entries)
	}
	if cfg.Environment.Entries[0].Name != "LINEAR_API_KEY" || cfg.Environment.Entries[0].Value != "<redacted>" || cfg.Environment.Entries[0].Source != ".env" {
		t.Fatalf("LINEAR_API_KEY entry = %#v", cfg.Environment.Entries[0])
	}
	if cfg.Environment.Entries[1].Name != "UNUSED_FLAG" || cfg.Environment.Entries[1].Value != "1" || cfg.Environment.Entries[1].Source != ".env" {
		t.Fatalf("UNUSED_FLAG entry = %#v", cfg.Environment.Entries[1])
	}
	if cfg.Environment.Entries[2].Name != "WORKSPACE_ROOT" || cfg.Environment.Entries[2].Value != filepath.Join(root, "dotenv-workspaces") || cfg.Environment.Entries[2].Source != ".env" {
		t.Fatalf("WORKSPACE_ROOT entry = %#v", cfg.Environment.Entries[2])
	}
}

func TestStoreLoadAndValidateResolvesProjectSlugFromEnv(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")
	withUnsetEnv(t, "PROJECT_SLUG")
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nPROJECT_SLUG=improve-harness\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: $PROJECT_SLUG
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
	if cfg.Tracker.ProjectSlug != "improve-harness" {
		t.Fatalf("Tracker.ProjectSlug = %q, want improve-harness", cfg.Tracker.ProjectSlug)
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

func TestStoreLoadAndValidateRejectsInvalidLoggingLevel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
logging:
  level: verbose
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
	if validationErr.Field != "logging.level" {
		t.Fatalf("ValidationError.Field = %q, want logging.level", validationErr.Field)
	}
}

func TestStoreLoadAndValidateUsesExecutableWorkflowWhenCWDDefaultIsMissing(t *testing.T) {
	root := t.TempDir()
	withWorkingDir(t, root)

	workflowPath := withExecutableWorkflow(t, `---
tracker:
  kind: linear
  api_key: executable-token
  project_slug: EXECUTABLE
---
Prompt
`)

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate("")
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	assertSameFile(t, cfg.SourcePath, workflowPath)
	if cfg.Tracker.ProjectSlug != "EXECUTABLE" {
		t.Fatalf("ProjectSlug = %q, want EXECUTABLE", cfg.Tracker.ProjectSlug)
	}
}

func TestStoreLoadAndValidatePrefersCWDWorkflowOverExecutableWorkflow(t *testing.T) {
	root := t.TempDir()
	withWorkingDir(t, root)
	withExecutableWorkflow(t, `---
tracker:
  kind: linear
  api_key: executable-token
  project_slug: EXECUTABLE
---
Executable prompt
`)

	cwdWorkflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(cwdWorkflowPath, []byte(`---
tracker:
  kind: linear
  api_key: cwd-token
  project_slug: CWD
---
Cwd prompt
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate("")
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	assertSameFile(t, cfg.SourcePath, cwdWorkflowPath)
	if cfg.Tracker.ProjectSlug != "CWD" {
		t.Fatalf("ProjectSlug = %q, want CWD", cfg.Tracker.ProjectSlug)
	}
}

func TestResolveSiblingWorkflowPathFindsReviewWorkflow(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "WORKFLOW.md")
	reviewPath := filepath.Join(root, ReviewWorkflowFilename)
	if err := os.WriteFile(mainPath, []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, []byte("review"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, found, err := ResolveSiblingWorkflowPath(mainPath, ReviewWorkflowFilename)
	if err != nil {
		t.Fatalf("ResolveSiblingWorkflowPath() error = %v", err)
	}
	if !found {
		t.Fatal("ResolveSiblingWorkflowPath() found = false, want true")
	}
	assertSameFile(t, resolved, reviewPath)
}

func TestValidateReviewWorkflowRejectsMismatchedSettings(t *testing.T) {
	mainCfg := RuntimeConfig{
		Tracker: TrackerConfig{
			Kind:           "linear",
			ProjectSlug:    "MAIN",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed"},
		},
		Workspace: WorkspaceConfig{Root: "/tmp/main"},
	}
	reviewCfg := RuntimeConfig{
		Tracker: TrackerConfig{
			Kind:           "linear",
			ProjectSlug:    "OTHER",
			ActiveStates:   []string{"In Review", "Todo"},
			TerminalStates: []string{"Done"},
		},
		Workspace: WorkspaceConfig{Root: "/tmp/review"},
	}

	if err := ValidateReviewWorkflow(mainCfg, reviewCfg); err == nil {
		t.Fatal("ValidateReviewWorkflow() error = nil, want mismatch error")
	}

	reviewCfg.Tracker.ProjectSlug = "MAIN"
	reviewCfg.Workspace.Root = "/tmp/main"
	reviewCfg.Tracker.ActiveStates = []string{"In Review"}
	reviewCfg.Tracker.TerminalStates = []string{"Closed", "Done"}
	if err := ValidateReviewWorkflow(mainCfg, reviewCfg); err != nil {
		t.Fatalf("ValidateReviewWorkflow() error = %v", err)
	}
}

func TestStoreReloadIfChangedRunsValidatorWithoutFileChange(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

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

	var allow bool
	store := NewStore(workflow.NewLoader())
	store.SetValidator(func(RuntimeConfig) error {
		if allow {
			return nil
		}
		return errors.New("validator_blocked")
	})

	if _, err := store.LoadAndValidate(path); err == nil {
		t.Fatal("LoadAndValidate() error = nil, want validator error")
	}

	allow = true
	if _, err := store.LoadAndValidate(path); err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	allow = false
	cfg, changed, err := store.ReloadIfChanged()
	if err == nil {
		t.Fatal("ReloadIfChanged() error = nil, want validator error")
	}
	if changed {
		t.Fatal("ReloadIfChanged() changed = true, want false")
	}
	if cfg.Tracker.ProjectSlug != "TEST" {
		t.Fatalf("ProjectSlug = %q, want TEST", cfg.Tracker.ProjectSlug)
	}
	if store.DispatchValidationError() == nil {
		t.Fatal("DispatchValidationError() = nil, want validator error")
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
logging:
  level: debug
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
	if reloaded.Logging.Level != "debug" {
		t.Fatalf("Logging.Level = %q, want debug", reloaded.Logging.Level)
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
