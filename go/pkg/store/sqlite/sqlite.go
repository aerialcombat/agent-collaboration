// Package sqlite implements store.Store against the shared ~/.agent-collab/
// sessions.db that Python also writes. Pure-Go driver (modernc.org/sqlite,
// no CGo) so the hook binary ships as a static single-file distribution.
//
// Schema authority is goose (github.com/pressly/goose/v3) via the
// peer-inbox-migrate binary under migrations/sqlite/. Open probes
// goose_db_version and returns store.ErrSchemaTooOld if the required
// migration hasn't been applied yet; the hook fail-opens and the bash
// wrapper routes to Python, which auto-invokes peer-inbox-migrate. This
// impl never writes schema — it reads + asserts.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"agent-collaboration/go/pkg/store"
)

const (
	// GooseVersionRequired is the minimum applied goose migration version
	// this build expects. `goose_db_version` is the authoritative source —
	// `meta.schema_version` is written by the same migration for legacy-
	// compatibility only and must not be consulted here. Bumped from 1 to 2
	// for the Topic 3 0002 migration that adds claimed_at / completed_at /
	// claim_owner on inbox + receive_mode / daemon_state on sessions: the
	// hook's ReadUnread query (this file) now references claimed_at in its
	// WHERE clause (SQL partition per Topic 3 §3.4 (a)). Bumped from 2 to 3
	// for the Topic 3 v0.1 (Arch D) 0003 migration that adds
	// sessions.daemon_cli_session_id; daemon's resume helpers read this
	// column at spawn time, so pre-0003 DBs must fail-open here before
	// reaching the SQL. Bumped 3 → 4 for v3.3 symmetric-federation's 0004
	// migration that adds peer_rooms.home_host; peer-web's /rooms.json and
	// routing helpers consult the column, so pre-0004 DBs must fail the
	// schema check first. Bumped 4 → 5 for v3.3 Item 4's 0005 migration
	// that adds inbox.server_seq for per-room monotonic ack ids. Bumped
	// 5 → 6 for v3.3 Item 5's 0006 migration that adds pending_outbound
	// for the laptop-side federation queue. Bumped 6 → 7 for v3.3 Item 7's
	// 0007 migration that adds sessions.auth_token — peer-web /api/send's
	// bearer-token validation reads this column, so pre-0007 DBs must
	// fail the schema check first. Bumped 7 → 8 for v3.8's 0008 migration
	// that adds sessions.state + sessions.state_changed_at for agent
	// activity monitoring; the hook binary writes these on every prompt.
	// Bumped 9 → 10 for v3.10's 0010 migration that adds the
	// card_events table backing the per-card timeline (Linear/GitHub
	// style activity + comments stream). Bumped 10 → 12 for v3.12's
	// 0012 migration (agents + pool_members + cards.assigned_to_agent_id
	// + card_runs.agent_id) which subsumes the missed v3.11 Phase 1 bump
	// (0011: card_runs + board_settings); the agents store layer reads
	// the new columns, so pre-0012 DBs must fail the schema check first.
	// Bumped 12 → 13 for v3.12.4 D3's 0013 migration that extends the
	// card_events.kind CHECK to allow 'assigned' / 'unassigned' kinds
	// emitted by AssignCardToAgent — pre-0013 DBs would reject the audit
	// event insert with a constraint error. Bumped 13 → 14 for
	// v3.12.4.5/.6's 0014 migration that adds cards.kind / cards.splittable /
	// cards.track_handoffs columns and extends card_events.kind CHECK with
	// 'split' / 'handoff'; the drainer reads kind/splittable to choose the
	// decomposer prompt path and to append the split addendum, and emits
	// 'split'/'handoff' events — pre-0014 DBs would either ignore the new
	// columns (silent default behavior) or reject the new event kinds.
	// Bumped 14 → 15 for Track 1 #1's 0015 migration that adds
	// board_settings.project_root for the per-board cwd binding
	// spawnWorker reads — pre-0015 DBs would silently ignore project_root
	// and inherit peer-web's cwd (the linkboard dogfood failure mode).
	GooseVersionRequired = 15

	sessionsDirRel = ".agent-collab/sessions"
)

