// Command hook — the Go replacement for the peer-inbox portion of the
// UserPromptSubmit hook. Replaces `agent-collab peer receive --format
// hook-json --mark-read`, which paid ~80-150ms per prompt on the Python
// interpreter cold-start. Wall-clock fresh-exec p99 is the DoD surface
// (budget <15ms, enforced by tests/hook-latency-exec.sh); the in-process
// bench at hook_bench_test.go is a regression gate on the inner-loop
// shape. Clean path (~1ms) is the bash-side mtime marker check; this
// binary does not even run when the marker is older than the hook's
// last read.
//
// Contract with hooks/peer-inbox-inject.sh:
//
//   - Env vars: AGENT_COLLAB_INBOX_DB (optional override),
//     AGENT_COLLAB_SESSION_KEY / CLAUDE_SESSION_ID / CODEX_SESSION_ID /
//     GEMINI_SESSION_ID (pick the first set), AGENT_COLLAB_HOOK_LOG
//     (structured slog output).
//
//   - Argv: first positional arg is the cwd from the Claude hook
//     payload; falls back to the process's own cwd.
//
//   - Stdout: on unread messages, emits the exact JSON envelope Claude
//     expects: {"hookSpecificOutput": {"hookEventName":
//     "UserPromptSubmit", "additionalContext": "<peer-inbox ...>..."}}.
//     On empty inbox, emits nothing.
//
//   - Fail-open: every error path exits 0 with no stdout output. The
//     user's turn must never be blocked by an agent-collab internal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-collaboration/go/pkg/obs"
	"agent-collaboration/go/pkg/store"
	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

const (
	// defaultHookBlockBudget mirrors HOOK_BLOCK_BUDGET's default in
	// scripts/peer-inbox-db.py. Messages are truncated if they push the
	// <peer-inbox> block past this. Override at runtime via the
	// AGENT_COLLAB_HOOK_BLOCK_BUDGET env var, matching Python (A1 symmetry
	// fix from W1 round-1 review).
	//
	// Budget counts UTF-8 bytes on both runtimes (idle-birch #3 ruling:
	// align to bytes, because HOOK_BLOCK_BUDGET is a 4 KiB transport
	// budget — bytes are the real constraint surface).
	defaultHookBlockBudget = 4 * 1024

	// hotPathBudget is the soft deadline the binary aims for. We don't
	// hard-abort if we exceed it — we just log a slog event so tests/
	// latency.sh can flag regressions.
	hotPathBudget = 10 * time.Millisecond

	// exitFallbackWanted is returned when Go cannot complete the fetch
	// but the Python CLI fallback could ("try again with Python before
	// touching the ack file"). The bash wrapper reads this as non-zero
	// and routes to `agent-collab peer receive`. Examples: DB schema not
	// yet migrated, SQLite open error, transient ReadUnread error.
	//
	// Exit 0 remains "Go handled it cleanly — ack and move on" (includes
	// both the success path and benign no-op paths like ErrNoSession
	// where Python would ALSO produce nothing, so fallback is wasted).
	exitFallbackWanted = 2
)

// sessionKeyEnvCandidates mirrors SESSION_KEY_ENV_CANDIDATES in
// scripts/peer-inbox-db.py. First-match wins.
var sessionKeyEnvCandidates = []string{
	"AGENT_COLLAB_SESSION_KEY",
	"CLAUDE_SESSION_ID",
	"CODEX_SESSION_ID",
	"GEMINI_SESSION_ID",
}

func main() {
	os.Exit(run())
}

