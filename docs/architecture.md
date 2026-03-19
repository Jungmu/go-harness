# Architecture

## Implemented Runtime Slice

The current Go harness implements:

- `cmd/harnessd` daemon entrypoint
- workflow/config loading from `WORKFLOW.md`
- optional sibling `REVIEW-WORKFLOW.md` loading for an in-process review lane
- Linear polling, issue refresh, GitHub pull request creation, and automatic `In Progress` / `Done` state transitions
- per-issue workspace creation and lifecycle hooks
- local Codex app-server execution with same-session continuation turns
- orchestrator-owned runtime state with retry and cancellation
- JSON status API and CLI `status`

## Component Boundaries

- `internal/workflow`
  - loads `WORKFLOW.md`
  - parses front matter and prompt body
  - renders the prompt template
- `internal/config`
  - applies defaults
  - resolves env/path values
  - validates runtime config
  - validates optional review-lane config against the active main workflow
  - preserves last-known-good config across reload errors
- `internal/github`
  - normalizes GitHub web/API endpoints into API and git remote URLs
  - resolves GitHub credentials from config or `gh auth token --hostname <host>`
  - validates workspace git cleanliness before completion
  - pushes the issue branch to the configured GitHub remote
  - creates or reuses the GitHub pull request for terminal handoff
- `internal/tracker/linear`
  - polls candidate issues
  - polls terminal issues for startup cleanup
  - refreshes issues by ID
  - resolves workflow states and transitions issues to `In Progress` and `Done`
  - normalizes Linear issue payloads
- `internal/workspace`
  - derives sanitized workspace paths
  - enforces root-bound path safety
  - runs `after_create`, `before_run`, `after_run`, `before_remove`
- `internal/agent/codex`
  - launches `bash -lc <codex.command>`
  - performs `initialize -> initialized -> thread/start -> turn/start`
  - reuses the same `thread_id` for continuation turns in one run
  - streams events and usage totals
  - optionally appends raw prompt and app-server transcript JSONL files under `workspace.root/.harness-prompts/`
- `internal/orchestrator`
  - owns `claimed`, `running`, `retry`, `completed`
  - runs one-time startup cleanup for terminal workspaces
  - sorts candidate dispatches by priority, creation time, then identifier
  - transitions issues to `In Progress` before prompt execution
  - creates or reuses the GitHub pull request before any successful `Done` transition
  - transitions successful runs to `Done` unless the run explicitly stops for retry or external state change
  - runs an optional review lane that keeps issues in `In Review` until a structured verdict moves them to `Done` or `Todo`
  - reconciles terminal/non-active issues
  - releases claims when tracker refresh no longer returns a running or retrying issue
  - refreshes the issue between turns to decide continuation vs stop
  - transitions issues to `In Review` when one live run exhausts `agent.max_turns`
  - preserves `retry.last_error` when an attempt exits with a worker error
  - blocks dispatch when workflow reload is invalid
  - records issue-level timeline events and appends them to `workspace.root/.harness-history/*.jsonl`
  - projects shared status snapshots and per-issue history buffers
- `internal/server`
  - serves `/`
  - serves `/healthz`
  - serves `/api/v1/state`
  - serves `/api/v1/issues/{identifier}`
  - serves `POST /api/v1/refresh`
  - renders a human-readable dashboard from the same runtime snapshot used by the JSON API, including both workflow paths, lane-specific dispatch health, redacted environment metadata, worker labels, and recent issue activity timeline

## Current Status Surfaces

- HTTP
  - `GET /`
  - `GET /healthz`
  - `GET /api/v1/state`
  - `GET /api/v1/issues/{identifier}`
  - `POST /api/v1/refresh`
- CLI
  - `harnessd status --addr http://127.0.0.1:8080`

## Remaining Milestone 2+ Work

- broader live Linear + real Codex coverage for tracker write flows
- tracker write tools
- auth and multi-tenant hardening beyond trusted local operation
