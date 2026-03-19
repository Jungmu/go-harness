package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const (
	defaultPageSize           = 50
	resolveProjectBySlugQuery = `
query SymphonyLinearProjectBySlug($projectRef: String!) {
  projects(filter: {slugId: {eq: $projectRef}}, first: 1) {
    nodes {
      slugId
      name
    }
  }
}`
	resolveProjectByNameQuery = `
query SymphonyLinearProjectByName($projectRef: String!) {
  projects(filter: {name: {eq: $projectRef}}, first: 1) {
    nodes {
      slugId
      name
    }
  }
}`
	pollBySlugQuery = `
query SymphonyLinearPollBySlug($projectRef: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) {
  issues(filter: {project: {slugId: {eq: $projectRef}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue {
            id
            identifier
            state { name }
          }
        }
      }
      createdAt
      updatedAt
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}`
	byIDsQuery = `
query SymphonyLinearIssuesByID($ids: [ID!]!, $first: Int!, $relationFirst: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue {
            id
            identifier
            state { name }
          }
        }
      }
      createdAt
      updatedAt
    }
  }
}`
	issueTeamQuery = `
query SymphonyLinearIssueTeam($ids: [ID!]!, $first: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      team { id }
    }
  }
}`
	workflowStatesQuery = `
query SymphonyLinearWorkflowStates($teamId: ID!) {
  workflowStates(filter: {team: {id: {eq: $teamId}}}, first: 100) {
    nodes {
      id
      name
      type
      position
    }
  }
}`
	issueUpdateStateQuery = `
mutation SymphonyLinearIssueUpdateState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
  }
}`
	issueCommentsQuery = `
query SymphonyLinearIssueComments($ids: [ID!]!, $first: Int!, $commentFirst: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      comments(first: $commentFirst) {
        nodes {
          id
          body
        }
      }
    }
  }
}`
	commentCreateQuery = `
mutation SymphonyLinearCommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment {
      id
    }
  }
}`
	commentUpdateQuery = `
mutation SymphonyLinearCommentUpdate($id: String!, $body: String!) {
  commentUpdate(id: $id, input: { body: $body }) {
    success
    comment {
      id
    }
  }
}`
)

type Client struct {
	httpClient *http.Client
	cfg        config.TrackerConfig
}

type graphqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
	OperationName string         `json:"operationName,omitempty"`
}

func NewClient(httpClient *http.Client, cfg config.TrackerConfig) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient, cfg: cfg}
}

func (c *Client) PollCandidates(ctx context.Context) ([]domain.Issue, error) {
	return c.pollProjectIssues(ctx, c.cfg.ActiveStates)
}

func (c *Client) PollTerminalIssues(ctx context.Context) ([]domain.Issue, error) {
	return c.pollProjectIssues(ctx, c.cfg.TerminalStates)
}

func (c *Client) pollProjectIssues(ctx context.Context, stateNames []string) ([]domain.Issue, error) {
	if len(stateNames) == 0 {
		return []domain.Issue{}, nil
	}

	projectSlug, err := c.resolveProjectSlug(ctx, c.cfg.ProjectSlug)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectSlug) == "" {
		return nil, fmt.Errorf("linear project not found for tracker.project_slug=%q", c.cfg.ProjectSlug)
	}
	return c.pollIssuesByProjectRef(ctx, projectSlug, stateNames)
}

func (c *Client) pollIssuesByProjectRef(ctx context.Context, projectSlug string, stateNames []string) ([]domain.Issue, error) {
	var (
		afterCursor string
		allIssues   []domain.Issue
	)

	for {
		body, err := c.doGraphQL(ctx, graphqlRequest{
			Query: pollBySlugQuery,
			Variables: map[string]any{
				"projectRef":    projectSlug,
				"stateNames":    stateNames,
				"first":         defaultPageSize,
				"relationFirst": defaultPageSize,
				"after":         nilIfEmpty(afterCursor),
			},
		})
		if err != nil {
			return nil, err
		}

		nodes, pageInfo, err := decodeIssuePage(body)
		if err != nil {
			return nil, err
		}

		allIssues = append(allIssues, nodes...)
		if pageInfo.EndCursor == "" || !pageInfo.HasNextPage {
			break
		}
		afterCursor = pageInfo.EndCursor
	}

	return allIssues, nil
}

