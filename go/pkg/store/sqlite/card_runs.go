package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CardRun statuses (v3.11). The schema CHECK constraint enforces these
// values; the constants prevent string-literal drift.
const (
	CardRunStatusRunning   = "running"
	CardRunStatusCompleted = "completed"
	CardRunStatusFailed    = "failed"
	CardRunStatusCancelled = "cancelled"
	CardRunStatusLost      = "lost"
)

// CardRun trigger values (v3.11).
const (
	CardRunTriggerManual     = "manual"     // POST /api/cards/{id}/run from UI/CLI
	CardRunTriggerDrainer    = "drainer"    // standing drainer dispatched it
	CardRunTriggerMatchmaker = "matchmaker" // reserved for Phase 4
)

var (
	// ErrCardRunNotFound — id lookup missed.
	ErrCardRunNotFound = errors.New("store: card_run not found")
)

// CardRun mirrors one row of the card_runs table.
type CardRun struct {
	ID          int64
	CardID      int64
	PairKey     string
	Host        string
	WorkerLabel string
	PID         int    // 0 once process exits or before it's set
	StartedAt   string // RFC3339 UTC
	EndedAt     string // empty while running
	Status      string
	ExitCode    int    // ignored when Status='running'
	LogPath     string // nullable; nulled by retention sweep
	Trigger     string
}

// CreateCardRunParams — explicit struct for inserting a new run.
type CreateCardRunParams struct {
	CardID      int64
	PairKey     string
	Host        string // defaults to SelfHost() when empty
	WorkerLabel string
	Trigger     string // defaults to 'manual' when empty
	LogPath     string // optional; can be set later via UpdateCardRun
}

// CreateCardRun inserts a new card_run row in status='running' with
// pid=NULL (the caller fills pid after the worker is spawned via
// UpdateCardRunPID). Returns the inserted row.
func (s *SQLiteLocal) CreateCardRun(ctx context.Context, p CreateCardRunParams) (*CardRun, error) {
	if p.CardID == 0 || p.PairKey == "" || p.WorkerLabel == "" {
		return nil, fmt.Errorf("CreateCardRun: card_id, pair_key, worker_label required")
	}
	if p.Host == "" {
		p.Host = SelfHost()
	}
	if p.Trigger == "" {
		p.Trigger = CardRunTriggerManual
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO card_runs (
		  card_id, pair_key, host, worker_label, started_at, status, log_path, trigger
		) VALUES (?, ?, ?, ?, ?, 'running', ?, ?)
	`, p.CardID, p.PairKey, p.Host, p.WorkerLabel, now, nullableString(p.LogPath), p.Trigger)
	if err != nil {
		return nil, fmt.Errorf("insert card_runs: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &CardRun{
		ID:          id,
		CardID:      p.CardID,
		PairKey:     p.PairKey,
		Host:        p.Host,
		WorkerLabel: p.WorkerLabel,
		StartedAt:   now,
		Status:      CardRunStatusRunning,
		LogPath:     p.LogPath,
		Trigger:     p.Trigger,
	}, nil
}

// UpdateCardRunPID sets the pid on a running card_run row. Called once
// the spawned process exists and we know its pid.
func (s *SQLiteLocal) UpdateCardRunPID(ctx context.Context, id int64, pid int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE card_runs SET pid=? WHERE id=? AND status='running'`, pid, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrCardRunNotFound
	}
	return nil
}

// UpdateCardRunLogPath sets log_path on a row. Called once the spawn
// is committed and we know where the file lives.
func (s *SQLiteLocal) UpdateCardRunLogPath(ctx context.Context, id int64, logPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE card_runs SET log_path=? WHERE id=?`, nullableString(logPath), id)
	return err
}

// FinishCardRun marks a card_run terminal — sets ended_at, status,
// exit_code, and clears pid. Idempotent in the sense that re-finishing
// an already-finished row is a no-op (the WHERE clause filters).
func (s *SQLiteLocal) FinishCardRun(ctx context.Context, id int64, status string, exitCode int) error {
	switch status {
	case CardRunStatusCompleted, CardRunStatusFailed,
		CardRunStatusCancelled, CardRunStatusLost:
	default:
		return fmt.Errorf("FinishCardRun: invalid terminal status %q", status)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		UPDATE card_runs
		   SET ended_at=?, status=?, exit_code=?, pid=NULL
		 WHERE id=? AND status='running'
	`, now, status, exitCode, id)
	return err
}

// GetCardRun returns one row by id.
func (s *SQLiteLocal) GetCardRun(ctx context.Context, id int64) (*CardRun, error) {
	row := s.db.QueryRowContext(ctx, cardRunSelectCols+` WHERE id=?`, id)
	r, err := scanCardRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardRunNotFound
	}
	return r, err
}

