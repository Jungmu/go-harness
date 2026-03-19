package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

func TestOrchestratorMovesIssueToInProgressThenDone(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Todo"}

	var transitions []string
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{issue}
		},
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			transitions = append(transitions, stateName)
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			if issue.State != "In Progress" {
				t.Fatalf("runner issue state = %q, want In Progress", issue.State)
			}
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"})
			onEvent(codex.Event{Type: "turn_completed", At: time.Now().UTC(), Usage: domain.RuntimeTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}})
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				Totals:         domain.RuntimeTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
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
	if len(transitions) != 2 || transitions[0] != "In Progress" || transitions[1] != "Done" {
		t.Fatalf("transitions = %#v, want [In Progress Done]", transitions)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "issue_claimed") {
		t.Fatalf("recent activity missing issue_claimed: %#v", snapshot.RecentActivity)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "issue_completed") {
		t.Fatalf("recent activity missing issue_completed: %#v", snapshot.RecentActivity)
	}
	historyPath := filepath.Join(cfg.Workspace.Root, ".harness-history", "ABC-1.jsonl")
	data, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", historyPath, err)
	}
	for _, expected := range []string{"issue_claimed", "workspace_prepared", "tracker_state_transition", "github_pull_request_ready", "issue_completed"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("history file missing %q: %s", expected, string(data))
		}
	}
}

func TestOrchestratorRetriesWhenPullRequestCreationFails(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Todo", BranchName: "feature/ABC-1"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner, WithPullRequestCreator(&fakePullRequestCreator{
		ensure: func(context.Context, domain.Issue, domain.Workspace) (domain.PullRequest, error) {
			return domain.PullRequest{}, errors.New("push failed")
		},
	}))
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
		return snapshot.Counts.Retrying == 1 && containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "github_pull_request_failed")
	})

	snapshot := orch.Snapshot()
	if len(snapshot.Completed) != 0 {
		t.Fatalf("completed = %#v, want empty", snapshot.Completed)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "attempt_failed") {
		t.Fatalf("recent activity missing attempt_failed: %#v", snapshot.RecentActivity)
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
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
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

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
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

func TestOrchestratorHandsOffIssueToInReviewWhenMaxTurnsReached(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}

	var transitions []string
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{issue}
		},
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			transitions = append(transitions, stateName)
			issue.State = stateName
			return issue, nil
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

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
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
	if len(transitions) != 1 || transitions[0] != "In Review" {
		t.Fatalf("transitions = %#v, want [In Review]", transitions)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "issue_completed") {
		t.Fatalf("recent activity missing issue_completed: %#v", snapshot.RecentActivity)
	}
}

func TestApplyWorkerExitPrefersSuccessfulHandoffOverNonActiveStopReason(t *testing.T) {
	cfg := loadTestConfig(t)
	orch := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{root: cfg.Workspace.Root}, &fakeRunner{})

	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Progress"}
	workspace := domain.Workspace{WorkspaceKey: "ABC-1", Path: filepath.Join(cfg.Workspace.Root, "ABC-1")}
	st := &state{
		running: map[string]*runningTask{
			issue.ID: {
				issue:      issue,
				attempt:    1,
				workspace:  workspace,
				startedAt:  time.Now().UTC(),
				stopReason: "non_active_state",
			},
		},
		claimed:    map[string]struct{}{issue.ID: {}},
		retryQueue: map[string]domain.RetryEntry{},
		completed:  map[string]struct{}{},
		history:    map[string][]domain.TimelineEvent{},
		rateLimits: map[string]domain.RateLimitSnapshot{},
	}

	handoff := issue
	handoff.State = "In Review"
	orch.applyWorkerExit(context.Background(), st, workerExit{
		Issue:     issue,
		Attempt:   1,
		Workspace: workspace,
		Result: codex.RunResult{
			Session: domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
		},
		Refreshed: &handoff,
	})

	if _, ok := st.completed[handoff.Identifier]; !ok {
		t.Fatalf("completed missing %q after successful handoff", handoff.Identifier)
	}
	if _, ok := st.claimed[issue.ID]; ok {
		t.Fatalf("claimed still contains %q after successful handoff", issue.ID)
	}
	if containsTimelineEvent(st.recentActivity, handoff.Identifier, "issue_released") {
		t.Fatalf("recent activity unexpectedly contains issue_released: %#v", st.recentActivity)
	}
	if !containsTimelineEvent(st.recentActivity, handoff.Identifier, "issue_completed") {
		t.Fatalf("recent activity missing issue_completed: %#v", st.recentActivity)
	}
}

