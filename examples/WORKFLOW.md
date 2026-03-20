---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-linear-project
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
    - Closed
    - Cancelled
    - Canceled
    - Duplicate

github:
  # Accepts GitHub web or API URLs, for example https://github.com/ or https://github.krafton.com/
  endpoint: https://github.com/
  # Optional when `gh auth login --hostname github.com` is already configured.
  token: $GITHUB_TOKEN
  owner: your-org
  repo: your-repo
  base_branch: main

polling:
  interval_ms: 30000

workspace:
  root: $HOME/symphony-workspaces

hooks:
  after_create: |
    test -n "$HARNESS_SOURCE_REPO" || { echo "HARNESS_SOURCE_REPO is required"; exit 1; }
    git -C "$HARNESS_SOURCE_REPO" worktree prune
    git -C "$HARNESS_SOURCE_REPO" worktree add --force --detach "$PWD" main
  before_run: git fetch --all --prune
  after_run: git status --short
  before_remove: |
    test -n "$HARNESS_SOURCE_REPO" || exit 0
    git -C "$HARNESS_SOURCE_REPO" worktree remove --force "$PWD"
    git -C "$HARNESS_SOURCE_REPO" worktree prune
  timeout_ms: 60000

agent:
  provider: codex
  max_concurrent_agents: 4
  max_turns: 20
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    In Progress: 2

codex:
  command: |
    export GOTOOLCHAIN=local
    export GOCACHE="$PWD/.harness/cache/go-build"
    export GOMODCACHE="$PWD/.harness/cache/go-mod"
    export GOTMPDIR="$PWD/.harness/cache/go-tmp"
    mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"
    exec codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000

claude:
  command: claude
  permission_mode: bypassPermissions
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000

logging:
  level: info
  capture_prompts: false

server:
  port: 8080
---

You are operating inside an isolated issue workspace for {{ issue.identifier }}.

Issue title: {{ issue.title }}
Issue state: {{ issue.state }}
Issue URL: {{ issue.url }}
Attempt: {{ attempt }}

{% if issue.description %}
Issue description:
{{ issue.description }}
{% endif %}

{% if issue.branch_name %}
Suggested branch name: {{ issue.branch_name }}
{% endif %}

Labels: {{ issue.labels }}
Blocked by: {{ issue.blocked_by }}

Requirements:

- Work only inside the provided workspace.
- If `.harness/review-notes.md` exists, read it first and resolve every blocking issue it lists.
- Use the issue context to make the smallest correct change.
- Run focused verification for the files you touched.
- Keep runtime artifacts, caches, scratch files, and transcripts under `.harness/` only. Do not commit them.
- Commit the final changes on the issue branch before ending the run.
- Leave the workspace in a clean git state so the harness can open the GitHub pull request when it moves the issue to `Done`.
