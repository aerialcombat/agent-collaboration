package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Activity-state thresholds mirror Python's IDLE_THRESHOLD_SECS /
// STALE_THRESHOLD_SECS at scripts/peer-inbox-db.py. Kept as untyped
// seconds + a helper for parity with the Python heuristic.
const (
	activeThresholdSecs = 5 * 60
	staleThresholdSecs  = 6 * 60 * 60
)

// WebMember is a peer entry in a pair or room listing. Label is the
// session's peer label. CWD is set in pair-key-mode responses (members
// can span cwds); cwd-mode pair responses leave it empty. Activity is
// derived from last_seen_at via activityState(); Channel reflects
// whether sessions.channel_socket is non-NULL.
type WebMember struct {
	Label      string
	CWD        string
	Agent      string
	Role       string
	Activity   string
	LastSeenAt string
	Channel    bool
}

// WebPair mirrors the Python _fetch_pairs per-pair shape. Peers is
// keyed by label (a or b). In pair-key mode peer_rooms state is left
// zero-valued (peer_rooms is cwd-keyed only; Python skips lookup).
type WebPair struct {
	A            string
	B            string
	Key          string // "<a>+<b>"
	Activity     string
	Total        int64
	LastID       int64
	LastAt       string
	TurnCount    int64
	TerminatedAt string
	TerminatedBy string
	Peers        map[string]WebMember
}

// WebRoom is the pair-key-mode room aggregate. Members is ordered by
// label (ASC); activity is the max (freshest) across members, with
// "terminated" overriding when peer_rooms flags it. HomeHost carries
// the v3.3-federation home-host label read from peer_rooms.home_host;
// rendered alongside PairKey as `<HomeHost>:<PairKey>` at the display
// layer.
type WebRoom struct {
	PairKey      string
	HomeHost     string
	Key          string
	Activity     string
	Total        int64
	LastID       int64
	LastAt       string
	TurnCount    int64
	TerminatedAt string
	TerminatedBy string
	Members      []WebMember
}

// WebMessage is one inbox row as rendered to the web client.
type WebMessage struct {
	ID        int64
	From      string
	To        string
	Body      string
	CreatedAt string
	Read      bool
}

// FetchPairsOpts selects the scope. Exactly one of PairKey / CWD must
// be set. In pair-key mode the pair list is the set of (a, b) label
// pairs whose inbox rows both originate and terminate within the pair
// member set. In cwd mode the list is (a, b) pairs confined to one
// cwd.
type FetchPairsOpts struct {
	PairKey string
	CWD     string
}

// FetchMessagesOpts picks the message stream. After is the last
// seen id (returns id > after). Pair narrows to one (a, b) edge within
// the scope; leave zero-valued to return all messages for the scope.
// PairKey/CWD match FetchPairsOpts semantics.
type FetchMessagesOpts struct {
	PairKey string
	CWD     string
	After   int64
	A       string // pair narrowing — both A and B must be set together
	B       string
	AsLabel string // cwd-mode + no pair narrowing: filter to conversations involving this label
}

// SessionAuth represents the identity derived from a bearer token on the
// /api/send path (v3.3 Item 7). Label + CWD tell the request handler who
// this caller is; PairKey (if set) scopes the token to a specific room.
type SessionAuth struct {
	Label   string
	CWD     string
	PairKey string
}

// SessionByToken looks up a session row by its auth_token. Returns
// (nil, nil) when the token is unknown so callers distinguish "not found"
// from "SQL error." Backed by idx_sessions_auth_token partial unique
// index.
func (s *SQLiteLocal) SessionByToken(ctx context.Context, token string) (*SessionAuth, error) {
	if token == "" {
		return nil, nil
	}
	var a SessionAuth
	var pk sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT label, cwd, pair_key
		FROM sessions
		WHERE auth_token = ?`, token).Scan(&a.Label, &a.CWD, &pk)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("SessionByToken: %w", err)
	}
	a.PairKey = nullString(pk)
	return &a, nil
}

// TerminateRoom marks a pair-key room as ended by writing terminated_at
// + terminated_by into peer_rooms. Reversible via
// `agent-collab peer reset --pair-key K`, so this is a soft-archive
// rather than a hard-delete. Idempotent — re-terminating an already-
// terminated room is a no-op (we do update terminated_by on each
// invocation so audit trail reflects the latest actor).
func (s *SQLiteLocal) TerminateRoom(ctx context.Context, pairKey, by string) error {
	roomKey := "pk:" + pairKey
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO peer_rooms (room_key, pair_key, turn_count, terminated_at, terminated_by)
		VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(room_key) DO UPDATE SET
		  terminated_at = excluded.terminated_at,
		  terminated_by = excluded.terminated_by`,
		roomKey, pairKey, nowISO, by)
	return err
}

