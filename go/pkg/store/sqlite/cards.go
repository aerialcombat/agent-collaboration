package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

	// v3.12 — designated agent assignment. 0 = use auto-select.
	AssignedToAgentID int64
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
		       created_at, updated_at, claimed_at, completed_at,
		       assigned_to_agent_id
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
	             created_at, updated_at, claimed_at, completed_at,
	             assigned_to_agent_id
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

	priorClaim := ""
	if currentClaim.Valid {
		priorClaim = currentClaim.String
	}
	meta, _ := jsonObject(map[string]any{
		"prior_claim":   priorClaim,
		"prior_status":  currentStatus,
		"new_status":    newStatus,
		"force":         force,
	})
	body := "claimed by " + label
	if priorClaim != "" && priorClaim != label {
		body = fmt.Sprintf("re-claimed from %s to %s", priorClaim, label)
	}
	s.recordEvent(ctx, AppendCardEventParams{
		CardID: id, Kind: CardEventClaim,
		Author: label,
		Body:   body,
		Meta:   meta,
	})

	return s.GetCard(ctx, id)
}

// UpdateCardStatus validates the new status against the enum and
// writes it. Sets completed_at on the first transition to a terminal
// state (done/cancelled). Leaves claimed_by alone — call ClaimCard
// or a future UnclaimCard for that. `author` is the label that gets
// recorded against the auto-emitted status_change event; pass "" to
// fall back to "system".
func (s *SQLiteLocal) UpdateCardStatus(ctx context.Context, id int64, status, author string) (*Card, error) {
	if !validCardStatus(status) {
		return nil, fmt.Errorf("%w: %q (expected one of %v)",
			ErrCardInvalidStatus, status, ValidCardStatuses)
	}
	// Snapshot prior status so the event can record the transition.
	prior, err := s.GetCard(ctx, id)
	if errors.Is(err, ErrCardNotFound) {
		return nil, ErrCardNotFound
	}
	if err != nil {
		return nil, err
	}
	if prior.Status == status {
		// No-op transition; don't record an event for status N → N.
		return prior, nil
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	var completedSet string
	if status == CardStatusDone || status == CardStatusCancelled {
		completedSet = ", completed_at = COALESCE(completed_at, ?)"
	}

	var res sql.Result
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

	// Auto-record the transition. Failures don't bubble (recordEvent logs to stderr).
	meta, _ := jsonObject(map[string]any{
		"old_status": prior.Status,
		"new_status": status,
	})
	s.recordEvent(ctx, AppendCardEventParams{
		CardID: id, Kind: CardEventStatusChange,
		Author: author,
		Body:   fmt.Sprintf("%s → %s", prior.Status, status),
		Meta:   meta,
	})

	return s.GetCard(ctx, id)
}

// jsonObject is a tiny convenience wrapper around json.Marshal so the
// auto-record callsites read cleanly (the err is discardable — meta is
// always a flat map of primitives).
func jsonObject(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

// Card event kinds — Linear/GitHub-style timeline. Comments are
// human/worker-authored; the rest are system-generated on state
// transitions and recorded automatically by the corresponding mutator.
const (
	CardEventComment      = "comment"
	CardEventStatusChange = "status_change"
	CardEventClaim        = "claim"
	CardEventBodyUpdate   = "body_update"
	CardEventRunDispatch  = "run_dispatch"
	CardEventRunComplete  = "run_complete"
	CardEventAssigned     = "assigned"   // v3.12.4 — designated agent set
	CardEventUnassigned   = "unassigned" // v3.12.4 — designation cleared
)

// CardEvent — one row in the per-card timeline.
type CardEvent struct {
	ID        int64
	CardID    int64
	Kind      string
	Author    string
	Body      string
	Meta      string // raw JSON; renderer parses per-kind
	CreatedAt string
}

// AssignCardToAgent sets cards.assigned_to_agent_id to the given
// agent's id and records an "assignment" event. agentID=0 clears the
// designation. Returns the updated card.
//
// Used by v3.12.4's drainer when the operator wants to force a
// specific agent regardless of role / load. The store doesn't validate
// that the agent is in the board's pool — designation can override
// pool scope.
func (s *SQLiteLocal) AssignCardToAgent(ctx context.Context, cardID, agentID int64, author string) (*Card, error) {
	if author == "" {
		author = "system"
	}
	prior, err := s.GetCard(ctx, cardID)
	if err != nil {
		return nil, err
	}
	if prior.AssignedToAgentID == agentID {
		return prior, nil // no-op
	}

	var res sql.Result
	if agentID == 0 {
		res, err = s.db.ExecContext(ctx,
			`UPDATE cards SET assigned_to_agent_id = NULL, updated_at = ? WHERE id = ?`,
			time.Now().UTC().Format("2006-01-02T15:04:05Z"), cardID)
	} else {
		// Verify the agent exists; FK would catch it, but a clean error
		// is friendlier than a generic constraint violation.
		if _, err := s.GetAgent(ctx, agentID); err != nil {
			return nil, err
		}
		res, err = s.db.ExecContext(ctx,
			`UPDATE cards SET assigned_to_agent_id = ?, updated_at = ? WHERE id = ?`,
			agentID, time.Now().UTC().Format("2006-01-02T15:04:05Z"), cardID)
	}
	if err != nil {
		return nil, fmt.Errorf("AssignCardToAgent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrCardNotFound
	}

	// Audit event.
	kind := CardEventAssigned
	body := ""
	if agentID == 0 {
		kind = CardEventUnassigned
	} else {
		// Look up label for human-readable body.
		if a, err := s.GetAgent(ctx, agentID); err == nil {
			body = "agent=" + a.Label
		}
	}
	s.recordEvent(ctx, AppendCardEventParams{
		CardID: cardID, Kind: kind, Author: author, Body: body,
	})

	return s.GetCard(ctx, cardID)
}

// AppendCardEventParams collects insert fields. Meta is raw JSON so
// callers can stash kind-specific data without growing the schema.
type AppendCardEventParams struct {
	CardID int64
	Kind   string
	Author string
	Body   string
	Meta   string // optional; defaults to "{}" if empty
}

// AppendCardEvent inserts one event row. Used both by external callers
// (CLI card-comment / MCP / HTTP) and internally by the auto-record
// hooks in ClaimCard/UpdateCardStatus/UpdateCardFields.
func (s *SQLiteLocal) AppendCardEvent(ctx context.Context, p AppendCardEventParams) (*CardEvent, error) {
	if p.CardID == 0 || p.Kind == "" || p.Author == "" {
		return nil, fmt.Errorf("AppendCardEvent: card_id, kind, author required")
	}
	meta := p.Meta
	if meta == "" {
		meta = "{}"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO card_events (card_id, kind, author, body, meta) VALUES (?, ?, ?, ?, ?)`,
		p.CardID, p.Kind, p.Author, p.Body, meta,
	)
	if err != nil {
		return nil, fmt.Errorf("AppendCardEvent: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("AppendCardEvent: %w", err)
	}
	return s.GetCardEvent(ctx, id)
}

// GetCardEvent fetches one row by id.
func (s *SQLiteLocal) GetCardEvent(ctx context.Context, id int64) (*CardEvent, error) {
	e := &CardEvent{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, card_id, kind, author, body, meta, created_at FROM card_events WHERE id = ?`,
		id,
	).Scan(&e.ID, &e.CardID, &e.Kind, &e.Author, &e.Body, &e.Meta, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrCardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetCardEvent: %w", err)
	}
	return e, nil
}

// ListCardEvents returns the timeline for a card, oldest-first. Pass
// limit=0 for all rows.
func (s *SQLiteLocal) ListCardEvents(ctx context.Context, cardID int64, limit int) ([]*CardEvent, error) {
	q := `SELECT id, card_id, kind, author, body, meta, created_at
	      FROM card_events
	      WHERE card_id = ?
	      ORDER BY created_at ASC, id ASC`
	args := []any{cardID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListCardEvents: %w", err)
	}
	defer rows.Close()
	var out []*CardEvent
	for rows.Next() {
		e := &CardEvent{}
		if err := rows.Scan(&e.ID, &e.CardID, &e.Kind, &e.Author, &e.Body, &e.Meta, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListCardEvents scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// recordEvent is a fire-and-forget event insertion used by the
// auto-record hooks in mutators. Errors are logged via fmt to stderr
// but never bubble up — recording an event must NEVER fail a mutation.
// (You'd rather have a successful claim with no event than no claim.)
func (s *SQLiteLocal) recordEvent(ctx context.Context, p AppendCardEventParams) {
	if p.Author == "" {
		p.Author = "system"
	}
	if _, err := s.AppendCardEvent(ctx, p); err != nil {
		fmt.Fprintf(os.Stderr, "card_events: record failed: %v\n", err)
	}
}

// MessagesByIDs fetches inbox rows by id in arbitrary order. Missing
// ids are silently skipped (the caller can compare lengths to detect).
// Used by context resolution: a card may reference msg_ids in
// context_refs that should be replayed back to a worker as conversation
// history.
func (s *SQLiteLocal) MessagesByIDs(ctx context.Context, ids []int64) ([]WebMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT server_seq, from_label, to_label, body, created_at
	      FROM inbox
	      WHERE server_seq IN (` + placeholders + `)
	      ORDER BY server_seq`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("MessagesByIDs: %w", err)
	}
	defer rows.Close()
	var out []WebMessage
	for rows.Next() {
		var m WebMessage
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Body, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("MessagesByIDs scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateCardFieldsParams collects the partial mutation set. Pointer
// fields distinguish "leave alone" (nil) from "set to zero value"
// (non-nil pointer to empty string / 0). All fields are optional;
// callers must set at least one or the call is a no-op.
type UpdateCardFieldsParams struct {
	Title       *string
	Body        *string
	NeedsRole   *string
	Priority    *int
	Tags        *string // raw JSON
	ContextRefs *string // raw JSON
}

// UpdateCardFields mutates non-status fields on a card and stamps
// updated_at. Status, claim ownership, and dependency edges go through
// their own dedicated verbs. Returns ErrCardNotFound when no row matches.
// `author` is recorded against the auto-emitted body_update event;
// pass "" to fall back to "system".
func (s *SQLiteLocal) UpdateCardFields(ctx context.Context, id int64, p UpdateCardFieldsParams, author string) (*Card, error) {
	sets := []string{}
	args := []any{}
	changed := []string{} // for the event meta — which fields touched
	if p.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *p.Title)
		changed = append(changed, "title")
	}
	if p.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *p.Body)
		changed = append(changed, "body")
	}
	if p.NeedsRole != nil {
		sets = append(sets, "needs_role = ?")
		args = append(args, *p.NeedsRole)
		changed = append(changed, "needs_role")
	}
	if p.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *p.Priority)
		changed = append(changed, "priority")
	}
	if p.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, *p.Tags)
		changed = append(changed, "tags")
	}
	if p.ContextRefs != nil {
		sets = append(sets, "context_refs = ?")
		args = append(args, *p.ContextRefs)
		changed = append(changed, "context_refs")
	}
	if len(sets) == 0 {
		return s.GetCard(ctx, id)
	}
	sets = append(sets, "updated_at = ?")
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	args = append(args, now, id)

	q := "UPDATE cards SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("UpdateCardFields: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("UpdateCardFields rows: %w", err)
	}
	if n == 0 {
		return nil, ErrCardNotFound
	}

	meta, _ := jsonObject(map[string]any{"changed": changed})
	s.recordEvent(ctx, AppendCardEventParams{
		CardID: id, Kind: CardEventBodyUpdate,
		Author: author,
		Body:   "updated " + strings.Join(changed, ", "),
		Meta:   meta,
	})

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

// HookCardsFilter scopes CardsForHook to one room + optional role hint.
// `Label` is required (we always filter the "claimed by me" set on it).
// `Role` may be empty — when it is, the pullable set is empty (role-less
// agents only see their own claimed work). Limit caps each bucket
// independently so a long claim list doesn't crowd out role-pullable
// suggestions.
type HookCardsFilter struct {
	PairKey string
	Label   string
	Role    string
	Limit   int // per-bucket cap; 0 → defaultHookCardsPerBucket
}

const defaultHookCardsPerBucket = 10

// HookCards is the two-bucket result returned to the hook path.
//
//	Claimed  — cards currently claimed_by=me in non-terminal status.
//	           Ordered by status priority (in_progress > in_review > todo),
//	           then priority DESC, then updated_at DESC (most-recently
//	           worked first).
//	Pullable — status=todo, no open blockers, claimed_by IS NULL, and
//	           needs_role matches (if Role set). Ordered by priority
//	           DESC then created_at ASC — highest-priority oldest-first,
//	           the same ordering ListCards uses.
type HookCards struct {
	Claimed  []*Card
	Pullable []*Card
}

// CardsForHook returns the two bundles of cards the pre-prompt hook
// injects into the agent's context: what the agent is on the hook for
// (claimed, active) and what's pullable by role. Scoped to PairKey so
// a cross-room fleet doesn't see another room's board.
//
// Both buckets use the same underlying SELECT list as ListCards +
// loadCardEdges, so derived Ready / BlockerIDs / BlockeeIDs are
// populated the same way.
func (s *SQLiteLocal) CardsForHook(ctx context.Context, f HookCardsFilter) (*HookCards, error) {
	if f.Label == "" || f.PairKey == "" {
		// No identity or no room scope — nothing sensible to return.
		return &HookCards{}, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultHookCardsPerBucket
	}
	out := &HookCards{}

	// --- claimed by me (non-terminal) ---------------------------------------
	//
	// Status priority ordering: in_progress first (active attention), then
	// in_review (waiting on reviewer action), then todo (queued). CASE
	// encodes the precedence; updated_at tiebreaker surfaces freshest.
	claimed, err := s.listCardsRaw(ctx, `
		WHERE pair_key = ?
		  AND claimed_by = ?
		  AND status IN ('in_progress','in_review','todo')
		ORDER BY
		  CASE status
		    WHEN 'in_progress' THEN 0
		    WHEN 'in_review'   THEN 1
		    WHEN 'todo'        THEN 2
		    ELSE 3
		  END,
		  priority DESC,
		  updated_at DESC
		LIMIT ?`,
		f.PairKey, f.Label, limit,
	)
	if err != nil {
		return nil, err
	}
	out.Claimed = claimed

	// --- role-pullable (todo + ready + matching role + unclaimed) -----------
	//
	// We only ship a "pullable" bucket when the caller has a role — role-
	// less agents don't have a queue to drain. Ready = status=todo AND no
	// blocker is still open (NOT IN done/cancelled).
	if f.Role != "" {
		pullable, err := s.listCardsRaw(ctx, `
			WHERE pair_key = ?
			  AND status = 'todo'
			  AND claimed_by IS NULL
			  AND needs_role = ?
			  AND NOT EXISTS (
			    SELECT 1 FROM card_dependencies d
			    JOIN cards b ON b.id = d.blocker_id
			    WHERE d.blockee_id = cards.id
			      AND b.status NOT IN ('done','cancelled')
			  )
			ORDER BY priority DESC, created_at ASC
			LIMIT ?`,
			f.PairKey, f.Role, limit,
		)
		if err != nil {
			return nil, err
		}
		out.Pullable = pullable
	}
	return out, nil
}

// listCardsRaw is the shared SELECT + scan + edge-load loop behind
// ListCards and CardsForHook. Takes a pre-formed WHERE+ORDER+LIMIT
// clause so callers don't have to re-ship the column list.
func (s *SQLiteLocal) listCardsRaw(ctx context.Context, whereAndOrder string, args ...any) ([]*Card, error) {
	q := `SELECT id, pair_key, home_host, title, body, status,
	             needs_role, claimed_by, created_by, priority,
	             tags, context_refs,
	             created_at, updated_at, claimed_at, completed_at,
	             assigned_to_agent_id
	      FROM cards ` + whereAndOrder
	rs, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listCardsRaw query: %w", err)
	}
	defer rs.Close()
	var out []*Card
	for rs.Next() {
		c, err := scanCard(rs)
		if err != nil {
			return nil, fmt.Errorf("listCardsRaw scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		if err := s.loadCardEdges(ctx, c); err != nil {
			return nil, err
		}
	}
	return out, nil
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
		assignedAgentID          sql.NullInt64
	)
	if err := r.Scan(
		&c.ID, &c.PairKey, &c.HomeHost, &c.Title, &body, &c.Status,
		&needsRole, &claimedBy, &c.CreatedBy, &c.Priority,
		&tags, &ctxRefs,
		&c.CreatedAt, &c.UpdatedAt, &claimedAt, &completedAt,
		&assignedAgentID,
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
	if assignedAgentID.Valid {
		c.AssignedToAgentID = assignedAgentID.Int64
	}
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

// CardBoardSummary is one row in the boards-index roll-up: per pair_key,
// total + per-status counts, ready-todo count, distinct claimers, and
// the most recent updated_at across all cards in the board. Used by
// peer-web's /api/boards endpoint to render the boards index without
// fetching every card.
type CardBoardSummary struct {
	PairKey       string
	Total         int
	Todo          int
	InProgress    int
	InReview      int
	Done          int
	Cancelled     int
	ReadyTodo     int
	DistinctRoles int
	Claimers      []string // distinct claimed_by labels (excl. nulls/empty)
	LastUpdatedAt string   // ISO; max(updated_at) across the board's cards
}

// CardBoardSummaries returns one summary row per pair_key that has at
// least one card. Aggregation is one round trip (per-status counts +
// last-updated) plus a second pass for distinct claimers, which keeps
// the query simple and avoids GROUP_CONCAT portability concerns.
//
// Pair keys with zero cards are excluded — a "board" exists only once a
// card has been created in that pair_key.
func (s *SQLiteLocal) CardBoardSummaries(ctx context.Context) ([]*CardBoardSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			c.pair_key,
			COUNT(*),
			SUM(CASE WHEN c.status = 'todo'        THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status = 'in_progress' THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status = 'in_review'   THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status = 'done'        THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status = 'cancelled'   THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status = 'todo' AND NOT EXISTS (
				SELECT 1 FROM card_dependencies d
				JOIN cards b ON b.id = d.blocker_id
				WHERE d.blockee_id = c.id
				  AND b.status NOT IN ('done','cancelled')
			) THEN 1 ELSE 0 END),
			COUNT(DISTINCT NULLIF(c.needs_role, '')),
			MAX(c.updated_at)
		FROM cards c
		GROUP BY c.pair_key
		ORDER BY MAX(c.updated_at) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("CardBoardSummaries: %w", err)
	}
	defer rows.Close()

	var out []*CardBoardSummary
	for rows.Next() {
		b := &CardBoardSummary{}
		if err := rows.Scan(&b.PairKey, &b.Total,
			&b.Todo, &b.InProgress, &b.InReview, &b.Done, &b.Cancelled,
			&b.ReadyTodo, &b.DistinctRoles, &b.LastUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("CardBoardSummaries scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CardBoardSummaries rows: %w", err)
	}

	// Distinct claimers per board, in a single pass.
	if len(out) > 0 {
		claimerRows, err := s.db.QueryContext(ctx, `
			SELECT pair_key, claimed_by
			FROM cards
			WHERE claimed_by IS NOT NULL AND claimed_by <> ''
			GROUP BY pair_key, claimed_by
			ORDER BY pair_key, claimed_by
		`)
		if err != nil {
			return nil, fmt.Errorf("CardBoardSummaries claimers: %w", err)
		}
		defer claimerRows.Close()

		byKey := make(map[string]*CardBoardSummary, len(out))
		for _, b := range out {
			byKey[b.PairKey] = b
		}
		for claimerRows.Next() {
			var pk, who string
			if err := claimerRows.Scan(&pk, &who); err != nil {
				return nil, fmt.Errorf("CardBoardSummaries claimer scan: %w", err)
			}
			if b, ok := byKey[pk]; ok {
				b.Claimers = append(b.Claimers, who)
			}
		}
		if err := claimerRows.Err(); err != nil {
			return nil, fmt.Errorf("CardBoardSummaries claimer rows: %w", err)
		}
	}

	return out, nil
}

