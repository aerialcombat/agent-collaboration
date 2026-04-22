package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// v3.5 channel-push gateway support. Peers on other hosts POST to this
// host's peer-web /api/channel-push?label=X&pair_key=Y, and peer-web
// forwards to the local unix socket for that session. The forwarding
// logic lives in the store so it can reuse the same URI parser + dial
// helpers as the internal push path.

// ErrNoLocalSession is returned when a channel-push arrives for a
// (label, pair_key) that has no registered session on this host. Caller
// surfaces as HTTP 404.
var ErrNoLocalSession = errors.New("store: no local session for label+pair_key")

// ErrNoLocalChannel is returned when a session exists but has no
// deliverable channel (empty channel_socket, or a channel URI that
// isn't a unix path — HTTP-only sessions can't receive a forwarded push
// because the receiving peer-web has no outbound channel to Claude).
// Caller surfaces as HTTP 410 Gone.
var ErrNoLocalChannel = errors.New("store: session has no local unix channel")

// ForwardLocalPush receives a channel-push payload that arrived over
// HTTP from a remote host and delivers it into the local session's
// context by writing to its unix socket. Mirrors the same unix-push
// used for same-host writes — callers on the other end (the remote
// host's Send / BroadcastLocal / EmitSystemEvent) see no difference.
//
// Resolution:
//   - if pairKey == "": match any row with (label) — useful for cwd-mode
//     sessions that have no pair_key.
//   - else: match (label, pair_key) exactly.
//
// Returns the HTTP status code returned by the local unix socket
// (typically 200) and the unix-socket response body for debugging.
func (s *SQLiteLocal) ForwardLocalPush(
	ctx context.Context,
	label, pairKey string,
	payload map[string]any,
) (int, string, error) {
	var channelURI sql.NullString
	var err error
	if pairKey == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT channel_socket FROM sessions WHERE label = ? AND pair_key IS NULL`,
			label,
		).Scan(&channelURI)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT channel_socket FROM sessions WHERE label = ? AND pair_key = ?`,
			label, pairKey,
		).Scan(&channelURI)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNoLocalSession
	}
	if err != nil {
		return 0, "", fmt.Errorf("ForwardLocalPush: lookup: %w", err)
	}
	if !channelURI.Valid || channelURI.String == "" {
		return 0, "", ErrNoLocalChannel
	}
	// Only unix/bare-path URIs are forwardable — we're the terminal hop.
	// If a session's channel_uri is itself an http://... URL, forwarding
	// would create a redirect loop. Bail.
	uri := channelURI.String
	if contains(uri, "://") {
		if !startsWith(uri, "unix://") {
			return 0, "", ErrNoLocalChannel
		}
	}
	code, body := pushChannel(uri, payload)
	if code == 0 {
		return 0, body, fmt.Errorf("forward unix push: %s", body)
	}

	// v3.8 phase 2 follow-up: a successful forward proves the receiving
	// MCP is alive and responsive. Bump last_seen_at on the row we
	// just delivered to so it doesn't drift into the stale bucket
	// just because it never originates sends of its own. Without this,
	// agents whose only traffic is *inbound* (e.g. laptop-client
	// receiving from a federated orange) look disconnected after the
	// 24hr stale threshold even though the socket was being written
	// to moments ago.
	if code == 200 {
		nowISO := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if pairKey == "" {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE sessions SET last_seen_at = ?
				 WHERE label = ? AND pair_key IS NULL`,
				nowISO, label)
		} else {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE sessions SET last_seen_at = ?
				 WHERE label = ? AND pair_key = ?`,
				nowISO, label, pairKey)
		}
	}
	return code, body, nil
}

// Tiny helpers — avoid importing strings here just for two calls that
// are both exact prefix/substring checks. Keeps this file dep-light.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
