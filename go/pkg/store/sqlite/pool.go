package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrPoolMemberNotFound — (pair_key, agent_id) lookup missed.
	ErrPoolMemberNotFound = errors.New("store: pool member not found")
)

// PoolMember mirrors one row of the pool_members table joined with the
// agent it points at. The agent fields are populated for List/Get
// returns so callers don't need a second round-trip.
type PoolMember struct {
	PairKey  string
	AgentID  int64
	Count    int
	Priority int
	AddedBy  string
	AddedAt  string

	// Joined from agents — read-only for callers; updates go through
	// the agents store layer.
	AgentLabel   string
	AgentRuntime string
	AgentRole    string // empty = no role filter
	AgentEnabled bool
}

// AddPoolMemberParams — explicit struct to keep the call site readable.
type AddPoolMemberParams struct {
	PairKey  string
	AgentID  int64
	Count    int    // 0 → defaults to 1
	Priority int    // 0 default
	AddedBy  string // optional; falls back to "owner" when empty
}

// UpdatePoolMemberParams — partial update via pointer fields.
type UpdatePoolMemberParams struct {
	Count    *int
	Priority *int
}

// AddPoolMember inserts a (pair_key, agent_id) row. Returns
// ErrAgentNotFound if the agent_id doesn't exist (FK enforces).
// Inserting a duplicate (pair_key, agent_id) returns an error
// surface that matches CRUD ergonomics — caller should check existing
// membership before adding.
func (s *SQLiteLocal) AddPoolMember(ctx context.Context, p AddPoolMemberParams) (*PoolMember, error) {
	if p.PairKey == "" {
		return nil, fmt.Errorf("AddPoolMember: pair_key required")
	}
	if p.AgentID == 0 {
		return nil, fmt.Errorf("AddPoolMember: agent_id required")
	}
	count := p.Count
	if count < 1 {
		count = 1
	}
	addedBy := p.AddedBy
	if addedBy == "" {
		addedBy = "owner"
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pool_members (pair_key, agent_id, count, priority, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.PairKey, p.AgentID, count, p.Priority, nullableString(addedBy), now)
	if err != nil {
		// FOREIGN KEY constraint surfaces as a generic constraint error;
		// give callers something actionable.
		if isSQLiteFKErr(err) {
			return nil, ErrAgentNotFound
		}
		return nil, fmt.Errorf("insert pool_members: %w", err)
	}
	return s.GetPoolMember(ctx, p.PairKey, p.AgentID)
}

// RemovePoolMember removes one (pair_key, agent_id) row. Returns
// ErrPoolMemberNotFound when no row matched.
func (s *SQLiteLocal) RemovePoolMember(ctx context.Context, pairKey string, agentID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pool_members WHERE pair_key=? AND agent_id=?`, pairKey, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPoolMemberNotFound
	}
	return nil
}

// GetPoolMember returns the membership joined with the agent.
func (s *SQLiteLocal) GetPoolMember(ctx context.Context, pairKey string, agentID int64) (*PoolMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pm.pair_key, pm.agent_id, pm.count, pm.priority,
		       COALESCE(pm.added_by, ''), pm.added_at,
		       a.label, a.runtime, COALESCE(a.role, ''), a.enabled
		  FROM pool_members pm
		  JOIN agents a ON a.id = pm.agent_id
		 WHERE pm.pair_key = ? AND pm.agent_id = ?
	`, pairKey, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrPoolMemberNotFound
	}
	return scanPoolMember(rows)
}

// ListPoolMembers returns the roster for a board, ordered by priority
// DESC then agent label. Always returns joined agent fields.
func (s *SQLiteLocal) ListPoolMembers(ctx context.Context, pairKey string) ([]PoolMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pm.pair_key, pm.agent_id, pm.count, pm.priority,
		       COALESCE(pm.added_by, ''), pm.added_at,
		       a.label, a.runtime, COALESCE(a.role, ''), a.enabled
		  FROM pool_members pm
		  JOIN agents a ON a.id = pm.agent_id
		 WHERE pm.pair_key = ?
		 ORDER BY pm.priority DESC, a.label ASC
	`, pairKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PoolMember
	for rows.Next() {
		m, err := scanPoolMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// UpdatePoolMember applies partial updates (count/priority) to an
// existing membership. Returns ErrPoolMemberNotFound if no row matches.
func (s *SQLiteLocal) UpdatePoolMember(ctx context.Context, pairKey string, agentID int64, p UpdatePoolMemberParams) error {
	var (
		sets []string
		args []any
	)
	if p.Count != nil {
		if *p.Count < 1 {
			return fmt.Errorf("UpdatePoolMember: count must be >= 1, got %d", *p.Count)
		}
		sets = append(sets, "count=?")
		args = append(args, *p.Count)
	}
	if p.Priority != nil {
		sets = append(sets, "priority=?")
		args = append(args, *p.Priority)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, pairKey, agentID)
	res, err := s.db.ExecContext(ctx,
		`UPDATE pool_members SET `+strings.Join(sets, ", ")+` WHERE pair_key=? AND agent_id=?`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPoolMemberNotFound
	}
	return nil
}

// CountPoolMembers returns the total slot count (sum of count across
// members) for a board's pool, plus the row count.
func (s *SQLiteLocal) CountPoolMembers(ctx context.Context, pairKey string) (rowCount, totalSlots int, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(count), 0) FROM pool_members WHERE pair_key=?`,
		pairKey)
	err = row.Scan(&rowCount, &totalSlots)
	return
}

