package server

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
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
      --bg: #f4f1ea;
      --panel: #fffdf8;
      --ink: #1f1f1f;
      --muted: #5f5a52;
      --border: #d9d1c3;
      --accent: #174c62;
      --warn: #9a3412;
      --ok: #166534;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "Iosevka Etoile", "IBM Plex Sans", "Segoe UI", sans-serif;
      background: radial-gradient(circle at top left, #fff9ef, var(--bg) 55%);
      color: var(--ink);
    }
    main {
      max-width: 1200px;
      margin: 0 auto;
      padding: 32px 20px 48px;
    }
    h1, h2 { margin: 0 0 12px; }
    .hero {
      display: grid;
      gap: 16px;
      margin-bottom: 24px;
    }
    .hero-card, .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 18px 20px;
      box-shadow: 0 10px 30px rgba(31, 31, 31, 0.04);
    }
    .stats {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 12px;
      margin-bottom: 24px;
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
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
      gap: 16px;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 14px;
    }
    th, td {
      padding: 10px 0;
      border-bottom: 1px solid var(--border);
      vertical-align: top;
      text-align: left;
    }
    th {
      font-size: 12px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--muted);
    }
    .mono {
      font-family: "Iosevka", "SFMono-Regular", monospace;
      font-size: 13px;
    }
    ul {
      margin: 8px 0 0;
      padding-left: 18px;
    }
    li + li { margin-top: 6px; }
    .empty {
      color: var(--muted);
      margin: 0;
    }
    .timeline {
      display: grid;
      gap: 10px;
      max-height: 420px;
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
  </style>
</head>
<body>
  <main>
    <section class="hero">
      <div class="hero-card">
        <h1>Go Harness</h1>
        <p>Last snapshot: <span class="mono">{{ formatTime .GeneratedAt }}</span></p>
        <p>Workflow: <span class="mono">{{ if .Workflow.Path }}{{ .Workflow.Path }}{{ else }}-{{ end }}</span></p>
        <p>Env file: <span class="mono">{{ if .Environment.DotEnvPath }}{{ .Environment.DotEnvPath }}{{ else }}-{{ end }}</span> {{ if .Environment.DotEnvPresent }}(present){{ else }}(missing){{ end }}</p>
        {{ if .Dispatch.Blocked }}
        <p class="dispatch-blocked"><strong>Dispatch blocked</strong>: {{ .Dispatch.Error }}</p>
        {{ else }}
        <p class="dispatch-ok"><strong>Dispatch healthy</strong></p>
        {{ end }}
        <div class="actions">
          <form method="post" action="/api/v1/refresh">
            <button type="submit">Trigger Refresh</button>
          </form>
          <a class="button secondary" href="/api/v1/state">Open JSON State</a>
        </div>
      </div>
    </section>

    <section class="stats">
      <div class="panel">
        <span class="stat-label">Running</span>
        <span class="stat-value">{{ .Counts.Running }}</span>
      </div>
      <div class="panel">
        <span class="stat-label">Retrying</span>
        <span class="stat-value">{{ .Counts.Retrying }}</span>
      </div>
      <div class="panel">
        <span class="stat-label">Completed</span>
        <span class="stat-value">{{ len .Completed }}</span>
      </div>
      <div class="panel">
        <span class="stat-label">Total Tokens</span>
        <span class="stat-value">{{ .CodexTotals.TotalTokens }}</span>
      </div>
    </section>

    <section class="grid">
      <div class="panel">
        <h2>Running</h2>
        {{ if .Running }}
        <table>
          <thead>
            <tr><th>Issue</th><th>Attempt</th><th>Session</th><th>Workspace</th></tr>
          </thead>
          <tbody>
            {{ range .Running }}
            <tr>
              <td>
                <div><strong>{{ .Issue.Identifier }}</strong></div>
                <div>{{ .Issue.Title }}</div>
                <div class="mono">{{ .Issue.State }}</div>
              </td>
              <td>{{ .Attempt }}</td>
              <td class="mono">{{ if .LiveSession }}{{ .LiveSession.SessionID }}{{ else }}-{{ end }}</td>
              <td class="mono">{{ .Workspace.Path }}</td>
            </tr>
            {{ end }}
          </tbody>
        </table>
        {{ else }}
        <p class="empty">No running issues.</p>
        {{ end }}
      </div>

      <div class="panel">
        <h2>Retry Queue</h2>
        {{ if .Retrying }}
        <table>
          <thead>
            <tr><th>Issue</th><th>Attempt</th><th>Reason</th><th>Due</th></tr>
          </thead>
          <tbody>
            {{ range .Retrying }}
            <tr>
              <td><strong>{{ .Identifier }}</strong></td>
              <td>{{ .Attempt }}</td>
              <td>{{ .Reason }}</td>
              <td class="mono">{{ formatTime .DueAt }}</td>
            </tr>
            {{ end }}
          </tbody>
        </table>
        {{ else }}
        <p class="empty">Retry queue is empty.</p>
        {{ end }}
      </div>

      <div class="panel">
        <h2>Completed</h2>
        {{ if .Completed }}
        <ul>
          {{ range .Completed }}
          <li class="mono">{{ . }}</li>
          {{ end }}
        </ul>
        {{ else }}
        <p class="empty">No completed issues in memory.</p>
        {{ end }}
      </div>

      <div class="panel">
        <h2>Recent Activity</h2>
        {{ if .RecentActivity }}
        <div class="timeline">
          {{ range .RecentActivity }}
          <div class="timeline-item">
            <div class="timeline-head">
              <div><strong>{{ if .Identifier }}{{ .Identifier }}{{ else }}-{{ end }}</strong> <span class="mono">{{ .Event }}</span></div>
              <div class="timeline-meta mono">{{ formatTime .At }}</div>
            </div>
            <div class="timeline-meta">
              {{ if .StateBefore }}{{ .StateBefore }}{{ end }}{{ if and .StateBefore .StateAfter }} -> {{ end }}{{ if .StateAfter }}{{ .StateAfter }}{{ end }}
              {{ if .Reason }} · reason={{ .Reason }}{{ end }}
              {{ if .Attempt }} · attempt={{ .Attempt }}{{ end }}
            </div>
            {{ if .Message }}<div>{{ .Message }}</div>{{ end }}
            {{ if .LastError }}<div class="mono">{{ .LastError }}</div>{{ end }}
          </div>
          {{ end }}
        </div>
        {{ else }}
        <p class="empty">No issue activity recorded yet.</p>
        {{ end }}
      </div>

      <div class="panel">
        <h2>Environment</h2>
        {{ if .Environment.Entries }}
        <table>
          <thead>
            <tr><th>Name</th><th>Source</th><th>Value</th></tr>
          </thead>
          <tbody>
            {{ range .Environment.Entries }}
            <tr>
              <td class="mono">{{ .Name }}</td>
              <td>{{ .Source }}</td>
              <td class="mono">{{ if .Value }}{{ .Value }}{{ else }}-{{ end }}</td>
            </tr>
            {{ end }}
          </tbody>
        </table>
        {{ else }}
        <p class="empty">No tracked environment entries.</p>
        {{ end }}
      </div>

      <div class="panel">
        <h2>Rate Limits</h2>
        {{ if .RateLimits }}
        <ul>
          {{ range .RateLimits }}
          <li><strong>{{ .Provider }}</strong> <span class="mono">{{ formatTime .UpdatedAt }}</span></li>
          {{ end }}
        </ul>
        {{ else }}
        <p class="empty">No rate limit snapshots received yet.</p>
        {{ end }}
      </div>
    </section>
  </main>
</body>
</html>`))

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
		if err := dashboardTemplate.Execute(&rendered, snapshot()); err != nil {
			writeError(w, http.StatusInternalServerError, "dashboard_render_failed", err.Error())
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rendered.Bytes())
	})
	return mux
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
