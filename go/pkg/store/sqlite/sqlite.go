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
	// compatibility only and must not be consulted here.
	GooseVersionRequired = 1

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

	// `_pragma=busy_timeout(5000)` ensures we don't hang forever fighting
	// Python over the write lock. `_pragma=journal_mode(WAL)` is set by
	// Python on every open; repeating it here is cheap and idempotent.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
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
	rows, err := tx.QueryContext(ctx, `
		UPDATE inbox
		SET read_at = ?
		WHERE to_cwd = ? AND to_label = ?
		  AND read_at IS NULL
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

// Compile-time assertion: SQLiteLocal implements store.Store.
var _ store.Store = (*SQLiteLocal)(nil)
