package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

type Client struct {
	httpClient *http.Client
	cfg        config.GitHubConfig
}

type pullRequestResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func NewClient(httpClient *http.Client, cfg config.GitHubConfig) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient, cfg: cfg}
}

func (c *Client) EnsurePullRequest(ctx context.Context, issue domain.Issue, workspace domain.Workspace) (domain.PullRequest, error) {
	workspacePath := strings.TrimSpace(workspace.Path)
	if workspacePath == "" {
		return domain.PullRequest{}, fmt.Errorf("github pull request creation requires a workspace path")
	}
	if _, err := c.gitOutput(ctx, workspacePath, "rev-parse", "--show-toplevel"); err != nil {
		return domain.PullRequest{}, fmt.Errorf("workspace is not a git repository: %w", err)
	}

	dirtyPaths, err := c.dirtyWorktreePaths(ctx, workspacePath)
	if err != nil {
		return domain.PullRequest{}, err
	}
	if len(dirtyPaths) > 0 {
		return domain.PullRequest{}, fmt.Errorf("workspace has uncommitted changes; commit before PR creation: %s", strings.Join(dirtyPaths, ", "))
	}

	headBranch, err := c.resolveHeadBranch(ctx, issue, workspacePath)
	if err != nil {
		return domain.PullRequest{}, err
	}
	if err := c.pushBranch(ctx, workspacePath, headBranch); err != nil {
		return domain.PullRequest{}, err
	}

	if pullRequest, ok, err := c.findOpenPullRequest(ctx, headBranch); err != nil {
		return domain.PullRequest{}, err
	} else if ok {
		pullRequest.Created = false
		return pullRequest, nil
	}

	pullRequest, err := c.createPullRequest(ctx, issue, headBranch)
	if err != nil {
		return domain.PullRequest{}, err
	}
	pullRequest.Created = true
	return pullRequest, nil
}

func (c *Client) dirtyWorktreePaths(ctx context.Context, workspacePath string) ([]string, error) {
	output, err := c.gitOutput(ctx, workspacePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("inspect git worktree: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || len(line) < 4 {
			continue
		}

		pathSpec := strings.TrimSpace(line[3:])
		if idx := strings.Index(pathSpec, " -> "); idx >= 0 {
			left := strings.TrimSpace(pathSpec[:idx])
			right := strings.TrimSpace(pathSpec[idx+4:])
			if !isHarnessArtifact(left) {
				paths = append(paths, left)
			}
			if !isHarnessArtifact(right) {
				paths = append(paths, right)
			}
			continue
		}
		if isHarnessArtifact(pathSpec) {
			continue
		}
		paths = append(paths, pathSpec)
	}
	return paths, nil
}

func isHarnessArtifact(path string) bool {
	trimmed := strings.TrimSpace(strings.Trim(path, `"`))
	trimmed = filepath.ToSlash(trimmed)
	return trimmed == ".harness" || strings.HasPrefix(trimmed, ".harness/")
}

func (c *Client) resolveHeadBranch(ctx context.Context, issue domain.Issue, workspacePath string) (string, error) {
	headBranch := strings.TrimSpace(issue.BranchName)
	if headBranch == "" {
		currentBranch, err := c.gitOutput(ctx, workspacePath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", fmt.Errorf("resolve git branch: %w", err)
		}
		headBranch = strings.TrimSpace(currentBranch)
	}
	if headBranch == "" || headBranch == "HEAD" {
		return "", fmt.Errorf("github pull request creation requires issue.branch_name or a checked out branch")
	}
	if strings.EqualFold(headBranch, c.cfg.BaseBranch) {
		return "", fmt.Errorf("github pull request head branch %q must differ from github.base_branch", headBranch)
	}
	return headBranch, nil
}

func (c *Client) pushBranch(ctx context.Context, workspacePath, headBranch string) error {
	remoteURL, err := c.remoteURL()
	if err != nil {
		return err
	}
	args := []string{}
	if isHTTPRemote(remoteURL) {
		authHeader := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.cfg.Token))
		args = append(args, "-c", "http.extraheader="+authHeader)
	}
	args = append(args, "push", "--force", remoteURL, "HEAD:refs/heads/"+headBranch)
	if _, err := c.gitOutput(ctx, workspacePath, args...); err != nil {
		return fmt.Errorf("push branch %q: %w", headBranch, err)
	}
	return nil
}

