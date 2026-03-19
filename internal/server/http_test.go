package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-harness/internal/domain"
)

func TestHandlerStateIssueAndRefreshEndpoints(t *testing.T) {
	t.Parallel()

	var refreshCount int
	handler := NewHandler(
		func() domain.StateSnapshot {
			return domain.StateSnapshot{
				GeneratedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC),
				Workflow:    domain.WorkflowStatus{Path: "/repo/WORKFLOW.md", ReviewPath: "/repo/REVIEW-WORKFLOW.md"},
				Environment: domain.EnvironmentStatus{
					DotEnvPath:    "/repo/.env",
					DotEnvPresent: true,
					Entries: []domain.EnvironmentEntry{
						{Name: "LINEAR_API_KEY", Value: "<redacted>", Source: ".env"},
					},
				},
				Counts: domain.SnapshotCounts{Running: 1},
				Dispatch: domain.DispatchStatus{
					Blocked: true,
					Error:   "review: invalid workflow",
					Workers: []domain.WorkerDispatchStatus{
						{Worker: "coding", Blocked: false},
						{Worker: "review", Blocked: true, Error: "invalid workflow"},
					},
				},
				RecentActivity: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 18, 8, 59, 0, 0, time.UTC), Identifier: "ABC-1", Event: "issue_claimed"},
				},
			}
		},
		func(identifier string) (domain.IssueRuntimeSnapshot, bool) {
			if identifier != "ABC-1" {
				return domain.IssueRuntimeSnapshot{}, false
			}
			return domain.IssueRuntimeSnapshot{
				GeneratedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC),
				Identifier:  identifier,
				Status:      "running",
				History: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 18, 8, 59, 0, 0, time.UTC), Identifier: identifier, Event: "issue_claimed"},
				},
				PromptTranscript: []domain.PromptTranscriptEntry{
					{At: time.Date(2026, 3, 18, 8, 58, 0, 0, time.UTC), Attempt: 1, Channel: "prompt", Payload: "Handle ABC-1"},
				},
			}, true
		},
		func() { refreshCount++ },
	)

	stateReq := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	stateRes := httptest.NewRecorder()
	handler.ServeHTTP(stateRes, stateReq)
	if stateRes.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/state status = %d, want 200", stateRes.Code)
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRes := httptest.NewRecorder()
	handler.ServeHTTP(healthRes, healthReq)
	if healthRes.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", healthRes.Code)
	}
	if !strings.Contains(healthRes.Body.String(), `"blocked":true`) {
		t.Fatalf("GET /healthz body = %q, want dispatch blocked marker", healthRes.Body.String())
	}
	if !strings.Contains(healthRes.Body.String(), `/repo/WORKFLOW.md`) {
		t.Fatalf("GET /healthz body = %q, want workflow path", healthRes.Body.String())
	}
	if !strings.Contains(healthRes.Body.String(), `/repo/REVIEW-WORKFLOW.md`) {
		t.Fatalf("GET /healthz body = %q, want review workflow path", healthRes.Body.String())
	}
	if !strings.Contains(healthRes.Body.String(), `/repo/.env`) {
		t.Fatalf("GET /healthz body = %q, want env path", healthRes.Body.String())
	}

	issueReq := httptest.NewRequest(http.MethodGet, "/api/v1/issues/ABC-1", nil)
	issueRes := httptest.NewRecorder()
	handler.ServeHTTP(issueRes, issueReq)
	if issueRes.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/ABC-1 status = %d, want 200", issueRes.Code)
	}

	var issuePayload domain.IssueRuntimeSnapshot
	if err := json.NewDecoder(issueRes.Body).Decode(&issuePayload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if issuePayload.Identifier != "ABC-1" || issuePayload.Status != "running" {
		t.Fatalf("issue payload = %#v", issuePayload)
	}
	if len(issuePayload.History) != 1 || issuePayload.History[0].Event != "issue_claimed" {
		t.Fatalf("issue history = %#v", issuePayload.History)
	}
	if len(issuePayload.PromptTranscript) != 1 || issuePayload.PromptTranscript[0].Channel != "prompt" {
		t.Fatalf("issue prompt transcript = %#v", issuePayload.PromptTranscript)
	}

	issuePageReq := httptest.NewRequest(http.MethodGet, "/issues/ABC-1", nil)
	issuePageRes := httptest.NewRecorder()
	handler.ServeHTTP(issuePageRes, issuePageReq)
	if issuePageRes.Code != http.StatusOK {
		t.Fatalf("GET /issues/ABC-1 status = %d, want 200", issuePageRes.Code)
	}
	if !strings.Contains(issuePageRes.Body.String(), "Prompt Transcript") || !strings.Contains(issuePageRes.Body.String(), "Handle ABC-1") {
		t.Fatalf("issue page body = %q, want prompt transcript", issuePageRes.Body.String())
	}

	refreshReq := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	refreshRes := httptest.NewRecorder()
	handler.ServeHTTP(refreshRes, refreshReq)
	if refreshRes.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/refresh status = %d, want 202", refreshRes.Code)
	}
	if refreshCount != 1 {
		t.Fatalf("refreshCount = %d, want 1", refreshCount)
	}
}

