-- Postgres dialect — v3.3 symmetric-federation (Item 7) schema addition.
-- Mirrors migrations/sqlite/0007_auth_token.sql; see that file for rationale.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS auth_token TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS auth_token_rotated_at TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_auth_token
  ON sessions(auth_token)
  WHERE auth_token IS NOT NULL;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_auth_token;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS auth_token_rotated_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN IF EXISTS auth_token;
-- +goose StatementEnd
