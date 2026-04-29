#!/usr/bin/env bash
# tests/cards-v312-agents-crud.sh — v3.12.1 agent registry smoke.
#
# Drives the five agent-* CLI verbs against an isolated temp DB. No
# peer-web, no dispatcher — this is the schema + store layer + CLI
# slice in isolation.
#
# Scenarios:
#   A1 — Create + plain-render: claude + pi agents land with
#         expected fields; list shows both.
#   A2 — Duplicate label: second create with same label fails
#         with a clean error (exit != 0, stderr matches).
#   A3 — Bad runtime: --runtime gemini rejected by CLI/store
#         (CHECK constraint).
#   A4 — Update partial: change role + disable; absent fields stay.
#   A5 — Filter enabled-only: list excludes disabled rows.
#   A6 — Lookup by id and label: both paths return same row.
#   A7 — Delete + cascade-not-found: deleting an agent referenced
#         by pool_members removes the pool row; subsequent get-by-id
#         returns not-found.
#   A8 — Empty model_config + empty role: NULL columns round-trip
#         as absent fields in JSON output.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
MIG_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
MIG_DIR="$PROJECT_ROOT/migrations/sqlite"

for b in "$PI_BIN" "$MIG_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b — run go build in go/"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t cards-v312-agents.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"

export AGENT_COLLAB_INBOX_DB="$DB"

cleanup() { rm -rf "$TMP_RAW"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "$*"; }

# Bootstrap minimum tables peer-inbox-migrate doesn't own, then run
# every migration up to 0012.
sqlite3 "$DB" >/dev/null <<'SQL'
PRAGMA journal_mode=WAL;
CREATE TABLE sessions (cwd TEXT NOT NULL, label TEXT NOT NULL, agent TEXT NOT NULL,
  role TEXT, session_key TEXT, channel_socket TEXT, pair_key TEXT,
  started_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, PRIMARY KEY (cwd, label));
CREATE TABLE inbox (id INTEGER PRIMARY KEY AUTOINCREMENT, to_cwd TEXT NOT NULL,
  to_label TEXT NOT NULL, from_cwd TEXT NOT NULL, from_label TEXT NOT NULL,
  body TEXT NOT NULL, created_at TEXT NOT NULL, read_at TEXT, room_key TEXT);
CREATE TABLE peer_rooms (room_key TEXT PRIMARY KEY, pair_key TEXT,
  turn_count INTEGER NOT NULL DEFAULT 0, terminated_at TEXT, terminated_by TEXT);
SQL

"$MIG_BIN" -driver sqlite -dsn "$DB" -dir "$MIG_DIR" up >/dev/null 2>&1 \
  || fail "schema migrate"
[ "$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")" = "13" ] \
  || fail "goose version != 13"

# --- A1: create both runtimes + list --------------------------------------
out_claude=$("$PI_BIN" agent-create --label test-claude --runtime claude --role impl \
  --config '{"model":"claude-opus-4-7"}' --format json) \
  || fail "A1 claude create rc=$?"
echo "$out_claude" | python3 -c 'import sys,json;a=json.load(sys.stdin);assert a["label"]=="test-claude" and a["runtime"]=="claude" and a["enabled"]==True,a' \
  || fail "A1 claude json shape"

out_pi=$("$PI_BIN" agent-create --label test-pi --runtime pi --role review --format json) \
  || fail "A1 pi create rc=$?"
echo "$out_pi" | python3 -c 'import sys,json;a=json.load(sys.stdin);assert a["label"]=="test-pi" and a["runtime"]=="pi",a' \
  || fail "A1 pi json shape"

