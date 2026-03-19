# Go Harness

Go Harness is a Go implementation of a Symphony-style automation service. It polls Linear issues, prepares an isolated workspace per issue, renders a repository-owned `WORKFLOW.md`, and runs a local Codex app-server session inside that workspace.

This repository currently contains a working runtime slice for local operation:

- workflow loading and validation from `WORKFLOW.md`
- Linear read polling and issue refresh
- per-issue workspace preparation and lifecycle hooks
- local Codex app-server execution
- orchestrator-owned retry, cancellation, and runtime snapshots
- automatic Linear state transitions to `In Progress` on start and `Done` on successful completion
- issue timeline logging to `.harness-history/*.jsonl` under `workspace.root`
- HTTP status endpoints, HTML dashboard, and CLI `status`

## Current Scope

The current implementation is intended for trusted local or small-team environments.

- tracker support: Linear only
- agent runtime: local `codex app-server`
- persistence: no database
- status surface: HTTP + HTML dashboard + CLI
- tracker writes: automatic `In Progress` and `Done` transitions only

More detail lives in `PLAN.md`, `SPEC.md`, and the docs under `docs/`.

## Prerequisites

- Go `1.25`
- `codex` installed and available on `PATH`
- a Linear API key
- a `WORKFLOW.md` file for the repository you want the harness to operate against

## Build

```bash
go build -o bin/harnessd ./cmd/harnessd
```

Or use the repository `Makefile`:

```bash
make build
```

## Common Commands

```bash
make test
make fmt
make test-live-e2e
```

If a repository-root `.env` file exists, `make test` and `make test-live-e2e` load it before invoking `go test`.

## Minimal `WORKFLOW.md`

Create a `WORKFLOW.md` in the target repository, or pass its path explicitly at startup.

```md
---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
workspace:
  root: ~/symphony-workspaces
logging:
  level: info
server:
  port: 8080
---

You are working on {{ issue.identifier }}.

Title: {{ issue.title }}
Description: {{ issue.description }}
```

Notes:

- `tracker.api_key` may be a literal token or `$ENV_VAR`
- `tracker.project_slug` accepts a Linear project `slugId` or an exact project name
- `tracker.project_slug` may also be set via `$ENV_VAR`
- `workspace.root` supports `~` and environment expansion
- `logging.level` accepts `debug`, `info`, `warn`, or `error`
- if no workflow path is passed, the daemon looks for `WORKFLOW.md` in its current working directory and then next to the executable
- `server.port` must be set in `WORKFLOW.md` or overridden with `--port` if you want the HTTP API enabled
- a fuller example is available at `examples/WORKFLOW.md`

## Run

Run against an explicit workflow file:

```bash
LINEAR_API_KEY=... ./bin/harnessd --port 8080 /absolute/path/to/WORKFLOW.md
```

Or run from a repository that already has `WORKFLOW.md`:

```bash
cd /path/to/target-repo
LINEAR_API_KEY=... /path/to/go-harness/bin/harnessd --port 8080
```

Or keep `WORKFLOW.md` and `.env` next to the binary in `bin/` and start it without an explicit path:

```bash
./bin/harnessd --port 8080
```

Important:

- do not run the daemon with cwd set to this source repository unless this repository itself is the target repo
- the agent session runs inside per-issue workspaces under `workspace.root`
- set `logging.level: debug` if you want a poll heartbeat and candidate-count logs while the daemon is idle
- startup logs print the resolved workflow path, `.env` path, and all tracked environment entries with sensitive values redacted

## Status And Operations

Start the daemon:

```bash
./bin/harnessd --port 8080 /absolute/path/to/WORKFLOW.md
```

Read runtime state from the CLI:

```bash
./bin/harnessd status --addr http://127.0.0.1:8080
```

HTTP endpoints:

- `GET /`
- `GET /healthz`
- `GET /api/v1/state`
- `GET /api/v1/issues/{identifier}`
- `POST /api/v1/refresh`

The root dashboard auto-refreshes and shows the active workflow file path, `.env` path and tracked env entries, running issues, retry queue, completed identifiers, token totals, and whether dispatch is currently blocked by an invalid workflow reload. Sensitive env values such as API keys are redacted in the dashboard and JSON state.

Execution history:

- the harness records issue-level timeline events in memory and exposes them as `recent_activity` in `GET /api/v1/state`
- the dashboard renders the same timeline in the `Recent Activity` panel
- the harness also appends per-issue JSONL history files under `workspace.root/.harness-history/`
- cleanup removes the workspace directory, but not the `.harness-history` audit trail

Issue state transitions:

- when a dispatch starts, the harness moves the issue to `In Progress`
- if the run exits with `max_turns_reached`, the harness moves the issue to `In Review`
- when a run completes successfully without an explicit retry stop reason, the harness moves the issue to `Done`
- if the run exits with cancellation or another retry path, the issue is left in its current state

## Live E2E

The repository includes an opt-in live integration test that exercises:

- real Linear polling against a dedicated test project/state
- real `codex app-server` execution in an isolated workspace
- handoff to `In Review` after one turn via `agent.max_turns = 1`

The test is skipped by default. It creates a temporary Linear issue, waits for the harness to create a deterministic marker file, and verifies that the harness hands the issue off to `In Review`.

Required environment:

- `GO_HARNESS_LIVE_E2E=1`
- `LINEAR_API_KEY`
- `GO_HARNESS_LIVE_LINEAR_TEAM_ID`
- `GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG`

Reference rules:

- `GO_HARNESS_LIVE_LINEAR_TEAM_ID`
  - accepts a team UUID, team key, or exact team name
- `GO_HARNESS_LIVE_LINEAR_PROJECT_SLUG`
  - accepts a project `slugId` or an exact project name
- `GO_HARNESS_LIVE_LINEAR_HANDOFF_STATE_NAME`
  - optional; defaults to `In Review`

Optional environment:

- `GO_HARNESS_LIVE_CODEX_COMMAND`
  - defaults to `codex app-server`
- `GO_HARNESS_LIVE_LINEAR_ACTIVE_STATE_NAME`
  - if unset, the test picks the lowest-position team workflow state of type `started`, then falls back to `unstarted`
- `GO_HARNESS_LIVE_LINEAR_TERMINAL_STATE_NAME`
  - if unset, the test picks the lowest-position team workflow state of type `completed`, then falls back to `canceled`

Recommended setup:

- use a dedicated Linear test project
- use a dedicated active state override if the team has multiple `started` states and you need a specific one
- use a terminal state that is safe for disposable test issues

Run it with:

```bash
GO_HARNESS_LIVE_E2E=1 go test ./cmd/harnessd -run TestLiveLinearCodexRetryAndCleanup -v
```

## Workflow Contract

The currently implemented workflow contract is documented in `docs/workflow-contract.md`.

Implemented front matter sections:

- `tracker`
- `polling`
- `workspace`
- `hooks`
- `agent`
- `codex`
- `logging`
- `server`

Supported prompt variables and reload behavior are also documented there.

## Repository Guide

- `SPEC.md`: language-agnostic Symphony service specification
- `PLAN.md`: Go v1 implementation plan and scope
- `docs/architecture.md`: implemented component boundaries and status surfaces
- `docs/workflow-contract.md`: exact `WORKFLOW.md` behavior and defaults
- `examples/WORKFLOW.md`: a fuller starter workflow for local Linear + Codex setups

## Limitations

- no persistent scheduler state across restarts
- no tracker write tools beyond automatic `In Progress` and `Done` transitions
- no remote worker support
- no auth or multi-tenant control plane
- live Linear + real Codex coverage is opt-in only
