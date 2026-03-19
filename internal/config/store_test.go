package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func withRequiredGitHubWorkflow(t *testing.T, content string) string {
	t.Helper()

	t.Setenv("GITHUB_TOKEN", "github-token")
	block := "github:\n  token: $GITHUB_TOKEN\n  owner: acme\n  repo: widgets\n  base_branch: main\n"
	if strings.Contains(content, "\ngithub:") {
		return content
	}
	if marker := "\npolling:"; strings.Contains(content, marker) {
		return strings.Replace(content, marker, "\n"+block+"polling:", 1)
	}
	if marker := "\nworkspace:"; strings.Contains(content, marker) {
		return strings.Replace(content, marker, "\n"+block+"workspace:", 1)
	}
	if marker := "\nlogging:"; strings.Contains(content, marker) {
		return strings.Replace(content, marker, "\n"+block+"logging:", 1)
	}
	if marker := "\n---\n"; strings.Contains(content, marker) {
		return strings.Replace(content, marker, "\n"+block+"---\n", 1)
	}
	t.Fatalf("workflow content missing a front matter insertion point:\n%s", content)
	return ""
}

func environmentEntryByName(t *testing.T, entries []EnvironmentEntry, name string) EnvironmentEntry {
	t.Helper()

	for _, entry := range entries {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("environment entry %q not found in %#v", name, entries)
	return EnvironmentEntry{}
}

func TestStoreLoadAndValidateAppliesDefaultsAndEnvResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")
	t.Setenv("WORKSPACE_ROOT", filepath.Join(root, "workspaces"))

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
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
`)
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
	if cfg.Logging.CapturePrompts {
		t.Fatal("Logging.CapturePrompts = true, want false by default")
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
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
---
Handle {{ issue.identifier }}
`)
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
	if len(cfg.Environment.Entries) != 3 {
		t.Fatalf("Environment.Entries = %#v, want 3 tracked entries", cfg.Environment.Entries)
	}
}

func TestStoreLoadAndValidatePrefersProcessEnvOverDotEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "process-token")
	t.Setenv("WORKSPACE_ROOT", filepath.Join(root, "process-workspaces"))
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nWORKSPACE_ROOT="+filepath.Join(root, "dotenv-workspaces")+"\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
---
Handle {{ issue.identifier }}
`)
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
	githubEntry := environmentEntryByName(t, cfg.Environment.Entries, "GITHUB_TOKEN")
	if githubEntry.Source != "process" || githubEntry.Value != "<redacted>" {
		t.Fatalf("Environment entry for GITHUB_TOKEN = %#v", githubEntry)
	}
	linearEntry := environmentEntryByName(t, cfg.Environment.Entries, "LINEAR_API_KEY")
	if linearEntry.Source != "process" || linearEntry.Value != "<redacted>" {
		t.Fatalf("Environment entry for LINEAR_API_KEY = %#v", linearEntry)
	}
	workspaceEntry := environmentEntryByName(t, cfg.Environment.Entries, "WORKSPACE_ROOT")
	if workspaceEntry.Source != "process" || workspaceEntry.Value != filepath.Join(root, "process-workspaces") {
		t.Fatalf("Environment entry for WORKSPACE_ROOT = %#v", workspaceEntry)
	}
}

func TestStoreLoadAndValidateTracksDotEnvEntriesWithRedaction(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")
	withUnsetEnv(t, "WORKSPACE_ROOT")
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nWORKSPACE_ROOT="+filepath.Join(root, "dotenv-workspaces")+"\nUNUSED_FLAG=1\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
workspace:
  root: $WORKSPACE_ROOT
---
Handle {{ issue.identifier }}
`)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	if len(cfg.Environment.Entries) != 4 {
		t.Fatalf("Environment.Entries = %#v, want 4 entries including github token and unused .env key", cfg.Environment.Entries)
	}
	githubEntry := environmentEntryByName(t, cfg.Environment.Entries, "GITHUB_TOKEN")
	if githubEntry.Value != "<redacted>" || githubEntry.Source != "process" {
		t.Fatalf("GITHUB_TOKEN entry = %#v", githubEntry)
	}
	linearEntry := environmentEntryByName(t, cfg.Environment.Entries, "LINEAR_API_KEY")
	if linearEntry.Value != "<redacted>" || linearEntry.Source != ".env" {
		t.Fatalf("LINEAR_API_KEY entry = %#v", linearEntry)
	}
	unusedEntry := environmentEntryByName(t, cfg.Environment.Entries, "UNUSED_FLAG")
	if unusedEntry.Value != "1" || unusedEntry.Source != ".env" {
		t.Fatalf("UNUSED_FLAG entry = %#v", unusedEntry)
	}
	workspaceEntry := environmentEntryByName(t, cfg.Environment.Entries, "WORKSPACE_ROOT")
	if workspaceEntry.Value != filepath.Join(root, "dotenv-workspaces") || workspaceEntry.Source != ".env" {
		t.Fatalf("WORKSPACE_ROOT entry = %#v", workspaceEntry)
	}
}

