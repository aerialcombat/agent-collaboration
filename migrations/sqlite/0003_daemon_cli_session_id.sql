-- SQLite dialect — Topic 3 v0.1 (Architecture D, CLI-native session-ID
-- pass-through) schema addition per
-- plans/v3.x-topic-3-arch-d-scoping.md §5.1. Single additive NULLable
-- column on sessions: persists the captured CLI vendor's session
-- identity (UUID for codex; UUID for gemini, translated to current
-- index at resume time per v3 amendment §4.2). NULL means "no captured
-- session, or explicitly reset".
--
-- The capture + translation logic lives in the Go daemon (commit 3 of
-- the Arch D ladder); this migration only creates storage. The reset
-- verb (peer-inbox daemon-reset-session + Python equivalent) lands in
-- commit 2 alongside the Go store methods.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN daemon_cli_session_id TEXT;
-- +goose StatementEnd


-- +goose Down

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN daemon_cli_session_id;
-- +goose StatementEnd