// SenderScope selects the sessions-table lookup key for sender cwd
// resolution. Exactly one of PairKey / CWD must be set, matching the
// FetchPairs discipline.
type SenderScope struct {
	PairKey string
	CWD     string
}

// SenderCWD returns the cwd of the sessions row for (scope, label).
// Returns sql.ErrNoRows if not registered. Used by the web composer
// to resolve where `peer-send` should be invoked from — the sender's
// own cwd, not the viewer's.
func (s *SQLiteLocal) SenderCWD(ctx context.Context, scope SenderScope, label string) (string, error) {
	if (scope.PairKey == "") == (scope.CWD == "") {
		return "", fmt.Errorf("SenderCWD: exactly one of PairKey/CWD required")
	}
	var cwd sql.NullString
	var err error
	if scope.PairKey != "" {
		err = s.db.QueryRowContext(ctx,
			"SELECT cwd FROM sessions WHERE pair_key = ? AND label = ?",
			scope.PairKey, label).Scan(&cwd)
	} else {
		err = s.db.QueryRowContext(ctx,
			"SELECT cwd FROM sessions WHERE cwd = ? AND label = ?",
			scope.CWD, label).Scan(&cwd)
	}
	if err != nil {
		return "", err
	}
	return nullString(cwd), nil
}

// ClearChannelSocket nulls the channel_socket column for a session.
// Used after auto-registering "owner" from the web composer — the
// session-register path may bind an ancestor process's socket to the
// human owner, who has no channel listener. Matches Python's manual
// scrub in cmd_peer_web's do_POST handler.
func (s *SQLiteLocal) ClearChannelSocket(ctx context.Context, cwd, label string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET channel_socket = NULL WHERE cwd = ? AND label = ?",
		cwd, label)
	return err
}