func TestStoreLoadAndValidateResolvesProjectSlugFromEnv(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")
	withUnsetEnv(t, "PROJECT_SLUG")
	withExecutableDotEnv(t, "LINEAR_API_KEY=dotenv-token\nPROJECT_SLUG=improve-harness\n")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: $PROJECT_SLUG
---
Handle {{ issue.identifier }}
`)
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
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
---
Prompt
`)
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

func TestStoreLoadAndValidateAllowsMissingGitHubToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
github:
  owner: acme
  repo: widgets
  base_branch: main
---
Prompt
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}
	if cfg.GitHub.Token != "" {
		t.Fatalf("GitHub.Token = %q, want empty when not configured", cfg.GitHub.Token)
	}
}

func TestStoreLoadAndValidateRejectsInvalidLoggingLevel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
logging:
  level: verbose
---
Prompt
`)
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

func TestStoreLoadAndValidateRejectsNonBooleanDraftPullRequest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
github:
  token: literal-token
  owner: acme
  repo: widgets
  base_branch: main
  draft_pull_request: yes
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
	if validationErr.Field != "github.draft_pull_request" {
		t.Fatalf("ValidationError.Field = %q, want github.draft_pull_request", validationErr.Field)
	}
}

func TestStoreLoadAndValidateRejectsNonBooleanCapturePrompts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
logging:
  capture_prompts: verbose
---
Prompt
`)
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
	if validationErr.Field != "logging.capture_prompts" {
		t.Fatalf("ValidationError.Field = %q, want logging.capture_prompts", validationErr.Field)
	}
}

