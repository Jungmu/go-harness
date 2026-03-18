# Architecture

## Implemented Runtime Slice

The current Go harness implements:

- `cmd/harnessd` daemon entrypoint
- workflow/config loading from `WORKFLOW.md`
- Linear read-only polling and issue refresh
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
  - preserves last-known-good config across reload errors
- `internal/tracker/linear`
  - polls candidate issues
  - refreshes issues by ID
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
- `internal/orchestrator`
  - owns `claimed`, `running`, `retry`, `completed`
  - reconciles terminal/non-active issues
  - refreshes the issue between turns to decide continuation vs stop
  - schedules `max_turns_reached` retries when one live run exhausts `agent.max_turns`
  - preserves `retry.last_error` when an attempt exits with a worker error
  - blocks dispatch when workflow reload is invalid
  - projects status snapshots
- `internal/server`
  - serves `/healthz`
  - serves `/api/v1/state`
  - serves `/api/v1/issues/{identifier}`
  - serves `POST /api/v1/refresh`

## Current Status Surfaces

- HTTP
  - `GET /healthz`
  - `GET /api/v1/state`
  - `GET /api/v1/issues/{identifier}`
  - `POST /api/v1/refresh`
- CLI
  - `harnessd status --addr http://127.0.0.1:8080`

## Remaining Milestone 2+ Work

- stronger issue detail history outside the running state
- broader live Linear + real Codex coverage for tracker write flows
- tracker write tools
- optional human-readable dashboard
