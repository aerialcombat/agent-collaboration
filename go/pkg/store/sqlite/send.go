package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// v3.4 Phase 1: peer-send logic ported from scripts/peer-inbox-db.py
// cmd_peer_send + cmd_peer_broadcast. Called directly by peer-web's
// handleSend so /api/send no longer shells out to python3. Semantics are
// a byte-level port — turn cap, termination token, idempotency dedup,
// server_seq assignment, channel push, and mention resolution all match
// the Python contract. Parity is asserted by tests/federation-smoke.sh
// and the v3.4 Phase 2+ fixture harness.

// Error sentinels for mapping to HTTP status codes. Matches the exit
// codes Python cmd_peer_send sets (EXIT_NOT_FOUND, EXIT_PEER_OFFLINE,
// EXIT_VALIDATION).
var (
	ErrPeerNotFound    = errors.New("peer not found")
	ErrPeerOffline     = errors.New("peer offline")
	ErrRoomTerminated  = errors.New("room terminated")
	ErrTurnCapExceeded = errors.New("room reached max turns")
	ErrBodyTooLarge    = errors.New("body too large")
	ErrEmptyBody       = errors.New("empty body rejected")
)

const (
	// Mirror of scripts/peer-inbox-db.py's DEFAULT_MAX_PAIR_TURNS +
	// TERMINATION_TOKEN + STALE_THRESHOLD_SECS constants. Kept here
	// so the Go port doesn't need to re-derive them at every call site.
	defaultMaxPairTurns  = 500
	terminationToken     = "[[end]]"
	defaultMaxBodyBytes  = 8 * 1024
	channelSocketTimeout = 2 * time.Second
)

var mentionRe = regexp.MustCompile(`@([a-z0-9][a-z0-9_-]{0,63})`)

// SendParams is the input to Send / BroadcastLocal. Caller is responsible
// for having resolved the sender's cwd + label (from bearer-token lookup
// or owner auto-register) before calling.
type SendParams struct {
	SenderCWD   string // absolute path; matches sessions.cwd
	SenderLabel string // matches sessions.label
	ToLabel     string // empty / "@room" = broadcast
	Body        string
	MessageID   string   // ULID for idempotency; auto-generated if empty
	PairKey     string   // empty = cwd-only (legacy degenerate pair)
	TargetCWD   string   // peer-send --to-cwd override; bypasses pair-key resolution
	Mentions    []string // explicit --mention args; body @tokens merged in
	Now         time.Time
}

// SendResult mirrors Python's peer-send --json output shape so
// /api/send JSON responses stay byte-compatible for remote callers.
type SendResult struct {
	To         string
	MessageID  string
	ServerSeq  int64
	PushStatus string
	DedupHit   bool
	Terminates bool
	Mentions   []string
}

