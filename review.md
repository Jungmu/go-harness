# Code Review Report: go-harness

**Date:** 2026-03-20
**Module:** `go-harness`
**Go Version:** 1.25.0
**Scope:** Full codebase review

---

## Overview

`go-harness` is a daemon that automates software issue resolution by orchestrating AI agents (Claude Code / Codex) against a Linear issue tracker and GitHub. It manages workspaces, runs coding agents, and optionally runs a second review agent to validate the output before merging.

---

## Architecture Summary

| Layer | Package | Responsibility |
|---|---|---|
| Entry point | `cmd/harnessd` | CLI, wiring, signal handling |
| Orchestration | `internal/orchestrator` | Issue lifecycle, dispatch, retry, timeline |
| Agent protocol | `internal/agent/codex` | JSON-RPC session with the AI app-server |
| Workspace | `internal/workspace` | Directory creation, hook execution |
| Config | `internal/config` | YAML workflow loading, hot-reload |
| Tracker | `internal/tracker/linear` | Linear GraphQL API |
| GitHub | `internal/github` | PR creation/update via GitHub REST API |
| Server | `internal/server` | HTTP status API |
| Domain | `internal/domain` | Shared types and helpers |

---

## Findings

### 1. Duplicated ID check in `awaitResponse` (Minor Bug)

**File:** `internal/agent/codex/runner.go:420-427`

```go
if intField(payload, "id") == id {
    if rpcErr := rpcError(payload); rpcErr != nil {
        return nil, rpcErr
    }
}
if intField(payload, "id") == id {
    return mapField(payload, "result"), nil
}
```

The `id` field is looked up twice in consecutive blocks. The intent is to first check for an RPC error and then return the result, but both blocks guard on the same condition. If the first block's error check is satisfied and returns `nil, rpcErr`, the second block is unreachable for that case. For the success path, both checks are redundant. This can be simplified into a single `if` block with an `else` clause, which would make the logic clearer and avoid the double map lookup.

**Severity:** Low — functionally correct, but confusing.

---

### 2. Force-push on every PR sync

**File:** `internal/github/client.go:146`

```go
args = append(args, "push", "--force", remoteURL, "HEAD:refs/heads/"+headBranch)
```

`git push --force` is used unconditionally on every PR handoff. For the automated single-agent workflow this is acceptable (each workspace owns its branch), but it is worth documenting the assumption explicitly: the harness must be the sole writer to the issue branch. If two concurrent agents targeted the same branch name, one would silently overwrite the other's commits.

**Severity:** Low — acceptable given the dispatch model, but should be noted in documentation.

---

### 3. Token exposure via git config header

**File:** `internal/github/client.go:143`

```go
authHeader := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.cfg.Token))
args = append(args, "-c", "http.extraheader="+authHeader)
```

The GitHub token is injected as a `-c` flag to `git`. On most systems the process command line is visible to other processes owned by the same user (e.g., via `ps`). Prefer using `GIT_ASKPASS` or a credential helper, or at minimum pass the header via `git -c` only in environments where process visibility is controlled.

**Severity:** Medium — real risk in shared-user or containerized environments where `ps` output is exposed.

---

### 4. `truncate` helper duplicated

**File:** `internal/workspace/manager.go:226`, `internal/agent/codex/runner.go:782`

Both `workspace` and `codex` packages define an identical `truncate(value string, limit int) string` function. The same logic also exists under the name `truncateLogValue` in `internal/orchestrator/orchestrator.go:1920`. This is a minor duplication that could be extracted to a small `internal/stringutil` package, but since all three are package-private it does not cause any correctness issue.

**Severity:** Low — code duplication only.

---

### 5. Unbounded `eventCh` backpressure

**File:** `internal/orchestrator/orchestrator.go:196`

```go
eventCh: make(chan any, 128),
```

The event channel has a fixed buffer of 128. Worker goroutines call `pushEvent`, which blocks when the channel is full (it selects between `eventCh` and `doneCh`). Under high concurrency — many parallel agents emitting frequent events — a slow main loop iteration could stall all workers. The buffer size of 128 is not configurable.

