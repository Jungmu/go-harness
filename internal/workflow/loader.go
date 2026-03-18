package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go-harness/internal/domain"
	"gopkg.in/yaml.v3"
)

type ErrorCode string

const (
	ErrMissingWorkflowFile       ErrorCode = "missing_workflow_file"
	ErrInvalidWorkflowYAML       ErrorCode = "invalid_workflow_yaml"
	ErrFrontMatterNotAMap        ErrorCode = "workflow_front_matter_not_a_map"
	ErrMalformedFrontMatterFence ErrorCode = "workflow_front_matter_missing_closing_fence"
	ErrTemplateRender            ErrorCode = "workflow_template_render_error"
)

type Error struct {
	Code ErrorCode
	Path string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Path)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Path, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}

type Loader struct{}

func NewLoader() *Loader {
	return &Loader{}
}

func (l *Loader) Load(path string) (domain.WorkflowDefinition, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return domain.WorkflowDefinition{}, &Error{Code: ErrMissingWorkflowFile, Path: path, Err: err}
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.WorkflowDefinition{}, &Error{Code: ErrMissingWorkflowFile, Path: absPath, Err: err}
		}
		return domain.WorkflowDefinition{}, &Error{Code: ErrMissingWorkflowFile, Path: absPath, Err: err}
	}

	config, body, err := splitWorkflow(absPath, string(raw))
	if err != nil {
		return domain.WorkflowDefinition{}, err
	}

	return domain.WorkflowDefinition{
		SourcePath:     absPath,
		Config:         config,
		PromptTemplate: strings.TrimSpace(body),
	}, nil
}

func splitWorkflow(path, content string) (map[string]any, string, error) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return map[string]any{}, content, nil
	}

	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return map[string]any{}, "", nil
	}

	end := -1
	for idx := 1; idx < len(lines); idx++ {
		if strings.TrimSpace(lines[idx]) == "---" {
			end = idx
			break
		}
	}

	if end == -1 {
		return nil, "", &Error{Code: ErrMalformedFrontMatterFence, Path: path}
	}

	frontMatter := strings.Join(lines[1:end], "\n")
	body := strings.Join(lines[end+1:], "\n")

	var decoded any
	if err := yaml.Unmarshal([]byte(frontMatter), &decoded); err != nil {
		return nil, "", &Error{Code: ErrInvalidWorkflowYAML, Path: path, Err: err}
	}

	if decoded == nil {
		return map[string]any{}, body, nil
	}

	config, ok := toStringMap(decoded)
	if !ok {
		return nil, "", &Error{Code: ErrFrontMatterNotAMap, Path: path}
	}

	return config, body, nil
}

func toStringMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = normalizeValue(item)
		}
		return converted, true
	case map[any]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			stringKey, ok := key.(string)
			if !ok {
				return nil, false
			}
			converted[stringKey] = normalizeValue(item)
		}
		return converted, true
	default:
		return nil, false
	}
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any, map[any]any:
		mapped, ok := toStringMap(typed)
		if ok {
			return mapped
		}
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeValue(item))
		}
		return out
	}
	return value
}
