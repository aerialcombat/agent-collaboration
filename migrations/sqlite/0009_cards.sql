-- SQLite dialect — v3.9 kanban cards.
--
-- Introduces the durable-work medium that complements the existing chat
-- medium (peer_rooms + inbox). Chat is synchronous / volatile / broadcast.
-- Cards are addressed, durable, stateful: an agent claims a card, drains
-- it at its own pace, transitions through states, and the work survives
-- across sessions.
--
-- Two tables:
--   cards               — the durable work artifact; one row per ticket.
--   card_dependencies   — M:N blocking edges. Blockee waits for blocker
--                         to reach status='done'. Derived "ready"
--                         (no pending blockers) drives agent queues.
--
-- Scope in v0:
--   - Cards are scoped to a pair_key (one board per room).
--   - home_host mirrors peer_rooms for eventual federation routing.
--   - Grouping via `tags` JSON array (avoid parent/child tree — Vibe
--     Kanban's explicit non-choice; see plans/kanban-poc.md if we write
--     one). Lineage lives in `context_refs.cards[]`.
--   - No card comments table — comments are chat messages in the
--     existing inbox that reference card IDs by convention.
--
-- Status values: 'todo' | 'in_progress' | 'in_review' | 'done' |
--   'cancelled'. A CHECK constraint enforces the enum; adding a new
--   state is a migration, which is the right bar.
--
-- Derived-readiness (no row in card_dependencies for which this card is
-- the blockee AND the blocker is not in ('done', 'cancelled')) is a
-- query-time property, NOT persisted. Avoids cascade-write races when a
-- blocker completes.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE cards (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  pair_key        TEXT NOT NULL,
  home_host       TEXT NOT NULL,
  title           TEXT NOT NULL,
  body            TEXT,
  status          TEXT NOT NULL DEFAULT 'todo'
                    CHECK (status IN ('todo','in_progress','in_review','done','cancelled')),
  needs_role      TEXT,
  claimed_by      TEXT,
  created_by      TEXT NOT NULL,
  priority        INTEGER NOT NULL DEFAULT 0,
  tags            TEXT,
  context_refs    TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  claimed_at      TEXT,
  completed_at    TEXT
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_cards_pair       ON cards(pair_key);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_cards_status     ON cards(status);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_cards_claimed    ON cards(claimed_by) WHERE claimed_by IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_cards_needs_role ON cards(needs_role) WHERE needs_role IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE card_dependencies (
  blocker_id   INTEGER NOT NULL,
  blockee_id   INTEGER NOT NULL,
  created_at   TEXT NOT NULL,
  created_by   TEXT NOT NULL,
  PRIMARY KEY (blocker_id, blockee_id),
  FOREIGN KEY (blocker_id) REFERENCES cards(id) ON DELETE CASCADE,
  FOREIGN KEY (blockee_id) REFERENCES cards(id) ON DELETE CASCADE,
  CHECK (blocker_id != blockee_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_deps_blocker ON card_dependencies(blocker_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_deps_blockee ON card_dependencies(blockee_id);
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_deps_blockee;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_deps_blocker;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS card_dependencies;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_cards_needs_role;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_cards_claimed;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_cards_status;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_cards_pair;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS cards;
-- +goose StatementEnd