// Send is the unicast peer-send. Returns ErrPeerNotFound / ErrPeerOffline
// / ErrRoomTerminated / ErrTurnCapExceeded as appropriate; callers map
// these to HTTP codes. On success, updates inbox + peer_rooms +
// sessions.last_seen_at, attempts a best-effort channel push, and
// touches the inbox-dirty marker.
func (s *SQLiteLocal) Send(ctx context.Context, p SendParams) (SendResult, error) {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if p.MessageID == "" {
		p.MessageID = NewULID(p.Now)
	}
	if err := validateBody(p.Body); err != nil {
		return SendResult{}, err
	}

	roomKey := roomKeyFor(p.SenderCWD, p.SenderLabel, p.ToLabel, p.PairKey)
	var toCWD string
	if p.TargetCWD != "" {
		// --to-cwd override: honor caller's explicit target regardless of
		// pair-key membership. Matches Python cmd_peer_send:2083-2084.
		toCWD = p.TargetCWD
	} else {
		var err error
		toCWD, err = s.resolveTargetCWD(ctx, p.PairKey, p.ToLabel, p.SenderCWD)
		if err != nil {
			return SendResult{}, err
		}
	}

	var (
		result   SendResult
		recvSock string
	)
	result.To = p.ToLabel
	result.MessageID = p.MessageID

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return SendResult{}, fmt.Errorf("open conn: %w", err)
	}
	// IMPORTANT: SQLiteLocal is configured with MaxOpenConns=1 so any
	// subsequent s.db.QueryContext call from THIS goroutine (e.g.
	// roomMembers) will deadlock while this conn is open. Release before
	// post-commit work.
	connReleased := false
	releaseConn := func() {
		if !connReleased {
			_ = conn.Close()
			connReleased = true
		}
	}
	defer releaseConn()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return SendResult{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed && !connReleased {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Idempotency dedup — matches Python's SELECT by idempotency_key.
	var dedupSeq sql.NullInt64
	err = conn.QueryRowContext(ctx,
		"SELECT server_seq FROM inbox WHERE idempotency_key = ? AND workspace_id = 'default'",
		p.MessageID).Scan(&dedupSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SendResult{}, fmt.Errorf("dedup probe: %w", err)
	}
	if err == nil {
		// Prior row exists — short-circuit with original seq, no push.
		result.DedupHit = true
		result.ServerSeq = dedupSeq.Int64
		result.PushStatus = "deduped"
		if _, cerr := conn.ExecContext(ctx, "COMMIT"); cerr != nil {
			return SendResult{}, fmt.Errorf("commit (dedup): %w", cerr)
		}
		committed = true
		releaseConn()
		return result, nil
	}

	// Target session must exist and be reachable.
	var lastSeenAt string
	var channelSocket sql.NullString
	err = conn.QueryRowContext(ctx,
		"SELECT last_seen_at, channel_socket FROM sessions WHERE cwd = ? AND label = ?",
		toCWD, p.ToLabel).Scan(&lastSeenAt, &channelSocket)
	if errors.Is(err, sql.ErrNoRows) {
		return SendResult{}, fmt.Errorf("%w: %s in %s", ErrPeerNotFound, p.ToLabel, toCWD)
	}
	if err != nil {
		return SendResult{}, fmt.Errorf("target lookup: %w", err)
	}
	age := secondsSinceISO(lastSeenAt, p.Now)
	if age > staleThresholdSecs {
		return SendResult{}, fmt.Errorf("%w: %s (last seen %ds ago)", ErrPeerOffline, p.ToLabel, age)
	}
	recvSock = channelSocket.String

	// Room state: exists?, terminated?, turn cap?
	turnCount, terminated, err := fetchRoomState(ctx, conn, roomKey)
	if err != nil {
		return SendResult{}, err
	}
	if terminated {
		return SendResult{}, fmt.Errorf("%w: %s", ErrRoomTerminated, roomKey)
	}
	maxTurns := int64(maxPairTurns())
	if turnCount >= maxTurns {
		return SendResult{}, fmt.Errorf("%w: %s (%d/%d)", ErrTurnCapExceeded, roomKey, turnCount, maxTurns)
	}

	terminates := strings.Contains(strings.ToLower(p.Body), terminationToken)
	nowISO := p.Now.Format("2006-01-02T15:04:05Z")

	nextSeq, err := nextServerSeq(ctx, conn, roomKey)
	if err != nil {
		return SendResult{}, err
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO inbox
		  (to_cwd, to_label, from_cwd, from_label, body, created_at,
		   room_key, idempotency_key, server_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		toCWD, p.ToLabel, p.SenderCWD, p.SenderLabel, p.Body, nowISO,
		roomKey, p.MessageID, nextSeq); err != nil {
		return SendResult{}, fmt.Errorf("insert inbox: %w", err)
	}
	if err := bumpRoom(ctx, conn, roomKey, nullablePairKey(p.PairKey)); err != nil {
		return SendResult{}, err
	}
	if terminates {
		if err := terminateRoom(ctx, conn, roomKey, nullablePairKey(p.PairKey), p.SenderLabel, nowISO); err != nil {
			return SendResult{}, err
		}
	}
	if err := bumpLastSeen(ctx, conn, p.SenderCWD, p.SenderLabel, nowISO); err != nil {
		return SendResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return SendResult{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	releaseConn()

	markInboxDirty()

	// Mention resolution (best-effort; needs a separate read after COMMIT
	// so we see our own write). Python resolves after the tx too.
	members, _ := s.roomMembers(ctx, p.SenderCWD, p.SenderLabel, p.PairKey)
	mentions := resolveMentions(p.Body, p.Mentions, members, p.SenderLabel)

	push := "no-channel"
	if recvSock != "" && channelAlive(recvSock) {
		meta := map[string]string{"to": p.ToLabel}
		// v3.6: stamp the sender's pair_key so recipients in multiple
		// rooms over the same channel socket can tell which room this
		// came from (and default replies back into the same room).
		if p.PairKey != "" {
			meta["pair_key"] = p.PairKey
		}
		// v3.7: stamp sender's role + agent so receivers can
		// distinguish a human (agent=human / role=owner) from agents.
		// Lets the MCP prioritize owner messages instead of applying
		// party-mode silence defaults to human pings.
		if fromRole, fromAgent, err := s.GetSessionRoleAgent(ctx, p.SenderCWD, p.SenderLabel); err == nil {
			if fromRole != "" {
				meta["from_role"] = fromRole
			}
			if fromAgent != "" {
				meta["from_agent"] = fromAgent
			}
		}
		if len(mentions) > 0 {
			meta["mentions"] = strings.Join(mentions, ",")
		}
		code, _ := pushChannel(recvSock, map[string]any{
			"from": p.SenderLabel,
			"body": p.Body,
			"meta": meta,
		})
		if code == 200 {
			push = "pushed"
		} else {
			push = fmt.Sprintf("push-failed(%d)", code)
		}
	}

	result.ServerSeq = nextSeq
	result.PushStatus = push
	result.Terminates = terminates
	result.Mentions = mentions
	return result, nil
}

// BroadcastLocal fans out a send to every live peer in the sender's
// scope. Matches Python cmd_peer_broadcast semantics: one turn per
// distinct room (pair_key mode coalesces to 1), per-row server_seq,
// best-effort channel push per recipient. Returns one SendResult per
// recipient (ordered by label).
//
// inbox.idempotency_key is intentionally NOT set on fan-out rows. Python
// cmd_peer_broadcast (peer-inbox-db.py:2353-2362) omits it because the
// inbox has a UNIQUE(workspace_id, idempotency_key) partial index
// (migrations/sqlite/0001:48) — a single message_id reused across
// recipients trips the constraint on the 2nd INSERT of any N>=2
// broadcast. Callers needing retry safety should dedupe at the HTTP
// layer (idempotency is a per-recipient concept that the broadcast
// shape doesn't cleanly express).
func (s *SQLiteLocal) BroadcastLocal(ctx context.Context, p SendParams) ([]SendResult, error) {
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

	results := make([]SendResult, 0, len(recipients))
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

	// Precheck: every distinct room must be non-terminated and under the
	// turn cap. Fail the whole batch on the first violation (matches
	// Python's semantics — broadcast is atomic or not at all).
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

	for _, r := range recipients {
		nextSeq, err := nextServerSeq(ctx, conn, r.roomKey)
		if err != nil {
			return nil, err
		}
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

	// Bump each distinct room once. Pair-key mode → one turn regardless
	// of recipient count; cwd-mode bumps each synthesized per-edge room.
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

	// Mention resolution + per-recipient best-effort channel push. Happen
	// post-commit, so receivers never see a channel message that isn't
	// backed by an inbox row.
	members, _ := s.roomMembers(ctx, p.SenderCWD, p.SenderLabel, p.PairKey)
	mentions := resolveMentions(p.Body, p.Mentions, members, p.SenderLabel)
	for i := range results {
		results[i].Mentions = mentions
	}
	// v3.7: resolve sender role + agent once for the whole fan-out so
	// every recipient's push meta carries the same from_role/from_agent.
	fromRole, fromAgent, _ := s.GetSessionRoleAgent(ctx, p.SenderCWD, p.SenderLabel)

	// Push meta includes broadcast="1" on every fan-out push, plus a
	// sorted cohort-labels list when the send is a multicast (ToLabel !=
	// ""). Mirrors Python cmd_peer_broadcast:2395-2402.
	var cohortLabels string
	if p.ToLabel != "" && p.ToLabel != "@room" {
		labels := make([]string, 0, len(recipients))
		for _, rr := range recipients {
			labels = append(labels, rr.toLabel)
		}
		sort.Strings(labels)
		cohortLabels = strings.Join(labels, ",")
	}
	for i, r := range recipients {
		if !channelAlive(r.channelSocket) {
			results[i].PushStatus = "no-channel"
			continue
		}
		meta := map[string]string{"to": r.toLabel, "broadcast": "1"}
		// v3.6: see Send — pair_key lets multi-room recipients route
		// replies back into the room the broadcast came from.
		if p.PairKey != "" {
			meta["pair_key"] = p.PairKey
		}
		// v3.7: same role/agent stamping as Send, so broadcast-mode
		// human pings aren't lost in party-mode silence.
		if fromRole != "" {
			meta["from_role"] = fromRole
		}
		if fromAgent != "" {
			meta["from_agent"] = fromAgent
		}
		if cohortLabels != "" {
			meta["cohort"] = cohortLabels
		}
		if len(mentions) > 0 {
			meta["mentions"] = strings.Join(mentions, ",")
		}
		code, _ := pushChannel(r.channelSocket, map[string]any{
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

// RegisterOwnerParams captures the narrow subset of session-register
// needed to auto-register the "owner" label from the web UI. The full
// v3.4 Phase 3 port adds agent / role / pair-key resolution, hook
// session-id discovery, channel socket binding, and system-event
// emission; here we cover only what handleSend's auto-register path
// used to shell out to Python for.
type RegisterOwnerParams struct {
	CWD     string
	PairKey string // required for owner auto-register (pair-key mode only)
	Now     time.Time
}

// RegisterOwner upserts the sessions row for label="owner" in the given
// (cwd, pair_key). Idempotent — safe to call from every web UI request.
// Mirrors Python's session-register --label owner --agent human --role
// owner --session-key owner-web-<pair-key> --force contract.
func (s *SQLiteLocal) RegisterOwner(ctx context.Context, p RegisterOwnerParams) error {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if p.CWD == "" || p.PairKey == "" {
		return fmt.Errorf("RegisterOwner: CWD and PairKey required")
	}
	nowISO := p.Now.Format("2006-01-02T15:04:05Z")
	sessionKey := "owner-web-" + p.PairKey

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// If a prior owner row exists, preserve its auth_token. Otherwise
	// mint a new one. The token is consumed by the bearer-auth path in
	// /api/send, not by the web UI directly — but rotating it on every
	// register would invalidate any external caller's cached copy.
	var existingToken sql.NullString
	err = conn.QueryRowContext(ctx,
		"SELECT auth_token FROM sessions WHERE cwd = ? AND label = 'owner'",
		p.CWD).Scan(&existingToken)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("probe existing: %w", err)
	}
	var token string
	if existingToken.Valid && existingToken.String != "" {
		token = existingToken.String
	} else {
		token = newAuthToken()
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO sessions
		  (cwd, label, agent, role, session_key, channel_socket, pair_key,
		   started_at, last_seen_at, receive_mode, auth_token, auth_token_rotated_at)
		VALUES (?, 'owner', 'human', 'owner', ?, NULL, ?, ?, ?, 'interactive', ?, ?)
		ON CONFLICT(cwd, label) DO UPDATE SET
		  agent = excluded.agent,
		  role = excluded.role,
		  session_key = excluded.session_key,
		  channel_socket = NULL,
		  pair_key = excluded.pair_key,
		  last_seen_at = excluded.last_seen_at,
		  receive_mode = 'interactive',
		  auth_token = COALESCE(sessions.auth_token, excluded.auth_token),
		  auth_token_rotated_at = COALESCE(sessions.auth_token_rotated_at, excluded.auth_token_rotated_at)`,
		p.CWD, sessionKey, p.PairKey, nowISO, nowISO, token, nowISO); err != nil {
		return fmt.Errorf("upsert owner: %w", err)
	}

	// Seed peer_rooms row for the owner's pair_key so home_host routing
	// decisions work. This mirrors the cmd_session_register federation
	// seam (v3.3 Item 1): if the room already exists, preserve its
	// home_host; otherwise stamp with localhost-equivalent. For the web
	// UI the room should already have been created by an explicit
	// room-create call, so the INSERT-OR-IGNORE is a safety net.
	if _, err := conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO peer_rooms (room_key, pair_key, turn_count, home_host)
		VALUES (?, ?, 0, ?)`,
		"pk:"+p.PairKey, p.PairKey, selfHostLabel()); err != nil {
		return fmt.Errorf("seed peer_rooms: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func newAuthToken() string {
	var b [32]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	// base64url without padding, matches Python session-register output.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, 43)
	// Simple base64url encoding, 256 bits → 43 chars.
	var v uint64
	var bits int
	for _, byt := range b {
		v = (v << 8) | uint64(byt)
		bits += 8
		for bits >= 6 {
			bits -= 6
			out = append(out, alphabet[(v>>uint(bits))&0x3F])
		}
	}
	if bits > 0 {
		out = append(out, alphabet[(v<<uint(6-bits))&0x3F])
	}
	return string(out)
}

func selfHostLabel() string {
	if v := os.Getenv("AGENT_COLLAB_SELF_HOST"); v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	h = strings.ToLower(h)
	// Sanitize non-[a-z0-9-] chars same as Python self_host().
	var b strings.Builder
	for _, r := range h {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "localhost"
	}
	return out
}

// ---- helpers ---------------------------------------------------------------

func validateBody(body string) error {
	if len(body) == 0 {
		return ErrEmptyBody
	}
	cap := defaultMaxBodyBytes
	if v := os.Getenv("AGENT_COLLAB_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cap = n
		}
	}
	if len(body) > cap {
		return fmt.Errorf("%w: %d bytes > %d cap", ErrBodyTooLarge, len(body), cap)
	}
	return nil
}

func maxPairTurns() int {
	if v := os.Getenv("AGENT_COLLAB_MAX_PAIR_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxPairTurns
}

// roomKeyFor mirrors Python's _room_key_for: pair-key-scoped rooms share
// a single counter; cwd-only rooms get a synthesized per-edge key
// keyed by the canonical (sorted) label pair.
func roomKeyFor(cwd, a, b, pairKey string) string {
	if pairKey != "" {
		return "pk:" + pairKey
	}
	la, lb := a, b
	if la > lb {
		la, lb = lb, la
	}
	return fmt.Sprintf("cwd:%s#%s+%s", cwd, la, lb)
}

func nullablePairKey(pk string) sql.NullString {
	if pk == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: pk, Valid: true}
}

// resolveTargetCWD mirrors the precedence Python cmd_peer_send uses for
// to-cwd resolution. pair_key mode looks up (pair_key, to_label) across
// cwds; fallback is the sender's own cwd.
func (s *SQLiteLocal) resolveTargetCWD(ctx context.Context, pairKey, toLabel, fallback string) (string, error) {
	if pairKey == "" {
		return fallback, nil
	}
	var cwd string
	err := s.db.QueryRowContext(ctx,
		"SELECT cwd FROM sessions WHERE pair_key = ? AND label = ?",
		pairKey, toLabel).Scan(&cwd)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolveTargetCWD: %w", err)
	}
	return cwd, nil
}

// broadcastRecipients returns the fan-out set: every live peer in the
// sender's scope minus the sender. Ordered by label for determinism.
// optional `onlyLabels` filters down to an explicit multicast subset
// (peer-broadcast's --to flag). toLabel == "" / "@room" means all.
type broadcastRecipient struct {
	toCWD         string
	toLabel       string
	roomKey       string
	channelSocket string
}

func (s *SQLiteLocal) broadcastRecipients(ctx context.Context, senderCWD, senderLabel, pairKey, onlyLabels string) ([]broadcastRecipient, error) {
	var rows *sql.Rows
	var err error
	if pairKey != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, last_seen_at, COALESCE(channel_socket, '')
			FROM sessions WHERE pair_key = ? AND label != ?
			ORDER BY label`, pairKey, senderLabel)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT cwd, label, last_seen_at, COALESCE(channel_socket, '')
			FROM sessions WHERE cwd = ? AND label != ?
			ORDER BY label`, senderCWD, senderLabel)
	}
	if err != nil {
		return nil, fmt.Errorf("broadcastRecipients: %w", err)
	}
	defer rows.Close()

	onlySet := map[string]bool{}
	if onlyLabels != "" && onlyLabels != "@room" {
		for _, l := range strings.Split(onlyLabels, ",") {
			onlySet[strings.TrimSpace(l)] = true
		}
	}

	now := time.Now().UTC()
	var out []broadcastRecipient
	for rows.Next() {
		var cwd, label, lastSeen, sock string
		if err := rows.Scan(&cwd, &label, &lastSeen, &sock); err != nil {
			return nil, err
		}
		if len(onlySet) > 0 && !onlySet[label] {
			continue
		}
		if secondsSinceISO(lastSeen, now) > staleThresholdSecs {
			continue
		}
		out = append(out, broadcastRecipient{
			toCWD:         cwd,
			toLabel:       label,
			roomKey:       roomKeyFor(cwd, senderLabel, label, pairKey),
			channelSocket: sock,
		})
	}
	return out, rows.Err()
}

func (s *SQLiteLocal) roomMembers(ctx context.Context, senderCWD, senderLabel, pairKey string) (map[string]bool, error) {
	members := map[string]bool{senderLabel: true}
	var rows *sql.Rows
	var err error
	if pairKey != "" {
		rows, err = s.db.QueryContext(ctx, "SELECT label FROM sessions WHERE pair_key = ?", pairKey)
	} else {
		rows, err = s.db.QueryContext(ctx, "SELECT label FROM sessions WHERE cwd = ?", senderCWD)
	}
	if err != nil {
		return members, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err == nil {
			members[l] = true
		}
	}
	return members, rows.Err()
}

func resolveMentions(body string, explicit []string, members map[string]bool, selfLabel string) []string {
	found := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		label := m[1]
		if members[label] && label != selfLabel {
			found[label] = true
		}
	}
	for _, m := range explicit {
		if m == "" || m == selfLabel {
			continue
		}
		if !members[m] {
			continue // Python err()s here; for HTTP we skip silently.
		}
		found[m] = true
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fetchRoomState(ctx context.Context, conn *sql.Conn, roomKey string) (turnCount int64, terminated bool, err error) {
	var tc sql.NullInt64
	var termAt sql.NullString
	err = conn.QueryRowContext(ctx,
		"SELECT turn_count, terminated_at FROM peer_rooms WHERE room_key = ?",
		roomKey).Scan(&tc, &termAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("fetchRoomState: %w", err)
	}
	return tc.Int64, termAt.Valid && termAt.String != "", nil
}

func nextServerSeq(ctx context.Context, conn *sql.Conn, roomKey string) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(server_seq), 0) + 1 FROM inbox WHERE room_key = ?",
		roomKey).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("nextServerSeq: %w", err)
	}
	return n, nil
}

func bumpRoom(ctx context.Context, conn *sql.Conn, roomKey string, pairKey sql.NullString) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO peer_rooms (room_key, pair_key, turn_count)
		VALUES (?, ?, 1)
		ON CONFLICT(room_key) DO UPDATE SET turn_count = turn_count + 1`,
		roomKey, pairKey)
	if err != nil {
		return fmt.Errorf("bumpRoom: %w", err)
	}
	return nil
}

func terminateRoom(ctx context.Context, conn *sql.Conn, roomKey string, pairKey sql.NullString, by, nowISO string) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO peer_rooms
		  (room_key, pair_key, turn_count, terminated_at, terminated_by)
		VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(room_key) DO UPDATE SET
		  terminated_at = excluded.terminated_at,
		  terminated_by = excluded.terminated_by`,
		roomKey, pairKey, nowISO, by)
	if err != nil {
		return fmt.Errorf("terminateRoom: %w", err)
	}
	return nil
}

func bumpLastSeen(ctx context.Context, conn *sql.Conn, cwd, label, nowISO string) error {
	_, err := conn.ExecContext(ctx,
		"UPDATE sessions SET last_seen_at = ? WHERE cwd = ? AND label = ?",
		nowISO, cwd, label)
	if err != nil {
		return fmt.Errorf("bumpLastSeen: %w", err)
	}
	return nil
}

func markInboxDirty() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".agent-collab")
	marker := filepath.Join(dir, "inbox-dirty")
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		_ = f.Close()
	}
	_ = os.Chtimes(marker, time.Now(), time.Now())
}

