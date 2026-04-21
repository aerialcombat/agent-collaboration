package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionRegisterParams carries the verb's inputs in a stable shape so
// the CLI layer can build it from flags and callers can share it in
// tests. Mirrors cmd_session_register in scripts/peer-inbox-db.py.
type SessionRegisterParams struct {
	CWD           string // resolved absolute path
	Label         string // already validated
	Agent         string // already validated
	Role          string // "" = NULL
	SessionKey    string // already resolved (env/args)
	ChannelSocket string // "" = NULL; Phase 4 wiring
	PairKey       string // "" = NULL; else joined-room key
	NewPair       bool   // --new-pair flag (mutex with PairKey)
	ReceiveMode   string // "" = preserve/default; else explicit
	HomeHost      string // "" = local self_host(); else federation label
	Force         bool
}

// SessionRegisterResult carries the computed output the verb needs to
// print. Separated from the CLI layer so a future daemon/web caller can
// reuse RegisterSession without depending on stdout/stderr shape.
type SessionRegisterResult struct {
	Label         string
	Agent         string
	Role          string
	CWD           string
	SessionKey    string
	PairKey       string // "" when cwd-scope
	AuthToken     string // new token if minted; else the preserved one
	AuthTokenNew  bool   // true = first time this row had a token (print it)
	ChannelPaired bool   // true when the caller passed a non-empty ChannelSocket
	IsNewJoin     bool   // pair-key room join event (caller may emit)
}

// ErrLabelCollision matches Python's EXIT_LABEL_COLLISION code (11).
// Returned when a different session is actively holding the same
// (cwd, label) seat OR when a pair-key-scoped label is claimed by
// another cwd.
var ErrLabelCollision = errors.New("store: label collision — seat held by a different session")

// ErrHomeHostImmutable matches the plan v3.3 §2 invariant: home_host
// cannot be changed once stamped on a peer_rooms row.
var ErrHomeHostImmutable = errors.New("store: home_host is immutable — use a different pair_key to switch hosts")

