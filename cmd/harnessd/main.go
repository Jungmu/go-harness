package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go-harness/internal/config"
	gh "go-harness/internal/github"
	"go-harness/internal/orchestrator"
	"go-harness/internal/server"
	"go-harness/internal/workflow"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, http.DefaultClient))
}

func run(args []string, stdout io.Writer, httpClient *http.Client) int {
	if len(args) > 0 && args[0] == "status" {
		return runStatus(args[1:], stdout, httpClient)
	}
	return runDaemon(args)
}

func runDaemon(args []string) int {
	var port int
	flags := flag.NewFlagSet("harnessd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.IntVar(&port, "port", 0, "override the HTTP status server port")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	var workflowPath string
	if positional := flags.Args(); len(positional) > 0 {
		workflowPath = positional[0]
	}

	store := config.NewStore(workflow.NewLoader())
	cfg, err := store.LoadAndValidate(workflowPath)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Error("failed to load configuration", slog.Any("error", err))
		return 1
	}

	var codingLevelVar slog.LevelVar
	codingLevelVar.Set(parseLogLevel(cfg.Logging.Level))
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &codingLevelVar}))
	codingLogger := logger.With("worker", "coding")

	if port > 0 {
		cfg.Server.Port = port
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	githubAuthorizer := gh.NewAuthorizer(logger.With("component", "github_auth"))
	trackerClient := &dynamicTracker{store: store, httpClient: http.DefaultClient}
	workspaceManager := &dynamicWorkspaceManager{store: store, logger: codingLogger}
	runner := &dynamicRunner{store: store, logger: codingLogger}
	pullRequests := &dynamicPullRequestCreator{store: store, httpClient: http.DefaultClient, authorizer: githubAuthorizer}
	configSource := &loggingConfigSource{store: store, levelVar: &codingLevelVar}
	orch := orchestrator.New(cfg, trackerClient, workspaceManager, runner, codingLogger, orchestrator.WithConfigSource(configSource), orchestrator.WithWorkerName("coding"), orchestrator.WithPullRequestCreator(pullRequests))

	var reviewOrch *orchestrator.Orchestrator
	reviewPath, foundReview, err := config.ResolveSiblingWorkflowPath(cfg.SourcePath, config.ReviewWorkflowFilename)
	if err != nil {
		logger.Error("failed to resolve review workflow", slog.Any("error", err))
		return 1
	}
	if foundReview {
		reviewStore := config.NewStore(workflow.NewLoader())
		reviewStore.SetValidator(func(reviewCfg config.RuntimeConfig) error {
			return config.ValidateReviewWorkflow(store.Current(), reviewCfg)
		})
		reviewCfg, err := reviewStore.LoadAndValidate(reviewPath)
		if err != nil {
			logger.Error("failed to load review configuration", slog.Any("error", err))
			return 1
		}
		var reviewLevelVar slog.LevelVar
		reviewLevelVar.Set(parseLogLevel(reviewCfg.Logging.Level))
		reviewLogger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &reviewLevelVar})).With("worker", "review")
		reviewTracker := &dynamicTracker{store: reviewStore, httpClient: http.DefaultClient}
		reviewWorkspaceManager := &dynamicWorkspaceManager{store: reviewStore, logger: reviewLogger}
		reviewRunner := &dynamicRunner{store: reviewStore, logger: reviewLogger}
		reviewPullRequests := &dynamicPullRequestCreator{store: reviewStore, httpClient: http.DefaultClient, authorizer: githubAuthorizer}
		reviewConfigSource := &loggingConfigSource{store: reviewStore, levelVar: &reviewLevelVar}
		reviewOrch = orchestrator.New(reviewCfg, reviewTracker, reviewWorkspaceManager, reviewRunner, reviewLogger, orchestrator.WithConfigSource(reviewConfigSource), orchestrator.WithReviewMode(), orchestrator.WithPullRequestCreator(reviewPullRequests))
		if err := reviewPullRequests.Warmup(rootCtx); err != nil {
			logger.Error("failed to authorize github for review workflow", slog.Any("error", err))
			return 1
		}
		logStartupConfiguration(reviewLogger, reviewCfg)
		logger.Info("review workflow enabled", slog.String("review_workflow_path", reviewCfg.SourcePath), slog.String("workflow_dir", filepath.Dir(reviewCfg.SourcePath)))
	} else {
		logger.Info("review workflow disabled", slog.String("expected_path", filepath.Join(filepath.Dir(cfg.SourcePath), config.ReviewWorkflowFilename)))
	}

	if err := pullRequests.Warmup(rootCtx); err != nil {
		logger.Error("failed to authorize github", slog.Any("error", err))
		return 1
	}

	runtime := &runtimeSurface{
		codingSnapshot:      orch.Snapshot,
		codingIssueSnapshot: orch.IssueSnapshot,
		codingRefresh:       orch.TriggerRefresh,
	}
	if reviewOrch != nil {
		runtime.reviewSnapshot = reviewOrch.Snapshot
		runtime.reviewIssueSnapshot = reviewOrch.IssueSnapshot
		runtime.reviewRefresh = reviewOrch.TriggerRefresh
	}

	if err := orch.Start(rootCtx); err != nil {
		logger.Error("failed to start orchestrator", slog.Any("error", err))
		return 1
	}
	if reviewOrch != nil {
		if err := reviewOrch.Start(rootCtx); err != nil {
			logger.Error("failed to start review orchestrator", slog.Any("error", err))
			_ = orch.Stop(context.Background())
			return 1
		}
	}

	logger.Info("harness daemon started",
		slog.String("workflow_path", cfg.SourcePath),
		slog.String("log_level", cfg.Logging.Level),
		slog.Bool("capture_prompts", cfg.Logging.CapturePrompts),
		slog.Duration("poll_interval", cfg.Polling.Interval),
		slog.String("workspace_root", cfg.Workspace.Root),
		slog.Int("server_port", cfg.Server.Port),
	)
	logStartupConfiguration(logger, cfg)

	var httpServer *http.Server
	if cfg.Server.Port > 0 {
		httpServer = &http.Server{
			Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port),
			Handler: server.NewHandler(runtime.Snapshot, runtime.IssueSnapshot, runtime.TriggerRefresh),
		}

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("http server exited", slog.Any("error", err))
			}
		}()
		logger.Info("http status server listening", slog.String("addr", httpServer.Addr))
	}

	<-rootCtx.Done()
	logger.Info("shutting down harness daemon")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if httpServer != nil {
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("failed to shut down http server", slog.Any("error", err))
		}
	}

	if err := orch.Stop(shutdownCtx); err != nil {
		logger.Error("failed to stop orchestrator", slog.Any("error", err))
		return 1
	}
	if reviewOrch != nil {
		if err := reviewOrch.Stop(shutdownCtx); err != nil {
			logger.Error("failed to stop review orchestrator", slog.Any("error", err))
			return 1
		}
	}

	return 0
}

