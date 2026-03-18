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
  max_concurrent_agents: 4
  max_turns: 20
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    In Progress: 2

codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000

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
- Use the issue context to make the smallest correct change.
- Run focused verification for the files you touched.
- Leave the workspace in a reviewable state for a human operator.