// AllPairKeys returns every pair_key known to the DB, unioned across
// peer_rooms (keyed rooms — present even before anyone joins) and
// sessions (catches rooms whose peer_rooms row was never written).
// Ordered by pair_key ASC for stable Go output.
func (s *SQLiteLocal) AllPairKeys(ctx context.Context) ([]string, error) {
	rs, err := s.db.QueryContext(ctx, `
		SELECT pair_key FROM peer_rooms
		 WHERE pair_key IS NOT NULL AND pair_key != ''
		UNION
		SELECT pair_key FROM sessions
		 WHERE pair_key IS NOT NULL AND pair_key != ''
		 ORDER BY pair_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("AllPairKeys: %w", err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var pk string
		if err := rs.Scan(&pk); err != nil {
			return nil, fmt.Errorf("AllPairKeys scan: %w", err)
		}
		out = append(out, pk)
	}
	return out, rs.Err()
}

// FetchRooms returns the single-room aggregate for a pair_key. Python
// always returns a one-element slice shape because the server is
// scoped to one pair_key; Go preserves the list shape for future
// multi-room-dashboard work.
func (s *SQLiteLocal) FetchRooms(ctx context.Context, pairKey string) ([]WebRoom, error) {
	if pairKey == "" {
		return nil, fmt.Errorf("FetchRooms: empty pairKey")
	}
	members, err := s.pairMembers(ctx, pairKey)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return []WebRoom{}, nil
	}

	roomKey := "pk:" + pairKey

	var (
		total  sql.NullInt64
		lastID sql.NullInt64
		lastAt sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*)        AS total,
		  MAX(id)         AS last_id,
		  MAX(created_at) AS last_at
		FROM inbox
		WHERE room_key = ?`, roomKey).Scan(&total, &lastID, &lastAt)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("FetchRooms: stats: %w", err)
	}

	// Member rows. Ordered by label so the Go + Python responses have
	// identical member ordering (Python uses `sorted({lbl for ...})`).
	memberLabels := make([]string, 0, len(members))
	seenLabels := map[string]bool{}
	for _, m := range members {
		if seenLabels[m.label] {
			continue
		}
		seenLabels[m.label] = true
		memberLabels = append(memberLabels, m.label)
	}
	sortStrings(memberLabels)

	now := time.Now().UTC()
	bestActivity := "unknown"
	activityRank := map[string]int{"unknown": -1, "stale": 0, "idle": 1, "active": 2}

	out := make([]WebMember, 0, len(memberLabels))
	for _, lbl := range memberLabels {
		m, err := s.fetchSessionByPairKey(ctx, pairKey, lbl)
		if err != nil {
			return nil, fmt.Errorf("FetchRooms: session(%s/%s): %w", pairKey, lbl, err)
		}
		if m == nil {
			out = append(out, WebMember{Label: lbl, Activity: "unknown"})
			continue
		}
		m.Activity = activityState(m.LastSeenAt, now)
		if activityRank[m.Activity] > activityRank[bestActivity] {
			bestActivity = m.Activity
		}
		out = append(out, *m)
	}

	var (
		turnCount    sql.NullInt64
		terminatedAt sql.NullString
		terminatedBy sql.NullString
		homeHost     sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT turn_count, terminated_at, terminated_by, home_host
		FROM peer_rooms WHERE room_key = ?`, roomKey).Scan(
		&turnCount, &terminatedAt, &terminatedBy, &homeHost)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("FetchRooms: peer_rooms: %w", err)
	}

	activity := bestActivity
	if len(out) == 0 {
		activity = "unknown"
	}
	if terminatedAt.Valid && terminatedAt.String != "" {
		activity = "terminated"
	}

	return []WebRoom{{
		PairKey:      pairKey,
		HomeHost:     nullString(homeHost),
		Key:          pairKey,
		Activity:     activity,
		Total:        total.Int64,
		LastID:       lastID.Int64,
		LastAt:       nullString(lastAt),
		TurnCount:    turnCount.Int64,
		TerminatedAt: nullString(terminatedAt),
		TerminatedBy: nullString(terminatedBy),
		Members:      out,
	}}, nil
}

// FetchPairs returns the edge-pair list for either pair-key or cwd
// mode. Matches the Python _fetch_pairs shape 1:1.
func (s *SQLiteLocal) FetchPairs(ctx context.Context, opts FetchPairsOpts) ([]WebPair, error) {
	if (opts.PairKey == "") == (opts.CWD == "") {
		return nil, fmt.Errorf("FetchPairs: exactly one of PairKey/CWD required")
	}

	type edgeRow struct {
		a, b           string
		lastID         int64
		lastAt         string
		total          int64
		unreadBackend  int64
	}
	var rows []edgeRow

	if opts.PairKey != "" {
		members, err := s.pairMembers(ctx, opts.PairKey)
		if err != nil {
			return nil, err
		}
		labelSet := map[string]bool{}
		for _, m := range members {
			labelSet[m.label] = true
		}
		if len(labelSet) == 0 {
			return []WebPair{}, nil
		}
		labels := make([]string, 0, len(labelSet))
		for l := range labelSet {
			labels = append(labels, l)
		}
		// Bind placeholders twice (from_label IN (...) AND to_label IN (...)).
		marks := strings.Repeat("?,", len(labels))
		marks = marks[:len(marks)-1]
		query := fmt.Sprintf(`
			SELECT
			  CASE WHEN from_label < to_label THEN from_label ELSE to_label END AS a,
			  CASE WHEN from_label < to_label THEN to_label ELSE from_label END AS b,
			  MAX(id)         AS last_id,
			  MAX(created_at) AS last_at,
			  COUNT(*)        AS total,
			  SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread_backend
			FROM inbox
			WHERE from_label IN (%s) AND to_label IN (%s)
			GROUP BY a, b
			ORDER BY last_at DESC`, marks, marks)
		args := make([]any, 0, 2*len(labels))
		for _, l := range labels {
			args = append(args, l)
		}
		for _, l := range labels {
			args = append(args, l)
		}
		rs, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("FetchPairs (pair_key): query: %w", err)
		}
		defer rs.Close()
		for rs.Next() {
			var r edgeRow
			var lastAt, lastID sql.NullString
			var total, unread sql.NullInt64
			var lastIDNum sql.NullInt64
			if err := rs.Scan(&r.a, &r.b, &lastIDNum, &lastAt, &total, &unread); err != nil {
				return nil, fmt.Errorf("FetchPairs (pair_key): scan: %w", err)
			}
			_ = lastID
			r.lastID = lastIDNum.Int64
			r.lastAt = nullString(lastAt)
			r.total = total.Int64
			r.unreadBackend = unread.Int64
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			return nil, err
		}
	} else {
		rs, err := s.db.QueryContext(ctx, `
			SELECT
			  CASE WHEN from_label < to_label THEN from_label ELSE to_label END AS a,
			  CASE WHEN from_label < to_label THEN to_label ELSE from_label END AS b,
			  MAX(id)         AS last_id,
			  MAX(created_at) AS last_at,
			  COUNT(*)        AS total,
			  SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread_backend
			FROM inbox
			WHERE to_cwd = ? AND from_cwd = ?
			GROUP BY a, b
			ORDER BY last_at DESC`, opts.CWD, opts.CWD)
		if err != nil {
			return nil, fmt.Errorf("FetchPairs (cwd): query: %w", err)
		}
		defer rs.Close()
		for rs.Next() {
			var r edgeRow
			var lastAt sql.NullString
			var total, unread, lastIDNum sql.NullInt64
			if err := rs.Scan(&r.a, &r.b, &lastIDNum, &lastAt, &total, &unread); err != nil {
				return nil, fmt.Errorf("FetchPairs (cwd): scan: %w", err)
			}
			r.lastID = lastIDNum.Int64
			r.lastAt = nullString(lastAt)
			r.total = total.Int64
			r.unreadBackend = unread.Int64
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	activityRank := map[string]int{"unknown": -1, "active": 0, "idle": 1, "stale": 2}
	out := make([]WebPair, 0, len(rows))
	for _, r := range rows {
		ai, err := s.lookupMember(ctx, opts, r.a)
		if err != nil {
			return nil, err
		}
		bi, err := s.lookupMember(ctx, opts, r.b)
		if err != nil {
			return nil, err
		}
		ai.Activity = activityOrUnknown(ai, now)
		bi.Activity = activityOrUnknown(bi, now)
		// Python "_worse" — pair activity = worst of the two.
		pairActivity := ai.Activity
		if activityRank[bi.Activity] > activityRank[pairActivity] {
			pairActivity = bi.Activity
		}

		var turnCount int64
		var terminatedAt, terminatedBy string
		if opts.CWD != "" {
			edgeRoomKey := "cwd:" + opts.CWD + "#" + r.a + "+" + r.b
			var tc sql.NullInt64
			var tat, tby sql.NullString
			err := s.db.QueryRowContext(ctx, `
				SELECT turn_count, terminated_at, terminated_by
				FROM peer_rooms WHERE room_key = ?`, edgeRoomKey).Scan(&tc, &tat, &tby)
			if err != nil && err != sql.ErrNoRows {
				return nil, fmt.Errorf("FetchPairs: peer_rooms(%s): %w", edgeRoomKey, err)
			}
			turnCount = tc.Int64
			terminatedAt = nullString(tat)
			terminatedBy = nullString(tby)
		}
		if terminatedAt != "" {
			pairActivity = "terminated"
		}

		out = append(out, WebPair{
			A:            r.a,
			B:            r.b,
			Key:          r.a + "+" + r.b,
			Activity:     pairActivity,
			Total:        r.total,
			LastID:       r.lastID,
			LastAt:       r.lastAt,
			TurnCount:    turnCount,
			TerminatedAt: terminatedAt,
			TerminatedBy: terminatedBy,
			Peers:        map[string]WebMember{r.a: ai, r.b: bi},
		})
	}
	return out, nil
}

// FetchMessages returns inbox rows > opts.After, filtered per scope.
func (s *SQLiteLocal) FetchMessages(ctx context.Context, opts FetchMessagesOpts) ([]WebMessage, error) {
	if (opts.PairKey == "") == (opts.CWD == "") {
		return nil, fmt.Errorf("FetchMessages: exactly one of PairKey/CWD required")
	}
	hasPair := opts.A != "" && opts.B != ""

	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case opts.PairKey != "" && hasPair:
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_label, to_label, body, created_at, read_at
			FROM inbox
			WHERE id > ? AND room_key = ?
			  AND ((from_label = ? AND to_label = ?)
			    OR (from_label = ? AND to_label = ?))
			ORDER BY id ASC`,
			opts.After, "pk:"+opts.PairKey,
			opts.A, opts.B, opts.B, opts.A)
	case opts.PairKey != "":
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_label, to_label, body, created_at, read_at
			FROM inbox
			WHERE id > ? AND room_key = ?
			ORDER BY id ASC`,
			opts.After, "pk:"+opts.PairKey)
	case hasPair:
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_label, to_label, body, created_at, read_at
			FROM inbox
			WHERE id > ? AND to_cwd = ? AND from_cwd = ?
			  AND ((from_label = ? AND to_label = ?)
			    OR (from_label = ? AND to_label = ?))
			ORDER BY id ASC`,
			opts.After, opts.CWD, opts.CWD,
			opts.A, opts.B, opts.B, opts.A)
	case opts.AsLabel != "":
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_label, to_label, body, created_at, read_at
			FROM inbox
			WHERE id > ?
			  AND ((to_cwd = ? AND to_label = ?)
			    OR (from_cwd = ? AND from_label = ?))
			ORDER BY id ASC`,
			opts.After, opts.CWD, opts.AsLabel, opts.CWD, opts.AsLabel)
	default:
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, from_label, to_label, body, created_at, read_at
			FROM inbox
			WHERE id > ? AND (to_cwd = ? OR from_cwd = ?)
			ORDER BY id ASC`,
			opts.After, opts.CWD, opts.CWD)
	}
	if err != nil {
		return nil, fmt.Errorf("FetchMessages: query: %w", err)
	}
	defer rows.Close()

	out := []WebMessage{}
	for rows.Next() {
		var m WebMessage
		var readAt sql.NullString
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Body, &m.CreatedAt, &readAt); err != nil {
			return nil, fmt.Errorf("FetchMessages: scan: %w", err)
		}
		m.Read = readAt.Valid && readAt.String != ""
		out = append(out, m)
	}
	return out, rows.Err()
}

