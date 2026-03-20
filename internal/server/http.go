package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"go-harness/internal/domain"
)

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"formatTime": func(value time.Time) string {
		if value.IsZero() {
			return "-"
		}
		return value.Format(time.RFC3339)
	},
	"relativeTime": func(value time.Time) string {
		if value.IsZero() {
			return "-"
		}
		now := time.Now().UTC()
		delta := value.Sub(now)
		if delta < 0 {
			delta = -delta
			return fmt.Sprintf("%s ago", humanDuration(delta))
		}
		return fmt.Sprintf("in %s", humanDuration(delta))
	},
	"clip": func(value string, limit int) string {
		value = strings.TrimSpace(value)
		if len(value) <= limit {
			return value
		}
		if limit <= 1 {
			return value[:limit]
		}
		return value[:limit-1] + "…"
	},
	"basePath": func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			return "-"
		}
		return filepath.Base(value)
	},
	"eventLabel": func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			return "-"
		}
		return strings.ReplaceAll(value, "_", " ")
	},
	"timelineSummary": func(event domain.TimelineEvent) string {
		if event.StateBefore != "" || event.StateAfter != "" {
			return strings.TrimSpace(event.StateBefore + " -> " + event.StateAfter)
		}
		if event.Message != "" {
			return event.Message
		}
		if event.LastError != "" {
			return event.LastError
		}
		if event.Reason != "" {
			return "reason=" + event.Reason
		}
		return strings.ReplaceAll(strings.TrimSpace(event.Event), "_", " ")
	},
	"runningSummary": func(value any) string {
		var snapshot domain.RunningSnapshot
		switch typed := value.(type) {
		case domain.RunningSnapshot:
			snapshot = typed
		case issuePanel:
			snapshot = typed.RunningSnapshot
		default:
			return "-"
		}
		if n := len(snapshot.RecentEvents); n > 0 {
			last := snapshot.RecentEvents[n-1]
			if strings.TrimSpace(last.Message) != "" {
				return last.Message
			}
			if strings.TrimSpace(last.PayloadSummary) != "" {
				return last.PayloadSummary
			}
			return strings.ReplaceAll(strings.TrimSpace(last.Event), "_", " ")
		}
		if snapshot.LiveSession != nil && strings.TrimSpace(snapshot.LiveSession.LastMessage) != "" {
			return snapshot.LiveSession.LastMessage
		}
		if snapshot.LiveSession != nil && strings.TrimSpace(snapshot.LiveSession.LastEvent) != "" {
			return strings.ReplaceAll(strings.TrimSpace(snapshot.LiveSession.LastEvent), "_", " ")
		}
		return "-"
	},
	"runningLastAt": func(value any) time.Time {
		var snapshot domain.RunningSnapshot
		switch typed := value.(type) {
		case domain.RunningSnapshot:
			snapshot = typed
		case issuePanel:
			snapshot = typed.RunningSnapshot
		default:
			return time.Time{}
		}
		if n := len(snapshot.RecentEvents); n > 0 {
			return snapshot.RecentEvents[n-1].At
		}
		if snapshot.LiveSession != nil {
			if !snapshot.LiveSession.LastEventAt.IsZero() {
				return snapshot.LiveSession.LastEventAt
			}
			return snapshot.LiveSession.StartedAt
		}
		return snapshot.StartedAt
	},
	"attentionCount": func(value any) int {
		switch snapshot := value.(type) {
		case domain.StateSnapshot:
			count := len(snapshot.Retrying)
			if snapshot.Dispatch.Blocked {
				count++
			}
			return count
		case dashboardView:
			count := len(snapshot.Retrying)
			if snapshot.Dispatch.Blocked {
				count++
			}
			return count
		default:
			return 0
		}
	},
	"transcriptLabel": func(entry domain.PromptTranscriptEntry) string {
		switch strings.TrimSpace(entry.Channel) {
		case "prompt":
			return "rendered prompt"
		case "stderr":
			return "stderr"
		case "stdin":
			return "stdin"
		case "stdout":
			return "stdout"
		default:
			if strings.TrimSpace(entry.Channel) == "" {
				return "transcript"
			}
			return entry.Channel
		}
	},
	"transcriptBody": func(entry domain.PromptTranscriptEntry) string {
		payload := strings.TrimSpace(entry.Payload)
		if payload == "" {
			return "-"
		}
		return payload
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="5">
  <title>Go Harness</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f3efe7;
      --panel: #fffdf8;
      --panel-strong: #fff8ee;
      --ink: #1f1f1f;
      --muted: #635c52;
      --border: #d9d1c3;
      --accent: #174c62;
      --accent-soft: #d9e7ed;
      --warn: #9a3412;
      --warn-soft: #fff0e8;
      --ok: #166534;
      --ok-soft: #e8f5ec;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "Iosevka Etoile", "IBM Plex Sans", "Segoe UI", sans-serif;
      background: radial-gradient(circle at top left, #fff9ef, var(--bg) 55%);
      color: var(--ink);
    }
    main {
      max-width: 1280px;
      margin: 0 auto;
      padding: 28px 20px 56px;
    }
    h1, h2, h3, p { margin: 0; }
    .shell {
      display: grid;
      gap: 18px;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 18px 20px;
      box-shadow: 0 10px 30px rgba(31, 31, 31, 0.04);
    }
    .hero {
      display: grid;
      grid-template-columns: minmax(0, 1.7fr) minmax(280px, 1fr);
      gap: 16px;
    }
    .hero-main {
      background: linear-gradient(135deg, #fffaf1, #fffdf8);
    }
    .hero-kicker {
      color: var(--muted);
      font-size: 13px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      margin-bottom: 10px;
    }
    .hero-title {
      display: flex;
      justify-content: space-between;
      align-items: start;
      gap: 16px;
      margin-bottom: 12px;
    }
    .hero-title h1 {
      font-size: 34px;
      line-height: 1.05;
    }
    .hero-summary {
      display: grid;
      gap: 8px;
      color: var(--muted);
      margin-bottom: 18px;
    }
    .hero-meta {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 10px;
      margin-bottom: 18px;
    }
    .meta-block {
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 12px 14px;
      background: rgba(255,255,255,0.7);
    }
    .meta-label {
      display: block;
      font-size: 12px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--muted);
      margin-bottom: 6px;
    }
    .stats {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 12px;
    }
    .stat {
      border: 1px solid var(--border);
      border-radius: 14px;
      padding: 14px 16px;
      background: var(--panel-strong);
    }
    .stat-label {
      display: block;
      font-size: 12px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--muted);
      margin-bottom: 6px;
    }
    .stat-value {
      font-size: 28px;
      font-weight: 700;
    }
    .stat-note {
      color: var(--muted);
      font-size: 13px;
      margin-top: 6px;
    }
    .dispatch-ok { color: var(--ok); }
    .dispatch-blocked { color: var(--warn); }
    .actions {
      display: flex;
      gap: 12px;
      flex-wrap: wrap;
      margin-top: 14px;
    }
    .button, button {
      appearance: none;
      border: 0;
      border-radius: 999px;
      background: var(--accent);
      color: white;
      padding: 10px 16px;
      font: inherit;
      cursor: pointer;
      text-decoration: none;
    }
    .button.secondary {
      background: #d6e2e8;
      color: #153441;
    }
    .section-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      margin-bottom: 14px;
    }
    .stack {
      display: grid;
      gap: 6px;
    }
    .muted {
      color: var(--muted);
    }
    .pill {
      display: inline-flex;
      align-items: center;
      border-radius: 999px;
      padding: 4px 10px;
      background: #e8f0f3;
      color: #153441;
      font-size: 12px;
      font-weight: 600;
    }
    .pill.warn {
      background: #fff1ea;
      color: var(--warn);
    }
    .pill.ok {
      background: var(--ok-soft);
      color: var(--ok);
    }
    .pill.neutral {
      background: var(--accent-soft);
      color: #153441;
    }
    .layout {
      display: grid;
      grid-template-columns: minmax(0, 1.35fr) minmax(340px, 0.9fr);
      gap: 16px;
    }
    .column {
      display: grid;
      gap: 16px;
    }
    .card-list {
      display: grid;
      gap: 12px;
    }
    .issue-card {
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 16px;
      background: #fffaf2;
      display: grid;
      gap: 14px;
    }
    .issue-card.warn {
      background: var(--warn-soft);
    }
    .issue-top {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: start;
    }
    .issue-title {
      display: grid;
      gap: 6px;
    }
    .issue-title h3 {
      font-size: 18px;
      line-height: 1.2;
    }
    .issue-links {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
    }
    .mono {
      font-family: "Iosevka", "SFMono-Regular", monospace;
      font-size: 13px;
    }
    .prompt-preview {
      display: grid;
      gap: 10px;
      border-top: 1px solid var(--border);
      padding-top: 12px;
    }
    .prompt-line {
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 10px 12px;
      background: #fffdf8;
    }
    .prompt-line pre {
      margin: 8px 0 0;
      white-space: pre-wrap;
      word-break: break-word;
      font: inherit;
    }
    .kvs {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 10px 14px;
    }
    .kv {
      display: grid;
      gap: 4px;
    }
    .kv-label {
      font-size: 12px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--muted);
    }
    .empty {
      color: var(--muted);
      margin: 0;
    }
    .timeline {
      display: grid;
      gap: 10px;
      max-height: 820px;
      overflow: auto;
    }
    .timeline-item {
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 12px 14px;
      background: #fffaf2;
    }
    .timeline-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 6px;
      flex-wrap: wrap;
    }
    .timeline-meta {
      color: var(--muted);
      font-size: 13px;
    }
    .attention-list {
      display: grid;
      gap: 10px;
    }
    .attention-item {
      border-left: 4px solid var(--warn);
      padding-left: 12px;
    }
    details.panel {
      padding: 0;
      overflow: hidden;
    }
    details > summary {
      list-style: none;
      cursor: pointer;
      padding: 18px 20px;
      font-weight: 700;
    }
    details > summary::-webkit-details-marker {
      display: none;
    }
    .details-body {
      border-top: 1px solid var(--border);
      padding: 18px 20px 20px;
    }
    .system-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
      gap: 16px;
    }
    .system-block {
      display: grid;
      gap: 10px;
    }
    .system-list {
      display: grid;
      gap: 8px;
    }
    .system-row {
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 10px 12px;
      background: #fffaf2;
    }
    @media (max-width: 960px) {
      .hero,
      .layout {
        grid-template-columns: 1fr;
      }
      .timeline {
        max-height: none;
      }
    }
  </style>
