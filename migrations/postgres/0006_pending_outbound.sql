-- Postgres dialect — v3.3 symmetric-federation (Item 5) schema addition.
-- Mirrors migrations/sqlite/0006_pending_outbound.sql; see that file for
-- rationale.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS pending_outbound (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id       TEXT NOT NULL UNIQUE,
  home_host        TEXT NOT NULL,
  pair_key         TEXT NOT NULL,
  from_label       TEXT NOT NULL,
  to_label         TEXT NOT NULL,
  body             TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0,
  last_attempt_at  TEXT,
  last_error       TEXT
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_pending_outbound_host_room
  ON pending_outbound(home_host, pair_key, id);
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_pending_outbound_host_room;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS pending_outbound;
-- +goose StatementEnd