list_all=$("$PI_BIN" agent-list --format json) || fail "A1 list rc=$?"
n_all=$(echo "$list_all" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
[ "$n_all" = "2" ] || fail "A1 list count: got $n_all, want 2"
ok "A1 ok — both agents created and visible in list"

# --- A2: duplicate label rejected -----------------------------------------
if "$PI_BIN" agent-create --label test-claude --runtime claude >/dev/null 2>&1; then
  fail "A2 dup unexpectedly succeeded"
fi
ok "A2 ok — duplicate label rejected"

# --- A3: bad runtime rejected ---------------------------------------------
if "$PI_BIN" agent-create --label test-bad --runtime gemini >/dev/null 2>&1; then
  fail "A3 bad runtime unexpectedly succeeded"
fi
# Sanity: row didn't land.
n_bad=$(sqlite3 "$DB" "SELECT COUNT(*) FROM agents WHERE label='test-bad';")
[ "$n_bad" = "0" ] || fail "A3 stale row in agents table"
ok "A3 ok — bad runtime rejected"

# --- A4: partial update ---------------------------------------------------
"$PI_BIN" agent-update --label test-claude --enabled off --role review --format json >/dev/null \
  || fail "A4 update rc=$?"
got=$("$PI_BIN" agent-get --label test-claude --format json) || fail "A4 get rc=$?"
echo "$got" | python3 -c 'import sys,json;a=json.load(sys.stdin);assert a["enabled"]==False and a["role"]=="review" and a["runtime"]=="claude",a' \
  || fail "A4 update fields incorrect"
# Untouched fields preserved.
echo "$got" | python3 -c 'import sys,json;a=json.load(sys.stdin);assert a["model_config"]=="{\"model\":\"claude-opus-4-7\"}",a' \
  || fail "A4 model_config wiped"
ok "A4 ok — partial update preserves untouched fields"

# --- A5: filter enabled-only ----------------------------------------------
enabled_only=$("$PI_BIN" agent-list --enabled-only --format json) || fail "A5 list rc=$?"
n_en=$(echo "$enabled_only" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
[ "$n_en" = "1" ] || fail "A5 enabled-only count: got $n_en, want 1"
echo "$enabled_only" | python3 -c 'import sys,json;a=json.load(sys.stdin)[0];assert a["label"]=="test-pi",a' \
  || fail "A5 wrong agent surfaced"
ok "A5 ok — enabled-only filter works"

# --- A6: lookup by id matches lookup by label -----------------------------
pi_id=$(echo "$out_pi" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
by_id=$("$PI_BIN" agent-get --id "$pi_id" --format json) || fail "A6 get-by-id rc=$?"
by_label=$("$PI_BIN" agent-get --label test-pi --format json) || fail "A6 get-by-label rc=$?"
[ "$by_id" = "$by_label" ] || fail "A6 mismatch between id and label lookup"
ok "A6 ok — id and label lookups return identical rows"

# --- A7: cascade on delete ------------------------------------------------
# Reference the agent from pool_members. Use a direct insert since the
# pool-add CLI doesn't ship until v3.12.2.
sqlite3 "$DB" "INSERT INTO pool_members (pair_key, agent_id, count, priority, added_at) VALUES ('test-board', $pi_id, 1, 0, '2026-04-30T00:00:00Z');"
n_pool_pre=$(sqlite3 "$DB" "SELECT COUNT(*) FROM pool_members WHERE agent_id=$pi_id;")
[ "$n_pool_pre" = "1" ] || fail "A7 pool seed failed"

"$PI_BIN" agent-delete --label test-pi >/dev/null 2>&1 || fail "A7 delete rc=$?"
n_pool_post=$(sqlite3 "$DB" "SELECT COUNT(*) FROM pool_members WHERE agent_id=$pi_id;")
[ "$n_pool_post" = "0" ] || fail "A7 cascade did not remove pool row"

# Subsequent get returns not-found.
if "$PI_BIN" agent-get --id "$pi_id" >/dev/null 2>&1; then
  fail "A7 get after delete unexpectedly succeeded"
fi
ok "A7 ok — delete cascades to pool_members + get-after-delete returns not-found"

# --- A8: NULL round-trip --------------------------------------------------
"$PI_BIN" agent-create --label test-bare --runtime pi --format json >/dev/null \
  || fail "A8 create rc=$?"
bare=$("$PI_BIN" agent-get --label test-bare --format json) || fail "A8 get rc=$?"
# role / model_config / worker_cmd should be absent (NULL → omit).
echo "$bare" | python3 -c '
import sys, json
a = json.load(sys.stdin)
forbidden = [k for k in ("role", "model_config", "worker_cmd") if k in a]
assert not forbidden, f"NULL fields leaked into JSON: {forbidden}"
assert a["label"] == "test-bare" and a["runtime"] == "pi", a
' || fail "A8 NULL fields not omitted"
ok "A8 ok — NULL columns omitted from JSON output"

echo "PASS cards-v312-agents-crud.sh — A1-A8 8/8"
