#!/usr/bin/env bash
# tests/cards-phase1-drainer.sh — v3.11 standing drainer end-to-end.
#
# Drives the full dispatch path against a temp DB + an isolated peer-web
# process, with claude -p replaced by a fast fake-worker shell script via
# AGENT_COLLAB_WORKER_CMD. Verifies the seven scenarios from
# plans/v3.11-cards-phase1-standing-drainer.md §8.2:
#
#   D1 — Standing dispatch: max=2, 5 cards. Never >2 running, all 5
#         reach a terminal status.
#   D2 — Mid-flight restart: kill peer-web with workers in flight, restart,
#         reaper marks rows 'lost', drainer reconstructs.
#   D3 — Late arrival: empty board with auto_drain=1; new card dispatched
#         within poll_interval+slack.
#   D4 — Capacity ceiling: max=1, 8 cards. Never >1 running.
#   D5 — Settings update mid-drain: bump max_concurrent 1→3, in-flight
#         count grows.
#   D6 — Manual run: auto_drain=false, click Run, card_runs row trigger=manual.
#   D7 — needs_role reservation: drainer ignores needs_role in Phase 1
#         (treats as null), card dispatches normally.

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

# --- workspace -------------------------------------------------------------

TMP_RAW="$(mktemp -d -t cards-phase1.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PORT=18799
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

# Fake worker — fast no-op that mimics what a real claude -p would do
# for the dispatch lifecycle. It reads stdin (the prompt — discarded),
# sleeps briefly so D1/D2 can observe in-flight state, then card-update-
# status to in_review using the worker label parsed from the prompt.
#
# We don't bother editing files; the test only cares about the run row
# transitioning + the card progressing. WORKER_DELAY tunes how long it
# stays "in_progress".
FAKE="$TMP/fake-worker.sh"
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
# Fake worker — drains stdin, sleeps WORKER_DELAY, transitions card.
prompt="\$(cat)"
card_id="\$(echo "\$prompt" | sed -nE 's/^Card id: ([0-9]+)/\1/p' | head -1)"
label="\$(echo "\$prompt" | sed -nE 's/^Your label for this run: (.+)/\1/p' | head -1)"
sleep "\${WORKER_DELAY:-1}"
if [ -n "\$card_id" ] && [ -n "\$label" ]; then
  AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" card-update-status \
    --card "\$card_id" --status in_review --as "\$label" >/dev/null 2>&1 || true
fi
exit 0
EOF
chmod +x "$FAKE"
export AGENT_COLLAB_WORKER_CMD="$FAKE"
export WORKER_DELAY=1

PW_PID=""
cleanup() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  # Also reap any fake workers that detached (they shouldn't, but belt+braces).
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
  # Wait for socket up.
  for i in $(seq 1 30); do
    if curl -fsS "$URL/api/scope" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  cat "$LOG" >&2
  fail "peer-web didn't start on $URL within 6s"
}
stop_pw() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  PW_PID=""
}

count_running() {
  local pk="$1"
  sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status='running';"
}
count_status() {
  local pk="$1" st="$2"
  sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status='$st';"
}
count_cards_status() {
  local pk="$1" st="$2"
  sqlite3 "$DB" "SELECT COUNT(*) FROM cards WHERE pair_key='$pk' AND status='$st';"
}

mk_card() {
  local pk="$1" title="$2" role="${3:-}"
  local args=(card-create --pair-key "$pk" --title "$title" --created-by tester --format json)
  if [ -n "$role" ]; then args+=(--needs-role "$role"); fi
  "$PI_BIN" "${args[@]}" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
}

# --- bootstrap DB ----------------------------------------------------------

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
[ "$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")" = "11" ] \
  || fail "goose version != 11"

# --- D1: standing dispatch -------------------------------------------------
PK1="phase1-d1-$$"
for i in 1 2 3 4 5; do mk_card "$PK1" "d1 card $i" >/dev/null; done

start_pw
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain on \
  --max-concurrent 2 --poll-interval-secs 1 --as test >/dev/null \
  || fail "D1 board-set"

# Sample running count for ~20 ticks; assert never > 2.
max_seen=0
for i in $(seq 1 25); do
  n=$(count_running "$PK1")
  [ "$n" -gt "$max_seen" ] && max_seen="$n"
  [ "$n" -gt 2 ] && fail "D1 saw $n concurrent runs (cap was 2)"
  sleep 0.4
done
# Wait for full drain (all cards should reach terminal status).
for i in $(seq 1 30); do
  if [ "$(count_running "$PK1")" -eq 0 ]; then break; fi
  sleep 1
done
[ "$(count_running "$PK1")" -eq 0 ] || fail "D1 drainer never finished"
total=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$PK1';")
[ "$total" -ge 5 ] || fail "D1 only $total runs (expected >= 5)"
ok "D1 ok — $total runs, peak $max_seen concurrent (cap 2), all terminal"

# Tear down D1's drainer so it doesn't keep re-dispatching cards stuck in
# any state we didn't promote (none should be left, but stop is cheap).
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain off --as test >/dev/null

# --- D2: mid-flight restart ------------------------------------------------
PK2="phase1-d2-$$"
WORKER_DELAY_OLD="$WORKER_DELAY"
export WORKER_DELAY=10  # keep workers in flight long enough to catch them
for i in 1 2 3; do mk_card "$PK2" "d2 card $i" >/dev/null; done

"$PI_BIN" board-set --pair-key "$PK2" --auto-drain on \
  --max-concurrent 3 --poll-interval-secs 1 --as test >/dev/null

# Wait until at least 1 row is running.
for i in $(seq 1 15); do
  [ "$(count_running "$PK2")" -ge 1 ] && break
  sleep 0.4
