-- Postgres dialect — mirrors migrations/sqlite/0003_daemon_cli_session_id.sql
-- in logical effect. Native IF NOT EXISTS on ALTER TABLE matches the
-- 0001/0002 pattern.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS daemon_cli_session_id TEXT;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS daemon_cli_session_id;
-- +goose StatementEnd