</head>
<body>
  <main>
    <div class="shell">
      <section class="hero">
        <div class="panel hero-main">
          <div class="hero-kicker">Operations</div>
          <div class="hero-title">
            <div>
              <h1>Go Harness Control Panel</h1>
            </div>
            {{ if .Dispatch.Blocked }}
            <span class="pill warn">Dispatch blocked</span>
            {{ else }}
            <span class="pill ok">Dispatch healthy</span>
            {{ end }}
          </div>
          <div class="hero-summary">
            <div>Designed for triage first: what is blocked, what is running, and what needs operator attention next.</div>
            <div>Last snapshot <span class="mono">{{ formatTime .GeneratedAt }}</span> · refreshed {{ relativeTime .GeneratedAt }}</div>
          </div>
          <div class="hero-meta">
            <div class="meta-block">
              <span class="meta-label">Workflow</span>
              <div class="mono">{{ if .Workflow.Path }}{{ .Workflow.Path }}{{ else }}-{{ end }}</div>
            </div>
            <div class="meta-block">
              <span class="meta-label">Review Workflow</span>
              <div class="mono">{{ if .Workflow.ReviewPath }}{{ .Workflow.ReviewPath }}{{ else }}-{{ end }}</div>
            </div>
            <div class="meta-block">
              <span class="meta-label">Env File</span>
              <div class="mono">{{ if .Environment.DotEnvPath }}{{ .Environment.DotEnvPath }}{{ else }}-{{ end }}</div>
              <div class="muted">{{ if .Environment.DotEnvPresent }}present{{ else }}missing{{ end }}</div>
            </div>
          </div>
          <div class="actions">
            <form method="post" action="/api/v1/refresh">
              <button type="submit">Trigger Refresh</button>
            </form>
            <a class="button secondary" href="/api/v1/state">Open JSON State</a>
          </div>
        </div>

        <div class="panel">
          <div class="section-head">
            <h2>At A Glance</h2>
            <span class="pill neutral">{{ attentionCount . }} attention</span>
          </div>
          <div class="stats">
            <div class="stat">
              <span class="stat-label">Running</span>
              <div class="stat-value">{{ .Counts.Running }}</div>
              <div class="stat-note">active issue workspaces</div>
            </div>
            <div class="stat">
              <span class="stat-label">Retrying</span>
              <div class="stat-value">{{ .Counts.Retrying }}</div>
              <div class="stat-note">issues waiting to re-run</div>
            </div>
            <div class="stat">
              <span class="stat-label">Completed</span>
              <div class="stat-value">{{ len .Completed }}</div>
              <div class="stat-note">completed in memory</div>
            </div>
            <div class="stat">
              <span class="stat-label">Total Tokens</span>
              <div class="stat-value">{{ .AgentTotals.TotalTokens }}</div>
              <div class="stat-note">{{ .AgentTotals.InputTokens }} in · {{ .AgentTotals.OutputTokens }} out</div>
            </div>
          </div>
          {{ if .Dispatch.Workers }}
          <div class="stack" style="margin-top:14px">
            {{ range .Dispatch.Workers }}
            <div class="system-row">
              <strong>{{ .Worker }}</strong> · {{ if .Blocked }}blocked{{ else }}healthy{{ end }}{{ if .Error }} · {{ .Error }}{{ end }}
            </div>
            {{ end }}
          </div>
          {{ end }}
        </div>
      </section>

      <section class="layout">
        <div class="column">
          <div class="panel">
            <div class="section-head">
              <h2>Action Needed</h2>
              <span class="muted">operator-first triage</span>
            </div>
            {{ if or .Dispatch.Blocked .Retrying }}
            <div class="attention-list">
              {{ if .Dispatch.Blocked }}
              <div class="attention-item">
                <div><strong>Dispatch blocked</strong></div>
                <div>{{ .Dispatch.Error }}</div>
              </div>
              {{ end }}
              {{ range .Retrying }}
              <div class="attention-item">
                <div><strong>{{ .Identifier }}</strong> <span class="pill warn">retry {{ .Attempt }}</span></div>
                <div>{{ .Reason }} · next run {{ relativeTime .DueAt }}</div>
                {{ if .LastError }}<div class="mono">{{ clip .LastError 180 }}</div>{{ end }}
              </div>
              {{ end }}
            </div>
            {{ else }}
            <p class="empty">No active failures need attention.</p>
            {{ end }}
          </div>

          <div class="panel">
            <div class="section-head">
              <h2>Active Issues</h2>
              <span class="muted">{{ .Counts.Running }} running</span>
            </div>
            {{ if .RunningCards }}
            <div class="card-list">
              {{ range .RunningCards }}
              <article class="issue-card">
                <div class="issue-top">
                  <div class="issue-title">
                    <div><strong>{{ .Issue.Identifier }}</strong></div>
                    <h3>{{ .Issue.Title }}</h3>
                    <div><span class="pill">{{ .Issue.State }}</span></div>
                  </div>
                  <div class="stack" style="justify-items:end">
                    <div class="pill neutral">{{ if .LiveSession }}{{ if .LiveSession.Worker }}{{ .LiveSession.Worker }}{{ else }}worker{{ end }}{{ else }}worker{{ end }}</div>
                    <div class="muted">attempt {{ .Attempt }}{{ if .LiveSession }} · turn {{ .LiveSession.TurnCount }}{{ end }}</div>
                  </div>
                </div>
                <div class="kvs">
                  <div class="kv">
                    <div class="kv-label">Live Summary</div>
                    <div>{{ runningSummary . }}</div>
                  </div>
                  <div class="kv">
                    <div class="kv-label">Last Activity</div>
                    <div>{{ relativeTime (runningLastAt .) }}</div>
                  </div>
                  <div class="kv">
                    <div class="kv-label">Session</div>
                    <div class="mono">{{ if .LiveSession }}{{ if .LiveSession.SessionID }}{{ clip .LiveSession.SessionID 36 }}{{ else }}-{{ end }}{{ else }}-{{ end }}</div>
                  </div>
                  <div class="kv">
                    <div class="kv-label">Workspace</div>
                    <div class="mono">{{ basePath .Workspace.Path }}</div>
                  </div>
                  {{ if .LiveSession }}
                  <div class="kv">
                    <div class="kv-label">Tokens</div>
                    <div>{{ .LiveSession.TotalTokens }}</div>
                  </div>
                  {{ end }}
                  <div class="kv">
                    <div class="kv-label">Started</div>
                    <div>{{ relativeTime .StartedAt }}</div>
                  </div>
                </div>
                {{ if .LastError }}
                <div class="mono">{{ clip .LastError 180 }}</div>
                {{ end }}
                {{ if .PromptTranscript }}
                <div class="prompt-preview">
                  <div class="section-head">
                    <h3>Prompt Log</h3>
                    <span class="muted">recent transcript preview</span>
                  </div>
                  {{ range .PromptTranscript }}
                  <div class="prompt-line">
                    <div class="timeline-meta mono">{{ relativeTime .At }} · {{ .Kind }}{{ if .Attempt }} · attempt {{ .Attempt }}{{ end }}</div>
                    <pre>{{ clip .Body 320 }}</pre>
                  </div>
                  {{ end }}
                </div>
                {{ end }}
                <div class="issue-links">
                  <a class="button secondary" href="/issues/{{ .Issue.Identifier }}">Open Issue Detail</a>
                  <a class="button secondary" href="/api/v1/issues/{{ .Issue.Identifier }}">Open Issue JSON</a>
                </div>
              </article>
              {{ end }}
            </div>
            {{ else }}
            <p class="empty">No running issues.</p>
            {{ end }}
          </div>

          <div class="panel">
            <div class="section-head">
              <h2>Retrying</h2>
              <span class="muted">{{ .Counts.Retrying }} queued</span>
            </div>
            {{ if .RetryCards }}
            <div class="card-list">
              {{ range .RetryCards }}
              <article class="issue-card warn">
                <div class="issue-top">
                  <div class="issue-title">
                    <div><strong>{{ .Identifier }}</strong></div>
                    <h3>{{ .Reason }}</h3>
                  </div>
                  <div class="pill warn">attempt {{ .Attempt }}</div>
                </div>
                <div class="kvs">
                  <div class="kv">
                    <div class="kv-label">Next Retry</div>
                    <div>{{ relativeTime .DueAt }}</div>
                    <div class="mono muted">{{ formatTime .DueAt }}</div>
                  </div>
                  <div class="kv">
                    <div class="kv-label">Reason</div>
                    <div>{{ .Reason }}</div>
                  </div>
                </div>
                {{ if .LastError }}
                <div class="mono">{{ clip .LastError 220 }}</div>
                {{ end }}
                {{ if .PromptTranscript }}
                <div class="prompt-preview">
                  <div class="section-head">
                    <h3>Prompt Log</h3>
                    <span class="muted">recent transcript preview</span>
                  </div>
                  {{ range .PromptTranscript }}
                  <div class="prompt-line">
                    <div class="timeline-meta mono">{{ relativeTime .At }} · {{ .Kind }}{{ if .Attempt }} · attempt {{ .Attempt }}{{ end }}</div>
                    <pre>{{ clip .Body 320 }}</pre>
                  </div>
                  {{ end }}
                </div>
                {{ end }}
                <div class="issue-links">
                  <a class="button secondary" href="/issues/{{ .Identifier }}">Open Issue Detail</a>
                  <a class="button secondary" href="/api/v1/issues/{{ .Identifier }}">Open Issue JSON</a>
                </div>
              </article>
              {{ end }}
            </div>
            {{ else }}
            <p class="empty">Retry queue is empty.</p>
            {{ end }}
          </div>
        </div>

        <div class="column">
          <div class="panel">
            <div class="section-head">
              <h2>Timeline</h2>
              <span class="muted">latest activity first</span>
            </div>
            {{ if .RecentActivity }}
            <div class="timeline">
              {{ range .RecentActivity }}
              <div class="timeline-item">
                <div class="timeline-head">
                  <div><strong>{{ if .Identifier }}{{ .Identifier }}{{ else }}-{{ end }}</strong> <span class="mono">{{ eventLabel .Event }}</span></div>
                  <div class="timeline-meta mono">{{ relativeTime .At }}</div>
                </div>
                <div>{{ clip (timelineSummary .) 220 }}</div>
                <div class="timeline-meta">
                  {{ formatTime .At }}
                  {{ if .Attempt }} · attempt={{ .Attempt }}{{ end }}
                  {{ if .Reason }} · reason={{ .Reason }}{{ end }}
                </div>
                {{ if .LastError }}<div class="mono">{{ clip .LastError 180 }}</div>{{ end }}
              </div>
              {{ end }}
            </div>
            {{ else }}
            <p class="empty">No issue activity recorded yet.</p>
            {{ end }}
          </div>

          <details class="panel">
            <summary>System Details</summary>
            <div class="details-body">
              <div class="system-grid">
                <div class="system-block">
                  <h3>Environment</h3>
                  {{ if .Environment.Entries }}
                  <div class="system-list">
                    {{ range .Environment.Entries }}
                    <div class="system-row">
                      <div class="mono">{{ .Name }}</div>
                      <div>{{ .Source }}</div>
                      <div class="mono muted">{{ if .Value }}{{ .Value }}{{ else }}-{{ end }}</div>
                    </div>
                    {{ end }}
                  </div>
                  {{ else }}
                  <p class="empty">No tracked environment entries.</p>
                  {{ end }}
                </div>

                <div class="system-block">
                  <h3>Rate Limits</h3>
                  {{ if .RateLimits }}
                  <div class="system-list">
                    {{ range .RateLimits }}
                    <div class="system-row">
                      <div><strong>{{ .Provider }}</strong></div>
                      <div class="mono">{{ formatTime .UpdatedAt }}</div>
                    </div>
                    {{ end }}
                  </div>
                  {{ else }}
                  <p class="empty">No rate limit snapshots received yet.</p>
                  {{ end }}
                </div>

                <div class="system-block">
                  <h3>Completed</h3>
                  {{ if .Completed }}
                  <div class="system-list">
                    {{ range .Completed }}
                    <div class="system-row mono">{{ . }}</div>
                    {{ end }}
                  </div>
                  {{ else }}
                  <p class="empty">No completed issues in memory.</p>
                  {{ end }}
                </div>
              </div>
            </div>
          </details>
        </div>
      </section>
    </div>
  </main>
