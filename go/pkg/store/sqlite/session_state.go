package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// v3.8 agent activity monitoring.
//
// Sessions get a lightweight `state` column reflecting "is this agent
// currently generating a turn?" vs "waiting for input?". Populated
// primarily by Claude Code hooks (UserPromptSubmit → active, Stop →
// idle), but any sender can flip it via the session-state CLI or
// direct call to SetSessionState / SetSessionStateByKey below.
//
// Distinct from last_seen_at (which only bumps on send) and the
// channel socket (which only says "the MCP subprocess is alive").
// State is the busy/idle signal.

// SessionState values. "active" + "idle" are the two valid writable
// values; NULL/empty = unknown (pre-v3.8 sessions, or agents without
// hook support).
const (
	SessionStateActive = "active"
	SessionStateIdle   = "idle"
)

// ErrSessionNotFound is returned by the state setters when no row
// matches the (cwd, label) or session_key. Caller maps to CLI/HTTP.
var ErrSessionNotFound = errors.New("store: session not found")

// SetSessionState writes (state, state_changed_at) onto the session row
// identified by (cwd, label). state_changed_at only moves when state
// actually transitions — the UPDATE's WHERE clause skips the write on
// idempotent calls, keeping the hook hot-path cheap.
//
// Returns ErrSessionNotFound if no row with that (cwd, label) exists.
// Transitioning to the same value on an already-matching row is a
// no-op (no error). Transitioning on an existing row returns nil.
func (s *SQLiteLocal) SetSessionState(
	ctx context.Context, cwd, label, state string, now time.Time,
) error {
	if state != SessionStateActive && state != SessionStateIdle {
		return fmt.Errorf("invalid state %q: must be %q or %q",
			state, SessionStateActive, SessionStateIdle)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowISO := now.Format("2006-01-02T15:04:05Z")

	// Gate the UPDATE on row existence via a probe so we can distinguish
	// "no such session" (ErrSessionNotFound) from "same state, no-op"
	// (nil). Leaning on RowsAffected alone would collapse the two.
	var existing sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT state FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("SetSessionState: probe: %w", err)
	}
	if existing.Valid && existing.String == state {
		return nil
	}
	if _, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, state_changed_at = ?
		 WHERE cwd = ? AND label = ?`,
		state, nowISO, cwd, label); err != nil {
		return fmt.Errorf("SetSessionState: update: %w", err)
	}
	return nil
}

// SweepStaleActive flips any session rows whose state="active" and
// whose state_changed_at is older than cutoff back to state="idle".
// Returns the number of rows updated.
//
// The watchdog calls this periodically (see peer-web's state_watchdog)
// to protect against "agent crashed mid-turn, Stop hook never fired"
// leaving UI stuck on busy forever. Cutoff should be comfortably
// longer than any legitimate turn; 30min is the default.
func (s *SQLiteLocal) SweepStaleActive(
	ctx context.Context, cutoff time.Duration, now time.Time,
) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowISO := now.Format("2006-01-02T15:04:05Z")
	// Compute the "newest acceptable state_changed_at" — anything
	// older than this is eligible to be swept.
	cutoffISO := now.Add(-cutoff).Format("2006-01-02T15:04:05Z")
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		   SET state = 'idle', state_changed_at = ?
		 WHERE state = 'active'
		   AND state_changed_at IS NOT NULL
		   AND state_changed_at < ?`,
		nowISO, cutoffISO)
	if err != nil {
		return 0, fmt.Errorf("SweepStaleActive: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetSessionStateByKey resolves the session via its session_key (the
// token the hook already carries in CLAUDE_SESSION_ID / AGENT_COLLAB_
// SESSION_KEY) and applies the state update. Preferred over
// SetSessionState on hot paths where (cwd, label) isn't pre-resolved —
// saves a walk-the-markers round-trip.
//
// Falls back silently (no-op, no error) when the key doesn't map to
// any session: first-prompt-before-register and hook-without-session
// cases should not surface as errors from the hook.
func (s *SQLiteLocal) SetSessionStateByKey(
	ctx context.Context, sessionKey, state string, now time.Time,
) error {
	if sessionKey == "" {
		return nil
	}
	if state != SessionStateActive && state != SessionStateIdle {
		return fmt.Errorf("invalid state %q", state)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowISO := now.Format("2006-01-02T15:04:05Z")

	// One-shot update that skips the write when state is already the
	// target value. Leverages SQLite's NULL-equality via IS.
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		   SET state = ?,
		       state_changed_at = ?
		 WHERE session_key = ?
		   AND (state IS NULL OR state != ?)`,
		state, nowISO, sessionKey, state)
	if err != nil {
		return fmt.Errorf("SetSessionStateByKey: %w", err)
	}
	return nil
}
