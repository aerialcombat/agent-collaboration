package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Card statuses (v3.9). The schema CHECK constraint also enforces this
// enum at the DB layer, but the constants avoid string-literal drift
// between the Go, Python, and MCP layers.
const (
	CardStatusTodo       = "todo"
	CardStatusInProgress = "in_progress"
	CardStatusInReview   = "in_review"
	CardStatusDone       = "done"
	CardStatusCancelled  = "cancelled"
)

// ValidCardStatuses enumerates legal status values — used by CLI flag
// validation + state-transition probe.
var ValidCardStatuses = []string{
	CardStatusTodo, CardStatusInProgress, CardStatusInReview,
	CardStatusDone, CardStatusCancelled,
}

var (
	// ErrCardNotFound — card_id lookup missed.
	ErrCardNotFound = errors.New("store: card not found")

	// ErrCardAlreadyClaimed — claim attempt against a card another
	// label is holding. The caller has to decide whether to wait for
	// release, pick a different card, or force-claim (future flag).
	ErrCardAlreadyClaimed = errors.New("store: card already claimed by another label")

	// ErrCardInvalidStatus — status string not in ValidCardStatuses.
	ErrCardInvalidStatus = errors.New("store: invalid card status")

	// ErrCardDependencyCycle — adding this edge would produce a cycle
	// in the card_dependencies graph. Rejected because a cycle
	// deadlocks the queue (every card waits forever for itself).
	ErrCardDependencyCycle = errors.New("store: card dependency would create a cycle")

	// ErrCardDependencySelf — blocker == blockee. The DB CHECK
	// constraint also rejects this but we surface a friendly error
	// before the round-trip.
	ErrCardDependencySelf = errors.New("store: card cannot block itself")
)

// Card mirrors one row of the cards table plus derived flags that the
// DB layer computes on read (blocker IDs, ready-to-claim). Keeping it
// in one struct lets the CLI/MCP surface return the same shape without
// re-JOINing for derived props on every caller.
type Card struct {
	ID           int64
	PairKey      string
	HomeHost     string
	Title        string
	Body         string
	Status       string
	NeedsRole    string
	ClaimedBy    string
	CreatedBy    string
	Priority     int
	Tags         string // raw JSON; callers can marshal on demand
	ContextRefs  string // raw JSON
	CreatedAt    string
	UpdatedAt    string
	ClaimedAt    string
	CompletedAt  string
	BlockerIDs   []int64 // cards that must reach done/cancelled before this is ready
	BlockeeIDs   []int64 // cards waiting on this one
	Ready        bool    // status=todo AND no unresolved blockers
}

// CardListFilter is the predicate set for list queries. Zero-value
// (all fields unset) lists every card in the DB — callers almost
// always scope by pair_key.
type CardListFilter struct {
	PairKey      string
	Status       string
	NeedsRole    string
	ClaimedBy    string
	ReadyOnly    bool   // status=todo AND no pending blockers
	BlockedOnly  bool   // status=todo AND >=1 pending blocker
	Limit        int    // 0 = no limit
}

// CreateCardParams — explicit struct so the CLI can pass a partial
// record without growing a 10-arg function signature.
type CreateCardParams struct {
	PairKey     string
	HomeHost    string
	Title       string
	Body        string
	NeedsRole   string
	CreatedBy   string
	Priority    int
	Tags        string // JSON (pre-marshaled by caller) or ""
	ContextRefs string // JSON or ""
}

