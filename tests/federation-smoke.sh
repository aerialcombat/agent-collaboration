#!/usr/bin/env bash
# tests/federation-smoke.sh — v3.3 symmetric-federation gate.
#
# Exercises items 1 + 3 + 4 + 5 + 7 end-to-end against two separate
# SQLite DBs + one live Go peer-web instance standing in as the remote
# "orange" host. The laptop DB never gains an inbox row — the write
# crosses the HTTP boundary and lands in orange's DB with an
# orange-assigned server_seq.
#
# Scenarios:
#   1. Room bound to remote home → peer-send routes to HTTP; orange
#      assigns server_seq; laptop.db inbox stays empty.
#   2. ULID retry with same message_id → remote returns dedup_hit=true
#      and reuses server_seq (idempotency).
#   3. Orange down → send queues locally (exit 0 + warning); orange up
#      → next send flushes queue in FIFO before the new message.
#   4. Auth enforcement — bearer-less non-owner send is rejected 401;
#      wrong label claim rejected 403.
#
# Cleanup on exit.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI="$PROJECT_ROOT/scripts/peer-inbox-db.py"
PW_BIN="$PROJECT_ROOT/go/bin/peer-web"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"; exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "skip: curl not on PATH"; exit 0
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "skip: sqlite3 not on PATH"; exit 0
fi
if [ ! -x "$PW_BIN" ]; then
  echo "skip: peer-web binary missing at $PW_BIN (run 'go build ./cmd/peer-web')"
  exit 0
fi

TMP="$(mktemp -d -t federation-smoke.XXXXXX)"
ORANGE_DB="$TMP/orange.db"
LAPTOP_DB="$TMP/laptop.db"
ORANGE_CWD="$TMP/o" ; mkdir -p "$ORANGE_CWD"
LAPTOP_CWD="$TMP/l" ; mkdir -p "$LAPTOP_CWD"
REMOTES="$TMP/remotes.json"
PW_LOG="$TMP/peer-web.log"
PW_PORT=$((20000 + RANDOM % 10000))

PW_PID=""
cleanup() {
  if [ -n "$PW_PID" ] && kill -0 "$PW_PID" 2>/dev/null; then
    kill "$PW_PID" 2>/dev/null
    wait "$PW_PID" 2>/dev/null
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  [ -f "$PW_LOG" ] && { echo "--- peer-web log ---" >&2; cat "$PW_LOG" >&2; }
  exit 1
}

pair_key="federation-smoke-$$-$(date +%s)"

run_py() {
  local db="$1"; shift
  local host="$1"; shift
  AGENT_COLLAB_INBOX_DB="$db" \
    AGENT_COLLAB_SELF_HOST="$host" \
    AGENT_COLLAB_REMOTES="$REMOTES" \
    python3 "$PI" "$@"
}

# --- Orange setup: create room, register alpha + laptop_sender ----------
run_py "$ORANGE_DB" orange room-create --pair-key "$pair_key" >/dev/null \
  || fail "orange room-create"
run_py "$ORANGE_DB" orange session-register \
  --cwd "$ORANGE_CWD" --label alpha --agent claude \
  --session-key alpha-orange --pair-key "$pair_key" >/dev/null \
  || fail "register alpha"

REG_OUT="$(run_py "$ORANGE_DB" orange session-register \
  --cwd "$ORANGE_CWD" --label laptop_sender --agent claude \
  --session-key ls-orange --pair-key "$pair_key" 2>&1)"
TOKEN="$(echo "$REG_OUT" | awk '/^auth_token:/ {print $2; exit}')"
[ -n "$TOKEN" ] || fail "register laptop_sender: no token printed"

# --- Laptop setup: remotes config + session registered with --home-host
cat > "$REMOTES" <<EOF
{"orange": {"base_url": "http://127.0.0.1:$PW_PORT", "auth_token_env": "AGENT_COLLAB_TOKEN_ORANGE"}}
EOF
export AGENT_COLLAB_TOKEN_ORANGE="$TOKEN"

run_py "$LAPTOP_DB" laptop session-register \
  --cwd "$LAPTOP_CWD" --label laptop_sender --agent claude \
  --session-key ls-laptop --pair-key "$pair_key" --home-host orange >/dev/null \
  || fail "laptop session-register"

# --- Scenario 3a (reorder): start with ORANGE DOWN. Two sends queue. ---
echo "msg-outage-1" | run_py "$LAPTOP_DB" laptop peer-send \
  --cwd "$LAPTOP_CWD" --as laptop_sender --to alpha --message-stdin --json \
  > "$TMP/r.outage1" || fail "outage send 1 exit non-zero"
grep -q '"push_status": "queued"' "$TMP/r.outage1" \
  || fail "outage send 1 should have queued (got: $(cat "$TMP/r.outage1"))"