func (c *Client) resolveProjectSlug(ctx context.Context, projectRef string) (string, error) {
	slug, err := c.lookupProjectSlug(ctx, resolveProjectBySlugQuery, projectRef)
	if err != nil {
		return "", err
	}
	if slug != "" {
		return slug, nil
	}
	return c.lookupProjectSlug(ctx, resolveProjectByNameQuery, projectRef)
}

func (c *Client) lookupProjectSlug(ctx context.Context, query, projectRef string) (string, error) {
	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: query,
		Variables: map[string]any{
			"projectRef": projectRef,
		},
	})
	if err != nil {
		return "", err
	}
	return decodeProjectSlug(body)
}

func (c *Client) FetchByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	ids = compactStrings(ids)
	if len(ids) == 0 {
		return []domain.Issue{}, nil
	}

	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: byIDsQuery,
		Variables: map[string]any{
			"ids":           ids,
			"first":         max(len(ids), 1),
			"relationFirst": defaultPageSize,
		},
	})
	if err != nil {
		return nil, err
	}

	issues, _, err := decodeIssuePage(body)
	if err != nil {
		return nil, err
	}
	return issues, nil
}

func (c *Client) TransitionState(ctx context.Context, issue domain.Issue, targetState string) (domain.Issue, error) {
	targetState = strings.TrimSpace(targetState)
	if strings.TrimSpace(issue.ID) == "" {
		return issue, fmt.Errorf("transition issue state: missing issue id")
	}
	if targetState == "" {
		return issue, fmt.Errorf("transition issue state: missing target state")
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), targetState) {
		issue.State = targetState
		return issue, nil
	}

	teamID, err := c.lookupIssueTeamID(ctx, issue.ID)
	if err != nil {
		return issue, err
	}
	stateID, resolvedName, err := c.lookupWorkflowStateID(ctx, teamID, targetState)
	if err != nil {
		return issue, err
	}

	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: issueUpdateStateQuery,
		Variables: map[string]any{
			"id":      issue.ID,
			"stateId": stateID,
		},
	})
	if err != nil {
		return issue, err
	}
	if err := decodeIssueUpdateSuccess(body); err != nil {
		return issue, err
	}

	issue.State = resolvedName
	return issue, nil
}

func (c *Client) UpsertProgressComment(ctx context.Context, issue domain.Issue, body string) error {
	if strings.TrimSpace(issue.ID) == "" {
		return fmt.Errorf("upsert progress comment: missing issue id")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("upsert progress comment: missing body")
	}

	comments, err := c.listIssueComments(ctx, issue.ID)
	if err != nil {
		return err
	}
	if existing := findHarnessProgressComment(comments); existing != nil {
		return c.updateComment(ctx, existing.ID, body)
	}
	return c.createComment(ctx, issue.ID, body)
}

func (c *Client) doGraphQL(ctx context.Context, payload graphqlRequest) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear api status %d: %s", resp.StatusCode, truncateString(string(raw), 512))
	}

	return raw, nil
}

type issueComment struct {
	ID   string
	Body string
}

type issuePageInfo struct {
	HasNextPage bool
	EndCursor   string
}