func TestOrchestratorContinuationStopsWithoutRetryWhenIssueTurnsTerminal(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Agent.MaxTurns = 3
	cfg.Polling.Interval = time.Hour

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
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
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

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
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

func TestOrchestratorUpsertsProgressCommentAcrossLifecycle(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Todo"}

	var comments []string
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
		upsertProgress: func(_ domain.Issue, body string) error {
			comments = append(comments, body)
			return nil
		},
	}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{root: cfg.Workspace.Root}, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		return len(orch.Snapshot().Completed) == 1
	})

	if len(comments) < 2 {
		t.Fatalf("progress comments = %#v, want at least start and completion updates", comments)
	}
	if !strings.Contains(comments[0], domain.HarnessProgressCommentHeading) || !strings.Contains(comments[0], "issue moved to in-progress") {
		t.Fatalf("first progress comment = %q", comments[0])
	}
	last := comments[len(comments)-1]
	if !strings.Contains(last, "issue moved to done") {
		t.Fatalf("final progress comment missing completion summary: %q", last)
	}
	if !strings.Contains(last, "https://github.example.com/acme/widgets/pull/1") {
		t.Fatalf("final progress comment missing pull request URL: %q", last)
	}
}

func TestOrchestratorIgnoresProgressCommentFailures(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Todo"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
		upsertProgress: func(_ domain.Issue, body string) error {
			return errors.New("comment write failed")
		},
	}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				RefreshedIssue: &issue,
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{root: cfg.Workspace.Root}, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool {
		return len(orch.Snapshot().Completed) == 1
	})

	snapshot := orch.Snapshot()
	if len(snapshot.Completed) != 1 || snapshot.Completed[0] != "ABC-1" {
		t.Fatalf("completed = %#v, want [ABC-1]", snapshot.Completed)
	}
}

func TestReviewOrchestratorMovesIssueToDoneFromVerdict(t *testing.T) {
	cfg := loadReviewTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Review"}

	var transitions []string
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			transitions = append(transitions, stateName)
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			if continueFn != nil {
				t.Fatal("review run should not use continuation")
			}
			if issue.State != "In Review" {
				t.Fatalf("review runner issue state = %q, want In Review", issue.State)
			}
			if !strings.Contains(prompt, "Do not edit code, docs, tests, or workflow files.") {
				t.Fatalf("review prompt missing contract: %q", prompt)
			}
			if err := os.MkdirAll(filepath.Dir(reviewNotesPath(workspace)), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(reviewNotesPath(workspace), []byte("No blocking issues."), 0o644); err != nil {
				t.Fatalf("WriteFile(notes) error = %v", err)
			}
			verdict := reviewVerdict{Decision: reviewDecisionDone, Summary: "Clean", BlockingIssues: []reviewBlockingIssue{}}
			raw, err := json.Marshal(verdict)
			if err != nil {
				t.Fatalf("Marshal(verdict) error = %v", err)
			}
			if err := os.WriteFile(reviewResultPath(workspace), raw, 0o644); err != nil {
				t.Fatalf("WriteFile(verdict) error = %v", err)
			}
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: "review-thread-turn-1", ThreadID: "review-thread", TurnID: "turn-1", TurnCount: 1})
			return codex.RunResult{
				Session: domain.LiveSession{SessionID: "review-thread-turn-1", ThreadID: "review-thread", TurnID: "turn-1", TurnCount: 1},
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner, WithReviewMode())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return atomic.LoadInt32(&workspaceManager.cleanupCalls) > 0 })

	snapshot := orch.Snapshot()
	if len(snapshot.Completed) != 1 || snapshot.Completed[0] != "ABC-1" {
		t.Fatalf("completed = %#v, want [ABC-1]", snapshot.Completed)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "issue_completed") {
		t.Fatalf("recent activity missing issue_completed: %#v", snapshot.RecentActivity)
	}
	if len(transitions) != 1 || transitions[0] != "Done" {
		t.Fatalf("transitions = %#v, want [Done]", transitions)
	}
}

