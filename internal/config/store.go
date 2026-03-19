package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go-harness/internal/domain"
	"go-harness/internal/workflow"
)

const (
	defaultLinearEndpoint  = "https://api.linear.app/graphql"
	defaultGitHubEndpoint  = "https://api.github.com"
	defaultPollingInterval = 30 * time.Second
	defaultHooksTimeout    = 60 * time.Second
	defaultMaxConcurrent   = 10
	defaultMaxTurns        = 20
	defaultMaxRetryBackoff = 5 * time.Minute
	defaultCodexCommand    = "codex app-server"
	defaultTurnTimeout     = time.Hour
	defaultReadTimeout     = 5 * time.Second
	defaultStallTimeout    = 5 * time.Minute
	defaultApprovalPolicy  = "never"
	defaultThreadSandbox   = "workspace-write"
	defaultLogLevel        = "info"
	ReviewWorkflowFilename = "REVIEW-WORKFLOW.md"
	reviewActiveStateName  = "In Review"
)

var (
	defaultActiveStates   = []string{"Todo", "In Progress"}
	defaultTerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
)

type Store struct {
	loader      *workflow.Loader
	validator   func(RuntimeConfig) error
	mu          sync.RWMutex
	path        string
	envPath     string
	cfg         RuntimeConfig
	workflowMod time.Time
	dotEnvState fileState
	err         error
	ready       bool
}

type RuntimeConfig struct {
	SourcePath     string
	PromptTemplate string
	Environment    EnvironmentConfig
	Tracker        TrackerConfig
	GitHub         GitHubConfig
	Polling        PollingConfig
	Workspace      WorkspaceConfig
	Hooks          HooksConfig
	Agent          AgentConfig
	Codex          CodexConfig
	Logging        LoggingConfig
	Server         ServerConfig
}

type TrackerConfig struct {
	Kind             string
	Endpoint         string
	APIKey           string
	ProjectSlug      string
	ActiveStates     []string
	TerminalStates   []string
	activeStateSet   map[string]struct{}
	terminalStateSet map[string]struct{}
}

type GitHubConfig struct {
	Endpoint         string
	Token            string
	Owner            string
	Repo             string
	BaseBranch       string
	RemoteURL        string
	DraftPullRequest bool
}

type PollingConfig struct {
	Interval time.Duration
}

type WorkspaceConfig struct {
	Root string
}

type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

type AgentConfig struct {
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoff            time.Duration
	MaxConcurrentAgentsByState map[string]int
}

type CodexConfig struct {
	Command           string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
	TurnTimeout       time.Duration
	ReadTimeout       time.Duration
	StallTimeout      time.Duration
}

type ServerConfig struct {
	Port int
}

type LoggingConfig struct {
	Level          string
	CapturePrompts bool
}

type EnvironmentConfig struct {
	DotEnvPath    string
	DotEnvPresent bool
	Entries       []EnvironmentEntry
}

type EnvironmentEntry struct {
	Name   string
	Value  string
	Source string
}

type ValidationError struct {
	Field   string
	Message string
}

type fileState struct {
	exists  bool
	modTime time.Time
}

type envResolver struct {
	dotEnv map[string]string
}

var envVarPattern = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

func NewStore(loader *workflow.Loader) *Store {
	return &Store{loader: loader}
}

func (s *Store) SetValidator(validator func(RuntimeConfig) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validator = validator
}

