-- SQLite dialect — v3.12.4.5/.6 — card decomposition (kind, splittable) and
-- handoffs (track_handoffs flag + 'split'/'handoff' card_event kinds).
--
-- Plan: plans/v3.12-decomposition-and-handoffs.md §4.
--
-- Three card columns are added:
--   * kind            — 'task' (default) or 'epic'. Epics get the decomposer
--                       prompt path in the drainer instead of a regular worker.
--   * splittable      — 0 (default) or 1. When 1, the worker prompt gains the
--                       mid-session split addendum.
--   * track_handoffs  — 0 (default) or 1. When 1, worker prompt gains the
--                       handoff discipline + buildWorkerPrompt prepends the
--                       latest handoff event.
--
-- Existing rows get the defaults (kind='task', splittable=0, track_handoffs=0)
-- so v3.12.4 behavior is preserved unchanged.
--
-- card_events.kind CHECK is extended with 'split' and 'handoff'. SQLite cannot
-- ALTER CHECK in place, so the table is recreated with copy-through (same
-- idiom as 0013).
--
-- Forward-only convention: a Down section exists for rollback safety but is
-- not exercised in production.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE cards ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'
    CHECK (kind IN ('task','epic'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards ADD COLUMN splittable INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards ADD COLUMN track_handoffs INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_cards_kind ON cards(kind) WHERE kind != 'task';
-- +goose StatementEnd

-- Recreate card_events with the extended CHECK constraint.
-- +goose StatementBegin
CREATE TABLE card_events_v3 (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id     INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN (
        'comment', 'status_change', 'claim',
        'body_update', 'run_dispatch', 'run_complete',
        'assigned', 'unassigned',
        'split', 'handoff'
    )),
    author      TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    meta        TEXT    NOT NULL DEFAULT '{}',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    FOREIGN KEY (card_id) REFERENCES cards(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO card_events_v3 (id, card_id, kind, author, body, meta, created_at)
SELECT id, card_id, kind, author, body, meta, created_at FROM card_events;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE card_events;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE card_events_v3 RENAME TO card_events;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id    ON card_events(card_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id_id ON card_events(card_id, id);
-- +goose StatementEnd


-- +goose Down

-- Drop the kind index first (depends on cards.kind column).
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_cards_kind;
-- +goose StatementEnd

-- SQLite ≥ 3.35 supports ALTER TABLE DROP COLUMN.
-- +goose StatementBegin
ALTER TABLE cards DROP COLUMN track_handoffs;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards DROP COLUMN splittable;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards DROP COLUMN kind;
-- +goose StatementEnd

-- Recreate card_events with the v0013 CHECK (no split/handoff).
-- +goose StatementBegin
CREATE TABLE card_events_v2 (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id     INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN (
        'comment', 'status_change', 'claim',
        'body_update', 'run_dispatch', 'run_complete',
        'assigned', 'unassigned'
    )),
    author      TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    meta        TEXT    NOT NULL DEFAULT '{}',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    FOREIGN KEY (card_id) REFERENCES cards(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO card_events_v2 (id, card_id, kind, author, body, meta, created_at)
SELECT id, card_id, kind, author, body, meta, created_at
  FROM card_events
 WHERE kind NOT IN ('split', 'handoff');
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE card_events;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE card_events_v2 RENAME TO card_events;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id    ON card_events(card_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id_id ON card_events(card_id, id);
-- +goose StatementEnd
