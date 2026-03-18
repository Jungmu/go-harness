package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go-harness/internal/agent/codex"
	"go-harness/internal/config"
	"go-harness/internal/domain"
	"go-harness/internal/workflow"
)

const (
	recentEventLimit = 50
	slotRetryDelay   = 1 * time.Second
	baseRetryBackoff = 5 * time.Second
)

type Tracker interface {
	PollCandidates(ctx context.Context) ([]domain.Issue, error)
	FetchByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
}

type WorkspaceManager interface {
	Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error)
	AfterRun(ctx context.Context, workspace domain.Workspace) error
	Cleanup(ctx context.Context, workspace domain.Workspace) error
}

type Runner interface {
	RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error)
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
	logger     *slog.Logger
	configs    ConfigSource

	controlCh chan struct{}
	eventCh   chan any
	doneCh    chan struct{}

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	snapshot atomic.Value
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
	running    map[string]*runningTask
	claimed    map[string]struct{}
	retryQueue map[string]domain.RetryEntry
	completed  map[string]struct{}
	totals     domain.RuntimeTotals
	rateLimits map[string]domain.RateLimitSnapshot
}

type workerEvent struct {
	IssueID string
	Event   codex.Event
}

type workerExit struct {
	Issue     domain.Issue
	Attempt   int
	Workspace domain.Workspace
	Result    codex.RunResult
	Refreshed *domain.Issue
	Err       error
}

func WithConfigSource(source ConfigSource) Option {
	return func(orch *Orchestrator) {
		orch.configs = source
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
	}
	for _, opt := range opts {
		opt(orch)
	}
	orch.snapshot.Store(domain.StateSnapshot{GeneratedAt: time.Now().UTC()})
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
	for _, running := range snapshot.Running {
		if running.Issue.Identifier == identifier {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt: snapshot.GeneratedAt,
				Identifier:  identifier,
				Status:      "running",
				Running:     &running,
			}, true
		}
	}
	for _, retry := range snapshot.Retrying {
		if retry.Identifier == identifier {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt: snapshot.GeneratedAt,
				Identifier:  identifier,
				Status:      "retrying",
				Retry:       &retry,
			}, true
		}
	}
	if slices.Contains(snapshot.Completed, identifier) {
		return domain.IssueRuntimeSnapshot{
			GeneratedAt: snapshot.GeneratedAt,
			Identifier:  identifier,
			Status:      "completed",
			Completed:   true,
		}, true
	}
	return domain.IssueRuntimeSnapshot{}, false
}

func (o *Orchestrator) loop(ctx context.Context) {
	defer o.wg.Done()
	defer close(o.doneCh)

	st := &state{
		running:    map[string]*runningTask{},
		claimed:    map[string]struct{}{},
		retryQueue: map[string]domain.RetryEntry{},
		completed:  map[string]struct{}{},
		rateLimits: map[string]domain.RateLimitSnapshot{},
	}

	ticker := time.NewTicker(o.cfg.Polling.Interval)
	defer ticker.Stop()

	o.maybeReloadConfig(ticker)
	o.handleTick(ctx, st)
	o.publishSnapshot(st)

	for {
		select {
		case <-ctx.Done():
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
			case workerExit:
				o.applyWorkerExit(ctx, st, event)
			}
			o.publishSnapshot(st)
		}
	}
}

