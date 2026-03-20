package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

func TestEnsurePullRequestCreatesAndPushesBranch(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			if got := r.URL.Query().Get("head"); got != "acme:feature/ABC-1" {
				t.Fatalf("head query = %q, want acme:feature/ABC-1", got)
			}
			if got := r.URL.Query().Get("base"); got != "main" {
				t.Fatalf("base query = %q, want main", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			createCalls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if payload["head"] != "feature/ABC-1" {
				t.Fatalf("payload head = %#v", payload["head"])
			}
			if payload["base"] != "main" {
				t.Fatalf("payload base = %#v", payload["base"])
			}
			if payload["title"] != "ABC-1: Example issue" {
				t.Fatalf("payload title = %#v", payload["title"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   17,
				"html_url": "https://github.example.com/acme/widgets/pull/17",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	pullRequest, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		URL:        "https://linear.example.com/ABC-1",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}

	if !pullRequest.Created {
		t.Fatal("Created = false, want true")
	}
	if pullRequest.URL != "https://github.example.com/acme/widgets/pull/17" {
		t.Fatalf("URL = %q", pullRequest.URL)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("createCalls = %d, want 1", createCalls.Load())
	}

	if output := gitOutput(t, workspacePath, "rev-parse", "HEAD"); output == "" {
		t.Fatal("workspace HEAD is empty")
	}
	remoteHead := gitOutput(t, remotePath, "rev-parse", "refs/heads/feature/ABC-1")
	workspaceHead := gitOutput(t, workspacePath, "rev-parse", "HEAD")
	if remoteHead != workspaceHead {
		t.Fatalf("remote head = %q, want %q", remoteHead, workspaceHead)
	}
}

func TestEnsurePullRequestReusesExistingOpenPullRequest(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	var postCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   23,
				"html_url": "https://github.example.com/acme/widgets/pull/23",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			postCalled.Store(true)
			t.Fatal("POST /pulls should not be called when an open PR already exists")
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	pullRequest, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}

	if pullRequest.Created {
		t.Fatal("Created = true, want false when reusing an existing PR")
	}
	if pullRequest.Number != 23 {
		t.Fatalf("Number = %d, want 23", pullRequest.Number)
	}
	if postCalled.Load() {
		t.Fatal("POST /pulls was called")
	}
}

func TestEnsurePullRequestForcePushesDivergedIssueBranch(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")
	runGit(t, workspacePath, "push", "origin", "HEAD:refs/heads/feature/ABC-1")

	otherClone := filepath.Join(t.TempDir(), "other")
	runGit(t, filepath.Dir(remotePath), "clone", remotePath, otherClone)
	runGit(t, otherClone, "checkout", "-b", "feature/ABC-1", "origin/feature/ABC-1")
	runGit(t, otherClone, "config", "user.name", "Harness Test")
	runGit(t, otherClone, "config", "user.email", "harness@example.com")
	commitFile(t, otherClone, "remote.txt", "remote\n", "remote update")
	runGit(t, otherClone, "push", "origin", "HEAD:refs/heads/feature/ABC-1")

	runGit(t, workspacePath, "reset", "--hard", "origin/main")
	commitFile(t, workspacePath, "local.txt", "local\n", "local retry commit")

	var postCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   23,
				"html_url": "https://github.example.com/acme/widgets/pull/23",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			postCalled.Store(true)
			t.Fatal("POST /pulls should not be called when an open PR already exists")
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	pullRequest, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}

	if pullRequest.Created {
		t.Fatal("Created = true, want false when reusing an existing PR")
	}
	remoteHead := gitOutput(t, remotePath, "rev-parse", "refs/heads/feature/ABC-1")
	workspaceHead := gitOutput(t, workspacePath, "rev-parse", "HEAD")
	if remoteHead != workspaceHead {
		t.Fatalf("remote head = %q, want %q after force push", remoteHead, workspaceHead)
	}
}

func TestEnsurePullRequestIgnoresHarnessArtifactsButRejectsOtherDirtyFiles(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	if err := os.MkdirAll(filepath.Join(workspacePath, ".harness"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.harness) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, ".harness", "review-notes.md"), []byte("review notes\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(review-notes) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspacePath, ".harness", "cache", "go-build"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.harness/cache/go-build) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, ".harness", "cache", "go-build", "cache.tmp"), []byte("cache\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(cache.tmp) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(dirty.txt) error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	_, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err == nil {
		t.Fatal("EnsurePullRequest() error = nil, want dirty-worktree error")
	}
	if !strings.Contains(err.Error(), "dirty.txt") {
		t.Fatalf("error = %v, want dirty.txt to be reported", err)
	}
}

func TestEnsurePullRequestReturnsDetailedGitHubValidationErrors(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "Validation Failed",
				"errors": []map[string]any{
					{
						"resource": "PullRequest",
						"field":    "base",
						"code":     "invalid",
						"message":  "No commits between main and feature/ABC-1",
					},
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	_, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err == nil {
		t.Fatal("EnsurePullRequest() error = nil, want github validation error")
	}
	for _, want := range []string{
		"Validation Failed",
		"PullRequest",
		"base",
		"invalid",
		"No commits between main and feature/ABC-1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}

func TestEnsurePullRequestRetriesTransientHeadInvalidError(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	originalDelays := pullRequestCreateRetryDelays
	pullRequestCreateRetryDelays = []time.Duration{0}
	t.Cleanup(func() {
		pullRequestCreateRetryDelays = originalDelays
	})

	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			if createCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": "Validation Failed",
					"errors": []map[string]any{
						{
							"resource": "PullRequest",
							"field":    "head",
							"code":     "invalid",
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   29,
				"html_url": "https://github.example.com/acme/widgets/pull/29",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	pullRequest, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}
	if !pullRequest.Created {
		t.Fatal("Created = false, want true after transient retry")
	}
	if pullRequest.Number != 29 {
		t.Fatalf("Number = %d, want 29", pullRequest.Number)
	}
	if createCalls.Load() != 2 {
		t.Fatalf("createCalls = %d, want 2", createCalls.Load())
	}
}

func TestEnsurePullRequestReusesOpenPRWhenCreateReportsDuplicate(t *testing.T) {
	remotePath, workspacePath := setupGitWorkspace(t)
	commitFile(t, workspacePath, "feature.txt", "hello\n", "feature commit")

	originalDelays := pullRequestCreateRetryDelays
	pullRequestCreateRetryDelays = []time.Duration{0}
	t.Cleanup(func() {
		pullRequestCreateRetryDelays = originalDelays
	})

	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			if getCalls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   31,
				"html_url": "https://github.example.com/acme/widgets/pull/31",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/widgets/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "Validation Failed",
				"errors": []map[string]any{
					{
						"resource": "PullRequest",
						"code":     "custom",
						"message":  "A pull request already exists for acme:feature/ABC-1.",
					},
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(server.Client(), config.GitHubConfig{
		Endpoint:   server.URL + "/api/v3",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
		RemoteURL:  remotePath,
	})
	pullRequest, err := client.EnsurePullRequest(context.Background(), domain.Issue{
		Identifier: "ABC-1",
		Title:      "Example issue",
		BranchName: "feature/ABC-1",
	}, domain.Workspace{Path: workspacePath})
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}
	if pullRequest.Created {
		t.Fatal("Created = true, want false when reusing existing PR")
	}
	if pullRequest.Number != 31 {
		t.Fatalf("Number = %d, want 31", pullRequest.Number)
	}
}

func TestClientBuildsGitHubDotComAPIAndRemoteFromWebEndpoint(t *testing.T) {
	client := NewClient(http.DefaultClient, config.GitHubConfig{
		Endpoint:   "https://github.com/",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	})

	apiURL, err := client.apiURL("/repos/acme/widgets/pulls")
	if err != nil {
		t.Fatalf("apiURL() error = %v", err)
	}
	if apiURL.String() != "https://api.github.com/repos/acme/widgets/pulls" {
		t.Fatalf("apiURL = %q", apiURL.String())
	}

	remoteURL, err := client.remoteURL()
	if err != nil {
		t.Fatalf("remoteURL() error = %v", err)
	}
	if remoteURL != "https://github.com/acme/widgets.git" {
		t.Fatalf("remoteURL = %q", remoteURL)
	}
}

func TestClientBuildsEnterpriseAPIAndRemoteFromWebEndpoint(t *testing.T) {
	client := NewClient(http.DefaultClient, config.GitHubConfig{
		Endpoint:   "https://github.krafton.com/",
		Token:      "github-token",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	})

	apiURL, err := client.apiURL("/repos/acme/widgets/pulls")
	if err != nil {
		t.Fatalf("apiURL() error = %v", err)
	}
	if apiURL.String() != "https://github.krafton.com/api/v3/repos/acme/widgets/pulls" {
		t.Fatalf("apiURL = %q", apiURL.String())
	}

	remoteURL, err := client.remoteURL()
	if err != nil {
		t.Fatalf("remoteURL() error = %v", err)
	}
	if remoteURL != "https://github.krafton.com/acme/widgets.git" {
		t.Fatalf("remoteURL = %q", remoteURL)
	}
}

func setupGitWorkspace(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	remotePath := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "--bare", remotePath)

	seedPath := filepath.Join(root, "seed")
	runGit(t, root, "init", seedPath)
	runGit(t, seedPath, "config", "user.name", "Harness Test")
	runGit(t, seedPath, "config", "user.email", "harness@example.com")
	if err := os.WriteFile(filepath.Join(seedPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	runGit(t, seedPath, "add", "README.md")
	runGit(t, seedPath, "commit", "-m", "seed")
	runGit(t, seedPath, "branch", "-M", "main")
	runGit(t, seedPath, "remote", "add", "origin", remotePath)
	runGit(t, seedPath, "push", "origin", "main")
	runGit(t, remotePath, "symbolic-ref", "HEAD", "refs/heads/main")

	workspacePath := filepath.Join(root, "workspace")
	runGit(t, root, "clone", remotePath, workspacePath)
	runGit(t, workspacePath, "checkout", "-b", "feature/ABC-1")
	runGit(t, workspacePath, "config", "user.name", "Harness Test")
	runGit(t, workspacePath, "config", "user.email", "harness@example.com")

	return remotePath, workspacePath
}

func commitFile(t *testing.T, repoPath, name, contents, message string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(repoPath, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", name, err)
	}
	runGit(t, repoPath, "add", name)
	runGit(t, repoPath, "commit", "-m", message)
}

func gitOutput(t *testing.T, repoPath string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
	return strings.TrimSpace(string(output))
}

func runGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
}
