package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"log/slog"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const testAppServerTimeout = 3 * time.Second

func TestRunnerRunAttemptHandshakeAndUsage(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeAppServer(t, workspace.Path, 1, false)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}

	if result.Session.SessionID != "thread-1-turn-1" {
		t.Fatalf("SessionID = %q", result.Session.SessionID)
	}
	if result.Totals.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want 15", result.Totals.TotalTokens)
	}

	transcript, err := os.ReadFile(filepath.Join(workspace.Path, "transcript.log"))
	if err != nil {
		t.Fatalf("ReadFile(transcript.log) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(transcript)), "\n")
	if len(lines) != 4 {
		t.Fatalf("transcript lines = %d, want 4; transcript=%q", len(lines), string(transcript))
	}
	if !strings.Contains(lines[2], `"sandbox":"workspaceWrite"`) {
		t.Fatalf("thread/start request = %q, want workspaceWrite sandbox", lines[2])
	}
	if !strings.Contains(lines[3], `"sandboxPolicy":{"type":"workspaceWrite"}`) {
		t.Fatalf("turn/start request = %q, want workspaceWrite sandboxPolicy", lines[3])
	}
}

func TestRunnerUnsupportedToolCallContinues(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeAppServer(t, workspace.Path, 1, true)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var events []string
	_, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, func(event Event) {
		events = append(events, event.Type)
	}, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if !slices.Contains(events, "unsupported_tool_call") {
		t.Fatalf("events = %#v, want unsupported_tool_call", events)
	}
}

func TestRunnerCapturePromptsWritesTranscriptJSONL(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeAppServer(t, workspace.Path, 1, false)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{CapturePrompts: true}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	_, err := runner.RunAttempt(context.Background(), domain.Issue{ID: "1", Identifier: "ABC-1", Title: "Example"}, workspace, "first line\nsecond line", 1, nil, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}

	path := filepath.Join(filepath.Dir(workspace.Path), promptTranscriptDirname, "ABC-1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}

	var entries []transcriptEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry transcriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("Unmarshal(transcript line) error = %v; line=%q", err, line)
		}
		entries = append(entries, entry)
	}

	if !containsTranscriptEntry(entries, "send", "prompt", "first line\nsecond line") {
		t.Fatalf("prompt transcript missing rendered prompt: %#v", entries)
	}
	if !containsTranscriptEntry(entries, "send", "stdin", `"method":"turn/start"`) {
		t.Fatalf("prompt transcript missing turn/start request: %#v", entries)
	}
	if !containsTranscriptEntry(entries, "recv", "stdout", `"method":"turn/completed"`) {
		t.Fatalf("prompt transcript missing turn/completed response: %#v", entries)
	}
}

func TestRunnerContinuationReusesThreadAndStartsSecondTurn(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeAppServer(t, workspace.Path, 2, false)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var startedEvents []Event
	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example", State: "In Progress"}, workspace, "prompt", 1, func(event Event) {
		if event.Type == "session_started" || event.Type == "turn_started" {
			startedEvents = append(startedEvents, event)
		}
	}, func(_ context.Context, session domain.LiveSession) (ContinueDecision, error) {
		if session.TurnCount == 1 {
			return ContinueDecision{
				Continue:       true,
				NextPrompt:     "continue prompt",
				RefreshedIssue: &domain.Issue{Identifier: "ABC-1", State: "In Progress"},
			}, nil
		}
		return ContinueDecision{
			Continue:       false,
			RefreshedIssue: &domain.Issue{Identifier: "ABC-1", State: "Done"},
		}, nil
	})
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if result.Session.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %q, want thread-1", result.Session.ThreadID)
	}
	if result.Session.TurnID != "turn-2" {
		t.Fatalf("TurnID = %q, want turn-2", result.Session.TurnID)
	}
	if result.Session.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", result.Session.TurnCount)
	}
	if len(startedEvents) != 2 {
		t.Fatalf("started events = %#v", startedEvents)
	}
	if startedEvents[0].ThreadID != startedEvents[1].ThreadID {
		t.Fatalf("thread ids differ across continuation: %#v", startedEvents)
	}

	transcript, err := os.ReadFile(filepath.Join(workspace.Path, "transcript.log"))
	if err != nil {
		t.Fatalf("ReadFile(transcript.log) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(transcript)), "\n")
	if len(lines) != 5 {
		t.Fatalf("transcript lines = %d, want 5; transcript=%q", len(lines), string(transcript))
	}
}

func TestRunnerUsesStartedNotificationsWhenResponsesAreEmpty(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeNotificationAppServer(t, workspace.Path)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if result.Session.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %q, want thread-1", result.Session.ThreadID)
	}
	if result.Session.TurnID != "turn-1" {
		t.Fatalf("TurnID = %q, want turn-1", result.Session.TurnID)
	}
}

