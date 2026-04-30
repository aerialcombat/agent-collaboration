#!/usr/bin/env bash
# tests/cards-poc.sh — v3.9 kanban cards smoke test.
#
# Exercises the Go CLI verbs end-to-end against a temp SQLite DB:
#
#   C1 — create 3 cards, verify all start status=todo + ready=true.
#   C2 — add 1→2 and 2→3 dependencies, verify the chain flips
#        #2 and #3 into blocked-state.
#   C3 — claim #1 (todo→in_progress) and mark done; verify #2 flips
#        to ready automatically (derived, not persisted).
#   C4 — add-dep 3→1 rejected as a cycle (exit 65, EX_DATAERR).
#   C5 — add-dep self-loop rejected (1→1).
#   C6 — invalid status update rejected (--status foo → EX_USAGE 64).
#   C7 — remove-dep idempotent no-op when edge didn't exist.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
MIG_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
MIG_DIR="$PROJECT_ROOT/migrations/sqlite"

for b in "$PI_BIN" "$MIG_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t cards-poc.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

cleanup() { rm -rf "$TMP_RAW"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# Bootstrap base schema.
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

version="$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")"
[ "$version" = "14" ] || fail "goose version $version, expected 14"

PK="cards-poc-$$"

status_of() { sqlite3 "$DB" "SELECT status FROM cards WHERE id=$1;"; }
ready_of() {
  "$PI_BIN" card-get --id "$1" --format json \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('ready'))"
}

# ---------- C1: create ------------------------------------------------------
id1="$("$PI_BIN" card-create --pair-key "$PK" --title "design feed schema" \
    --created-by oozoo-manager --needs-role backend --priority 1 \
    --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')"
id2="$("$PI_BIN" card-create --pair-key "$PK" --title "backend endpoint" \
    --created-by oozoo-manager --needs-role backend \
    --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')"
id3="$("$PI_BIN" card-create --pair-key "$PK" --title "ios UI" \
    --created-by oozoo-manager --needs-role ios \
    --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')"

for id in "$id1" "$id2" "$id3"; do
  [ "$(status_of "$id")" = "todo" ] || fail "C1 #${id} not todo"
  [ "$(ready_of "$id")" = "True" ] || fail "C1 #${id} not ready"
done
echo "C1 ok — 3 cards created, all todo+ready"

# ---------- C2: wire dependency chain ---------------------------------------
"$PI_BIN" card-add-dep --blocker "$id1" --blockee "$id2" --as oozoo-manager >/dev/null
"$PI_BIN" card-add-dep --blocker "$id2" --blockee "$id3" --as oozoo-manager >/dev/null
[ "$(ready_of "$id1")" = "True" ]  || fail "C2 #1 should stay ready (no blockers)"
[ "$(ready_of "$id2")" = "False" ] || fail "C2 #2 should be blocked by #1"
[ "$(ready_of "$id3")" = "False" ] || fail "C2 #3 should be blocked by #2"
ready_ids="$("$PI_BIN" card-list --pair-key "$PK" --ready-only --format json \
  | python3 -c 'import sys,json;print(",".join(str(c["id"]) for c in json.load(sys.stdin)))')"
[ "$ready_ids" = "$id1" ] || fail "C2 ready-only got [$ready_ids], expected [$id1]"
echo "C2 ok — chain wired, only head is ready"

# ---------- C3: complete blocker, downstream unblocks -----------------------
"$PI_BIN" card-claim --id "$id1" --as voice-backend-claude --format json >/dev/null
[ "$(status_of "$id1")" = "in_progress" ] || fail "C3 claim didn't flip to in_progress"
"$PI_BIN" card-update-status --id "$id1" --status done --format json >/dev/null
[ "$(status_of "$id1")" = "done" ] || fail "C3 done transition missing"
[ "$(ready_of "$id2")" = "True" ]  || fail "C3 #2 should be ready after #1 done"
[ "$(ready_of "$id3")" = "False" ] || fail "C3 #3 still blocked by #2"
echo "C3 ok — completing blocker auto-unlocks next card"

# ---------- C4: cycle rejected ---------------------------------------------
out="$("$PI_BIN" card-add-dep --blocker "$id3" --blockee "$id1" --as oozoo-manager 2>&1)"
rc=$?
[ "$rc" = "65" ] || fail "C4 expected rc=65 (EX_DATAERR), got $rc (out=$out)"
echo "$out" | grep -qi "cycle" || fail "C4 error message missing 'cycle'"
echo "C4 ok — cycle rejected with rc=65"

# ---------- C5: self-loop rejected ------------------------------------------
if "$PI_BIN" card-add-dep --blocker "$id1" --blockee "$id1" --as oozoo-manager 2>/dev/null; then
  fail "C5 self-loop should have failed"
fi
echo "C5 ok — self-loop rejected"

# ---------- C6: invalid status rejected -------------------------------------
if "$PI_BIN" card-update-status --id "$id2" --status foo 2>/dev/null; then
  fail "C6 invalid status should have failed"
fi
echo "C6 ok — invalid status rejected"

# ---------- C7: remove-dep idempotent --------------------------------------
"$PI_BIN" card-remove-dep --blocker 99 --blockee 100 --format json \
  | grep -q '"ok":false' || fail "C7 remove non-existent edge should report ok=false"
echo "C7 ok — remove-dep no-op is idempotent"

echo "PASS cards-poc.sh — 7/7"
