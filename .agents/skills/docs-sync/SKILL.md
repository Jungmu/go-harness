---
name: docs-sync
description: Use when code or plan changes affect documented behavior, config semantics, architecture, workflow examples, operator commands, or repository rules and the docs must be updated consistently in the same change. Do not use for typo-only edits or internal refactors with no user-visible or contract-visible effect.
---

# Docs Sync

Use this skill when a change should update repository docs in the same patch instead of leaving follow-up documentation debt.

## Focus

- architecture or component-boundary changes
- workflow or config contract changes
- default values, validation rules, or reload semantics
- operator-visible commands and status surfaces
- repository-wide development rules

## Document Map

Pick the smallest set that matches the change. Treat these as files or directories under the current repository root, not paths relative to this skill folder:

- `PLAN.md` for architecture, invariants, scope, and implementation phases
- `WORKFLOW.md` for workflow examples and runtime contract examples
- `docs/workflow-contract.md` for exact workflow or config semantics
- `docs/architecture.md` for component boundaries and data flow
- `AGENTS.md` for repository-wide development rules

## Rules

- Update docs in the same change as the behavior change.
- Keep examples aligned with real defaults and endpoint names.
- Use the same path names, state names, and field names the code uses.
- Prefer short, imperative wording over broad prose.
- Remove contradictions instead of adding caveats next to stale text.

## Verification

- Re-read all changed docs after editing.
- Check for mismatched defaults, path names, endpoint names, and state names.
- Check that examples still match the current contract.
- If docs reference commands, make sure those commands exist or are planned in `PLAN.md`.

## Do Not

- Leave a public contract change documented in only one file when multiple docs cover it.
- Add aspirational behavior to docs unless it is clearly marked as future work.
- Copy long implementation details into multiple files when one source of truth is enough.
