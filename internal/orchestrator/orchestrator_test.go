package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"go-harness/internal/agent/codex"
	"go-harness/internal/config"
	"go-harness/internal/domain"
	"go-harness/internal/workflow"
)

func TestOrchestratorSchedulesRetryAfterSuccessfulActiveRun(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{issue}
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"})
			onEvent(codex.Event{Type: "turn_completed", At: time.Now().UTC(), Usage: domain.RuntimeTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}})
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				Totals:         domain.RuntimeTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := New(cfg, tracker, workspaceManager, runner, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		snapshot := orch.Snapshot()
		return snapshot.Counts.Retrying == 1
	})

	snapshot := orch.Snapshot()
	if snapshot.Retrying[0].Reason != "active_still_open" {
		t.Fatalf("retry reason = %q, want active_still_open", snapshot.Retrying[0].Reason)
	}
}

func TestOrchestratorCancelsTerminalIssueAndCleansWorkspace(t *testing.T) {
	cfg := loadTestConfig(t)
	active := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}
	terminal := active
	terminal.State = "Done"

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{active} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{terminal}
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"})
			<-ctx.Done()
			return codex.RunResult{}, ctx.Err()
		},
	}

	orch := New(cfg, tracker, workspaceManager, runner, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		return atomic.LoadInt32(&workspaceManager.cleanupCalls) > 0
	})

	waitFor(t, func() bool {
		return orch.Snapshot().Counts.Running == 0
	})
}

func TestOrchestratorRetriesWhenMaxTurnsReached(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{issue}
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1", TurnCount: 1})
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1", TurnCount: 1},
				StopReason:     "max_turns_reached",
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := New(cfg, tracker, workspaceManager, runner, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		snapshot := orch.Snapshot()
		return snapshot.Counts.Retrying == 1
	})

	snapshot := orch.Snapshot()
	if snapshot.Retrying[0].Reason != "max_turns_reached" {
		t.Fatalf("retry reason = %q, want max_turns_reached", snapshot.Retrying[0].Reason)
	}
}

func TestOrchestratorContinuationStopsWithoutRetryWhenIssueTurnsTerminal(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Agent.MaxTurns = 3

	active := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}
	terminal := active
	terminal.State = "Done"

	var fetchCount atomic.Int32
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{active} },
		fetchByIDs: func(_ []string) []domain.Issue {
			call := fetchCount.Add(1)
			if call == 1 {
				return []domain.Issue{active}
			}
			return []domain.Issue{terminal}
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			first := domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1", TurnCount: 1}
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: first.SessionID, ThreadID: first.ThreadID, TurnID: first.TurnID, TurnCount: first.TurnCount})

			decision, err := continueFn(ctx, first)
			if err != nil {
				t.Fatalf("continueFn(first) error = %v", err)
			}
			if !decision.Continue {
				t.Fatalf("continueFn(first) Continue = false, want true")
			}
			if !strings.Contains(decision.NextPrompt, "Issue: ABC-1") {
				t.Fatalf("continuation prompt missing issue identifier: %q", decision.NextPrompt)
			}
			if !strings.Contains(decision.NextPrompt, "Continuation turn: 2") {
				t.Fatalf("continuation prompt missing turn count: %q", decision.NextPrompt)
			}

			second := domain.LiveSession{SessionID: "thread-1-turn-2", ThreadID: "thread-1", TurnID: "turn-2", TurnCount: 2}
			onEvent(codex.Event{Type: "turn_started", At: time.Now().UTC(), SessionID: second.SessionID, ThreadID: second.ThreadID, TurnID: second.TurnID, TurnCount: second.TurnCount})

			decision, err = continueFn(ctx, second)
			if err != nil {
				t.Fatalf("continueFn(second) error = %v", err)
			}
			if decision.Continue {
				t.Fatalf("continueFn(second) Continue = true, want false")
			}
			if decision.RefreshedIssue == nil || decision.RefreshedIssue.State != "Done" {
				t.Fatalf("continueFn(second) refreshed issue = %#v, want terminal issue", decision.RefreshedIssue)
			}

			return codex.RunResult{
				Session:        second,
				RefreshedIssue: decision.RefreshedIssue,
			}, nil
		},
	}

	orch := New(cfg, tracker, workspaceManager, runner, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		snapshot := orch.Snapshot()
		return len(snapshot.Completed) == 1
	})

	snapshot := orch.Snapshot()
	if snapshot.Counts.Retrying != 0 {
		t.Fatalf("retrying = %d, want 0", snapshot.Counts.Retrying)
	}
	if snapshot.Completed[0] != "ABC-1" {
		t.Fatalf("completed = %#v, want ABC-1", snapshot.Completed)
	}
	if fetchCount.Load() != 2 {
		t.Fatalf("fetch count = %d, want 2", fetchCount.Load())
	}
}

type fakeTracker struct {
	pollCandidates func() []domain.Issue
	fetchByIDs     func([]string) []domain.Issue
}

func (f *fakeTracker) PollCandidates(_ context.Context) ([]domain.Issue, error) {
	return f.pollCandidates(), nil
}

func (f *fakeTracker) FetchByIDs(_ context.Context, ids []string) ([]domain.Issue, error) {
	return f.fetchByIDs(ids), nil
}

type fakeWorkspaceManager struct {
	root         string
	cleanupCalls int32
}

func (f *fakeWorkspaceManager) Prepare(_ context.Context, issue domain.Issue) (domain.Workspace, error) {
	path := filepath.Join(f.root, domain.SanitizeWorkspaceKey(issue.Identifier))
	if err := os.MkdirAll(path, 0o755); err != nil {
		return domain.Workspace{}, err
	}
	return domain.Workspace{
		Path:         path,
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
		CreatedNow:   true,
	}, nil
}

func (f *fakeWorkspaceManager) AfterRun(_ context.Context, _ domain.Workspace) error {
	return nil
}

func (f *fakeWorkspaceManager) Cleanup(_ context.Context, workspace domain.Workspace) error {
	atomic.AddInt32(&f.cleanupCalls, 1)
	return os.RemoveAll(workspace.Path)
}

type fakeRunner struct {
	run func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error)
}

func (f *fakeRunner) RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
	return f.run(ctx, issue, workspace, prompt, attempt, onEvent, continueFn)
}

func loadTestConfig(t *testing.T) config.RuntimeConfig {
	t.Helper()

	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done"]
polling:
  interval_ms: 20
workspace:
  root: ` + filepath.Join(root, "workspaces") + `
agent:
  max_concurrent_agents: 1
  max_turns: 1
  max_retry_backoff_ms: 100
codex:
  command: "codex app-server"
---
Handle {{ issue.identifier }}
`

	path := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.NewStore(workflow.NewLoader()).LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}
	return cfg
}

func waitFor(t *testing.T, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