func secondsSinceISO(ts string, now time.Time) int64 {
	t, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		return 1 << 30 // treat unparseable as "very stale"
	}
	return int64(now.Sub(t).Seconds())
}

// ---- ULID (Crockford base32) ----------------------------------------------

const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-char Crockford-base32 ULID: 48-bit ms timestamp
// + 80-bit randomness. Zero-dep port of the Python impl so /api/send's
// server-generated message_ids stay format-compatible.
func NewULID(now time.Time) string {
	ms := now.UnixMilli() & ((1 << 48) - 1)
	var randBytes [10]byte
	_, _ = io.ReadFull(rand.Reader, randBytes[:])
	// 128 bits total: upper 48 ms timestamp, lower 80 random.
	var hi, lo uint64
	hi = uint64(ms)>>16
	lo = uint64(ms&0xFFFF)<<48
	lo |= uint64(randBytes[0])<<40 | uint64(randBytes[1])<<32
	lo |= uint64(randBytes[2])<<24 | uint64(randBytes[3])<<16
	lo |= uint64(randBytes[4])<<8 | uint64(randBytes[5])
	// randBytes[6..9] go into hi's low bits.
	hi = (hi<<32) | uint64(randBytes[6])<<24 | uint64(randBytes[7])<<16 |
		uint64(randBytes[8])<<8 | uint64(randBytes[9])
	out := [26]byte{}
	for i := 25; i >= 0; i-- {
		out[i] = ulidAlphabet[lo&0x1F]
		lo = (lo >> 5) | (hi&0x1F)<<59
		hi >>= 5
	}
	return string(out[:])
}

