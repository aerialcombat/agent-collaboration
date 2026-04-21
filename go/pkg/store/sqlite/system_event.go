package sqlite

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"
)

// SystemEventParams carries the inputs to EmitSystemEvent. Extracted
// into a struct so callers (session-register, session-close) read
// cleanly and so future kinds beyond join/leave land without a
// signature churn.
type SystemEventParams struct {
	SelfCWD   string
	SelfLabel string
	PairKey   string            // required — system events are pair-key-scoped
	Kind      string            // "join" / "leave"; stamped into push meta.system
	Body      string            // the "[system] <label> joined/left ..." line
	ExtraMeta map[string]string // optional; only [a-z0-9_]+ keys survive
	Now       time.Time
}

var systemMetaKeyRE = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// EmitSystemEvent fans a system-flavored message out to every other
// live peer in a pair-key room. Mirrors Python emit_system_event
// (peer-inbox-db.py:1517-1588):
//
//   - Writes an inbox row per recipient (so non-channel peers see the
//     event via the hook on their next prompt) in a single
//     BEGIN IMMEDIATE tx. server_seq is monotonic per-room.
//   - Does NOT bump peer_rooms.turn_count — system events are "free"
//     and don't count against the turn cap.
//   - Post-commit, pushes over each recipient's channel_socket when
//     alive; meta.system=<kind> lets receivers distinguish
//     join/leave/etc from regular messages.
//
// Caller validates pair_key is non-empty. No-op when there are no
// live peers.
func (s *SQLiteLocal) EmitSystemEvent(ctx context.Context, p SystemEventParams) error {
	if p.PairKey == "" {
		return fmt.Errorf("EmitSystemEvent: pair_key required")
	}
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	nowISO := p.Now.Format("2006-01-02T15:04:05Z")
	roomKey := "pk:" + p.PairKey

	// Enumerate candidates outside the write tx. SQLiteLocal has
	// MaxOpenConns=1, so overlapping an Acquire() with a later Conn()
	// would deadlock.
	rows, err := s.db.QueryContext(ctx, `
		SELECT cwd, label, last_seen_at, COALESCE(channel_socket, '')
		FROM sessions WHERE pair_key = ? AND NOT (cwd = ? AND label = ?)`,
		p.PairKey, p.SelfCWD, p.SelfLabel)
	if err != nil {
		return fmt.Errorf("system event candidates: %w", err)
	}
	type recip struct {
		cwd, label, sock string
	}
	var live []recip
	for rows.Next() {
		var r recip
		var lastSeen string
		if err := rows.Scan(&r.cwd, &r.label, &lastSeen, &r.sock); err != nil {
			rows.Close()
			return fmt.Errorf("scan candidate: %w", err)
		}
		if secondsSinceISO(lastSeen, p.Now) <= staleThresholdSecs {
			live = append(live, r)
		}
	}
	rows.Close()
	if len(live) == 0 {
		return nil
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open conn: %w", err)
	}
	connReleased := false
	releaseConn := func() {
		if !connReleased {
			_ = conn.Close()
			connReleased = true
		}
	}
	defer releaseConn()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed && !connReleased {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	for _, r := range live {
		nextSeq, err := nextServerSeq(ctx, conn, roomKey)
		if err != nil {
			return err
		}
		// idempotency_key deliberately unset — system events don't
		// dedupe (every join emits its own distinct body/timestamp).
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO inbox
			  (to_cwd, to_label, from_cwd, from_label, body, created_at,
			   room_key, server_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.cwd, r.label, p.SelfCWD, p.SelfLabel, p.Body, nowISO,
			roomKey, nextSeq); err != nil {
			return fmt.Errorf("insert system inbox row: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	releaseConn()
	markInboxDirty()

	// Filter ExtraMeta by Python's validation rule (alnum/_ keys only)
	// so Go and Python push identical meta for the same input.
	base := map[string]string{"system": p.Kind}
	for k, v := range p.ExtraMeta {
		if systemMetaKeyRE.MatchString(k) {
			base[k] = v
		}
	}

	for _, r := range live {
		if r.sock == "" {
			continue
		}
		if _, err := os.Stat(r.sock); err != nil {
			continue
		}
		meta := make(map[string]string, len(base)+1)
		for k, v := range base {
			meta[k] = v
		}
		meta["to"] = r.label
		_, _ = sendOverUnixSocket(r.sock, map[string]any{
			"from": p.SelfLabel,
			"body": p.Body,
			"meta": meta,
		})
	}
	return nil
}