func decodeIssuePage(raw []byte) ([]domain.Issue, issuePageInfo, error) {
	var payload struct {
		Data struct {
			Issues struct {
				Nodes    []map[string]any `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, issuePageInfo{}, err
	}
	if len(payload.Errors) > 0 {
		return nil, issuePageInfo{}, fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}

	issues := make([]domain.Issue, 0, len(payload.Data.Issues.Nodes))
	for _, item := range payload.Data.Issues.Nodes {
		issues = append(issues, normalizeIssue(item))
	}

	return issues, issuePageInfo{
		HasNextPage: payload.Data.Issues.PageInfo.HasNextPage,
		EndCursor:   payload.Data.Issues.PageInfo.EndCursor,
	}, nil
}

func decodeProjectSlug(raw []byte) (string, error) {
	var payload struct {
		Data struct {
			Projects struct {
				Nodes []struct {
					SlugID string `json:"slugId"`
					Name   string `json:"name"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if len(payload.Errors) > 0 {
		return "", fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}
	if len(payload.Data.Projects.Nodes) == 0 {
		return "", nil
	}
	return strings.TrimSpace(payload.Data.Projects.Nodes[0].SlugID), nil
}

type workflowState struct {
	ID       string
	Name     string
	Type     string
	Position float64
}

func (c *Client) lookupIssueTeamID(ctx context.Context, issueID string) (string, error) {
	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: issueTeamQuery,
		Variables: map[string]any{
			"ids":   []string{issueID},
			"first": 1,
		},
	})
	if err != nil {
		return "", err
	}
	return decodeIssueTeamID(body, issueID)
}

func decodeIssueTeamID(raw []byte, issueID string) (string, error) {
	var payload struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID   string `json:"id"`
					Team struct {
						ID string `json:"id"`
					} `json:"team"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if len(payload.Errors) > 0 {
		return "", fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}
	if len(payload.Data.Issues.Nodes) == 0 || strings.TrimSpace(payload.Data.Issues.Nodes[0].Team.ID) == "" {
		return "", fmt.Errorf("linear issue team not found for issue_id=%q", issueID)
	}
	return strings.TrimSpace(payload.Data.Issues.Nodes[0].Team.ID), nil
}

func (c *Client) lookupWorkflowStateID(ctx context.Context, teamID, targetState string) (string, string, error) {
	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: workflowStatesQuery,
		Variables: map[string]any{
			"teamId": teamID,
		},
	})
	if err != nil {
		return "", "", err
	}
	states, err := decodeWorkflowStates(body)
	if err != nil {
		return "", "", err
	}

	if exact := findWorkflowStateByName(states, targetState); exact != nil {
		return exact.ID, exact.Name, nil
	}

	if fallback := c.pickWorkflowStateFallback(states, targetState); fallback != nil {
		return fallback.ID, fallback.Name, nil
	}

	return "", "", fmt.Errorf("linear workflow state %q not found for team_id=%q", targetState, teamID)
}

func decodeWorkflowStates(raw []byte) ([]workflowState, error) {
	var payload struct {
		Data struct {
			WorkflowStates struct {
				Nodes []struct {
					ID       string  `json:"id"`
					Name     string  `json:"name"`
					Type     string  `json:"type"`
					Position float64 `json:"position"`
				} `json:"nodes"`
			} `json:"workflowStates"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if len(payload.Errors) > 0 {
		return nil, fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}

	states := make([]workflowState, 0, len(payload.Data.WorkflowStates.Nodes))
	for _, state := range payload.Data.WorkflowStates.Nodes {
		if strings.TrimSpace(state.ID) == "" || strings.TrimSpace(state.Name) == "" {
			continue
		}
		states = append(states, workflowState{
			ID:       strings.TrimSpace(state.ID),
			Name:     strings.TrimSpace(state.Name),
			Type:     strings.TrimSpace(state.Type),
			Position: state.Position,
		})
	}
	return states, nil
}

func decodeIssueUpdateSuccess(raw []byte) error {
	var payload struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if len(payload.Errors) > 0 {
		return fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}
	if !payload.Data.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate did not report success")
	}
	return nil
}

func (c *Client) listIssueComments(ctx context.Context, issueID string) ([]issueComment, error) {
	body, err := c.doGraphQL(ctx, graphqlRequest{
		Query: issueCommentsQuery,
		Variables: map[string]any{
			"ids":          []string{issueID},
			"first":        1,
			"commentFirst": 100,
		},
	})
	if err != nil {
		return nil, err
	}
	return decodeIssueComments(body, issueID)
}