func (s *Store) LoadAndValidate(path string) (RuntimeConfig, error) {
	resolvedPath, err := resolveWorkflowPath(path)
	if err != nil {
		return RuntimeConfig{}, err
	}
	envPath := executableDotEnvPath()

	cfg, workflowMod, dotEnvState, err := s.loadConfig(resolvedPath, envPath)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if err := s.validateRuntime(cfg); err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return RuntimeConfig{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = resolvedPath
	s.envPath = envPath
	s.cfg = cfg
	s.workflowMod = workflowMod
	s.dotEnvState = dotEnvState
	s.err = nil
	s.ready = true

	return cfg, nil
}

func (s *Store) Current() RuntimeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) ReloadIfChanged() (RuntimeConfig, bool, error) {
	s.mu.RLock()
	if !s.ready {
		s.mu.RUnlock()
		return RuntimeConfig{}, false, fmt.Errorf("config store is not initialized")
	}
	path := s.path
	envPath := s.envPath
	lastWorkflowMod := s.workflowMod
	lastDotEnvState := s.dotEnvState
	current := s.cfg
	validator := s.validator
	s.mu.RUnlock()

	info, err := os.Stat(path)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return current, false, err
	}
	dotEnvState, err := readFileState(envPath)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return current, false, err
	}
	if info.ModTime().Equal(lastWorkflowMod) && dotEnvState == lastDotEnvState {
		if validator != nil {
			if err := validator(current); err != nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
				return current, false, err
			}
			s.mu.Lock()
			s.err = nil
			s.mu.Unlock()
		}
		return current, false, nil
	}

	cfg, workflowMod, dotEnvState, err := s.loadConfig(path, envPath)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return current, false, err
	}
	if validator != nil {
		if err := validator(cfg); err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			return current, false, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.workflowMod = workflowMod
	s.dotEnvState = dotEnvState
	s.err = nil

	return cfg, true, nil
}

func (s *Store) DispatchValidationError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *Store) validateRuntime(cfg RuntimeConfig) error {
	s.mu.RLock()
	validator := s.validator
	s.mu.RUnlock()
	if validator == nil {
		return nil
	}
	return validator(cfg)
}

func (c RuntimeConfig) IsActiveState(state string) bool {
	_, ok := c.Tracker.activeStateSet[domain.NormalizeState(state)]
	return ok
}

func (c RuntimeConfig) IsTerminalState(state string) bool {
	_, ok := c.Tracker.terminalStateSet[domain.NormalizeState(state)]
	return ok
}

func (c RuntimeConfig) MaxConcurrentForState(state string) int {
	if limit, ok := c.Agent.MaxConcurrentAgentsByState[domain.NormalizeState(state)]; ok && limit > 0 {
		return limit
	}
	return c.Agent.MaxConcurrentAgents
}

