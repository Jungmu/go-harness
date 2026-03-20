package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-harness/internal/agent"
	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const providerName = "claude"

type Runner struct {
	cfg     config.ClaudeConfig
	logging config.LoggingConfig
	logger  *slog.Logger
}

type streamLine struct {
	source string
	text   string
	err    error
}

func NewRunner(cfg config.ClaudeConfig, logging config.LoggingConfig, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, logging: logging, logger: logger}
}

func (r *Runner) RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(agent.Event), continueFn agent.ContinueFunc) (agent.RunResult, error) {
	recorder := agent.NewTranscriptRecorder(r.logging.CapturePrompts, providerName, issue, workspace, attempt, r.logger)

	result := agent.RunResult{
		RateLimits: map[string]domain.RateLimitSnapshot{},
	}
	currentPrompt := prompt
	turnCount := 1
	conversationID := ""

	for {
		liveSession, err := r.runTurn(ctx, workspace, issue, currentPrompt, turnCount, conversationID, recorder, onEvent)
		if err != nil {
			return agent.RunResult{}, err
		}
		result.Session = liveSession
		conversationID = liveSession.ConversationID

		if continueFn == nil {
			return result, nil
		}

		decision, err := continueFn(ctx, liveSession)
		if err != nil {
			return agent.RunResult{}, err
		}
		result.RefreshedIssue = decision.RefreshedIssue
		result.StopReason = decision.StopReason
		if !decision.Continue {
			return result, nil
		}

		currentPrompt = decision.NextPrompt
		turnCount++
	}
}

