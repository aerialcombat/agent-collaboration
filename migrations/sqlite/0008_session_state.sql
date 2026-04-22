-- SQLite dialect — v3.8 agent activity monitoring.
--
-- Adds `state` + `state_changed_at` to sessions so each agent row can
-- carry a lightweight busy/idle indicator, visible in peer-web and
-- federated across hosts. Distinct from last_seen_at (which measures
-- "has this process done anything lately"); state measures "is this
-- agent currently generating a turn."
--
-- Values: "active" (turn in progress) | "idle" (waiting) | NULL
-- (unknown — pre-v3.8 sessions, or agents without hook support).
--
-- Claude Code's UserPromptSubmit / Stop lifecycle hooks set the state;
-- the peer-inbox-hook Go binary also flips to "active" inline on every
-- prompt submission (zero extra process). Non-hook agents can call
-- `agent-collab session state <active|idle>` to set it manually.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN state TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN state_changed_at TEXT;
-- +goose StatementEnd


-- +goose Down

-- SQLite prior to 3.35 cannot DROP COLUMN; best-effort.
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN state_changed_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN state;
-- +goose StatementEnd
