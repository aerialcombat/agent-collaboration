package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// v3.4 Phase 3: store-level helpers for the peer-send CLI wrapper. Send
// itself landed in Phase 1 (send.go); this file adds the few read-only
// pre-flights the CLI needs (pair-key lookup already exists in list.go;
// home-host routing and --to inference are new here).
//
// Scope boundary: these are query-only helpers that mirror one-liner
// Python functions (_get_room_home_host, _infer_peer_label). They never
// mutate state — all writes route through SQLiteLocal.Send / BroadcastLocal
// already in send.go.

// ErrPeerLabelAmbiguous is returned by InferPeerLabel when more than one
// live peer is in scope. Carries the sorted candidate list so the CLI
// wrapper can echo Python's "N live peers in pair_key=... ([x, y]); pass
// --to <label>" message. Matches Python's EXIT_VALIDATION path.
type ErrPeerLabelAmbiguous struct {
	Scope      string   // e.g. "pair_key=foo" or "cwd=/tmp/x"
	Candidates []string // sorted, live labels
}

func (e *ErrPeerLabelAmbiguous) Error() string {
	return fmt.Sprintf("ambiguous peer label in %s (%d candidates: %v)", e.Scope, len(e.Candidates), e.Candidates)
}

// ErrPeerLabelMissing is returned by InferPeerLabel when there is no live
// peer in scope at all. Matches Python's EXIT_NOT_FOUND path.
type ErrPeerLabelMissing struct {
	Scope string
}

func (e *ErrPeerLabelMissing) Error() string {
	return fmt.Sprintf("no live peer in %s", e.Scope)
}

// GetRoomHomeHost mirrors Python's _get_room_home_host. Returns the
// peer_rooms.home_host value for a room key, or "" if the row is absent
// OR the column is NULL. Callers (the peer-send CLI wrapper) treat ""
// as "local" — matches pre-v3.3 behavior for cwd-only degenerate rooms
// that never had peer_rooms entries.
func (s *SQLiteLocal) GetRoomHomeHost(ctx context.Context, roomKey string) (string, error) {
	var host sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT home_host FROM peer_rooms WHERE room_key = ?",
		roomKey,
	).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get room home_host: %w", err)
	}
	if !host.Valid {
		return "", nil
	}
	return host.String, nil
}

// InferPeerLabel mirrors Python's _infer_peer_label. Returns the single
// live peer label in the caller's scope (pair_key match when set, else
// cwd match). Stale peers (last_seen_at older than 30m) are filtered so
// a long-abandoned session can't steal an inferred send.
//
// Error cases:
//   - 0 live peers: ErrPeerLabelMissing — CLI surfaces as EXIT_NOT_FOUND.
//   - >1 live peer: ErrPeerLabelAmbiguous carrying the sorted candidate
//     list — CLI surfaces as EXIT_VALIDATION.
//
// `now` is taken from the caller for testability; pass time.Now().UTC()
// in production. Matches Python's seconds_since behavior of using wall
// clock; the only reason it's a parameter is unit tests.
func (s *SQLiteLocal) InferPeerLabel(ctx context.Context, selfCWD, selfLabel, selfPairKey string, now time.Time) (string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if selfPairKey != "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT label, last_seen_at FROM sessions
			 WHERE pair_key = ? AND NOT (cwd = ? AND label = ?)`,
			selfPairKey, selfCWD, selfLabel)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT label, last_seen_at FROM sessions
			 WHERE cwd = ? AND NOT (cwd = ? AND label = ?)`,
			selfCWD, selfCWD, selfLabel)
	}
	if err != nil {
		return "", fmt.Errorf("infer peer label: %w", err)
	}
	defer rows.Close()

	var live []string
	for rows.Next() {
		var label, lastSeen string
		if err := rows.Scan(&label, &lastSeen); err != nil {
			return "", fmt.Errorf("scan peer candidate: %w", err)
		}
		// Python's activity_state != "stale" means age < STALE_THRESHOLD_SECS.
		if secondsSinceISO(lastSeen, now) < staleThresholdSecs {
			live = append(live, label)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("infer peer rows: %w", err)
	}

	sort.Strings(live)
	scope := "cwd=" + selfCWD
	if selfPairKey != "" {
		scope = "pair_key=" + selfPairKey
	}
	switch len(live) {
	case 0:
		return "", &ErrPeerLabelMissing{Scope: scope}
	case 1:
		return live[0], nil
	default:
		return "", &ErrPeerLabelAmbiguous{Scope: scope, Candidates: live}
	}
}

