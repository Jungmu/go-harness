package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const (
	initializeID  = 1
	threadStartID = 2
	turnStartID   = 3
)

type Event struct {
	Type           string
	At             time.Time
	Message        string
	PayloadSummary string
	SessionID      string
	ThreadID       string
	TurnID         string
	AppServerPID   int
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

type Runner struct {
	cfg    config.CodexConfig
	logger *slog.Logger
}

type streamLine struct {
	source string
	text   string
	err    error
}

type appSession struct {
	stdinCloser   io.Closer
	cancel        context.CancelFunc
	encoder       *json.Encoder
	lines         chan streamLine
	waitCh        chan error
	threadID      string
	workspacePath string
	appServerPID  int
	nextRequestID int
}

func NewRunner(cfg config.CodexConfig, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, logger: logger}
}

func (r *Runner) RunAttempt(ctx context.Context, issue domain.Issue, workspace domain.Workspace, prompt string, attempt int, onEvent func(Event), continueFn ContinueFunc) (RunResult, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	session, err := r.startSession(runCtx, workspace.Path, onEvent)
	if err != nil {
		return RunResult{}, err
	}
	defer session.close()

	result := RunResult{
		RateLimits: map[string]domain.RateLimitSnapshot{},
	}

	currentPrompt := prompt
	turnCount := 1

	for {
		liveSession, err := r.runTurn(runCtx, session, issue, currentPrompt, turnCount, &result, onEvent)
		if err != nil {
			return RunResult{}, err
		}
		result.Session = liveSession

		if continueFn == nil {
			return result, nil
		}

		decision, err := continueFn(runCtx, liveSession)
		if err != nil {
			return RunResult{}, err
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

func (r *Runner) startSession(ctx context.Context, workspacePath string, onEvent func(Event)) (*appSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(sessionCtx, "bash", "-lc", r.cfg.Command)
	cmd.Dir = workspacePath

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
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

	encoder := json.NewEncoder(stdin)
	if err := encodeMessage(encoder, map[string]any{
		"id":     initializeID,
		"method": "initialize",
		"params": map[string]any{
			"capabilities": map[string]any{"experimentalApi": true},
			"clientInfo": map[string]any{
				"name":    "go-harness",
				"title":   "Go Harness",
				"version": "0.1.0",
			},
		},
	}); err != nil {
		cancel()
		return nil, err
	}

	if _, err := r.awaitResponse(sessionCtx, lines, waitCh, initializeID, onEvent); err != nil {
		cancel()
		return nil, err
	}
	if err := encodeMessage(encoder, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		cancel()
		return nil, err
	}
	if err := encodeMessage(encoder, map[string]any{
		"id":     threadStartID,
		"method": "thread/start",
		"params": map[string]any{
			"approvalPolicy": r.cfg.ApprovalPolicy,
			"sandbox":        protocolThreadSandbox(r.cfg.ThreadSandbox),
			"cwd":            workspacePath,
		},
	}); err != nil {
		cancel()
		return nil, err
	}

	threadID, err := r.awaitStarted(sessionCtx, lines, waitCh, threadStartID, "thread/started", "thread", onEvent)
	if threadID == "" {
		cancel()
		if err == nil {
			err = fmt.Errorf("thread/start response missing thread id")
		}
		return nil, err
	}

	return &appSession{
		stdinCloser:   stdin,
		cancel:        cancel,
		encoder:       encoder,
		lines:         lines,
		waitCh:        waitCh,
		threadID:      threadID,
		workspacePath: workspacePath,
		appServerPID:  processPID(cmd),
		nextRequestID: turnStartID,
	}, nil
}

func (r *Runner) runTurn(ctx context.Context, session *appSession, issue domain.Issue, prompt string, turnCount int, result *RunResult, onEvent func(Event)) (domain.LiveSession, error) {
	requestID := session.nextRequestID
	session.nextRequestID++

	if err := encodeMessage(session.encoder, map[string]any{
		"id":     requestID,
		"method": "turn/start",
		"params": map[string]any{
			"threadId": session.threadID,
			"input": []map[string]any{
				{"type": "text", "text": prompt},
			},
			"cwd":            session.workspacePath,
			"title":          fmt.Sprintf("%s: %s", issue.Identifier, issue.Title),
			"approvalPolicy": r.cfg.ApprovalPolicy,
			"sandboxPolicy":  protocolSandboxPolicy(r.cfg.TurnSandboxPolicy),
		},
	}); err != nil {
		return domain.LiveSession{}, err
	}

	turnID, err := r.awaitStarted(ctx, session.lines, session.waitCh, requestID, "turn/started", "turn", onEvent)
	if turnID == "" {
		if err == nil {
			err = fmt.Errorf("turn/start response missing turn id")
		}
		return domain.LiveSession{}, err
	}

	liveSession := domain.LiveSession{
		SessionID:    domain.FormatSessionID(session.threadID, turnID),
		ThreadID:     session.threadID,
		TurnID:       turnID,
		StartedAt:    time.Now().UTC(),
		TurnCount:    turnCount,
		AppServerPID: session.appServerPID,
	}

	eventType := "session_started"
	if turnCount > 1 {
		eventType = "turn_started"
	}
	emit(onEvent, Event{
		Type:         eventType,
		At:           liveSession.StartedAt,
		SessionID:    liveSession.SessionID,
		ThreadID:     liveSession.ThreadID,
		TurnID:       liveSession.TurnID,
		AppServerPID: liveSession.AppServerPID,
		TurnCount:    turnCount,
		Message:      "turn started",
	})

	turnTimer := time.NewTimer(r.cfg.TurnTimeout)
	defer turnTimer.Stop()
	stallTimer := time.NewTimer(r.cfg.StallTimeout)
	defer stallTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return domain.LiveSession{}, ctx.Err()
		case <-turnTimer.C:
			return domain.LiveSession{}, fmt.Errorf("turn_timeout")
		case <-stallTimer.C:
			return domain.LiveSession{}, fmt.Errorf("stall_timeout")
		case err := <-session.waitCh:
			if err == nil {
				return domain.LiveSession{}, fmt.Errorf("app_server_exited")
			}
			if !errors.Is(err, context.Canceled) {
				return domain.LiveSession{}, fmt.Errorf("app_server_exit: %w", err)
			}
			return domain.LiveSession{}, err
		case line, ok := <-session.lines:
			if !ok {
				return domain.LiveSession{}, fmt.Errorf("app_server_stream_closed")
			}
			if line.err != nil {
				return domain.LiveSession{}, line.err
			}

			resetTimer(stallTimer, r.cfg.StallTimeout)

			if line.source == "stderr" {
				emit(onEvent, Event{
					Type:           "stderr",
					At:             time.Now().UTC(),
					Message:        line.text,
					PayloadSummary: truncate(line.text, 200),
					SessionID:      liveSession.SessionID,
					ThreadID:       liveSession.ThreadID,
					TurnID:         liveSession.TurnID,
					AppServerPID:   liveSession.AppServerPID,
					TurnCount:      turnCount,
				})
				continue
			}

			payload, err := decodeLine(line.text)
			if err != nil {
				emit(onEvent, Event{
					Type:           "malformed",
					At:             time.Now().UTC(),
					Message:        err.Error(),
					PayloadSummary: truncate(line.text, 200),
					SessionID:      liveSession.SessionID,
					ThreadID:       liveSession.ThreadID,
					TurnID:         liveSession.TurnID,
					AppServerPID:   liveSession.AppServerPID,
					TurnCount:      turnCount,
				})
				continue
			}

			applyUsageAndRateLimits(payload, result)
			liveSession.InputTokens = result.Totals.InputTokens
			liveSession.OutputTokens = result.Totals.OutputTokens
			liveSession.TotalTokens = result.Totals.TotalTokens

			if method := stringField(payload, "method"); method != "" {
				switch method {
				case "turn/completed":
					emit(onEvent, r.makeEvent("turn_completed", payload, liveSession, turnCount))
					return liveSession, nil
				case "turn/failed":
					emit(onEvent, r.makeEvent("turn_failed", payload, liveSession, turnCount))
					return domain.LiveSession{}, fmt.Errorf("turn_failed")
				case "turn/cancelled":
					emit(onEvent, r.makeEvent("turn_cancelled", payload, liveSession, turnCount))
					return domain.LiveSession{}, fmt.Errorf("turn_cancelled")
				}

				handled, err := r.handleProtocolMethod(session.encoder, payload, liveSession, turnCount, onEvent)
				if err != nil {
					return domain.LiveSession{}, err
				}
				if handled {
					continue
				}

				emit(onEvent, r.makeEvent("notification", payload, liveSession, turnCount))
			}
		}
	}
}

func (r *Runner) awaitResponse(ctx context.Context, lines <-chan streamLine, waitCh <-chan error, id int, onEvent func(Event)) (map[string]any, error) {
	timer := time.NewTimer(r.cfg.ReadTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("read_timeout waiting for response %d", id)
		case err := <-waitCh:
			if err == nil {
				return nil, fmt.Errorf("app_server_exited")
			}
			if !errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, err
		case line, ok := <-lines:
			if !ok {
				return nil, fmt.Errorf("stream closed while waiting for response %d", id)
			}
			if line.err != nil {
				return nil, line.err
			}
			if line.source == "stderr" {
				emit(onEvent, Event{Type: "stderr", At: time.Now().UTC(), Message: line.text, PayloadSummary: truncate(line.text, 200)})
				continue
			}

			payload, err := decodeLine(line.text)
			if err != nil {
				emit(onEvent, Event{Type: "malformed", At: time.Now().UTC(), Message: err.Error(), PayloadSummary: truncate(line.text, 200)})
				continue
			}
			if intField(payload, "id") == id {
				if rpcErr := rpcError(payload); rpcErr != nil {
					return nil, rpcErr
				}
			}
			if intField(payload, "id") == id {
				return mapField(payload, "result"), nil
			}
			emit(onEvent, Event{
				Type:           "startup_notification",
				At:             time.Now().UTC(),
				PayloadSummary: truncate(line.text, 200),
			})
		}
	}
}

