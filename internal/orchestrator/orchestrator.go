package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-harness/internal/agent"
	"go-harness/internal/config"
	"go-harness/internal/domain"
	"go-harness/internal/workflow"
)

const (
	recentEventLimit       = 50
	timelineEventLimit     = 200
	recentActivityLimit    = 100
	slotRetryDelay         = 1 * time.Second
	baseRetryBackoff       = 5 * time.Second
	progressCommentTimeout = 15 * time.Second
	startStateName         = "In Progress"
	doneStateName          = "Done"
	reviewStateName        = "In Review"
	codingWorkerName       = "coding"
	reviewWorkerName       = "review"
)

type Tracker interface {
	PollCandidates(ctx context.Context) ([]domain.Issue, error)
	PollTerminalIssues(ctx context.Context) ([]domain.Issue, error)
	FetchByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
	TransitionState(ctx context.Context, issue domain.Issue, stateName string) (domain.Issue, error)
	UpsertProgressComment(ctx context.Context, issue domain.Issue, body string) error
}

type WorkspaceManager interface {
	Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error)
	AfterRun(ctx context.Context, workspace domain.Workspace) error
	Cleanup(ctx context.Context, workspace domain.Workspace) error
}

type Runner interface {
	RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(agent.Event), continueFn agent.ContinueFunc) (agent.RunResult, error)
}

type PullRequestCreator interface {
	EnsurePullRequest(ctx context.Context, issue domain.Issue, workspace domain.Workspace) (domain.PullRequest, error)
}

type ConfigSource interface {
	Current() config.RuntimeConfig
	ReloadIfChanged() (config.RuntimeConfig, bool, error)
	DispatchValidationError() error
}

type Option func(*Orchestrator)

type Orchestrator struct {
	cfg        config.RuntimeConfig
	tracker    Tracker
	workspaces WorkspaceManager
	runner     Runner
	pullReqs   PullRequestCreator
	logger     *slog.Logger
	configs    ConfigSource
	lastBlock  string
	workerName string
	reviewMode bool

	controlCh chan struct{}
	eventCh   chan any
	doneCh    chan struct{}

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	snapshot atomic.Value
	history  atomic.Value
}

type runningTask struct {
	issue         domain.Issue
	attempt       int
	workspace     domain.Workspace
	startedAt     time.Time
	liveSession   *domain.LiveSession
	recentEvents  []domain.RecentEvent
	lastError     string
	cancel        context.CancelFunc
	cleanupOnExit bool
	stopReason    string
}

type state struct {
	running        map[string]*runningTask
	claimed        map[string]struct{}
	retryQueue     map[string]domain.RetryEntry
	completed      map[string]struct{}
	history        map[string][]domain.TimelineEvent
	recentActivity []domain.TimelineEvent
	totals         domain.RuntimeTotals
	rateLimits     map[string]domain.RateLimitSnapshot
}

type workerEvent struct {
	IssueID string
	Event   agent.Event
}

type workerExit struct {
	Issue     domain.Issue
	Attempt   int
	Workspace domain.Workspace
	Result    agent.RunResult
	Refreshed *domain.Issue
	Err       error
}

type workerIssueUpdate struct {
	IssueID string
	Issue   domain.Issue
	Message string
}

type timelineUpdate struct {
	IssueID      string
	Identifier   string
	Workspace    domain.Workspace
	Event        domain.TimelineEvent
	ApplyToIssue *domain.Issue
}

type progressCommentUpdate struct {
	Attempt        int
	Status         string
	Summary        string
	Worker         string
	UpdatedAt      time.Time
	NextRetryAt    time.Time
	LastError      string
	PullRequestURL string
}

type cleanupResult struct {
	Issue     domain.Issue
	Workspace domain.Workspace
	Err       error
}

func WithConfigSource(source ConfigSource) Option {
	return func(orch *Orchestrator) {
		orch.configs = source
	}
}

func WithWorkerName(worker string) Option {
	return func(orch *Orchestrator) {
		if strings.TrimSpace(worker) != "" {
			orch.workerName = strings.TrimSpace(worker)
		}
	}
}

func WithReviewMode() Option {
	return func(orch *Orchestrator) {
		orch.reviewMode = true
		if strings.TrimSpace(orch.workerName) == "" {
			orch.workerName = reviewWorkerName
		}
	}
}

func WithPullRequestCreator(creator PullRequestCreator) Option {
	return func(orch *Orchestrator) {
		orch.pullReqs = creator
	}
}

func New(cfg config.RuntimeConfig, tracker Tracker, workspaces WorkspaceManager, runner Runner, logger *slog.Logger, opts ...Option) *Orchestrator {
	orch := &Orchestrator{
		cfg:        cfg,
		tracker:    tracker,
		workspaces: workspaces,
		runner:     runner,
		logger:     logger,
		controlCh:  make(chan struct{}, 1),
		eventCh:    make(chan any, 128),
		doneCh:     make(chan struct{}),
		workerName: codingWorkerName,
	}
	for _, opt := range opts {
		opt(orch)
	}
	orch.snapshot.Store(domain.StateSnapshot{
		GeneratedAt: time.Now().UTC(),
		Workflow:    domain.WorkflowStatus{Path: cfg.SourcePath},
		Environment: environmentStatus(cfg),
	})
	orch.history.Store(map[string][]domain.TimelineEvent{})
	return orch
}

func (o *Orchestrator) Start(ctx context.Context) error {
	if o.cancel != nil {
		return errors.New("orchestrator already started")
	}

	runCtx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	o.wg.Add(1)
	go o.loop(runCtx)
	return nil
}

func (o *Orchestrator) Stop(ctx context.Context) error {
	if o.cancel == nil {
		return nil
	}
	o.cancel()

	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) TriggerRefresh() {
	select {
	case o.controlCh <- struct{}{}:
	default:
	}
}

func (o *Orchestrator) Snapshot() domain.StateSnapshot {
	value, _ := o.snapshot.Load().(domain.StateSnapshot)
	return value
}

func (o *Orchestrator) IssueSnapshot(identifier string) (domain.IssueRuntimeSnapshot, bool) {
	snapshot := o.Snapshot()
	history := o.issueHistory(identifier)
	transcript := o.issueTranscript(identifier)
	for _, running := range snapshot.Running {
		if running.Issue.Identifier == identifier {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt:      snapshot.GeneratedAt,
				Identifier:       identifier,
				Status:           "running",
				Running:          &running,
				History:          history,
				PromptTranscript: transcript,
			}, true
		}
	}
	for _, retry := range snapshot.Retrying {
		if retry.Identifier == identifier {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt:      snapshot.GeneratedAt,
				Identifier:       identifier,
				Status:           "retrying",
				Retry:            &retry,
				History:          history,
				PromptTranscript: transcript,
			}, true
		}
	}
	if slices.Contains(snapshot.Completed, identifier) {
		return domain.IssueRuntimeSnapshot{
			GeneratedAt:      snapshot.GeneratedAt,
			Identifier:       identifier,
			Status:           "completed",
			History:          history,
			PromptTranscript: transcript,
			Completed:        true,
		}, true
	}
	if len(history) > 0 || len(transcript) > 0 {
		return domain.IssueRuntimeSnapshot{
			GeneratedAt:      snapshot.GeneratedAt,
			Identifier:       identifier,
			Status:           "observed",
			History:          history,
			PromptTranscript: transcript,
		}, true
	}
	return domain.IssueRuntimeSnapshot{}, false
}

func (o *Orchestrator) issueTranscript(identifier string) []domain.PromptTranscriptEntry {
	path := agent.TranscriptPath(o.cfg.Workspace.Root, identifier)
	if path == "" {
		return nil
	}
	entries, err := agent.ReadTranscript(path, 80)
	if err != nil {
		o.logger.Warn("prompt transcript read failed",
			slog.String("issue", identifier),
			slog.String("path", path),
			slog.Any("error", err),
		)
		return nil
	}
	return entries
}

