package workflow

import (
	"fmt"
	"strings"

	"go-harness/internal/domain"
)

type renderContext struct {
	Issue   domain.Issue
	Attempt int
}

type textNode struct {
	Text string
}

type varNode struct {
	Expr string
}

type ifNode struct {
	Condition string
	Then      []node
	Else      []node
}

type node interface {
	render(renderContext) (string, error)
}

func RenderPrompt(template string, issue domain.Issue, attempt int) (string, error) {
	nodes, pos, endTag, err := parseNodes(template, 0)
	if err != nil {
		return "", err
	}
	if endTag != "" {
		return "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unexpected template tag %q", endTag)}
	}
	if pos != len(template) {
		return "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unexpected trailing template content")}
	}

	var builder strings.Builder
	for _, item := range nodes {
		rendered, err := item.render(renderContext{Issue: issue, Attempt: attempt})
		if err != nil {
			return "", err
		}
		builder.WriteString(rendered)
	}

	return strings.TrimSpace(builder.String()), nil
}

func RenderContinuationPrompt(issue domain.Issue, turnCount int) string {
	return strings.TrimSpace(fmt.Sprintf(`
Continue working on the same issue in the existing agent conversation and workspace.

- Issue: %s
- Tracker state: %s
- Continuation turn: %d

Pick up from the current workspace state. Do not restart from scratch.
`, issue.Identifier, issue.State, turnCount))
}

func parseNodes(template string, pos int) ([]node, int, string, error) {
	nodes := make([]node, 0)

	for pos < len(template) {
		nextVar := strings.Index(template[pos:], "{{")
		nextStmt := strings.Index(template[pos:], "{%")
		next := nextTagIndex(nextVar, nextStmt)

		if next == -1 {
			nodes = append(nodes, textNode{Text: template[pos:]})
			return nodes, len(template), "", nil
		}

		tagPos := pos + next
		if tagPos > pos {
			nodes = append(nodes, textNode{Text: template[pos:tagPos]})
		}

		switch {
		case nextVar >= 0 && (nextStmt < 0 || nextVar < nextStmt):
			end := strings.Index(template[tagPos:], "}}")
			if end == -1 {
				return nil, 0, "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unclosed variable tag")}
			}

			expr := strings.TrimSpace(template[tagPos+2 : tagPos+end])
			nodes = append(nodes, varNode{Expr: expr})
			pos = tagPos + end + 2
		default:
			end := strings.Index(template[tagPos:], "%}")
			if end == -1 {
				return nil, 0, "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unclosed statement tag")}
			}

			stmt := strings.TrimSpace(template[tagPos+2 : tagPos+end])
			pos = tagPos + end + 2

			switch {
			case strings.HasPrefix(stmt, "if "):
				cond := strings.TrimSpace(strings.TrimPrefix(stmt, "if "))
				thenNodes, nextPos, closing, err := parseNodes(template, pos)
				if err != nil {
					return nil, 0, "", err
				}
				pos = nextPos

				var elseNodes []node
				switch closing {
				case "else":
					elseNodes, pos, closing, err = parseNodes(template, pos)
					if err != nil {
						return nil, 0, "", err
					}
					if closing != "endif" {
						return nil, 0, "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("if block missing endif")}
					}
				case "endif":
				default:
					return nil, 0, "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("if block missing endif")}
				}

				nodes = append(nodes, ifNode{Condition: cond, Then: thenNodes, Else: elseNodes})
			case stmt == "else" || stmt == "endif":
				return nodes, pos, stmt, nil
			default:
				return nil, 0, "", &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unknown statement %q", stmt)}
			}
		}
	}

	return nodes, pos, "", nil
}

func nextTagIndex(nextVar, nextStmt int) int {
	switch {
	case nextVar == -1:
		return nextStmt
	case nextStmt == -1:
		return nextVar
	case nextVar < nextStmt:
		return nextVar
	default:
		return nextStmt
	}
}

func (n textNode) render(_ renderContext) (string, error) {
	return n.Text, nil
}

func (n varNode) render(ctx renderContext) (string, error) {
	value, err := resolveExpression(n.Expr, ctx)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (n ifNode) render(ctx renderContext) (string, error) {
	truthy, err := resolveTruthy(n.Condition, ctx)
	if err != nil {
		return "", err
	}

	target := n.Then
	if !truthy {
		target = n.Else
	}

	var builder strings.Builder
	for _, item := range target {
		rendered, err := item.render(ctx)
		if err != nil {
			return "", err
		}
		builder.WriteString(rendered)
	}

	return builder.String(), nil
}

func resolveTruthy(expr string, ctx renderContext) (bool, error) {
	value, err := resolveRaw(expr, ctx)
	if err != nil {
		return false, err
	}

	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != "", nil
	case int:
		return typed != 0, nil
	case bool:
		return typed, nil
	case []string:
		return len(typed) > 0, nil
	case []domain.Blocker:
		return len(typed) > 0, nil
	default:
		return value != nil, nil
	}
}

func resolveExpression(expr string, ctx renderContext) (string, error) {
	value, err := resolveRaw(expr, ctx)
	if err != nil {
		return "", err
	}

	switch typed := value.(type) {
	case string:
		return typed, nil
	case int:
		return fmt.Sprintf("%d", typed), nil
	case bool:
		if typed {
			return "true", nil
		}
		return "false", nil
	case []string:
		return strings.Join(typed, ", "), nil
	case []domain.Blocker:
		identifiers := make([]string, 0, len(typed))
		for _, blocker := range typed {
			if blocker.Identifier != "" {
				identifiers = append(identifiers, blocker.Identifier)
			}
		}
		return strings.Join(identifiers, ", "), nil
	default:
		return "", nil
	}
}

func resolveRaw(expr string, ctx renderContext) (any, error) {
	if strings.Contains(expr, "|") {
		return nil, &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unsupported filter in %q", expr)}
	}

	switch strings.TrimSpace(expr) {
	case "attempt":
		return ctx.Attempt, nil
	case "issue.id":
		return ctx.Issue.ID, nil
	case "issue.identifier":
		return ctx.Issue.Identifier, nil
	case "issue.title":
		return ctx.Issue.Title, nil
	case "issue.description":
		return ctx.Issue.Description, nil
	case "issue.priority":
		return ctx.Issue.Priority, nil
	case "issue.state":
		return ctx.Issue.State, nil
	case "issue.branch_name":
		return ctx.Issue.BranchName, nil
	case "issue.url":
		return ctx.Issue.URL, nil
	case "issue.labels":
		return ctx.Issue.Labels, nil
	case "issue.blocked_by":
		return ctx.Issue.BlockedBy, nil
	default:
		return nil, &Error{Code: ErrTemplateRender, Err: fmt.Errorf("unknown variable %q", expr)}
	}
}
