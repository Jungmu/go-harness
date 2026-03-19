package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/domain"
	"go-harness/internal/orchestrator"
	"go-harness/internal/server"
	"go-harness/internal/workflow"
)

const (
	liveLinearEndpoint = "https://api.linear.app/graphql"
	liveMarkerFileName = "live-marker.txt"
)

type liveE2EProfile struct {
	APIKey            string
	TeamID            string
	ProjectID         string
	ProjectSlug       string
	ActiveStateID     string
	ActiveStateName   string
	HandoffStateName  string
	TerminalStateID   string
	TerminalStateName string
	CodexCommand      string
}

type liveWorkflowState struct {
	ID       string
	Name     string
	Type     string
	Position float64
}

type liveLinearClient struct {
	httpClient *http.Client
	apiKey     string
	endpoint   string
}

type liveGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

func TestPickLiveWorkflowState(t *testing.T) {
	t.Parallel()

	states := []liveWorkflowState{
		{ID: "todo", Name: "Todo", Type: "unstarted", Position: 1},
		{ID: "started-2", Name: "Working", Type: "started", Position: 2},
		{ID: "started-1", Name: "In Progress", Type: "started", Position: 1},
		{ID: "done", Name: "Done", Type: "completed", Position: 1},
	}

	state, err := pickLiveWorkflowState(states, "", "started", "unstarted")
	if err != nil {
		t.Fatalf("pickLiveWorkflowState(active) error = %v", err)
	}
	if state.ID != "started-1" {
		t.Fatalf("picked state = %#v, want started-1", state)
	}

	state, err = pickLiveWorkflowState(states, "Done", "completed", "canceled")
	if err != nil {
		t.Fatalf("pickLiveWorkflowState(named) error = %v", err)
	}
	if state.ID != "done" {
		t.Fatalf("picked named state = %#v, want done", state)
	}
}

func TestLiveLinearCodexHandsOffAtMaxTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Linear + Codex E2E in short mode")
	}

	profile, ok := loadLiveE2EProfile(t)
	if !ok {
		return
	}

	testCtx, cancelTest := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelTest()

	linearClient := &liveLinearClient{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		apiKey:     profile.APIKey,
		endpoint:   liveLinearEndpoint,
	}

	profile, err := resolveLiveE2EProfile(testCtx, linearClient, profile)
	if err != nil {
		t.Fatalf("resolveLiveE2EProfile() error = %v", err)
	}

	titleSuffix := time.Now().UTC().Format("20060102-150405")
	issue, err := linearClient.createIssue(testCtx, map[string]any{
		"teamId":      profile.TeamID,
		"projectId":   profile.ProjectID,
		"stateId":     profile.activeStateID(),
		"title":       "go-harness live e2e " + titleSuffix,
		"description": "Temporary issue created by the go-harness live Linear + Codex E2E test.",
	})
	if err != nil {
		t.Fatalf("createIssue() error = %v", err)
	}
	t.Logf("created live test issue %s (%s)", issue.Identifier, issue.ID)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := linearClient.updateIssueState(cleanupCtx, issue.ID, profile.terminalStateID()); err != nil {
			t.Logf("cleanup issue state update failed: %v", err)
		}
	}()

	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(buildLiveWorkflow(profile, workspaceRoot)), 0o644); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore(workflow.NewLoader())
	cfg, err := store.LoadAndValidate(workflowPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	orch := orchestrator.New(
		cfg,
		&dynamicTracker{store: store, httpClient: linearClient.httpClient},
		&dynamicWorkspaceManager{store: store, logger: logger},
		&dynamicRunner{store: store, logger: logger},
		logger,
		orchestrator.WithConfigSource(store),
	)
	if err := orch.Start(testCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := orch.Stop(shutdownCtx); err != nil {
			t.Logf("orchestrator stop failed: %v", err)
		}
	}()

	statusServer := httptest.NewServer(server.NewHandler(orch.Snapshot, orch.IssueSnapshot, orch.TriggerRefresh))
	defer statusServer.Close()

	if err := waitForLiveCondition(testCtx, 250*time.Millisecond, func() (bool, string, error) {
		issueRuntime, found, err := tryFetchIssueSnapshot(statusServer.Client(), statusServer.URL, issue.Identifier)
		if err != nil {
			return false, "", err
		}
		if !found {
			return false, "issue is not present in runtime state yet", nil
		}
		return true, describeIssueRuntime(issueRuntime), nil
	}); err != nil {
		t.Fatalf("waiting for issue to enter runtime state failed: %v", err)
	}

	markerPath := filepath.Join(workspaceRoot, domain.SanitizeWorkspaceKey(issue.Identifier), liveMarkerFileName)
	if err := waitForLiveCondition(testCtx, 250*time.Millisecond, func() (bool, string, error) {
		content, readErr := os.ReadFile(markerPath)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				issueRuntime, found, err := tryFetchIssueSnapshot(statusServer.Client(), statusServer.URL, issue.Identifier)
				if err != nil {
					return false, "", err
				}
				if !found {
					return false, "marker file not created yet; issue not present in runtime state", nil
				}
				if issueRuntime.Status == "completed" {
					return false, "", fmt.Errorf("issue completed before marker creation")
				}
				return false, "marker file not created yet; " + describeIssueRuntime(issueRuntime), nil
			}
			return false, "", readErr
		}

		marker := string(content)
		if !strings.Contains(marker, "issue="+issue.Identifier) {
			return false, "marker file missing issue identifier", nil
		}
		if !strings.Contains(marker, "attempt=1") {
			return false, "marker file missing attempt=1", nil
		}
		return true, "", nil
	}); err != nil {
		t.Fatalf("waiting for marker file failed: %v", err)
	}

	if err := waitForLiveCondition(testCtx, 250*time.Millisecond, func() (bool, string, error) {
		issueRuntime, err := fetchIssueSnapshot(statusServer.Client(), statusServer.URL, issue.Identifier)
		if err != nil {
			return false, "", err
		}
		if issueRuntime.Status != "completed" {
			return false, fmt.Sprintf("issue runtime status = %q, want completed", issueRuntime.Status), nil
		}

		stateName, err := linearClient.issueStateName(testCtx, issue.ID)
		if err != nil {
			return false, "", err
		}
		if stateName != profile.HandoffStateName {
			return false, fmt.Sprintf("linear issue state = %q, want %q", stateName, profile.HandoffStateName), nil
		}
		return true, "", nil
	}); err != nil {
		t.Fatalf("waiting for handoff completion failed: %v", err)
	}
}

