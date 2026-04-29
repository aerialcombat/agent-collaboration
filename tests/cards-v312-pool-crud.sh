#!/usr/bin/env bash
# tests/cards-v312-pool-crud.sh — v3.12.2 pool composition smoke.
#
# Drives pool-add/remove/list/update across two boards with different
# rosters, verifying:
#
#   B1 — Add: 3 agents, 2 boards, distinct pools, joined fields populated
#   B2 — Update: count + priority changes round-trip
#   B3 — Remove: row gone, sibling unaffected
#   B4 — Cascade-on-agent-delete: removing an agent prunes its pool rows
#   B5 — Duplicate (pair_key, agent) is rejected (FK PRIMARY KEY collision
#         surfaces as a non-zero exit; existing row is unchanged)
#   B6 — Bad agent label rejected before any DB write
#   B7 — pool-update on missing membership returns ErrPoolMemberNotFound
#   B8 — pool-list on empty board returns "(empty pool)" / [] in JSON

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

TMP_RAW="$(mktemp -d -t cards-v312-pool.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"

export AGENT_COLLAB_INBOX_DB="$DB"

cleanup() { rm -rf "$TMP_RAW"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "$*"; }

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
[ "$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")" = "12" ] \
  || fail "goose version != 12"

# Seed three agents.
"$PI_BIN" agent-create --label pool-claude-1 --runtime claude --role impl   --format json >/dev/null
"$PI_BIN" agent-create --label pool-claude-2 --runtime claude --role review --format json >/dev/null
"$PI_BIN" agent-create --label pool-pi-1     --runtime pi     --role review --format json >/dev/null

# --- B1: distinct pools across two boards ---------------------------------
"$PI_BIN" pool-add --pair-key board-A --agent pool-claude-1 --count 3 --priority 5 --format json >/dev/null \
  || fail "B1 add A/claude-1"
"$PI_BIN" pool-add --pair-key board-A --agent pool-pi-1     --priority 1 --format json >/dev/null \
  || fail "B1 add A/pi-1"
"$PI_BIN" pool-add --pair-key board-B --agent pool-claude-2 --count 2 --format json >/dev/null \
  || fail "B1 add B/claude-2"

a_list=$("$PI_BIN" pool-list --pair-key board-A --format json) || fail "B1 list A"
b_list=$("$PI_BIN" pool-list --pair-key board-B --format json) || fail "B1 list B"

n_a=$(echo "$a_list" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
n_b=$(echo "$b_list" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
[ "$n_a" = "2" ] || fail "B1 board-A count: got $n_a, want 2"
[ "$n_b" = "1" ] || fail "B1 board-B count: got $n_b, want 1"

echo "$a_list" | python3 -c '
import sys, json
rows = json.load(sys.stdin)
labels = {r["agent_label"] for r in rows}
assert labels == {"pool-claude-1", "pool-pi-1"}, labels
# Ordered by priority DESC: claude-1 (priority=5) first
assert rows[0]["agent_label"] == "pool-claude-1", rows[0]
# Joined fields populated
assert rows[0]["agent_runtime"] == "claude" and rows[0]["agent_role"] == "impl", rows[0]
' || fail "B1 join/order"
ok "B1 ok — distinct pools, joined fields populated, ordered by priority"

# --- B2: update count + priority -----------------------------------------
"$PI_BIN" pool-update --pair-key board-A --agent pool-claude-1 --count 6 --priority 9 --format json >/dev/null \
  || fail "B2 update"
got=$("$PI_BIN" pool-list --pair-key board-A --format json) || fail "B2 list"
echo "$got" | python3 -c '
import sys, json
rows = json.load(sys.stdin)
c1 = next(r for r in rows if r["agent_label"] == "pool-claude-1")
assert c1["count"] == 6 and c1["priority"] == 9, c1
' || fail "B2 update fields incorrect"
ok "B2 ok — count + priority round-trip via update"

# --- B3: remove one, sibling unaffected -----------------------------------
"$PI_BIN" pool-remove --pair-key board-A --agent pool-pi-1 --format json >/dev/null \
  || fail "B3 remove"
got=$("$PI_BIN" pool-list --pair-key board-A --format json) || fail "B3 list"
n_after=$(echo "$got" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
[ "$n_after" = "1" ] || fail "B3 after-remove count: got $n_after, want 1"
echo "$got" | python3 -c '
import sys, json
rows = json.load(sys.stdin)
assert rows[0]["agent_label"] == "pool-claude-1", rows
' || fail "B3 wrong sibling survived"
ok "B3 ok — remove cleared one row, sibling unaffected"

# --- B4: cascade on agent-delete ------------------------------------------
"$PI_BIN" pool-add --pair-key board-A --agent pool-claude-2 --format json >/dev/null \
  || fail "B4 seed"
n_pre=$(sqlite3 "$DB" "SELECT COUNT(*) FROM pool_members WHERE agent_id IN (SELECT id FROM agents WHERE label='pool-claude-2');")
[ "$n_pre" -ge "2" ] || fail "B4 seed sanity: claude-2 should have 2 rows (board-A + board-B), got $n_pre"

"$PI_BIN" agent-delete --label pool-claude-2 --format json >/dev/null \
  || fail "B4 agent-delete"
n_post=$(sqlite3 "$DB" "SELECT COUNT(*) FROM pool_members WHERE agent_id IN (SELECT id FROM agents WHERE label='pool-claude-2');")
[ "$n_post" = "0" ] || fail "B4 cascade: $n_post rows survived agent delete"
ok "B4 ok — agent-delete cascaded; membership rows pruned"

# --- B5: duplicate (pair_key, agent) rejected -----------------------------
if "$PI_BIN" pool-add --pair-key board-A --agent pool-claude-1 --format json >/dev/null 2>&1; then
  fail "B5 duplicate add unexpectedly succeeded"
fi
# Existing row still has the values from B2 (count=6, priority=9)
got=$("$PI_BIN" pool-list --pair-key board-A --format json)
echo "$got" | python3 -c '
import sys, json
rows = json.load(sys.stdin)
c1 = next(r for r in rows if r["agent_label"] == "pool-claude-1")
assert c1["count"] == 6 and c1["priority"] == 9, c1
' || fail "B5 duplicate corrupted existing row"
ok "B5 ok — duplicate add rejected; existing row preserved"

# --- B6: bad agent label rejected -----------------------------------------
if "$PI_BIN" pool-add --pair-key board-A --agent does-not-exist --format json >/dev/null 2>&1; then
  fail "B6 bad-agent unexpectedly succeeded"
fi
ok "B6 ok — unknown agent label rejected"

# --- B7: pool-update on missing membership --------------------------------
if "$PI_BIN" pool-update --pair-key board-Z --agent pool-claude-1 --count 5 --format json >/dev/null 2>&1; then
  fail "B7 update-missing unexpectedly succeeded"
fi
ok "B7 ok — pool-update on missing membership rejected"

# --- B8: pool-list on empty board -----------------------------------------
empty_json=$("$PI_BIN" pool-list --pair-key board-Z --format json) || fail "B8 list rc"
[ "$(echo "$empty_json" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')" = "0" ] \
  || fail "B8 empty json should be []"
empty_plain=$("$PI_BIN" pool-list --pair-key board-Z) || fail "B8 list plain rc"
echo "$empty_plain" | grep -q "(empty pool)" \
  || fail "B8 plain output missing '(empty pool)'"
ok "B8 ok — pool-list on empty board returns [] / (empty pool)"

echo "PASS cards-v312-pool-crud.sh — B1-B8 8/8"
