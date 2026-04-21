-- Postgres dialect — v3.3 symmetric-federation (Item 4) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Mirrors
-- migrations/sqlite/0005_server_seq.sql in logical effect. See that file
-- for rationale.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN IF NOT EXISTS server_seq BIGINT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_inbox_room_seq
  ON inbox(room_key, server_seq)
  WHERE server_seq IS NOT NULL;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_inbox_room_seq;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN IF EXISTS server_seq;
-- +goose StatementEnd
