-- SQLite dialect — v3.12.4 D3 — extend card_events.kind CHECK to allow
-- 'assigned' and 'unassigned' kinds emitted by AssignCardToAgent.
--
-- The 0010 migration created card_events with a CHECK constraint
-- enumerating kinds. Extending an enum on SQLite means dropping +
-- recreating the table because CHECK can't be altered in place. Data
-- preserved by copy-through.
--
-- Forward-only convention: a Down section exists for rollback safety
-- but is not exercised in production.

-- +goose Up

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
SELECT id, card_id, kind, author, body, meta, created_at FROM card_events;
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


-- +goose Down

-- +goose StatementBegin
CREATE TABLE card_events_v0 (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id     INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN (
        'comment', 'status_change', 'claim',
        'body_update', 'run_dispatch', 'run_complete'
    )),
    author      TEXT    NOT NULL,
    body        TEXT    NOT NULL DEFAULT '',
    meta        TEXT    NOT NULL DEFAULT '{}',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    FOREIGN KEY (card_id) REFERENCES cards(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO card_events_v0 (id, card_id, kind, author, body, meta, created_at)
SELECT id, card_id, kind, author, body, meta, created_at
  FROM card_events
 WHERE kind NOT IN ('assigned', 'unassigned');
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE card_events;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE card_events_v0 RENAME TO card_events;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id    ON card_events(card_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_events_card_id_id ON card_events(card_id, id);
-- +goose StatementEnd