func (r *Runner) awaitStarted(ctx context.Context, lines <-chan streamLine, waitCh <-chan error, responseID int, notificationMethod, entityKey string, onEvent func(Event)) (string, error) {
	timer := time.NewTimer(r.cfg.ReadTimeout)
	defer timer.Stop()

	entityID := ""
	lastSummary := ""

	for {
		if entityID != "" {
			return entityID, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			if lastSummary != "" {
				return "", fmt.Errorf("read_timeout waiting for %s; last_event=%s", notificationMethod, lastSummary)
			}
			return "", fmt.Errorf("read_timeout waiting for %s", notificationMethod)
		case err := <-waitCh:
			if err == nil {
				return "", fmt.Errorf("app_server_exited")
			}
			if !errors.Is(err, context.Canceled) {
				return "", err
			}
			return "", err
		case line, ok := <-lines:
			if !ok {
				return "", fmt.Errorf("stream closed while waiting for %s", notificationMethod)
			}
			if line.err != nil {
				return "", line.err
			}
			if line.source == "stderr" {
				lastSummary = truncate(line.text, 200)
				emit(onEvent, Event{Type: "stderr", At: time.Now().UTC(), Message: line.text, PayloadSummary: truncate(line.text, 200)})
				continue
			}

			payload, err := decodeLine(line.text)
			if err != nil {
				lastSummary = truncate(line.text, 200)
				emit(onEvent, Event{Type: "malformed", At: time.Now().UTC(), Message: err.Error(), PayloadSummary: truncate(line.text, 200)})
				continue
			}

			if intField(payload, "id") == responseID {
				if rpcErr := rpcError(payload); rpcErr != nil {
					return "", rpcErr
				}
				if result := mapField(payload, "result"); result != nil && entityID == "" {
					entityID = stringField(mapField(result, entityKey), "id")
				}
				continue
			}

			if stringField(payload, "method") == notificationMethod {
				if params := mapField(payload, "params"); params != nil && entityID == "" {
					entityID = stringField(mapField(params, entityKey), "id")
				}
				continue
			}

			lastSummary = truncate(line.text, 200)
			emit(onEvent, Event{
				Type:           "startup_notification",
				At:             time.Now().UTC(),
				PayloadSummary: truncate(line.text, 200),
			})
		}
	}
}