func TestReviewOrchestratorMovesIssueBackToTodoWithoutCleanup(t *testing.T) {
	cfg := loadReviewTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Review"}

	var transitions []string
	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			transitions = append(transitions, stateName)
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			if err := os.MkdirAll(filepath.Dir(reviewNotesPath(workspace)), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(reviewNotesPath(workspace), []byte("Fix the nil dereference."), 0o644); err != nil {
				t.Fatalf("WriteFile(notes) error = %v", err)
			}
			verdict := reviewVerdict{
				Decision: reviewDecisionTodo,
				Summary:  "Blocking issue found",
				BlockingIssues: []reviewBlockingIssue{{
					Title:  "Nil dereference",
					Reason: "Result can be nil",
					File:   "internal/orchestrator/orchestrator.go",
					Line:   1,
				}},
			}
			raw, err := json.Marshal(verdict)
			if err != nil {
				t.Fatalf("Marshal(verdict) error = %v", err)
			}
			if err := os.WriteFile(reviewResultPath(workspace), raw, 0o644); err != nil {
				t.Fatalf("WriteFile(verdict) error = %v", err)
			}
			return codex.RunResult{
				Session: domain.LiveSession{SessionID: "review-thread-turn-1", ThreadID: "review-thread", TurnID: "turn-1", TurnCount: 1},
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner, WithReviewMode())
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
		return containsTimelineReason(snapshot.RecentActivity, "ABC-1", "issue_released", reviewRejectedStopReason)
	})

	snapshot := orch.Snapshot()
	if len(snapshot.Completed) != 0 {
		t.Fatalf("completed = %#v, want empty", snapshot.Completed)
	}
	if snapshot.Counts.Retrying != 0 {
		t.Fatalf("retrying = %d, want 0", snapshot.Counts.Retrying)
	}
	if atomic.LoadInt32(&workspaceManager.cleanupCalls) != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", atomic.LoadInt32(&workspaceManager.cleanupCalls))
	}
	if len(transitions) != 1 || transitions[0] != "Todo" {
		t.Fatalf("transitions = %#v, want [Todo]", transitions)
	}
}

func TestReviewOrchestratorRetriesWhenVerdictMissingAndClearsStaleVerdict(t *testing.T) {
	cfg := loadReviewTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "In Review"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs:     func(_ []string) []domain.Issue { return []domain.Issue{issue} },
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{
		root: cfg.Workspace.Root,
		prepare: func(_ domain.Issue, workspace domain.Workspace) error {
			if err := os.MkdirAll(filepath.Dir(reviewResultPath(workspace)), 0o755); err != nil {
				return err
			}
			return os.WriteFile(reviewResultPath(workspace), []byte(`{"decision":"done","summary":"stale","blocking_issues":[]}`), 0o644)
		},
	}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			return codex.RunResult{
				Session: domain.LiveSession{SessionID: "review-thread-turn-1", ThreadID: "review-thread", TurnID: "turn-1", TurnCount: 1},
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner, WithReviewMode())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return orch.Snapshot().Counts.Retrying == 1 })

	snapshot := orch.Snapshot()
	if snapshot.Retrying[0].Identifier != "ABC-1" {
		t.Fatalf("retrying = %#v", snapshot.Retrying)
	}
	if !containsTimelineEvent(snapshot.RecentActivity, "ABC-1", "attempt_failed") {
		t.Fatalf("recent activity missing attempt_failed: %#v", snapshot.RecentActivity)
	}
	if _, err := os.Stat(filepath.Join(cfg.Workspace.Root, "ABC-1", ".harness", "review-result.json")); !os.IsNotExist(err) {
		t.Fatalf("stale verdict file still exists: %v", err)
	}
}

