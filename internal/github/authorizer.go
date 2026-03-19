package github

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"go-harness/internal/config"
)

type commandRunner func(context.Context, string, ...string) (string, error)

type Authorizer struct {
	logger     *slog.Logger
	lookupPath func(string) (string, error)
	runCommand commandRunner

	mu    sync.Mutex
	cache map[string]string
}

func NewAuthorizer(logger *slog.Logger) *Authorizer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Authorizer{
		logger:     logger,
		lookupPath: exec.LookPath,
		runCommand: runCommand,
		cache:      map[string]string{},
	}
}

func (a *Authorizer) ResolveConfig(ctx context.Context, cfg config.GitHubConfig) (config.GitHubConfig, error) {
	if strings.TrimSpace(cfg.Token) != "" {
		return cfg, nil
	}

	endpoints, err := resolveEndpointURLs(cfg.Endpoint)
	if err != nil {
		return cfg, err
	}
	hostname := endpoints.webBase.Hostname()

	a.mu.Lock()
	if token := strings.TrimSpace(a.cache[hostname]); token != "" {
		a.mu.Unlock()
		cfg.Token = token
		return cfg, nil
	}
	a.mu.Unlock()

	if _, err := a.lookupPath("gh"); err != nil {
		return cfg, fmt.Errorf("github.token is empty and GitHub CLI is not installed; run `gh auth login --hostname %s` or set github.token", hostname)
	}

	a.logger.Info("resolving github token from gh auth", slog.String("hostname", hostname))
	token, err := a.runCommand(ctx, "gh", "auth", "token", "--hostname", hostname)
	if err != nil {
		return cfg, fmt.Errorf("github.token is empty and GitHub CLI has no usable auth for %s: %w; run `gh auth login --hostname %s` or set github.token", hostname, err, hostname)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return cfg, fmt.Errorf("github.token is empty and GitHub CLI returned an empty token for %s; run `gh auth login --hostname %s` or set github.token", hostname, hostname)
	}

	a.mu.Lock()
	a.cache[hostname] = token
	a.mu.Unlock()
	cfg.Token = token
	return cfg, nil
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(),
		"GH_PROMPT_DISABLED=1",
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
		return "", fmt.Errorf("gh command failed: %s", details)
	}
	return strings.TrimSpace(stdout.String()), nil
}
