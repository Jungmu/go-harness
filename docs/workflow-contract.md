# Workflow Contract

## Scope

This document describes the currently implemented `WORKFLOW.md` contract in the Go harness, plus the optional sibling `REVIEW-WORKFLOW.md` review-lane contract.

## File Resolution

The runtime resolves the workflow file in this order:

1. explicit CLI path
2. `WORKFLOW.md` in the current process working directory
3. `WORKFLOW.md` in the executable directory

If the file cannot be read, the loader returns `missing_workflow_file`.

If a sibling `REVIEW-WORKFLOW.md` exists next to the active `WORKFLOW.md`, the daemon starts a second in-process review lane from that file.

## Parsing Rules

- `WORKFLOW.md` may start with YAML front matter fenced by `---`.
- Front matter must decode to a map.
- Non-map front matter returns `workflow_front_matter_not_a_map`.
- Invalid YAML returns `invalid_workflow_yaml`.
- The Markdown body is the prompt template after trimming outer whitespace.

## Supported Front Matter Keys

- `tracker`
- `github`
- `polling`
- `workspace`
- `hooks`
- `agent`
- `codex`
- `logging`
- `server`

Unknown keys are ignored.

## Implemented Defaults

- `tracker.endpoint = https://api.linear.app/graphql`
- `tracker.active_states = ["Todo", "In Progress"]`
- `tracker.terminal_states = ["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]`
- `github.endpoint = https://api.github.com`
- `github.draft_pull_request = false`
- `polling.interval_ms = 30000`
- `workspace.root = <system-temp>/symphony_workspaces`
- `hooks.timeout_ms = 60000`
- `agent.max_concurrent_agents = 10`
- `agent.max_turns = 20`
- `agent.max_retry_backoff_ms = 300000`
- `codex.command = "codex app-server"`
- `codex.approval_policy = "never"`
- `codex.thread_sandbox = "workspace-write"`
- `codex.turn_sandbox_policy = {"type":"workspace-write"}`
- `codex.turn_timeout_ms = 3600000`
- `codex.read_timeout_ms = 5000`
- `codex.stall_timeout_ms = 300000`
- `logging.level = "info"`
- `logging.capture_prompts = false`

## Value Resolution

- `tracker.api_key` accepts a literal value or `$VAR_NAME`.
- `tracker.project_slug` accepts a Linear project `slugId` or an exact project name.
- `github.token` accepts a literal value or `$VAR_NAME`.
- `github.owner`, `github.repo`, `github.base_branch`, `github.endpoint`, and `github.remote_url` accept literal values or `$VAR_NAME`.
- `$VAR_NAME` resolves from the process environment first, then from `.env` in the executable directory.
- `workspace.root` supports `~` expansion and `$VAR` expansion using the same environment lookup order.
- `logging.level` accepts `debug`, `info`, `warn`, or `error`.
- `logging.capture_prompts` accepts a boolean. Non-boolean values return `invalid logging.capture_prompts: must be a boolean`.
- `github.draft_pull_request` accepts a boolean. Non-boolean values return `invalid github.draft_pull_request: must be a boolean`.
- The shell command in `codex.command` is passed directly to `bash -lc`.
- `github.endpoint` accepts GitHub web URLs and API URLs. Supported examples include `https://github.com/`, `https://api.github.com`, `https://github.krafton.com/`, and `https://github.krafton.com/api/v3`.
- `github.token` is optional. If it is empty, daemon startup runs `gh auth token --hostname <host>` using the host derived from `github.endpoint`.
- If GitHub CLI auth is missing for that host, startup fails and the operator must run `gh auth login --hostname <host>` or set `github.token`.
- If `github.remote_url` is omitted, the runtime derives the push remote from `github.endpoint`, `github.owner`, and `github.repo`.
- If no workflow path is passed and the current working directory has no `WORKFLOW.md`, the loader falls back to the executable directory before returning `missing_workflow_file`.

## Workspace Hook Environment

- Workspace hooks run with the issue workspace as `cwd`.
- Workspace hooks inherit the process environment.
- The runtime also injects `HARNESS_WORKFLOW_PATH` with the resolved workflow file path.
- The runtime also injects `HARNESS_WORKFLOW_DIR` with the directory that contains the active workflow file.
- If the runtime finds a `.git` entry while walking upward from `HARNESS_WORKFLOW_DIR`, it also injects `HARNESS_SOURCE_REPO` with that repository root.
- For compatibility with older local copies of the bundled workflow files, the runtime also injects `GO_HARNESS_SOURCE_REPO` with the same value as `HARNESS_SOURCE_REPO`.
- If `after_create` fails for a newly created workspace, the runtime removes that partially prepared workspace directory before returning the error.

## Prompt Rendering

The current renderer supports:

- `{{ issue.id }}`
- `{{ issue.identifier }}`
- `{{ issue.title }}`
- `{{ issue.description }}`
- `{{ issue.priority }}`
- `{{ issue.state }}`
- `{{ issue.branch_name }}`
- `{{ issue.url }}`
- `{{ issue.labels }}`
- `{{ issue.blocked_by }}`
- `{{ attempt }}`
- `{% if ... %}`, `{% else %}`, `{% endif %}`

Unknown variables and filters return `workflow_template_render_error`.