func (r *Runner) handleProtocolMethod(encoder *json.Encoder, payload map[string]any, session domain.LiveSession, turnCount int, onEvent func(Event)) (bool, error) {
	method := stringField(payload, "method")

	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		if r.cfg.ApprovalPolicy != "never" {
			return false, fmt.Errorf("approval_required")
		}
		if err := encodeMessage(encoder, map[string]any{
			"id":     payload["id"],
			"result": map[string]any{"decision": "acceptForSession"},
		}); err != nil {
			return false, err
		}
		emit(onEvent, r.makeEvent("approval_auto_approved", payload, session, turnCount))
		return true, nil
	case "execCommandApproval", "applyPatchApproval":
		if r.cfg.ApprovalPolicy != "never" {
			return false, fmt.Errorf("approval_required")
		}
		if err := encodeMessage(encoder, map[string]any{
			"id":     payload["id"],
			"result": map[string]any{"decision": "approved_for_session"},
		}); err != nil {
			return false, err
		}
		emit(onEvent, r.makeEvent("approval_auto_approved", payload, session, turnCount))
		return true, nil
	case "item/tool/call":
		params := mapField(payload, "params")
		toolName := stringField(params, "tool")
		if toolName == "" {
			toolName = stringField(params, "name")
		}
		output := fmt.Sprintf("unsupported dynamic tool call: %s", strings.TrimSpace(toolName))
		if err := encodeMessage(encoder, map[string]any{
			"id": payload["id"],
			"result": map[string]any{
				"success": false,
				"output":  output,
				"contentItems": []map[string]any{
					{"type": "inputText", "text": output},
				},
			},
		}); err != nil {
			return false, err
		}
		emit(onEvent, r.makeEvent("unsupported_tool_call", payload, session, turnCount))
		return true, nil
	case "item/tool/requestUserInput":
		return false, fmt.Errorf("user_input_required")
	default:
		return false, nil
	}
}

