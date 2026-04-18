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

	"agent-collaboration/go/pkg/envelope"
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
	// Topic 3 §3.4 (f): daemon-spawn short-circuit. Matches the shell
	// hook's additive AGENT_COLLAB_FORCE_PY=1 pattern at
	// hooks/peer-inbox-inject.sh:~126. When the daemon (W3) spawns a
	// CLI, it exports AGENT_COLLAB_DAEMON_SPAWN=1 and delivers the
	// peer-inbox envelope directly via prompt injection (§2.4). The
	// hook must no-op for daemon spawns to avoid double-consumption
	// via the interactive ReadUnread path. Correctness safety net is
	// §3.4 (a) SQL partition (commit c96868f); this early-exit is a
	// performance optimization that skips the DB round-trip entirely.
	if os.Getenv("AGENT_COLLAB_DAEMON_SPAWN") == "1" {
		return 0
	}

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
				"hookEventName":     hookEventName(),
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

// hookEventName returns the hookSpecificOutput.hookEventName value to
// emit in the Claude-shape envelope. Reads AGENT_COLLAB_HOOK_EVENT_NAME
// which the bash wrapper populates from the stdin JSON's hook_event_name
// field ("UserPromptSubmit" for Claude/Codex, "BeforeAgent" for Gemini).
// Falls back to "UserPromptSubmit" when unset — the safe default when
// the binary is invoked standalone (e.g. tests/hook-latency-exec.sh)
// since Claude and Codex both use it.
func hookEventName() string {
	if v := os.Getenv("AGENT_COLLAB_HOOK_EVENT_NAME"); v != "" {
		return v
	}
	return "UserPromptSubmit"
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

// buildPeerInboxEnvelope lifts a slice of InboxMessage rows into the
// canonical envelope.Envelope schema (§5.1 of
// plans/v3.x-topic-3-implementation-scope.md).
//
// Hook-side callers pass to=nil: the hook path resolves the recipient
// implicitly via ResolveSelf so there's no need to embed it in the
// envelope. Daemon-side callers that already know the addressee pass a
// populated *envelope.Addressee; the same renderer consumes either
// shape.
//
// This is step one of the path-(a) structural refactor (§5.2): the
// schema becomes the internal data structure, but formatHookBlock
// (below) still emits byte-identical text against the pre-refactor
// fixtures.
func buildPeerInboxEnvelope(
	rows []store.InboxMessage,
	to *envelope.Addressee,
) envelope.Envelope {
	msgs := make([]envelope.Message, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, envelope.Message{
			ID:        r.ID,
			FromCWD:   r.FromCWD,
			FromLabel: r.FromLabel,
			Body:      r.Body,
			CreatedAt: r.CreatedAt,
			RoomKey:   r.RoomKey,
		})
	}
	return envelope.BuildFromHookRows(msgs, to)
}

// formatHookBlock mirrors format_hook_block in scripts/peer-inbox-db.py —
// byte-for-byte compatible so the Python CLI path and the Go hook path
// render identical additionalContext strings. Callers MUST produce the
// same output regardless of which language rendered it.
//
// Structured as a thin text renderer over buildPeerInboxEnvelope:
// Topic 3 §5.2 path (a) — the canonical JSON schema is now the
// internal data structure, but the text format is byte-identical to
// the pre-refactor hand-rolled implementation. tests/hook-parity.sh
// stays green; tests/envelope-round-trip.sh asserts the schema
// round-trip preserves output.
func formatHookBlock(rows []store.InboxMessage) string {
	if len(rows) == 0 {
		return ""
	}
	env := buildPeerInboxEnvelope(rows, nil)
	return renderEnvelopeText(env)
}

// renderEnvelopeText serializes an envelope.Envelope into the Option J
// <peer-inbox>...</peer-inbox> text block. The text format is fixed
// by tests/hook-parity.sh fixtures; any divergence is a path (b) scope
// creep per §5.2 and requires re-scoping before landing.
//
// v0 emits only the envelope's Messages; To / ContinuitySummary /
// State / ContentStop are not yet surfaced in the hook-text rendering.
// The daemon prompt-injection consumer (commit 7) uses the same text
// byte-for-byte for the message-only case; future format extensions
// for the optional fields are a path (b) decision out of scope here.
func renderEnvelopeText(env envelope.Envelope) string {
	if len(env.Messages) == 0 {
		return ""
	}

	// labels = sorted({ m.FromLabel for m in env.Messages })
	labelSet := make(map[string]struct{}, len(env.Messages))
	for _, m := range env.Messages {
		labelSet[m.FromLabel] = struct{}{}
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
		len(env.Messages), strings.Join(labels, ","),
	)
	b.WriteString(header)
	// Reserve space for the closing tag so the final block fits within
	// budget. Truncation message (~60 bytes when triggered) is unaccounted
	// headroom; HOOK_BLOCK_BUDGET is a 4 KiB ceiling against ~hundreds-
	// of-bytes messages, so the approximation is safe.
	const closingTag = "</peer-inbox>"
	used := len(header) + len(closingTag)

	included, truncated := 0, 0
	for _, m := range env.Messages {
		entry := fmt.Sprintf("\n[%s @ %s]\n%s\n", m.FromLabel, m.CreatedAt, m.Body)
		if used+len(entry) > budget && included > 0 {
			truncated = len(env.Messages) - included
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