func applyConfig(cfg *RuntimeConfig, raw map[string]any, env envResolver) error {
	if trackerMap := mapValue(raw, "tracker"); trackerMap != nil {
		cfg.Tracker.Kind = stringValue(trackerMap, "kind", cfg.Tracker.Kind)
		cfg.Tracker.Endpoint = stringValue(trackerMap, "endpoint", cfg.Tracker.Endpoint)
		cfg.Tracker.APIKey = resolveSecret(stringValue(trackerMap, "api_key", cfg.Tracker.APIKey), env)
		cfg.Tracker.ProjectSlug = resolveEnvReference(stringValue(trackerMap, "project_slug", cfg.Tracker.ProjectSlug), env)
		if states := stringSliceValue(trackerMap, "active_states"); len(states) > 0 {
			cfg.Tracker.ActiveStates = states
		}
		if states := stringSliceValue(trackerMap, "terminal_states"); len(states) > 0 {
			cfg.Tracker.TerminalStates = states
		}
	}

	if githubMap := mapValue(raw, "github"); githubMap != nil {
		cfg.GitHub.Endpoint = resolveEnvReference(stringValue(githubMap, "endpoint", cfg.GitHub.Endpoint), env)
		cfg.GitHub.Token = resolveSecret(stringValue(githubMap, "token", cfg.GitHub.Token), env)
		cfg.GitHub.Owner = resolveEnvReference(stringValue(githubMap, "owner", cfg.GitHub.Owner), env)
		cfg.GitHub.Repo = resolveEnvReference(stringValue(githubMap, "repo", cfg.GitHub.Repo), env)
		cfg.GitHub.BaseBranch = resolveEnvReference(stringValue(githubMap, "base_branch", cfg.GitHub.BaseBranch), env)
		cfg.GitHub.RemoteURL = resolveEnvReference(stringValue(githubMap, "remote_url", cfg.GitHub.RemoteURL), env)
		if rawDraftPullRequest, ok := githubMap["draft_pull_request"]; ok {
			draftPullRequest, ok := rawDraftPullRequest.(bool)
			if !ok {
				return &ValidationError{Field: "github.draft_pull_request", Message: "must be a boolean"}
			}
			cfg.GitHub.DraftPullRequest = draftPullRequest
		}
	}

	if pollingMap := mapValue(raw, "polling"); pollingMap != nil {
		if value, ok := intValue(pollingMap, "interval_ms"); ok {
			cfg.Polling.Interval = time.Duration(value) * time.Millisecond
		}
	}

	if workspaceMap := mapValue(raw, "workspace"); workspaceMap != nil {
		cfg.Workspace.Root = resolvePath(stringValue(workspaceMap, "root", cfg.Workspace.Root), env)
	}

	if hooksMap := mapValue(raw, "hooks"); hooksMap != nil {
		cfg.Hooks.AfterCreate = stringValue(hooksMap, "after_create", cfg.Hooks.AfterCreate)
		cfg.Hooks.BeforeRun = stringValue(hooksMap, "before_run", cfg.Hooks.BeforeRun)
		cfg.Hooks.AfterRun = stringValue(hooksMap, "after_run", cfg.Hooks.AfterRun)
		cfg.Hooks.BeforeRemove = stringValue(hooksMap, "before_remove", cfg.Hooks.BeforeRemove)
		if value, ok := intValue(hooksMap, "timeout_ms"); ok {
			cfg.Hooks.Timeout = time.Duration(value) * time.Millisecond
		}
	}

	if agentMap := mapValue(raw, "agent"); agentMap != nil {
		if value, ok := intValue(agentMap, "max_concurrent_agents"); ok {
			cfg.Agent.MaxConcurrentAgents = value
		}
		if value, ok := intValue(agentMap, "max_turns"); ok {
			cfg.Agent.MaxTurns = value
		}
		if value, ok := intValue(agentMap, "max_retry_backoff_ms"); ok {
			cfg.Agent.MaxRetryBackoff = time.Duration(value) * time.Millisecond
		}
		if stateMap := mapValue(agentMap, "max_concurrent_agents_by_state"); stateMap != nil {
			out := make(map[string]int, len(stateMap))
			for key, rawValue := range stateMap {
				if value, ok := number(rawValue); ok && value > 0 {
					out[domain.NormalizeState(key)] = value
				}
			}
			cfg.Agent.MaxConcurrentAgentsByState = out
		}
	}

	if codexMap := mapValue(raw, "codex"); codexMap != nil {
		cfg.Codex.Command = stringValue(codexMap, "command", cfg.Codex.Command)
		cfg.Codex.ApprovalPolicy = stringValue(codexMap, "approval_policy", cfg.Codex.ApprovalPolicy)
		cfg.Codex.ThreadSandbox = stringValue(codexMap, "thread_sandbox", cfg.Codex.ThreadSandbox)
		if turnSandbox := mapValue(codexMap, "turn_sandbox_policy"); turnSandbox != nil {
			cfg.Codex.TurnSandboxPolicy = turnSandbox
		}
		if value, ok := intValue(codexMap, "turn_timeout_ms"); ok {
			cfg.Codex.TurnTimeout = time.Duration(value) * time.Millisecond
		}
		if value, ok := intValue(codexMap, "read_timeout_ms"); ok {
			cfg.Codex.ReadTimeout = time.Duration(value) * time.Millisecond
		}
		if value, ok := intValue(codexMap, "stall_timeout_ms"); ok {
			cfg.Codex.StallTimeout = time.Duration(value) * time.Millisecond
		}
	}

	if loggingMap := mapValue(raw, "logging"); loggingMap != nil {
		cfg.Logging.Level = stringValue(loggingMap, "level", cfg.Logging.Level)
		if rawCapturePrompts, ok := loggingMap["capture_prompts"]; ok {
			capturePrompts, ok := rawCapturePrompts.(bool)
			if !ok {
				return &ValidationError{Field: "logging.capture_prompts", Message: "must be a boolean"}
			}
			cfg.Logging.CapturePrompts = capturePrompts
		}
	}

	if serverMap := mapValue(raw, "server"); serverMap != nil {
		if value, ok := intValue(serverMap, "port"); ok {
			cfg.Server.Port = value
		}
	}

	return nil
}