func TestStoreLoadAndValidateUsesExecutableWorkflowWhenCWDDefaultIsMissing(t *testing.T) {
	root := t.TempDir()
	withWorkingDir(t, root)

	workflowPath := withExecutableWorkflow(t, withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: executable-token
  project_slug: EXECUTABLE
---
Prompt
`))

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
	withExecutableWorkflow(t, withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: executable-token
  project_slug: EXECUTABLE
---
Executable prompt
`))

	cwdWorkflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(cwdWorkflowPath, []byte(withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: cwd-token
  project_slug: CWD
---
Cwd prompt
`)), 0o644); err != nil {
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
		GitHub: GitHubConfig{
			Endpoint:   "https://api.github.com",
			Token:      "token-1",
			Owner:      "acme",
			Repo:       "widgets",
			BaseBranch: "main",
		},
		Tracker: TrackerConfig{
			Kind:           "linear",
			ProjectSlug:    "MAIN",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed"},
		},
		Workspace: WorkspaceConfig{Root: "/tmp/main"},
	}
	reviewCfg := RuntimeConfig{
		GitHub: GitHubConfig{
			Endpoint:   "https://api.github.com",
			Token:      "token-2",
			Owner:      "other",
			Repo:       "widgets",
			BaseBranch: "main",
		},
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
	reviewCfg.GitHub.Owner = "acme"
	reviewCfg.Tracker.ActiveStates = []string{"In Review"}
	reviewCfg.Tracker.TerminalStates = []string{"Closed", "Done"}
	if err := ValidateReviewWorkflow(mainCfg, reviewCfg); err != nil {
		t.Fatalf("ValidateReviewWorkflow() error = %v", err)
	}
}

func TestStoreLoadAndValidateReviewWorkflowInheritsMainConfig(t *testing.T) {
	root := t.TempDir()
	reviewPath := filepath.Join(root, ReviewWorkflowFilename)
	if err := os.WriteFile(reviewPath, []byte(`---
tracker:
  active_states:
    - In Review
agent:
  max_turns: 1
---
Review prompt
`), 0o644); err != nil {
		t.Fatal(err)
	}

	mainCfg := RuntimeConfig{
		SourcePath:     "/repo/WORKFLOW.md",
		PromptTemplate: "Main prompt",
		Environment: EnvironmentConfig{
			DotEnvPath:    "/repo/.env",
			DotEnvPresent: true,
			Entries: []EnvironmentEntry{
				{Name: "LINEAR_API_KEY", Value: "<redacted>", Source: ".env"},
				{Name: "GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG", Value: "improve-harness", Source: ".env"},
			},
		},
		Tracker: TrackerConfig{
			Kind:           "linear",
			Endpoint:       "https://api.linear.app/graphql",
			APIKey:         "linear-token",
			ProjectSlug:    "MAIN",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed"},
		},
		GitHub: GitHubConfig{
			Endpoint:   "https://api.github.com",
			Token:      "github-token",
			Owner:      "acme",
			Repo:       "widgets",
			BaseBranch: "main",
		},
		Polling:   PollingConfig{Interval: 30 * time.Second},
		Workspace: WorkspaceConfig{Root: filepath.Join(root, "workspaces")},
		Hooks: HooksConfig{
			AfterCreate:  "echo after-create",
			BeforeRun:    "echo before-run",
			AfterRun:     "echo after-run",
			BeforeRemove: "echo before-remove",
			Timeout:      time.Minute,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents:        5,
			MaxTurns:                   3,
			MaxRetryBackoff:            5 * time.Minute,
			MaxConcurrentAgentsByState: map[string]int{"in progress": 2},
		},
		Codex: CodexConfig{
			Command:           "codex app-server",
			ApprovalPolicy:    "never",
			ThreadSandbox:     "danger-full-access",
			TurnSandboxPolicy: map[string]any{"type": "danger-full-access"},
			TurnTimeout:       time.Hour,
			ReadTimeout:       5 * time.Second,
			StallTimeout:      5 * time.Minute,
		},
		Logging: LoggingConfig{
			Level:          "debug",
			CapturePrompts: true,
		},
	}
	mainCfg.Tracker.activeStateSet = makeStateSet(mainCfg.Tracker.ActiveStates)
	mainCfg.Tracker.terminalStateSet = makeStateSet(mainCfg.Tracker.TerminalStates)

	store := NewStore(workflow.NewLoader())
	store.SetBaseConfig(func() RuntimeConfig { return mainCfg })
	store.SetValidator(func(reviewCfg RuntimeConfig) error {
		return ValidateReviewWorkflow(mainCfg, reviewCfg)
	})

	cfg, err := store.LoadAndValidate(reviewPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}
	if cfg.SourcePath != reviewPath {
		t.Fatalf("SourcePath = %q, want %q", cfg.SourcePath, reviewPath)
	}
	if cfg.PromptTemplate != "Review prompt" {
		t.Fatalf("PromptTemplate = %q, want review prompt", cfg.PromptTemplate)
	}
	if cfg.GitHub.Owner != mainCfg.GitHub.Owner || cfg.GitHub.Repo != mainCfg.GitHub.Repo {
		t.Fatalf("GitHub config not inherited: %#v", cfg.GitHub)
	}
	if cfg.Tracker.ProjectSlug != mainCfg.Tracker.ProjectSlug {
		t.Fatalf("ProjectSlug = %q, want %q", cfg.Tracker.ProjectSlug, mainCfg.Tracker.ProjectSlug)
	}
	if cfg.Workspace.Root != mainCfg.Workspace.Root {
		t.Fatalf("Workspace.Root = %q, want %q", cfg.Workspace.Root, mainCfg.Workspace.Root)
	}
	if cfg.Agent.MaxTurns != 1 {
		t.Fatalf("Agent.MaxTurns = %d, want 1", cfg.Agent.MaxTurns)
	}
	if !cfg.Logging.CapturePrompts {
		t.Fatal("Logging.CapturePrompts = false, want inherited true")
	}
	if len(cfg.Environment.Entries) != 2 {
		t.Fatalf("Environment.Entries = %#v, want inherited entries", cfg.Environment.Entries)
	}
}

func TestStoreReloadIfChangedReloadsWhenBaseConfigChanges(t *testing.T) {
	root := t.TempDir()
	reviewPath := filepath.Join(root, ReviewWorkflowFilename)
	if err := os.WriteFile(reviewPath, []byte(`---
tracker:
  active_states:
    - In Review
---
Review prompt
`), 0o644); err != nil {
		t.Fatal(err)
	}

	baseCfg := RuntimeConfig{
		SourcePath: "/repo/WORKFLOW.md",
		Tracker: TrackerConfig{
			Kind:           "linear",
			Endpoint:       "https://api.linear.app/graphql",
			APIKey:         "linear-token",
			ProjectSlug:    "MAIN",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed"},
		},
		GitHub: GitHubConfig{
			Endpoint:   "https://api.github.com",
			Owner:      "acme",
			Repo:       "widgets",
			BaseBranch: "main",
		},
		Polling:   PollingConfig{Interval: 30 * time.Second},
		Workspace: WorkspaceConfig{Root: filepath.Join(root, "workspaces-a")},
		Hooks:     HooksConfig{Timeout: time.Minute},
		Agent: AgentConfig{
			MaxConcurrentAgents:        5,
			MaxTurns:                   3,
			MaxRetryBackoff:            5 * time.Minute,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: CodexConfig{
			Command:           "codex app-server",
			ApprovalPolicy:    "never",
			ThreadSandbox:     "workspace-write",
			TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
			TurnTimeout:       time.Hour,
			ReadTimeout:       5 * time.Second,
			StallTimeout:      5 * time.Minute,
		},
		Logging: LoggingConfig{Level: "info"},
	}
	baseCfg.Tracker.activeStateSet = makeStateSet(baseCfg.Tracker.ActiveStates)
	baseCfg.Tracker.terminalStateSet = makeStateSet(baseCfg.Tracker.TerminalStates)

	store := NewStore(workflow.NewLoader())
	store.SetBaseConfig(func() RuntimeConfig { return baseCfg })
	store.SetValidator(func(reviewCfg RuntimeConfig) error {
		return ValidateReviewWorkflow(baseCfg, reviewCfg)
	})

	cfg, err := store.LoadAndValidate(reviewPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}
	if cfg.Workspace.Root != baseCfg.Workspace.Root {
		t.Fatalf("Workspace.Root = %q, want %q", cfg.Workspace.Root, baseCfg.Workspace.Root)
	}

	baseCfg.Workspace.Root = filepath.Join(root, "workspaces-b")
	time.Sleep(20 * time.Millisecond)

	reloaded, changed, err := store.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged() error = %v", err)
	}
	if !changed {
		t.Fatal("ReloadIfChanged() changed = false, want true after base config change")
	}
	if reloaded.Workspace.Root != baseCfg.Workspace.Root {
		t.Fatalf("Workspace.Root = %q, want %q", reloaded.Workspace.Root, baseCfg.Workspace.Root)
	}
}

func TestStoreReloadIfChangedRunsValidatorWithoutFileChange(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
---
Prompt
`)
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
	valid := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
polling:
  interval_ms: 25
---
Prompt
`)
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(workflow.NewLoader())
	cfg, err := store.LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	invalid := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
---
Prompt
`)
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
	validAgain := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST2
polling:
  interval_ms: 50
logging:
  level: debug
  capture_prompts: true
---
Prompt v2
`)
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
	if !reloaded.Logging.CapturePrompts {
		t.Fatal("Logging.CapturePrompts = false, want true after reload")
	}
	if store.DispatchValidationError() != nil {
		t.Fatalf("DispatchValidationError() = %v, want nil after valid reload", store.DispatchValidationError())
	}
}

func TestStoreReloadIfChangedReloadsWhenExecutableDotEnvChanges(t *testing.T) {
	root := t.TempDir()
	withUnsetEnv(t, "LINEAR_API_KEY")

	path := filepath.Join(root, "WORKFLOW.md")
	content := withRequiredGitHubWorkflow(t, `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
---
Prompt
`)
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