func (r *Runner) makeEvent(eventType string, payload map[string]any, session domain.LiveSession, turnCount int) Event {
	event := Event{
		Type:           eventType,
		At:             time.Now().UTC(),
		Message:        stringField(payload, "method"),
		PayloadSummary: truncate(payloadSummary(payload), 240),
		SessionID:      session.SessionID,
		ThreadID:       session.ThreadID,
		TurnID:         session.TurnID,
		AppServerPID:   session.AppServerPID,
		TurnCount:      turnCount,
	}
	if usage, ok := usageFromPayload(payload); ok {
		event.Usage = usage
	}
	if rateLimit, ok := rateLimitFromPayload(payload); ok {
		event.RateLimit = &rateLimit
	}
	return event
}

func applyUsageAndRateLimits(payload map[string]any, result *RunResult) {
	if usage, ok := usageFromPayload(payload); ok {
		result.Totals = usage
		result.Session.InputTokens = usage.InputTokens
		result.Session.OutputTokens = usage.OutputTokens
		result.Session.TotalTokens = usage.TotalTokens
	}
	if snapshot, ok := rateLimitFromPayload(payload); ok {
		result.RateLimits[snapshot.Provider] = snapshot
	}
}

func usageFromPayload(payload map[string]any) (domain.RuntimeTotals, bool) {
	candidates := []map[string]any{
		payload,
		mapField(payload, "params"),
		mapField(payload, "result"),
		mapField(payload, "details"),
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		usage := mapField(candidate, "usage")
		if usage == nil {
			continue
		}
		result := domain.RuntimeTotals{
			InputTokens:  intFieldFlexible(usage, "inputTokens", "input_tokens"),
			OutputTokens: intFieldFlexible(usage, "outputTokens", "output_tokens"),
			TotalTokens:  intFieldFlexible(usage, "totalTokens", "total_tokens"),
		}
		return result, true
	}
	return domain.RuntimeTotals{}, false
}