func loadLiveE2EProfile(t *testing.T) (liveE2EProfile, bool) {
	t.Helper()

	if os.Getenv("GO_HARNESS_LIVE_E2E") != "1" {
		t.Skip("set GO_HARNESS_LIVE_E2E=1 to run the live Linear + Codex E2E test")
	}

	profile := liveE2EProfile{
		APIKey:            strings.TrimSpace(os.Getenv("LINEAR_API_KEY")),
		TeamID:            strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_LINEAR_TEAM_ID")),
		ProjectSlug:       strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG")),
		ActiveStateName:   strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_LINEAR_ACTIVE_STATE_NAME")),
		HandoffStateName:  strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_LINEAR_HANDOFF_STATE_NAME")),
		TerminalStateName: strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_LINEAR_TERMINAL_STATE_NAME")),
		CodexCommand:      strings.TrimSpace(os.Getenv("GO_HARNESS_LIVE_CODEX_COMMAND")),
	}
	if profile.CodexCommand == "" {
		profile.CodexCommand = "codex app-server"
	}
	if profile.HandoffStateName == "" {
		profile.HandoffStateName = "In Review"
	}

	missing := make([]string, 0, 3)
	if profile.APIKey == "" {
		missing = append(missing, "LINEAR_API_KEY")
	}
	if profile.TeamID == "" {
		missing = append(missing, "GO_HARNESS_LIVE_LINEAR_TEAM_ID")
	}
	if profile.ProjectSlug == "" {
		missing = append(missing, "GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG")
	}

	if len(missing) > 0 {
		t.Skipf("missing live E2E env vars: %s", strings.Join(missing, ", "))
	}

	return profile, true
}

