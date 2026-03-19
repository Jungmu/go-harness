---
tracker:
  active_states:
    - In Review

agent:
  max_turns: 1
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