func decodeIssueComments(raw []byte, issueID string) ([]issueComment, error) {
	var payload struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID       string `json:"id"`
					Comments struct {
						Nodes []struct {
							ID   string `json:"id"`
							Body string `json:"body"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if len(payload.Errors) > 0 {
		return nil, fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}
	if len(payload.Data.Issues.Nodes) == 0 {
		return nil, fmt.Errorf("linear issue not found for issue_id=%q", issueID)
	}

	comments := make([]issueComment, 0, len(payload.Data.Issues.Nodes[0].Comments.Nodes))
	for _, comment := range payload.Data.Issues.Nodes[0].Comments.Nodes {
		if strings.TrimSpace(comment.ID) == "" {
			continue
		}
		comments = append(comments, issueComment{
			ID:   strings.TrimSpace(comment.ID),
			Body: comment.Body,
		})
	}
	return comments, nil
}

func findHarnessProgressComment(comments []issueComment) *issueComment {
	for i := range comments {
		if strings.HasPrefix(strings.TrimSpace(comments[i].Body), domain.HarnessProgressCommentHeading) {
			return &comments[i]
		}
	}
	return nil
}

func (c *Client) createComment(ctx context.Context, issueID, body string) error {
	raw, err := c.doGraphQL(ctx, graphqlRequest{
		Query: commentCreateQuery,
		Variables: map[string]any{
			"issueId": issueID,
			"body":    body,
		},
	})
	if err != nil {
		return err
	}
	return decodeCommentMutationSuccess(raw, "commentCreate")
}

func (c *Client) updateComment(ctx context.Context, commentID, body string) error {
	raw, err := c.doGraphQL(ctx, graphqlRequest{
		Query: commentUpdateQuery,
		Variables: map[string]any{
			"id":   commentID,
			"body": body,
		},
	})
	if err != nil {
		return err
	}
	return decodeCommentMutationSuccess(raw, "commentUpdate")
}

func decodeCommentMutationSuccess(raw []byte, field string) error {
	var payload struct {
		Data   map[string]map[string]any `json:"data"`
		Errors []map[string]any          `json:"errors"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if len(payload.Errors) > 0 {
		return fmt.Errorf("linear graphql errors: %v", payload.Errors)
	}
	result := payload.Data[field]
	if result == nil {
		return fmt.Errorf("linear %s response missing", field)
	}
	success, _ := result["success"].(bool)
	if !success {
		return fmt.Errorf("linear %s did not report success", field)
	}
	return nil
}

func findWorkflowStateByName(states []workflowState, target string) *workflowState {
	for i := range states {
		if strings.EqualFold(strings.TrimSpace(states[i].Name), target) {
			return &states[i]
		}
	}
	return nil
}

func (c *Client) pickWorkflowStateFallback(states []workflowState, target string) *workflowState {
	switch {
	case strings.EqualFold(target, "In Progress"):
		return bestWorkflowState(states, func(state workflowState) bool {
			if strings.EqualFold(state.Type, "started") {
				return true
			}
			return strings.EqualFold(state.Type, "unstarted") && slices.Contains(c.cfg.ActiveStates, state.Name)
		})
	case strings.EqualFold(target, "Done"):
		return bestWorkflowState(states, func(state workflowState) bool {
			if strings.EqualFold(state.Type, "completed") {
				return true
			}
			return strings.EqualFold(state.Type, "canceled") && slices.Contains(c.cfg.TerminalStates, state.Name)
		})
	default:
		return nil
	}
}

func bestWorkflowState(states []workflowState, keep func(workflowState) bool) *workflowState {
	var best *workflowState
	for i := range states {
		if !keep(states[i]) {
			continue
		}
		candidate := &states[i]
		if best == nil || candidate.Position < best.Position || (candidate.Position == best.Position && strings.Compare(candidate.Name, best.Name) < 0) {
			best = candidate
		}
	}
	return best
}

func normalizeIssue(raw map[string]any) domain.Issue {
	return domain.Issue{
		ID:          stringField(raw, "id"),
		Identifier:  stringField(raw, "identifier"),
		Title:       stringField(raw, "title"),
		Description: stringField(raw, "description"),
		Priority:    intField(raw, "priority"),
		State:       stringField(mapField(raw, "state"), "name"),
		BranchName:  stringField(raw, "branchName"),
		URL:         stringField(raw, "url"),
		Labels:      extractLabels(mapField(raw, "labels")),
		BlockedBy:   extractBlockers(mapField(raw, "inverseRelations")),
		CreatedAt:   parseTime(stringField(raw, "createdAt")),
		UpdatedAt:   parseTime(stringField(raw, "updatedAt")),
	}
}

func extractLabels(raw map[string]any) []string {
	nodes, _ := raw["nodes"].([]any)
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if mapped, ok := node.(map[string]any); ok {
			if name := strings.ToLower(strings.TrimSpace(stringField(mapped, "name"))); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

func extractBlockers(raw map[string]any) []domain.Blocker {
	nodes, _ := raw["nodes"].([]any)
	out := make([]domain.Blocker, 0, len(nodes))
	for _, node := range nodes {
		mapped, ok := node.(map[string]any)
		if !ok || !strings.EqualFold(stringField(mapped, "type"), "blocks") {
			continue
		}
		issue := mapField(mapped, "issue")
		out = append(out, domain.Blocker{
			ID:         stringField(issue, "id"),
			Identifier: stringField(issue, "identifier"),
			State:      stringField(mapField(issue, "state"), "name"),
		})
	}
	return out
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

func mapField(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	mapped, _ := raw[key].(map[string]any)
	return mapped
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || slices.Contains(result, trimmed) {
			continue
		}
		result = append(result, trimmed)
	}
	return result
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
