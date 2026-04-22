-- Postgres dialect — v3.8 agent activity monitoring.
-- Mirrors migrations/sqlite/0008_session_state.sql; see that file for rationale.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS state TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS state_changed_at TEXT;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS state_changed_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS state;
-- +goose StatementEnd
