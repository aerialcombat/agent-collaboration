#!/usr/bin/env bash
# tests/cards-v312-pool-dispatch.sh — v3.12.4 pool-aware drainer smoke.
#
# Drives the full dispatch path with a pool of two stub agents (each
# pointing at fake-worker.sh via worker_cmd) and verifies:
#
#   E1 — Role-match: card needs_role=impl → only the impl-tagged agent runs
#   E2 — Lowest-load tiebreak: 2 cards, each goes to a different agent
#   E3 — Slot saturation: agent count=1 + designated-by-role → second
#         eligible card waits, then a different agent picks it up
#   E4 — Designated assignment overrides role: card needs=impl assigned
#         to the review agent → review agent runs it
#   E5 — Empty pool fallback: no pool members → drainer uses
#         AGENT_COLLAB_WORKER_CMD (Phase 1 path, agent_id = NULL)
#   E6 — Disabled agent skipped: pool member disabled → others picked
#   E7 — Worker label embeds agent label: runner row's worker_label
#         starts with "{agent.label}-"

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

TMP_RAW="$(mktemp -d -t cards-v312-pool-dispatch.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PORT=18800
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

FAKE="$TMP/fake-worker.sh"
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
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
export WORKER_DELAY=2

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
  fail "peer-web didn't start on $URL within 6s"
}
stop_pw() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true
  fi
  PW_PID=""
}

mk_card() {
  local pk="$1" title="$2" role="${3:-}"
  local args=(card-create --pair-key "$pk" --title "$title" --created-by tester --format json)
  [ -n "$role" ] && args+=(--needs-role "$role")
  "$PI_BIN" "${args[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
}

# Wait until card_runs has $1 rows where status='running' for $pk
wait_running() {
  local pk="$1" want="$2" max="${3:-50}"
  for _ in $(seq 1 "$max"); do
    n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status='running';")
    if [ "$n" = "$want" ]; then return 0; fi
    sleep 0.1
  done
  return 1
}
wait_terminal() {
  local pk="$1" want="$2" max="${3:-100}"
  for _ in $(seq 1 "$max"); do
    n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status IN ('completed','failed','cancelled','lost');")
    if [ "$n" -ge "$want" ]; then return 0; fi
    sleep 0.1
  done
  return 1
}

# --- bootstrap DB ---------------------------------------------------------

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
[ "$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")" = "15" ] \
  || fail "goose version != 15"

# Two stub agents — both point at the fake worker so we don't actually
# need claude/pi runtimes. The pool just decides which one's id gets
# stamped on card_runs.agent_id and which label namespaces the run.
"$PI_BIN" agent-create --label stub-impl   --runtime claude --role impl   --worker-cmd "$FAKE" >/dev/null
"$PI_BIN" agent-create --label stub-review --runtime pi     --role review --worker-cmd "$FAKE" >/dev/null
"$PI_BIN" agent-create --label stub-any    --runtime claude               --worker-cmd "$FAKE" >/dev/null

# --- E1: role-match -------------------------------------------------------
PK1="e1-rolematch-$$"
"$PI_BIN" pool-add --pair-key "$PK1" --agent stub-impl   --priority 5 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK1" --agent stub-review --priority 1 >/dev/null

C1=$(mk_card "$PK1" "needs impl" "impl")
start_pw
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain on --max-concurrent 2 --as e1 >/dev/null
wait_terminal "$PK1" 1 || fail "E1 card didn't terminate"
got_agent=$(sqlite3 "$DB" "SELECT label FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.card_id=$C1 LIMIT 1;")
[ "$got_agent" = "stub-impl" ] || fail "E1 wrong agent: $got_agent (want stub-impl)"
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain off --as e1 >/dev/null
stop_pw
ok "E1 ok — role=impl card routed to stub-impl"