// RegisterSession mirrors cmd_session_register's transactional core:
// reads any existing row, applies the session-key / pair-key /
// receive-mode / auth-token resolution rules, UPSERTs sessions, and
// (when a pair-key is set) ensures a peer_rooms row with the right
// home_host. Returns the result carrying the output-print inputs.
//
// Scope: core path + channel-socket binding. Still deferred
// (Phase 5.1 follow-ups):
//   - emit_system_event (room-join announcements)
//   - sweep_stale_markers_for_label (marker cleanup after register)
//   - recent_seen_sessions fallback (hook-log-session bridge)
//
// Callers resolve ChannelSocket themselves (e.g. the CLI walks its
// process tree for a pending-channel file). The store just persists
// whatever it's handed — keeps RegisterSession OS-agnostic for tests.
func (s *SQLiteLocal) RegisterSession(ctx context.Context, p SessionRegisterParams) (SessionRegisterResult, error) {
	if p.CWD == "" || p.Label == "" || p.Agent == "" || p.SessionKey == "" {
		return SessionRegisterResult{}, fmt.Errorf("register: cwd, label, agent, session_key required")
	}
	if p.PairKey != "" && p.NewPair {
		return SessionRegisterResult{}, fmt.Errorf("register: --pair-key and --new-pair are mutually exclusive")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionRegisterResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		existingAgent       sql.NullString
		existingRole        sql.NullString
		existingLastSeen    sql.NullString
		existingSessionKey  sql.NullString
		existingPairKey     sql.NullString
		existingReceiveMode sql.NullString
		existingAuthToken   sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT agent, role, last_seen_at, session_key, pair_key, receive_mode, auth_token
		FROM sessions WHERE cwd = ? AND label = ?
	`, p.CWD, p.Label).Scan(
		&existingAgent, &existingRole, &existingLastSeen,
		&existingSessionKey, &existingPairKey,
		&existingReceiveMode, &existingAuthToken,
	)
	rowExists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return SessionRegisterResult{}, fmt.Errorf("read existing: %w", err)
	}

	// Idle-collision guard: a different session holding the same label
	// within IDLE_THRESHOLD_SECS fails loud unless --force.
	if rowExists && !p.Force && existingSessionKey.Valid && existingSessionKey.String != p.SessionKey {
		if ls := existingLastSeen.String; ls != "" {
			if t, perr := time.Parse("2006-01-02T15:04:05Z", ls); perr == nil {
				if age := time.Since(t).Seconds(); age < 5*60 {
					return SessionRegisterResult{}, fmt.Errorf("%w: label %q owned by a different session, last seen %.0fs ago; use --force or a different label",
						ErrLabelCollision, p.Label, age)
				}
			}
		}
	}

	// Pair-key resolution:
	//   --pair-key KEY   → use it
	//   --new-pair       → mint a fresh slug
	//   else existing    → preserve
	//   else             → NULL (legacy cwd-only scope)
	pairKey := ""
	switch {
	case p.PairKey != "":
		pairKey = p.PairKey
	case p.NewPair:
		pairKey = generatePairKey()
	case rowExists && existingPairKey.Valid && existingPairKey.String != "":
		pairKey = existingPairKey.String
	}

	// Duplicate-label-in-pair pre-check for a friendlier error than the
	// unique index constraint violation.
	if pairKey != "" {
		var dupCWD string
		qerr := tx.QueryRowContext(ctx, `
			SELECT cwd FROM sessions
			WHERE pair_key = ? AND label = ? AND NOT (cwd = ? AND label = ?)
		`, pairKey, p.Label, p.CWD, p.Label).Scan(&dupCWD)
		if qerr == nil {
			return SessionRegisterResult{}, fmt.Errorf("%w: pair key %q already has a session labeled %q in %s; choose a different label",
				ErrLabelCollision, pairKey, p.Label, dupCWD)
		}
		if qerr != sql.ErrNoRows {
			return SessionRegisterResult{}, fmt.Errorf("dup-label probe: %w", qerr)
		}
	}

	// is_new_join: pair-key-scoped row appears, seat changes hands, or
	// session-key swaps. Silent otherwise.
	isNewJoin := false
	if pairKey != "" {
		if !rowExists {
			isNewJoin = true
		} else if (existingPairKey.String) != pairKey {
			isNewJoin = true
		} else if existingSessionKey.String != p.SessionKey {
			isNewJoin = true
		}
	}

	// receive_mode: explicit > preserved > "interactive".
	receiveMode := "interactive"
	switch {
	case p.ReceiveMode != "":
		receiveMode = p.ReceiveMode
	case rowExists && existingReceiveMode.Valid && existingReceiveMode.String != "":
		receiveMode = existingReceiveMode.String
	}

	// Auth token: preserve when already minted (idempotent refresh);
	// else mint a fresh one. Matches Python's COALESCE in the UPSERT.
	authToken := ""
	authTokenNew := false
	var tokenRotatedAt any = nil
	if rowExists && existingAuthToken.Valid && existingAuthToken.String != "" {
		authToken = existingAuthToken.String
	} else {
		minted, merr := mintAuthToken()
		if merr != nil {
			return SessionRegisterResult{}, merr
		}
		authToken = minted
		authTokenNew = true
		tokenRotatedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Normalize optional NULL-ish fields for SQL.
	roleArg := nullable(p.Role)
	chanArg := nullable(p.ChannelSocket)
	pairArg := nullable(pairKey)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (cwd, label, agent, role, session_key, channel_socket, pair_key,
		                     started_at, last_seen_at, receive_mode, auth_token, auth_token_rotated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cwd, label) DO UPDATE SET
		  agent = excluded.agent,
		  role = excluded.role,
		  session_key = excluded.session_key,
		  channel_socket = excluded.channel_socket,
		  pair_key = excluded.pair_key,
		  started_at = excluded.started_at,
		  last_seen_at = excluded.last_seen_at,
		  receive_mode = excluded.receive_mode,
		  auth_token = COALESCE(sessions.auth_token, excluded.auth_token),
		  auth_token_rotated_at = COALESCE(sessions.auth_token_rotated_at, excluded.auth_token_rotated_at)
	`,
		p.CWD, p.Label, p.Agent, roleArg, p.SessionKey, chanArg, pairArg,
		now, now, receiveMode, authToken, tokenRotatedAt,
	); err != nil {
		return SessionRegisterResult{}, fmt.Errorf("upsert session: %w", err)
	}

	if pairKey != "" {
		homeHost := p.HomeHost
		if homeHost == "" {
			homeHost = SelfHost()
		}
		roomKey := "pk:" + pairKey
		var existingHome sql.NullString
		qerr := tx.QueryRowContext(ctx,
			`SELECT home_host FROM peer_rooms WHERE room_key = ?`, roomKey).Scan(&existingHome)
		switch {
		case qerr == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO peer_rooms (room_key, pair_key, turn_count, home_host) VALUES (?, ?, 0, ?)`,
				roomKey, pairKey, homeHost,
			); err != nil {
				return SessionRegisterResult{}, fmt.Errorf("insert peer_rooms: %w", err)
			}
		case qerr == nil:
			if p.HomeHost != "" && existingHome.String != p.HomeHost {
				return SessionRegisterResult{}, fmt.Errorf("%w: room %s already bound to host %q",
					ErrHomeHostImmutable, pairKey, existingHome.String)
			}
		default:
			return SessionRegisterResult{}, fmt.Errorf("probe peer_rooms: %w", qerr)
		}
	}

	if err := tx.Commit(); err != nil {
		return SessionRegisterResult{}, fmt.Errorf("commit: %w", err)
	}

	return SessionRegisterResult{
		Label: p.Label, Agent: p.Agent, Role: p.Role, CWD: p.CWD,
		SessionKey: p.SessionKey, PairKey: pairKey,
		AuthToken: authToken, AuthTokenNew: authTokenNew,
		ChannelPaired: p.ChannelSocket != "", IsNewJoin: isNewJoin,
	}, nil
}

// mintAuthToken mints a 32-byte URL-safe base64 token (trailing =
// stripped), matching Python's base64.urlsafe_b64encode(token_bytes(32))
// .rstrip("="). Always produces 43 ASCII chars.
func mintAuthToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint auth_token: %w", err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "="), nil
}

// nullable converts an empty string into a NULL-valued driver arg so
// TEXT columns keep their NULL semantics (vs. an empty string literal).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