func rateLimitFromPayload(payload map[string]any) (domain.RateLimitSnapshot, bool) {
	candidates := []map[string]any{
		payload,
		mapField(payload, "params"),
		mapField(payload, "result"),
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		raw := mapField(candidate, "rateLimits")
		if raw == nil {
			raw = mapField(candidate, "rate_limits")
		}
		if raw != nil {
			return domain.RateLimitSnapshot{
				Provider:  "codex",
				UpdatedAt: time.Now().UTC(),
				Raw:       raw,
			}, true
		}
	}
	return domain.RateLimitSnapshot{}, false
}

func pumpLines(source string, reader io.Reader, lines chan<- streamLine, wg *sync.WaitGroup) {
	defer wg.Done()

	buffered := bufio.NewReader(reader)
	for {
		line, err := buffered.ReadString('\n')
		if line != "" {
			lines <- streamLine{
				source: source,
				text:   strings.TrimRight(line, "\r\n"),
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				lines <- streamLine{source: source, err: err}
			}
			return
		}
	}
}

func encodeMessage(encoder *json.Encoder, payload map[string]any) error {
	return encoder.Encode(payload)
}

func decodeLine(line string) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func emit(onEvent func(Event), event Event) {
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

func payloadSummary(payload map[string]any) string {
	if method := stringField(payload, "method"); method != "" {
		return method
	}
	if raw, err := json.Marshal(payload); err == nil {
		return string(raw)
	}
	return ""
}

func stringField(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	value, _ := raw[key].(string)
	return value
}

func intField(raw map[string]any, key string) int {
	if raw == nil {
		return 0
	}
	switch typed := raw[key].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

func intFieldFlexible(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		if value := intField(raw, key); value != 0 {
			return value
		}
	}
	return 0
}

func mapField(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	value, _ := raw[key].(map[string]any)
	return value
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func protocolThreadSandbox(value string) string {
	switch strings.TrimSpace(value) {
	case "workspace-write":
		return "workspaceWrite"
	case "read-only":
		return "readOnly"
	case "danger-full-access":
		return "dangerFullAccess"
	default:
		return value
	}
}

func protocolSandboxPolicy(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}

	normalized := make(map[string]any, len(value))
	for key, raw := range value {
		normalized[key] = raw
	}

	if sandboxType, ok := normalized["type"].(string); ok {
		normalized["type"] = protocolThreadSandbox(sandboxType)
	}
	return normalized
}

func rpcError(payload map[string]any) error {
	raw := mapField(payload, "error")
	if raw == nil {
		return nil
	}

	message := stringField(raw, "message")
	if message == "" {
		message = payloadSummary(raw)
	}
	if message == "" {
		message = "jsonrpc error"
	}
	return errors.New(message)
}

func (s *appSession) close() {
	if s.stdinCloser != nil {
		_ = s.stdinCloser.Close()
	}
	s.cancel()
	select {
	case <-s.waitCh:
	case <-time.After(2 * time.Second):
	}
}
