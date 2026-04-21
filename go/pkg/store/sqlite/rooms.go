package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrRoomExists mirrors Python's EXIT_LABEL_COLLISION for room-create:
// returned when an explicit --pair-key collides with an existing
// peer_rooms row.
var ErrRoomExists = errors.New("store: room already exists")

// GeneratePairKey exposes the package-private generatePairKey helper
// so the CLI layer can mint adjective-noun-XXXX slugs without
// duplicating the wordlist. Returns a string of shape "adj-noun-xxxx".
func GeneratePairKey() string {
	return generatePairKey()
}

// CreateRoom ports cmd_room_create's transactional body: under
// BEGIN IMMEDIATE, check peer_rooms for room_key, INSERT if absent,
// else ErrRoomExists. home_host stamps the self-hosted label so
// cross-host routing can later recognise the room's origin.
func (s *SQLiteLocal) CreateRoom(ctx context.Context, pairKey, host string) error {
	if pairKey == "" {
		return fmt.Errorf("CreateRoom: empty pair_key")
	}
	roomKey := "pk:" + pairKey

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("CreateRoom: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var dummy int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM peer_rooms WHERE room_key = ?", roomKey).Scan(&dummy)
	if err == nil {
		return ErrRoomExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("CreateRoom: probe: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO peer_rooms (room_key, pair_key, turn_count, home_host)
		VALUES (?, ?, 0, ?)`, roomKey, pairKey, host); err != nil {
		return fmt.Errorf("CreateRoom: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("CreateRoom: commit: %w", err)
	}
	return nil
}

// DeleteRoom ports cmd_peer_reset's DELETE FROM peer_rooms. Returns
// true if a row was removed, false if the key was absent. Called with
// any of the three room_key shapes: "pk:<pair_key>", "cwd:<cwd>#a+b",
// or an operator-supplied --room-key escape hatch.
func (s *SQLiteLocal) DeleteRoom(ctx context.Context, roomKey string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("DeleteRoom: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"DELETE FROM peer_rooms WHERE room_key = ?", roomKey)
	if err != nil {
		return false, fmt.Errorf("DeleteRoom: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("DeleteRoom: rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("DeleteRoom: commit: %w", err)
	}
	return n > 0, nil
}