func (o *Orchestrator) loop(ctx context.Context) {
	defer o.wg.Done()
	defer close(o.doneCh)

	st := &state{
		running:    map[string]*runningTask{},
		claimed:    map[string]struct{}{},
		retryQueue: map[string]domain.RetryEntry{},
		completed:  map[string]struct{}{},
		history:    map[string][]domain.TimelineEvent{},
		rateLimits: map[string]domain.RateLimitSnapshot{},
	}

	ticker := time.NewTicker(o.cfg.Polling.Interval)
	defer ticker.Stop()

	o.maybeReloadConfig(ticker)
	if !o.reviewMode {
		o.startupCleanup(ctx)
	}
	o.handleTick(ctx, st)
	o.publishSnapshot(st)

	for {
		select {
		case <-ctx.Done():
			o.logger.Info("orchestrator stopping",
				slog.Int("running", len(st.running)),
				slog.Int("retrying", len(st.retryQueue)),
			)
			for _, entry := range st.running {
				entry.cancel()
			}
			return
		case <-ticker.C:
			o.maybeReloadConfig(ticker)
			o.handleTick(ctx, st)
			o.publishSnapshot(st)
		case <-o.controlCh:
			o.maybeReloadConfig(ticker)
			o.handleTick(ctx, st)
			o.publishSnapshot(st)
		case raw := <-o.eventCh:
			switch event := raw.(type) {
			case workerEvent:
				o.applyWorkerEvent(st, event)
			case workerIssueUpdate:
				o.applyWorkerIssueUpdate(st, event)
			case timelineUpdate:
				o.applyTimelineUpdate(st, event)
			case cleanupResult:
				o.applyCleanupResult(st, event)
			case workerExit:
				o.applyWorkerExit(ctx, st, event)
			}
			o.publishSnapshot(st)
		}
	}
}

func (o *Orchestrator) handleTick(ctx context.Context, st *state) {
	o.logger.Debug("orchestrator tick",
		slog.Int("running", len(st.running)),
		slog.Int("retrying", len(st.retryQueue)),
		slog.Int("claimed", len(st.claimed)),
	)
	o.reconcileRunning(ctx, st)
	if err := o.dispatchValidationError(); err != nil {
		if message := err.Error(); message != o.lastBlock {
			o.logger.Warn("dispatch blocked due to invalid workflow reload", slog.Any("error", err))
			o.lastBlock = message
		}
		return
	}
	if o.lastBlock != "" {
		o.logger.Info("dispatch unblocked after valid workflow reload")
		o.lastBlock = ""
	}
	o.dispatchDueRetries(ctx, st)

	candidates, err := o.tracker.PollCandidates(ctx)
	if err != nil {
		o.logger.Error("candidate poll failed", slog.Any("error", err))
		return
	}
	sortDispatchCandidates(candidates)
	o.logger.Debug("candidate poll completed", slog.Int("candidates", len(candidates)))
	if len(candidates) == 0 {
		o.logger.Info("candidate poll returned no issues",
			slog.String("project", o.cfg.Tracker.ProjectSlug),
			slog.Any("active_states", o.cfg.Tracker.ActiveStates),
		)
	}

	for _, issue := range candidates {
		if reason := o.dispatchSkipReason(issue, st); reason != "" {
			o.logger.Info("candidate skipped",
				slog.String("issue", issue.Identifier),
				slog.String("state", issue.State),
				slog.String("reason", reason),
			)
			continue
		}
		o.dispatch(ctx, st, issue, 1)
	}
}

func (o *Orchestrator) reconcileRunning(ctx context.Context, st *state) {
	if len(st.running) == 0 {
		return
	}

	ids := make([]string, 0, len(st.running))
	for issueID := range st.running {
		ids = append(ids, issueID)
	}
	issues, err := o.tracker.FetchByIDs(ctx, ids)
	if err != nil {
		o.logger.Error("running issue refresh failed", slog.Any("error", err))
		return
	}

	issueByID := make(map[string]domain.Issue, len(issues))
	for _, issue := range issues {
		issueByID[issue.ID] = issue
	}

	for _, issueID := range ids {
		entry := st.running[issueID]
		if entry == nil {
			continue
		}
		issue, ok := issueByID[issueID]
		if !ok {
			if entry.stopReason != "missing_issue" {
				entry.stopReason = "missing_issue"
				o.recordTimeline(st, entry.issue, entry.workspace, domain.TimelineEvent{
					At:           time.Now().UTC(),
					IssueID:      entry.issue.ID,
					Identifier:   entry.issue.Identifier,
					Attempt:      entry.attempt,
					Event:        "issue_cancelled",
					Status:       "cancelled",
					Reason:       "missing_issue",
					WorkspaceKey: entry.workspace.WorkspaceKey,
					Workspace:    entry.workspace.Path,
					Message:      "issue left the running set because it was missing from tracker refresh",
				})
				o.logger.Info("running issue missing from tracker refresh",
					slog.String("issue", entry.issue.Identifier),
				)
			}
			entry.cancel()
			continue
		}

		if o.cfg.IsTerminalState(issue.State) {
			entry.issue = issue
			entry.cleanupOnExit = true
			entry.stopReason = "terminal_state"
			o.recordTimeline(st, issue, entry.workspace, domain.TimelineEvent{
				At:           time.Now().UTC(),
				IssueID:      issue.ID,
				Identifier:   issue.Identifier,
				Attempt:      entry.attempt,
				Event:        "issue_cancelled",
				Status:       "cancelled",
				Reason:       "terminal_state",
				StateAfter:   issue.State,
				WorkspaceKey: entry.workspace.WorkspaceKey,
				Workspace:    entry.workspace.Path,
				Message:      "issue left running set because it reached a terminal state",
			})
			o.logger.Info("running issue transitioned to terminal state",
				slog.String("issue", issue.Identifier),
				slog.String("state", issue.State),
			)
			entry.cancel()
			continue
		}

		if o.cfg.IsActiveState(issue.State) {
			entry.issue = issue
			continue
		}

		entry.issue = issue
		entry.cleanupOnExit = false
		entry.stopReason = "non_active_state"
		o.recordTimeline(st, issue, entry.workspace, domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			Attempt:      entry.attempt,
			Event:        "issue_cancelled",
			Status:       "cancelled",
			Reason:       "non_active_state",
			StateAfter:   issue.State,
			WorkspaceKey: entry.workspace.WorkspaceKey,
			Workspace:    entry.workspace.Path,
			Message:      "issue left active states while the run was in progress",
		})
		o.logger.Info("running issue left active states",
			slog.String("issue", issue.Identifier),
			slog.String("state", issue.State),
		)
		entry.cancel()
	}
}