</body>
</html>`))

var issueTemplate = template.Must(template.New("issue").Funcs(template.FuncMap{
	"formatTime": func(value time.Time) string {
		if value.IsZero() {
			return "-"
		}
		return value.Format(time.RFC3339)
	},
	"relativeTime": func(value time.Time) string {
		if value.IsZero() {
			return "-"
		}
		now := time.Now().UTC()
		delta := value.Sub(now)
		if delta < 0 {
			return fmt.Sprintf("%s ago", humanDuration(-delta))
		}
		return fmt.Sprintf("in %s", humanDuration(delta))
	},
	"clip": func(value string, limit int) string {
		value = strings.TrimSpace(value)
		if len(value) <= limit {
			return value
		}
		if limit <= 1 {
			return value[:limit]
		}
		return value[:limit-1] + "…"
	},
	"eventLabel": func(value string) string {
		if strings.TrimSpace(value) == "" {
			return "-"
		}
		return strings.ReplaceAll(strings.TrimSpace(value), "_", " ")
	},
	"timelineSummary": func(event domain.TimelineEvent) string {
		if event.StateBefore != "" || event.StateAfter != "" {
			return strings.TrimSpace(event.StateBefore + " -> " + event.StateAfter)
		}
		if event.Message != "" {
			return event.Message
		}
		if event.LastError != "" {
			return event.LastError
		}
		return strings.ReplaceAll(strings.TrimSpace(event.Event), "_", " ")
	},
	"transcriptLabel": func(entry domain.PromptTranscriptEntry) string {
		switch strings.TrimSpace(entry.Channel) {
		case "prompt":
			return "rendered prompt"
		case "stderr":
			return "stderr"
		case "stdin":
			return "stdin"
		case "stdout":
			return "stdout"
		default:
			if strings.TrimSpace(entry.Channel) == "" {
				return "transcript"
			}
			return entry.Channel
		}
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Issue Detail</title>
  <style>
    body { margin: 0; font-family: "Iosevka Etoile", "IBM Plex Sans", sans-serif; background: #f3efe7; color: #1f1f1f; }
    main { max-width: 1100px; margin: 0 auto; padding: 28px 20px 56px; display: grid; gap: 16px; }
    .panel { background: #fffdf8; border: 1px solid #d9d1c3; border-radius: 16px; padding: 18px 20px; }
    .top { display: flex; justify-content: space-between; gap: 12px; align-items: start; }
    .muted { color: #635c52; }
    .mono { font-family: "Iosevka", "SFMono-Regular", monospace; font-size: 13px; word-break: break-word; }
    .button { display: inline-flex; text-decoration: none; border-radius: 999px; padding: 10px 16px; background: #174c62; color: white; }
    .stack { display: grid; gap: 10px; }
    .item { border: 1px solid #d9d1c3; border-radius: 12px; padding: 12px 14px; background: #fffaf2; }
    .item pre { margin: 8px 0 0; white-space: pre-wrap; word-break: break-word; font: inherit; }
    h1,h2,h3,p { margin: 0; }
  </style>
</head>
<body>
  <main>
    <section class="panel">
      <div class="top">
        <div>
          <h1>{{ .Identifier }}</h1>
          <p class="muted">status={{ .Status }} · snapshot {{ formatTime .GeneratedAt }}</p>
        </div>
        <a class="button" href="/">Back To Dashboard</a>
      </div>
    </section>

    <section class="panel">
      <div class="top">
        <div>
          <h2>Prompt Transcript</h2>
          <p class="muted">latest entries first, human-readable preview</p>
        </div>
        <a class="button" href="/api/v1/issues/{{ .Identifier }}">Open JSON</a>
      </div>
      {{ if .Transcript }}
      <div class="stack" style="margin-top:14px">
        {{ range .Transcript }}
        <div class="item">
          <div class="mono muted">{{ relativeTime .At }} · {{ .Kind }}{{ if .Attempt }} · attempt {{ .Attempt }}{{ end }}</div>
          <pre>{{ clip .Body 4000 }}</pre>
        </div>
        {{ end }}
      </div>
      {{ else }}
      <p class="muted" style="margin-top:14px">No prompt transcript entries recorded for this issue.</p>
      {{ end }}
    </section>

    <section class="panel">
      <h2>Issue Timeline</h2>
      {{ if .History }}
      <div class="stack" style="margin-top:14px">
        {{ range .History }}
        <div class="item">
          <div><strong>{{ eventLabel .Event }}</strong></div>
          <div>{{ clip (timelineSummary .) 320 }}</div>
          <div class="mono muted">{{ formatTime .At }}{{ if .Attempt }} · attempt {{ .Attempt }}{{ end }}</div>
        </div>
        {{ end }}
      </div>
      {{ else }}
      <p class="muted" style="margin-top:14px">No timeline entries recorded for this issue.</p>
      {{ end }}
    </section>
  </main>
</body>
</html>`))

