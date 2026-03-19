package main

import (
	"context"
	"log/slog"
	"net/http"

	"go-harness/internal/agent/codex"
	"go-harness/internal/config"
	"go-harness/internal/domain"
	"go-harness/internal/tracker/linear"
	"go-harness/internal/workspace"
)

type dynamicTracker struct {
	store      *config.Store
	httpClient *http.Client
}

func (d *dynamicTracker) PollCandidates(ctx context.Context) ([]domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).PollCandidates(ctx)
}

func (d *dynamicTracker) FetchByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).FetchByIDs(ctx, ids)
}

func (d *dynamicTracker) TransitionState(ctx context.Context, issue domain.Issue, stateName string) (domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).TransitionState(ctx, issue, stateName)
}

type dynamicWorkspaceManager struct {
	store  *config.Store
	logger *slog.Logger
}

func (d *dynamicWorkspaceManager) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.Hooks, d.logger).Prepare(ctx, issue)
}

func (d *dynamicWorkspaceManager) AfterRun(ctx context.Context, issueWorkspace domain.Workspace) error {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.Hooks, d.logger).AfterRun(ctx, issueWorkspace)
}

func (d *dynamicWorkspaceManager) Cleanup(ctx context.Context, issueWorkspace domain.Workspace) error {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.Hooks, d.logger).Cleanup(ctx, issueWorkspace)
}

type dynamicRunner struct {
	store  *config.Store
	logger *slog.Logger
}

func (d *dynamicRunner) RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(codex.Event), continueFn codex.ContinueFunc) (codex.RunResult, error) {
	cfg := d.store.Current()
	return codex.NewRunner(cfg.Codex, d.logger).RunAttempt(ctx, issue, workspace, prompt, attempt, onEvent, continueFn)
}
