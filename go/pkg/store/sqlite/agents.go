package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Agent runtime values (v3.12). Matches the CHECK constraint on agents.runtime.
const (
	AgentRuntimeClaude = "claude"
	AgentRuntimePi     = "pi"
)

var (
	// ErrAgentNotFound — id or label lookup missed.
	ErrAgentNotFound = errors.New("store: agent not found")
	// ErrAgentLabelTaken — UNIQUE constraint on agents.label.
	ErrAgentLabelTaken = errors.New("store: agent label already exists")
)

// Agent mirrors one row of the agents table.
type Agent struct {
	ID          int64
	Label       string
	Runtime     string
	WorkerCmd   string // empty = use runtime default
	Role        string // empty = no role filter
	ModelConfig string // raw JSON; empty = default
	Enabled     bool
	CreatedAt   string // RFC3339 UTC
	UpdatedAt   string // RFC3339 UTC
}

// CreateAgentParams — explicit struct for inserting a new agent.
type CreateAgentParams struct {
	Label       string
	Runtime     string
	WorkerCmd   string
	Role        string
	ModelConfig string
	Enabled     *bool // nil → defaults to true
}

// UpdateAgentParams — every field is a pointer so callers can update a
// subset. nil = leave unchanged.
type UpdateAgentParams struct {
	Runtime     *string
	WorkerCmd   *string
	Role        *string
	ModelConfig *string
	Enabled     *bool
}

// CreateAgent inserts a new agents row and returns the inserted struct.
// Label collisions return ErrAgentLabelTaken.
func (s *SQLiteLocal) CreateAgent(ctx context.Context, p CreateAgentParams) (*Agent, error) {
	if p.Label == "" {
		return nil, fmt.Errorf("CreateAgent: label required")
	}
	if p.Runtime != AgentRuntimeClaude && p.Runtime != AgentRuntimePi {
		return nil, fmt.Errorf("CreateAgent: runtime must be 'claude' or 'pi', got %q", p.Runtime)
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (
		  label, runtime, worker_cmd, role, model_config,
		  enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.Label, p.Runtime,
		nullableString(p.WorkerCmd),
		nullableString(p.Role),
		nullableString(p.ModelConfig),
		boolToInt(enabled), now, now)
	if err != nil {
		if isSQLiteUniqueErr(err) {
			return nil, ErrAgentLabelTaken
		}
		return nil, fmt.Errorf("insert agents: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Agent{
		ID:          id,
		Label:       p.Label,
		Runtime:     p.Runtime,
		WorkerCmd:   p.WorkerCmd,
		Role:        p.Role,
		ModelConfig: p.ModelConfig,
		Enabled:     enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// GetAgent returns the agent by id.
func (s *SQLiteLocal) GetAgent(ctx context.Context, id int64) (*Agent, error) {
	return s.scanOneAgent(ctx,
		`SELECT id, label, runtime, worker_cmd, role, model_config,
		        enabled, created_at, updated_at
		   FROM agents WHERE id=?`, id)
}

// GetAgentByLabel returns the agent with the given label.
func (s *SQLiteLocal) GetAgentByLabel(ctx context.Context, label string) (*Agent, error) {
	return s.scanOneAgent(ctx,
		`SELECT id, label, runtime, worker_cmd, role, model_config,
		        enabled, created_at, updated_at
		   FROM agents WHERE label=?`, label)
}

// ListAgents returns all agents, optionally filtered to enabled only.
// Ordered by label ascending for stable CLI output.
func (s *SQLiteLocal) ListAgents(ctx context.Context, enabledOnly bool) ([]Agent, error) {
	q := `SELECT id, label, runtime, worker_cmd, role, model_config,
	             enabled, created_at, updated_at
	        FROM agents`
	if enabledOnly {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY label ASC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateAgent applies a partial update. nil fields are unchanged.
// Returns ErrAgentNotFound if no row matches the id.
func (s *SQLiteLocal) UpdateAgent(ctx context.Context, id int64, p UpdateAgentParams) error {
	var (
		sets []string
		args []any
	)
	if p.Runtime != nil {
		if *p.Runtime != AgentRuntimeClaude && *p.Runtime != AgentRuntimePi {
			return fmt.Errorf("UpdateAgent: runtime must be 'claude' or 'pi', got %q", *p.Runtime)
		}
		sets = append(sets, "runtime=?")
		args = append(args, *p.Runtime)
	}
	if p.WorkerCmd != nil {
		sets = append(sets, "worker_cmd=?")
		args = append(args, nullableString(*p.WorkerCmd))
	}
	if p.Role != nil {
		sets = append(sets, "role=?")
		args = append(args, nullableString(*p.Role))
	}
	if p.ModelConfig != nil {
		sets = append(sets, "model_config=?")
		args = append(args, nullableString(*p.ModelConfig))
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*p.Enabled))
	}
	if len(sets) == 0 {
		return nil // nothing to update
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	sets = append(sets, "updated_at=?")
	args = append(args, now)
	args = append(args, id)

	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// DeleteAgent removes an agent by id. CASCADE drops pool_members rows;
// cards.assigned_to_agent_id and card_runs.agent_id ON DELETE SET NULL.
// Returns ErrAgentNotFound if no row matched.
func (s *SQLiteLocal) DeleteAgent(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// scanOneAgent runs the given query expecting at most one row.
func (s *SQLiteLocal) scanOneAgent(ctx context.Context, q string, args ...any) (*Agent, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrAgentNotFound
	}
	return scanAgent(rows)
}

// scanAgent unmarshals a single row using the standard column order.
func scanAgent(rows *sql.Rows) (*Agent, error) {
	var (
		a            Agent
		workerCmd    sql.NullString
		role         sql.NullString
		modelConfig  sql.NullString
		enabledInt   int
	)
	if err := rows.Scan(
		&a.ID, &a.Label, &a.Runtime,
		&workerCmd, &role, &modelConfig,
		&enabledInt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if workerCmd.Valid {
		a.WorkerCmd = workerCmd.String
	}
	if role.Valid {
		a.Role = role.String
	}
	if modelConfig.Valid {
		a.ModelConfig = modelConfig.String
	}
	a.Enabled = enabledInt != 0
	return &a, nil
}

// isSQLiteUniqueErr returns true if the given error is a SQLite UNIQUE
// constraint violation. Used to surface ErrAgentLabelTaken cleanly.
func isSQLiteUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
