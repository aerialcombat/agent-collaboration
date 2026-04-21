-- SQLite dialect — v3.3 symmetric-federation (Item 5) schema addition per
-- plans/v3.3-symmetric-federation-scoping.md §5. Adds a local outbound
-- queue so laptop-originated remote sends that hit a network failure
-- (tailnet flake, orange reboot) buffer on-disk and flush on the next
-- successful contact — no silent drops, no user-visible hangs.
--
-- Scope: only peer-send targeting a remote-home room writes here. Local
-- SQLite writes never enqueue (their failure path is a hard error on a
-- local disk problem, unrelated to federation).
--
-- TTL: rows older than 24h are dropped by the flush path with a stderr
-- warning. Stale messages re-entering an archived room is worse than
-- losing a day-old flake.
--
-- Ordering: FIFO per-room on flush. Per-sender cross-room ordering is
-- NOT guaranteed — the plan §5 Item 5 commits to per-sender FIFO only,
-- matching backend-architect's recommendation.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS pending_outbound (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
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

-- Flush order: per-room FIFO. Index supports the ORDER BY id scan
-- partitioned by home_host (flush only rooms that share the currently-
-- reachable host) and by pair_key (reconstruct room order).
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