func TestHandlerDashboardRendersSnapshot(t *testing.T) {
	t.Parallel()

	handler := NewHandler(
		func() domain.StateSnapshot {
			return domain.StateSnapshot{
				GeneratedAt: time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC),
				Workflow:    domain.WorkflowStatus{Path: "/repo/WORKFLOW.md", ReviewPath: "/repo/REVIEW-WORKFLOW.md"},
				Environment: domain.EnvironmentStatus{
					DotEnvPath:    "/repo/.env",
					DotEnvPresent: true,
					Entries: []domain.EnvironmentEntry{
						{Name: "LINEAR_API_KEY", Value: "<redacted>", Source: ".env"},
						{Name: "GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG", Value: "test", Source: "process"},
					},
				},
				Counts: domain.SnapshotCounts{Running: 1, Retrying: 1},
				Dispatch: domain.DispatchStatus{
					Blocked: true,
					Error:   "review: invalid workflow",
					Workers: []domain.WorkerDispatchStatus{
						{Worker: "coding", Blocked: false},
						{Worker: "review", Blocked: true, Error: "invalid workflow"},
					},
				},
				Running: []domain.RunningSnapshot{
					{
						Issue:     domain.Issue{Identifier: "ABC-1", Title: "Example", State: "In Progress"},
						Attempt:   2,
						Workspace: domain.Workspace{Path: "/tmp/ABC-1"},
						LiveSession: &domain.LiveSession{
							SessionID: "thread-1-turn-2",
							Worker:    "coding",
						},
					},
				},
				Retrying: []domain.RetryEntry{
					{Identifier: "ABC-2", Attempt: 3, Reason: "attempt_failed", DueAt: time.Date(2026, 3, 18, 9, 1, 0, 0, time.UTC), LastError: "git fetch failed: not a git repository"},
				},
				RecentActivity: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 18, 9, 0, 30, 0, time.UTC), Identifier: "ABC-1", Event: "issue_claimed", Attempt: 2, Message: "issue claimed for execution"},
					{At: time.Date(2026, 3, 18, 9, 0, 45, 0, time.UTC), Identifier: "ABC-1", Event: "tracker_state_transition", StateBefore: "Todo", StateAfter: "In Progress", Message: "issue moved to in-progress"},
				},
				Completed: []string{"ABC-3"},
			}
		},
		func(identifier string) (domain.IssueRuntimeSnapshot, bool) {
			switch identifier {
			case "ABC-1":
				return domain.IssueRuntimeSnapshot{
					Identifier: identifier,
					Status:     "running",
					PromptTranscript: []domain.PromptTranscriptEntry{
						{At: time.Date(2026, 3, 18, 9, 0, 40, 0, time.UTC), Attempt: 2, Channel: "prompt", Payload: "Implement the fix for ABC-1"},
					},
				}, true
			case "ABC-2":
				return domain.IssueRuntimeSnapshot{
					Identifier: identifier,
					Status:     "retrying",
					PromptTranscript: []domain.PromptTranscriptEntry{
						{At: time.Date(2026, 3, 18, 9, 0, 55, 0, time.UTC), Attempt: 3, Channel: "stderr", Payload: "git fetch failed"},
					},
				}, true
			default:
				return domain.IssueRuntimeSnapshot{}, false
			}
		},
		func() {},
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.Code)
	}
	if contentType := res.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", contentType)
	}
	body := res.Body.String()
	for _, expected := range []string{"Go Harness Control Panel", "Dispatch blocked", "/repo/WORKFLOW.md", "/repo/REVIEW-WORKFLOW.md", "/repo/.env", "LINEAR_API_KEY", "&lt;redacted&gt;", "GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG", "coding", "review", "ABC-1", "ABC-2", "ABC-3", "Action Needed", "Active Issues", "Retrying", "Timeline", "System Details", "issue claimed", "tracker state transition", "Todo -&gt; In Progress", "git fetch failed: not a git repository", "Prompt Log", "Implement the fix for ABC-1", "Open Issue Detail"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard body missing %q: %q", expected, body)
		}
	}
}

func TestHandlerReturnsJSONErrors(t *testing.T) {
	t.Parallel()

	handler := NewHandler(
		func() domain.StateSnapshot { return domain.StateSnapshot{} },
		func(string) (domain.IssueRuntimeSnapshot, bool) { return domain.IssueRuntimeSnapshot{}, false },
		func() {},
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/MISSING", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/issues/MISSING status = %d, want 404", res.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/refresh", nil)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/v1/refresh status = %d, want 405", res.Code)
	}
}
