package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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