echo "msg-outage-2" | run_py "$LAPTOP_DB" laptop peer-send \
  --cwd "$LAPTOP_CWD" --as laptop_sender --to alpha --message-stdin --json \
  > "$TMP/r.outage2" || fail "outage send 2 exit non-zero"

queue_count="$(sqlite3 "$LAPTOP_DB" \
  "SELECT COUNT(*) FROM pending_outbound WHERE home_host='orange'")"
[ "$queue_count" = "2" ] || fail "expected 2 queued rows, got $queue_count"

# --- Start peer-web on orange.db ---------------------------------------
AGENT_COLLAB_INBOX_DB="$ORANGE_DB" AGENT_COLLAB_SELF_HOST=orange \
  "$PW_BIN" --port "$PW_PORT" --pair-key "$pair_key" --cwd "$ORANGE_CWD" \
  > "$PW_LOG" 2>&1 &
PW_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 0.2
  curl -sf "http://127.0.0.1:$PW_PORT/api/scope?pair_key=$pair_key" >/dev/null && break
done
curl -sf "http://127.0.0.1:$PW_PORT/api/scope?pair_key=$pair_key" >/dev/null \
  || fail "peer-web did not come up on port $PW_PORT"

# --- Scenario 3b: next send flushes queue in FIFO before new message ---
echo "msg-recovery" | run_py "$LAPTOP_DB" laptop peer-send \
  --cwd "$LAPTOP_CWD" --as laptop_sender --to alpha --message-stdin --json \
  > "$TMP/r.recovery" || fail "recovery send exit non-zero"
grep -q '"queue_flushed": 2' "$TMP/r.recovery" \
  || fail "recovery send should report queue_flushed=2 (got: $(cat "$TMP/r.recovery"))"

post_queue="$(sqlite3 "$LAPTOP_DB" \
  "SELECT COUNT(*) FROM pending_outbound")"
[ "$post_queue" = "0" ] || fail "queue should be empty post-flush, got $post_queue"

# --- Scenario 1 verification: laptop.db has zero inbox rows ------------
laptop_inbox="$(sqlite3 "$LAPTOP_DB" "SELECT COUNT(*) FROM inbox")"
[ "$laptop_inbox" = "0" ] || fail "laptop.db inbox should be empty, got $laptop_inbox rows"

# --- Scenario 1 verification: orange has FIFO-ordered messages ---------
bodies="$(sqlite3 "$ORANGE_DB" \
  "SELECT trim(body, char(10)) FROM inbox WHERE from_label='laptop_sender' AND body LIKE 'msg-%' ORDER BY server_seq" \
  | tr '\n' '|')"
expected="msg-outage-1|msg-outage-2|msg-recovery|"
[ "$bodies" = "$expected" ] \
  || fail "orange inbox body order mismatch: expected=$expected got=$bodies"

# --- Scenario 2: ULID retry idempotency --------------------------------
RETRY_ID="01SMOKERETRY00000000000000"
echo "retry-body" | run_py "$LAPTOP_DB" laptop peer-send \
  --cwd "$LAPTOP_CWD" --as laptop_sender --to alpha --message-stdin \
  --message-id "$RETRY_ID" --json > "$TMP/r.retry1"
first_seq="$(python3 -c "import json; print(json.load(open('$TMP/r.retry1'))['server_seq'])")"

echo "retry-body" | run_py "$LAPTOP_DB" laptop peer-send \
  --cwd "$LAPTOP_CWD" --as laptop_sender --to alpha --message-stdin \
  --message-id "$RETRY_ID" --json > "$TMP/r.retry2"
grep -q '"dedup_hit": true' "$TMP/r.retry2" \
  || fail "retry should report dedup_hit=true (got: $(cat "$TMP/r.retry2"))"
second_seq="$(python3 -c "import json; print(json.load(open('$TMP/r.retry2'))['server_seq'])")"
[ "$first_seq" = "$second_seq" ] \
  || fail "retry should reuse server_seq $first_seq, got $second_seq"

# --- Scenario 4: auth enforcement on /api/send -------------------------
code_no_token="$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST "http://127.0.0.1:$PW_PORT/api/send?pair_key=$pair_key" \
  -H "Content-Type: application/json" \
  -d '{"from":"laptop_sender","to":"alpha","body":"x","message_id":"AUTH1"}')"
[ "$code_no_token" = "401" ] || fail "no-token send: expected 401, got $code_no_token"

code_wrong_label="$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST "http://127.0.0.1:$PW_PORT/api/send?pair_key=$pair_key" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"from":"alpha","to":"alpha","body":"x","message_id":"AUTH2"}')"
[ "$code_wrong_label" = "403" ] || fail "wrong-label claim: expected 403, got $code_wrong_label"

echo "OK  federation smoke (items 1+3+4+5+7): queue=${queue_count}->0, fifo=preserved, idempotency_retry_seq=${first_seq}, auth_no_token=${code_no_token}, auth_wrong_label=${code_wrong_label}"