// SQLiteLocal is the v1 Store implementation.
type SQLiteLocal struct {
	db   *sql.DB
	path string
}

// Open connects to the local sessions.db, validates schema_version, and
// returns a ready Store. The DB path resolves from the AGENT_COLLAB_INBOX_DB
// env var or falls back to ~/.agent-collab/sessions.db.
func Open(ctx context.Context) (*SQLiteLocal, error) {
	path := os.Getenv("AGENT_COLLAB_INBOX_DB")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		path = filepath.Join(home, ".agent-collab", "sessions.db")
	}

	// Brand-new install: the DB file may not exist yet. Python creates it
	// on its next invocation; we fail-open here.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, store.ErrSchemaTooOld
	}

	// busy_timeout 15s gives concurrent writers room to queue without
	// failing — 10 parallel "Run" dispatches each do a small write tx;
	// even at 50ms each they fit under the timeout 30x over.
	// _txlock=immediate makes BeginTx start with BEGIN IMMEDIATE so
	// reads inside a write tx can't deadlock with another writer
	// promoting from snapshot to write (SQLITE_BUSY_SNAPSHOT).
	// journal_mode=WAL lets readers run while a writer holds the lock.
	// foreign_keys(on) enables CASCADE / SET NULL clauses; SQLite has
	// FK enforcement off by default per-connection. v3.12 pool_members
	// + assigned_to_agent_id + card_runs.agent_id depend on it.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)

	// Verify schema. ErrSchemaTooOld bubbles up; caller fail-opens.
	if err := checkSchemaVersion(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteLocal{db: db, path: path}, nil
}

func checkSchemaVersion(ctx context.Context, db *sql.DB) error {
	// goose_db_version is the authoritative version source (v3.0 W1 review
	// round ruling: A3 resolution (a) — migrations are applied via
	// `github.com/pressly/goose/v3` through the peer-inbox-migrate binary).
	// Absence of the table means the DB has never been migrated; caller
	// fail-opens, bash wrapper routes to Python, Python auto-invokes the
	// migrate binary.
	var tableExists int
	err := db.QueryRowContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version'",
	).Scan(&tableExists)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrSchemaTooOld
	}
	if err != nil {
		return fmt.Errorf("probe goose_db_version table: %w", err)
	}

	// MAX(version_id) WHERE is_applied reads the highest successfully
	// applied migration. COALESCE to 0 covers the "table exists but empty"
	// shape that can occur mid-rollback.
	var max int64
	err = db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1",
	).Scan(&max)
	if err != nil {
		return fmt.Errorf("read goose_db_version: %w", err)
	}
	if max < GooseVersionRequired {
		return store.ErrSchemaTooOld
	}
	return nil
}

// Close releases the underlying sql.DB handle.
func (s *SQLiteLocal) Close() error { return s.db.Close() }

// ResolveSelf walks up from cwd looking for .agent-collab/sessions/ markers.
// Matches the resolution order in scripts/peer-inbox-db.py:resolve_self
// minus the --as flag path (the hook never passes --as; it identifies the
// session purely from env vars or a sole-marker directory).
//
// Mirrors Python's `_verify_and_return_marker` path-drift guard:
// rejects markers whose recorded `cwd` differs from the marker's own
// on-disk parent-parent-parent directory. Returns ErrPathDrift so the
// caller can distinguish "marker is suspicious" from "no marker at all."
func (s *SQLiteLocal) ResolveSelf(ctx context.Context, cwd, sessionKeyEnv string) (store.Session, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return store.Session{}, fmt.Errorf("abs cwd: %w", err)
	}

	if sessionKeyEnv != "" {
		if sess, err := findMarkerByKey(absCwd, sessionKeyEnv); err == nil {
			return sess, nil
		} else if errors.Is(err, store.ErrPathDrift) {
			return store.Session{}, err
		}
		// Otherwise: env-var set but no matching marker. Try sole-
		// marker convenience path next.
	}

	if sessDir := findAnySessionsDir(absCwd); sessDir != "" {
		entries, err := os.ReadDir(sessDir)
		if err == nil {
			var onlyMarker string
			count := 0
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				onlyMarker = filepath.Join(sessDir, e.Name())
				count++
			}
			if count == 1 {
				if sess, err := readMarker(onlyMarker); err == nil {
					return sess, nil
				} else if errors.Is(err, store.ErrPathDrift) {
					return store.Session{}, err
				}
			}
		}
	}

	return store.Session{}, store.ErrNoSession
}