// ListCardRunsByCard returns all runs for a card, newest first. limit=0
// means no cap.
func (s *SQLiteLocal) ListCardRunsByCard(ctx context.Context, cardID int64, limit int) ([]*CardRun, error) {
	q := cardRunSelectCols + ` WHERE card_id=? ORDER BY id DESC`
	args := []any{cardID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CardRun
	for rows.Next() {
		r, err := scanCardRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunningCardRuns returns every row in status='running' optionally
// filtered to a single host (empty = all hosts; callers normally pass
// SelfHost() because each host reaps only its own).
func (s *SQLiteLocal) ListRunningCardRuns(ctx context.Context, host string) ([]*CardRun, error) {
	q := cardRunSelectCols + ` WHERE status='running'`
	var args []any
	if host != "" {
		q += ` AND host=?`
		args = append(args, host)
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CardRun
	for rows.Next() {
		r, err := scanCardRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountRunningForBoard returns the number of card_runs in status='running'
// for a given pair_key on this host. Used by the standing drainer to
// compute capacity deficit.
func (s *SQLiteLocal) CountRunningForBoard(ctx context.Context, pairKey, host string) (int, error) {
	if host == "" {
		host = SelfHost()
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM card_runs
		 WHERE pair_key=? AND host=? AND status='running'
	`, pairKey, host).Scan(&n)
	return n, err
}

// HasRunningRunForCard returns true when a running row already exists
// for this card. The standing drainer uses this to skip cards already
// being worked.
func (s *SQLiteLocal) HasRunningRunForCard(ctx context.Context, cardID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM card_runs
		 WHERE card_id=? AND status='running'
	`, cardID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

const cardRunSelectCols = `
SELECT id, card_id, pair_key, host, worker_label,
       COALESCE(pid, 0), started_at, COALESCE(ended_at, ''),
       status, COALESCE(exit_code, 0), COALESCE(log_path, ''), trigger
  FROM card_runs`

func scanCardRun(r rowScanner) (*CardRun, error) {
	var c CardRun
	if err := r.Scan(
		&c.ID, &c.CardID, &c.PairKey, &c.Host, &c.WorkerLabel,
		&c.PID, &c.StartedAt, &c.EndedAt,
		&c.Status, &c.ExitCode, &c.LogPath, &c.Trigger,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// --- board_settings -----------------------------------------------------

// BoardSettings mirrors one row of the board_settings table.
type BoardSettings struct {
	PairKey          string
	AutoDrain        bool
	MaxConcurrent    int
	AutoPromote      bool
	PollIntervalSecs int
	UpdatedAt        string
	UpdatedBy        string
}

// DefaultBoardSettings returns the row a board has when no row has been
// written yet. Caller is responsible for stamping PairKey.
func DefaultBoardSettings() BoardSettings {
	return BoardSettings{
		AutoDrain:        false,
		MaxConcurrent:    3,
		AutoPromote:      false,
		PollIntervalSecs: 5,
	}
}

// GetBoardSettings returns the row for pair_key, or DefaultBoardSettings
// stamped with that key if the row is absent. Never returns sql.ErrNoRows.
func (s *SQLiteLocal) GetBoardSettings(ctx context.Context, pairKey string) (BoardSettings, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT pair_key, auto_drain, max_concurrent, auto_promote,
		       poll_interval_secs, updated_at, updated_by
		  FROM board_settings WHERE pair_key=?
	`, pairKey)
	var b BoardSettings
	var ad, ap int
	err := row.Scan(&b.PairKey, &ad, &b.MaxConcurrent, &ap,
		&b.PollIntervalSecs, &b.UpdatedAt, &b.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		def := DefaultBoardSettings()
		def.PairKey = pairKey
		return def, nil
	}
	if err != nil {
		return BoardSettings{}, err
	}
	b.AutoDrain = ad != 0
	b.AutoPromote = ap != 0
	return b, nil
}

// ListBoardSettingsAutoDrain returns every row with auto_drain=1. Used
// at peer-web boot to spawn a drainer per opted-in board.
func (s *SQLiteLocal) ListBoardSettingsAutoDrain(ctx context.Context) ([]BoardSettings, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pair_key, auto_drain, max_concurrent, auto_promote,
		       poll_interval_secs, updated_at, updated_by
		  FROM board_settings WHERE auto_drain=1 ORDER BY pair_key
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardSettings
	for rows.Next() {
		var b BoardSettings
		var ad, ap int
		if err := rows.Scan(&b.PairKey, &ad, &b.MaxConcurrent, &ap,
			&b.PollIntervalSecs, &b.UpdatedAt, &b.UpdatedBy); err != nil {
			return nil, err
		}
		b.AutoDrain = ad != 0
		b.AutoPromote = ap != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpsertBoardSettings writes the row, replacing on conflict. Caller-set
// timestamps + updated_by stamp who changed the row (UI passes "owner",
// CLI passes the calling label, etc).
func (s *SQLiteLocal) UpsertBoardSettings(ctx context.Context, b BoardSettings) error {
	if b.PairKey == "" {
		return fmt.Errorf("UpsertBoardSettings: pair_key required")
	}
	if b.MaxConcurrent < 1 {
		return fmt.Errorf("UpsertBoardSettings: max_concurrent must be >= 1, got %d", b.MaxConcurrent)
	}
	if b.PollIntervalSecs < 1 {
		return fmt.Errorf("UpsertBoardSettings: poll_interval_secs must be >= 1, got %d", b.PollIntervalSecs)
	}
	if b.UpdatedBy == "" {
		b.UpdatedBy = "unknown"
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO board_settings (
		  pair_key, auto_drain, max_concurrent, auto_promote,
		  poll_interval_secs, updated_at, updated_by
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pair_key) DO UPDATE SET
		  auto_drain=excluded.auto_drain,
		  max_concurrent=excluded.max_concurrent,
		  auto_promote=excluded.auto_promote,
		  poll_interval_secs=excluded.poll_interval_secs,
		  updated_at=excluded.updated_at,
		  updated_by=excluded.updated_by
	`,
		b.PairKey, boolToInt(b.AutoDrain), b.MaxConcurrent,
		boolToInt(b.AutoPromote), b.PollIntervalSecs, now, b.UpdatedBy,
	)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
