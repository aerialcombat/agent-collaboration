-- SQLite dialect — v3.10 card_events.
--
-- Linear/GitHub-style timeline on each card. Two streams interleaved
-- chronologically:
--
--   1. Comments     — free-form, human-or-worker authored (kind='comment').
--   2. System events — auto-recorded on state transitions (kind in
--                      'status_change', 'claim', 'body_update',
--                      'run_dispatch', 'run_complete').
--
-- One table, one shape. The drawer renders both interleaved by
-- created_at ASC; filtering by kind is a query concern.
--
-- Why one table instead of "comments" + "activity": Linear/GitHub
-- prove the unified-timeline UX is what humans want. Two tables
-- means two queries + interleave-on-render. The kind column is the
-- only thing that distinguishes them.
--
-- Worker/agent flow:
--   1. Run dispatched → handleCardRun inserts run_dispatch event with
--      meta = {worker_label, pid, log_path}.
--   2. Worker spawned by claude -p posts mid-progress comments via
--      the new card-comment CLI verb / card_comment MCP tool.
--   3. Worker completes → card-update-status to in_review fires the
--      status_change event auto-recorded by UpdateCardStatus.
--
-- Body field stores rendered/displayable text. Meta JSON carries
-- structured data the renderer needs (old/new status, fields touched,
-- log path, etc).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE card_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id     INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN (
        'comment', 'status_change', 'claim',
        'body_update', 'run_dispatch', 'run_complete'
    )),
    author      TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    meta        TEXT    NOT NULL DEFAULT '{}',  -- JSON
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (card_id) REFERENCES cards(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id ON card_events(card_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_events_card_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS card_events;
-- +goose StatementEnd
