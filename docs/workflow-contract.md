# Workflow Contract

## Scope

This document describes the currently implemented `WORKFLOW.md` contract in the Go harness.

## File Resolution

The runtime resolves the workflow file in this order:

1. explicit CLI path
2. `WORKFLOW.md` in the current process working directory

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

## Value Resolution

- `tracker.api_key` accepts a literal value or `$VAR_NAME`.
- `$VAR_NAME` resolves from the process environment first, then from `.env` in the executable directory.
- `workspace.root` supports `~` expansion and `$VAR` expansion using the same environment lookup order.
- The shell command in `codex.command` is passed directly to `bash -lc`.

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

- The first turn prompt is rendered from the `WORKFLOW.md` body template.
- If the issue is still active after `turn/completed`, the runner reuses the same live Codex thread and workspace.
- Continuation turns use an internal continuation prompt instead of re-rendering the full workflow template.
- The continuation prompt includes the issue identifier, refreshed tracker state, and the next turn count.
- If the refreshed issue is still active and the current turn count has reached `agent.max_turns`, the run stops and the orchestrator schedules a retry with reason `max_turns_reached`.

## Reload Semantics

- The orchestrator checks the workflow file for changes on each poll tick.
- The config layer also reloads when `.env` in the executable directory changes.
- A valid change becomes the new active config immediately.
- A polling interval change resets the future tick cadence immediately.
- An invalid reload keeps the last-known-good config active.
- While the latest reload error is present, new dispatches and retry dispatches are blocked.
- Reconciliation of already running issues continues while dispatch is blocked.

This is currently implemented as poll-time file change detection, not an `fsnotify` watcher.