func TestCodingOrchestratorPromptMentionsReviewNotesWhenPresent(t *testing.T) {
	cfg := loadTestConfig(t)
	issue := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Todo"}

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue { return []domain.Issue{issue} },
		fetchByIDs: func(_ []string) []domain.Issue {
			return []domain.Issue{domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Done"}}
		},
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{
		root: cfg.Workspace.Root,
		prepare: func(_ domain.Issue, workspace domain.Workspace) error {
			if err := os.MkdirAll(filepath.Dir(reviewNotesPath(workspace)), 0o755); err != nil {
				return err
			}
			return os.WriteFile(reviewNotesPath(workspace), []byte("Fix the review issues."), 0o644)
		},
	}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			if !strings.Contains(prompt, ".harness/review-notes.md") {
				t.Fatalf("coding prompt missing review-notes guidance: %q", prompt)
			}
			if !strings.Contains(prompt, "GitHub handoff:") {
				t.Fatalf("coding prompt missing github handoff guidance: %q", prompt)
			}
			return codex.RunResult{
				Session:        domain.LiveSession{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1"},
				RefreshedIssue: &domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example", State: "Done"},
			}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return len(orch.Snapshot().Completed) == 1 })
}

func TestOrchestratorReleasesMissingRunningIssueAndRecoversSlot(t *testing.T) {
	cfg := loadTestConfig(t)
	first := domain.Issue{ID: "1", Identifier: "ABC-1", Title: "First", State: "Todo", Priority: 2, CreatedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC)}
	second := domain.Issue{ID: "2", Identifier: "ABC-2", Title: "Second", State: "Todo", Priority: 1, CreatedAt: time.Date(2026, 3, 18, 8, 0, 0, 0, time.UTC)}

	var pollCount atomic.Int32
	var firstStarted atomic.Bool
	var secondStarted atomic.Bool

	tracker := &fakeTracker{
		pollCandidates: func() []domain.Issue {
			switch pollCount.Add(1) {
			case 1:
				return []domain.Issue{first}
			default:
				return []domain.Issue{second}
			}
		},
		fetchByIDs: func(ids []string) []domain.Issue {
			if slices.Contains(ids, first.ID) {
				return nil
			}
			if slices.Contains(ids, second.ID) {
				return []domain.Issue{second}
			}
			return nil
		},
		transitionState: func(issue domain.Issue, stateName string) (domain.Issue, error) {
			issue.State = stateName
			return issue, nil
		},
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	runner := &fakeRunner{
		run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
			onEvent(codex.Event{Type: "session_started", At: time.Now().UTC(), SessionID: issue.Identifier + "-session", ThreadID: issue.Identifier + "-thread", TurnID: "turn-1"})
			if issue.ID == first.ID {
				firstStarted.Store(true)
				<-ctx.Done()
				return codex.RunResult{}, ctx.Err()
			}
			secondStarted.Store(true)
			return codex.RunResult{RefreshedIssue: &issue}, nil
		},
	}

	orch := newTestOrchestrator(cfg, tracker, workspaceManager, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return secondStarted.Load() })
	waitFor(t, func() bool {
		snapshot := orch.Snapshot()
		return slices.Contains(snapshot.Completed, "ABC-2")
	})

	if !firstStarted.Load() || !secondStarted.Load() {
		t.Fatalf("firstStarted=%v secondStarted=%v, want both true", firstStarted.Load(), secondStarted.Load())
	}
	snapshot := orch.Snapshot()
	if !containsTimelineReason(snapshot.RecentActivity, "ABC-1", "issue_released", "missing_issue") {
		t.Fatalf("recent activity missing released event for missing issue: %#v", snapshot.RecentActivity)
	}
	if snapshot.Counts.Running != 0 {
		t.Fatalf("running = %d, want 0 after second issue completion", snapshot.Counts.Running)
	}
}