func (o *Orchestrator) dispatchDueRetries(ctx context.Context, st *state) {
	if len(st.retryQueue) == 0 {
		return
	}

	now := time.Now().UTC()
	dueIDs := make([]string, 0, len(st.retryQueue))
	for issueID, retry := range st.retryQueue {
		if !retry.DueAt.After(now) {
			dueIDs = append(dueIDs, issueID)
		}
	}
	if len(dueIDs) == 0 {
		return
	}

	issues, err := o.tracker.FetchByIDs(ctx, dueIDs)
	if err != nil {
		o.logger.Error("retry refresh failed", slog.Any("error", err))
		return
	}

	issueByID := make(map[string]domain.Issue, len(issues))
	for _, issue := range issues {
		issueByID[issue.ID] = issue
	}

	for _, issueID := range dueIDs {
		retry := st.retryQueue[issueID]
		issue, ok := issueByID[issueID]
		if !ok {
			delete(st.retryQueue, issueID)
			delete(st.claimed, issueID)
			o.recordTimeline(st, domain.Issue{ID: issueID, Identifier: retry.Identifier}, domain.Workspace{
				Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(retry.Identifier)),
				WorkspaceKey: domain.SanitizeWorkspaceKey(retry.Identifier),
			}, domain.TimelineEvent{
				At:           now,
				IssueID:      issueID,
				Identifier:   retry.Identifier,
				Attempt:      retry.Attempt,
				Event:        "issue_released",
				Status:       "released",
				Reason:       "missing_issue",
				WorkspaceKey: domain.SanitizeWorkspaceKey(retry.Identifier),
				Workspace:    filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(retry.Identifier)),
				LastError:    retry.LastError,
				Message:      "retry entry released because the issue was missing from tracker refresh",
			})
			continue
		}

		if o.cfg.IsTerminalState(issue.State) {
			delete(st.retryQueue, issueID)
			delete(st.claimed, issueID)
			st.completed[issue.Identifier] = struct{}{}
			o.recordTimeline(st, issue, domain.Workspace{
				Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
				WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
			}, domain.TimelineEvent{
				At:           now,
				IssueID:      issue.ID,
				Identifier:   issue.Identifier,
				Attempt:      retry.Attempt,
				Event:        "issue_completed",
				Status:       "completed",
				Reason:       "terminal_state_detected_during_retry_reconcile",
				StateAfter:   issue.State,
				WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
				Workspace:    filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
				Message:      "retry entry resolved because the issue was already terminal",
			})
			o.asyncCleanup(issue, domain.Workspace{
				Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
				WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
			})
			continue
		}
		if !o.cfg.IsActiveState(issue.State) {
			delete(st.retryQueue, issueID)
			delete(st.claimed, issueID)
			continue
		}
		if !o.hasAvailableSlot(issue.State, st) {
			retry.DueAt = now.Add(slotRetryDelay)
			st.retryQueue[issueID] = retry
			continue
		}

		delete(st.retryQueue, issueID)
		o.dispatch(ctx, st, issue, retry.Attempt)
	}
}

func (o *Orchestrator) shouldDispatch(issue domain.Issue, st *state) bool {
	return o.dispatchSkipReason(issue, st) == ""
}

func (o *Orchestrator) dispatchSkipReason(issue domain.Issue, st *state) string {
	if issue.ID == "" || issue.Identifier == "" {
		return "missing_identity"
	}
	if !o.cfg.IsActiveState(issue.State) || o.cfg.IsTerminalState(issue.State) {
		return "inactive_or_terminal_state"
	}
	if _, ok := st.claimed[issue.ID]; ok {
		return "already_claimed"
	}
	if _, ok := st.running[issue.ID]; ok {
		return "already_running"
	}
	if _, ok := st.retryQueue[issue.ID]; ok {
		return "already_retrying"
	}
	if !o.hasAvailableSlot(issue.State, st) {
		return "no_available_slot"
	}
	if domain.NormalizeState(issue.State) == "todo" && hasActiveBlocker(issue, o.cfg) {
		return "blocked_by_active_issue"
	}
	return ""
}

func (o *Orchestrator) hasAvailableSlot(stateName string, st *state) bool {
	if len(st.running) >= o.cfg.Agent.MaxConcurrentAgents {
		return false
	}

	limit := o.cfg.MaxConcurrentForState(stateName)
	count := 0
	for _, entry := range st.running {
		if domain.NormalizeState(entry.issue.State) == domain.NormalizeState(stateName) {
			count++
		}
	}
	return count < limit
}

func (o *Orchestrator) dispatch(ctx context.Context, st *state, issue domain.Issue, attempt int) {
	runCtx, cancel := context.WithCancel(ctx)
	workspace := domain.Workspace{
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
		Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
	}

	st.claimed[issue.ID] = struct{}{}
	st.running[issue.ID] = &runningTask{
		issue:        issue,
		attempt:      attempt,
		workspace:    workspace,
		startedAt:    time.Now().UTC(),
		recentEvents: make([]domain.RecentEvent, 0, 8),
		cancel:       cancel,
	}
	o.recordTimeline(st, issue, workspace, domain.TimelineEvent{
		At:           time.Now().UTC(),
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		Attempt:      attempt,
		Event:        "issue_claimed",
		Status:       "running",
		StateBefore:  issue.State,
		WorkspaceKey: workspace.WorkspaceKey,
		Workspace:    workspace.Path,
		Message:      "issue claimed for execution",
	})
	o.logger.Info("dispatching issue",
		slog.String("issue", issue.Identifier),
		slog.String("state", issue.State),
		slog.Int("attempt", attempt),
		slog.String("workspace", workspace.Path),
	)

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		o.executeAttempt(runCtx, issue, attempt)
	}()
}

