package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go-harness/internal/config"
	"go-harness/internal/domain"
)

const (
	reviewArtifactsDirName   = ".harness"
	reviewResultFilename     = "review-result.json"
	reviewNotesFilename      = "review-notes.md"
	reviewDecisionDone       = "done"
	reviewDecisionTodo       = "todo"
	reviewRejectedStopReason = "review_rejected"
)

type reviewVerdict struct {
	Decision       string                `json:"decision"`
	Summary        string                `json:"summary"`
	BlockingIssues []reviewBlockingIssue `json:"blocking_issues"`
}

type reviewBlockingIssue struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
}

func prepareReviewArtifacts(workspace domain.Workspace) error {
	if err := os.MkdirAll(reviewArtifactsDir(workspace), 0o755); err != nil {
		return err
	}
	err := os.Remove(reviewResultPath(workspace))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// peekReviewVerdict reads the current verdict file without deleting it.
// Use loadReviewVerdict when you want to consume the verdict.
func peekReviewVerdict(workspace domain.Workspace) (reviewVerdict, error) {
	path := reviewResultPath(workspace)
	raw, err := os.ReadFile(path)
	if err != nil {
		return reviewVerdict{}, err
	}
	var verdict reviewVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return reviewVerdict{}, fmt.Errorf("parse review verdict: %w", err)
	}
	verdict.Decision = strings.ToLower(strings.TrimSpace(verdict.Decision))
	return verdict, nil
}

func loadReviewVerdict(workspace domain.Workspace) (reviewVerdict, error) {
	path := reviewResultPath(workspace)
	raw, err := os.ReadFile(path)
	if err != nil {
		return reviewVerdict{}, err
	}

	var verdict reviewVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return reviewVerdict{}, fmt.Errorf("parse review verdict: %w", err)
	}
	verdict.Decision = strings.ToLower(strings.TrimSpace(verdict.Decision))
	if err := validateReviewVerdict(verdict); err != nil {
		return reviewVerdict{}, err
	}
	if err := validateReviewNotes(workspace); err != nil {
		return reviewVerdict{}, err
	}
	if err := os.Remove(path); err != nil {
		return reviewVerdict{}, err
	}
	return verdict, nil
}

func validateReviewVerdict(verdict reviewVerdict) error {
	switch verdict.Decision {
	case reviewDecisionDone, reviewDecisionTodo:
	default:
		return fmt.Errorf("review verdict decision must be %q or %q", reviewDecisionDone, reviewDecisionTodo)
	}
	if strings.TrimSpace(verdict.Summary) == "" {
		return fmt.Errorf("review verdict summary must not be empty")
	}
	for i, issue := range verdict.BlockingIssues {
		if strings.TrimSpace(issue.Title) == "" {
			return fmt.Errorf("review verdict blocking_issues[%d].title must not be empty", i)
		}
		if strings.TrimSpace(issue.Reason) == "" {
			return fmt.Errorf("review verdict blocking_issues[%d].reason must not be empty", i)
		}
		if strings.TrimSpace(issue.File) == "" {
			return fmt.Errorf("review verdict blocking_issues[%d].file must not be empty", i)
		}
	}
	switch verdict.Decision {
	case reviewDecisionDone:
		if len(verdict.BlockingIssues) > 0 {
			return fmt.Errorf("review verdict blocking_issues must be empty when decision=%q", reviewDecisionDone)
		}
	case reviewDecisionTodo:
		if len(verdict.BlockingIssues) == 0 {
			return fmt.Errorf("review verdict blocking_issues must be non-empty when decision=%q", reviewDecisionTodo)
		}
	}
	return nil
}

func validateReviewNotes(workspace domain.Workspace) error {
	raw, err := os.ReadFile(reviewNotesPath(workspace))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return fmt.Errorf("review notes must not be empty")
	}
	return nil
}

func appendCodingReviewNotesGuidance(prompt string, workspace domain.Workspace) string {
	if _, err := os.Stat(reviewNotesPath(workspace)); err != nil {
		return prompt
	}
	return strings.TrimSpace(prompt) + "\n\n" + strings.TrimSpace(
		"Review follow-up:\n\n"+
			"- If `.harness/review-notes.md` exists in the workspace, read it before making changes.\n"+
			"- Treat the blocking issues in that file as required work for this issue.\n",
	)
}

func appendGitHubPRGuidance(prompt string, cfg config.GitHubConfig) string {
	return strings.TrimSpace(prompt) + "\n\n" + strings.TrimSpace(fmt.Sprintf(
		"GitHub handoff:\n\n"+
			"- Leave the workspace on a clean git state before the run ends.\n"+
			"- Commit the final code changes on the issue branch so the harness can push it.\n"+
			"- The harness will create or reuse the GitHub pull request against %s/%s on branch %s when the issue moves to done.\n",
		cfg.Owner,
		cfg.Repo,
		cfg.BaseBranch,
	))
}

func appendReviewPromptContract(prompt string, workspace domain.Workspace) string {
	return strings.TrimSpace(prompt) + "\n\n" + strings.TrimSpace(fmt.Sprintf(`
Review contract:

- Review the current workspace state and the existing changes.
- Do not edit code, docs, tests, or workflow files.
- Write the machine-readable verdict JSON only to %s.
- Write human-readable review notes only to %s.
- The JSON must contain: decision, summary, blocking_issues.
- decision must be %q or %q.
- If decision is %q, blocking_issues must be [].
- If decision is %q, blocking_issues must include one or more blocking issues.
- Each blocking issue must include title, reason, and file. line is optional.
- If you are uncertain, choose %q.
- Do not treat nits or optional improvements as blocking issues.
`, reviewResultPath(workspace), reviewNotesPath(workspace), reviewDecisionDone, reviewDecisionTodo, reviewDecisionDone, reviewDecisionTodo, reviewDecisionTodo))
}

func reviewArtifactsDir(workspace domain.Workspace) string {
	return filepath.Join(workspace.Path, reviewArtifactsDirName)
}

func reviewResultPath(workspace domain.Workspace) string {
	return filepath.Join(reviewArtifactsDir(workspace), reviewResultFilename)
}

func reviewNotesPath(workspace domain.Workspace) string {
	return filepath.Join(reviewArtifactsDir(workspace), reviewNotesFilename)
}
