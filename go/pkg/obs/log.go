// Package obs wraps log/slog with a structured JSON handler for stderr.
// Kept tiny in W1 — the full observability story (histograms, /metrics,
// error-path telemetry) lands in W3. This file exists so W1 components
// don't scatter ad-hoc fmt.Fprintln calls across stderr.
package obs

import (
	"log/slog"
	"os"
	"sync"
)

var (
	once   sync.Once
	logger *slog.Logger
)

// Logger returns a process-wide *slog.Logger writing structured JSON to
// stderr. Fields common across the agent-collaboration project:
//
//	event        — short dotted name ("hook.start", "store.query", "error.schema")
//	operation    — what was being attempted when the event fired
//	duration_ms  — measured wall time (float64, millisecond resolution)
//	result       — "ok" | "fail" | "skipped"
//	pair_key     — room identifier if in-scope
//	workspace_id — multi-tenant identifier (v1: always "default")
//	session      — local session label
func Logger() *slog.Logger {
	once.Do(func() {
		level := slog.LevelInfo
		if v := os.Getenv("AGENT_COLLAB_LOG_LEVEL"); v == "debug" {
			level = slog.LevelDebug
		}
		logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: level,
		}))
	})
	return logger
}