# --- E2: lowest-load tiebreak -------------------------------------------
# Pool: stub-any (priority 0) + stub-impl (priority 5). Cards have no
# needs_role → both eligible. Two cards dispatched ~simultaneously
# should split: priority 5 first, then load-tiebreak picks the other.
PK2="e2-load-$$"
"$PI_BIN" pool-add --pair-key "$PK2" --agent stub-impl --priority 5 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK2" --agent stub-any  --priority 0 >/dev/null
C2A=$(mk_card "$PK2" "load1" "")
C2B=$(mk_card "$PK2" "load2" "")
start_pw
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain on --max-concurrent 2 --as e2 >/dev/null
wait_terminal "$PK2" 2 || fail "E2 cards didn't both terminate"
got_agents=$(sqlite3 "$DB" "SELECT label FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.pair_key='$PK2' ORDER BY cr.id;")
# Either agent can pick first depending on tick timing; the assertion
# is that BOTH agents were used.
distinct=$(echo "$got_agents" | sort -u | wc -l | tr -d ' ')
[ "$distinct" = "2" ] || fail "E2 expected both agents to run, got: $got_agents"
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain off --as e2 >/dev/null
stop_pw
ok "E2 ok — both pool members ran (lowest-load tiebreak distributed work)"

# --- E3: slot saturation drops a saturated agent ------------------------
# stub-impl with count=1, stub-any with count=2. Three cards (no role).
# Should fan out 1 + 2.
PK3="e3-sat-$$"
"$PI_BIN" pool-add --pair-key "$PK3" --agent stub-impl --count 1 --priority 0 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK3" --agent stub-any  --count 2 --priority 0 >/dev/null
for i in 1 2 3; do mk_card "$PK3" "sat-$i" >/dev/null; done
start_pw
"$PI_BIN" board-set --pair-key "$PK3" --auto-drain on --max-concurrent 3 --as e3 >/dev/null
wait_terminal "$PK3" 3 || fail "E3 didn't drain 3 cards"
n_impl=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.pair_key='$PK3' AND a.label='stub-impl';")
n_any=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.pair_key='$PK3' AND a.label='stub-any';")
# stub-impl can serve only 1 concurrent card; over 3 cards we expect
# stub-impl=1, stub-any=2 (slot-saturation respected throughout).
[ "$n_impl" = "1" ] || fail "E3 stub-impl ran $n_impl times (expected 1)"
[ "$n_any" = "2" ] || fail "E3 stub-any  ran $n_any times (expected 2)"
"$PI_BIN" board-set --pair-key "$PK3" --auto-drain off --as e3 >/dev/null
stop_pw
ok "E3 ok — slot-saturated agent dropped; load fanned out 1 + 2"

# --- E4: designated overrides role match -------------------------------
PK4="e4-designated-$$"
"$PI_BIN" pool-add --pair-key "$PK4" --agent stub-impl   --priority 10 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK4" --agent stub-review --priority 0  >/dev/null
C4=$(mk_card "$PK4" "designated" "impl")
"$PI_BIN" card-assign --card "$C4" --agent stub-review --as e4 >/dev/null \
  || fail "E4 assign failed"
start_pw
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain on --max-concurrent 2 --as e4 >/dev/null
wait_terminal "$PK4" 1 || fail "E4 card didn't terminate"
got_agent=$(sqlite3 "$DB" "SELECT label FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.card_id=$C4 LIMIT 1;")
[ "$got_agent" = "stub-review" ] || fail "E4 designated lost: got $got_agent (want stub-review)"
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain off --as e4 >/dev/null
stop_pw
ok "E4 ok — designated assignment overrode role-match"

# --- E5: empty pool fallback ------------------------------------------
PK5="e5-empty-$$"
C5=$(mk_card "$PK5" "fallback" "")
# Empty pool, AGENT_COLLAB_WORKER_CMD env exported → Phase 1 fallback.
export AGENT_COLLAB_WORKER_CMD="$FAKE"
start_pw
"$PI_BIN" board-set --pair-key "$PK5" --auto-drain on --max-concurrent 1 --as e5 >/dev/null
wait_terminal "$PK5" 1 || fail "E5 fallback didn't run"
got_agent_id=$(sqlite3 "$DB" "SELECT COALESCE(agent_id, 0) FROM card_runs WHERE card_id=$C5 LIMIT 1;")
[ "$got_agent_id" = "0" ] || fail "E5 expected NULL agent_id, got $got_agent_id"
got_label=$(sqlite3 "$DB" "SELECT worker_label FROM card_runs WHERE card_id=$C5 LIMIT 1;")
echo "$got_label" | grep -q "^runner-" \
  || fail "E5 worker_label should start with 'runner-' (Phase 1 fallback): got $got_label"
