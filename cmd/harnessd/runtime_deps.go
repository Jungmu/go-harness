package main

import (
	"context"
	"log/slog"
	"net/http"

	"go-harness/internal/agent"
	"go-harness/internal/agent/claude"
	"go-harness/internal/agent/codex"
	"go-harness/internal/config"
	"go-harness/internal/domain"
	gh "go-harness/internal/github"
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

func (d *dynamicTracker) PollTerminalIssues(ctx context.Context) ([]domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).PollTerminalIssues(ctx)
}

func (d *dynamicTracker) FetchByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).FetchByIDs(ctx, ids)
}

func (d *dynamicTracker) TransitionState(ctx context.Context, issue domain.Issue, stateName string) (domain.Issue, error) {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).TransitionState(ctx, issue, stateName)
}

func (d *dynamicTracker) UpsertProgressComment(ctx context.Context, issue domain.Issue, body string) error {
	cfg := d.store.Current()
	return linear.NewClient(d.httpClient, cfg.Tracker).UpsertProgressComment(ctx, issue, body)
}

type dynamicWorkspaceManager struct {
	store  *config.Store
	logger *slog.Logger
}

func (d *dynamicWorkspaceManager) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.SourcePath, cfg.Hooks, d.logger).Prepare(ctx, issue)
}

func (d *dynamicWorkspaceManager) AfterRun(ctx context.Context, issueWorkspace domain.Workspace) error {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.SourcePath, cfg.Hooks, d.logger).AfterRun(ctx, issueWorkspace)
}

func (d *dynamicWorkspaceManager) Cleanup(ctx context.Context, issueWorkspace domain.Workspace) error {
	cfg := d.store.Current()
	return workspace.NewManager(cfg.Workspace.Root, cfg.SourcePath, cfg.Hooks, d.logger).Cleanup(ctx, issueWorkspace)
}

type dynamicRunner struct {
	store  *config.Store
	logger *slog.Logger
}

func (d *dynamicRunner) RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(agent.Event), continueFn agent.ContinueFunc) (agent.RunResult, error) {
	cfg := d.store.Current()
	switch cfg.Agent.Provider {
	case "claude":
		return claude.NewRunner(cfg.Claude, cfg.Logging, d.logger).RunAttempt(ctx, issue, workspace, prompt, attempt, onEvent, continueFn)
	default:
		return codex.NewRunner(cfg.Codex, cfg.Logging, d.logger).RunAttempt(ctx, issue, workspace, prompt, attempt, onEvent, continueFn)
	}
}

type dynamicPullRequestCreator struct {
	store      *config.Store
	httpClient *http.Client
	authorizer *gh.Authorizer
}

func (d *dynamicPullRequestCreator) Warmup(ctx context.Context) error {
	cfg := d.store.Current()
	_, err := d.authorizer.ResolveConfig(ctx, cfg.GitHub)
	return err
}

func (d *dynamicPullRequestCreator) EnsurePullRequest(ctx context.Context, issue domain.Issue, workspace domain.Workspace) (domain.PullRequest, error) {
	cfg := d.store.Current()
	resolved, err := d.authorizer.ResolveConfig(ctx, cfg.GitHub)
	if err != nil {
		return domain.PullRequest{}, err
	}
	return gh.NewClient(d.httpClient, resolved).EnsurePullRequest(ctx, issue, workspace)
}