func resolveLiveE2EProfile(ctx context.Context, client *liveLinearClient, profile liveE2EProfile) (liveE2EProfile, error) {
	teamID, err := client.lookupTeamIDByRef(ctx, profile.TeamID)
	if err != nil {
		return liveE2EProfile{}, err
	}
	profile.TeamID = teamID

	projectID, projectSlug, err := client.lookupProjectByRef(ctx, profile.ProjectSlug)
	if err != nil {
		return liveE2EProfile{}, err
	}
	profile.ProjectID = projectID
	profile.ProjectSlug = projectSlug

	states, err := client.lookupWorkflowStates(ctx, profile.TeamID)
	if err != nil {
		return liveE2EProfile{}, err
	}

	active, err := pickLiveWorkflowState(states, profile.ActiveStateName, "started", "unstarted")
	if err != nil {
		return liveE2EProfile{}, fmt.Errorf("select active state: %w", err)
	}
	profile.ActiveStateID = active.ID
	profile.ActiveStateName = active.Name

	handoff, err := pickLiveWorkflowState(states, profile.HandoffStateName, "started", "unstarted", "completed", "canceled")
	if err != nil {
		return liveE2EProfile{}, fmt.Errorf("select handoff state: %w", err)
	}
	profile.HandoffStateName = handoff.Name

	terminal, err := pickLiveWorkflowState(states, profile.TerminalStateName, "completed", "canceled")
	if err != nil {
		return liveE2EProfile{}, fmt.Errorf("select terminal state: %w", err)
	}
	profile.TerminalStateID = terminal.ID
	profile.TerminalStateName = terminal.Name

	return profile, nil
}

func buildLiveWorkflow(profile liveE2EProfile, workspaceRoot string) string {
	activeStateBlock := fmt.Sprintf("    - %q\n", profile.ActiveStateName)
	if !strings.EqualFold(strings.TrimSpace(profile.ActiveStateName), "In Progress") {
		activeStateBlock += `    - "In Progress"
`
	}

	return fmt.Sprintf(`---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: %q
  active_states:
%s
  terminal_states:
    - %q

polling:
  interval_ms: 250

workspace:
  root: %q

agent:
  max_concurrent_agents: 1
  max_turns: 1
  max_retry_backoff_ms: 1000

codex:
  command: %q
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspace-write
  turn_timeout_ms: 120000
  read_timeout_ms: 15000
  stall_timeout_ms: 30000
---
Create or overwrite a file named %s in the current workspace.
Create it immediately in the current working directory.

The file must contain exactly these lines:
issue={{ issue.identifier }}
attempt={{ attempt }}
state={{ issue.state }}

Do not inspect the repository. Do not run git. Do not ask for approval or user input.
Do not modify any other files. End the turn immediately after the file exists.
`, profile.ProjectSlug, activeStateBlock, profile.TerminalStateName, workspaceRoot, profile.CodexCommand, liveMarkerFileName)
}

func (p liveE2EProfile) activeStateID() string {
	return p.ActiveStateID
}

func (p liveE2EProfile) terminalStateID() string {
	return p.TerminalStateID
}

func pickLiveWorkflowState(states []liveWorkflowState, preferredName string, preferredTypes ...string) (liveWorkflowState, error) {
	if preferredName = strings.TrimSpace(preferredName); preferredName != "" {
		for _, state := range states {
			if strings.EqualFold(strings.TrimSpace(state.Name), preferredName) {
				return state, nil
			}
		}
		return liveWorkflowState{}, fmt.Errorf("state %q not found in team workflow states", preferredName)
	}

	for _, preferredType := range preferredTypes {
		var chosen *liveWorkflowState
		for _, state := range states {
			if !strings.EqualFold(strings.TrimSpace(state.Type), preferredType) {
				continue
			}
			if chosen == nil || state.Position < chosen.Position {
				candidate := state
				chosen = &candidate
			}
		}
		if chosen != nil {
			return *chosen, nil
		}
	}

	return liveWorkflowState{}, fmt.Errorf("no workflow state matched preferred types %v", preferredTypes)
}

func (c *liveLinearClient) createIssue(ctx context.Context, input map[string]any) (domain.Issue, error) {
	var payload struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
				URL        string `json:"url"`
				State      struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}

	err := c.graphql(ctx, `
mutation LiveIssueCreate($teamId: String!, $projectId: String, $stateId: String, $title: String!, $description: String!) {
  issueCreate(input: {
    teamId: $teamId
    projectId: $projectId
    stateId: $stateId
    title: $title
    description: $description
  }) {
    success
    issue {
      id
      identifier
      title
      url
      state { name }
    }
  }
}
`, input, &payload)
	if err != nil {
		return domain.Issue{}, err
	}
	if !payload.IssueCreate.Success || payload.IssueCreate.Issue.ID == "" {
		return domain.Issue{}, fmt.Errorf("issueCreate did not return a created issue")
	}

	return domain.Issue{
		ID:         payload.IssueCreate.Issue.ID,
		Identifier: payload.IssueCreate.Issue.Identifier,
		Title:      payload.IssueCreate.Issue.Title,
		URL:        payload.IssueCreate.Issue.URL,
		State:      payload.IssueCreate.Issue.State.Name,
	}, nil
}