func run() int {
	start := time.Now()
	log := obs.Logger()

	// Redirect slog to the hook log file if one is set. Falls back to
	// stderr otherwise (which the bash hook captures into hook.log).
	if p := os.Getenv("AGENT_COLLAB_HOOK_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			log = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
			defer f.Close()
		}
	}

	cwd := resolveCWD()
	sessionKey := discoverSessionKey()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		// Fail-open on any Open failure — including ErrSchemaTooOld, which
		// means Python hasn't migrated this DB yet. Return
		// exitFallbackWanted so the bash wrapper re-routes to the Python
		// CLI (which will migrate + read) instead of touching the ack
		// file. Without the non-zero exit the hook would silently suppress
		// unread messages until the next send bumped the marker.
		log.Info("hook.open_failed",
			"result", "fallback_wanted",
			"err", err.Error(),
			"schema_too_old", errors.Is(err, store.ErrSchemaTooOld),
		)
		return exitFallbackWanted
	}
	defer st.Close()

	self, err := st.ResolveSelf(ctx, cwd, sessionKey)
	if err != nil {
		// No session here (ErrNoSession): silently skip, same as the
		// Python hook. Not a failure; just means the user is in a cwd
		// that never registered with peer-inbox — Python fallback would
		// produce nothing either, so exit 0 cleanly and let bash ack.
		log.Debug("hook.no_session", "cwd", cwd, "result", "skip")
		return 0
	}

	rows, err := st.ReadUnread(ctx, self)
	if err != nil {
		// Read failed mid-transaction: rollback already happened in
		// SQLiteLocal, but the caller should not mark this fetch observed.
		// Request Python fallback rather than swallowing as success.
		log.Info("hook.read_failed",
			"session", self.Label,
			"result", "fallback_wanted",
			"err", err.Error(),
		)
		return exitFallbackWanted
	}

	block := formatHookBlock(rows)
	if block != "" {
		envelope := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": block,
			},
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(envelope); err != nil {
			log.Info("hook.encode_failed", "result", "fail_open", "err", err.Error())
			return 0
		}
	}

	elapsed := time.Since(start)
	log.Debug("hook.complete",
		"session", self.Label,
		"messages", len(rows),
		"duration_ms", elapsed.Milliseconds(),
		"over_budget", elapsed > hotPathBudget,
		"result", "ok",
	)
	return 0
}

// resolveCWD reads the cwd from argv[1] if provided (bash hook passes the
// Claude-reported cwd there), else falls back to the process's own cwd.
func resolveCWD() string {
	if len(os.Args) > 1 && os.Args[1] != "" {
		return os.Args[1]
	}
	if c, err := os.Getwd(); err == nil {
		return c
	}
	return "."
}

func discoverSessionKey() string {
	for _, name := range sessionKeyEnvCandidates {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// hookBlockBudgetBytes reads AGENT_COLLAB_HOOK_BLOCK_BUDGET at runtime,
// falling back to defaultHookBlockBudget. Matches Python's
// HOOK_BLOCK_BUDGET env-override so both runtimes honor the same cap
// (A1 symmetry fix, W1 round-1 review).
func hookBlockBudgetBytes() int {
	if v := os.Getenv("AGENT_COLLAB_HOOK_BLOCK_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultHookBlockBudget
}

// formatHookBlock mirrors format_hook_block in scripts/peer-inbox-db.py —
// byte-for-byte compatible so the Python CLI path and the Go hook path
// render identical additionalContext strings. Callers MUST produce the
// same output regardless of which language rendered it.
func formatHookBlock(rows []store.InboxMessage) string {
	if len(rows) == 0 {
		return ""
	}

	// labels = sorted({ r.FromLabel for r in rows })
	labelSet := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		labelSet[r.FromLabel] = struct{}{}
	}
	labels := make([]string, 0, len(labelSet))
	for l := range labelSet {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	budget := hookBlockBudgetBytes()
	var b strings.Builder
	header := fmt.Sprintf(
		`<peer-inbox messages="%d" from-session-labels="%s">`,
		len(rows), strings.Join(labels, ","),
	)
	b.WriteString(header)
	// Reserve space for the closing tag so the final block fits within
	// budget. Truncation message (~60 bytes when triggered) is unaccounted
	// headroom; HOOK_BLOCK_BUDGET is a 4 KiB ceiling against ~hundreds-
	// of-bytes messages, so the approximation is safe.
	const closingTag = "</peer-inbox>"
	used := len(header) + len(closingTag)

	included, truncated := 0, 0
	for _, r := range rows {
		entry := fmt.Sprintf("\n[%s @ %s]\n%s\n", r.FromLabel, r.CreatedAt, r.Body)
		if used+len(entry) > budget && included > 0 {
			truncated = len(rows) - included
			break
		}
		b.WriteString(entry)
		used += len(entry)
		included++
	}
	if truncated > 0 {
		fmt.Fprintf(&b,
			"\n[+%d more messages truncated; run agent-collab peer receive to view]\n",
			truncated,
		)
	}
	b.WriteString("</peer-inbox>")
	return b.String()
}
