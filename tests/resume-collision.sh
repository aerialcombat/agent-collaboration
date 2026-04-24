#!/usr/bin/env bash
# tests/resume-collision.sh — session-register liveness-aware collision.
#
# Validates the fix that lets `claude --resume` rejoin a room inside the
# 5-minute idle window when the prior Claude's MCP socket is gone. Three
# scenarios:
#
#   R1 — live socket + different session_key + recent last_seen
#        → EXIT_LABEL_COLLISION (unchanged, legitimate collision).
#   R2 — dead socket path + different session_key + recent last_seen
#        → success (the resume path; prior session is effectively gone).
#   R3 — same session_key (idempotent refresh) stays allowed regardless
#        of socket state.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
MIGRATE_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"

for b in "$PI_BIN" "$MIGRATE_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t resume-collision.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"

SOCK_SERVER_PID=""
cleanup() {
  if [ -n "$SOCK_SERVER_PID" ]; then
    kill "$SOCK_SERVER_PID" 2>/dev/null
    wait "$SOCK_SERVER_PID" 2>/dev/null
  fi
  rm -rf "$TMP_RAW"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Bootstrap schema via the migration binary (same pattern as session-state-v38.sh).
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

CWD="$TMP/agent"
mkdir -p "$CWD"
PK="resume-collision-$$"

# Paths are kept short so macOS's 104-char AF_UNIX cap doesn't bite.
LIVE_SOCK="$TMP/L.sock"
DEAD_SOCK="$TMP/D.sock"

# Start a Python listener bound to LIVE_SOCK in the background. It just
# accepts-and-closes — that's enough for connect() to succeed.
S="$LIVE_SOCK" python3 -c "
import os, socket, sys
p = os.environ['S']
if os.path.exists(p): os.unlink(p)
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(p); s.listen(16)
sys.stdout.write('ready\n'); sys.stdout.flush()
while True:
    c, _ = s.accept(); c.close()
" >"$TMP/server.out" 2>"$TMP/server.err" &
SOCK_SERVER_PID=$!

# Wait for 'ready' on stdout.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ -s "$TMP/server.out" ] && break
  sleep 0.1
done
[ -e "$LIVE_SOCK" ] || fail "live socket never bound (srv stderr: $(cat "$TMP/server.err"))"

# Sanity: DEAD_SOCK path was never created.
[ ! -e "$DEAD_SOCK" ] || fail "DEAD_SOCK should not exist"

register_with() {
  local session_key="$1"
  AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-register \
    --cwd "$CWD" --label alpha --agent claude \
    --session-key "$session_key" --pair-key "$PK" 2>&1
}

seed_row() {
  # Seed the sessions row directly so we can control channel_socket and
  # recency without invoking the register verb.
  local key="$1" channel="$2" age_secs="$3"
  local ts
  ts="$(python3 -c "import datetime as d; print((d.datetime.now(d.timezone.utc) - d.timedelta(seconds=$age_secs)).strftime('%Y-%m-%dT%H:%M:%SZ'))")"
  sqlite3 "$DB" >/dev/null <<SQL
DELETE FROM sessions WHERE cwd='$CWD' AND label='alpha';
INSERT INTO sessions (cwd, label, agent, role, session_key, channel_socket, pair_key, started_at, last_seen_at, receive_mode, auth_token, auth_token_rotated_at)
VALUES ('$CWD', 'alpha', 'claude', 'peer', '$key', $( [ -n "$channel" ] && echo "'$channel'" || echo NULL ), '$PK', '$ts', '$ts', 'interactive', 'seed-token', '$ts');
INSERT OR IGNORE INTO peer_rooms (room_key, pair_key, turn_count, home_host) VALUES ('pk:$PK', '$PK', 0, 'testhost');
SQL
}

# ---------- R1: live socket + different key + recent → collision ------------
seed_row "prior-sk-R1" "$LIVE_SOCK" 60
out="$(register_with "new-sk-R1")"
rc=$?
if [ "$rc" -eq 0 ]; then
  fail "R1 expected collision, register succeeded (out=$out)"
fi
echo "$out" | grep -q "already active\|label collision" \
  || fail "R1 wrong error (out=$out)"
echo "R1 ok — live socket still fires collision"

# ---------- R2: dead socket + different key + recent → success --------------
seed_row "prior-sk-R2" "$DEAD_SOCK" 60
out="$(register_with "new-sk-R2")"
rc=$?
if [ "$rc" -ne 0 ]; then
  fail "R2 expected success (dead socket = stale row), got rc=$rc out=$out"
fi
new_key="$(sqlite3 "$DB" "SELECT session_key FROM sessions WHERE cwd='$CWD' AND label='alpha';")"
[ "$new_key" = "new-sk-R2" ] || fail "R2 session_key not updated (got '$new_key')"
echo "R2 ok — dead socket allows silent resume"

# ---------- R3: same session_key → idempotent refresh (unchanged) -----------
seed_row "same-sk-R3" "$LIVE_SOCK" 60
out="$(register_with "same-sk-R3")"
rc=$?
if [ "$rc" -ne 0 ]; then
  fail "R3 idempotent refresh should succeed, rc=$rc out=$out"
fi
echo "R3 ok — same session_key still idempotent"

# ---------- R4: channel-less row + different key + recent → collision -------
# No prior socket to probe, so we keep the conservative collision.
seed_row "prior-sk-R4" "" 60
out="$(register_with "new-sk-R4")"
rc=$?
if [ "$rc" -eq 0 ]; then
  fail "R4 channel-less row should still collide without --force (out=$out)"
fi
echo "R4 ok — no-channel row keeps conservative collision"

# ---------- R5: idle window expired (>300s) → success regardless -----------
seed_row "prior-sk-R5" "$LIVE_SOCK" 400
out="$(register_with "new-sk-R5")"
rc=$?
if [ "$rc" -ne 0 ]; then
  fail "R5 past idle window should succeed, rc=$rc out=$out"
fi
echo "R5 ok — past idle window still succeeds"

echo "PASS resume-collision.sh — 5/5"
