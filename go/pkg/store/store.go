// Package store declares the Store interface — the narrow surface the hook
// binary and (later) the CLI use for all persistence. v1 has one
// implementation (SQLiteLocal); v2 adds PostgresRemote and a composite that
// routes reads/writes between them. The hook must never touch an sqlite3
// driver directly — only this interface.
package store

import (
	"context"
	"errors"
)

// ErrSchemaTooOld is returned when the DB schema version is below what this
// binary requires. Callers (e.g. the hook) should fail-open: log + exit 0,
// since the user's turn must never block on an agent-collab internal issue.
var ErrSchemaTooOld = errors.New("store: schema version older than required; run newer agent-collab to migrate")

// ErrNoSession is returned when the hook is invoked in a cwd with no
// registered session (and no env-var-identifiable session key). Caller
// fail-opens.
var ErrNoSession = errors.New("store: no session registered for this cwd")

// ErrPathDrift is returned when a marker file records a cwd different
// from its own on-disk location — typical signature of the marker being
// copied or the enclosing directory having been moved after register.
// Python errors out with EXIT_PATH_DRIFT; Go returns this error and the
// caller (hook) fail-opens + logs so Python can reissue the helpful
// "re-run agent-collab session register" message on the next prompt.
var ErrPathDrift = errors.New("store: marker path-drift — marker was moved or copied")

// ErrReceiveModeMismatch is returned by daemon-mode verbs when the
// session's receive_mode column does not match the verb's expectation
// (Topic 3 §3.4 guarantee (b), verb-entry gate). `DaemonModeClaim` and
// `DaemonModeComplete` require `receive_mode = 'daemon'`. Callers
// translate this into the CLI-level EX_DATAERR (65) fail-loud surface.
var ErrReceiveModeMismatch = errors.New("store: receive-mode mismatch — daemon verb invoked on non-daemon session")

// ErrStaleClaim is returned by DaemonModeComplete when the completion
// UPDATE matches zero rows (Topic 3 §3.4 guarantee (d), alpha §B). Zero
// rows means the claim was reaped by the sweeper between claim-time and
// complete-time — the daemon's work on that batch is rejected and the
// sweeper-then-reclaim cycle redelivers on the next claim pass.
var ErrStaleClaim = errors.New("store: stale claim — completion UPDATE matched 0 rows; claim was reaped")

// Session identifies a (cwd, label) pair resolved from markers + env vars.
type Session struct {
	CWD   string
	Label string
}

// InboxMessage is the hot-path row shape. It intentionally omits the
// read_at column because ReadUnread only ever returns not-yet-read rows.
type InboxMessage struct {
	ID         int64
	FromCWD    string
	FromLabel  string
	Body       string
	CreatedAt  string // ISO-8601 UTC
	RoomKey    string
}

// ReapedClaim is one row returned by DaemonModeSweep. Keeps `ClaimOwner`
// even though the sweep UPDATE sets `claimed_at = NULL` only — Topic 3
// alpha §A fix preserves `claim_owner` as an audit trail so downstream
// observability can surface which daemon's batch got reaped.
type ReapedClaim struct {
	ID         int64
	ToCWD      string
	ToLabel    string
	ClaimOwner string
}

// Store is the minimal surface the hook binary needs. It is deliberately
// small — adding methods here is a design change that needs review, not a
// drive-by.
type Store interface {
	// ResolveSelf returns the Session registered for this cwd + env-var-
	// discoverable session key. Walks up from cwd looking for
	// .agent-collab/sessions/<hash>.json markers. Returns ErrNoSession if
	// nothing is found.
	ResolveSelf(ctx context.Context, cwd string, sessionKeyEnv string) (Session, error)

	// ReadUnread claims and returns all unread messages for the given
	// session in a single BEGIN IMMEDIATE ... COMMIT. Also bumps
	// last_seen_at on the sessions row. Atomic with respect to concurrent
	// readers — UPDATE ... RETURNING guarantees no double-delivery.
	ReadUnread(ctx context.Context, self Session) ([]InboxMessage, error)

	// Close releases any underlying driver handles.
	Close() error
}
