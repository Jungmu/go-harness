package agent

import (
	"context"
	"time"

	"go-harness/internal/domain"
)

type Event struct {
	Provider       string
	Type           string
	At             time.Time
	Message        string
	PayloadSummary string
	SessionID      string
	ConversationID string
	TurnID         string
	RuntimePID     int
	TurnCount      int
	Usage          domain.RuntimeTotals
	RateLimit      *domain.RateLimitSnapshot
}

type ContinueDecision struct {
	Continue       bool
	NextPrompt     string
	StopReason     string
	RefreshedIssue *domain.Issue
}

type ContinueFunc func(ctx context.Context, session domain.LiveSession) (ContinueDecision, error)

type RunResult struct {
	Session        domain.LiveSession
	Totals         domain.RuntimeTotals
	RateLimits     map[string]domain.RateLimitSnapshot
	StopReason     string
	RefreshedIssue *domain.Issue
}

type Runner interface {
	RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(Event), continueFn ContinueFunc) (RunResult, error)
}
