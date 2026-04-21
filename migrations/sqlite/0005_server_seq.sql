-- SQLite dialect — v3.3 symmetric-federation (Item 4) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Adds a per-room monotonic
-- sequence to inbox so clients can:
--   (a) confirm durability with a server-assigned id that outlives process
--       restarts (matters for the outbound queue — Item 5);
--   (b) poll with `since_seq=N` and get read-your-writes guarantees without
--       racing against the per-row autoincrement `id`, which is global and
--       not per-room dense;
--   (c) retry an HTTP POST whose ack was lost and get back the same seq —
--       the existing `idempotency_key` unique index handles dedup, and
--       this column lets the server return the original seq on retry.
--
-- Existing columns reused: `idempotency_key` (from 0001) is the client-
-- generated ULID. Unique index on (workspace_id, idempotency_key) already
-- exists in 0001, so dedup is free; this migration only adds server_seq.
--
-- Assignment strategy (enforced at the application layer): inside the
-- BEGIN IMMEDIATE transaction that inserts the inbox row, compute
-- `next_seq = COALESCE(MAX(server_seq), 0) + 1 FROM inbox WHERE room_key = ?`
-- and write it in the same INSERT. SQLite's per-DB write serialization
-- makes this safe without extra locking.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE inbox ADD COLUMN server_seq INTEGER;
-- +goose StatementEnd

-- Partial index on (room_key, server_seq DESC) — supports the MAX lookup
-- inside peer-send and the `since_seq=N` poll pattern. Partial on
-- `server_seq IS NOT NULL` so pre-0005 rows (null server_seq) cost
-- nothing.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_inbox_room_seq
  ON inbox(room_key, server_seq)
  WHERE server_seq IS NOT NULL;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_inbox_room_seq;
-- +goose StatementEnd

-- SQLite prior to 3.35 cannot DROP COLUMN; best-effort.
-- +goose StatementBegin
ALTER TABLE inbox DROP COLUMN server_seq;
-- +goose StatementEnd
