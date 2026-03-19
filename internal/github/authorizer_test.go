package github

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"go-harness/internal/config"
)

func TestAuthorizerResolveConfigUsesGitHubCLIForGitHubDotCom(t *testing.T) {
	authorizer := NewAuthorizer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	authorizer.lookupPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	var gotName string
	var gotArgs []string
	authorizer.runCommand = func(_ context.Context, name string, args ...string) (string, error) {
		gotName = name
		gotArgs = append([]string{}, args...)
		return "cli-token\n", nil
	}

	resolved, err := authorizer.ResolveConfig(context.Background(), config.GitHubConfig{
		Endpoint:   "https://github.com/",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if gotName != "gh" {
		t.Fatalf("command name = %q, want gh", gotName)
	}
	if want := []string{"auth", "token", "--hostname", "github.com"}; len(gotArgs) != len(want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	} else {
		for i := range want {
			if gotArgs[i] != want[i] {
				t.Fatalf("args = %#v, want %#v", gotArgs, want)
			}
		}
	}
	if resolved.Token != "cli-token" {
		t.Fatalf("Token = %q, want cli-token", resolved.Token)
	}
}

func TestAuthorizerResolveConfigUsesGitHubCLIForEnterpriseHost(t *testing.T) {
	authorizer := NewAuthorizer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	authorizer.lookupPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	var gotArgs []string
	authorizer.runCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		gotArgs = append([]string{}, args...)
		return "enterprise-token", nil
	}

	resolved, err := authorizer.ResolveConfig(context.Background(), config.GitHubConfig{
		Endpoint:   "https://github.krafton.com/",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if len(gotArgs) != 4 || gotArgs[3] != "github.krafton.com" {
		t.Fatalf("args = %#v, want hostname github.krafton.com", gotArgs)
	}
	if resolved.Token != "enterprise-token" {
		t.Fatalf("Token = %q, want enterprise-token", resolved.Token)
	}
}

func TestAuthorizerResolveConfigCachesByHostname(t *testing.T) {
	authorizer := NewAuthorizer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	authorizer.lookupPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	calls := 0
	authorizer.runCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		return "cached-token", nil
	}

	cfg := config.GitHubConfig{
		Endpoint:   "https://github.krafton.com/api/v3",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	}
	first, err := authorizer.ResolveConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	second, err := authorizer.ResolveConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveConfig() second error = %v", err)
	}
	if first.Token != "cached-token" || second.Token != "cached-token" {
		t.Fatalf("tokens = %q, %q; want cached-token", first.Token, second.Token)
	}
	if calls != 1 {
		t.Fatalf("gh auth token calls = %d, want 1", calls)
	}
}

func TestAuthorizerResolveConfigRequiresGitHubCLIWhenTokenMissing(t *testing.T) {
	authorizer := NewAuthorizer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	authorizer.lookupPath = func(string) (string, error) { return "", errors.New("missing") }

	_, err := authorizer.ResolveConfig(context.Background(), config.GitHubConfig{
		Endpoint:   "https://github.com/",
		Owner:      "acme",
		Repo:       "widgets",
		BaseBranch: "main",
	})
	if err == nil {
		t.Fatal("ResolveConfig() error = nil, want missing gh error")
	}
}