type dashboardView struct {
	domain.StateSnapshot
	RunningCards []issuePanel
	RetryCards   []retryPanel
}

type issuePanel struct {
	domain.RunningSnapshot
	PromptTranscript []transcriptViewEntry
}

type retryPanel struct {
	domain.RetryEntry
	PromptTranscript []transcriptViewEntry
}

type issueDetailView struct {
	GeneratedAt time.Time
	Identifier  string
	Status      string
	Transcript  []transcriptViewEntry
	History     []domain.TimelineEvent
}

type transcriptViewEntry struct {
	At      time.Time
	Attempt int
	Kind    string
	Body    string
}

func NewHandler(snapshot func() domain.StateSnapshot, issueSnapshot func(string) (domain.IssueRuntimeSnapshot, bool), triggerRefresh func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		current := snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"generated":   current.GeneratedAt,
			"workflow":    current.Workflow,
			"environment": current.Environment,
			"dispatch":    current.Dispatch,
			"counts":      current.Counts,
		})
	})
	mux.HandleFunc("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, snapshot())
	})
	mux.HandleFunc("/api/v1/issues/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		identifier := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
		if identifier == "" {
			writeError(w, http.StatusNotFound, "not_found", "issue identifier is required")
			return
		}
		payload, ok := issueSnapshot(identifier)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "issue is not present in the current runtime state")
			return
		}
		writeJSON(w, http.StatusOK, payload)
	})
	mux.HandleFunc("/issues/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		identifier := strings.TrimPrefix(r.URL.Path, "/issues/")
		if identifier == "" {
			writeError(w, http.StatusNotFound, "not_found", "issue identifier is required")
			return
		}
		payload, ok := issueSnapshot(identifier)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "issue is not present in the current runtime state")
			return
		}
		view := issueDetailView{
			GeneratedAt: payload.GeneratedAt,
			Identifier:  payload.Identifier,
			Status:      payload.Status,
			Transcript:  reverseTranscriptView(normalizeTranscript(payload.PromptTranscript)),
			History:     payload.History,
		}
		var rendered bytes.Buffer
		if err := issueTemplate.Execute(&rendered, view); err != nil {
			writeError(w, http.StatusInternalServerError, "issue_render_failed", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rendered.Bytes())
	})
	mux.HandleFunc("/api/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		triggerRefresh()
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}

		var rendered bytes.Buffer
		if err := dashboardTemplate.Execute(&rendered, buildDashboardView(snapshot(), issueSnapshot)); err != nil {
			writeError(w, http.StatusInternalServerError, "dashboard_render_failed", err.Error())
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rendered.Bytes())
	})
	return mux
}