func TestDispatchDueRetriesReleasesMissingIssue(t *testing.T) {
	cfg := loadTestConfig(t)
	orch := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{root: cfg.Workspace.Root}, &fakeRunner{})
	st := &state{
		running: map[string]*runningTask{},
		claimed: map[string]struct{}{
			"1": {},
		},
		retryQueue: map[string]domain.RetryEntry{
			"1": {
				IssueID:    "1",
				Identifier: "ABC-1",
				Attempt:    2,
				DueAt:      time.Now().Add(-time.Second),
				Reason:     "attempt_failed",
				LastError:  "boom",
			},
		},
		completed:  map[string]struct{}{},
		history:    map[string][]domain.TimelineEvent{},
		rateLimits: map[string]domain.RateLimitSnapshot{},
	}
	tracker := &fakeTracker{
		fetchByIDs: func([]string) []domain.Issue { return nil },
	}
	orch.tracker = tracker

	orch.dispatchDueRetries(context.Background(), st)

	if _, ok := st.retryQueue["1"]; ok {
		t.Fatalf("retryQueue still contains issue after missing refresh: %#v", st.retryQueue)
	}
	if _, ok := st.claimed["1"]; ok {
		t.Fatalf("claimed still contains issue after missing refresh")
	}
	if !containsTimelineReason(st.recentActivity, "ABC-1", "issue_released", "missing_issue") {
		t.Fatalf("recent activity missing issue_released for missing retry issue: %#v", st.recentActivity)
	}
}

func TestSortDispatchCandidatesByPriorityThenCreatedAtThenIdentifier(t *testing.T) {
	candidates := []domain.Issue{
		{Identifier: "ABC-3", Priority: 0, CreatedAt: time.Date(2026, 3, 18, 11, 0, 0, 0, time.UTC)},
		{Identifier: "ABC-2", Priority: 2, CreatedAt: time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC)},
		{Identifier: "ABC-1", Priority: 1, CreatedAt: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)},
		{Identifier: "ABC-0", Priority: 1, CreatedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC)},
		{Identifier: "ABC-4", Priority: 1, CreatedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC)},
	}

	sortDispatchCandidates(candidates)

	ordered := []string{
		candidates[0].Identifier,
		candidates[1].Identifier,
		candidates[2].Identifier,
		candidates[3].Identifier,
		candidates[4].Identifier,
	}
	if !slices.Equal(ordered, []string{"ABC-0", "ABC-4", "ABC-1", "ABC-2", "ABC-3"}) {
		t.Fatalf("ordered = %#v", ordered)
	}
}

func TestIssueSnapshotReturnsPerIssueHistoryBeyondRecentActivityLimit(t *testing.T) {
	cfg := loadTestConfig(t)
	orch := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{root: cfg.Workspace.Root}, &fakeRunner{})

	history := make([]domain.TimelineEvent, 0, recentActivityLimit+5)
	recent := make([]domain.TimelineEvent, 0, recentActivityLimit)
	for i := 0; i < recentActivityLimit+5; i++ {
		event := domain.TimelineEvent{
			At:         time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second),
			Identifier: "ABC-1",
			Event:      "event-" + time.Duration(i).String(),
		}
		history = append(history, event)
		if i >= 5 {
			recent = append(recent, event)
		}
	}

	st := &state{
		running:    map[string]*runningTask{},
		claimed:    map[string]struct{}{},
		retryQueue: map[string]domain.RetryEntry{},
		completed:  map[string]struct{}{"ABC-1": {}},
		history: map[string][]domain.TimelineEvent{
			"ABC-1": history,
		},
		recentActivity: recent,
		rateLimits:     map[string]domain.RateLimitSnapshot{},
	}

	orch.publishSnapshot(st)
	snapshot, ok := orch.IssueSnapshot("ABC-1")
	if !ok {
		t.Fatal("IssueSnapshot() = not found, want issue history")
	}
	if snapshot.Status != "completed" {
		t.Fatalf("status = %q, want completed", snapshot.Status)
	}
	if len(snapshot.History) != len(history) {
		t.Fatalf("history length = %d, want %d", len(snapshot.History), len(history))
	}
	if snapshot.History[0].Event != history[0].Event {
		t.Fatalf("first history event = %q, want %q", snapshot.History[0].Event, history[0].Event)
	}
}

