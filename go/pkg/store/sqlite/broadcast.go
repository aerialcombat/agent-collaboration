package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// v3.4 Phase 3 scaffolding for peer-broadcast + peer-round CLI verbs.
// BroadcastLocal itself (the fan-out write) lives in send.go. The CLI
// layer needs a read-only pre-flight to match Python's
// cmd_peer_broadcast validation order — enumerate live peers in scope,
// check cohort presence, detect empty-room — before kicking off the
// transactional write.

// LivePeer is one row returned by LivePeersInScope.
type LivePeer struct {
	CWD      string
	Label    string
	LastSeen string
}

// LivePeersInScope returns non-stale peers in the sender's scope
// (pair_key if set, else cwd), excluding the sender. Stale is the same
// 30-minute threshold used by BroadcastLocal's transactional filter —
// pre-flight and actual write see the same visibility window.
//
// Rows are ordered by label so pre-flight error text (e.g. "sorted
// candidates") is deterministic and matches Python's sorted(...) shape.
func (s *SQLiteLocal) LivePeersInScope(ctx context.Context, selfCWD, selfLabel, pairKey string) ([]LivePeer, error) {
	var rows *sql.Rows
	var err error
	if pairKey != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, last_seen_at
			FROM sessions
			WHERE pair_key = ? AND NOT (cwd = ? AND label = ?)
			ORDER BY label`,
			pairKey, selfCWD, selfLabel)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, last_seen_at
			FROM sessions
			WHERE cwd = ? AND NOT (cwd = ? AND label = ?)
			ORDER BY label`,
			selfCWD, selfCWD, selfLabel)
	}
	if err != nil {
		return nil, fmt.Errorf("LivePeersInScope: %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	var out []LivePeer
	for rows.Next() {
		var p LivePeer
		if err := rows.Scan(&p.CWD, &p.Label, &p.LastSeen); err != nil {
			return nil, err
		}
		if secondsSinceISO(p.LastSeen, now) > staleThresholdSecs {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RoomMembers returns the full label roster for the sender's scope,
// including the sender. Mirrors Python's _room_members helper. Used by
// the CLI to pre-validate --mention targets before the write tx.
func (s *SQLiteLocal) RoomMembers(ctx context.Context, selfCWD, selfLabel, pairKey string) (map[string]bool, error) {
	members, err := s.roomMembers(ctx, selfCWD, selfLabel, pairKey)
	if err != nil {
		return nil, err
	}
	return members, nil
}

// GetSessionRole reads the role column for (cwd, label), returning ""
// when the row is missing or the column is NULL. Used by peer-round to
// warn when the caller isn't registered as mediator.
func (s *SQLiteLocal) GetSessionRole(ctx context.Context, cwd, label string) (string, error) {
	var role sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label,
	).Scan(&role)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("GetSessionRole: %w", err)
	}
	if !role.Valid {
		return "", nil
	}
	return role.String, nil
}

// GetSessionRoleAgent reads the role + agent columns for (cwd, label),
// returning ("", "", nil) when the row is missing or the columns are
// NULL. v3.7: used by Send / BroadcastLocal / EmitSystemEvent to stamp
// meta.from_role and meta.from_agent on channel pushes so receiving
// MCPs can distinguish a human owner's broadcast from agent chatter
// (the motivating case: owner messages in peer-web should pull
// responses, not drift past as party-mode silence).
func (s *SQLiteLocal) GetSessionRoleAgent(ctx context.Context, cwd, label string) (string, string, error) {
	var role, agent sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT role, agent FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label,
	).Scan(&role, &agent)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", fmt.Errorf("GetSessionRoleAgent: %w", err)
	}
	r := ""
	if role.Valid {
		r = role.String
	}
	a := ""
	if agent.Valid {
		a = agent.String
	}
	return r, a, nil
}