func buildDashboardView(snapshot domain.StateSnapshot, issueSnapshot func(string) (domain.IssueRuntimeSnapshot, bool)) dashboardView {
	view := dashboardView{
		StateSnapshot: snapshot,
		RunningCards:  make([]issuePanel, 0, len(snapshot.Running)),
		RetryCards:    make([]retryPanel, 0, len(snapshot.Retrying)),
	}
	for _, running := range snapshot.Running {
		panel := issuePanel{RunningSnapshot: running}
		if payload, ok := issueSnapshot(running.Issue.Identifier); ok {
			panel.PromptTranscript = promptPreview(payload.PromptTranscript, 3)
		}
		view.RunningCards = append(view.RunningCards, panel)
	}
	for _, retry := range snapshot.Retrying {
		panel := retryPanel{RetryEntry: retry}
		if payload, ok := issueSnapshot(retry.Identifier); ok {
			panel.PromptTranscript = promptPreview(payload.PromptTranscript, 3)
		}
		view.RetryCards = append(view.RetryCards, panel)
	}
	return view
}

func promptPreview(entries []domain.PromptTranscriptEntry, limit int) []transcriptViewEntry {
	items := normalizeTranscript(entries)
	if len(items) == 0 || limit <= 0 {
		return nil
	}
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	return reverseTranscriptView(items)
}

