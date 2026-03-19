# Go Harness

Go Harness is a Go implementation of a Symphony-style automation service. It polls Linear issues, prepares an isolated workspace per issue, renders a repository-owned `WORKFLOW.md`, and runs a local Codex app-server session inside that workspace. If a sibling `REVIEW-WORKFLOW.md` exists, the same daemon also runs a review lane for `In Review` issues.

This repository currently contains a working runtime slice for local operation:

- workflow loading and validation from `WORKFLOW.md`
- Linear read polling and issue refresh
- per-issue workspace preparation and lifecycle hooks
- local Codex app-server execution
- orchestrator-owned retry, cancellation, and runtime snapshots
- automatic Linear state transitions to `In Progress` on start and `Done` on successful completion
- optional in-process review lane driven by `REVIEW-WORKFLOW.md`
- issue timeline logging to `.harness-history/*.jsonl` under `workspace.root`
- optional raw prompt transcript logging to `.harness-prompts/*.jsonl` under `workspace.root`
- HTTP status endpoints, HTML dashboard, and CLI `status`
- startup terminal-workspace cleanup and deterministic dispatch ordering

## Current Scope

The current implementation is intended for trusted local or small-team environments.

- tracker support: Linear only
- agent runtime: local `codex app-server`
- persistence: no database
- status surface: HTTP + HTML dashboard + CLI
- tracker writes: automatic state transitions plus a persistent Linear progress comment

More detail lives in `PLAN.md`, `SPEC.md`, and the docs under `docs/`.

## Install

Primary distribution is prebuilt release binaries. Go is not required to run the harness.

Supported release targets:

- macOS Apple Silicon: `darwin-arm64`
- Linux x86_64: `linux-amd64`
- Linux ARM64: `linux-arm64`

Windows is not a supported target. Use WSL if you need to run the harness from a Windows machine.

Example install on macOS Apple Silicon:

```bash
curl -L https://github.com/Jungmu/go-harness/releases/latest/download/harnessd_darwin_arm64.tar.gz -o harnessd.tar.gz
tar -xzf harnessd.tar.gz
chmod +x harnessd
sudo mv harnessd /usr/local/bin/harnessd
```

Example install on Ubuntu:

```bash
curl -L https://github.com/Jungmu/go-harness/releases/latest/download/harnessd_linux_amd64.tar.gz -o harnessd.tar.gz
tar -xzf harnessd.tar.gz
chmod +x harnessd
sudo mv harnessd /usr/local/bin/harnessd
```

Example install on Linux ARM64:

```bash
curl -L https://github.com/Jungmu/go-harness/releases/latest/download/harnessd_linux_arm64.tar.gz -o harnessd.tar.gz
tar -xzf harnessd.tar.gz
chmod +x harnessd
sudo mv harnessd /usr/local/bin/harnessd
```

Each release also publishes a matching `.sha256` file for checksum verification.

## Runtime Prerequisites

- `codex` installed and available on `PATH`
- a Linear API key
- a `WORKFLOW.md` file for the repository you want the harness to operate against
- an optional sibling `REVIEW-WORKFLOW.md` if you want the daemon to process `In Review` issues

## Build From Source

Building from source is mainly for contributors.

```bash
go build -o bin/harnessd ./cmd/harnessd
```

Or use the repository `Makefile`:

```bash
make build
```

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
  capture_prompts: false
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
- for a real repository, prefer `git worktree add`/`git worktree remove` in hooks instead of cloning into each issue workspace
- workspace hooks automatically receive `HARNESS_SOURCE_REPO`, `HARNESS_WORKFLOW_PATH`, and `HARNESS_WORKFLOW_DIR`; the examples use `HARNESS_SOURCE_REPO` for `git worktree`
- `logging.level` accepts `debug`, `info`, `warn`, or `error`
- `logging.capture_prompts` accepts `true` or `false`; set it to `true` only in development if you want raw prompt transcripts on disk
- if no workflow path is passed, the daemon looks for `WORKFLOW.md` in its current working directory and then next to the executable
- `server.port` must be set in `WORKFLOW.md` or overridden with `--port` if you want the HTTP API enabled
- a fuller example is available at `examples/WORKFLOW.md`
- an optional review-lane example is available at `examples/REVIEW-WORKFLOW.md`

## Run

Run against an explicit workflow file:

```bash
LINEAR_API_KEY=... harnessd --port 8080 /absolute/path/to/WORKFLOW.md
```