func (c *liveLinearClient) updateIssueState(ctx context.Context, issueID, stateID string) error {
	var payload struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}

	err := c.graphql(ctx, `
mutation LiveIssueUpdate($id: String!, $stateId: String) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
  }
}
`, map[string]any{
		"id":      issueID,
		"stateId": stateID,
	}, &payload)
	if err != nil {
		return err
	}
	if !payload.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate did not report success")
	}
	return nil
}

func (c *liveLinearClient) issueStateName(ctx context.Context, issueID string) (string, error) {
	var payload struct {
		Issue *struct {
			State struct {
				Name string `json:"name"`
			} `json:"state"`
		} `json:"issue"`
	}

	err := c.graphql(ctx, `
query LiveIssueState($id: String!) {
  issue(id: $id) {
    state { name }
  }
}
`, map[string]any{
		"id": issueID,
	}, &payload)
	if err != nil {
		return "", err
	}
	if payload.Issue == nil {
		return "", fmt.Errorf("issue %q not found", issueID)
	}
	return payload.Issue.State.Name, nil
}

func (c *liveLinearClient) lookupTeamIDByRef(ctx context.Context, ref string) (string, error) {
	var payload struct {
		Teams struct {
			Nodes []struct {
				ID   string `json:"id"`
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}

	if err := c.graphql(ctx, `
query LiveTeams {
  teams(first: 100) {
    nodes {
      id
      key
      name
    }
  }
}
`, nil, &payload); err != nil {
		return "", fmt.Errorf("lookup team by ref: %w", err)
	}

	ref = strings.TrimSpace(ref)
	for _, team := range payload.Teams.Nodes {
		switch {
		case strings.EqualFold(strings.TrimSpace(team.ID), ref):
			return team.ID, nil
		case strings.EqualFold(strings.TrimSpace(team.Key), ref):
			return team.ID, nil
		case strings.EqualFold(strings.TrimSpace(team.Name), ref):
			return team.ID, nil
		}
	}
	return "", fmt.Errorf("team reference %q not found; use team UUID, key, or exact name", ref)
}

func (c *liveLinearClient) lookupProjectByRef(ctx context.Context, ref string) (string, string, error) {
	var payload struct {
		Projects struct {
			Nodes []struct {
				ID     string `json:"id"`
				SlugID string `json:"slugId"`
				Name   string `json:"name"`
			} `json:"nodes"`
		} `json:"projects"`
	}

	if err := c.graphql(ctx, `
query LiveProjects {
  projects(first: 250) {
    nodes {
      id
      slugId
      name
    }
  }
}
`, nil, &payload); err != nil {
		return "", "", fmt.Errorf("lookup project by ref: %w", err)
	}

	ref = strings.TrimSpace(ref)
	for _, project := range payload.Projects.Nodes {
		if strings.EqualFold(strings.TrimSpace(project.SlugID), ref) && strings.TrimSpace(project.ID) != "" {
			return project.ID, project.SlugID, nil
		}
	}

	var matchedByName []struct {
		ID     string
		SlugID string
		Name   string
	}
	for _, project := range payload.Projects.Nodes {
		if strings.EqualFold(strings.TrimSpace(project.Name), ref) && strings.TrimSpace(project.ID) != "" {
			matchedByName = append(matchedByName, struct {
				ID     string
				SlugID string
				Name   string
			}{
				ID:     project.ID,
				SlugID: project.SlugID,
				Name:   project.Name,
			})
		}
	}

	switch len(matchedByName) {
	case 1:
		return matchedByName[0].ID, matchedByName[0].SlugID, nil
	case 0:
		return "", "", fmt.Errorf("project reference %q not found; use project slugId or exact project name", ref)
	default:
		return "", "", fmt.Errorf("project reference %q matched multiple project names; use the slugId instead", ref)
	}
}

func (c *liveLinearClient) lookupWorkflowStates(ctx context.Context, teamID string) ([]liveWorkflowState, error) {
	var payload struct {
		WorkflowStates struct {
			Nodes []struct {
				ID       string  `json:"id"`
				Name     string  `json:"name"`
				Type     string  `json:"type"`
				Position float64 `json:"position"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}

	if err := c.graphql(ctx, `
query LiveWorkflowStates($teamId: ID!) {
  workflowStates(filter: {team: {id: {eq: $teamId}}}, first: 100) {
    nodes {
      id
      name
      type
      position
    }
  }
}
`, map[string]any{"teamId": teamID}, &payload); err != nil {
		return nil, fmt.Errorf("lookup workflow states: %w", err)
	}

	states := make([]liveWorkflowState, 0, len(payload.WorkflowStates.Nodes))
	for _, state := range payload.WorkflowStates.Nodes {
		if strings.TrimSpace(state.ID) == "" || strings.TrimSpace(state.Name) == "" {
			continue
		}
		states = append(states, liveWorkflowState{
			ID:       state.ID,
			Name:     state.Name,
			Type:     state.Type,
			Position: state.Position,
		})
	}
	if len(states) == 0 {
		return nil, fmt.Errorf("team %s returned no workflow states", teamID)
	}
	return states, nil
}

func (c *liveLinearClient) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(liveGraphQLRequest{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear api status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, item := range envelope.Errors {
			if strings.TrimSpace(item.Message) != "" {
				messages = append(messages, item.Message)
			}
		}
		return fmt.Errorf("linear graphql errors: %s", strings.Join(messages, "; "))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func fetchStateSnapshot(client *http.Client, baseURL string) (domain.StateSnapshot, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/state", nil)
	if err != nil {
		return domain.StateSnapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return domain.StateSnapshot{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.StateSnapshot{}, fmt.Errorf("GET /api/v1/state returned %s", resp.Status)
	}

	var snapshot domain.StateSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return domain.StateSnapshot{}, err
	}
	return snapshot, nil
}

func fetchIssueSnapshot(client *http.Client, baseURL, identifier string) (domain.IssueRuntimeSnapshot, error) {
	snapshot, _, err := tryFetchIssueSnapshot(client, baseURL, identifier)
	return snapshot, err
}

func tryFetchIssueSnapshot(client *http.Client, baseURL, identifier string) (domain.IssueRuntimeSnapshot, bool, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/issues/"+identifier, nil)
	if err != nil {
		return domain.IssueRuntimeSnapshot{}, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return domain.IssueRuntimeSnapshot{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.IssueRuntimeSnapshot{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return domain.IssueRuntimeSnapshot{}, false, fmt.Errorf("GET /api/v1/issues/%s returned %s", identifier, resp.Status)
	}

	var snapshot domain.IssueRuntimeSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return domain.IssueRuntimeSnapshot{}, false, err
	}
	return snapshot, true, nil
}

func triggerRefresh(client *http.Client, baseURL string) error {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/refresh", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST /api/v1/refresh returned %s", resp.Status)
	}
	return nil
}

func waitForLiveCondition(ctx context.Context, interval time.Duration, fn func() (bool, string, error)) error {
	var lastDetail string
	for {
		ok, detail, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if strings.TrimSpace(detail) != "" {
			lastDetail = detail
		}

		select {
		case <-ctx.Done():
			if lastDetail == "" {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %s", ctx.Err(), lastDetail)
		case <-time.After(interval):
		}
	}
}

func describeIssueRuntime(snapshot domain.IssueRuntimeSnapshot) string {
	switch snapshot.Status {
	case "running":
		if snapshot.Running != nil && snapshot.Running.LiveSession != nil {
			return fmt.Sprintf("status=running turn=%d last_event=%s", snapshot.Running.LiveSession.TurnCount, snapshot.Running.LiveSession.LastEvent)
		}
		return "status=running"
	case "retrying":
		if snapshot.Retry != nil {
			if strings.TrimSpace(snapshot.Retry.LastError) != "" {
				return fmt.Sprintf("status=retrying reason=%s attempt=%d last_error=%s", snapshot.Retry.Reason, snapshot.Retry.Attempt, snapshot.Retry.LastError)
			}
			return fmt.Sprintf("status=retrying reason=%s attempt=%d", snapshot.Retry.Reason, snapshot.Retry.Attempt)
		}
		return "status=retrying"
	case "completed":
		return "status=completed"
	default:
		return "status=" + snapshot.Status
	}
}
