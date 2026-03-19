# Workflow Contract

## Scope

This document describes the currently implemented `WORKFLOW.md` contract in the Go harness.

## File Resolution

The runtime resolves the workflow file in this order:

1. explicit CLI path
2. `WORKFLOW.md` in the current process working directory
3. `WORKFLOW.md` in the executable directory

If the file cannot be read, the loader returns `missing_workflow_file`.

## Parsing Rules

- `WORKFLOW.md` may start with YAML front matter fenced by `---`.
- Front matter must decode to a map.
- Non-map front matter returns `workflow_front_matter_not_a_map`.
- Invalid YAML returns `invalid_workflow_yaml`.
- The Markdown body is the prompt template after trimming outer whitespace.

## Supported Front Matter Keys

- `tracker`
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

## Value Resolution

- `tracker.api_key` accepts a literal value or `$VAR_NAME`.
- `tracker.project_slug` accepts a Linear project `slugId` or an exact project name.
- `$VAR_NAME` resolves from the process environment first, then from `.env` in the executable directory.
- `workspace.root` supports `~` expansion and `$VAR` expansion using the same environment lookup order.
- `logging.level` accepts `debug`, `info`, `warn`, or `error`.
- The shell command in `codex.command` is passed directly to `bash -lc`.
- If no workflow path is passed and the current working directory has no `WORKFLOW.md`, the loader falls back to the executable directory before returning `missing_workflow_file`.

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

## Continuation Turns

- Before the first turn starts, the runtime moves a claimed issue to `In Progress` if it is not already there.
- The first turn prompt is rendered from the `WORKFLOW.md` body template.
- If the issue is still active after `turn/completed`, the runner reuses the same live Codex thread and workspace.
- Continuation turns use an internal continuation prompt instead of re-rendering the full workflow template.
- The continuation prompt includes the issue identifier, refreshed tracker state, and the next turn count.
- If the refreshed issue is still active and the current turn count has reached `agent.max_turns`, the run stops and the orchestrator transitions the issue to `In Review`.
- If a run exits successfully without an explicit retry stop reason and the issue is still in an active state, the runtime transitions the issue to `Done`.

## Reload Semantics

- The orchestrator checks the workflow file for changes on each poll tick.
- The config layer also reloads when `.env` in the executable directory changes.
- A valid change becomes the new active config immediately.
- A `logging.level` change updates the process log verbosity on the next successful reload.
- A polling interval change resets the future tick cadence immediately.
- An invalid reload keeps the last-known-good config active.
- While the latest reload error is present, new dispatches and retry dispatches are blocked.
- Reconciliation of already running issues continues while dispatch is blocked.

This is currently implemented as poll-time file change detection, not an `fsnotify` watcher.

## Execution History

- The runtime records issue-level timeline events for dispatch, workspace preparation, tracker state transitions, runner milestones, retries, and cleanup.
- `GET /api/v1/state` exposes the most recent events as `recent_activity`.
- `GET /api/v1/issues/{identifier}` includes issue history when recent events are available.
- The runtime also appends JSONL audit records under `workspace.root/.harness-history/`.
