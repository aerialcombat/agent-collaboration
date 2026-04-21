-- SQLite dialect — v3.3 symmetric-federation (Item 1) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Adds a `home_host` column
-- to peer_rooms so rooms declare which host owns writes. Display layer
-- renders room IDs as `<home_host>:<pair_key>`; CLI routing uses the column
-- to decide between local SQLite write and HTTP POST to a remote peer-web.
--
-- Backfill: the ALTER adds the column with DEFAULT '' (empty sentinel).
-- The Python runtime (scripts/peer-inbox-db.py open_db) stamps empty rows
-- with the current host's self-label on next open. This keeps the SQL
-- dialect-portable and lets the backfill value come from env/hostname
-- resolution that SQL can't do natively.
--
-- Invariant (enforced at the application layer, not SQL): once a row has a
-- non-empty home_host, it is immutable. Room "promotion" — moving a room
-- from one host to another — is out of scope for v3.3.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE peer_rooms ADD COLUMN home_host TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd


-- +goose Down

-- SQLite prior to 3.35 cannot DROP COLUMN; this Down is best-effort,
-- mirroring the 0002 / 0003 migration Down semantics.
-- +goose StatementBegin
ALTER TABLE peer_rooms DROP COLUMN home_host;
-- +goose StatementEnd
