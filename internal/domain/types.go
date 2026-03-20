package domain

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var invalidWorkspaceChar = regexp.MustCompile(`[^A-Za-z0-9._-]`)

const HarnessProgressCommentHeading = "## Harness Progress"

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
	Provider       string    `json:"provider,omitempty"`
	SessionID      string    `json:"session_id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	LastEvent      string    `json:"last_event"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
	LastMessage    string    `json:"last_message,omitempty"`
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	TotalTokens    int       `json:"total_tokens"`
	TurnCount      int       `json:"turn_count"`
	RuntimePID     int       `json:"runtime_pid,omitempty"`
	Worker         string    `json:"worker,omitempty"`
}

type RecentEvent struct {
	At             time.Time `json:"at"`
	Event          string    `json:"event"`
	Message        string    `json:"message,omitempty"`
	PayloadSummary string    `json:"payload_summary,omitempty"`
}

type TimelineEvent struct {
	At             time.Time `json:"at"`
	IssueID        string    `json:"issue_id,omitempty"`
	Identifier     string    `json:"identifier,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	Event          string    `json:"event"`
	Status         string    `json:"status,omitempty"`
	Message        string    `json:"message,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	StateBefore    string    `json:"state_before,omitempty"`
	StateAfter     string    `json:"state_after,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	WorkspaceKey   string    `json:"workspace_key,omitempty"`
	Workspace      string    `json:"workspace,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
}

type RetryEntry struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Reason     string    `json:"reason"`
	LastError  string    `json:"last_error,omitempty"`
}

type PullRequest struct {
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	Created    bool   `json:"created,omitempty"`
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
	GeneratedAt    time.Time           `json:"generated_at"`
	Workflow       WorkflowStatus      `json:"workflow"`
	Environment    EnvironmentStatus   `json:"environment"`
	Counts         SnapshotCounts      `json:"counts"`
	Dispatch       DispatchStatus      `json:"dispatch"`
	Running        []RunningSnapshot   `json:"running"`
	Retrying       []RetryEntry        `json:"retrying"`
	RecentActivity []TimelineEvent     `json:"recent_activity,omitempty"`
	AgentTotals    RuntimeTotals       `json:"agent_totals"`
	RateLimits     []RateLimitSnapshot `json:"rate_limits"`
	Completed      []string            `json:"completed,omitempty"`
}

type IssueRuntimeSnapshot struct {
	GeneratedAt      time.Time               `json:"generated_at"`
	Identifier       string                  `json:"identifier"`
	Status           string                  `json:"status"`
	Running          *RunningSnapshot        `json:"running,omitempty"`
	Retry            *RetryEntry             `json:"retry,omitempty"`
	History          []TimelineEvent         `json:"history,omitempty"`
	PromptTranscript []PromptTranscriptEntry `json:"prompt_transcript,omitempty"`
	Completed        bool                    `json:"completed,omitempty"`
}

type PromptTranscriptEntry struct {
	At             time.Time `json:"at"`
	Attempt        int       `json:"attempt,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Direction      string    `json:"direction"`
	Channel        string    `json:"channel"`
	SessionID      string    `json:"session_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	TurnCount      int       `json:"turn_count,omitempty"`
	Payload        string    `json:"payload"`
}

type SnapshotCounts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

type DispatchStatus struct {
	Blocked bool                   `json:"blocked"`
	Error   string                 `json:"error,omitempty"`
	Workers []WorkerDispatchStatus `json:"workers,omitempty"`
}

type WorkflowStatus struct {
	Path       string `json:"path,omitempty"`
	ReviewPath string `json:"review_path,omitempty"`
}

type WorkerDispatchStatus struct {
	Worker  string `json:"worker"`
	Blocked bool   `json:"blocked"`
	Error   string `json:"error,omitempty"`
}

type EnvironmentStatus struct {
	DotEnvPath    string             `json:"dotenv_path,omitempty"`
	DotEnvPresent bool               `json:"dotenv_present"`
	Entries       []EnvironmentEntry `json:"entries,omitempty"`
}

type EnvironmentEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Source string `json:"source"`
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

func NormalizeBranchName(raw, identifier string) string {
	fallback := branchSlug(identifier)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	parts := strings.Split(raw, "/")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		if slug := branchSlug(part); slug != "" {
			normalized = append(normalized, slug)
		}
	}
	if len(normalized) == 0 {
		return fallback
	}
	return strings.Join(normalized, "/")
}

func branchSlug(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))
	lastHyphen := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastHyphen = false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(unicode.ToLower(r))
			lastHyphen = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if builder.Len() > 0 && !lastHyphen {
				builder.WriteByte('-')
				lastHyphen = true
			}
		case r < utf8.RuneSelf:
			if builder.Len() > 0 && !lastHyphen {
				builder.WriteByte('-')
				lastHyphen = true
			}
		default:
			if builder.Len() > 0 && !lastHyphen {
				builder.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(builder.String(), "-")
}

func FormatSessionID(conversationID, turnID string) string {
	if conversationID == "" || turnID == "" {
		return ""
	}
	return conversationID + "-" + turnID
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
