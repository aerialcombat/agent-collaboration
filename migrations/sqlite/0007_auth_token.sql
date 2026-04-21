-- SQLite dialect — v3.3 symmetric-federation (Item 7) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Adds a per-session
-- bearer token so peer-web's /api/send can authenticate cross-host
-- senders: tailnet = transport trust, not identity. A compromised-but-
-- authorized tailnet device should not be able to impersonate arbitrary
-- agents in arbitrary rooms.
--
-- The token is minted by session-register when the row is created or
-- refreshed, and printed to stdout exactly once so the operator can copy
-- it to the client host's remotes.json. Subsequent register calls for
-- the same (cwd, label, session_key) keep the token — safe for
-- idempotent re-registrations. --force-rotate-token (future flag)
-- replaces the token; this migration only creates storage.
--
-- Index is a partial unique index (WHERE auth_token IS NOT NULL) so
-- rows that haven't been token-minted yet (legacy + pre-0007 sessions)
-- don't collide. /api/send's lookup is `SELECT ... FROM sessions WHERE
-- auth_token = ?` which this index covers.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN auth_token TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN auth_token_rotated_at TEXT;
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

-- SQLite prior to 3.35 cannot DROP COLUMN; best-effort.
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN auth_token_rotated_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN auth_token;
-- +goose StatementEnd
