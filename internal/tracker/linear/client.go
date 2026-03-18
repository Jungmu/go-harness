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
	defaultPageSize = 50
	pollQuery       = `
query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) {
  issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) {
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
	if len(c.cfg.ActiveStates) == 0 {
		return []domain.Issue{}, nil
	}

	var (
		afterCursor string
		allIssues   []domain.Issue
	)

	for {
		body, err := c.doGraphQL(ctx, graphqlRequest{
			Query: pollQuery,
			Variables: map[string]any{
				"projectSlug":   c.cfg.ProjectSlug,
				"stateNames":    c.cfg.ActiveStates,
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