Or run from a repository that already has `WORKFLOW.md`:

```bash
cd /path/to/target-repo
LINEAR_API_KEY=... harnessd --port 8080
```

Or keep `WORKFLOW.md` and `.env` next to a copied `harnessd` binary and start it without an explicit path:

```bash
harnessd --port 8080
```

Important:

- do not run the daemon with cwd set to this source repository unless this repository itself is the target repo
- the agent session runs inside per-issue workspaces under `workspace.root`
- if `REVIEW-WORKFLOW.md` exists next to the active `WORKFLOW.md`, the daemon starts a second in-process review orchestrator
- set `logging.level: debug` if you want a poll heartbeat and candidate-count logs while the daemon is idle
- set `logging.capture_prompts: true` only when you need raw Codex exchange logs on disk for debugging
- startup logs print the resolved workflow path, `.env` path, and all tracked environment entries with sensitive values redacted

## Status And Operations

Start the daemon:

```bash
harnessd --port 8080 /absolute/path/to/WORKFLOW.md
```

Read runtime state from the CLI:

```bash
harnessd status --addr http://127.0.0.1:8080
```

HTTP endpoints:

- `GET /`
- `GET /healthz`
- `GET /api/v1/state`
- `GET /api/v1/issues/{identifier}`
- `POST /api/v1/refresh`

The root dashboard auto-refreshes and shows the active workflow file paths, `.env` path and tracked env entries, running issues, retry queue, completed identifiers, worker labels, token totals, and whether dispatch is currently blocked by an invalid workflow reload. Sensitive env values such as API keys are redacted in the dashboard and JSON state.

Execution history:

- the harness records issue-level timeline events in memory and exposes them as `recent_activity` in `GET /api/v1/state`
- the dashboard renders the same timeline in the `Recent Activity` panel
- `GET /api/v1/issues/{identifier}` returns the per-issue in-memory history buffer, not just the global recent-activity window
- the harness also appends per-issue JSONL history files under `workspace.root/.harness-history/`
- if `logging.capture_prompts` is enabled, the runner also appends raw prompt/stdin/stdout/stderr transcript files under `workspace.root/.harness-prompts/`
- raw prompt transcripts are file-only; they are not exposed through the HTTP status API or dashboard
- cleanup removes the workspace directory, but not the `.harness-history` or `.harness-prompts` audit trail

Review lane:

- if `REVIEW-WORKFLOW.md` exists next to the active `WORKFLOW.md`, the daemon starts a review lane that polls `In Review` issues
- the review lane runs one Codex turn per attempt and expects `.harness/review-result.json` plus `.harness/review-notes.md` in the issue workspace
- a review verdict with `decision="done"` transitions the issue to `Done`
- a review verdict with `decision="todo"` transitions the issue back to `Todo` and preserves the workspace
- invalid or missing review artifacts fail the attempt and use the normal retry policy

Dispatch order:

- candidate issues are dispatched in deterministic order: `priority` ascending, then `created_at` ascending, then `identifier`
- issues missing from tracker refresh are released from running or retry state instead of keeping a stuck claim

Issue state transitions:

- when a dispatch starts, the harness moves the issue to `In Progress`
- if the run exits with `max_turns_reached`, the harness moves the issue to `In Review`
- when a run completes successfully without an explicit retry stop reason, the harness moves the issue to `Done`
- if the run exits with cancellation or another retry path, the issue is left in its current state
- when a review attempt returns `decision="todo"`, the harness moves the issue back to `Todo` without cleaning the workspace

Startup cleanup:

- on startup, the harness fetches terminal issues for the configured project and removes matching workspaces under `workspace.root`
- startup cleanup keeps `.harness-history` and `.harness-prompts` files and only removes per-issue workspace directories
- cleanup fetch failures are logged as warnings and do not block daemon startup

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
- `examples/REVIEW-WORKFLOW.md`: a fuller starter review workflow for `In Review` issues

## Development

Contributor prerequisites:

- Go `1.25`

Common commands:

```bash
make build
make test
make fmt
make test-live-e2e
```

If a repository-root `.env` file exists, `make test` and `make test-live-e2e` load it before invoking `go test`.

## Limitations

- no persistent scheduler state across restarts
- no tracker write tools beyond automatic state transitions and the persistent harness progress comment
- no remote worker support
- no auth or multi-tenant control plane
- live Linear + real Codex coverage is opt-in only
