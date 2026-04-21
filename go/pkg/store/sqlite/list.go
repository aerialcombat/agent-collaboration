package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"agent-collaboration/go/pkg/store"
)

// SessionRow is one row returned by ListSessions. Mirrors the column
// selection in cmd_session_list in scripts/peer-inbox-db.py:
//
//	SELECT cwd, label, agent, role, started_at, last_seen_at
//	FROM sessions ORDER BY last_seen_at DESC [WHERE cwd = ?]
//
// Role is nullable in the schema; callers apply Python's "peer" default
// when the column is NULL/empty. See cmd_session_list's
// `r['role'] or 'peer'` formatting.
type SessionRow struct {
	CWD        string
	Label      string
	Agent      string
	Role       string // "" when column is NULL
	StartedAt  string
	LastSeenAt string
}

// ListPeers returns the sibling sessions a given (self) session shares
// scope with: pair_key match when self has one, else cwd match.
// Excludes the self row. Ordered by last_seen_at DESC. Mirrors
// cmd_peer_list in scripts/peer-inbox-db.py.
//
// selfPairKey is the raw value of sessions.pair_key for (selfCWD,
// selfLabel); caller is expected to fetch it first (or pass "" to use
// the cwd-scope fallback).
func (s *SQLiteLocal) ListPeers(ctx context.Context, selfCWD, selfLabel, selfPairKey string) ([]SessionRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if selfPairKey != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, agent, role, started_at, last_seen_at
			FROM sessions
			WHERE pair_key = ? AND NOT (cwd = ? AND label = ?)
			ORDER BY last_seen_at DESC`, selfPairKey, selfCWD, selfLabel)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, agent, role, started_at, last_seen_at
			FROM sessions
			WHERE cwd = ? AND NOT (cwd = ? AND label = ?)
			ORDER BY last_seen_at DESC`, selfCWD, selfCWD, selfLabel)
	}
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var role sql.NullString
		if err := rows.Scan(&r.CWD, &r.Label, &r.Agent, &role, &r.StartedAt, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan peer row: %w", err)
		}
		if role.Valid {
			r.Role = role.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list peers rows err: %w", err)
	}
	return out, nil
}

// ListUnreadForSelf returns the inbox rows addressed to (self) that are
// unread AND not in-flight under a daemon claim — the SQL partition
// invariant (Topic 3 §3.4 (a)). Non-mutating: callers that want to
// claim-and-mark use ReadUnread instead. Mirrors the read-only inspect
// path of cmd_peer_receive in scripts/peer-inbox-db.py.
//
// sinceISO is optional; when non-empty, rows with created_at < sinceISO
// are filtered out (Python's --since flag). Ordered by created_at ASC
// to match Python's ORDER BY.
func (s *SQLiteLocal) ListUnreadForSelf(ctx context.Context, self store.Session, sinceISO string) ([]store.InboxMessage, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if sinceISO != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_cwd, from_label, body, created_at, COALESCE(room_key, '')
			FROM inbox
			WHERE to_cwd = ? AND to_label = ?
			  AND read_at IS NULL
			  AND claimed_at IS NULL
			  AND created_at >= ?
			ORDER BY created_at ASC`, self.CWD, self.Label, sinceISO)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_cwd, from_label, body, created_at, COALESCE(room_key, '')
			FROM inbox
			WHERE to_cwd = ? AND to_label = ?
			  AND read_at IS NULL
			  AND claimed_at IS NULL
			ORDER BY created_at ASC`, self.CWD, self.Label)
	}
	if err != nil {
		return nil, fmt.Errorf("list unread: %w", err)
	}
	defer rows.Close()

	var out []store.InboxMessage
	for rows.Next() {
		var m store.InboxMessage
		if err := rows.Scan(&m.ID, &m.FromCWD, &m.FromLabel, &m.Body, &m.CreatedAt, &m.RoomKey); err != nil {
			return nil, fmt.Errorf("scan unread row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unread rows err: %w", err)
	}
	return out, nil
}

// GetSessionPairKey reads pair_key for (cwd, label). Returns "" if the
// session row is missing or pair_key is NULL. Mirrors Python's
// _get_session_pair_key.
func (s *SQLiteLocal) GetSessionPairKey(ctx context.Context, cwd, label string) (string, error) {
	var pk sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT pair_key FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label).Scan(&pk)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("get session pair_key: %w", err)
	}
	if !pk.Valid {
		return "", nil
	}
	return pk.String, nil
}

// ListSessions returns sessions ordered by last_seen_at DESC. When
// scopeCWD is empty the query runs unfiltered (the --all-cwds path);
// otherwise it restricts to rows with cwd = scopeCWD.
func (s *SQLiteLocal) ListSessions(ctx context.Context, scopeCWD string) ([]SessionRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if scopeCWD == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, agent, role, started_at, last_seen_at
			FROM sessions ORDER BY last_seen_at DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, agent, role, started_at, last_seen_at
			FROM sessions WHERE cwd = ? ORDER BY last_seen_at DESC`, scopeCWD)
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var role sql.NullString
		if err := rows.Scan(&r.CWD, &r.Label, &r.Agent, &role, &r.StartedAt, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		if role.Valid {
			r.Role = role.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions rows err: %w", err)
	}
	return out, nil
}
