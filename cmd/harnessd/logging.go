package main

import (
	"log/slog"
	"strings"

	"go-harness/internal/config"
)

type loggingConfigSource struct {
	store    *config.Store
	levelVar *slog.LevelVar
}

func (s *loggingConfigSource) Current() config.RuntimeConfig {
	return s.store.Current()
}

func (s *loggingConfigSource) ReloadIfChanged() (config.RuntimeConfig, bool, error) {
	cfg, changed, err := s.store.ReloadIfChanged()
	if err != nil {
		return cfg, changed, err
	}
	if changed {
		s.levelVar.Set(parseLogLevel(cfg.Logging.Level))
	}
	return cfg, changed, nil
}

func (s *loggingConfigSource) DispatchValidationError() error {
	return s.store.DispatchValidationError()
}

func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