func (o *Orchestrator) executeAttempt(ctx context.Context, issue domain.Issue, attempt int) {
	workspace, err := o.workspaces.Prepare(ctx, issue)
	if err != nil {
		o.pushEvent(timelineUpdate{
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Workspace: domain.Workspace{
				WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
				Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
			},
			Event: domain.TimelineEvent{
				At:           time.Now().UTC(),
				IssueID:      issue.ID,
				Identifier:   issue.Identifier,
				Attempt:      attempt,
				Event:        "workspace_prepare_failed",
				Status:       "error",
				Reason:       "workspace_prepare_failed",
				WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
				Workspace:    filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
				LastError:    err.Error(),
				Message:      "workspace preparation failed before the run could start",
			},
		})
		o.pushEvent(workerExit{Issue: issue, Attempt: attempt, Workspace: domain.Workspace{WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier), Path: filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier))}, Err: err})
		return
	}
	o.logger.Info("workspace prepared",
		slog.String("issue", issue.Identifier),
		slog.Int("attempt", attempt),
		slog.String("workspace_path", workspace.Path),
		slog.Bool("created_now", workspace.CreatedNow),
	)
	o.pushEvent(timelineUpdate{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Workspace:  workspace,
		Event: domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			Attempt:      attempt,
			Event:        "workspace_prepared",
			Status:       "running",
			StateBefore:  issue.State,
			WorkspaceKey: workspace.WorkspaceKey,
			Workspace:    workspace.Path,
			Message:      "workspace prepared and hooks completed",
		},
	})

	if o.reviewMode {
		if err := prepareReviewArtifacts(workspace); err != nil {
			o.pushEvent(timelineUpdate{
				IssueID:    issue.ID,
				Identifier: issue.Identifier,
				Workspace:  workspace,
				Event: domain.TimelineEvent{
					At:           time.Now().UTC(),
					IssueID:      issue.ID,
					Identifier:   issue.Identifier,
					Attempt:      attempt,
					Event:        "review_artifacts_prepare_failed",
					Status:       "error",
					Reason:       "review_artifacts_prepare_failed",
					StateAfter:   issue.State,
					WorkspaceKey: workspace.WorkspaceKey,
					Workspace:    workspace.Path,
					LastError:    err.Error(),
					Message:      "review artifacts could not be prepared before the run started",
				},
			})
			_ = o.workspaces.AfterRun(context.Background(), workspace)
			o.pushEvent(workerExit{Issue: issue, Attempt: attempt, Workspace: workspace, Err: err})
			return
		}
	}

	runIssue, err := o.startIssueForAttempt(ctx, issue, attempt)
	if err != nil {
		_ = o.workspaces.AfterRun(context.Background(), workspace)
		o.pushEvent(workerExit{Issue: issue, Attempt: attempt, Workspace: workspace, Err: err})
		return
	}

	prompt, err := workflow.RenderPrompt(o.cfg.PromptTemplate, runIssue, attempt)
	if err != nil {
		o.pushEvent(timelineUpdate{
			IssueID:    runIssue.ID,
			Identifier: runIssue.Identifier,
			Workspace:  workspace,
			Event: domain.TimelineEvent{
				At:           time.Now().UTC(),
				IssueID:      runIssue.ID,
				Identifier:   runIssue.Identifier,
				Attempt:      attempt,
				Event:        "prompt_render_failed",
				Status:       "error",
				Reason:       "prompt_render_failed",
				StateAfter:   runIssue.State,
				WorkspaceKey: workspace.WorkspaceKey,
				Workspace:    workspace.Path,
				LastError:    err.Error(),
				Message:      "workflow prompt rendering failed",
			},
		})
		_ = o.workspaces.AfterRun(context.Background(), workspace)
		o.pushEvent(workerExit{Issue: runIssue, Attempt: attempt, Workspace: workspace, Err: err})
		return
	}
	prompt = o.augmentPrompt(prompt, workspace)

	var continueFn agent.ContinueFunc
	if !o.reviewMode {
		continueFn = func(runCtx context.Context, session domain.LiveSession) (agent.ContinueDecision, error) {
			refreshed, err := o.refreshIssue(runCtx, runIssue.ID)
			if err != nil {
				return agent.ContinueDecision{}, err
			}
			if refreshed == nil {
				return agent.ContinueDecision{Continue: false}, nil
			}
			if o.cfg.IsTerminalState(refreshed.State) || !o.cfg.IsActiveState(refreshed.State) {
				return agent.ContinueDecision{
					Continue:       false,
					RefreshedIssue: refreshed,
				}, nil
			}
			if session.TurnCount >= o.cfg.Agent.MaxTurns {
				return agent.ContinueDecision{
					Continue:       false,
					StopReason:     "max_turns_reached",
					RefreshedIssue: refreshed,
				}, nil
			}
			return agent.ContinueDecision{
				Continue:       true,
				NextPrompt:     workflow.RenderContinuationPrompt(*refreshed, session.TurnCount+1),
				RefreshedIssue: refreshed,
			}, nil
		}
	}

	result, err := o.runner.RunAttempt(ctx, runIssue, workspace, prompt, attempt, func(event agent.Event) {
		o.pushEvent(workerEvent{IssueID: runIssue.ID, Event: event})
	}, continueFn)

	// In review mode, require consensus: both agents must review and both must approve
	// before the issue moves to done. The second verdict file is left in place for
	// completeIssueIfNeeded to consume via the normal loadReviewVerdict path.
	if err == nil && o.reviewMode {
		firstVerdict, peekErr := peekReviewVerdict(workspace)
		if peekErr == nil && validateReviewVerdict(firstVerdict) == nil && validateReviewNotes(workspace) == nil {
			o.pushEvent(timelineUpdate{
				IssueID:    runIssue.ID,
				Identifier: runIssue.Identifier,
				Workspace:  workspace,
				Event: domain.TimelineEvent{
					At:           time.Now().UTC(),
					IssueID:      runIssue.ID,
					Identifier:   runIssue.Identifier,
					Attempt:      attempt,
					Event:        "review_second_pass_started",
					Status:       "running",
					WorkspaceKey: workspace.WorkspaceKey,
					Workspace:    workspace.Path,
					Message:      "first review agent completed; starting second review agent for consensus",
				},
			})
			if prepErr := prepareReviewArtifacts(workspace); prepErr != nil {
				err = prepErr
			} else {
				result2, err2 := o.runner.RunAttempt(ctx, runIssue, workspace, prompt, attempt, func(event agent.Event) {
					o.pushEvent(workerEvent{IssueID: runIssue.ID, Event: event})
				}, nil)
				if err2 != nil {
					err = err2
				} else {
					result = result2
					// Enforce consensus: if the first agent rejected, the final verdict
					// must remain "todo" even if the second agent approved.
					if firstVerdict.Decision == reviewDecisionTodo {
						if secondVerdict, peekErr2 := peekReviewVerdict(workspace); peekErr2 == nil &&
							secondVerdict.Decision == reviewDecisionDone {
							err = writeConsensusFailureVerdict(workspace, firstVerdict)
						}
					}
				}
			}
		}
	}

	afterRunErr := o.workspaces.AfterRun(context.Background(), workspace)
	if err == nil && afterRunErr != nil {
		o.pushEvent(timelineUpdate{
			IssueID:    runIssue.ID,
			Identifier: runIssue.Identifier,
			Workspace:  workspace,
			Event: domain.TimelineEvent{
				At:           time.Now().UTC(),
				IssueID:      runIssue.ID,
				Identifier:   runIssue.Identifier,
				Attempt:      attempt,
				Event:        "after_run_failed",
				Status:       "error",
				Reason:       "after_run_failed",
				StateAfter:   runIssue.State,
				WorkspaceKey: workspace.WorkspaceKey,
				Workspace:    workspace.Path,
				LastError:    afterRunErr.Error(),
				Message:      "after_run hook failed after the runner exited",
			},
		})
		err = afterRunErr
	}
	if err == nil {
		completedIssue, stopReason, transitionErr := o.completeIssueIfNeeded(ctx, runIssue, workspace, attempt, result)
		if transitionErr != nil {
			err = transitionErr
		} else if completedIssue != nil {
			result.RefreshedIssue = completedIssue
		}
		if stopReason != "" {
			result.StopReason = stopReason
		}
	}

	o.pushEvent(workerExit{
		Issue:     runIssue,
		Attempt:   attempt,
		Workspace: workspace,
		Result:    result,
		Refreshed: result.RefreshedIssue,
		Err:       err,
	})
}

func (o *Orchestrator) startIssueForAttempt(ctx context.Context, issue domain.Issue, attempt int) (domain.Issue, error) {
	if o.reviewMode {
		o.syncProgressComment(ctx, issue, progressCommentUpdate{
			Attempt: attempt,
			Status:  "running",
			Summary: "review attempt started",
		})
		return issue, nil
	}
	started, err := o.transitionIssueState(ctx, issue, startStateName, "issue moved to in-progress")
	if err != nil {
		return issue, err
	}
	o.syncProgressComment(ctx, started, progressCommentUpdate{
		Attempt: attempt,
		Status:  "running",
		Summary: "issue moved to in-progress",
	})
	return started, nil
}

func (o *Orchestrator) augmentPrompt(prompt string, workspace domain.Workspace) string {
	if o.reviewMode {
		return appendReviewPromptContract(prompt, workspace)
	}
	prompt = appendCodingReviewNotesGuidance(prompt, workspace)
	return appendGitHubPRGuidance(prompt, o.cfg.GitHub)
}

func (o *Orchestrator) transitionIssueState(ctx context.Context, issue domain.Issue, targetState, message string) (domain.Issue, error) {
	if strings.EqualFold(strings.TrimSpace(issue.State), targetState) {
		return issue, nil
	}
	transitioned, err := o.tracker.TransitionState(ctx, issue, targetState)
	if err != nil {
		return issue, err
	}
	o.pushEvent(workerIssueUpdate{
		IssueID: issue.ID,
		Issue:   transitioned,
		Message: message,
	})
	o.pushEvent(timelineUpdate{
		IssueID:    issue.ID,
		Identifier: transitioned.Identifier,
		Workspace: domain.Workspace{
			WorkspaceKey: domain.SanitizeWorkspaceKey(transitioned.Identifier),
			Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(transitioned.Identifier)),
		},
		Event: domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      transitioned.ID,
			Identifier:   transitioned.Identifier,
			Event:        "tracker_state_transition",
			Status:       "running",
			Message:      message,
			StateBefore:  issue.State,
			StateAfter:   transitioned.State,
			WorkspaceKey: domain.SanitizeWorkspaceKey(transitioned.Identifier),
			Workspace:    filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(transitioned.Identifier)),
		},
		ApplyToIssue: &transitioned,
	})
	o.logger.Info("tracker state transitioned",
		slog.String("issue", transitioned.Identifier),
		slog.String("from", issue.State),
		slog.String("to", transitioned.State),
	)
	return transitioned, nil
}