**Severity:** Low — unlikely in practice given the default `max_concurrent_agents` limits, but worth monitoring at scale.

---

### 6. Stale worktree cleanup relies on process exit only

**File:** `internal/agent/codex/runner.go:834-843`

```go
func (s *appSession) close() {
    if s.stdinCloser != nil {
        _ = s.stdinCloser.Close()
    }
    s.cancel()
    select {
    case <-s.waitCh:
    case <-time.After(2 * time.Second):
    }
}
```

When the session is closed, the code waits up to 2 seconds for the child process to exit. If the AI app-server hangs (e.g., waiting for network IO), the harness proceeds anyway. The orphaned process continues to run and may hold the workspace lock or consume resources. Adding a `SIGKILL` fallback after the timeout would be more robust.

**Severity:** Medium — can cause resource leaks on a long-running daemon.

---

### 7. Review verdict file deleted before confirmation

**File:** `internal/orchestrator/review.go:65-67`

```go
if err := os.Remove(path); err != nil {
    return reviewVerdict{}, err
}
return verdict, nil
```

`review-result.json` is removed immediately after it is read, before the caller has had a chance to act on the verdict (e.g., transition the tracker state or open a PR). If the process crashes after the file is deleted but before the state transition completes, the verdict is lost and the issue will be re-reviewed from scratch on the next attempt. A safer pattern is to rename/archive the file rather than delete it, or delete it only after the downstream operations succeed.

**Severity:** Medium — potential data loss on crash between verdict read and tracker update.

---

### 8. No retry on GitHub PR creation network errors

**File:** `internal/github/client.go:180-204`

`createPullRequest` makes a single HTTP attempt with no retry. Transient network errors or GitHub API rate-limit responses (HTTP 429 / 403 with `Retry-After`) will cause the entire issue completion to fail. The orchestrator will retry the whole agent run, which is wasteful. Retrying only the PR creation step with exponential backoff would be more efficient.

**Severity:** Low — the orchestrator-level retry handles it, but at high cost.

---

## Positive Observations

- **Clean domain separation.** The `domain` package is free of I/O; all side effects live in concrete packages wired at the top level. This makes testing straightforward.
- **Atomic snapshot publishing.** `orchestrator.go` uses `atomic.Value` for `snapshot` and `history`, which allows lock-free reads from the HTTP status handler concurrently with the main loop writing.
- **Path traversal protection.** `workspace/manager.go:ensureSafePath` verifies that a derived path cannot escape the configured workspace root. Symlink detection via `Lstat` is present.
- **Configurable hot-reload.** `config/store.go` watches both the workflow file and the `.env` file for changes without requiring a daemon restart.
- **Structured logging throughout.** All log calls use `slog` with typed attributes, which is idiomatic for Go 1.21+ and enables downstream log aggregation.
- **Graceful shutdown.** Signal handling with a 10-second timeout for the HTTP server and orchestrator stop is correct and avoids abrupt termination.

---

## Summary

| Finding | Severity | File |
|---|---|---|
| Duplicate `id` check in `awaitResponse` | Low | `internal/agent/codex/runner.go` |
| Force-push without concurrency guard | Low | `internal/github/client.go` |
| Token in process args via `-c http.extraheader` | Medium | `internal/github/client.go` |
| `truncate` helper duplicated across packages | Low | Multiple |
| Fixed-size event channel | Low | `internal/orchestrator/orchestrator.go` |
| No SIGKILL fallback for hung app-server | Medium | `internal/agent/codex/runner.go` |
| Review verdict file deleted before downstream confirmation | Medium | `internal/orchestrator/review.go` |
| No retry on GitHub PR HTTP errors | Low | `internal/github/client.go` |

Overall the codebase is well-structured, readable, and follows modern Go idioms. The three medium-severity findings (token exposure, orphaned processes, and lost verdict on crash) are worth addressing before running the harness in a production or multi-user environment.