func TestOrchestratorStartupCleanupRemovesTerminalWorkspaceAndKeepsAuditFiles(t *testing.T) {
	cfg := loadTestConfig(t)
	workspacePath := filepath.Join(cfg.Workspace.Root, "ABC-1")
	historyPath := filepath.Join(cfg.Workspace.Root, ".harness-history", "ABC-1.jsonl")
	promptTranscriptPath := filepath.Join(cfg.Workspace.Root, ".harness-prompts", "ABC-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(history) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(promptTranscriptPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(prompt transcript) error = %v", err)
	}
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	if err := os.WriteFile(historyPath, []byte("event\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(history) error = %v", err)
	}
	if err := os.WriteFile(promptTranscriptPath, []byte("prompt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt transcript) error = %v", err)
	}

	tracker := &fakeTracker{
		pollCandidates:     func() []domain.Issue { return nil },
		pollTerminalIssues: func() []domain.Issue { return []domain.Issue{{ID: "1", Identifier: "ABC-1", State: "Done"}} },
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	orch := newTestOrchestrator(cfg, tracker, workspaceManager, &fakeRunner{run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
		t.Fatal("runner should not start")
		return codex.RunResult{}, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return atomic.LoadInt32(&workspaceManager.cleanupCalls) > 0 })

	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after startup cleanup: %v", err)
	}
	if _, err := os.Stat(historyPath); err != nil {
		t.Fatalf("history file missing after startup cleanup: %v", err)
	}
	if _, err := os.Stat(promptTranscriptPath); err != nil {
		t.Fatalf("prompt transcript missing after startup cleanup: %v", err)
	}
}