func validateAndNormalize(cfg *RuntimeConfig, env envResolver) error {
	cfg.Tracker.Kind = strings.TrimSpace(cfg.Tracker.Kind)
	if cfg.Tracker.Kind == "" {
		return &ValidationError{Field: "tracker.kind", Message: "is required"}
	}
	if cfg.Tracker.Kind != "linear" {
		return &ValidationError{Field: "tracker.kind", Message: "only \"linear\" is supported"}
	}
	if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
		return &ValidationError{Field: "tracker.api_key", Message: "must resolve to a non-empty token"}
	}
	if strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" {
		return &ValidationError{Field: "tracker.project_slug", Message: "is required"}
	}
	if strings.TrimSpace(cfg.GitHub.Endpoint) == "" {
		return &ValidationError{Field: "github.endpoint", Message: "must not be empty"}
	}
	if strings.TrimSpace(cfg.GitHub.Owner) == "" {
		return &ValidationError{Field: "github.owner", Message: "is required"}
	}
	if strings.TrimSpace(cfg.GitHub.Repo) == "" {
		return &ValidationError{Field: "github.repo", Message: "is required"}
	}
	if strings.TrimSpace(cfg.GitHub.BaseBranch) == "" {
		return &ValidationError{Field: "github.base_branch", Message: "is required"}
	}
	if cfg.Polling.Interval <= 0 {
		return &ValidationError{Field: "polling.interval_ms", Message: "must be positive"}
	}
	if cfg.Hooks.Timeout <= 0 {
		return &ValidationError{Field: "hooks.timeout_ms", Message: "must be positive"}
	}
	if cfg.Agent.MaxConcurrentAgents <= 0 {
		return &ValidationError{Field: "agent.max_concurrent_agents", Message: "must be positive"}
	}
	if cfg.Agent.MaxTurns <= 0 {
		return &ValidationError{Field: "agent.max_turns", Message: "must be positive"}
	}
	if cfg.Agent.MaxRetryBackoff <= 0 {
		return &ValidationError{Field: "agent.max_retry_backoff_ms", Message: "must be positive"}
	}
	if strings.TrimSpace(cfg.Codex.Command) == "" {
		return &ValidationError{Field: "codex.command", Message: "must not be empty"}
	}
	if cfg.Codex.TurnTimeout <= 0 {
		return &ValidationError{Field: "codex.turn_timeout_ms", Message: "must be positive"}
	}
	if cfg.Codex.ReadTimeout <= 0 {
		return &ValidationError{Field: "codex.read_timeout_ms", Message: "must be positive"}
	}
	if cfg.Codex.StallTimeout <= 0 {
		return &ValidationError{Field: "codex.stall_timeout_ms", Message: "must be positive"}
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Logging.Level)) {
	case "debug", "info", "warn", "error":
		cfg.Logging.Level = strings.ToLower(strings.TrimSpace(cfg.Logging.Level))
	default:
		return &ValidationError{Field: "logging.level", Message: "must be one of debug, info, warn, error"}
	}
	if cfg.Server.Port < 0 {
		return &ValidationError{Field: "server.port", Message: "must be non-negative"}
	}

	root, err := filepath.Abs(resolvePath(cfg.Workspace.Root, env))
	if err != nil {
		return err
	}
	cfg.Workspace.Root = filepath.Clean(root)
	cfg.Tracker.ActiveStates = normalizeStates(cfg.Tracker.ActiveStates)
	cfg.Tracker.TerminalStates = normalizeStates(cfg.Tracker.TerminalStates)
	cfg.Tracker.activeStateSet = makeStateSet(cfg.Tracker.ActiveStates)
	cfg.Tracker.terminalStateSet = makeStateSet(cfg.Tracker.TerminalStates)

	return nil
}

func resolveWorkflowPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		cwdPath := filepath.Join(cwd, "WORKFLOW.md")
		executablePath := executableWorkflowPath()

		resolved, err := firstExistingPath(cwdPath, executablePath)
		if err != nil {
			return "", err
		}
		if resolved == "" {
			path = cwdPath
			if executablePath != "" && filepath.Clean(executablePath) != filepath.Clean(cwdPath) {
				path = executablePath
			}
		} else {
			path = resolved
		}
	}

	return filepath.Abs(path)
}

