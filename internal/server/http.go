package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"go-harness/internal/domain"
)

func NewHandler(snapshot func() domain.StateSnapshot, issueSnapshot func(string) (domain.IssueRuntimeSnapshot, bool), triggerRefresh func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