func reverseTranscriptView(entries []transcriptViewEntry) []transcriptViewEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]transcriptViewEntry, len(entries))
	for i := range entries {
		out[len(entries)-1-i] = entries[i]
	}
	return out
}

func normalizeTranscript(entries []domain.PromptTranscriptEntry) []transcriptViewEntry {
	if len(entries) == 0 {
		return nil
	}
	items := make([]transcriptViewEntry, 0, len(entries))
	for _, entry := range entries {
		if item, ok := normalizeTranscriptEntry(entry); ok {
			items = append(items, item)
		}
	}
	return items
}

func normalizeTranscriptEntry(entry domain.PromptTranscriptEntry) (transcriptViewEntry, bool) {
	switch strings.TrimSpace(entry.Channel) {
	case "prompt":
		return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Prompt", Body: strings.TrimSpace(entry.Payload)}, true
	case "stderr":
		return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "stderr", Body: strings.TrimSpace(entry.Payload)}, true
	case "stdout":
		return normalizeProtocolPayload(entry)
	default:
		return transcriptViewEntry{}, false
	}
}

func normalizeProtocolPayload(entry domain.PromptTranscriptEntry) (transcriptViewEntry, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(entry.Payload), &payload); err != nil {
		return transcriptViewEntry{}, false
	}
	method, _ := payload["method"].(string)
	switch method {
	case "item/completed":
		item := nestedMap(payload, "params", "item")
		switch strings.ToLower(stringValue(item, "type")) {
		case "agentmessage":
			if text := strings.TrimSpace(stringValue(item, "text")); text != "" {
				return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Agent", Body: text}, true
			}
		case "commandexecution":
			body := strings.TrimSpace(stringValue(item, "aggregatedOutput"))
			if body == "" {
				body = strings.TrimSpace(stringValue(item, "command"))
			}
			if body != "" {
				return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Command", Body: body}, true
			}
		}
	case "item/started":
		item := nestedMap(payload, "params", "item")
		if strings.ToLower(stringValue(item, "type")) == "commandexecution" {
			if command := strings.TrimSpace(stringValue(item, "command")); command != "" {
				return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Command", Body: command}, true
			}
		}
	case "codex/event/plan_update":
		plan := nestedSlice(payload, "params", "msg", "plan")
		lines := make([]string, 0, len(plan))
		for _, raw := range plan {
			stepMap, _ := raw.(map[string]any)
			step := strings.TrimSpace(stringValue(stepMap, "step"))
			status := strings.TrimSpace(stringValue(stepMap, "status"))
			if step == "" {
				continue
			}
			if status != "" {
				lines = append(lines, "["+status+"] "+step)
			} else {
				lines = append(lines, step)
			}
		}
		if len(lines) > 0 {
			return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Plan", Body: strings.Join(lines, "\n")}, true
		}
	case "turn/started":
		return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Turn", Body: "turn started"}, true
	case "turn/completed":
		return transcriptViewEntry{At: entry.At, Attempt: entry.Attempt, Kind: "Turn", Body: "turn completed"}, true
	}
	return transcriptViewEntry{}, false
}

