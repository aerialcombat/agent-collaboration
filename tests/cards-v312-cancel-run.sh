#!/usr/bin/env bash
# tests/cards-v312-cancel-run.sh — v3.12.4.7 smoke for the operator
# panic button (card-cancel-run).
#
#   K1 — Cancel a long-running fake worker. Asserts:
#         * card_runs row → status='cancelled', exit_code=-1, pid=NULL.
#         * Card status flipped back to todo, claimed_by cleared.
#         * Audit comment recorded ("operator cancelled run #N…").
#         * status_change event with trigger='release'.
#         * Worker process is no longer alive.
#   K2 — Cancel with --no-release leaves the card in_progress + claimed.
#   K3 — Cancel when nothing is running returns exit code != 0
#         (exitNotFound) and writes nothing.

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

TMP_RAW="$(mktemp -d -t cards-v312-cancel-run.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PORT=18860
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

# Long-sleeping fake worker: writes its PID to /tmp/<card>.pid then sleeps 30s.
# The cancel verb should SIGTERM it; trap+exit lets us verify clean wind-down.
FAKE="$TMP/fake-sleeper.sh"
# Real workers (claude -p, native binaries) handle SIGTERM directly. A
# bash-script fake has to defeat bash's "trap deferred until foreground
# command returns" quirk by backgrounding sleep + wait — otherwise a
# SIGTERM to the script's pid sits queued for the full sleep duration.
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
set -uo pipefail
prompt="\$(cat)"
card_id="\$(echo "\$prompt" | sed -nE 's/^Card id:[[:space:]]+([0-9]+)/\1/p' | head -1)"
[ -n "\$card_id" ] && echo "\$\$" > "$TMP/worker-\${card_id}.pid"
sleep_pid=""
trap '[ -n "\$sleep_pid" ] && kill \$sleep_pid 2>/dev/null; exit 0' TERM
sleep 30 &
sleep_pid=\$!
wait
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
  rm -rf "$TMP_RAW"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "$*"; }

start_pw() {
  rm -f "$LOG"
  "$PW_BIN" --port "$PORT" >"$LOG" 2>&1 &
  PW_PID=$!
  for _ in $(seq 1 30); do
    if curl -fsS "$URL/api/scope" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  cat "$LOG" >&2
  fail "peer-web didn't start on $URL"
}
stop_pw() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  PW_PID=""
}

wait_for_pid_file() {
  local cid="$1" max="${2:-50}"
  for _ in $(seq 1 "$max"); do
    [ -s "$TMP/worker-${cid}.pid" ] && return 0
    sleep 0.1
  done
  return 1
}

# --- bootstrap ---
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

"$PI_BIN" agent-create --label sleeper --runtime claude --worker-cmd "$FAKE" >/dev/null

# --- K1: cancel a running worker ---
PK="k1-cancel-$$"
"$PI_BIN" pool-add --pair-key "$PK" --agent sleeper --priority 5 >/dev/null
C1=$("$PI_BIN" card-create --pair-key "$PK" --title "cancel me" --body "long sleeper" \
  --created-by tester --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

start_pw
"$PI_BIN" board-set --pair-key "$PK" --auto-drain on --max-concurrent 1 --as k1 >/dev/null
wait_for_pid_file "$C1" || fail "K1 worker never wrote pid file"
WPID=$(cat "$TMP/worker-${C1}.pid")
[ -n "$WPID" ] && kill -0 "$WPID" 2>/dev/null || fail "K1 worker pid $WPID not alive"

"$PI_BIN" card-cancel-run --card "$C1" --as k1 --format json > "$TMP/cancel-k1.json" \
  || fail "K1 cancel verb returned non-zero"
grep -q '"signalled":true' "$TMP/cancel-k1.json" \
  || fail "K1 cancel json should have signalled=true; got $(cat "$TMP/cancel-k1.json")"

# Verify worker actually died.
for _ in $(seq 1 30); do
  kill -0 "$WPID" 2>/dev/null || break
  sleep 0.1
done
kill -0 "$WPID" 2>/dev/null && fail "K1 worker pid $WPID still alive after cancel"

# DB assertions.
rs=$(sqlite3 "$DB" "SELECT status FROM card_runs WHERE card_id=$C1 ORDER BY id DESC LIMIT 1")
[ "$rs" = "cancelled" ] || fail "K1 expected run status=cancelled, got $rs"
ec=$(sqlite3 "$DB" "SELECT exit_code FROM card_runs WHERE card_id=$C1 ORDER BY id DESC LIMIT 1")
[ "$ec" = "-1" ] || fail "K1 expected exit_code=-1, got $ec"
cs=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$C1")
cb=$(sqlite3 "$DB" "SELECT COALESCE(claimed_by,'') FROM cards WHERE id=$C1")
[ "$cs" = "todo" ] || fail "K1 card should be todo after release, got $cs"
[ -z "$cb" ] || fail "K1 claim should be cleared, got '$cb'"
n_audit=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$C1 AND kind='comment' AND body LIKE 'operator cancelled run%'")
[ "$n_audit" = "1" ] || fail "K1 expected 1 audit comment, got $n_audit"
n_release=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$C1 AND kind='status_change' AND meta LIKE '%\"trigger\":\"release\"%'")
[ "$n_release" -ge 1 ] || fail "K1 expected status_change with trigger=release, got $n_release"

"$PI_BIN" board-set --pair-key "$PK" --auto-drain off --as k1 >/dev/null
stop_pw
ok "K1 ok — worker SIGTERM'd, run cancelled, claim released, audit recorded"

# --- K2: --no-release leaves the card in_progress ---
PK2="k2-no-release-$$"
"$PI_BIN" pool-add --pair-key "$PK2" --agent sleeper --priority 5 >/dev/null
C2=$("$PI_BIN" card-create --pair-key "$PK2" --title "no-release" --body "another sleeper" \
  --created-by tester --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
start_pw
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain on --max-concurrent 1 --as k2 >/dev/null
wait_for_pid_file "$C2" || fail "K2 worker never wrote pid file"
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain off --as k2 >/dev/null  # stop drainer first to prevent re-pickup before our test
"$PI_BIN" card-cancel-run --card "$C2" --as k2 --no-release --format json >/dev/null \
  || fail "K2 cancel verb returned non-zero"
sleep 0.5
cs=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$C2")
[ "$cs" = "in_progress" ] || fail "K2 with --no-release expected in_progress, got $cs"
cb=$(sqlite3 "$DB" "SELECT COALESCE(claimed_by,'') FROM cards WHERE id=$C2")
[ -n "$cb" ] || fail "K2 with --no-release expected claim retained"
stop_pw
ok "K2 ok — --no-release left card in_progress + claimed"

# --- K3: nothing running → exitNotFound, no write ---
PK3="k3-nothing-$$"
C3=$("$PI_BIN" card-create --pair-key "$PK3" --title "nothing running" --created-by tester --format json | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
"$PI_BIN" card-cancel-run --card "$C3" --as k3 >/dev/null 2>&1
rc=$?
[ "$rc" = "6" ] || fail "K3 expected exit code 6 (exitNotFound), got $rc"
n_events=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$C3")
[ "$n_events" = "0" ] || fail "K3 expected zero audit events, got $n_events"
ok "K3 ok — no-running-run returned exitNotFound; no events written"

echo "PASS cards-v312-cancel-run.sh — K1-K3 3/3"