func ResolveSiblingWorkflowPath(baseWorkflowPath, filename string) (string, bool, error) {
	baseWorkflowPath = strings.TrimSpace(baseWorkflowPath)
	filename = strings.TrimSpace(filename)
	if baseWorkflowPath == "" {
		return "", false, fmt.Errorf("base workflow path is required")
	}
	if filename == "" {
		return "", false, fmt.Errorf("sibling workflow filename is required")
	}
	path := filepath.Join(filepath.Dir(baseWorkflowPath), filename)
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return "", false, nil
		}
		resolved, err := filepath.Abs(path)
		if err != nil {
			return "", false, err
		}
		return resolved, true, nil
	}
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return "", false, err
}

func ValidateReviewWorkflow(mainCfg, reviewCfg RuntimeConfig) error {
	if !strings.EqualFold(strings.TrimSpace(reviewCfg.Tracker.Kind), "linear") {
		return fmt.Errorf("review workflow must use tracker.kind=linear")
	}
	if strings.TrimSpace(reviewCfg.GitHub.Endpoint) != strings.TrimSpace(mainCfg.GitHub.Endpoint) {
		return fmt.Errorf("review workflow github.endpoint must match main workflow")
	}
	if strings.TrimSpace(reviewCfg.GitHub.Owner) != strings.TrimSpace(mainCfg.GitHub.Owner) {
		return fmt.Errorf("review workflow github.owner must match main workflow")
	}
	if strings.TrimSpace(reviewCfg.GitHub.Repo) != strings.TrimSpace(mainCfg.GitHub.Repo) {
		return fmt.Errorf("review workflow github.repo must match main workflow")
	}
	if strings.TrimSpace(reviewCfg.GitHub.BaseBranch) != strings.TrimSpace(mainCfg.GitHub.BaseBranch) {
		return fmt.Errorf("review workflow github.base_branch must match main workflow")
	}
	if strings.TrimSpace(reviewCfg.GitHub.RemoteURL) != strings.TrimSpace(mainCfg.GitHub.RemoteURL) {
		return fmt.Errorf("review workflow github.remote_url must match main workflow")
	}
	if strings.TrimSpace(reviewCfg.Tracker.ProjectSlug) != strings.TrimSpace(mainCfg.Tracker.ProjectSlug) {
		return fmt.Errorf("review workflow tracker.project_slug must match main workflow")
	}
	if filepath.Clean(reviewCfg.Workspace.Root) != filepath.Clean(mainCfg.Workspace.Root) {
		return fmt.Errorf("review workflow workspace.root must match main workflow")
	}
	if len(reviewCfg.Tracker.ActiveStates) != 1 || !strings.EqualFold(strings.TrimSpace(reviewCfg.Tracker.ActiveStates[0]), reviewActiveStateName) {
		return fmt.Errorf("review workflow tracker.active_states must be [\"%s\"]", reviewActiveStateName)
	}
	if !sameNormalizedStates(mainCfg.Tracker.TerminalStates, reviewCfg.Tracker.TerminalStates) {
		return fmt.Errorf("review workflow tracker.terminal_states must match main workflow")
	}
	return nil
}

func executableWorkflowPath() string {
	executablePath, err := os.Executable()
	if err != nil || strings.TrimSpace(executablePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(executablePath), "WORKFLOW.md")
}

func firstExistingPath(paths ...string) (string, error) {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				continue
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", nil
}

