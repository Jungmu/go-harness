package workflow

import (
	"strings"
	"testing"

	"go-harness/internal/domain"
)

func TestRenderPromptVariablesAndIfElse(t *testing.T) {
	t.Parallel()

	template := strings.TrimSpace(`
Issue {{ issue.identifier }}
{% if issue.description %}
Desc {{ issue.description }}
{% else %}
No description
{% endif %}
Attempt {{ attempt }}
`)

	rendered, err := RenderPrompt(template, domain.Issue{
		Identifier: "ABC-123",
	}, 2)
	if err != nil {
		t.Fatalf("RenderPrompt() error = %v", err)
	}

	if !strings.Contains(rendered, "Issue ABC-123") {
		t.Fatalf("rendered prompt missing identifier: %q", rendered)
	}
	if !strings.Contains(rendered, "No description") {
		t.Fatalf("rendered prompt missing else branch: %q", rendered)
	}
	if !strings.Contains(rendered, "Attempt 2") {
		t.Fatalf("rendered prompt missing attempt: %q", rendered)
	}
}

func TestRenderPromptRejectsUnknownVariable(t *testing.T) {
	t.Parallel()

	_, err := RenderPrompt("{{ issue.unknown }}", domain.Issue{}, 1)
	if err == nil {
		t.Fatal("RenderPrompt() error = nil, want error")
	}
}

func TestRenderContinuationPromptIncludesIssueStateAndTurn(t *testing.T) {
	t.Parallel()

	rendered := RenderContinuationPrompt(domain.Issue{
		Identifier: "ABC-123",
		State:      "In Progress",
	}, 2)

	if !strings.Contains(rendered, "Issue: ABC-123") {
		t.Fatalf("continuation prompt missing identifier: %q", rendered)
	}
	if !strings.Contains(rendered, "Tracker state: In Progress") {
		t.Fatalf("continuation prompt missing state: %q", rendered)
	}
	if !strings.Contains(rendered, "Continuation turn: 2") {
		t.Fatalf("continuation prompt missing turn count: %q", rendered)
	}
}
