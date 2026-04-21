package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// AdoptSessionKey re-keys an existing (cwd, label) row to newKey.
// Returns (oldKey, rowExists, err). When rowExists is false no UPDATE
// runs. Wrapped in BEGIN IMMEDIATE so the read-then-update is atomic
// against concurrent writers. Mirrors cmd_session_adopt.
func (s *SQLiteLocal) AdoptSessionKey(ctx context.Context, cwd, label, newKey string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldKey sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT session_key FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label,
	).Scan(&oldKey)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("probe session row: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET session_key = ? WHERE cwd = ? AND label = ?`,
		newKey, cwd, label,
	); err != nil {
		return "", true, fmt.Errorf("update session_key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", true, fmt.Errorf("commit: %w", err)
	}

	out := ""
	if oldKey.Valid {
		out = oldKey.String
	}
	return out, true, nil
}

// DeleteSession removes the row for (cwd, label). When sessionKey is
// non-empty, the WHERE also requires `session_key = ? OR session_key
// IS NULL` — matches Python's "only delete if we own this session_key
// OR the row has no key" guard. Returns rows-affected > 0.
func (s *SQLiteLocal) DeleteSession(ctx context.Context, cwd, label, sessionKey string) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if sessionKey != "" {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM sessions WHERE cwd = ? AND label = ?
			  AND (session_key = ? OR session_key IS NULL)`,
			cwd, label, sessionKey,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`DELETE FROM sessions WHERE cwd = ? AND label = ?`,
			cwd, label,
		)
	}
	if err != nil {
		return false, fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}
