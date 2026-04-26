-- SQLite dialect — v3.11 standing drainer + durable runs.
--
-- Phase 1 of the cards async-drain migration (plans/v3.11-cards-phase1-
-- standing-drainer.md). Flips the dispatch model from one-shot wave to
-- per-board standing drainer, and makes every run durable.
--
-- Two new tables + one reserved column:
--
--   card_runs       — one row per worker dispatch. Source of truth for
--                     "what is running right now" once Phase 1 lands.
--                     Drainer reads this table to compute capacity
--                     deficit; reaper scans it on peer-web boot.
--   board_settings  — per-pair_key drainer config. auto_drain replaces
--                     the in-memory Server.drainers map; max_concurrent
--                     bounds parallelism per board.
--
--   cards.promotion_due_at — reserved for Phase 3 deferred auto-promote.
--                     Phase 1 leaves it NULL.
--
-- Phase headroom (these reservations make Phases 2-6 additive, not
-- breaking, when they land):
--
--   card_runs.host          — Phase 6 cross-host: which host ran the
--                             worker. Phase 1 always = self_host().
--   card_runs.worker_label  — Phase 1 free-form (e.g. "drainer-1234-...").
--                             Phase 4 promotes to FK on workers.label.
--   card_runs.trigger       — Phase 1: 'manual' | 'drainer'. Phase 4
--                             adds 'matchmaker'.
--   board_settings.auto_promote — replaces the old ?auto_promote=1 query
--                             param. Phase 3 reuses this column to drive
--                             the deferred-promote timer.
--
-- Forward-only convention: a Down section exists for rollback safety
-- but is not exercised in production.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE card_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  card_id       INTEGER NOT NULL,
  pair_key      TEXT    NOT NULL,
  host          TEXT    NOT NULL,
  worker_label  TEXT    NOT NULL,
  pid           INTEGER,
  started_at    TEXT    NOT NULL,
  ended_at      TEXT,
  status        TEXT    NOT NULL
                  CHECK (status IN ('running','completed','failed','cancelled','lost')),
  exit_code     INTEGER,
  log_path      TEXT,
  trigger       TEXT    NOT NULL DEFAULT 'manual'
                  CHECK (trigger IN ('manual','drainer','matchmaker')),
  FOREIGN KEY (card_id) REFERENCES cards(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_runs_card_id   ON card_runs(card_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_runs_status    ON card_runs(status);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_runs_pair_key  ON card_runs(pair_key);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE board_settings (
  pair_key            TEXT PRIMARY KEY,
  auto_drain          INTEGER NOT NULL DEFAULT 0,
  max_concurrent      INTEGER NOT NULL DEFAULT 3,
  auto_promote        INTEGER NOT NULL DEFAULT 0,
  poll_interval_secs  INTEGER NOT NULL DEFAULT 5,
  updated_at          TEXT    NOT NULL,
  updated_by          TEXT    NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards ADD COLUMN promotion_due_at TEXT;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
ALTER TABLE cards DROP COLUMN promotion_due_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS board_settings;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_runs_pair_key;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_runs_status;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_runs_card_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS card_runs;
-- +goose StatementEnd