func (c *Client) findOpenPullRequest(ctx context.Context, headBranch string) (domain.PullRequest, bool, error) {
	endpoint, err := c.apiURL(fmt.Sprintf("/repos/%s/%s/pulls", c.cfg.Owner, c.cfg.Repo))
	if err != nil {
		return domain.PullRequest{}, false, err
	}

	query := endpoint.Query()
	query.Set("state", "open")
	query.Set("head", c.cfg.Owner+":"+headBranch)
	query.Set("base", c.cfg.BaseBranch)
	endpoint.RawQuery = query.Encode()

	var response []pullRequestResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint.String(), nil, &response); err != nil {
		return domain.PullRequest{}, false, fmt.Errorf("find open pull requests: %w", err)
	}
	if len(response) == 0 {
		return domain.PullRequest{}, false, nil
	}
	return domain.PullRequest{
		Number:     response[0].Number,
		URL:        response[0].HTMLURL,
		HeadBranch: headBranch,
		BaseBranch: c.cfg.BaseBranch,
	}, true, nil
}

func (c *Client) createPullRequest(ctx context.Context, issue domain.Issue, headBranch string) (domain.PullRequest, error) {
	endpoint, err := c.apiURL(fmt.Sprintf("/repos/%s/%s/pulls", c.cfg.Owner, c.cfg.Repo))
	if err != nil {
		return domain.PullRequest{}, err
	}

	payload := map[string]any{
		"title": prTitle(issue, headBranch),
		"head":  headBranch,
		"base":  c.cfg.BaseBranch,
		"body":  prBody(issue),
		"draft": c.cfg.DraftPullRequest,
	}

	var response pullRequestResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint.String(), payload, &response); err != nil {
		return domain.PullRequest{}, fmt.Errorf("create pull request: %w", err)
	}
	return domain.PullRequest{
		Number:     response.Number,
		URL:        response.HTMLURL,
		HeadBranch: headBranch,
		BaseBranch: c.cfg.BaseBranch,
	}, nil
}

func prTitle(issue domain.Issue, headBranch string) string {
	switch {
	case strings.TrimSpace(issue.Identifier) != "" && strings.TrimSpace(issue.Title) != "":
		return strings.TrimSpace(issue.Identifier) + ": " + strings.TrimSpace(issue.Title)
	case strings.TrimSpace(issue.Title) != "":
		return strings.TrimSpace(issue.Title)
	case strings.TrimSpace(issue.Identifier) != "":
		return strings.TrimSpace(issue.Identifier)
	default:
		return strings.TrimSpace(headBranch)
	}
}

func prBody(issue domain.Issue) string {
	lines := []string{"Automated PR created by go-harness."}
	if strings.TrimSpace(issue.Identifier) != "" || strings.TrimSpace(issue.URL) != "" {
		lines = append(lines, "", "Tracking:")
		if strings.TrimSpace(issue.Identifier) != "" {
			lines = append(lines, "- Issue: "+strings.TrimSpace(issue.Identifier))
		}
		if strings.TrimSpace(issue.URL) != "" {
			lines = append(lines, "- Tracker: "+strings.TrimSpace(issue.URL))
		}
	}
	return strings.Join(lines, "\n")
}

func (c *Client) remoteURL() (string, error) {
	if strings.TrimSpace(c.cfg.RemoteURL) != "" {
		return strings.TrimSpace(c.cfg.RemoteURL), nil
	}

	endpoints, err := resolveEndpointURLs(c.cfg.Endpoint)
	if err != nil {
		return "", err
	}

	remote := buildURL(endpoints.webBase, fmt.Sprintf("/%s/%s.git", c.cfg.Owner, c.cfg.Repo))
	return remote.String(), nil
}

func (c *Client) apiURL(path string) (*url.URL, error) {
	endpoints, err := resolveEndpointURLs(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	return buildURL(endpoints.apiBase, path), nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeGitHubAPIError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func decodeGitHubAPIError(statusCode int, raw []byte) error {
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		parts := make([]string, 0, len(payload.Errors)+1)
		if strings.TrimSpace(payload.Message) != "" {
			parts = append(parts, strings.TrimSpace(payload.Message))
		}
		for _, item := range payload.Errors {
			if strings.TrimSpace(item.Message) != "" {
				parts = append(parts, strings.TrimSpace(item.Message))
			}
		}
		if len(parts) > 0 {
			return fmt.Errorf("github api status %d: %s", statusCode, strings.Join(parts, "; "))
		}
	}
	message := strings.TrimSpace(string(raw))
	if len(message) > 512 {
		message = message[:512]
	}
	return fmt.Errorf("github api status %d: %s", statusCode, message)
}

func isHTTPRemote(remote string) bool {
	parsed, err := url.Parse(remote)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func (c *Client) gitOutput(ctx context.Context, workspacePath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspacePath
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		details := strings.TrimSpace(stderr.String())
		if details == "" {
			details = strings.TrimSpace(stdout.String())
		}
		if details == "" {
			details = err.Error()
		}
		return "", fmt.Errorf("git command failed: %s", details)
	}
	return strings.TrimSpace(stdout.String()), nil
}
