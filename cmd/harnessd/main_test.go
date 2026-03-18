package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunStatusPrintsSnapshot(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/state" {
			t.Fatalf("path = %q, want /api/v1/state", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"generated_at":"2026-03-18T09:00:00Z","counts":{"running":1,"retrying":0}}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	exitCode := run([]string{"status", "--addr", server.URL}, &stdout, server.Client())
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), `"running": 1`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
