package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-harness/internal/config"
)

func TestClientPollCandidatesAndFetchByIDs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token-123" {
			t.Fatalf("Authorization header = %q, want token-123", got)
		}

		var payload graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		switch {
		case payload.Variables["projectSlug"] == "TEST":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							sampleIssuePayload("ABC-1"),
						},
						"pageInfo": map[string]any{
							"hasNextPage": false,
							"endCursor":   "",
						},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							sampleIssuePayload("ABC-2"),
						},
					},
				},
			})
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.TrackerConfig{
		Endpoint:       server.URL,
		APIKey:         "token-123",
		ProjectSlug:    "TEST",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})

	issues, err := client.PollCandidates(context.Background())
	if err != nil {
		t.Fatalf("PollCandidates() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "ABC-1" {
		t.Fatalf("PollCandidates() = %#v", issues)
	}
	if len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0].Identifier != "BLK-1" {
		t.Fatalf("blocked_by = %#v", issues[0].BlockedBy)
	}
	if got := issues[0].Labels; len(got) != 1 || got[0] != "backend" {
		t.Fatalf("labels = %#v", got)
	}
	if issues[0].CreatedAt.IsZero() || issues[0].UpdatedAt.IsZero() {
		t.Fatalf("timestamps were not parsed: %#v", issues[0])
	}

	refreshed, err := client.FetchByIDs(context.Background(), []string{"issue-2"})
	if err != nil {
		t.Fatalf("FetchByIDs() error = %v", err)
	}
	if len(refreshed) != 1 || refreshed[0].Identifier != "ABC-2" {
		t.Fatalf("FetchByIDs() = %#v", refreshed)
	}
}

func sampleIssuePayload(identifier string) map[string]any {
	return map[string]any{
		"id":          "issue-" + identifier,
		"identifier":  identifier,
		"title":       "Example",
		"description": "Details",
		"priority":    2,
		"state":       map[string]any{"name": "In Progress"},
		"branchName":  "feature/test",
		"url":         "https://linear.app/example",
		"labels": map[string]any{
			"nodes": []map[string]any{
				{"name": "Backend"},
			},
		},
		"inverseRelations": map[string]any{
			"nodes": []map[string]any{
				{
					"type": "blocks",
					"issue": map[string]any{
						"id":         "blk-1",
						"identifier": "BLK-1",
						"state":      map[string]any{"name": "Todo"},
					},
				},
			},
		},
		"createdAt": time.Date(2026, 3, 18, 1, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"updatedAt": time.Date(2026, 3, 18, 2, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}