func (r *Runner) runTurn(ctx context.Context, workspace domain.Workspace, issue domain.Issue, prompt string, turnCount int, conversationID string, recorder *agent.TranscriptRecorder, onEvent func(agent.Event)) (domain.LiveSession, error) {
	runCtx, cancel := context.WithTimeout(ctx, r.cfg.TurnTimeout)
	defer cancel()

	command := strings.TrimSpace(r.cfg.Command)
	if command == "" {
		return domain.LiveSession{}, fmt.Errorf("claude command is empty")
	}

	args := []string{"-p", "--verbose", "--output-format", "stream-json", "--permission-mode", r.cfg.PermissionMode}
	if conversationID != "" {
		args = append(args, "-r", conversationID)
	}
	if model := strings.TrimSpace(r.cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if fallback := strings.TrimSpace(r.cfg.FallbackModel); fallback != "" {
		args = append(args, "--fallback-model", fallback)
	}
	if effort := strings.TrimSpace(r.cfg.Effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if allowed := strings.Join(r.cfg.AllowedTools, ","); strings.TrimSpace(allowed) != "" {
		args = append(args, "--allowedTools", allowed)
	}
	if disallowed := strings.Join(r.cfg.DisallowedTools, ","); strings.TrimSpace(disallowed) != "" {
		args = append(args, "--disallowedTools", disallowed)
	}
	args = append(args, prompt)

	shellArgs := append([]string{"-lc", command + ` "$@"`, "claude"}, args...)
	cmd := exec.CommandContext(runCtx, "bash", shellArgs...)
	cmd.Dir = workspace.Path

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return domain.LiveSession{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return domain.LiveSession{}, err
	}
	if err := cmd.Start(); err != nil {
		return domain.LiveSession{}, err
	}

	lines := make(chan streamLine, 64)
	waitCh := make(chan error, 1)
	var pumps sync.WaitGroup
	pumps.Add(2)
	go pumpLines("stdout", stdout, lines, &pumps)
	go pumpLines("stderr", stderr, lines, &pumps)
	go func() {
		pumps.Wait()
		close(lines)
	}()
	go func() {
		waitCh <- cmd.Wait()
	}()

	startedAt := time.Now().UTC()
	liveSession := domain.LiveSession{
		Provider:       providerName,
		ConversationID: conversationID,
		SessionID:      synthesizeTurnSessionID(conversationID, turnCount),
		StartedAt:      startedAt,
		TurnCount:      turnCount,
		RuntimePID:     processPID(cmd),
	}
	recorder.RecordPrompt(prompt, liveSession, turnCount)

	eventType := "session_started"
	if turnCount > 1 {
		eventType = "turn_started"
	}
	emit(onEvent, agent.Event{
		Provider:   providerName,
		Type:       eventType,
		At:         startedAt,
		RuntimePID: liveSession.RuntimePID,
		TurnCount:  turnCount,
		Message:    "turn started",
	})

	readTimer := time.NewTimer(r.cfg.ReadTimeout)
	defer readTimer.Stop()
	turnTimer := time.NewTimer(r.cfg.TurnTimeout)
	defer turnTimer.Stop()
	stallTimer := time.NewTimer(r.cfg.StallTimeout)
	defer stallTimer.Stop()

	sawOutput := false
	sawResult := false

	for {
		select {
		case <-ctx.Done():
			return domain.LiveSession{}, ctx.Err()
		case <-readTimer.C:
			if !sawOutput {
				return domain.LiveSession{}, fmt.Errorf("read_timeout")
			}
		case <-turnTimer.C:
			return domain.LiveSession{}, fmt.Errorf("turn_timeout")
		case <-stallTimer.C:
			return domain.LiveSession{}, fmt.Errorf("stall_timeout")
		case err := <-waitCh:
			if !sawResult {
				if err == nil {
					return domain.LiveSession{}, fmt.Errorf("claude_exited_without_result")
				}
				if !errors.Is(err, context.Canceled) {
					return domain.LiveSession{}, fmt.Errorf("claude_exit: %w", err)
				}
				return domain.LiveSession{}, err
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				return domain.LiveSession{}, fmt.Errorf("claude_exit: %w", err)
			}
			return liveSession, nil
		case line, ok := <-lines:
			if !ok {
				if sawResult {
					return liveSession, nil
				}
				return domain.LiveSession{}, fmt.Errorf("claude_stream_closed")
			}
			if line.err != nil {
				return domain.LiveSession{}, line.err
			}

			sawOutput = true
			resetTimer(stallTimer, r.cfg.StallTimeout)
			recorder.RecordIO("recv", line.source, line.text, liveSession, turnCount)

			if line.source == "stderr" {
				emit(onEvent, agent.Event{
					Provider:       providerName,
					Type:           "stderr",
					At:             time.Now().UTC(),
					Message:        line.text,
					PayloadSummary: truncate(line.text, 200),
					SessionID:      liveSession.SessionID,
					ConversationID: liveSession.ConversationID,
					TurnID:         liveSession.TurnID,
					RuntimePID:     liveSession.RuntimePID,
					TurnCount:      turnCount,
				})
				continue
			}

			payload, err := decodeLine(line.text)
			if err != nil {
				emit(onEvent, agent.Event{
					Provider:       providerName,
					Type:           "malformed",
					At:             time.Now().UTC(),
					Message:        err.Error(),
					PayloadSummary: truncate(line.text, 200),
					SessionID:      liveSession.SessionID,
					ConversationID: liveSession.ConversationID,
					RuntimePID:     liveSession.RuntimePID,
					TurnCount:      turnCount,
				})
				continue
			}

			if id := stringField(payload, "session_id"); id != "" {
				liveSession.ConversationID = id
				liveSession.SessionID = synthesizeTurnSessionID(id, turnCount)
			}
			if liveSession.ConversationID == "" {
				if id := stringField(payload, "conversation_id"); id != "" {
					liveSession.ConversationID = id
					liveSession.SessionID = synthesizeTurnSessionID(id, turnCount)
				}
			}

			event := agent.Event{
				Provider:       providerName,
				At:             time.Now().UTC(),
				Message:        stringField(payload, "type"),
				PayloadSummary: truncate(line.text, 240),
				SessionID:      liveSession.SessionID,
				ConversationID: liveSession.ConversationID,
				TurnID:         liveSession.TurnID,
				RuntimePID:     liveSession.RuntimePID,
				TurnCount:      turnCount,
			}

			switch stringField(payload, "type") {
			case "result":
				sawResult = true
				if liveSession.ConversationID == "" {
					emit(onEvent, withType(event, "turn_failed"))
					return domain.LiveSession{}, errors.New("missing session_id in claude result")
				}
				if message := stringField(payload, "result"); message != "" {
					liveSession.LastMessage = message
					event.Message = message
				}
				if isError(payload) {
					emit(onEvent, withType(event, "turn_failed"))
					return domain.LiveSession{}, resultError(payload)
				}
				emit(onEvent, withType(event, "turn_completed"))
			default:
				emit(onEvent, withType(event, "notification"))
			}
		}
	}
}

func synthesizeTurnSessionID(conversationID string, turnCount int) string {
	if strings.TrimSpace(conversationID) == "" || turnCount <= 0 {
		return ""
	}
	return conversationID + "-turn-" + strconv.Itoa(turnCount)
}

func withType(event agent.Event, eventType string) agent.Event {
	event.Type = eventType
	return event
}

func isError(payload map[string]any) bool {
	value, ok := payload["is_error"]
	if !ok {
		return false
	}
	typed, _ := value.(bool)
	return typed
}

func resultError(payload map[string]any) error {
	message := strings.TrimSpace(stringField(payload, "result"))
	if message == "" {
		message = strings.TrimSpace(stringField(payload, "subtype"))
	}
	if message == "" {
		message = "claude turn failed"
	}
	return errors.New(message)
}

func pumpLines(source string, reader io.Reader, lines chan<- streamLine, wg *sync.WaitGroup) {
	defer wg.Done()

	buffered := bufio.NewReader(reader)
	for {
		line, err := buffered.ReadString('\n')
		if line != "" {
			lines <- streamLine{source: source, text: strings.TrimRight(line, "\r\n")}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				lines <- streamLine{source: source, err: err}
			}
			return
		}
	}
}

func decodeLine(line string) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func emit(onEvent func(agent.Event), event agent.Event) {
	if onEvent != nil {
		onEvent(event)
	}
}

func processPID(cmd *exec.Cmd) int {
	if cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func stringField(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	value, _ := raw[key].(string)
	return value
}