// scanPoolMember unmarshals one joined row.
func scanPoolMember(rows *sql.Rows) (*PoolMember, error) {
	var (
		m          PoolMember
		enabledInt int
	)
	if err := rows.Scan(
		&m.PairKey, &m.AgentID, &m.Count, &m.Priority,
		&m.AddedBy, &m.AddedAt,
		&m.AgentLabel, &m.AgentRuntime, &m.AgentRole, &enabledInt,
	); err != nil {
		return nil, err
	}
	m.AgentEnabled = enabledInt != 0
	return &m, nil
}

// isSQLiteFKErr returns true for FOREIGN KEY constraint violations.
func isSQLiteFKErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "FOREIGN KEY constraint failed") ||
		strings.Contains(msg, "constraint failed: FOREIGN KEY")
}

// PickAgentForCard chooses which agent should drain a card (v3.12.4).
//
// Selection rules:
//
//  1. If card.assigned_to_agent_id is set and the agent is enabled,
//     return that agent (designated wins). If the designated agent
//     is disabled or deleted, fall through to auto-selection so the
//     card isn't deadlocked on a missing agent.
//  2. Otherwise consider enabled pool members of the card's pair_key.
//     A member is eligible iff:
//        - card.needs_role == "" (any agent matches), OR
//        - agent.role == card.needs_role, OR
//        - agent.role == "*" (declared willing-to-work-on-anything).
//  3. Among eligible members, rank by:
//        - running_count_for_agent ASC  (least-busy first; respects count slots)
//        - priority DESC                (operator tiebreak)
//        - agent_id ASC                 (deterministic last resort)
//     Members where running_count >= count (slot-saturated) are dropped.
//  4. If no eligible member remains, return (nil, nil) — caller falls
//     back to AGENT_COLLAB_WORKER_CMD (Phase 1 behavior preserved).
//
// running_count is computed from card_runs WHERE status='running' AND
// agent_id matches; null agent_id (legacy / fallback rows) is ignored.
func (s *SQLiteLocal) PickAgentForCard(ctx context.Context, card *Card) (*Agent, error) {
	if card == nil {
		return nil, fmt.Errorf("PickAgentForCard: card required")
	}

	// 1. Designated assignment wins when present + enabled.
	if card.AssignedToAgentID != 0 {
		a, err := s.GetAgent(ctx, card.AssignedToAgentID)
		if err != nil {
			if errors.Is(err, ErrAgentNotFound) {
				// Fall through to auto-select.
			} else {
				return nil, err
			}
		} else if a.Enabled {
			return a, nil
		}
		// Disabled or missing → fall through.
	}

	// 2-4. Auto-select from pool.
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.label, a.runtime,
		       COALESCE(a.worker_cmd, ''), COALESCE(a.role, ''),
		       COALESCE(a.model_config, ''), a.enabled,
		       a.created_at, a.updated_at,
		       pm.count, pm.priority,
		       (SELECT COUNT(*) FROM card_runs cr
		         WHERE cr.agent_id = a.id AND cr.status = 'running')
		         AS running_count
		  FROM pool_members pm
		  JOIN agents a ON a.id = pm.agent_id
		 WHERE pm.pair_key = ?
		   AND a.enabled = 1
		   AND (
		     ? = '' OR a.role IS NULL OR a.role = '' OR a.role = ? OR a.role = '*'
		   )
	`, card.PairKey, card.NeedsRole, card.NeedsRole)
	if err != nil {
		return nil, fmt.Errorf("PickAgentForCard: query pool: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		agent        Agent
		count        int
		priority     int
		runningCount int
	}
	var pool []candidate
	for rows.Next() {
		var (
			c          candidate
			workerCmd  string
			role       string
			modelCfg   string
			enabledInt int
		)
		if err := rows.Scan(
			&c.agent.ID, &c.agent.Label, &c.agent.Runtime,
			&workerCmd, &role, &modelCfg, &enabledInt,
			&c.agent.CreatedAt, &c.agent.UpdatedAt,
			&c.count, &c.priority, &c.runningCount,
		); err != nil {
			return nil, err
		}
		c.agent.WorkerCmd = workerCmd
		c.agent.Role = role
		c.agent.ModelConfig = modelCfg
		c.agent.Enabled = enabledInt != 0
		// Drop slot-saturated agents.
		if c.runningCount >= c.count {
			continue
		}
		// Filter NULL-role members when needs_role is set: NULL/"" role
		// matches *any* card. The SQL OR-clause already lets them through;
		// keep the matching strict to avoid surprising the operator.
		if card.NeedsRole != "" && c.agent.Role != "" && c.agent.Role != "*" && c.agent.Role != card.NeedsRole {
			continue
		}
		pool = append(pool, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}

	// Rank and pick the head.
	best := pool[0]
	for _, c := range pool[1:] {
		switch {
		case c.runningCount < best.runningCount:
			best = c
		case c.runningCount == best.runningCount && c.priority > best.priority:
			best = c
		case c.runningCount == best.runningCount && c.priority == best.priority && c.agent.ID < best.agent.ID:
			best = c
		}
	}
	return &best.agent, nil
}
