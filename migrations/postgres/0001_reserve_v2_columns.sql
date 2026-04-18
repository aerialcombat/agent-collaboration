-- Postgres dialect — applied by `go/cmd/migrate` against the v2 cloud
-- Postgres instance, and by the schema-portability CI job against a
-- postgres:16 container. `goose_db_version` tracks applied state.
--
-- This file mirrors migrations/sqlite/0001_reserve_v2_columns.sql in
-- logical effect. The only divergence is native `IF NOT EXISTS` on the
-- ALTER TABLE statements, which SQLite lacks.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS user_id        TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS workspace_id   TEXT NOT NULL DEFAULT 'default';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS client_seq     INTEGER;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS outbox (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  workspace_id    TEXT NOT NULL DEFAULT 'default',
  idempotency_key TEXT NOT NULL,
  envelope_bytes  BYTEA NOT NULL,
  created_at      TEXT NOT NULL,
  sent_at         TEXT,
  attempts        INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_workspace_idem
  ON outbox(workspace_id, idempotency_key);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_inbox_workspace_idem
  ON inbox(workspace_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
-- +goose StatementEnd

-- Legacy compatibility: see SQLite file for rationale.
-- +goose StatementBegin
INSERT INTO meta (key, value) VALUES ('schema_version', '10')
  ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_inbox_workspace_idem;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_outbox_workspace_idem;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS outbox;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS client_seq;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS idempotency_key;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS workspace_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS user_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS meta;
-- +goose StatementEnd
