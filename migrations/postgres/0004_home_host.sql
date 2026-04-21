-- Postgres dialect — v3.3 symmetric-federation (Item 1) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Mirrors
-- migrations/sqlite/0004_home_host.sql in logical effect.
--
-- See the SQLite file for rationale (column purpose, backfill handling,
-- immutability invariant).

-- +goose Up

-- +goose StatementBegin
ALTER TABLE peer_rooms ADD COLUMN IF NOT EXISTS home_host TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
ALTER TABLE peer_rooms DROP COLUMN IF EXISTS home_host;
-- +goose StatementEnd
