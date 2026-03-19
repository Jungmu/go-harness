package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-harness/internal/config"
)

func TestRunStatusPrintsSnapshot(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/state" {
			t.Fatalf("path = %q, want /api/v1/state", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"generated_at":"2026-03-18T09:00:00Z","counts":{"running":1,"retrying":0}}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	exitCode := run([]string{"status", "--addr", server.URL}, &stdout, server.Client())
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), `"running": 1`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestLogStartupConfigurationPrintsEnvironmentEntries(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logStartupConfiguration(logger, config.RuntimeConfig{
		SourcePath: "/repo/WORKFLOW.md",
		Polling:    config.PollingConfig{Interval: 30 * time.Second},
		Workspace:  config.WorkspaceConfig{Root: "/tmp/workspaces"},
		Logging:    config.LoggingConfig{Level: "info", CapturePrompts: true},
		Server:     config.ServerConfig{Port: 8080},
		Environment: config.EnvironmentConfig{
			DotEnvPath:    "/repo/.env",
			DotEnvPresent: true,
			Entries: []config.EnvironmentEntry{
				{Name: "LINEAR_API_KEY", Value: "<redacted>", Source: ".env"},
				{Name: "GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG", Value: "improve-harness", Source: "process"},
			},
		},
	})

	logs := output.String()
	for _, expected := range []string{
		`"msg":"resolved startup environment"`,
		`"workflow_path":"/repo/WORKFLOW.md"`,
		`"capture_prompts":true`,
		`"dotenv_path":"/repo/.env"`,
		`"dotenv_present":true`,
		`"name":"LINEAR_API_KEY"`,
		`"value":"<redacted>"`,
		`"name":"GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG"`,
		`"value":"improve-harness"`,
	} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("logs missing %q: %s", expected, logs)
		}
	}
}
