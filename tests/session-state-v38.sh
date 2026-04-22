#!/usr/bin/env bash
# tests/session-state-v38.sh — v3.8 agent activity monitoring.
#
# Exercises the sessions.state / state_changed_at column end-to-end:
#
#   S1 — peer-inbox session-state CLI sets/transitions/no-ops state
#        on an explicit (cwd, label) target.
#   S2 — session-state sets state by session-key (hook path).
#   S3 — no-session / no-key fail-open exits 0 with no DB touch.
#   S4 — peer-web /api/rooms surfaces state on the member payload.
#   S5 — the Go UserPromptSubmit hook binary flips state to active
#        when it runs for a registered session.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
PW_BIN="$PROJECT_ROOT/go/bin/peer-web"
HOOK_BIN="$PROJECT_ROOT/go/bin/peer-inbox-hook"
MIGRATE_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"

for b in "$PI_BIN" "$PW_BIN" "$MIGRATE_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }
command -v curl >/dev/null 2>&1 || { echo "skip: curl not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t v38-state.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"

cleanup() {
  [ -n "${PW_PID:-}" ] && kill "$PW_PID" 2>/dev/null
  rm -rf "$TMP_RAW"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Bootstrap schema (same pattern as multi-room-v36.sh).
sqlite3 "$DB" >/dev/null <<'SQL'
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS sessions (
  cwd             TEXT NOT NULL,
  label           TEXT NOT NULL,
  agent           TEXT NOT NULL,
  role            TEXT,
  session_key     TEXT,
  channel_socket  TEXT,
  pair_key        TEXT,
  started_at      TEXT NOT NULL,
  last_seen_at    TEXT NOT NULL,
  PRIMARY KEY (cwd, label)
);
CREATE TABLE IF NOT EXISTS inbox (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  to_cwd      TEXT NOT NULL, to_label TEXT NOT NULL,
  from_cwd    TEXT NOT NULL, from_label TEXT NOT NULL,
  body        TEXT NOT NULL, created_at TEXT NOT NULL,
  read_at     TEXT, room_key TEXT
);
CREATE TABLE IF NOT EXISTS peer_rooms (
  room_key       TEXT PRIMARY KEY,
  pair_key       TEXT,
  turn_count     INTEGER NOT NULL DEFAULT 0,
  terminated_at  TEXT,
  terminated_by  TEXT
);
SQL
"$MIGRATE_BIN" -driver sqlite -dsn "$DB" \
  -dir "$PROJECT_ROOT/migrations/sqlite" up >/dev/null 2>&1 \
  || fail "schema bootstrap"

# Verify the 0008 migration added the new columns.
cols="$(sqlite3 "$DB" "SELECT group_concat(name) FROM pragma_table_info('sessions')")"
case "$cols" in
  *state*state_changed_at*) : ;;
  *) fail "0008 didn't add state / state_changed_at (cols=$cols)" ;;
esac

# Register a session.
PK="v38-state-$$"
CWD_A="$TMP/a"; mkdir -p "$CWD_A"
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" room-create --pair-key "$PK" >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-register \
  --cwd "$CWD_A" --label alpha --agent claude --session-key "alpha-sk-$$" \
  --pair-key "$PK" --force >/dev/null || fail "register alpha"

state_of() {
  sqlite3 "$DB" "SELECT COALESCE(state,'NULL') FROM sessions WHERE label='alpha'"
}
state_changed_of() {
  sqlite3 "$DB" "SELECT COALESCE(state_changed_at,'NULL') FROM sessions WHERE label='alpha'"
}

# S1 — explicit (cwd, label).
[ "$(state_of)" = "NULL" ] || fail "S1: pre-check — initial state should be NULL, got=$(state_of)"
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state active --cwd "$CWD_A" --label alpha --quiet \
  || fail "S1: CLI set active"
