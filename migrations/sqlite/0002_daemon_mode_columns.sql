-- SQLite dialect — Topic 3 v0 (auto-reply daemon) schema additions per
-- plans/v3.x-topic-3-implementation-scope.md §3.1. All columns are
-- additive NULLable (or NOT NULL with safe default for CHECK-constrained
-- text columns) so existing interactive-mode rows are unaffected.
--
-- The SQL partition guarantee (§3.4 (a)) — adding `AND claimed_at IS NULL`
-- to interactive receive WHERE clauses — lives in the Python and Go
-- runtimes, not this migration. Both runtimes must land the predicate in
-- the same commit that migrates the schema (per W1 pattern). This file
-- creates the columns; the runtime guards consume them.

-- +goose Up

-- inbox: daemon-mode claim state. All three columns NULLable so
-- interactive-mode rows never have to set them; claimed_at = NULL
-- AND read_at IS NULL is the canonical "unclaimed, unread" state.
-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN claimed_at TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN completed_at TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN claim_owner TEXT;
-- +goose StatementEnd

-- Index on the in-flight predicate (claimed_at NOT NULL AND
-- completed_at NULL). Supports the sweeper scan + closed-state claim
-- preflight; tiny partial index so it costs nothing on interactive-mode
-- rows.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_inbox_daemon_inflight
  ON inbox(claim_owner, claimed_at)
  WHERE claimed_at IS NOT NULL AND completed_at IS NULL;
-- +goose StatementEnd

-- sessions: receive_mode gates verb-entry (§3.4 (b)); daemon_state
-- gates claim-time closed-check (§3.4 (e)). CHECK constraints enforce
-- the allowed value set at the storage layer.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN receive_mode TEXT NOT NULL DEFAULT 'interactive'
  CHECK (receive_mode IN ('interactive', 'daemon'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN daemon_state TEXT NOT NULL DEFAULT 'open'
  CHECK (daemon_state IN ('open', 'closed'));
-- +goose StatementEnd


-- +goose Down

-- SQLite prior to 3.35 cannot DROP COLUMN; this Down is best-effort,
-- mirroring the 0001 migration's Down semantics.
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_inbox_daemon_inflight;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN daemon_state;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN receive_mode;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN claim_owner;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN completed_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN claimed_at;
-- +goose StatementEnd