func (o *Orchestrator) handleTick(ctx context.Context, st *state) {
	o.reconcileRunning(ctx, st)
	if err := o.dispatchValidationError(); err != nil {
		o.logger.Warn("dispatch blocked due to invalid workflow reload", slog.Any("error", err))
		return
	}
	o.dispatchDueRetries(ctx, st)

	candidates, err := o.tracker.PollCandidates(ctx)
	if err != nil {
		o.logger.Error("candidate poll failed", slog.Any("error", err))
		return
	}

	for _, issue := range candidates {
		if !o.shouldDispatch(issue, st) {
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

	for _, issue := range issues {
		entry := st.running[issue.ID]
		if entry == nil {
			continue
		}

		if o.cfg.IsTerminalState(issue.State) {
			entry.issue = issue
			entry.cleanupOnExit = true
			entry.stopReason = "terminal_state"
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
			continue
		}

		if o.cfg.IsTerminalState(issue.State) {
			delete(st.retryQueue, issueID)
			delete(st.claimed, issueID)
			st.completed[issue.Identifier] = struct{}{}
			o.asyncCleanup(domain.Workspace{
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
	if issue.ID == "" || issue.Identifier == "" {
		return false
	}
	if !o.cfg.IsActiveState(issue.State) || o.cfg.IsTerminalState(issue.State) {
		return false
	}
	if _, ok := st.claimed[issue.ID]; ok {
		return false
	}
	if _, ok := st.running[issue.ID]; ok {
		return false
	}
	if _, ok := st.retryQueue[issue.ID]; ok {
		return false
	}
	if !o.hasAvailableSlot(issue.State, st) {
		return false
	}
	if domain.NormalizeState(issue.State) == "todo" && hasActiveBlocker(issue, o.cfg) {
		return false
	}
	return true
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

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		o.executeAttempt(runCtx, issue, attempt)
	}()
}

func (o *Orchestrator) executeAttempt(ctx context.Context, issue domain.Issue, attempt int) {
	workspace, err := o.workspaces.Prepare(ctx, issue)
	if err != nil {
		o.pushEvent(workerExit{Issue: issue, Attempt: attempt, Workspace: domain.Workspace{WorkspaceKey: domain.SanitizeWorkspaceKey(issue.Identifier), Path: filepath.Join(o.cfg.Workspace.Root, domain.SanitizeWorkspaceKey(issue.Identifier))}, Err: err})
		return
	}

	prompt, err := workflow.RenderPrompt(o.cfg.PromptTemplate, issue, attempt)
	if err != nil {
		_ = o.workspaces.AfterRun(context.Background(), workspace)
		o.pushEvent(workerExit{Issue: issue, Attempt: attempt, Workspace: workspace, Err: err})
		return
	}

	result, err := o.runner.RunAttempt(ctx, issue, workspace, prompt, attempt, func(event codex.Event) {
		o.pushEvent(workerEvent{IssueID: issue.ID, Event: event})
	}, func(runCtx context.Context, session domain.LiveSession) (codex.ContinueDecision, error) {
		refreshed, err := o.refreshIssue(runCtx, issue.ID)
		if err != nil {
			return codex.ContinueDecision{}, err
		}
		if refreshed == nil {
			return codex.ContinueDecision{Continue: false}, nil
		}
		if o.cfg.IsTerminalState(refreshed.State) || !o.cfg.IsActiveState(refreshed.State) {
			return codex.ContinueDecision{
				Continue:       false,
				RefreshedIssue: refreshed,
			}, nil
		}
		if session.TurnCount >= o.cfg.Agent.MaxTurns {
			return codex.ContinueDecision{
				Continue:       false,
				StopReason:     "max_turns_reached",
				RefreshedIssue: refreshed,
			}, nil
		}
		return codex.ContinueDecision{
			Continue:       true,
			NextPrompt:     workflow.RenderContinuationPrompt(*refreshed, session.TurnCount+1),
			RefreshedIssue: refreshed,
		}, nil
	})

	afterRunErr := o.workspaces.AfterRun(context.Background(), workspace)
	if err == nil && afterRunErr != nil {
		err = afterRunErr
	}

	o.pushEvent(workerExit{
		Issue:     issue,
		Attempt:   attempt,
		Workspace: workspace,
		Result:    result,
		Refreshed: result.RefreshedIssue,
		Err:       err,
	})
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
			SessionID:    event.Event.SessionID,
			ThreadID:     event.Event.ThreadID,
			TurnID:       event.Event.TurnID,
			StartedAt:    event.Event.At,
			AppServerPID: event.Event.AppServerPID,
			TurnCount:    entry.attempt,
		}
	}

	entry.liveSession.SessionID = event.Event.SessionID
	entry.liveSession.ThreadID = event.Event.ThreadID
	entry.liveSession.TurnID = event.Event.TurnID
	entry.liveSession.LastEvent = event.Event.Type
	entry.liveSession.LastEventAt = event.Event.At
	entry.liveSession.LastMessage = event.Event.Message
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
	case entry.stopReason == "terminal_state":
		st.completed[entry.issue.Identifier] = struct{}{}
		delete(st.claimed, exit.Issue.ID)
		o.asyncCleanup(exit.Workspace)
	case entry.stopReason == "non_active_state":
		delete(st.claimed, exit.Issue.ID)
	case exit.Err == nil && exit.Refreshed != nil && o.cfg.IsTerminalState(exit.Refreshed.State):
		st.completed[exit.Refreshed.Identifier] = struct{}{}
		delete(st.claimed, exit.Issue.ID)
		o.asyncCleanup(exit.Workspace)
	case exit.Err == nil && exit.Refreshed != nil && o.cfg.IsActiveState(exit.Refreshed.State):
		reason := "active_still_open"
		if exit.Result.StopReason != "" {
			reason = exit.Result.StopReason
		}
		o.scheduleRetry(st, *exit.Refreshed, exit.Attempt+1, reason, "", time.Now().UTC())
	case exit.Err == nil:
		delete(st.claimed, exit.Issue.ID)
	default:
		entry.lastError = exit.Err.Error()
		o.logger.Error("attempt failed", slog.String("issue", exit.Issue.Identifier), slog.Int("attempt", exit.Attempt), slog.Any("error", exit.Err))
		o.scheduleRetry(st, exit.Issue, exit.Attempt+1, retryReason(exit.Err), entry.lastError, time.Now().UTC())
	}

	_ = ctx
}

func (o *Orchestrator) scheduleRetry(st *state, issue domain.Issue, attempt int, reason, lastError string, now time.Time) {
	st.claimed[issue.ID] = struct{}{}
	st.retryQueue[issue.ID] = domain.RetryEntry{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Attempt:    attempt,
		DueAt:      now.Add(backoff(attempt, o.cfg.Agent.MaxRetryBackoff)),
		Reason:     reason,
		LastError:  lastError,
	}
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

	o.snapshot.Store(domain.StateSnapshot{
		GeneratedAt: time.Now().UTC(),
		Counts: domain.SnapshotCounts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		Running:     running,
		Retrying:    retrying,
		CodexTotals: st.totals,
		RateLimits:  domain.SortedRateLimits(st.rateLimits),
		Completed:   completed,
	})
}

func (o *Orchestrator) asyncCleanup(workspace domain.Workspace) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), o.cfg.Hooks.Timeout)
		defer cancel()
		if err := o.workspaces.Cleanup(cleanupCtx, workspace); err != nil {
			o.logger.Error("workspace cleanup failed", slog.String("path", workspace.Path), slog.Any("error", err))
		}
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
