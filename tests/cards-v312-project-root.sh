#!/usr/bin/env bash
# tests/cards-v312-project-root.sh — Track 1 #1 smoke for the per-board
# project_root binding. Verifies that when board_settings.project_root
# is set, dispatched workers land in that directory (cmd.Dir), and when
# unset they inherit peer-web's cwd.
#
#   P1 — board_settings.project_root set to /tmp/foo → fake worker
#         records its $PWD; assertion: $PWD == /tmp/foo.
#   P2 — board_settings.project_root unset → fake worker's $PWD
#         matches peer-web's launch cwd (the test's $PROJECT_ROOT).
#   P3 — relative project_root rejected at the CLI layer (board-set
#         exits non-zero).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
PW_BIN="$PROJECT_ROOT/go/bin/peer-web"
MIG_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
MIG_DIR="$PROJECT_ROOT/migrations/sqlite"

for b in "$PI_BIN" "$PW_BIN" "$MIG_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b — run go build in go/"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }
command -v curl    >/dev/null 2>&1 || { echo "skip: curl not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t cards-v312-project-root.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PROJECT_DIR_A="$TMP/project-A"
PROJECT_DIR_B="$TMP/project-B"
mkdir -p "$PROJECT_DIR_A" "$PROJECT_DIR_B"
PORT=18870
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

# Fake worker writes $PWD to a file keyed by card id. P1/P2 read those.
FAKE="$TMP/fake-cwd-recorder.sh"
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
set -uo pipefail
prompt="\$(cat)"
card_id="\$(echo "\$prompt" | sed -nE 's/^Card id:[[:space:]]+([0-9]+)/\1/p' | head -1)"
label="\$(echo "\$prompt" | sed -nE 's/^Your label.*:[[:space:]]+(.+)/\1/p' | head -1)"
[ -n "\$card_id" ] && pwd > "$TMP/cwd-\${card_id}.txt"
sleep 0.5
if [ -n "\$card_id" ] && [ -n "\$label" ]; then
  AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" card-update-status \
    --card "\$card_id" --status in_review --as "\$label" >/dev/null 2>&1 || true
fi
exit 0
EOF
chmod +x "$FAKE"

PW_PID=""
cleanup() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  pkill -f "$FAKE" 2>/dev/null || true
  if [ "${KEEP_TMP:-0}" = "1" ]; then
    echo "KEEP_TMP=1 — leaving $TMP_RAW for inspection" >&2
  else
    rm -rf "$TMP_RAW"
  fi
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "$*"; }

start_pw_in() {
  # Start peer-web with a specific cwd so we can verify the inherit-cwd
  # path (P2) shows the launch cwd, not whatever this test happened to
  # be invoked from. Use pushd/popd instead of a subshell because
  # `( cd ... && bin ) &` leaks the underlying process when the subshell
  # wrapper exits before bin and `kill $!` only signals the wrapper.
  local cwd="$1"
  rm -f "$LOG"
  # Pre-flight: bail fast if the port is already bound by a zombie
  # peer-web from a prior failed run. Otherwise curl /api/scope below
  # would spuriously succeed against the wrong process.
  if command -v lsof >/dev/null 2>&1 && lsof -i ":$PORT" >/dev/null 2>&1; then
    fail "port $PORT is already in use — pkill -f 'peer-web --port' and retry"
  fi
  pushd "$cwd" >/dev/null
  "$PW_BIN" --port "$PORT" >"$LOG" 2>&1 &
  PW_PID=$!
  popd >/dev/null
  for _ in $(seq 1 30); do
    if curl -fsS "$URL/api/scope" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  cat "$LOG" >&2
  fail "peer-web didn't start"
}
stop_pw() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  PW_PID=""
}

wait_terminal() {
  local pk="$1" want="$2" max="${3:-150}"
  for _ in $(seq 1 "$max"); do
    n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status IN ('completed','failed','cancelled','lost');")
    if [ "$n" -ge "$want" ]; then return 0; fi
    sleep 0.1
  done
  return 1
}

# Bootstrap.
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
"$MIG_BIN" -driver sqlite -dsn "$DB" -dir "$MIG_DIR" up >/dev/null 2>&1 || fail "migrate"

"$PI_BIN" agent-create --label cwd-tester --runtime claude --worker-cmd "$FAKE" >/dev/null

# --- P1: project_root set → worker lands there -----------------------------
PK1="p1-set-$$"
"$PI_BIN" pool-add --pair-key "$PK1" --agent cwd-tester --priority 5 >/dev/null
C1=$("$PI_BIN" card-create --pair-key "$PK1" --title "p1 cwd test" --created-by tester \
  --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

start_pw_in "$PROJECT_ROOT"
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain on --max-concurrent 1 \
  --project-root "$PROJECT_DIR_A" --as p1 >/dev/null \
  || fail "P1 board-set with --project-root failed"

# Confirm settings persisted.
got_root=$(sqlite3 "$DB" "SELECT project_root FROM board_settings WHERE pair_key='$PK1'")
[ "$got_root" = "$PROJECT_DIR_A" ] || fail "P1 project_root persisted=$got_root, want $PROJECT_DIR_A"

wait_terminal "$PK1" 1 || fail "P1 worker didn't terminate"
[ -s "$TMP/cwd-${C1}.txt" ] || fail "P1 worker didn't write cwd file"
got_pwd=$(cat "$TMP/cwd-${C1}.txt")
# realpath both sides to handle macOS /private/var symlink prefix.
expected=$(cd "$PROJECT_DIR_A" && pwd -P)
actual=$(cd "$got_pwd" && pwd -P)
[ "$actual" = "$expected" ] \
  || fail "P1 worker pwd=$actual, expected=$expected (board project_root=$PROJECT_DIR_A)"
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain off --as p1 >/dev/null
stop_pw
ok "P1 ok — project_root set → worker cwd = $expected"

# --- P2: project_root unset → worker inherits peer-web's cwd ---------------
PK2="p2-unset-$$"
"$PI_BIN" pool-add --pair-key "$PK2" --agent cwd-tester --priority 5 >/dev/null
C2=$("$PI_BIN" card-create --pair-key "$PK2" --title "p2 cwd test" --created-by tester \
  --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# Launch peer-web from PROJECT_DIR_B specifically — so we can prove
# the worker inherited THIS path, not e.g. the test's invocation cwd.
start_pw_in "$PROJECT_DIR_B"
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain on --max-concurrent 1 --as p2 >/dev/null

wait_terminal "$PK2" 1 || fail "P2 worker didn't terminate"
[ -s "$TMP/cwd-${C2}.txt" ] || fail "P2 worker didn't write cwd file"
got_pwd=$(cat "$TMP/cwd-${C2}.txt")
expected=$(cd "$PROJECT_DIR_B" && pwd -P)
actual=$(cd "$got_pwd" && pwd -P)
[ "$actual" = "$expected" ] \
  || fail "P2 worker pwd=$actual, expected=$expected (peer-web cwd inheritance)"
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain off --as p2 >/dev/null
stop_pw
ok "P2 ok — unset project_root → worker inherits peer-web cwd ($expected)"

# --- P3: relative project_root rejected at CLI layer ------------------------
if "$PI_BIN" board-set --pair-key foo-relative --project-root "relative/path" --as p3 >/dev/null 2>&1; then
  fail "P3 relative path unexpectedly accepted"
fi
ok "P3 ok — relative --project-root rejected at CLI"

echo "PASS cards-v312-project-root.sh — P1-P3 3/3"