func (o *Orchestrator) completeIssueIfNeeded(ctx context.Context, issue domain.Issue, workspace domain.Workspace, attempt int, result agent.RunResult) (*domain.Issue, string, error) {
	if o.reviewMode {
		verdict, err := loadReviewVerdict(workspace)
		if err != nil {
			return nil, "", err
		}
		switch verdict.Decision {
		case reviewDecisionDone:
			pullRequest, err := o.ensurePullRequest(ctx, coalesceIssue(result.RefreshedIssue, issue), workspace, attempt)
			if err != nil {
				return nil, "", err
			}
			completed, err := o.transitionIssueState(ctx, coalesceIssue(result.RefreshedIssue, issue), doneStateName, "review accepted and issue moved to done")
			if err != nil {
				return nil, "", err
			}
			o.syncProgressComment(ctx, completed, progressCommentUpdate{
				Attempt:        attempt,
				Status:         "completed",
				Summary:        "review accepted and issue moved to done",
				PullRequestURL: pullRequest.URL,
			})
			return &completed, "", nil
		case reviewDecisionTodo:
			reopened, err := o.transitionIssueState(ctx, coalesceIssue(result.RefreshedIssue, issue), "Todo", "review found blocking issues and moved issue back to todo")
			if err != nil {
				return nil, "", err
			}
			o.syncProgressComment(ctx, reopened, progressCommentUpdate{
				Attempt: attempt,
				Status:  "released",
				Summary: "review found blocking issues and moved issue back to todo",
			})
			return &reopened, reviewRejectedStopReason, nil
		default:
			return nil, "", fmt.Errorf("unsupported review decision %q", verdict.Decision)
		}
	}
	if result.StopReason == "max_turns_reached" {
		handoffIssue := coalesceIssue(result.RefreshedIssue, issue)
		if o.cfg.IsActiveState(handoffIssue.State) && !o.cfg.IsTerminalState(handoffIssue.State) {
			handoff, err := o.transitionIssueState(ctx, handoffIssue, reviewStateName, "issue moved to in-review after max turns reached")
			if err != nil {
				return nil, "", err
			}
			o.syncProgressComment(ctx, handoff, progressCommentUpdate{
				Attempt: attempt,
				Status:  "handoff",
				Summary: "issue moved to in-review after max turns reached",
			})
			return &handoff, "", nil
		}
		return result.RefreshedIssue, "", nil
	}
	if result.StopReason != "" {
		return result.RefreshedIssue, "", nil
	}
	if result.RefreshedIssue != nil {
		switch {
		case o.cfg.IsTerminalState(result.RefreshedIssue.State):
			return result.RefreshedIssue, "", nil
		case !o.cfg.IsActiveState(result.RefreshedIssue.State):
			return result.RefreshedIssue, "", nil
		}
	}

	pullRequest, err := o.ensurePullRequest(ctx, coalesceIssue(result.RefreshedIssue, issue), workspace, attempt)
	if err != nil {
		return nil, "", err
	}
	completed, err := o.transitionIssueState(ctx, coalesceIssue(result.RefreshedIssue, issue), doneStateName, "issue moved to done")
	if err != nil {
		return nil, "", err
	}
	o.syncProgressComment(ctx, completed, progressCommentUpdate{
		Attempt:        attempt,
		Status:         "completed",
		Summary:        "issue moved to done",
		PullRequestURL: pullRequest.URL,
	})
	return &completed, "", nil
}

func (o *Orchestrator) ensurePullRequest(ctx context.Context, issue domain.Issue, workspace domain.Workspace, attempt int) (domain.PullRequest, error) {
	if o.pullReqs == nil {
		return domain.PullRequest{}, fmt.Errorf("pull request creator is not configured")
	}
	pullRequest, err := o.pullReqs.EnsurePullRequest(ctx, issue, workspace)
	if err != nil {
		o.pushEvent(timelineUpdate{
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Workspace:  workspace,
			Event: domain.TimelineEvent{
				At:           time.Now().UTC(),
				IssueID:      issue.ID,
				Identifier:   issue.Identifier,
				Attempt:      attempt,
				Event:        "github_pull_request_failed",
				Status:       "error",
				Reason:       "github_pull_request_failed",
				StateAfter:   issue.State,
				WorkspaceKey: workspace.WorkspaceKey,
				Workspace:    workspace.Path,
				LastError:    err.Error(),
				Message:      "github pull request creation failed before the issue could move to done",
			},
		})
		return domain.PullRequest{}, err
	}

	message := "github pull request is ready"
	if strings.TrimSpace(pullRequest.URL) != "" {
		message = "github pull request is ready: " + pullRequest.URL
	}
	reason := "github_pull_request_reused"
	if pullRequest.Created {
		reason = "github_pull_request_created"
	}
	o.pushEvent(timelineUpdate{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Workspace:  workspace,
		Event: domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			Attempt:      attempt,
			Event:        "github_pull_request_ready",
			Status:       "running",
			Reason:       reason,
			StateAfter:   issue.State,
			WorkspaceKey: workspace.WorkspaceKey,
			Workspace:    workspace.Path,
			Message:      message,
		},
	})
	o.logger.Info("github pull request ready",
		slog.String("issue", issue.Identifier),
		slog.Bool("created", pullRequest.Created),
		slog.String("head_branch", pullRequest.HeadBranch),
		slog.String("base_branch", pullRequest.BaseBranch),
		slog.String("url", pullRequest.URL),
	)
	return pullRequest, nil
}

func (o *Orchestrator) applyWorkerEvent(st *state, event workerEvent) {
	entry := st.running[event.IssueID]
	if entry == nil {
		return
	}

	recent := domain.RecentEvent{
		At:             event.Event.At,
		Event:          event.Event.Type,
		Message:        event.Event.Message,
		PayloadSummary: event.Event.PayloadSummary,
	}
	entry.recentEvents = append(entry.recentEvents, recent)
	if len(entry.recentEvents) > recentEventLimit {
		entry.recentEvents = entry.recentEvents[len(entry.recentEvents)-recentEventLimit:]
	}

	if entry.liveSession == nil {
		entry.liveSession = &domain.LiveSession{
			Provider:       event.Event.Provider,
			SessionID:      event.Event.SessionID,
			ConversationID: event.Event.ConversationID,
			TurnID:         event.Event.TurnID,
			StartedAt:      event.Event.At,
			RuntimePID:     event.Event.RuntimePID,
			TurnCount:      entry.attempt,
			Worker:         o.workerName,
		}
	}

	entry.liveSession.Provider = event.Event.Provider
	entry.liveSession.SessionID = event.Event.SessionID
	entry.liveSession.ConversationID = event.Event.ConversationID
	entry.liveSession.TurnID = event.Event.TurnID
	entry.liveSession.LastEvent = event.Event.Type
	entry.liveSession.LastEventAt = event.Event.At
	entry.liveSession.LastMessage = event.Event.Message
	entry.liveSession.Worker = o.workerName
	if event.Event.TurnCount > 0 {
		entry.liveSession.TurnCount = event.Event.TurnCount
	}
	if event.Event.Usage.TotalTokens > 0 {
		entry.liveSession.InputTokens = event.Event.Usage.InputTokens
		entry.liveSession.OutputTokens = event.Event.Usage.OutputTokens
		entry.liveSession.TotalTokens = event.Event.Usage.TotalTokens
	}

	if event.Event.RateLimit != nil {
		st.rateLimits[event.Event.RateLimit.Provider] = *event.Event.RateLimit
	}

	switch event.Event.Type {
	case "session_started", "turn_started", "turn_completed", "turn_failed", "turn_cancelled", "unsupported_tool_call", "approval_auto_approved":
		o.recordTimeline(st, entry.issue, entry.workspace, domain.TimelineEvent{
			At:             event.Event.At,
			IssueID:        entry.issue.ID,
			Identifier:     entry.issue.Identifier,
			Attempt:        entry.attempt,
			Event:          event.Event.Type,
			Status:         "running",
			Message:        truncateLogValue(event.Event.Message, 240),
			Provider:       event.Event.Provider,
			SessionID:      event.Event.SessionID,
			ConversationID: event.Event.ConversationID,
			TurnID:         event.Event.TurnID,
			WorkspaceKey:   entry.workspace.WorkspaceKey,
			Workspace:      entry.workspace.Path,
		})
		attrs := []any{
			slog.String("issue", entry.issue.Identifier),
			slog.Int("attempt", entry.attempt),
			slog.String("event", event.Event.Type),
		}
		if event.Event.SessionID != "" {
			attrs = append(attrs, slog.String("session_id", event.Event.SessionID))
		}
		if event.Event.ConversationID != "" {
			attrs = append(attrs, slog.String("conversation_id", event.Event.ConversationID))
		}
		if event.Event.Provider != "" {
			attrs = append(attrs, slog.String("provider", event.Event.Provider))
		}
		if event.Event.Message != "" {
			attrs = append(attrs, slog.String("message", truncateLogValue(event.Event.Message, 240)))
		}
		if event.Event.PayloadSummary != "" {
			attrs = append(attrs, slog.String("payload", truncateLogValue(event.Event.PayloadSummary, 240)))
		}
		o.logger.Info("runner event", attrs...)
	}
}

