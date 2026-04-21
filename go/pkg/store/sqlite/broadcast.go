package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// v3.4 Phase 3 scaffolding for peer-broadcast + peer-round CLI verbs.
// BroadcastLocal itself (the fan-out write) already landed in Phase 1
// via send.go. The CLI layer needs a read-only pre-flight to match
// Python's cmd_peer_broadcast validation order — enumerate live peers
// in scope, check cohort presence, detect empty-room — before kicking
// off the transactional write.

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

// BroadcastCLI is the peer-broadcast verb's fan-out write. Distinct
// from the Phase-1 BroadcastLocal(send.go) in one important way: it
// does NOT set inbox.idempotency_key on fan-out rows, matching Python
// cmd_peer_broadcast (peer-inbox-db.py:2353-2362) which omits the
// column from its INSERT.
//
// Why the Phase-1 BroadcastLocal can't be reused here:
//
//   inbox has a UNIQUE(workspace_id, idempotency_key) partial index
//   (migrations/sqlite/0001:48). BroadcastLocal reuses the caller's
//   SendParams.MessageID (a single ULID) for every recipient — which
//   violates the constraint on the 2nd INSERT of any N>=2 broadcast.
//   /api/send only ever calls BroadcastLocal with N=1 (per-remote fan-
//   out happens one HTTP POST at a time), which is why the bug didn't
//   surface until Phase 3 stood up the CLI verb with multi-recipient
//   fixtures.
//
// This function mirrors Python's transactional shape: pre-check every
// distinct room is non-terminated and under the turn cap, fan-out the
// INSERTs (one server_seq per recipient, per-room monotonic), bump
// each distinct room exactly once, optionally terminate on [[end]],
// bump sender's last_seen — all in a single BEGIN IMMEDIATE.
//
// Post-commit: resolves mentions and issues best-effort channel-socket
// pushes per recipient, same as BroadcastLocal.
//
// Results are returned in BroadcastLocal's shape (one SendResult per
// recipient, sorted by To label) so the CLI stdout composition code is
// single-sourced.
func (s *SQLiteLocal) BroadcastCLI(ctx context.Context, p SendParams) ([]SendResult, error) {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if err := validateBody(p.Body); err != nil {
		return nil, err
	}

	recipients, err := s.broadcastRecipients(ctx, p.SenderCWD, p.SenderLabel, p.PairKey, p.ToLabel)
	if err != nil {
		return nil, err
	}
	if len(recipients) == 0 {
		return nil, nil
	}

	terminates := strings.Contains(strings.ToLower(p.Body), terminationToken)
	nowISO := p.Now.Format("2006-01-02T15:04:05Z")

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open conn: %w", err)
	}
	connReleased := false
	releaseConn := func() {
		if !connReleased {
			_ = conn.Close()
			connReleased = true
		}
	}
	defer releaseConn()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed && !connReleased {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Pre-check every distinct room. In pair-key mode that's one room
	// regardless of recipient count; in cwd mode each edge is its own.
	seenRooms := map[string]bool{}
	for _, r := range recipients {
		if seenRooms[r.roomKey] {
			continue
		}
		seenRooms[r.roomKey] = true
		tc, term, err := fetchRoomState(ctx, conn, r.roomKey)
		if err != nil {
			return nil, err
		}
		if term {
			return nil, fmt.Errorf("%w: %s", ErrRoomTerminated, r.roomKey)
		}
		cap := int64(maxPairTurns())
		if tc >= cap {
			return nil, fmt.Errorf("%w: %s (%d/%d)", ErrTurnCapExceeded, r.roomKey, tc, cap)
		}
	}

	results := make([]SendResult, 0, len(recipients))
	for _, r := range recipients {
		nextSeq, err := nextServerSeq(ctx, conn, r.roomKey)
		if err != nil {
			return nil, err
		}
		// NOTE: idempotency_key intentionally omitted — matches Python
		// cmd_peer_broadcast + side-steps the UNIQUE partial index.
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO inbox
			  (to_cwd, to_label, from_cwd, from_label, body, created_at,
			   room_key, server_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.toCWD, r.toLabel, p.SenderCWD, p.SenderLabel, p.Body, nowISO,
			r.roomKey, nextSeq); err != nil {
			return nil, fmt.Errorf("insert inbox: %w", err)
		}
		results = append(results, SendResult{
			To: r.toLabel, ServerSeq: nextSeq,
			PushStatus: "pending", Terminates: terminates,
		})
	}

	bumped := map[string]bool{}
	for _, r := range recipients {
		if bumped[r.roomKey] {
			continue
		}
		bumped[r.roomKey] = true
		if err := bumpRoom(ctx, conn, r.roomKey, nullablePairKey(p.PairKey)); err != nil {
			return nil, err
		}
		if terminates {
			if err := terminateRoom(ctx, conn, r.roomKey, nullablePairKey(p.PairKey), p.SenderLabel, nowISO); err != nil {
				return nil, err
			}
		}
	}
	if err := bumpLastSeen(ctx, conn, p.SenderCWD, p.SenderLabel, nowISO); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true
	releaseConn()
	markInboxDirty()

	// Post-commit: mentions + per-recipient channel push.
	members, _ := s.roomMembers(ctx, p.SenderCWD, p.SenderLabel, p.PairKey)
	mentions := resolveMentions(p.Body, p.Mentions, members, p.SenderLabel)
	for i := range results {
		results[i].Mentions = mentions
	}
	for i, r := range recipients {
		if r.channelSocket == "" {
			results[i].PushStatus = "no-channel"
			continue
		}
		if _, err := os.Stat(r.channelSocket); err != nil {
			results[i].PushStatus = "no-channel"
			continue
		}
		// cohort-labels meta: Python stamps every push with a comma-sorted
		// cohort list when the send is a multicast (ToLabel != "").
		meta := map[string]string{"to": r.toLabel, "broadcast": "1"}
		if p.ToLabel != "" && p.ToLabel != "@room" {
			// Build the comma-sorted present-label list from the actual
			// recipient set (Python: sorted(r["label"] for r in live)
			// after cohort filter).
			labels := make([]string, 0, len(recipients))
			for _, rr := range recipients {
				labels = append(labels, rr.toLabel)
			}
			sort.Strings(labels)
			meta["cohort"] = strings.Join(labels, ",")
		}
		if len(mentions) > 0 {
			meta["mentions"] = strings.Join(mentions, ",")
		}
		code, _ := sendOverUnixSocket(r.channelSocket, map[string]any{
			"from": p.SenderLabel,
			"body": p.Body,
			"meta": meta,
		})
		if code == 200 {
			results[i].PushStatus = "pushed"
		} else {
			results[i].PushStatus = fmt.Sprintf("push-failed(%d)", code)
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].To < results[j].To })
	return results, nil
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
