package domain

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var invalidWorkspaceChar = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Blocker struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	State      string `json:"state"`
}

type Issue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    int       `json:"priority"`
	State       string    `json:"state"`
	BranchName  string    `json:"branch_name"`
	URL         string    `json:"url"`
	Labels      []string  `json:"labels"`
	BlockedBy   []Blocker `json:"blocked_by"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type WorkflowDefinition struct {
	SourcePath     string
	Config         map[string]any
	PromptTemplate string
}

type Workspace struct {
	Path         string `json:"path"`
	WorkspaceKey string `json:"workspace_key"`
	CreatedNow   bool   `json:"created_now"`
}

type RunAttempt struct {
	IssueID       string    `json:"issue_id"`
	Identifier    string    `json:"identifier"`
	Attempt       int       `json:"attempt"`
	WorkspacePath string    `json:"workspace_path"`
	StartedAt     time.Time `json:"started_at"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
}

type LiveSession struct {
	SessionID    string    `json:"session_id"`
	ThreadID     string    `json:"thread_id"`
	TurnID       string    `json:"turn_id"`
	StartedAt    time.Time `json:"started_at"`
	LastEvent    string    `json:"last_event"`
	LastEventAt  time.Time `json:"last_event_at,omitempty"`
	LastMessage  string    `json:"last_message,omitempty"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	TurnCount    int       `json:"turn_count"`
	AppServerPID int       `json:"app_server_pid,omitempty"`
	Worker       string    `json:"worker,omitempty"`
}

type RecentEvent struct {
	At             time.Time `json:"at"`
	Event          string    `json:"event"`
	Message        string    `json:"message,omitempty"`
	PayloadSummary string    `json:"payload_summary,omitempty"`
}

type RetryEntry struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Reason     string    `json:"reason"`
	LastError  string    `json:"last_error,omitempty"`
}

type RuntimeTotals struct {
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	TotalTokens    int     `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type RateLimitSnapshot struct {
	Provider  string         `json:"provider"`
	UpdatedAt time.Time      `json:"updated_at"`
	Raw       map[string]any `json:"raw"`
}

type RunningSnapshot struct {
	Issue        Issue         `json:"issue"`
	Attempt      int           `json:"attempt"`
	Workspace    Workspace     `json:"workspace"`
	StartedAt    time.Time     `json:"started_at"`
	LiveSession  *LiveSession  `json:"live_session,omitempty"`
	RecentEvents []RecentEvent `json:"recent_events,omitempty"`
	LastError    string        `json:"last_error,omitempty"`
}

type StateSnapshot struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Counts      SnapshotCounts      `json:"counts"`
	Running     []RunningSnapshot   `json:"running"`
	Retrying    []RetryEntry        `json:"retrying"`
	CodexTotals RuntimeTotals       `json:"codex_totals"`
	RateLimits  []RateLimitSnapshot `json:"rate_limits"`
	Completed   []string            `json:"completed,omitempty"`
}

type IssueRuntimeSnapshot struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Identifier  string           `json:"identifier"`
	Status      string           `json:"status"`
	Running     *RunningSnapshot `json:"running,omitempty"`
	Retry       *RetryEntry      `json:"retry,omitempty"`
	Completed   bool             `json:"completed,omitempty"`
}

type SnapshotCounts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

func NormalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func SanitizeWorkspaceKey(identifier string) string {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return "unknown"
	}

	sanitized := invalidWorkspaceChar.ReplaceAllString(trimmed, "_")
	if sanitized == "" {
		return "unknown"
	}

	return sanitized
}

func FormatSessionID(threadID, turnID string) string {
	if threadID == "" || turnID == "" {
		return ""
	}
	return threadID + "-" + turnID
}

func SortedRateLimits(rateLimits map[string]RateLimitSnapshot) []RateLimitSnapshot {
	keys := make([]string, 0, len(rateLimits))
	for key := range rateLimits {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]RateLimitSnapshot, 0, len(keys))
	for _, key := range keys {
		result = append(result, rateLimits[key])
	}

	return result
}