// channelAlive returns true when the URI looks deliverable. For unix paths,
// stats the socket file (existing pre-dial check preserves the "no-channel"
// vs "push-failed" distinction Python has). For http/https, always returns
// true — reachability is a runtime answer determined by the POST itself.
func channelAlive(uri string) bool {
	if uri == "" {
		return false
	}
	// Fast path: bare unix path (no scheme).
	if !strings.Contains(uri, "://") {
		_, err := os.Stat(uri)
		return err == nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "unix":
		_, err := os.Stat(u.Path)
		return err == nil
	case "http", "https":
		return true
	default:
		return false
	}
}

// ---- Channel push dispatcher ----------------------------------------------

// pushChannel is the v3.5 unified channel-push entry point. The `uri` is
// pulled from sessions.channel_socket (historical name; now holds a URI).
// Scheme dispatch:
//
//   unix:///path/to.sock      → sendOverUnixSocket (same-host)
//   /path/to.sock (bare)      → sendOverUnixSocket (legacy unix-path format)
//   http://host/.../channel-push?label=X   → sendOverHTTPChannelPush
//   https://...               → sendOverHTTPChannelPush (TLS)
//
// Returns (httpStatusCode, bodyOrError). code==0 means the transport failed
// before any status line was read; the string payload carries the diagnostic.
func pushChannel(uri string, payload map[string]any) (int, string) {
	if uri == "" {
		return 0, "no channel"
	}
	// Heuristic: if the value has no scheme (no ://), treat as a bare unix
	// socket path — this matches every channel_socket ever written before v3.5.
	if !strings.Contains(uri, "://") {
		return sendOverUnixSocket(uri, payload)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return 0, "parse uri: " + err.Error()
	}
	switch u.Scheme {
	case "unix":
		return sendOverUnixSocket(u.Path, payload)
	case "http", "https":
		return sendOverHTTPChannelPush(uri, payload)
	default:
		return 0, "unsupported channel scheme: " + u.Scheme
	}
}

