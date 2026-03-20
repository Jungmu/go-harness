package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-harness/internal/domain"
)

const PromptTranscriptDirname = ".harness-prompts"

type transcriptEntry struct {
	At             time.Time `json:"at"`
	IssueID        string    `json:"issue_id,omitempty"`
	Identifier     string    `json:"identifier,omitempty"`
	WorkspaceKey   string    `json:"workspace_key,omitempty"`
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

type TranscriptRecorder struct {
	enabled      bool
	path         string
	issueID      string
	identifier   string
	workspaceKey string
	attempt      int
	provider     string
	logger       *slog.Logger
	mu           sync.Mutex
}

func NewTranscriptRecorder(enabled bool, provider string, issue domain.Issue, workspace domain.Workspace, attempt int, logger *slog.Logger) *TranscriptRecorder {
	workspaceKey := strings.TrimSpace(workspace.WorkspaceKey)
	if workspaceKey == "" {
		workspaceKey = domain.SanitizeWorkspaceKey(issue.Identifier)
	}

	path := ""
	if enabled && workspaceKey != "" && strings.TrimSpace(workspace.Path) != "" {
		path = filepath.Join(filepath.Dir(workspace.Path), PromptTranscriptDirname, workspaceKey+".jsonl")
	}

	return &TranscriptRecorder{
		enabled:      enabled && path != "",
		path:         path,
		issueID:      issue.ID,
		identifier:   issue.Identifier,
		workspaceKey: workspaceKey,
		attempt:      attempt,
		provider:     strings.TrimSpace(provider),
		logger:       logger,
	}
}

func (r *TranscriptRecorder) RecordPrompt(prompt string, session domain.LiveSession, turnCount int) {
	r.append(transcriptEntry{
		At:             time.Now().UTC(),
		Provider:       r.provider,
		Direction:      "send",
		Channel:        "prompt",
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnID:         session.TurnID,
		TurnCount:      turnCount,
		Payload:        prompt,
	})
}

func (r *TranscriptRecorder) RecordIO(direction, channel, payload string, session domain.LiveSession, turnCount int) {
	r.append(transcriptEntry{
		At:             time.Now().UTC(),
		Provider:       r.provider,
		Direction:      direction,
		Channel:        channel,
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnID:         session.TurnID,
		TurnCount:      turnCount,
		Payload:        payload,
	})
}

func (r *TranscriptRecorder) append(entry transcriptEntry) {
	if r == nil || !r.enabled {
		return
	}

	entry.IssueID = r.issueID
	entry.Identifier = r.identifier
	entry.WorkspaceKey = r.workspaceKey
	entry.Attempt = r.attempt

	line, err := json.Marshal(entry)
	if err != nil {
		r.warn("prompt transcript marshal failed", slog.Any("error", err))
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		r.warn("prompt transcript directory create failed", slog.String("dir", filepath.Dir(r.path)), slog.Any("error", err))
		return
	}

	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.warn("prompt transcript append failed", slog.String("path", r.path), slog.Any("error", err))
		return
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		r.warn("prompt transcript write failed", slog.String("path", r.path), slog.Any("error", err))
	}
}

func (r *TranscriptRecorder) warn(msg string, attrs ...any) {
	if r.logger == nil {
		return
	}
	r.logger.Warn(msg, attrs...)
}

func TranscriptPath(workspaceRoot, identifier string) string {
	workspaceKey := strings.TrimSpace(domain.SanitizeWorkspaceKey(identifier))
	if strings.TrimSpace(workspaceRoot) == "" || workspaceKey == "" {
		return ""
	}
	return filepath.Join(workspaceRoot, PromptTranscriptDirname, workspaceKey+".jsonl")
}

func ReadTranscript(path string, limit int) ([]domain.PromptTranscriptEntry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	if limit <= 0 {
		limit = 80
	}
	buffer := make([]domain.PromptTranscriptEntry, 0, limit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw transcriptEntry
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		entry := domain.PromptTranscriptEntry{
			At:             raw.At,
			Attempt:        raw.Attempt,
			Provider:       raw.Provider,
			Direction:      raw.Direction,
			Channel:        raw.Channel,
			SessionID:      raw.SessionID,
			ConversationID: raw.ConversationID,
			TurnID:         raw.TurnID,
			TurnCount:      raw.TurnCount,
			Payload:        raw.Payload,
		}
		if len(buffer) == limit {
			copy(buffer, buffer[1:])
			buffer[len(buffer)-1] = entry
			continue
		}
		buffer = append(buffer, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buffer, nil
}