func (o *Orchestrator) applyTimelineUpdate(st *state, update timelineUpdate) {
	issue := domain.Issue{ID: update.IssueID, Identifier: update.Identifier}
	if entry := st.running[update.IssueID]; entry != nil {
		issue = entry.issue
		if update.ApplyToIssue != nil {
			entry.issue = *update.ApplyToIssue
			issue = entry.issue
		}
		if update.Workspace.Path == "" {
			update.Workspace = entry.workspace
		}
	}
	if issue.Identifier == "" {
		issue.Identifier = update.Identifier
	}
	o.recordTimeline(st, issue, update.Workspace, update.Event)
}

func (o *Orchestrator) applyWorkerIssueUpdate(st *state, update workerIssueUpdate) {
	entry := st.running[update.IssueID]
	if entry == nil {
		return
	}
	entry.issue = update.Issue
	entry.recentEvents = append(entry.recentEvents, domain.RecentEvent{
		At:      time.Now().UTC(),
		Event:   "tracker_state_transition",
		Message: update.Message,
	})
	if len(entry.recentEvents) > recentEventLimit {
		entry.recentEvents = entry.recentEvents[len(entry.recentEvents)-recentEventLimit:]
	}
}

func (o *Orchestrator) applyCleanupResult(st *state, result cleanupResult) {
	if result.Err != nil {
		o.recordTimeline(st, result.Issue, result.Workspace, domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      result.Issue.ID,
			Identifier:   result.Issue.Identifier,
			Event:        "workspace_cleanup_failed",
			Status:       "error",
			WorkspaceKey: result.Workspace.WorkspaceKey,
			Workspace:    result.Workspace.Path,
			LastError:    result.Err.Error(),
			Message:      "workspace cleanup failed",
		})
		o.logger.Error("workspace cleanup failed", slog.String("path", result.Workspace.Path), slog.Any("error", result.Err))
		return
	}
	o.recordTimeline(st, result.Issue, result.Workspace, domain.TimelineEvent{
		At:           time.Now().UTC(),
		IssueID:      result.Issue.ID,
		Identifier:   result.Issue.Identifier,
		Event:        "workspace_cleaned",
		Status:       "completed",
		WorkspaceKey: result.Workspace.WorkspaceKey,
		Workspace:    result.Workspace.Path,
		Message:      "workspace removed after run completion",
	})
	o.logger.Info("workspace cleaned", slog.String("path", result.Workspace.Path))
}

func (o *Orchestrator) applyWorkerExit(ctx context.Context, st *state, exit workerExit) {
	entry := st.running[exit.Issue.ID]
	if entry == nil {
		return
	}

	delete(st.running, exit.Issue.ID)
	st.totals.InputTokens += exit.Result.Totals.InputTokens
	st.totals.OutputTokens += exit.Result.Totals.OutputTokens
	st.totals.TotalTokens += exit.Result.Totals.TotalTokens
	st.totals.SecondsRunning += time.Since(entry.startedAt).Seconds()
	for key, snapshot := range exit.Result.RateLimits {
		st.rateLimits[key] = snapshot
	}

	switch {
	case exit.Err == nil && exit.Refreshed != nil && o.cfg.IsTerminalState(exit.Refreshed.State):
		st.completed[exit.Refreshed.Identifier] = struct{}{}
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, *exit.Refreshed, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Refreshed.ID,
			Identifier:     exit.Refreshed.Identifier,
			Attempt:        exit.Attempt,
			Event:          "issue_completed",
			Status:         "completed",
			StateAfter:     exit.Refreshed.State,
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			Message:        "run completed and issue is now terminal",
		})
		o.logger.Info("issue completed",
			slog.String("issue", exit.Refreshed.Identifier),
			slog.String("state", exit.Refreshed.State),
			slog.Int("attempt", exit.Attempt),
		)
		o.asyncCleanup(*exit.Refreshed, exit.Workspace)
	case exit.Err == nil && exit.Result.StopReason == reviewRejectedStopReason && exit.Refreshed != nil:
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, *exit.Refreshed, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Refreshed.ID,
			Identifier:     exit.Refreshed.Identifier,
			Attempt:        exit.Attempt,
			Event:          "issue_released",
			Status:         "released",
			Reason:         reviewRejectedStopReason,
			StateAfter:     exit.Refreshed.State,
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			Message:        "review found blocking issues and released the issue back to todo",
		})
		o.logger.Info("issue returned to todo after review",
			slog.String("issue", exit.Refreshed.Identifier),
			slog.Int("attempt", exit.Attempt),
		)
	case exit.Err == nil && exit.Refreshed != nil && !o.cfg.IsActiveState(exit.Refreshed.State):
		st.completed[exit.Refreshed.Identifier] = struct{}{}
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, *exit.Refreshed, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Refreshed.ID,
			Identifier:     exit.Refreshed.Identifier,
			Attempt:        exit.Attempt,
			Event:          "issue_completed",
			Status:         "completed",
			Reason:         "handoff_state",
			StateAfter:     exit.Refreshed.State,
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			Message:        "run completed and issue moved to a non-active handoff state",
		})
		o.logger.Info("issue handed off",
			slog.String("issue", exit.Refreshed.Identifier),
			slog.String("state", exit.Refreshed.State),
			slog.Int("attempt", exit.Attempt),
		)
	case entry.stopReason == "terminal_state":
		st.completed[entry.issue.Identifier] = struct{}{}
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, entry.issue, exit.Workspace, domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      entry.issue.ID,
			Identifier:   entry.issue.Identifier,
			Attempt:      exit.Attempt,
			Event:        "issue_completed",
			Status:       "completed",
			Reason:       "terminal_state",
			StateAfter:   entry.issue.State,
			WorkspaceKey: exit.Workspace.WorkspaceKey,
			Workspace:    exit.Workspace.Path,
			Message:      "run stopped because the tracker issue entered a terminal state",
		})
		o.logger.Info("stopping issue after terminal state change",
			slog.String("issue", entry.issue.Identifier),
			slog.String("state", entry.issue.State),
		)
		o.asyncCleanup(entry.issue, exit.Workspace)
	case entry.stopReason == "non_active_state":
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, entry.issue, exit.Workspace, domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      entry.issue.ID,
			Identifier:   entry.issue.Identifier,
			Attempt:      exit.Attempt,
			Event:        "issue_released",
			Status:       "released",
			Reason:       "non_active_state",
			StateAfter:   entry.issue.State,
			WorkspaceKey: exit.Workspace.WorkspaceKey,
			Workspace:    exit.Workspace.Path,
			Message:      "run stopped because the issue left the configured active states",
		})
		o.logger.Info("stopping issue after non-active state change",
			slog.String("issue", entry.issue.Identifier),
			slog.String("state", entry.issue.State),
		)
	case entry.stopReason == "missing_issue":
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, entry.issue, exit.Workspace, domain.TimelineEvent{
			At:           time.Now().UTC(),
			IssueID:      entry.issue.ID,
			Identifier:   entry.issue.Identifier,
			Attempt:      exit.Attempt,
			Event:        "issue_released",
			Status:       "released",
			Reason:       "missing_issue",
			WorkspaceKey: exit.Workspace.WorkspaceKey,
			Workspace:    exit.Workspace.Path,
			Message:      "run stopped because the issue was missing from tracker refresh",
		})
		o.logger.Info("stopping issue after tracker refresh omitted it",
			slog.String("issue", entry.issue.Identifier),
		)
	case exit.Err == nil && exit.Refreshed != nil && o.cfg.IsActiveState(exit.Refreshed.State):
		reason := "active_still_open"
		if exit.Result.StopReason != "" {
			reason = exit.Result.StopReason
		}
		o.recordTimeline(st, *exit.Refreshed, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Refreshed.ID,
			Identifier:     exit.Refreshed.Identifier,
			Attempt:        exit.Attempt,
			Event:          "attempt_completed",
			Status:         "retrying",
			Reason:         reason,
			StateAfter:     exit.Refreshed.State,
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			Message:        "attempt completed but the issue remains active",
		})
		o.scheduleRetry(ctx, st, *exit.Refreshed, exit.Attempt+1, reason, "", time.Now().UTC())
	case exit.Err == nil:
		delete(st.claimed, exit.Issue.ID)
		o.recordTimeline(st, exit.Issue, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Issue.ID,
			Identifier:     exit.Issue.Identifier,
			Attempt:        exit.Attempt,
			Event:          "attempt_completed",
			Status:         "completed",
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			Message:        "attempt completed without retry",
		})
		o.logger.Info("attempt completed",
			slog.String("issue", exit.Issue.Identifier),
			slog.Int("attempt", exit.Attempt),
		)
	default:
		entry.lastError = exit.Err.Error()
		o.recordTimeline(st, exit.Issue, exit.Workspace, domain.TimelineEvent{
			At:             time.Now().UTC(),
			IssueID:        exit.Issue.ID,
			Identifier:     exit.Issue.Identifier,
			Attempt:        exit.Attempt,
			Event:          "attempt_failed",
			Status:         "retrying",
			Reason:         retryReason(exit.Err),
			StateAfter:     exit.Issue.State,
			Provider:       exit.Result.Session.Provider,
			SessionID:      exit.Result.Session.SessionID,
			ConversationID: exit.Result.Session.ConversationID,
			TurnID:         exit.Result.Session.TurnID,
			WorkspaceKey:   exit.Workspace.WorkspaceKey,
			Workspace:      exit.Workspace.Path,
			LastError:      exit.Err.Error(),
			Message:        "attempt failed and will be retried",
		})
		o.logger.Error("attempt failed", slog.String("issue", exit.Issue.Identifier), slog.Int("attempt", exit.Attempt), slog.Any("error", exit.Err))
		o.scheduleRetry(ctx, st, exit.Issue, exit.Attempt+1, retryReason(exit.Err), entry.lastError, time.Now().UTC())
	}

	_ = ctx
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, st *state, issue domain.Issue, attempt int, reason, lastError string, now time.Time) {
	st.claimed[issue.ID] = struct{}{}
	dueAt := now.Add(backoff(attempt, o.cfg.Agent.MaxRetryBackoff))
	st.retryQueue[issue.ID] = domain.RetryEntry{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Attempt:    attempt,
		DueAt:      dueAt,
		Reason:     reason,
		LastError:  lastError,
	}
	o.recordTimeline(st, issue, domain.Workspace{
		Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
	}, domain.TimelineEvent{
		At:           now,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		Attempt:      attempt,
		Event:        "retry_scheduled",
		Status:       "retrying",
		Reason:       reason,
		StateAfter:   issue.State,
		WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier),
		Workspace:    filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier)),
		LastError:    lastError,
		Message:      "issue scheduled for a future retry",
	})
	o.logger.Info("scheduled retry",
		slog.String("issue", issue.Identifier),
		slog.Int("attempt", attempt),
		slog.String("reason", reason),
		slog.Time("due_at", dueAt),
		slog.String("last_error", truncateLogValue(lastError, 240)),
	)
	o.syncProgressComment(ctx, issue, progressCommentUpdate{
		Attempt:     attempt,
		Status:      "retrying",
		Summary:     "issue scheduled for a future retry",
		NextRetryAt: dueAt,
		LastError:   lastError,
	})
}

