package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"go-harness/internal/agent"
	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const testClaudeTimeout = 3 * time.Second

func TestRunnerRunAttemptSuccess(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeClaude(t, workspace.Path, false, false, false)
	runner := NewRunner(config.ClaudeConfig{
		Command:        `exec "` + script + `"`,
		PermissionMode: "bypassPermissions",
		TurnTimeout:    testClaudeTimeout,
		ReadTimeout:    testClaudeTimeout,
		StallTimeout:   testClaudeTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if result.Session.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", result.Session.Provider)
	}
	if result.Session.ConversationID != "claude-session-1" {
		t.Fatalf("ConversationID = %q", result.Session.ConversationID)
	}
	if result.Session.SessionID != "claude-session-1-turn-1" {
		t.Fatalf("SessionID = %q", result.Session.SessionID)
	}
	if result.Totals.TotalTokens != 0 {
		t.Fatalf("TotalTokens = %d, want 0", result.Totals.TotalTokens)
	}
}

func TestRunnerContinuationUsesResume(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeClaude(t, workspace.Path, false, false, false)
	runner := NewRunner(config.ClaudeConfig{
		Command:        `exec "` + script + `"`,
		PermissionMode: "bypassPermissions",
		TurnTimeout:    testClaudeTimeout,
		ReadTimeout:    testClaudeTimeout,
		StallTimeout:   testClaudeTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, func(_ context.Context, session domain.LiveSession) (agent.ContinueDecision, error) {
		if session.TurnCount == 1 {
			return agent.ContinueDecision{
				Continue:       true,
				NextPrompt:     "continue prompt",
				RefreshedIssue: &domain.Issue{Identifier: "ABC-1", State: "In Progress"},
			}, nil
		}
		return agent.ContinueDecision{
			Continue:       false,
			RefreshedIssue: &domain.Issue{Identifier: "ABC-1", State: "Done"},
		}, nil
	})
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if result.Session.SessionID != "claude-session-1-turn-2" {
		t.Fatalf("SessionID = %q, want claude-session-1-turn-2", result.Session.SessionID)
	}

	argsLog, err := os.ReadFile(filepath.Join(workspace.Path, "claude-args.log"))
	if err != nil {
		t.Fatalf("ReadFile(claude-args.log) error = %v", err)
	}
	logText := string(argsLog)
	if !strings.Contains(logText, "-r\nclaude-session-1\n") {
		t.Fatalf("resume args missing: %q", logText)
	}
	if !strings.Contains(logText, "continue prompt") {
		t.Fatalf("continuation prompt missing from args log: %q", logText)
	}
}

func TestRunnerFailsWhenSessionIDMissing(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeClaude(t, workspace.Path, false, true, false)
	runner := NewRunner(config.ClaudeConfig{
		Command:        `exec "` + script + `"`,
		PermissionMode: "bypassPermissions",
		TurnTimeout:    testClaudeTimeout,
		ReadTimeout:    testClaudeTimeout,
		StallTimeout:   testClaudeTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	_, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "missing session_id") {
		t.Fatalf("RunAttempt() error = %v, want missing session_id", err)
	}
}

func TestRunnerFailsOnMalformedOrErrorResult(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeClaude(t, workspace.Path, true, false, true)
	runner := NewRunner(config.ClaudeConfig{
		Command:        `exec "` + script + `"`,
		PermissionMode: "bypassPermissions",
		TurnTimeout:    testClaudeTimeout,
		ReadTimeout:    testClaudeTimeout,
		StallTimeout:   testClaudeTimeout,
	}, config.LoggingConfig{CapturePrompts: true}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var events []string
	_, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, func(event agent.Event) {
		events = append(events, event.Type)
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("RunAttempt() error = %v, want permission denied", err)
	}
	if !strings.Contains(strings.Join(events, ","), "malformed") {
		t.Fatalf("events = %#v, want malformed", events)
	}

	transcriptPath := filepath.Join(filepath.Dir(workspace.Path), agent.PromptTranscriptDirname, "ABC-1.jsonl")
	entries, err := agent.ReadTranscript(transcriptPath, 80)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(entries) == 0 || entries[0].Provider != "claude" {
		t.Fatalf("entries = %#v, want claude provider", entries)
	}
}

func writeFakeClaude(t *testing.T, dir string, includeMalformed bool, omitSessionID bool, failResult bool) string {
	t.Helper()

	sessionIDExpr := `"claude-session-1"`
	if omitSessionID {
		sessionIDExpr = `""`
	}
	resultBody := `{"type":"result","subtype":"success","session_id":` + sessionIDExpr + `,"result":"done"}`
	if failResult {
		resultBody = `{"type":"result","subtype":"error","session_id":"claude-session-1","is_error":true,"result":"permission denied"}`
	}
	malformed := ""
	if includeMalformed {
		malformed = `printf '%s\n' 'not-json-at-all'`
	}

	script := `#!/bin/sh
set -eu
log="$PWD/claude-args.log"
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$log"
done
` + malformed + `
printf '%s\n' '{"type":"system","subtype":"init"}'
printf '%s\n' '` + resultBody + `'
`

	path := filepath.Join(dir, "fake_claude.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