// CreateCard inserts a new card in state=todo and returns the row
// back to the caller with its generated ID + timestamps. home_host
// defaults to SelfHost() when unset so federation routing is correct
// even when a caller forgets to stamp it.
func (s *SQLiteLocal) CreateCard(ctx context.Context, p CreateCardParams) (*Card, error) {
	if p.PairKey == "" || p.Title == "" || p.CreatedBy == "" {
		return nil, fmt.Errorf("CreateCard: pair_key, title, created_by required")
	}
	if p.HomeHost == "" {
		p.HomeHost = SelfHost()
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO cards
		  (pair_key, home_host, title, body, status, needs_role,
		   created_by, priority, tags, context_refs,
		   created_at, updated_at)
		VALUES (?, ?, ?, ?, 'todo', ?, ?, ?, ?, ?, ?, ?)`,
		p.PairKey, p.HomeHost, p.Title, nullable(p.Body),
		nullable(p.NeedsRole), p.CreatedBy, p.Priority,
		nullable(p.Tags), nullable(p.ContextRefs),
		now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateCard insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("CreateCard lastid: %w", err)
	}
	return s.GetCard(ctx, id)
}

// GetCard returns one card plus its blocker/blockee edges + derived
// Ready flag. ErrCardNotFound when no row matches.
func (s *SQLiteLocal) GetCard(ctx context.Context, id int64) (*Card, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, pair_key, home_host, title, body, status,
		       needs_role, claimed_by, created_by, priority,
		       tags, context_refs,
		       created_at, updated_at, claimed_at, completed_at
		FROM cards WHERE id = ?`, id)
	c, err := scanCard(row)
	if err == sql.ErrNoRows {
		return nil, ErrCardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetCard: %w", err)
	}
	if err := s.loadCardEdges(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ListCards applies filter and returns a slice of cards with
// Blocker/Blockee IDs + Ready computed. Ordered by priority DESC,
// created_at ASC so "urgent & oldest" rises to the top.
func (s *SQLiteLocal) ListCards(ctx context.Context, f CardListFilter) ([]*Card, error) {
	var conds []string
	var args []any
	if f.PairKey != "" {
		conds = append(conds, "pair_key = ?")
		args = append(args, f.PairKey)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.NeedsRole != "" {
		conds = append(conds, "needs_role = ?")
		args = append(args, f.NeedsRole)
	}
	if f.ClaimedBy != "" {
		conds = append(conds, "claimed_by = ?")
		args = append(args, f.ClaimedBy)
	}

	// Ready / Blocked filters are mutually exclusive; both imply
	// status=todo. We layer them on top of any explicit Status the
	// caller passes by short-circuiting conflict.
	if f.ReadyOnly && f.BlockedOnly {
		return nil, fmt.Errorf("ListCards: ready_only and blocked_only are mutually exclusive")
	}
	if f.ReadyOnly || f.BlockedOnly {
		conds = append(conds, "status = 'todo'")
		// NOT EXISTS blocker-with-open-status is the "ready" half;
		// EXISTS blocker-with-open-status is "blocked". Blocker is
		// "open" if it's NOT in (done, cancelled).
		sub := `EXISTS (
			SELECT 1 FROM card_dependencies d
			JOIN cards b ON b.id = d.blocker_id
			WHERE d.blockee_id = cards.id
			  AND b.status NOT IN ('done','cancelled')
		)`
		if f.ReadyOnly {
			conds = append(conds, "NOT "+sub)
		} else {
			conds = append(conds, sub)
		}
	}

	q := `SELECT id, pair_key, home_host, title, body, status,
	             needs_role, claimed_by, created_by, priority,
	             tags, context_refs,
	             created_at, updated_at, claimed_at, completed_at
	      FROM cards`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY priority DESC, created_at ASC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rs, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListCards query: %w", err)
	}
	defer rs.Close()

	var out []*Card
	for rs.Next() {
		c, err := scanCard(rs)
		if err != nil {
			return nil, fmt.Errorf("ListCards scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	// Edges are per-card; one query per card is fine at PoC scale.
	// If list pages ever exceed a few hundred rows, batch with a
	// single IN-query here.
	for _, c := range out {
		if err := s.loadCardEdges(ctx, c); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ClaimCard atomically sets claimed_by + claimed_at + status=in_progress
// for a todo-state card. Re-claims by the same label are idempotent.
// ErrCardAlreadyClaimed when a different label holds it and force=false.
func (s *SQLiteLocal) ClaimCard(ctx context.Context, id int64, label string, force bool) (*Card, error) {
	if label == "" {
		return nil, fmt.Errorf("ClaimCard: label required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ClaimCard begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		currentClaim sql.NullString
		currentStatus string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT claimed_by, status FROM cards WHERE id = ?`, id,
	).Scan(&currentClaim, &currentStatus)
	if err == sql.ErrNoRows {
		return nil, ErrCardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ClaimCard probe: %w", err)
	}

	if currentClaim.Valid && currentClaim.String != "" &&
		currentClaim.String != label && !force {
		return nil, fmt.Errorf("%w: card %d held by %q",
			ErrCardAlreadyClaimed, id, currentClaim.String)
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	// Only bump status to in_progress when we're transitioning OUT of
	// todo. If a card was already in_progress (idempotent re-claim by
	// the same owner), keep its state. in_review → claim → stays
	// in_review (someone re-grabbed a reviewed card — let status be
	// changed explicitly via update-status, not implicitly via claim).
	newStatus := currentStatus
	if currentStatus == CardStatusTodo {
		newStatus = CardStatusInProgress
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cards
		   SET claimed_by = ?, claimed_at = COALESCE(claimed_at, ?),
		       status = ?, updated_at = ?
		 WHERE id = ?`,
		label, now, newStatus, now, id,
	); err != nil {
		return nil, fmt.Errorf("ClaimCard update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ClaimCard commit: %w", err)
	}
	return s.GetCard(ctx, id)
}

// UpdateCardStatus validates the new status against the enum and
// writes it. Sets completed_at on the first transition to a terminal
// state (done/cancelled). Leaves claimed_by alone — call ClaimCard
// or a future UnclaimCard for that.
func (s *SQLiteLocal) UpdateCardStatus(ctx context.Context, id int64, status string) (*Card, error) {
	if !validCardStatus(status) {
		return nil, fmt.Errorf("%w: %q (expected one of %v)",
			ErrCardInvalidStatus, status, ValidCardStatuses)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	var completedSet string
	if status == CardStatusDone || status == CardStatusCancelled {
		completedSet = ", completed_at = COALESCE(completed_at, ?)"
	}

	var (
		res sql.Result
		err error
	)
	if completedSet != "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE cards SET status = ?, updated_at = ?`+completedSet+` WHERE id = ?`,
			status, now, now, id,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE cards SET status = ?, updated_at = ? WHERE id = ?`,
			status, now, id,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("UpdateCardStatus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("UpdateCardStatus rows: %w", err)
	}
	if n == 0 {
		return nil, ErrCardNotFound
	}
	return s.GetCard(ctx, id)
}

// AddCardDependency adds an edge blocker -> blockee after validating
// (a) neither endpoint equals the other, (b) both cards exist,
// (c) the new edge doesn't create a cycle. The cycle check walks the
// transitive blocker closure of `blocker` and rejects if `blockee`
// is in it (that would mean blockee is already upstream of blocker,
// so adding blocker→blockee closes the loop).
func (s *SQLiteLocal) AddCardDependency(ctx context.Context, blockerID, blockeeID int64, createdBy string) error {
	if blockerID == blockeeID {
		return ErrCardDependencySelf
	}
	if createdBy == "" {
		return fmt.Errorf("AddCardDependency: created_by required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AddCardDependency begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Existence check for both cards — FK will catch it too but the
	// friendlier error is worth the round-trip.
	if err := cardExists(ctx, tx, blockerID); err != nil {
		return fmt.Errorf("AddCardDependency blocker: %w", err)
	}
	if err := cardExists(ctx, tx, blockeeID); err != nil {
		return fmt.Errorf("AddCardDependency blockee: %w", err)
	}

	// Cycle detection: recursive CTE walks upstream blockers of
	// `blocker`. If `blockee` appears in that set, the new edge
	// closes a loop.
	var exists int
	err = tx.QueryRowContext(ctx, `
		WITH RECURSIVE upstream(id) AS (
		    SELECT blocker_id FROM card_dependencies WHERE blockee_id = ?
		    UNION
		    SELECT d.blocker_id FROM card_dependencies d
		    JOIN upstream u ON d.blockee_id = u.id
		)
		SELECT COUNT(*) FROM upstream WHERE id = ?`,
		blockerID, blockeeID,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("AddCardDependency cycle-probe: %w", err)
	}
	if exists > 0 {
		return fmt.Errorf("%w: %d→%d would close a loop",
			ErrCardDependencyCycle, blockerID, blockeeID)
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err = tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO card_dependencies
		  (blocker_id, blockee_id, created_at, created_by)
		VALUES (?, ?, ?, ?)`,
		blockerID, blockeeID, now, createdBy,
	)
	if err != nil {
		return fmt.Errorf("AddCardDependency insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AddCardDependency commit: %w", err)
	}
	return nil
}

// RemoveCardDependency drops the (blocker, blockee) edge. Returns
// whether a row was affected (false = edge didn't exist).
func (s *SQLiteLocal) RemoveCardDependency(ctx context.Context, blockerID, blockeeID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM card_dependencies WHERE blocker_id = ? AND blockee_id = ?`,
		blockerID, blockeeID,
	)
	if err != nil {
		return false, fmt.Errorf("RemoveCardDependency: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("RemoveCardDependency rows: %w", err)
	}
	return n > 0, nil
}

// --- helpers ----------------------------------------------------------------

func validCardStatus(s string) bool {
	for _, v := range ValidCardStatuses {
		if s == v {
			return true
		}
	}
	return false
}

func cardExists(ctx context.Context, tx *sql.Tx, id int64) error {
	var probe int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM cards WHERE id = ?`, id).Scan(&probe)
	if err == sql.ErrNoRows {
		return ErrCardNotFound
	}
	return err
}

// scanCard works for both *sql.Row and *sql.Rows — the SELECT list is
// identical for GetCard and ListCards, so we DRY through a type-agnostic
// scanner.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCard(r rowScanner) (*Card, error) {
	var (
		c                        Card
		body, needsRole          sql.NullString
		claimedBy, tags          sql.NullString
		ctxRefs, claimedAt       sql.NullString
		completedAt              sql.NullString
	)
	if err := r.Scan(
		&c.ID, &c.PairKey, &c.HomeHost, &c.Title, &body, &c.Status,
		&needsRole, &claimedBy, &c.CreatedBy, &c.Priority,
		&tags, &ctxRefs,
		&c.CreatedAt, &c.UpdatedAt, &claimedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	c.Body = nullString(body)
	c.NeedsRole = nullString(needsRole)
	c.ClaimedBy = nullString(claimedBy)
	c.Tags = nullString(tags)
	c.ContextRefs = nullString(ctxRefs)
	c.ClaimedAt = nullString(claimedAt)
	c.CompletedAt = nullString(completedAt)
	return &c, nil
}

// loadCardEdges fills BlockerIDs, BlockeeIDs, and derives Ready. Runs
// as a separate query to keep scanCard identical for Get and List.
func (s *SQLiteLocal) loadCardEdges(ctx context.Context, c *Card) error {
	rs1, err := s.db.QueryContext(ctx,
		`SELECT blocker_id FROM card_dependencies WHERE blockee_id = ?`, c.ID)
	if err != nil {
		return fmt.Errorf("loadCardEdges blockers: %w", err)
	}
	defer rs1.Close()
	for rs1.Next() {
		var id int64
		if err := rs1.Scan(&id); err != nil {
			return err
		}
		c.BlockerIDs = append(c.BlockerIDs, id)
	}

	rs2, err := s.db.QueryContext(ctx,
		`SELECT blockee_id FROM card_dependencies WHERE blocker_id = ?`, c.ID)
	if err != nil {
		return fmt.Errorf("loadCardEdges blockees: %w", err)
	}
	defer rs2.Close()
	for rs2.Next() {
		var id int64
		if err := rs2.Scan(&id); err != nil {
			return err
		}
		c.BlockeeIDs = append(c.BlockeeIDs, id)
	}

	// Ready iff status=todo AND no blocker is still open.
	if c.Status != CardStatusTodo {
		c.Ready = false
		return nil
	}
	if len(c.BlockerIDs) == 0 {
		c.Ready = true
		return nil
	}
	var open int
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM card_dependencies d
		JOIN cards b ON b.id = d.blocker_id
		WHERE d.blockee_id = ?
		  AND b.status NOT IN ('done','cancelled')`,
		c.ID,
	).Scan(&open)
	if err != nil {
		return fmt.Errorf("loadCardEdges readiness: %w", err)
	}
	c.Ready = open == 0
	return nil
}
