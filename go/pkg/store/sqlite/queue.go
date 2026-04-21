package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PendingOutbound is one row from the v3.3 federation queue. Matches
// the migrations/sqlite/0006_pending_outbound.sql column set used by
// peer-queue --show / --flush.
type PendingOutbound struct {
	ID        int64
	MessageID string
	HomeHost  string
	PairKey   string
	FromLabel string
	ToLabel   string
	Body      string
	CreatedAt string
	Attempts  int64
	LastError string
}

// ListPendingOutbound returns pending_outbound rows, optionally
// filtered to a single home_host. Ordering matches the Python verbs:
//   - scoped by host → ORDER BY id
//   - unscoped        → ORDER BY home_host, pair_key, id  (FIFO per room)
func (s *SQLiteLocal) ListPendingOutbound(
	ctx context.Context, homeHost string,
) ([]PendingOutbound, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if homeHost != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, message_id, home_host, pair_key, from_label,
			       to_label, body, created_at, attempts, last_error
			FROM pending_outbound
			WHERE home_host = ?
			ORDER BY id`, homeHost)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, message_id, home_host, pair_key, from_label,
			       to_label, body, created_at, attempts, last_error
			FROM pending_outbound
			ORDER BY home_host, pair_key, id`)
	}
	if err != nil {
		return nil, fmt.Errorf("ListPendingOutbound: query: %w", err)
	}
	defer rows.Close()

	out := []PendingOutbound{}
	for rows.Next() {
		var r PendingOutbound
		var lastErr sql.NullString
		if err := rows.Scan(
			&r.ID, &r.MessageID, &r.HomeHost, &r.PairKey, &r.FromLabel,
			&r.ToLabel, &r.Body, &r.CreatedAt, &r.Attempts, &lastErr,
		); err != nil {
			return nil, fmt.Errorf("ListPendingOutbound: scan: %w", err)
		}
		r.LastError = nullString(lastErr)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnqueueOutbound persists a failed remote-send for later retry.
// Idempotent on message_id — a retry that also fails updates the
// attempts counter in place rather than duplicating the row. Matches
// Python _enqueue_outbound (peer-inbox-db.py:902-947).
func (s *SQLiteLocal) EnqueueOutbound(
	ctx context.Context,
	messageID, homeHost, pairKey, fromLabel, toLabel, body, lastError string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("EnqueueOutbound: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	var existingID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM pending_outbound WHERE message_id = ?`,
		messageID,
	).Scan(&existingID)
	switch err {
	case nil:
		if _, err := tx.ExecContext(ctx, `
			UPDATE pending_outbound
			   SET attempts = attempts + 1,
			       last_attempt_at = ?,
			       last_error = ?
			 WHERE id = ?`,
			now, lastError, existingID,
		); err != nil {
			return fmt.Errorf("EnqueueOutbound: bump attempt: %w", err)
		}
	case sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pending_outbound
			  (message_id, home_host, pair_key, from_label, to_label, body,
			   created_at, attempts, last_attempt_at, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			messageID, homeHost, pairKey, fromLabel, toLabel, body,
			now, now, lastError,
		); err != nil {
			return fmt.Errorf("EnqueueOutbound: insert: %w", err)
		}
	default:
		return fmt.Errorf("EnqueueOutbound: probe: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("EnqueueOutbound: commit: %w", err)
	}
	return nil
}

// DropPendingOutbound removes a row by id. Used after a successful
// flush replay or a TTL-drop.
func (s *SQLiteLocal) DropPendingOutbound(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM pending_outbound WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("DropPendingOutbound: %w", err)
	}
	return nil
}

// BumpPendingOutboundAttempt increments attempts + updates
// last_attempt_at + last_error. Used after a failed flush replay so
// the queue accumulates diagnostic breadcrumbs across retries.
func (s *SQLiteLocal) BumpPendingOutboundAttempt(
	ctx context.Context, id int64, lastError string,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pending_outbound
		SET attempts = attempts + 1,
		    last_attempt_at = ?,
		    last_error = ?
		WHERE id = ?`, time.Now().UTC().Format("2006-01-02T15:04:05Z"), lastError, id)
	if err != nil {
		return fmt.Errorf("BumpPendingOutboundAttempt: %w", err)
	}
	return nil
}
