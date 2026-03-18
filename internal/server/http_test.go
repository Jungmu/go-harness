package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
				Counts:      domain.SnapshotCounts{Running: 1},
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