func (o *Orchestrator) publishSnapshot(st *state) {
	running := make([]domain.RunningSnapshot, 0, len(st.running))
	for _, entry := range st.running {
		snapshot := domain.RunningSnapshot{
			Issue:       entry.issue,
			Attempt:     entry.attempt,
			Workspace:   entry.workspace,
			StartedAt:   entry.startedAt,
			LiveSession: entry.liveSession,
			LastError:   entry.lastError,
		}
		snapshot.RecentEvents = append(snapshot.RecentEvents, entry.recentEvents...)
		running = append(running, snapshot)
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].Issue.Identifier < running[j].Issue.Identifier
	})

	retrying := make([]domain.RetryEntry, 0, len(st.retryQueue))
	for _, entry := range st.retryQueue {
		retrying = append(retrying, entry)
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].DueAt.Before(retrying[j].DueAt)
	})

	completed := make([]string, 0, len(st.completed))
	for identifier := range st.completed {
		completed = append(completed, identifier)
	}
	sort.Strings(completed)

	dispatch := domain.DispatchStatus{}
	if err := o.dispatchValidationError(); err != nil {
		dispatch.Blocked = true
		dispatch.Error = err.Error()
	}

	recentActivity := reverseTimeline(st.recentActivity)

	o.snapshot.Store(domain.StateSnapshot{
		GeneratedAt: time.Now().UTC(),
		Workflow:    domain.WorkflowStatus{Path: o.cfg.SourcePath},
		Environment: environmentStatus(o.cfg),
		Counts: domain.SnapshotCounts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		Dispatch:       dispatch,
		Running:        running,
		Retrying:       retrying,
		RecentActivity: recentActivity,
		AgentTotals:    st.totals,
		RateLimits:     domain.SortedRateLimits(st.rateLimits),
		Completed:      completed,
	})
	o.history.Store(cloneHistoryMap(st.history))
}

func (o *Orchestrator) recordTimeline(st *state, issue domain.Issue, workspace domain.Workspace, event domain.TimelineEvent) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.IssueID == "" {
		event.IssueID = issue.ID
	}
	if event.Identifier == "" {
		event.Identifier = issue.Identifier
	}
	if event.WorkspaceKey == "" {
		event.WorkspaceKey = workspace.WorkspaceKey
	}
	if event.Workspace == "" {
		event.Workspace = workspace.Path
	}

	identifier := strings.TrimSpace(event.Identifier)
	if identifier != "" {
		st.history[identifier] = append(st.history[identifier], event)
		if len(st.history[identifier]) > timelineEventLimit {
			st.history[identifier] = st.history[identifier][len(st.history[identifier])-timelineEventLimit:]
		}
	}

	st.recentActivity = append(st.recentActivity, event)
	if len(st.recentActivity) > recentActivityLimit {
		st.recentActivity = st.recentActivity[len(st.recentActivity)-recentActivityLimit:]
	}

	o.appendTimelineToDisk(workspace, event)
}

func (o *Orchestrator) appendTimelineToDisk(workspace domain.Workspace, event domain.TimelineEvent) {
	workspaceKey := strings.TrimSpace(workspace.WorkspaceKey)
	if workspaceKey == "" {
		workspaceKey = domain.SanitizeWorkspaceKey(event.Identifier)
	}
	if workspaceKey == "" {
		return
	}

	historyDir := filepath.Join(o.cfg.Workspace.Root, ".harness-history")
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		o.logger.Warn("timeline history directory create failed", slog.String("dir", historyDir), slog.Any("error", err))
		return
	}

	line, err := json.Marshal(event)
	if err != nil {
		o.logger.Warn("timeline event marshal failed", slog.String("issue", event.Identifier), slog.Any("error", err))
		return
	}

	path := filepath.Join(historyDir, workspaceKey+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		o.logger.Warn("timeline history append failed", slog.String("path", path), slog.Any("error", err))
		return
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		o.logger.Warn("timeline history write failed", slog.String("path", path), slog.Any("error", err))
	}
}

func (o *Orchestrator) asyncCleanup(issue domain.Issue, workspace domain.Workspace) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), o.cfg.Hooks.Timeout)
		defer cancel()
		o.pushEvent(cleanupResult{
			Issue:     issue,
			Workspace: workspace,
			Err:       o.workspaces.Cleanup(cleanupCtx, workspace),
		})
	}()
}