func nestedMap(root map[string]any, keys ...string) map[string]any {
	current := root
	for i, key := range keys {
		value, ok := current[key]
		if !ok {
			return nil
		}
		if i == len(keys)-1 {
			result, _ := value.(map[string]any)
			return result
		}
		next, _ := value.(map[string]any)
		if next == nil {
			return nil
		}
		current = next
	}
	return nil
}

func nestedSlice(root map[string]any, keys ...string) []any {
	if len(keys) == 0 {
		return nil
	}
	parent := nestedMap(root, keys[:len(keys)-1]...)
	if parent == nil {
		return nil
	}
	values, _ := parent[keys[len(keys)-1]].([]any)
	return values
}

func stringValue(root map[string]any, key string) string {
	if root == nil {
		return ""
	}
	value, _ := root[key]
	text, _ := value.(string)
	return text
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func humanDuration(value time.Duration) string {
	if value < time.Minute {
		seconds := int(value.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return fmt.Sprintf("%ds", seconds)
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm", int(value.Round(time.Minute)/time.Minute))
	}
	if value < 24*time.Hour {
		hours := value / time.Hour
		minutes := (value % time.Hour) / time.Minute
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	days := value / (24 * time.Hour)
	hours := (value % (24 * time.Hour)) / time.Hour
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, hours)
}