func (s *Store) loadConfig(path, envPath string) (RuntimeConfig, time.Time, fileState, error) {
	definition, err := s.loader.Load(path)
	if err != nil {
		return RuntimeConfig{}, time.Time{}, fileState{}, err
	}
	dotEnv, dotEnvState, err := loadDotEnvFile(envPath)
	if err != nil {
		return RuntimeConfig{}, time.Time{}, fileState{}, err
	}
	env := envResolver{dotEnv: dotEnv}

	cfg := RuntimeConfig{
		SourcePath:     definition.SourcePath,
		PromptTemplate: definition.PromptTemplate,
		Environment: EnvironmentConfig{
			DotEnvPath:    envPath,
			DotEnvPresent: dotEnvState.exists,
		},
		Tracker: TrackerConfig{
			Endpoint:       defaultLinearEndpoint,
			ActiveStates:   append([]string{}, defaultActiveStates...),
			TerminalStates: append([]string{}, defaultTerminalStates...),
		},
		GitHub: GitHubConfig{
			Endpoint: defaultGitHubEndpoint,
		},
		Polling: PollingConfig{Interval: defaultPollingInterval},
		Workspace: WorkspaceConfig{
			Root: filepath.Join(os.TempDir(), "symphony_workspaces"),
		},
		Hooks: HooksConfig{Timeout: defaultHooksTimeout},
		Agent: AgentConfig{
			MaxConcurrentAgents:        defaultMaxConcurrent,
			MaxTurns:                   defaultMaxTurns,
			MaxRetryBackoff:            defaultMaxRetryBackoff,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: CodexConfig{
			Command:           defaultCodexCommand,
			ApprovalPolicy:    defaultApprovalPolicy,
			ThreadSandbox:     defaultThreadSandbox,
			TurnSandboxPolicy: map[string]any{"type": "workspace-write"},
			TurnTimeout:       defaultTurnTimeout,
			ReadTimeout:       defaultReadTimeout,
			StallTimeout:      defaultStallTimeout,
		},
		Logging: LoggingConfig{
			Level:          defaultLogLevel,
			CapturePrompts: false,
		},
	}

	if err := applyConfig(&cfg, definition.Config, env); err != nil {
		return RuntimeConfig{}, time.Time{}, fileState{}, err
	}
	if err := validateAndNormalize(&cfg, env); err != nil {
		return RuntimeConfig{}, time.Time{}, fileState{}, err
	}
	cfg.Environment.Entries = buildEnvironmentEntries(definition.Config, env)

	info, err := os.Stat(definition.SourcePath)
	if err != nil {
		return RuntimeConfig{}, time.Time{}, fileState{}, err
	}

	return cfg, info.ModTime(), dotEnvState, nil
}

func mapValue(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	value, ok := raw[key]
	if !ok {
		return nil
	}
	mapped, _ := value.(map[string]any)
	return mapped
}

func stringValue(raw map[string]any, key, fallback string) string {
	if raw == nil {
		return fallback
	}
	value, ok := raw[key]
	if !ok {
		return fallback
	}
	if typed, ok := value.(string); ok {
		return strings.TrimSpace(typed)
	}
	return fallback
}

func stringSliceValue(raw map[string]any, key string) []string {
	if raw == nil {
		return nil
	}
	value, ok := raw[key]
	if !ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if typed, ok := item.(string); ok && strings.TrimSpace(typed) != "" {
			out = append(out, strings.TrimSpace(typed))
		}
	}
	return out
}

func intValue(raw map[string]any, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	value, ok := raw[key]
	if !ok {
		return 0, false
	}
	return number(value)
}

func number(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

func resolveSecret(value string, env envResolver) string {
	if !strings.HasPrefix(value, "$") {
		return strings.TrimSpace(value)
	}
	resolved, _ := env.Lookup(strings.TrimPrefix(value, "$"))
	return strings.TrimSpace(resolved)
}

func resolveEnvReference(value string, env envResolver) string {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "$") {
		return trimmed
	}
	resolved, _ := env.Lookup(strings.TrimPrefix(trimmed, "$"))
	return strings.TrimSpace(resolved)
}

func resolvePath(value string, env envResolver) string {
	if strings.TrimSpace(value) == "" {
		return value
	}

	out := value
	if strings.HasPrefix(out, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			out = filepath.Join(home, strings.TrimPrefix(out, "~"))
		}
	}

	return os.Expand(out, func(key string) string {
		value, _ := env.Lookup(key)
		return value
	})
}

func (e envResolver) Lookup(key string) (string, bool) {
	if value, ok := os.LookupEnv(key); ok {
		return value, true
	}
	value, ok := e.dotEnv[key]
	return value, ok
}

func executableDotEnvPath() string {
	executablePath, err := os.Executable()
	if err != nil || strings.TrimSpace(executablePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(executablePath), ".env")
}

func readFileState(path string) (fileState, error) {
	if strings.TrimSpace(path) == "" {
		return fileState{}, nil
	}

	info, err := os.Stat(path)
	if err == nil {
		return fileState{exists: true, modTime: info.ModTime()}, nil
	}
	if os.IsNotExist(err) {
		return fileState{}, nil
	}
	return fileState{}, err
}

