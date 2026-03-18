---
name: orchestrator-change
description: Use when a task changes orchestrator or runner behavior, including claim or running or retry state transitions, continuation turns, cancellation, reconciliation, retry scheduling, runtime totals, recent events, or status projections derived from runtime state. Do not use for isolated tracker-client or pure HTTP handler work that leaves the state machine unchanged.
---

# Orchestrator Change

Use this skill when the change touches scheduler semantics, worker lifecycle, or any state projected from the orchestrator.

## Focus

- `claimed`, `running`, `retry`, and `completed` state transitions
- continuation turns and `thread_id` reuse
- cancellation on external issue state changes
- retry scheduling and backoff reasons
- recent event buffering, token totals, rate-limit snapshots
- status snapshots used by CLI and HTTP surfaces

## Required Inputs

Read these first. Resolve repository documents from the working repository, not relative to this skill folder:

- the `PLAN.md` file at the current repository root
- the `SPEC.md` file at the parent workspace root
- the orchestrator package
- the runner package
- status projection code and tests when touched

## Invariants

- Only the orchestrator mutates scheduler state.
- A successful turn does not imply the issue is done.
- Continuation turns reuse the same live app-server process and `thread_id`.
- Terminal issue transition cancels the run and performs cleanup.
- Non-active, non-terminal issue transition cancels the run and releases the claim.
- Restart does not recover in-flight sessions or retry timers.
- CLI status and HTTP status reuse the same snapshot source.

## Update Checklist

- Identify the affected state transitions before editing code.
- Update domain types, orchestrator logic, snapshot projection, and logs together when state shape changes.
- Keep retry reasons explicit and operator-visible.
- If a change affects public behavior, update `PLAN.md` and operator-facing docs in the same change.

## Minimum Verification

- Claim to running to release transition
- Running to retry transition after worker failure
- Continuation turn reuse on active issues
- `max_turns` reached path
- Cancellation on external terminal transition
- Cancellation on external non-active transition
- Snapshot projection of recent events or totals when touched

## Do Not

- Let runner code directly decide scheduler ownership.
- Reintroduce hidden mutable global state for orchestration.
- Duplicate status logic between CLI and HTTP handlers.