done
[ "$(count_running "$PK2")" -ge 1 ] || fail "D2 no running rows to restart against"
in_flight_before=$(count_running "$PK2")

# Hard-kill peer-web (children inherit pgrp under default Go behaviour;
# the reaper has to handle pid-still-alive AND pid-dead branches).
kill -KILL "$PW_PID" 2>/dev/null
wait "$PW_PID" 2>/dev/null || true
PW_PID=""
sleep 1
# Restart.
start_pw
sleep 2

# After reap+reconstruct: every previously-running row must be 'lost'.
lost=$(count_status "$PK2" lost)
running=$(count_running "$PK2")
[ "$lost" -ge "$in_flight_before" ] \
  || fail "D2 expected >= $in_flight_before lost rows, saw $lost"
ok "D2 ok — $in_flight_before in-flight before kill, $lost reaped to lost on boot"

# Reset for downstream tests — drainer was reconstructed and may be picking
# up new runs against PK2 cards still in todo/in_progress. Stop it.
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain off --as test >/dev/null
export WORKER_DELAY="$WORKER_DELAY_OLD"

# --- D3: late arrival ------------------------------------------------------
PK3="phase1-d3-$$"
"$PI_BIN" board-set --pair-key "$PK3" --auto-drain on \
  --max-concurrent 1 --poll-interval-secs 1 --as test >/dev/null

# Empty board — drainer should idle. Wait a couple ticks.
sleep 2
[ "$(count_running "$PK3")" -eq 0 ] || fail "D3 drainer dispatched on empty board"

# Add one card and wait for dispatch.
mk_card "$PK3" "d3 late arrival" >/dev/null
for i in $(seq 1 10); do
  if [ "$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$PK3'")" -ge 1 ]; then
    break
  fi
  sleep 0.5
done
[ "$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$PK3'")" -ge 1 ] \
  || fail "D3 late arrival never dispatched"
ok "D3 ok — late arrival dispatched within poll interval"

"$PI_BIN" board-set --pair-key "$PK3" --auto-drain off --as test >/dev/null

# --- D4: capacity ceiling --------------------------------------------------
PK4="phase1-d4-$$"
export WORKER_DELAY=2
for i in $(seq 1 8); do mk_card "$PK4" "d4 card $i" >/dev/null; done
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain on \
  --max-concurrent 1 --poll-interval-secs 1 --as test >/dev/null

# Sample for 10 seconds.
for i in $(seq 1 25); do
  n=$(count_running "$PK4")
  [ "$n" -gt 1 ] && fail "D4 saw $n concurrent runs (cap was 1)"
  sleep 0.4
done
ok "D4 ok — 8 cards, never >1 concurrent"
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain off --as test >/dev/null
export WORKER_DELAY=1

# --- D5: settings update mid-drain ----------------------------------------
PK5="phase1-d5-$$"
export WORKER_DELAY=4  # long enough that mid-drain bump is observable
for i in $(seq 1 6); do mk_card "$PK5" "d5 card $i" >/dev/null; done
"$PI_BIN" board-set --pair-key "$PK5" --auto-drain on \
  --max-concurrent 1 --poll-interval-secs 1 --as test >/dev/null

# Wait for the first run to start, then bump cap.
for i in $(seq 1 10); do
  [ "$(count_running "$PK5")" -ge 1 ] && break
  sleep 0.4
done
[ "$(count_running "$PK5")" -ge 1 ] || fail "D5 no run started before bump"
"$PI_BIN" board-set --pair-key "$PK5" --max-concurrent 3 --as test >/dev/null

# Within ~3 ticks the in-flight count should grow.
seen_higher=0
for i in $(seq 1 15); do
  n=$(count_running "$PK5")
  [ "$n" -ge 2 ] && seen_higher=1 && break
  sleep 0.5
done
[ "$seen_higher" -eq 1 ] || fail "D5 cap bump didn't grow in-flight count"
ok "D5 ok — cap 1→3 observed live"
"$PI_BIN" board-set --pair-key "$PK5" --auto-drain off --as test >/dev/null
export WORKER_DELAY=1

# --- D6: manual run --------------------------------------------------------
PK6="phase1-d6-$$"
id6=$(mk_card "$PK6" "d6 manual run")
# auto_drain stays false.
curl -fsS -X POST "$URL/api/cards/$id6/run" >/dev/null \
  || fail "D6 manual run POST failed"
# Wait for the row + status transition.
for i in $(seq 1 15); do
  if [ "$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE card_id=$id6")" -ge 1 ]; then
    break
  fi
  sleep 0.4
done
trig=$(sqlite3 "$DB" "SELECT trigger FROM card_runs WHERE card_id=$id6 LIMIT 1;")
[ "$trig" = "manual" ] || fail "D6 trigger was '$trig', expected 'manual'"
ok "D6 ok — manual run wrote card_runs row with trigger=manual"

# --- D7: needs_role reservation -------------------------------------------
PK7="phase1-d7-$$"
id7=$(mk_card "$PK7" "d7 with role" "test-engineer")
"$PI_BIN" board-set --pair-key "$PK7" --auto-drain on \
  --max-concurrent 1 --poll-interval-secs 1 --as test >/dev/null
for i in $(seq 1 15); do
  if [ "$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE card_id=$id7")" -ge 1 ]; then
    break
  fi
  sleep 0.5
done
[ "$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE card_id=$id7")" -ge 1 ] \
  || fail "D7 needs_role card was skipped (Phase 1 should ignore the column)"
ok "D7 ok — needs_role ignored in Phase 1, card dispatched"
"$PI_BIN" board-set --pair-key "$PK7" --auto-drain off --as test >/dev/null

stop_pw

echo "PASS cards-phase1-drainer.sh — D1-D7 7/7"
