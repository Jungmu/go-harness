package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/domain"
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
		case strings.Contains(payload.Query, "projects(filter: {slugId: {eq: $projectRef}}"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"projects": map[string]any{
						"nodes": []map[string]any{
							{"slugId": "TEST", "name": "Test Project"},
						},
					},
				},
			})
		case payload.Variables["projectRef"] == "TEST":
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

func TestClientTransitionStateUsesExactWorkflowStateName(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		switch {
		case strings.Contains(payload.Query, "issues(filter: {id: {in: $ids}}, first: $first)") && strings.Contains(payload.Query, "team { id }"):
			requests = append(requests, "issue-team")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							{
								"id":   "issue-1",
								"team": map[string]any{"id": "team-1"},
							},
						},
					},
				},
			})
		case strings.Contains(payload.Query, "workflowStates(filter: {team: {id: {eq: $teamId}}}"):
			requests = append(requests, "workflow-states")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"workflowStates": map[string]any{
						"nodes": []map[string]any{
							{"id": "state-1", "name": "Todo", "type": "unstarted", "position": 0},
							{"id": "state-2", "name": "In Progress", "type": "started", "position": 1},
							{"id": "state-3", "name": "Done", "type": "completed", "position": 2},
						},
					},
				},
			})
		case strings.Contains(payload.Query, "issueUpdate(id: $id, input: { stateId: $stateId })"):
			requests = append(requests, "issue-update")
			if got := payload.Variables["stateId"]; got != "state-2" {
				t.Fatalf("stateId = %#v, want state-2", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		default:
			t.Fatalf("unexpected query: %s", payload.Query)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.TrackerConfig{
		Endpoint:       server.URL,
		APIKey:         "token-123",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})

	updated, err := client.TransitionState(context.Background(), domain.Issue{ID: "issue-1", Identifier: "ABC-1", State: "Todo"}, "In Progress")
	if err != nil {
		t.Fatalf("TransitionState() error = %v", err)
	}
	if updated.State != "In Progress" {
		t.Fatalf("updated state = %q, want In Progress", updated.State)
	}
	if !slices.Equal(requests, []string{"issue-team", "workflow-states", "issue-update"}) {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestClientTransitionStateFallsBackToCompletedWorkflowState(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		switch {
		case strings.Contains(payload.Query, "issues(filter: {id: {in: $ids}}, first: $first)") && strings.Contains(payload.Query, "team { id }"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							{
								"id":   "issue-1",
								"team": map[string]any{"id": "team-1"},
							},
						},
					},
				},
			})
		case strings.Contains(payload.Query, "workflowStates(filter: {team: {id: {eq: $teamId}}}"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"workflowStates": map[string]any{
						"nodes": []map[string]any{
							{"id": "state-closed", "name": "Closed", "type": "completed", "position": 1},
							{"id": "state-cancelled", "name": "Cancelled", "type": "canceled", "position": 2},
						},
					},
				},
			})
		case strings.Contains(payload.Query, "issueUpdate(id: $id, input: { stateId: $stateId })"):
			if got := payload.Variables["stateId"]; got != "state-closed" {
				t.Fatalf("stateId = %#v, want state-closed", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		default:
			t.Fatalf("unexpected query: %s", payload.Query)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.TrackerConfig{
		Endpoint:       server.URL,
		APIKey:         "token-123",
		TerminalStates: []string{"Closed", "Cancelled", "Done"},
	})

	updated, err := client.TransitionState(context.Background(), domain.Issue{ID: "issue-1", Identifier: "ABC-1", State: "In Progress"}, "Done")
	if err != nil {
		t.Fatalf("TransitionState() error = %v", err)
	}
	if updated.State != "Closed" {
		t.Fatalf("updated state = %q, want Closed fallback", updated.State)
	}
}

func TestClientPollTerminalIssuesUsesTerminalStates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		switch {
		case strings.Contains(payload.Query, "projects(filter: {slugId: {eq: $projectRef}}"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"projects": map[string]any{
						"nodes": []map[string]any{
							{"slugId": "TEST", "name": "Test Project"},
						},
					},
				},
			})
		case payload.Variables["projectRef"] == "TEST":
			stateNames, _ := payload.Variables["stateNames"].([]any)
			if len(stateNames) != 2 || stateNames[0] != "Done" || stateNames[1] != "Cancelled" {
				t.Fatalf("stateNames = %#v, want terminal states", payload.Variables["stateNames"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							sampleIssuePayload("ABC-7"),
						},
						"pageInfo": map[string]any{
							"hasNextPage": false,
							"endCursor":   "",
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected query: %s", payload.Query)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.TrackerConfig{
		Endpoint:       server.URL,
		APIKey:         "token-123",
		ProjectSlug:    "TEST",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done", "Cancelled"},
	})

	issues, err := client.PollTerminalIssues(context.Background())
	if err != nil {
		t.Fatalf("PollTerminalIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "ABC-7" {
		t.Fatalf("PollTerminalIssues() = %#v", issues)
	}
}

func TestClientPollCandidatesFallsBackToProjectName(t *testing.T) {
	t.Parallel()

	var slugResolveRequests, nameResolveRequests, issuePollRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		switch {
		case strings.Contains(payload.Query, "projects(filter: {slugId: {eq: $projectRef}}"):
			slugResolveRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"projects": map[string]any{
						"nodes": []map[string]any{},
					},
				},
			})
		case strings.Contains(payload.Query, "projects(filter: {name: {eq: $projectRef}}"):
			nameResolveRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"projects": map[string]any{
						"nodes": []map[string]any{
							{"slugId": "improve-harness-fa597a2ac3a5", "name": "improve-harness"},
						},
					},
				},
			})
		case payload.Variables["projectRef"] == "improve-harness-fa597a2ac3a5":
			issuePollRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							sampleIssuePayload("ABC-9"),
						},
						"pageInfo": map[string]any{
							"hasNextPage": false,
							"endCursor":   "",
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected query: %s", payload.Query)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.TrackerConfig{
		Endpoint:     server.URL,
		APIKey:       "token-123",
		ProjectSlug:  "improve-harness",
		ActiveStates: []string{"Todo", "In Progress"},
	})

	issues, err := client.PollCandidates(context.Background())
	if err != nil {
		t.Fatalf("PollCandidates() error = %v", err)
	}
	if slugResolveRequests != 1 || nameResolveRequests != 1 || issuePollRequests != 1 {
		t.Fatalf("requests = slug_resolve:%d name_resolve:%d issue_poll:%d, want 1 each", slugResolveRequests, nameResolveRequests, issuePollRequests)
	}
	if len(issues) != 1 || issues[0].Identifier != "ABC-9" {
		t.Fatalf("PollCandidates() = %#v", issues)
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