func backoff(attempt int, maxBackoff time.Duration) time.Duration {
	if attempt <= 1 {
		return baseRetryBackoff
	}
	backoff := baseRetryBackoff
	for i := 2; i <= attempt; i++ {
		backoff *= 2
		if backoff >= maxBackoff {
			return maxBackoff
		}
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func hasActiveBlocker(issue domain.Issue, cfg config.RuntimeConfig) bool {
	for _, blocker := range issue.BlockedBy {
		if !cfg.IsTerminalState(blocker.State) {
			return true
		}
	}
	return false
}

func retryReason(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	message := err.Error()
	switch {
	case slices.Contains([]string{"turn_timeout", "stall_timeout", "approval_required", "user_input_required"}, message):
		return message
	default:
		return "attempt_failed"
	}
}

func (o *Orchestrator) syncProgressComment(ctx context.Context, issue domain.Issue, update progressCommentUpdate) {
	if strings.TrimSpace(issue.ID) == "" {
		return
	}
	if strings.TrimSpace(update.Worker) == "" {
		update.Worker = o.workerName
	}
	if update.UpdatedAt.IsZero() {
		update.UpdatedAt = time.Now().UTC()
	}
	commentCtx, cancel := context.WithTimeout(ctx, progressCommentTimeout)
	defer cancel()

	if err := o.tracker.UpsertProgressComment(commentCtx, issue, renderProgressComment(issue, update)); err != nil {
		o.logger.Warn("progress comment update failed",
			slog.String("issue", issue.Identifier),
			slog.Any("error", err),
		)
	}
}

func renderProgressComment(issue domain.Issue, update progressCommentUpdate) string {
	lines := []string{
		domain.HarnessProgressCommentHeading,
		"",
		"Updated automatically by Go Harness.",
		"",
		"- Issue: `" + strings.TrimSpace(issue.Identifier) + "`",
		"- Status: `" + strings.TrimSpace(update.Status) + "`",
		"- Worker: `" + strings.TrimSpace(update.Worker) + "`",
		"- Tracker state: `" + strings.TrimSpace(issue.State) + "`",
		fmt.Sprintf("- Attempt: `%d`", update.Attempt),
		"- Updated at (UTC): `" + update.UpdatedAt.UTC().Format(time.RFC3339) + "`",
		"- Summary: " + strings.TrimSpace(update.Summary),
	}
	if url := strings.TrimSpace(update.PullRequestURL); url != "" {
		lines = append(lines, "- Pull request: "+url)
	}
	if !update.NextRetryAt.IsZero() {
		lines = append(lines, "- Next retry at (UTC): `"+update.NextRetryAt.UTC().Format(time.RFC3339)+"`")
	}
	if lastError := strings.TrimSpace(update.LastError); lastError != "" {
		lines = append(lines, "- Last error: "+truncateLogValue(lastError, 500))
	}
	return strings.Join(lines, "\n")
}

func (o *Orchestrator) pushEvent(event any) {
	select {
	case o.eventCh <- event:
	case <-o.doneCh:
	}
}

func (o *Orchestrator) maybeReloadConfig(ticker *time.Ticker) {
	if o.configs == nil {
		return
	}

	cfg, changed, err := o.configs.ReloadIfChanged()
	if err != nil {
		o.logger.Warn("workflow reload failed", slog.Any("error", err))
		return
	}
	if !changed {
		return
	}

	intervalChanged := o.cfg.Polling.Interval != cfg.Polling.Interval
	o.cfg = cfg
	if intervalChanged {
		ticker.Reset(o.cfg.Polling.Interval)
	}
	o.logger.Info("workflow configuration reloaded",
		slog.String("source_path", cfg.SourcePath),
		slog.String("log_level", cfg.Logging.Level),
		slog.Bool("capture_prompts", cfg.Logging.CapturePrompts),
		slog.Duration("poll_interval", cfg.Polling.Interval),
	)
}

func (o *Orchestrator) dispatchValidationError() error {
	if o.configs == nil {
		return nil
	}
	return o.configs.DispatchValidationError()
}

func (o *Orchestrator) refreshIssue(ctx context.Context, issueID string) (*domain.Issue, error) {
	issues, err := o.tracker.FetchByIDs(ctx, []string{issueID})
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, nil
	}
	return &issues[0], nil
}

func coalesceIssue(preferred *domain.Issue, fallback domain.Issue) domain.Issue {
	if preferred != nil {
		return *preferred
	}
	return fallback
}

func reverseTimeline(events []domain.TimelineEvent) []domain.TimelineEvent {
	if len(events) == 0 {
		return nil
	}
	reversed := make([]domain.TimelineEvent, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		reversed = append(reversed, events[i])
	}
	return reversed
}

func (o *Orchestrator) issueHistory(identifier string) []domain.TimelineEvent {
	if strings.TrimSpace(identifier) == "" {
		return nil
	}
	value, _ := o.history.Load().(map[string][]domain.TimelineEvent)
	return cloneTimelineEvents(value[identifier])
}

func cloneHistoryMap(history map[string][]domain.TimelineEvent) map[string][]domain.TimelineEvent {
	if len(history) == 0 {
		return map[string][]domain.TimelineEvent{}
	}
	cloned := make(map[string][]domain.TimelineEvent, len(history))
	for identifier, events := range history {
		cloned[identifier] = cloneTimelineEvents(events)
	}
	return cloned
}

func cloneTimelineEvents(events []domain.TimelineEvent) []domain.TimelineEvent {
	if len(events) == 0 {
		return nil
	}
	cloned := make([]domain.TimelineEvent, len(events))
	copy(cloned, events)
	return cloned
}

func sortDispatchCandidates(candidates []domain.Issue) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]

		leftPriority := dispatchPriority(left.Priority)
		rightPriority := dispatchPriority(right.Priority)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}

		switch {
		case left.CreatedAt.IsZero() && right.CreatedAt.IsZero():
		case left.CreatedAt.IsZero():
			return false
		case right.CreatedAt.IsZero():
			return true
		case !left.CreatedAt.Equal(right.CreatedAt):
			return left.CreatedAt.Before(right.CreatedAt)
		}

		return strings.Compare(left.Identifier, right.Identifier) < 0
	})
}

func dispatchPriority(priority int) int {
	if priority <= 0 {
		return 1_000_000
	}
	return priority
}

func (o *Orchestrator) startupCleanup(ctx context.Context) {
	issues, err := o.tracker.PollTerminalIssues(ctx)
	if err != nil {
		o.logger.Warn("startup terminal cleanup failed", slog.Any("error", err))
		return
	}
	if len(issues) == 0 {
		return
	}

	for _, issue := range issues {
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			continue
		}
		workspace := domain.Workspace{
			WorkspaceKey: domain.SanitizeWorkspaceKey(identifier),
			Path:         filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(identifier)),
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, o.cfg.Hooks.Timeout)
		err := o.workspaces.Cleanup(cleanupCtx, workspace)
		cancel()
		if err != nil {
			o.logger.Warn("startup terminal workspace cleanup failed",
				slog.String("issue", identifier),
				slog.String("workspace", workspace.Path),
				slog.Any("error", err),
			)
			continue
		}
		o.logger.Info("startup terminal workspace cleaned",
			slog.String("issue", identifier),
			slog.String("workspace", workspace.Path),
		)
	}
}

func truncateLogValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func environmentStatus(cfg config.RuntimeConfig) domain.EnvironmentStatus {
	entries := make([]domain.EnvironmentEntry, 0, len(cfg.Environment.Entries))
	for _, entry := range cfg.Environment.Entries {
		entries = append(entries, domain.EnvironmentEntry{
			Name:   entry.Name,
			Value:  entry.Value,
			Source: entry.Source,
		})
	}
	return domain.EnvironmentStatus{
		DotEnvPath:    cfg.Environment.DotEnvPath,
		DotEnvPresent: cfg.Environment.DotEnvPresent,
		Entries:       entries,
	}
}