func loadDotEnvFile(path string) (map[string]string, fileState, error) {
	state, err := readFileState(path)
	if err != nil {
		return nil, fileState{}, err
	}
	if !state.exists {
		return map[string]string{}, state, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fileState{}, err
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		if err := parseDotEnvLine(scanner.Text(), values); err != nil {
			return nil, fileState{}, fmt.Errorf("parse %s:%d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fileState{}, err
	}
	return values, state, nil
}

func parseDotEnvLine(line string, values map[string]string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil
	}
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}

	separator := strings.Index(trimmed, "=")
	if separator <= 0 {
		return fmt.Errorf("expected KEY=VALUE")
	}

	key := strings.TrimSpace(trimmed[:separator])
	if key == "" || strings.ContainsAny(key, " \t") {
		return fmt.Errorf("invalid key %q", key)
	}

	value := strings.TrimSpace(trimmed[separator+1:])
	if len(value) >= 2 {
		if value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		} else if value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
	} else if comment := strings.Index(value, " #"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}

	values[key] = value
	return nil
}

func normalizeStates(states []string) []string {
	result := make([]string, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		trimmed := strings.TrimSpace(state)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		result = append(result, trimmed)
		seen[trimmed] = struct{}{}
	}
	return result
}

func sameNormalizedStates(left, right []string) bool {
	leftNormalized := normalizeStates(left)
	rightNormalized := normalizeStates(right)
	if len(leftNormalized) != len(rightNormalized) {
		return false
	}
	leftSet := makeStateSet(leftNormalized)
	rightSet := makeStateSet(rightNormalized)
	if len(leftSet) != len(rightSet) {
		return false
	}
	for key := range leftSet {
		if _, ok := rightSet[key]; !ok {
			return false
		}
	}
	return true
}

func makeStateSet(states []string) map[string]struct{} {
	result := make(map[string]struct{}, len(states))
	for _, state := range states {
		result[domain.NormalizeState(state)] = struct{}{}
	}
	return result
}

func buildEnvironmentEntries(raw map[string]any, env envResolver) []EnvironmentEntry {
	keys := referencedEnvKeys(raw)
	for key := range env.dotEnv {
		keys[key] = struct{}{}
	}
	if len(keys) == 0 {
		return nil
	}

	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	slices.Sort(sorted)

	entries := make([]EnvironmentEntry, 0, len(sorted))
	for _, key := range sorted {
		entry := EnvironmentEntry{Name: key, Source: "missing"}
		if value, ok := os.LookupEnv(key); ok {
			entry.Source = "process"
			entry.Value = redactEnvValue(key, value)
		} else if value, ok := env.dotEnv[key]; ok {
			entry.Source = ".env"
			entry.Value = redactEnvValue(key, value)
		}
		entries = append(entries, entry)
	}
	return entries
}

func referencedEnvKeys(value any) map[string]struct{} {
	result := make(map[string]struct{})
	collectEnvKeys(result, value)
	return result
}

func collectEnvKeys(out map[string]struct{}, value any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			collectEnvKeys(out, item)
		}
	case []any:
		for _, item := range typed {
			collectEnvKeys(out, item)
		}
	case string:
		matches := envVarPattern.FindAllStringSubmatch(typed, -1)
		for _, match := range matches {
			key := match[1]
			if key == "" {
				key = match[2]
			}
			if key != "" {
				out[key] = struct{}{}
			}
		}
	}
}

func redactEnvValue(key, value string) string {
	if value == "" {
		return ""
	}
	name := strings.ToLower(strings.TrimSpace(key))
	if strings.Contains(name, "key") ||
		strings.Contains(name, "token") ||
		strings.Contains(name, "secret") ||
		strings.Contains(name, "password") ||
		strings.Contains(name, "passwd") ||
		strings.Contains(name, "pwd") ||
		strings.Contains(name, "auth") ||
		strings.Contains(name, "credential") ||
		strings.Contains(name, "cookie") ||
		strings.Contains(name, "session") ||
		strings.Contains(name, "private") {
		return "<redacted>"
	}
	if len(value) > 512 {
		return value[:512]
	}
	return value
}