type pairMember struct {
	cwd   string
	label string
}

func (s *SQLiteLocal) pairMembers(ctx context.Context, pairKey string) ([]pairMember, error) {
	rs, err := s.db.QueryContext(ctx, `
		SELECT cwd, label FROM sessions WHERE pair_key = ? ORDER BY label`, pairKey)
	if err != nil {
		return nil, fmt.Errorf("pairMembers: %w", err)
	}
	defer rs.Close()
	var out []pairMember
	for rs.Next() {
		var m pairMember
		if err := rs.Scan(&m.cwd, &m.label); err != nil {
			return nil, fmt.Errorf("pairMembers scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rs.Err()
}

func (s *SQLiteLocal) fetchSessionByPairKey(ctx context.Context, pairKey, label string) (*WebMember, error) {
	var (
		cwd, agent, role, lastSeen sql.NullString
		channel                    sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT cwd, last_seen_at, agent, role, channel_socket
		FROM sessions WHERE pair_key = ? AND label = ?`,
		pairKey, label).Scan(&cwd, &lastSeen, &agent, &role, &channel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &WebMember{
		Label:      label,
		CWD:        nullString(cwd),
		Agent:      nullString(agent),
		Role:       nullString(role),
		LastSeenAt: nullString(lastSeen),
		Channel:    channel.Valid && channel.String != "",
	}, nil
}

func (s *SQLiteLocal) lookupMember(ctx context.Context, opts FetchPairsOpts, label string) (WebMember, error) {
	if opts.PairKey != "" {
		m, err := s.fetchSessionByPairKey(ctx, opts.PairKey, label)
		if err != nil {
			return WebMember{}, err
		}
		if m == nil {
			return WebMember{Label: label, Activity: "unknown"}, nil
		}
		return *m, nil
	}
	var (
		agent, role, lastSeen sql.NullString
		channel               sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT last_seen_at, agent, role, channel_socket
		FROM sessions WHERE cwd = ? AND label = ?`,
		opts.CWD, label).Scan(&lastSeen, &agent, &role, &channel)
	if err == sql.ErrNoRows {
		return WebMember{Label: label, Activity: "unknown"}, nil
	}
	if err != nil {
		return WebMember{}, err
	}
	return WebMember{
		Label:      label,
		Agent:      nullString(agent),
		Role:       nullString(role),
		LastSeenAt: nullString(lastSeen),
		Channel:    channel.Valid && channel.String != "",
	}, nil
}

func activityState(lastSeen string, now time.Time) string {
	if lastSeen == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, lastSeen)
	if err != nil {
		// Python's parse_iso accepts a broader format set; retry without
		// timezone suffix.
		t, err = time.Parse("2006-01-02T15:04:05", lastSeen)
		if err != nil {
			return "unknown"
		}
	}
	age := now.Sub(t).Seconds()
	if age < activeThresholdSecs {
		return "active"
	}
	if age < staleThresholdSecs {
		return "idle"
	}
	return "stale"
}

func activityOrUnknown(m WebMember, now time.Time) string {
	if m.LastSeenAt == "" {
		return "unknown"
	}
	return activityState(m.LastSeenAt, now)
}

func nullString(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

func sortStrings(xs []string) {
	// tiny local sort — avoids importing sort for a 1-use call site
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