[ "$(state_of)" = "active" ] || fail "S1: expected active, got=$(state_of)"
first_ts="$(state_changed_of)"
[ "$first_ts" != "NULL" ] || fail "S1: state_changed_at stayed NULL"

# Idempotent no-op — same state, timestamp must NOT move.
sleep 1
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state active --cwd "$CWD_A" --label alpha --quiet \
  || fail "S1: CLI set active (2nd)"
[ "$(state_changed_of)" = "$first_ts" ] || fail "S1: state_changed_at moved on no-op transition (want=$first_ts got=$(state_changed_of))"

# Transition to idle — timestamp MUST move.
sleep 1
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state idle --cwd "$CWD_A" --label alpha --quiet \
  || fail "S1: set idle"
[ "$(state_of)" = "idle" ] || fail "S1: expected idle, got=$(state_of)"
[ "$(state_changed_of)" != "$first_ts" ] || fail "S1: state_changed_at did not move on transition"

echo "OK  S1 — explicit target sets / transitions / no-ops correctly"

# S2 — session-key path.
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state active --session-key "alpha-sk-$$" --quiet \
  || fail "S2: by session-key"
[ "$(state_of)" = "active" ] || fail "S2: expected active, got=$(state_of)"

echo "OK  S2 — session-key path writes state"

# S3 — fail-open: no session matches, no key, no crash.
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state active --session-key "does-not-exist" --quiet \
  || fail "S3: unknown key should fail-open exit 0"
# Row should still be "active" (unchanged by the no-op).
[ "$(state_of)" = "active" ] || fail "S3: unknown-key write leaked to alpha"

echo "OK  S3 — unknown session-key fails open without touching DB"

# S4 — peer-web exposes state on the member payload.
PW_PORT=$((30000 + RANDOM % 10000))
AGENT_COLLAB_INBOX_DB="$DB" "$PW_BIN" --port "$PW_PORT" --cwd "$CWD_A" >/tmp/v38-pw.log 2>&1 &
PW_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 0.2
  curl -sf "http://127.0.0.1:$PW_PORT/" >/dev/null 2>&1 && break
done

resp="$(curl -sS --max-time 3 "http://127.0.0.1:$PW_PORT/api/rooms?pair_key=$PK")"
echo "$resp" | python3 - "$resp" <<'PY' || fail "S4: /api/rooms missing state"
import json, sys
d = json.loads(sys.argv[1])
rooms = d.get("rooms") or []
if not rooms:
    print("no rooms in response", file=sys.stderr); sys.exit(1)
members = rooms[0].get("members") or []
if not members:
    print("no members", file=sys.stderr); sys.exit(1)
alpha = next((m for m in members if m.get("label") == "alpha"), None)
if alpha is None:
    print("no alpha member", file=sys.stderr); sys.exit(1)
if alpha.get("state") != "active":
    print(f"state wrong: got={alpha.get('state')!r}", file=sys.stderr); sys.exit(1)
if not alpha.get("state_changed_at"):
    print("missing state_changed_at", file=sys.stderr); sys.exit(1)
PY

echo "OK  S4 — peer-web /api/rooms exposes state + state_changed_at"

# S5 — Go UserPromptSubmit hook binary flips state to active.
if [ -x "$HOOK_BIN" ]; then
  AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-state idle --cwd "$CWD_A" --label alpha --quiet
  [ "$(state_of)" = "idle" ] || fail "S5: pre-idle failed"
  AGENT_COLLAB_INBOX_DB="$DB" CLAUDE_SESSION_ID="alpha-sk-$$" "$HOOK_BIN" "$CWD_A" >/dev/null 2>&1 || true
  [ "$(state_of)" = "active" ] || fail "S5: hook binary did not flip to active (got=$(state_of))"
  echo "OK  S5 — hook binary flips state to active on UserPromptSubmit"
else
  echo "skip S5: $HOOK_BIN not built"
fi

echo "all v3.8 session-state checks passed"