func TestOrchestratorStartupCleanupContinuesOnTrackerFailure(t *testing.T) {
	cfg := loadTestConfig(t)
	tracker := &fakeTracker{
		pollCandidates:     func() []domain.Issue { return nil },
		pollTerminalIssues: func() []domain.Issue { panic("unreachable") },
		pollTerminalErr:    os.ErrPermission,
	}

	orch := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{root: cfg.Workspace.Root}, &fakeRunner{run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
		t.Fatal("runner should not start")
		return codex.RunResult{}, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v, want startup cleanup warning only", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return !orch.Snapshot().GeneratedAt.IsZero() })
}

func TestOrchestratorStartupCleanupIgnoresMissingWorkspace(t *testing.T) {
	cfg := loadTestConfig(t)
	tracker := &fakeTracker{
		pollCandidates:     func() []domain.Issue { return nil },
		pollTerminalIssues: func() []domain.Issue { return []domain.Issue{{ID: "1", Identifier: "ABC-404", State: "Done"}} },
	}
	workspaceManager := &fakeWorkspaceManager{root: cfg.Workspace.Root}
	orch := newTestOrchestrator(cfg, tracker, workspaceManager, &fakeRunner{run: func(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
		t.Fatal("runner should not start")
		return codex.RunResult{}, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		_ = orch.Stop(context.Background())
	}()

	waitFor(t, func() bool { return atomic.LoadInt32(&workspaceManager.cleanupCalls) > 0 })
}

type fakeTracker struct {
	pollCandidates     func() []domain.Issue
	pollTerminalIssues func() []domain.Issue
	pollTerminalErr    error
	fetchByIDs         func([]string) []domain.Issue
	transitionState    func(domain.Issue, string) (domain.Issue, error)
	upsertProgress     func(domain.Issue, string) error
}

func (f *fakeTracker) PollCandidates(_ context.Context) ([]domain.Issue, error) {
	if f.pollCandidates == nil {
		return nil, nil
	}
	return f.pollCandidates(), nil
}

func (f *fakeTracker) PollTerminalIssues(_ context.Context) ([]domain.Issue, error) {
	if f.pollTerminalErr != nil {
		return nil, f.pollTerminalErr
	}
	if f.pollTerminalIssues == nil {
		return nil, nil
	}
	return f.pollTerminalIssues(), nil
}

func (f *fakeTracker) FetchByIDs(_ context.Context, ids []string) ([]domain.Issue, error) {
	if f.fetchByIDs == nil {
		return nil, nil
	}
	return f.fetchByIDs(ids), nil
}

func (f *fakeTracker) TransitionState(_ context.Context, issue domain.Issue, stateName string) (domain.Issue, error) {
	if f.transitionState == nil {
		issue.State = stateName
		return issue, nil
	}
	return f.transitionState(issue, stateName)
}

func (f *fakeTracker) UpsertProgressComment(_ context.Context, issue domain.Issue, body string) error {
	if f.upsertProgress == nil {
		return nil
	}
	return f.upsertProgress(issue, body)
}

type fakeWorkspaceManager struct {
	root         string
	cleanupCalls int32
	prepare      func(domain.Issue, domain.Workspace) error
}

func (f *fakeWorkspaceManager) Prepare(_ context.Context, issue domain.Issue) (domain.Workspace, error) {
	path := filepath.Join(f.root, domain.SanitizeWorkspaceKey(issue.Identifier))
	if err := os.MkdirAll(path, 0o755); err != nil {
		return domain.Workspace{}, err
	}
	workspace := domain.Workspace{
		Path:         path,
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
		CreatedNow:   true,
	}
	if f.prepare != nil {
		if err := f.prepare(issue, workspace); err != nil {
			return domain.Workspace{}, err
		}
	}
	return workspace, nil
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

type fakePullRequestCreator struct {
	ensure func(context.Context, domain.Issue, domain.Workspace) (domain.PullRequest, error)
}

func (f *fakePullRequestCreator) EnsurePullRequest(ctx context.Context, issue domain.Issue, workspace domain.Workspace) (domain.PullRequest, error) {
	if f.ensure != nil {
		return f.ensure(ctx, issue, workspace)
	}
	headBranch := issue.BranchName
	if strings.TrimSpace(headBranch) == "" {
		headBranch = "feature/" + issue.Identifier
	}
	return domain.PullRequest{
		Number:     1,
		URL:        "https://github.example.com/acme/widgets/pull/1",
		HeadBranch: headBranch,
		BaseBranch: "main",
		Created:    true,
	}, nil
}

func newTestOrchestrator(cfg config.RuntimeConfig, tracker Tracker, workspaceManager WorkspaceManager, runner Runner, opts ...Option) *Orchestrator {
	tail := append([]Option{
		WithPullRequestCreator(&fakePullRequestCreator{
			ensure: func(_ context.Context, issue domain.Issue, _ domain.Workspace) (domain.PullRequest, error) {
				headBranch := strings.TrimSpace(issue.BranchName)
				if headBranch == "" {
					headBranch = "feature/" + issue.Identifier
				}
				return domain.PullRequest{
					Number:     1,
					URL:        "https://github.example.com/acme/widgets/pull/1",
					HeadBranch: headBranch,
					BaseBranch: cfg.GitHub.BaseBranch,
					Created:    true,
				}, nil
			},
		}),
	}, opts...)
	return New(cfg, tracker, workspaceManager, runner, slog.New(slog.NewTextHandler(os.Stderr, nil)), tail...)
}

func loadTestConfig(t *testing.T) config.RuntimeConfig {
	t.Helper()

	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")
	t.Setenv("GITHUB_TOKEN", "github-token")

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done"]
github:
  token: $GITHUB_TOKEN
  owner: acme
  repo: widgets
  base_branch: main
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

func loadReviewTestConfig(t *testing.T) config.RuntimeConfig {
	t.Helper()

	root := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "linear-token")
	t.Setenv("GITHUB_TOKEN", "github-token")

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: TEST
  active_states: ["In Review"]
  terminal_states: ["Done"]
github:
  token: $GITHUB_TOKEN
  owner: acme
  repo: widgets
  base_branch: main
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
Review {{ issue.identifier }}
`

	path := filepath.Join(root, "REVIEW-WORKFLOW.md")
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

func containsTimelineEvent(events []domain.TimelineEvent, identifier, eventType string) bool {
	for _, event := range events {
		if event.Identifier == identifier && event.Event == eventType {
			return true
		}
	}
	return false
}

func containsTimelineReason(events []domain.TimelineEvent, identifier, eventType, reason string) bool {
	for _, event := range events {
		if event.Identifier == identifier && event.Event == eventType && event.Reason == reason {
			return true
		}
	}
	return false
}