// findMarkerByKey mirrors Python's find_marker_by_key — walks up from cwd
// looking for .agent-collab/sessions/<sha256(key)[:16]>.json. Returns
// ErrPathDrift if the found marker's recorded cwd disagrees with its
// on-disk location, ErrNoSession if no marker is reachable.
func findMarkerByKey(cwd, sessionKey string) (store.Session, error) {
	sum := sha256.Sum256([]byte(sessionKey))
	target := hex.EncodeToString(sum[:])[:16] + ".json"
	cur := cwd
	for {
		candidate := filepath.Join(cur, sessionsDirRel, target)
		if sess, err := readMarker(candidate); err == nil {
			return sess, nil
		} else if errors.Is(err, store.ErrPathDrift) {
			return store.Session{}, err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return store.Session{}, store.ErrNoSession
		}
		cur = parent
	}
}

func findAnySessionsDir(cwd string) string {
	cur := cwd
	for {
		candidate := filepath.Join(cur, sessionsDirRel)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// readMarker loads a marker JSON file and enforces Python's
// _verify_and_return_marker path-drift invariant: the marker's recorded
// `cwd` must match the marker's actual on-disk owner
// (<cwd>/.agent-collab/sessions/<file>.json → <cwd>). Returns
// ErrPathDrift if they disagree. Unreadable/malformed markers return
// ErrNoSession so callers can fall through to the sole-marker or
// no-session branches without surfacing a "suspicious marker" error.
func readMarker(path string) (store.Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return store.Session{}, store.ErrNoSession
	}
	var m struct {
		CWD   string `json:"cwd"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return store.Session{}, store.ErrNoSession
	}
	if m.CWD == "" || m.Label == "" {
		return store.Session{}, store.ErrNoSession
	}

	// marker lives at <markerOwner>/.agent-collab/sessions/<file>.json,
	// so three parents up is the session's canonical cwd. Comparing
	// symlink-resolved paths on both sides catches the "marker was
	// copied / directory was moved after register" case.
	markerOwner := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	ownerResolved := resolveOrSelf(markerOwner)
	recordedResolved := resolveOrSelf(m.CWD)
	if ownerResolved != recordedResolved {
		return store.Session{}, store.ErrPathDrift
	}
	return store.Session{CWD: ownerResolved, Label: m.Label}, nil
}

// resolveOrSelf returns filepath.EvalSymlinks(p) if it succeeds, or the
// cleaned absolute path otherwise. Mirrors Python's
// Path.resolve(strict=True) + fallback pattern in _verify_and_return_marker.
func resolveOrSelf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// ReadUnread runs the hot-path claim-and-mark: BEGIN, UPDATE inbox SET
// read_at=now ... RETURNING (acquires the writer lock), UPDATE sessions
// last_seen_at, COMMIT. Returns the rows that this call just claimed.
func (s *SQLiteLocal) ReadUnread(ctx context.Context, self store.Session) ([]store.InboxMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// Defer rollback; overridden by the COMMIT below on success. sql.ErrTxDone
	// after commit is not fatal.
	defer func() { _ = tx.Rollback() }()

	// The UPDATE ... RETURNING below acquires the writer lock itself;
	// it's the only writer in this tx, so no up-front BEGIN IMMEDIATE
	// is needed (and modernc rejects a nested BEGIN anyway).
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	// SQL partition (Topic 3 §3.4 guarantee (a)): AND claimed_at IS NULL
	// excludes daemon-mode in-flight rows from the interactive read path.
	// Daemon-claimed rows (claimed_at set, read_at still NULL) are invisible
	// to the hook hot-path by construction, preventing the double-consumption
	// failure mode idle-birch surfaced during Topic 3 scoping review. Stays
	// byte-identical to pre-Topic-3 hook output because the existing inbox
	// state has zero rows with claimed_at NOT NULL (no daemon writers yet),
	// so tests/hook-parity.sh fixtures remain unchanged.
	rows, err := tx.QueryContext(ctx, `
		UPDATE inbox
		SET read_at = ?
		WHERE to_cwd = ? AND to_label = ?
		  AND read_at IS NULL
		  AND claimed_at IS NULL
		RETURNING id, from_cwd, from_label, body, created_at, COALESCE(room_key, '')
	`, now, self.CWD, self.Label)
	if err != nil {
		return nil, fmt.Errorf("update inbox: %w", err)
	}
	defer rows.Close()

	var out []store.InboxMessage
	for rows.Next() {
		var m store.InboxMessage
		if err := rows.Scan(&m.ID, &m.FromCWD, &m.FromLabel, &m.Body, &m.CreatedAt, &m.RoomKey); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows err: %w", err)
	}
	rows.Close()

	// Bump last_seen_at on the sender's session row, matching Python's
	// bump_last_seen. Keeps "active" state accurate for other peers'
	// broadcast-live-filter queries.
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?
		WHERE cwd = ? AND label = ?
	`, now, self.CWD, self.Label); err != nil {
		return nil, fmt.Errorf("bump last_seen: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Topic 3 v0 daemon-mode verbs (Go parity with scripts/peer-inbox-db.py
// `_cmd_peer_receive_daemon_mode / _cmd_peer_receive_complete /
// _cmd_peer_receive_sweep`). These methods serve the new `go/cmd/peer-inbox/`
// binary + (later) the daemon binary in commit 7. They do not share rows
// with the interactive `ReadUnread` path — the SQL partition (§3.4 (a),
// `claimed_at IS NULL` in ReadUnread's WHERE) keeps daemon-claimed rows
// invisible to interactive reads.
// -----------------------------------------------------------------------------

// sessionReceiveMode reads the receive_mode column for (cwd, label) or
// returns "interactive" if the session row is missing or the column is
// empty. Mirrors Python's `_sessions_receive_mode` (§3.4 guarantee (b)
// verb-entry gate).
func (s *SQLiteLocal) sessionReceiveMode(ctx context.Context, cwd, label string) (string, error) {
	var mode sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT receive_mode FROM sessions WHERE cwd = ? AND label = ?",
		cwd, label,
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "interactive", nil
	}
	if err != nil {
		return "", fmt.Errorf("read receive_mode: %w", err)
	}
	if !mode.Valid || mode.String == "" {
		return "interactive", nil
	}
	return mode.String, nil
}

// sessionDaemonState reads the daemon_state column for (cwd, label) or
// returns "open" if the session row is missing or the column is empty.
// Mirrors Python's `_sessions_daemon_state` (§3.4 guarantee (e) claim-
// time closed-state check).
func (s *SQLiteLocal) sessionDaemonState(ctx context.Context, cwd, label string) (string, error) {
	var state sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT daemon_state FROM sessions WHERE cwd = ? AND label = ?",
		cwd, label,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "open", nil
	}
	if err != nil {
		return "", fmt.Errorf("read daemon_state: %w", err)
	}
	if !state.Valid || state.String == "" {
		return "open", nil
	}
	return state.String, nil
}

// DaemonModeClaim atomically claims all unclaimed, unread rows addressed
// to `self` into daemon-mode state (SET claimed_at, claim_owner). Returns
// the rows that this call claimed. Mirrors Python's
// `_cmd_peer_receive_daemon_mode` (Topic 3 §2.1 + §3.4 (a)(b)(e)).
//
// Preflights:
//   - §3.4 (b): session's receive_mode must be 'daemon'; else return
//     store.ErrReceiveModeMismatch.
//   - §3.4 (e): session's daemon_state must be 'open'; if 'closed',
//     return an empty slice with no error (daemon is intentionally idle
//     and sweeper-requeued rows must not trigger a new spawn).
//
// The UPDATE's WHERE includes `read_at IS NULL AND claimed_at IS NULL`,
// which together with the writer lock make the claim atomic against
// concurrent daemons and against the interactive hot path (which also
// filters `claimed_at IS NULL`).
func (s *SQLiteLocal) DaemonModeClaim(ctx context.Context, self store.Session) ([]store.InboxMessage, error) {
	mode, err := s.sessionReceiveMode(ctx, self.CWD, self.Label)
	if err != nil {
		return nil, err
	}
	if mode != "daemon" {
		return nil, store.ErrReceiveModeMismatch
	}
	state, err := s.sessionDaemonState(ctx, self.CWD, self.Label)
	if err != nil {
		return nil, err
	}
	if state == "closed" {
		// §3.4 (e): closed-state preflight returns empty batch without
		// mutating inbox. No error — caller (daemon) treats as "no work."
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	rows, err := tx.QueryContext(ctx, `
		UPDATE inbox
		SET claimed_at = ?, claim_owner = ?
		WHERE to_cwd = ? AND to_label = ?
		  AND read_at IS NULL
		  AND claimed_at IS NULL
		RETURNING id, from_cwd, from_label, body, created_at, COALESCE(room_key, '')
	`, now, self.Label, self.CWD, self.Label)
	if err != nil {
		return nil, fmt.Errorf("claim inbox: %w", err)
	}

	var out []store.InboxMessage
	for rows.Next() {
		var m store.InboxMessage
		if err := rows.Scan(&m.ID, &m.FromCWD, &m.FromLabel, &m.Body, &m.CreatedAt, &m.RoomKey); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claim row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claim rows err: %w", err)
	}
	rows.Close()

	// Bump last_seen_at to mirror Python's bump_last_seen on the daemon
	// session's row.
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?
		WHERE cwd = ? AND label = ?
	`, now, self.CWD, self.Label); err != nil {
		return nil, fmt.Errorf("bump last_seen: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// DaemonModeComplete marks the caller's in-flight claim complete by
// stamping completed_at on every row matching
// `to_cwd, to_label, claim_owner = self.Label AND claimed_at IS NOT NULL
// AND completed_at IS NULL`. Returns the list of ids completed on
// success. Mirrors Python's `_cmd_peer_receive_complete` (§3.4 (d),
// alpha §B).
//
// Zero-row UPDATE is fail-loud: the claim was reaped by the sweeper
// between claim-time and complete-time, or was already completed. We
// return store.ErrStaleClaim so the caller surfaces the contract
// violation (the daemon's batch work is rejected; sweeper-then-reclaim
// handles redelivery).
//
// Also preflights receive_mode='daemon' (§3.4 (b)) — calling --complete
// against an interactive-mode session is a CLI-level mistake.
func (s *SQLiteLocal) DaemonModeComplete(ctx context.Context, self store.Session) ([]int64, error) {
	mode, err := s.sessionReceiveMode(ctx, self.CWD, self.Label)
	if err != nil {
		return nil, err
	}
	if mode != "daemon" {
		return nil, store.ErrReceiveModeMismatch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	rows, err := tx.QueryContext(ctx, `
		UPDATE inbox
		SET completed_at = ?
		WHERE to_cwd = ? AND to_label = ?
		  AND claim_owner = ?
		  AND claimed_at IS NOT NULL
		  AND completed_at IS NULL
		RETURNING id
	`, now, self.CWD, self.Label, self.Label)
	if err != nil {
		return nil, fmt.Errorf("complete inbox: %w", err)
	}

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan complete row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("complete rows err: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		// Don't commit — no work was done. ErrStaleClaim is the §3.4 (d)
		// fail-loud signal.
		return nil, store.ErrStaleClaim
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return ids, nil
}

// DaemonModeSweep reaps in-flight claims older than `ttl` by reverting
// `claimed_at` to NULL. `claim_owner` is intentionally preserved as an
// audit trail (Topic 3 alpha §A fix — DO NOT also nullify claim_owner).
// Returns the reaped rows so callers can log which daemon's batch got
// requeued. Mirrors Python's `_cmd_peer_receive_sweep` (§3.3).
//
// A row reaped by this pass re-appears in the next DaemonModeClaim call,
// which is the recovery loop for the legitimate-slow-but-reaped race
// (alpha §B).
func (s *SQLiteLocal) DaemonModeSweep(ctx context.Context, ttl time.Duration) ([]store.ReapedClaim, error) {
	reaped, _, err := s.DaemonModeSweepWithCutoff(ctx, ttl)
	return reaped, err
}

// DaemonModeSweepWithCutoff is like DaemonModeSweep but also returns the
// exact cutoff ISO-8601 string that appeared in the WHERE clause, so
// CLI callers can emit it in JSON output without recomputing `now` on
// their own (which would drift by up to a second from the actual
// cutoff). Matches Python's single-value cutoff in the `--format json`
// payload.
func (s *SQLiteLocal) DaemonModeSweepWithCutoff(ctx context.Context, ttl time.Duration) ([]store.ReapedClaim, string, error) {
	if ttl <= 0 {
		return nil, "", fmt.Errorf("sweep ttl must be positive, got %v", ttl)
	}
	cutoff := time.Now().UTC().Add(-ttl).Format("2006-01-02T15:04:05Z")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, cutoff, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		UPDATE inbox
		SET claimed_at = NULL
		WHERE claimed_at IS NOT NULL
		  AND completed_at IS NULL
		  AND claimed_at < ?
		RETURNING id, to_cwd, to_label, claim_owner
	`, cutoff)
	if err != nil {
		return nil, cutoff, fmt.Errorf("sweep inbox: %w", err)
	}

	var reaped []store.ReapedClaim
	for rows.Next() {
		var r store.ReapedClaim
		var owner sql.NullString
		if err := rows.Scan(&r.ID, &r.ToCWD, &r.ToLabel, &owner); err != nil {
			rows.Close()
			return nil, cutoff, fmt.Errorf("scan reap row: %w", err)
		}
		if owner.Valid {
			r.ClaimOwner = owner.String
		}
		reaped = append(reaped, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, cutoff, fmt.Errorf("reap rows err: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, cutoff, fmt.Errorf("commit: %w", err)
	}
	return reaped, cutoff, nil
}

// SetDaemonState flips sessions.daemon_state to the given value ('open'
// or 'closed') for the (cwd, label) session. Topic 3 §6 Layer 1 uses
// this to transition a daemon dormant after a content-stop sentinel
// arrives; Layer 2 bullet 1 names the same operation. The daemon
// calls this directly (no verb-level CLI needed in v0).
//
// Validates the state string against the CHECK constraint shape that
// the 0002 migration installed (CHECK (daemon_state IN ('open',
// 'closed'))) so callers get a clean Go-level error rather than a
// raw SQLite constraint failure.
func (s *SQLiteLocal) SetDaemonState(ctx context.Context, self store.Session, newState string) error {
	switch newState {
	case "open", "closed":
	default:
		return fmt.Errorf("daemon_state must be 'open' or 'closed', got %q", newState)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET daemon_state = ?
		WHERE cwd = ? AND label = ?
	`, newState, self.CWD, self.Label)
	if err != nil {
		return fmt.Errorf("set daemon_state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// No row with this (cwd, label). Caller likely misconfigured
		// — surface loud so operator can fix rather than silently
		// losing the state transition.
		return fmt.Errorf("no session row for cwd=%q label=%q", self.CWD, self.Label)
	}
	return nil
}

// SetDaemonCLISessionID persists the captured CLI-vendor session-ID for
// the (cwd, label) session. Topic 3 v0.1 (Architecture D) §5.2: the
// daemon's spawn helpers call this on first capture (codex regex-banner
// per §6.1; gemini --list-sessions delta-snapshot per §6.2).
//
// The ID is opaque from the daemon's perspective — UUIDs for both
// codex and gemini in current CLI versions, but the column is TEXT so
// future CLI vendors can use any string identity. Daemon NEVER
// introspects the CLI vendor's session-store contents (§3.4 invariant 2,
// pointer-only).
func (s *SQLiteLocal) SetDaemonCLISessionID(ctx context.Context, self store.Session, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET daemon_cli_session_id = ?
		WHERE cwd = ? AND label = ?
	`, sessionID, self.CWD, self.Label)
	if err != nil {
		return fmt.Errorf("set daemon_cli_session_id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no session row for cwd=%q label=%q", self.CWD, self.Label)
	}
	return nil
}

// GetDaemonCLISessionID reads the persisted CLI session-ID for the
// (cwd, label) session. Returns empty string + nil when the column is
// NULL (no captured session yet, or explicitly reset). Returns
// non-nil error only on SQL failure or session-not-found.
//
// Daemon's resume-helper calls this at every batch start: empty string
// means "spawn fresh + capture"; non-empty means "use this session-ID
// in the spawn argv per §4.1/§4.2".
func (s *SQLiteLocal) GetDaemonCLISessionID(ctx context.Context, self store.Session) (string, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT daemon_cli_session_id
		FROM sessions
		WHERE cwd = ? AND label = ?
	`, self.CWD, self.Label).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no session row for cwd=%q label=%q", self.CWD, self.Label)
		}
		return "", fmt.Errorf("get daemon_cli_session_id: %w", err)
	}
	if !id.Valid {
		return "", nil
	}
	return id.String, nil
}

// GetSessionAgentAndCLISessionID reads both the `agent` string and the
// persisted CLI session-ID for the (cwd, label) session in a single
// SELECT. Topic 3 v0.2 §8.1: the reset verb (a separate process from the
// daemon) needs `sessions.agent` to gate the pi-specific file-delete
// side-effect; reading both columns atomically avoids a race where the
// agent check could observe one row and the clear observes a different
// one if the operator re-registers mid-reset.
//
// Returns (agent, cliSessionID, error). agent is always non-empty on
// success (schema `agent TEXT NOT NULL`). cliSessionID is "" when the
// column is NULL.
func (s *SQLiteLocal) GetSessionAgentAndCLISessionID(ctx context.Context, self store.Session) (string, string, error) {
	var agent string
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT agent, daemon_cli_session_id
		FROM sessions
		WHERE cwd = ? AND label = ?
	`, self.CWD, self.Label).Scan(&agent, &id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("no session row for cwd=%q label=%q", self.CWD, self.Label)
		}
		return "", "", fmt.Errorf("get session agent + daemon_cli_session_id: %w", err)
	}
	if !id.Valid {
		return agent, "", nil
	}
	return agent, id.String, nil
}

// ClearDaemonCLISessionID NULLs the persisted CLI session-ID for the
// (cwd, label) session. Used by the operator reset verb
// (peer-inbox daemon-reset-session per §8.1) and by the auto-GC hook on
// L1 content-stop in transitionClosed (§8.2).
//
// Idempotent per §3.4 invariant 3: clearing an already-NULL column
// returns nil (success) — operators recovering from confused state
// shouldn't need to know the prior state. The "row matched but value
// was already NULL" case is indistinguishable from "row matched and
// value was non-NULL" via RowsAffected on UPDATE in SQLite (always 1
// when the WHERE matches), so the idempotency is structural.
func (s *SQLiteLocal) ClearDaemonCLISessionID(ctx context.Context, self store.Session) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET daemon_cli_session_id = NULL
		WHERE cwd = ? AND label = ?
	`, self.CWD, self.Label)
	if err != nil {
		return fmt.Errorf("clear daemon_cli_session_id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no session row for cwd=%q label=%q", self.CWD, self.Label)
	}
	return nil
}

// Compile-time assertion: SQLiteLocal implements store.Store.
var _ store.Store = (*SQLiteLocal)(nil)
