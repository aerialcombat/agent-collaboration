-- SQLite dialect — Track 1 #1 — board-level project_root binding.
--
-- Adds board_settings.project_root TEXT NULL. When non-empty, peer-web's
-- spawnWorker sets cmd.Dir to this path so workers dispatched on this
-- board land in the right project tree instead of inheriting peer-web's
-- own cwd (which would be wherever peer-web was started, typically the
-- agent-collaboration repo).
--
-- Source: linkboard dogfood observation #1. Without this, multi-project
-- deployments are impossible because the operator has to embed the
-- absolute project path in every card body and trust workers to cd
-- before every shell command.
--
-- Backward compat: defaults to NULL. When NULL or empty, spawnWorker
-- falls back to the prior behavior (inherit peer-web cwd). Existing
-- single-project deployments are unaffected.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE board_settings ADD COLUMN project_root TEXT;
-- +goose StatementEnd


-- +goose Down

-- SQLite ≥ 3.35 supports DROP COLUMN.
-- +goose StatementBegin
ALTER TABLE board_settings DROP COLUMN project_root;
-- +goose StatementEnd
