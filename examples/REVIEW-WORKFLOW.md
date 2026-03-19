---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-linear-project
  active_states:
    - In Review
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
  after_create: git clone git@github.com:your-org/your-repo.git .
  before_run: git fetch --all --prune
  after_run: git status --short
  timeout_ms: 60000

agent:
  max_concurrent_agents: 2
  max_turns: 1
  max_retry_backoff_ms: 300000

codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000

logging:
  level: info
  capture_prompts: false
---

You are reviewing the current workspace state for {{ issue.identifier }}.

Issue title: {{ issue.title }}
Issue state: {{ issue.state }}
Issue URL: {{ issue.url }}
Attempt: {{ attempt }}

{% if issue.description %}
Issue description:
{{ issue.description }}
{% endif %}

Labels: {{ issue.labels }}
Blocked by: {{ issue.blocked_by }}

Requirements:

- Review the existing workspace changes as they are.
- Do not make new code changes.
- Write a reviewer-facing summary to `.harness/review-notes.md`.
- Write the machine-readable verdict to `.harness/review-result.json`.
