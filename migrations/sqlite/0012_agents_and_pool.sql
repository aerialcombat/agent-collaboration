-- SQLite dialect — v3.12 agent registry + per-board pool.
--
-- See plans/v3.12-agent-pool-with-standup.md.
--
-- Two new tables + two new columns:
--
--   agents          — global agent registry. A row is a *config*, not a
--                     running process. systemd template units consume
--                     these rows to spawn long-lived sessions.
--   pool_members    — per-board roster. (pair_key, agent_id) is unique;
--                     count > 1 lets a single agent definition fill N
--                     parallel slots. priority breaks auto-select ties.
--
--   cards.assigned_to_agent_id — designated assignment override.
--                     NULL = use auto-select / chat claim.
--   card_runs.agent_id        — structured FK identifying the agent
--                     that ran the dispatch. Coexists with
--                     card_runs.worker_label (free-form display).
--
-- This single migration covers v3.12.1 (agents CRUD), v3.12.2 (pool
-- composition), and v3.12.4-.5 (dispatcher + run linkage). Sub-phases
-- after this are pure code, no further migrations.
--
-- Forward-only convention: a Down section exists for rollback safety
-- but is not exercised in production.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE agents (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  label         TEXT    NOT NULL UNIQUE,
  runtime       TEXT    NOT NULL CHECK (runtime IN ('claude','pi')),
  worker_cmd    TEXT,
  role          TEXT,
  model_config  TEXT,
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT    NOT NULL,
  updated_at    TEXT    NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agents_role    ON agents(role)    WHERE role IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agents_enabled ON agents(enabled) WHERE enabled = 1;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE pool_members (
  pair_key   TEXT    NOT NULL,
  agent_id   INTEGER NOT NULL,
  count      INTEGER NOT NULL DEFAULT 1 CHECK (count >= 1),
  priority   INTEGER NOT NULL DEFAULT 0,
  added_by   TEXT,
  added_at   TEXT    NOT NULL,
  PRIMARY KEY (pair_key, agent_id),
  FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_pool_members_pair_key ON pool_members(pair_key);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards ADD COLUMN assigned_to_agent_id INTEGER
  REFERENCES agents(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE card_runs ADD COLUMN agent_id INTEGER
  REFERENCES agents(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_card_runs_agent_id ON card_runs(agent_id) WHERE agent_id IS NOT NULL;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_card_runs_agent_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE card_runs DROP COLUMN agent_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE cards DROP COLUMN assigned_to_agent_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_pool_members_pair_key;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS pool_members;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agents_enabled;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agents_role;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS agents;
-- +goose StatementEnd
