-- Postgres dialect — mirrors migrations/sqlite/0002_daemon_mode_columns.sql
-- in logical effect. Only divergence from SQLite is native
-- `IF NOT EXISTS` on ALTER TABLE statements, matching the 0001 pattern.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS claimed_at   TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS completed_at TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS claim_owner  TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_inbox_daemon_inflight
  ON inbox(claim_owner, claimed_at)
  WHERE claimed_at IS NOT NULL AND completed_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS receive_mode TEXT NOT NULL DEFAULT 'interactive'
  CHECK (receive_mode IN ('interactive', 'daemon'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS daemon_state TEXT NOT NULL DEFAULT 'open'
  CHECK (daemon_state IN ('open', 'closed'));
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_inbox_daemon_inflight;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS daemon_state;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS receive_mode;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS claim_owner;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS completed_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS claimed_at;
-- +goose StatementEnd