## Review Workflow Contract

- `REVIEW-WORKFLOW.md` is optional and is discovered only as a sibling of the active `WORKFLOW.md`.
- The review workflow starts from the main workflow's effective config and applies only the overrides declared in `REVIEW-WORKFLOW.md`.
- The review workflow must use `tracker.kind = linear`.
- The review workflow must use the same `github.endpoint`, `github.owner`, `github.repo`, `github.base_branch`, and `github.remote_url` as the main workflow.
- The review workflow must use the same `tracker.project_slug`, `workspace.root`, and `tracker.terminal_states` set as the main workflow.
- The review workflow must use `tracker.active_states = ["In Review"]`.
- An invalid `REVIEW-WORKFLOW.md` blocks daemon startup if the file exists.
- Review reloads still have their own file validation, but inherited main-workflow config changes also trigger a review effective-config reload. An invalid review reload blocks only review dispatch.
- Review prompts are rendered from the `REVIEW-WORKFLOW.md` body template and then extended with an internal review contract suffix.
- Review turns do not use continuation. Each review attempt runs exactly one turn.

## Review Result Contract

- A successful review turn must write `.harness/review-result.json` and `.harness/review-notes.md` inside the issue workspace.
- Before a review turn starts, the runtime removes any stale `.harness/review-result.json`.
- On successful verdict parsing, the runtime deletes `.harness/review-result.json` and keeps `.harness/review-notes.md`.
- `review-result.json` must decode to:
  - `decision`: `"done"` or `"todo"`
  - `summary`: non-empty string
  - `blocking_issues`: array
- `decision="done"` requires `blocking_issues = []`.
- `decision="todo"` requires at least one blocking issue.
- Each blocking issue must include `title`, `reason`, and `file`. `line` is optional.
- Missing or invalid review artifacts fail the attempt and follow the normal retry path.

## Continuation Turns

- On startup, the runtime fetches terminal issues for the configured project and removes matching workspace directories under `workspace.root`.
- On startup, if `github.token` is absent, the runtime resolves a token with `gh auth token --hostname <host>` before starting the orchestrator.
- Before the first turn starts, the runtime moves a claimed issue to `In Progress` if it is not already there.
- When work starts, retries, hands off to review, or completes, the runtime creates or updates one persistent Linear comment whose body starts with `## Harness Progress`.
- The first turn prompt is rendered from the `WORKFLOW.md` body template.
- If the issue is still active after `turn/completed`, the runner reuses the same live Codex thread and workspace.
- Continuation turns use an internal continuation prompt instead of re-rendering the full workflow template.
- The continuation prompt includes the issue identifier, refreshed tracker state, and the next turn count.
- If the refreshed issue is still active and the current turn count has reached `agent.max_turns`, the run stops and the orchestrator transitions the issue to `In Review`.
- Before the runtime transitions an issue to `Done`, it pushes the current workspace `HEAD` to the issue branch and creates or reuses a GitHub pull request.
- GitHub PR creation requires a clean git worktree other than `.harness/*` runtime artifacts such as review files, tool caches, and scratch output.
- If GitHub PR creation fails, the attempt follows the normal retry path and the issue does not move to `Done`.
- If `.harness/review-notes.md` exists in a coding workspace, the runtime appends an internal prompt suffix telling the coding lane to read it first.
- Review turns do not transition the issue to `In Progress`; they keep the issue in `In Review` until the verdict transitions it to `Done` or `Todo`.
- When a review verdict returns `decision="done"`, the runtime creates or reuses the GitHub PR before the final `Done` transition.

## Reload Semantics

- The orchestrator checks the workflow file for changes on each poll tick.
- The config layer also reloads when `.env` in the executable directory changes.
- A valid change becomes the new active config immediately.
- A `logging.level` change updates the process log verbosity on the next successful reload.
- A `logging.capture_prompts` change applies to future run attempts after the next successful reload; already-running attempts keep their current capture mode.
- A polling interval change resets the future tick cadence immediately.
- An invalid reload keeps the last-known-good config active.
- While the latest reload error is present, new dispatches and retry dispatches are blocked.
- Reconciliation of already running issues continues while dispatch is blocked.

This is currently implemented as poll-time file change detection, not an `fsnotify` watcher.

## Execution History

- The runtime records issue-level timeline events for dispatch, workspace preparation, tracker state transitions, runner milestones, retries, and cleanup.
- `GET /api/v1/state` exposes the most recent events as `recent_activity`.
- `GET /api/v1/issues/{identifier}` returns the per-issue in-memory history buffer for the identifier when present.
- Running snapshots include `live_session.worker` so operators can distinguish `coding` from `review`.
- The runtime also appends JSONL audit records under `workspace.root/.harness-history/`.
- The persistent `## Harness Progress` Linear comment is tracker-only state; it is not projected into the HTTP status API.
- If `logging.capture_prompts = true`, the Codex runner also appends per-issue JSONL prompt transcripts under `workspace.root/.harness-prompts/`.
- Prompt transcripts record the plain rendered turn prompt plus raw stdin/stdout/stderr lines for that issue attempt.
- Prompt transcripts are not included in `GET /api/v1/state` or `GET /api/v1/issues/{identifier}`.
