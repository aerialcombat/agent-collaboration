-- SQLite dialect — applied by `go/cmd/migrate` against the local
-- ~/.agent-collab/sessions.db. `goose_db_version` prevents re-apply, so
-- the bare `ALTER TABLE ADD COLUMN` statements below are safe — they
-- only run once.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN user_id TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'default';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN idempotency_key TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN client_seq INTEGER;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS outbox (
  id              INTEGER PRIMARY KEY,
  workspace_id    TEXT NOT NULL DEFAULT 'default',
  idempotency_key TEXT NOT NULL,
  envelope_bytes  BLOB NOT NULL,
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

-- Legacy compatibility: populate meta.schema_version = 10 so tooling that
-- still inspects the meta table (pre-goose runtimes, external scripts)
-- sees a sensible value. Authoritative version source is goose_db_version.
-- +goose StatementBegin
INSERT INTO meta (key, value) VALUES ('schema_version', '10')
  ON CONFLICT(key) DO UPDATE SET value = excluded.value;
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
-- SQLite prior to 3.35 cannot DROP COLUMN; this Down is best-effort.
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN client_seq;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN idempotency_key;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN workspace_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN user_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS meta;
-- +goose StatementEnd