func runStatus(args []string, stdout io.Writer, httpClient *http.Client) int {
	flags := flag.NewFlagSet("harnessd status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var addr string
	flags.StringVar(&addr, "addr", "http://127.0.0.1:8080", "base address of the running harness daemon")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	url := strings.TrimRight(addr, "/") + "/api/v1/state"
	resp, err := httpClient.Get(url)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "status request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(os.Stderr, "status request failed: %s\n", resp.Status)
		return 1
	}

	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "status decode failed: %v\n", err)
		return 1
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "status encode failed: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, string(encoded))
	return 0
}

func logStartupConfiguration(logger *slog.Logger, cfg config.RuntimeConfig) {
	entries := make([]map[string]string, 0, len(cfg.Environment.Entries))
	for _, entry := range cfg.Environment.Entries {
		item := map[string]string{
			"name":   entry.Name,
			"source": entry.Source,
		}
		if entry.Value != "" {
			item["value"] = entry.Value
		}
		entries = append(entries, item)
	}

	logger.Info("resolved startup environment",
		slog.String("workflow_path", cfg.SourcePath),
		slog.Bool("capture_prompts", cfg.Logging.CapturePrompts),
		slog.String("dotenv_path", cfg.Environment.DotEnvPath),
		slog.Bool("dotenv_present", cfg.Environment.DotEnvPresent),
		slog.Any("environment_entries", entries),
	)
}