func TestRunnerUsesStartedNotificationsWithoutResponses(t *testing.T) {
	t.Parallel()

	workspace := domain.Workspace{Path: t.TempDir(), WorkspaceKey: "ABC-1"}
	script := writeFakeNotificationOnlyAppServer(t, workspace.Path)
	runner := NewRunner(config.CodexConfig{
		Command:           `"` + script + `"`,
		ApprovalPolicy:    "never",
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
		TurnTimeout:       testAppServerTimeout,
		ReadTimeout:       testAppServerTimeout,
		StallTimeout:      testAppServerTimeout,
	}, config.LoggingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result, err := runner.RunAttempt(context.Background(), domain.Issue{Identifier: "ABC-1", Title: "Example"}, workspace, "prompt", 1, nil, nil)
	if err != nil {
		t.Fatalf("RunAttempt() error = %v", err)
	}
	if result.Session.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %q, want thread-1", result.Session.ThreadID)
	}
	if result.Session.TurnID != "turn-1" {
		t.Fatalf("TurnID = %q, want turn-1", result.Session.TurnID)
	}
}

func writeFakeAppServer(t *testing.T, dir string, turns int, emitToolCall bool) string {
	t.Helper()

	modeGuard := ""
	if emitToolCall {
		modeGuard = `
      if [ "$turn" -eq 1 ]; then
        printf '%s\n' '{"id":"tool-1","method":"item/tool/call","params":{"tool":"tracker_get_issue","arguments":{}}}'
        IFS= read -r toolresp
        printf '%s\n' "$toolresp" >> "$log"
      fi
`
	}

	script := `#!/bin/sh
set -eu
log="$PWD/transcript.log"
turn=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$log"
  request_id=$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"id":'"$request_id"',"result":{"protocolVersion":"1.0"}}'
      ;;
    *'"method":"initialized"'*)
      ;;
    *'"method":"thread/start"'*)
      printf '%s\n' '{"id":'"$request_id"',"result":{"thread":{"id":"thread-1"}}}'
      ;;
    *'"method":"turn/start"'*)
      turn=$((turn + 1))
      printf '%s\n' '{"id":'"$request_id"',"result":{"turn":{"id":"turn-'"$turn"'"}}}'
` + modeGuard + `
      printf '%s\n' '{"method":"turn/completed","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}'
      ;;
    *)
      ;;
  esac
done
`

	path := filepath.Join(dir, "fake_app_server.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsTranscriptEntry(entries []transcriptEntry, direction, channel, snippet string) bool {
	for _, entry := range entries {
		if entry.Direction == direction && entry.Channel == channel && strings.Contains(entry.Payload, snippet) {
			return true
		}
	}
	return false
}

func writeFakeNotificationAppServer(t *testing.T, dir string) string {
	t.Helper()

	script := `#!/bin/sh
set -eu
log="$PWD/transcript.log"
turn=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$log"
  request_id=$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"id":'"$request_id"',"result":{"protocolVersion":"1.0"}}'
      ;;
    *'"method":"initialized"'*)
      ;;
    *'"method":"thread/start"'*)
      printf '%s\n' '{"id":'"$request_id"',"result":{}}'
      printf '%s\n' '{"method":"thread/started","params":{"thread":{"id":"thread-1"}}}'
      ;;
    *'"method":"turn/start"'*)
      turn=$((turn + 1))
      printf '%s\n' '{"id":'"$request_id"',"result":{}}'
      printf '%s\n' '{"method":"turn/started","params":{"turn":{"id":"turn-'"$turn"'"}}}'
      printf '%s\n' '{"method":"turn/completed","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}'
      ;;
    *)
      ;;
  esac
done
`

	path := filepath.Join(dir, "fake_notification_app_server.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFakeNotificationOnlyAppServer(t *testing.T, dir string) string {
	t.Helper()

	script := `#!/bin/sh
set -eu
log="$PWD/transcript.log"
turn=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$log"
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"id":1,"result":{"protocolVersion":"1.0"}}'
      ;;
    *'"method":"initialized"'*)
      ;;
    *'"method":"thread/start"'*)
      printf '%s\n' '{"method":"thread/started","params":{"thread":{"id":"thread-1"}}}'
      ;;
    *'"method":"turn/start"'*)
      turn=$((turn + 1))
      printf '%s\n' '{"method":"turn/started","params":{"turn":{"id":"turn-'"$turn"'"}}}'
      printf '%s\n' '{"method":"turn/completed","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}'
      ;;
    *)
      ;;
  esac
done
`

	path := filepath.Join(dir, "fake_notification_only_app_server.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
