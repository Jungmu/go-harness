---
name: workflow-contract
description: Use when a task changes `WORKFLOW.md`, workflow parsing, config defaults, value resolution, startup or per-tick validation, typed workflow errors, prompt rendering semantics, or workflow reload behavior. Do not use for general Go changes that do not affect the workflow or config contract.
---

# Workflow Contract

Use this skill when the change affects the repository-owned workflow contract rather than a purely internal implementation detail.

## Focus

- `WORKFLOW.md` discovery and parsing
- YAML front matter schema and defaults
- `$VAR` and path resolution rules
- strict prompt rendering behavior
- startup validation and per-tick dispatch validation
- reload semantics and last-known-good behavior
- documented typed workflow or config errors

## Required Inputs

Read these first. Resolve them from the working repository, not relative to this skill folder:

- the `PLAN.md` file at the current repository root
- the `SPEC.md` file at the current repository root
- the `WORKFLOW.md` file at the current repository root, when it exists
- the `docs/workflow-contract.md` file under the current repository root, when it exists

## Invariants

- Path precedence stays `explicit workflow path` first, then `./WORKFLOW.md`.
- Workflow read or parse failure blocks startup and must not silently fall back.
- Prompt body may fall back only when the body is blank, not when parsing failed.
- Unknown template variables or filters are render errors.
- Startup validation and per-tick dispatch validation stay separate behaviors.
- Invalid reload keeps the last-known-good effective config.
- Invalid reload blocks new dispatches but does not kill the service.
- Codex config values such as approval and sandbox stay pass-through values unless there is a deliberate contract change.

## Update Checklist

- Update implementation and tests together.
- Update examples and defaults anywhere they are documented.
- Keep `WORKFLOW.md`, `PLAN.md`, and `docs/workflow-contract.md` aligned when public behavior changes.
- If a field is added, specify default, validation rule, reload behavior, and error behavior.
- If a field is removed or narrowed, document the migration impact in the same change.

## Minimum Verification

- Missing workflow file
- Invalid YAML
- Front matter decodes to a non-map
- Default values applied correctly
- Strict template parse or render failure
- Valid reload applies to future dispatch
- Invalid reload keeps last-known-good and blocks dispatch

## Do Not

- Add ad-hoc env reads outside the workflow or config layer.
- Change workflow behavior in code without updating the contract docs.
- Blur startup failure behavior and runtime dispatch-skipping behavior.