"$PI_BIN" board-set --pair-key "$PK5" --auto-drain off --as e5 >/dev/null
stop_pw
unset AGENT_COLLAB_WORKER_CMD
ok "E5 ok — empty pool fell back to AGENT_COLLAB_WORKER_CMD; agent_id NULL"

# --- E6: disabled agent skipped ---------------------------------------
PK6="e6-disabled-$$"
"$PI_BIN" pool-add --pair-key "$PK6" --agent stub-impl   --priority 10 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK6" --agent stub-review --priority 0  >/dev/null
"$PI_BIN" agent-update --label stub-impl --enabled off >/dev/null
C6=$(mk_card "$PK6" "skip-disabled" "")
start_pw
"$PI_BIN" board-set --pair-key "$PK6" --auto-drain on --max-concurrent 1 --as e6 >/dev/null
wait_terminal "$PK6" 1 || fail "E6 card didn't run"
got_agent=$(sqlite3 "$DB" "SELECT label FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE cr.card_id=$C6 LIMIT 1;")
[ "$got_agent" = "stub-review" ] || fail "E6 disabled agent ran: got $got_agent"
"$PI_BIN" agent-update --label stub-impl --enabled on >/dev/null
"$PI_BIN" board-set --pair-key "$PK6" --auto-drain off --as e6 >/dev/null
stop_pw
ok "E6 ok — disabled stub-impl skipped; stub-review picked it up"

# --- E7: worker_label namespacing -------------------------------------
# Use any prior run that went through the pool — E1's stub-impl run.
ns_label=$(sqlite3 "$DB" "SELECT worker_label FROM card_runs cr JOIN agents a ON a.id = cr.agent_id WHERE a.label='stub-impl' ORDER BY cr.id LIMIT 1;")
echo "$ns_label" | grep -q "^stub-impl-" \
  || fail "E7 worker_label should start with 'stub-impl-': got $ns_label"
ok "E7 ok — pool-routed run's worker_label namespaced by agent: $ns_label"

# --- E8: needs_role with no pool match → audit warning + fallback ------
# Track 1 #3 (linkboard dogfood obs #3): card with needs_role that no
# pool member can satisfy must dispatch on the fallback worker AND
# emit a system comment so the operator sees the audit gap.
PK8="e8-rolemiss-$$"
"$PI_BIN" pool-add --pair-key "$PK8" --agent stub-impl   --priority 5 >/dev/null
"$PI_BIN" pool-add --pair-key "$PK8" --agent stub-review --priority 1 >/dev/null
C8=$(mk_card "$PK8" "needs missing role" "qa")  # pool has impl + review, NOT qa
export AGENT_COLLAB_WORKER_CMD="$FAKE"
start_pw
"$PI_BIN" board-set --pair-key "$PK8" --auto-drain on --max-concurrent 1 --as e8 >/dev/null
wait_terminal "$PK8" 1 || fail "E8 fallback didn't run"
got_agent_id=$(sqlite3 "$DB" "SELECT COALESCE(agent_id, 0) FROM card_runs WHERE card_id=$C8 LIMIT 1;")
[ "$got_agent_id" = "0" ] || fail "E8 expected agent_id=NULL (fallback), got $got_agent_id"
n_warn=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$C8 AND kind='comment' AND author='system' AND body LIKE 'no pool member matches role%'")
[ "$n_warn" = "1" ] || fail "E8 expected 1 no-pool-match warning comment, got $n_warn"
n_warn_meta=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$C8 AND meta LIKE '%\"reason\":\"no_pool_match\"%'")
[ "$n_warn_meta" = "1" ] || fail "E8 expected meta.reason=no_pool_match in warning, got $n_warn_meta"
"$PI_BIN" board-set --pair-key "$PK8" --auto-drain off --as e8 >/dev/null
stop_pw
unset AGENT_COLLAB_WORKER_CMD
ok "E8 ok — needs_role with no pool match → fallback dispatch + audit warning"

echo "PASS cards-v312-pool-dispatch.sh — E1-E8 8/8"