// sendOverHTTPChannelPush POSTs the channel payload to a peer host's
// /api/channel-push endpoint. The URL carries label + pair_key as query
// params (set by session-register when the URI was minted). The receiving
// peer-web looks up its own local session with that label and forwards to
// the local unix socket. PoC: unauthenticated. v3.5.1 adds bearer auth.
func sendOverHTTPChannelPush(pushURL string, payload map[string]any) (int, string) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "marshal error: " + err.Error()
	}
	req, err := http.NewRequest("POST", pushURL, bytes.NewReader(body))
	if err != nil {
		return 0, "new request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: channelSocketTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// ---- Unix-socket push -----------------------------------------------------

func sendOverUnixSocket(socketPath string, payload map[string]any) (int, string) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "marshal error"
	}
	req := fmt.Sprintf("POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
	conn, err := net.DialTimeout("unix", socketPath, channelSocketTimeout)
	if err != nil {
		return 0, err.Error()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(channelSocketTimeout))
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, err.Error()
	}
	if _, err := conn.Write(body); err != nil {
		return 0, err.Error()
	}
	var buf strings.Builder
	bb := make([]byte, 4096)
	for {
		n, err := conn.Read(bb)
		if n > 0 {
			buf.Write(bb[:n])
		}
		if err != nil {
			break
		}
	}
	raw := buf.String()
	head, respBody, _ := strings.Cut(raw, "\r\n\r\n")
	line, _, _ := strings.Cut(head, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	code := 0
	if len(parts) >= 2 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			code = n
		}
	}
	return code, respBody
}
