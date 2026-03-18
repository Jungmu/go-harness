# Go Harness

This repository is a Go implementation plan for a Symphony-style harness. Follow `../SPEC.md` first, then `PLAN.md`. If code changes alter the intended contract, update the relevant docs in the same change.

## Working Agreements

- Read `PLAN.md` and the files you will touch before editing code.
- Keep changes narrowly scoped. Do not mix unrelated refactors into the same change.
- Prefer small, explicit functions and standard library building blocks over framework-heavy abstractions.
- When a task matches a repo-local skill in `.agents/skills/`, use that skill before inventing a new workflow.
- Add new project-specific rules here. Use `AGENTS.override.md` only in subdirectories that genuinely need different rules.

## Architecture Invariants

- Treat the orchestrator as the only component allowed to mutate scheduler state such as `claimed`, `running`, `retry`, and `completed`.
- Load runtime behavior from `WORKFLOW.md` through the workflow/config packages. Do not add ad-hoc environment reads in runtime code.
- Never run a Codex turn with cwd set to the source repository. Agent execution must happen inside the per-issue workspace.
- Keep every workspace path under the configured workspace root and derive directory names from the sanitized `WorkspaceKey`.
- Keep tracker reads in the tracker adapter or client layer. Keep tracker writes behind injected high-level tools.
- Preserve v1 restart semantics: do not attempt to recover in-flight sessions or retry timers after process restart.
- Reuse one snapshot source for CLI status output and HTTP status endpoints.
- Include issue and session context in structured logs and status DTOs whenever that context exists.

## Testing And Validation

- Once the repository has a `go.mod`, run focused `go test` commands for touched packages while iterating.
- Before handoff, if the repository has a `go.mod`, run `go test ./...`; otherwise run the narrowest available verification for the changed docs or config work and state that the Go test gate is not available yet.
- When changing workflow or config behavior, add tests for defaults, validation, reload behavior, and typed error cases.
- When changing orchestrator or runner behavior, add tests for retry scheduling, cancellation, continuation turns, and status projection.
- When changing operator-visible behavior, update docs in the same change.

## Docs Update Policy

- Update `PLAN.md` when architecture, invariants, or public behavior change.
- Update `WORKFLOW.md` and `docs/workflow-contract.md` when workflow or config semantics change.
- Update `docs/architecture.md` when component boundaries or data flow change.

## Go Style

- Prefer table-driven tests for state-machine and config-layer coverage.
- Keep package APIs small. Export only types or functions that need to cross package boundaries.
- Avoid hidden global state. Pass dependencies explicitly.
- Keep JSON and YAML wire shapes stable once documented.
