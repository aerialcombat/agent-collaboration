#!/usr/bin/env bash
# tests/cards-v312-decomposition.sh — v3.12.4.5 smoke for the
# decomposition layer (kind=epic + splittable=true). Drives the full
# dispatch path with a single stub agent backed by fake-worker.sh that
# reads the card body for a "MODE: …" line and performs the matching
# decomposition flow.
#
#   F1 — kind=epic dispatched → fake decomposer emits 3 child cards via
#         the CLI, wires each as blockee of the epic, exits → drainer
#         promotes the epic to done; 3 children exist.
#   F2 — splittable=true card → fake worker emits 2 child cards, wires
#         each as blocker of the parent, comments rationale, flips parent
#         back to todo, exits → parent has 2 open blockers + comment.
#   F3 — prompt addenda assertion: dispatched epic prompt contains the
#         decomposer wording; dispatched splittable prompt contains the
#         "max 5 children" cap. Cap enforcement is prompt-level only in
#         v3.12.4.5 — verifies the discipline reaches the worker.
#   F4 — kind=epic dispatched but the fake decomposer exits without
#         creating any children → epic stays in_progress + a system
#         comment event records "decomposer exited cleanly without
#         creating children" so an operator can review.

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

TMP_RAW="$(mktemp -d -t cards-v312-decomposition.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PROMPT_DIR="$TMP/prompts"
mkdir -p "$PROMPT_DIR"
PORT=18820
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

# Fake worker reads the prompt body for a "MODE: foo" line and dispatches
# the matching CLI sequence. Dumps the prompt to PROMPT_DIR/<card_id>.txt
# so F3 can introspect.
FAKE="$TMP/fake-decomposer.sh"
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
set -uo pipefail
prompt="\$(cat)"
card_id="\$(echo "\$prompt" | sed -nE 's/^(Epic card id|Card id):[[:space:]]+([0-9]+)/\2/p' | head -1)"
pair_key="\$(echo "\$prompt" | sed -nE 's/^Pair key:?[[:space:]]+(.+)/\1/p' | head -1)"
label="\$(echo "\$prompt" | sed -nE 's/^Your label.*:[[:space:]]+(.+)/\1/p' | head -1)"
mode="\$(echo "\$prompt" | sed -nE 's/.*MODE:[[:space:]]*([a-z0-9-]+).*/\1/p' | head -1)"

[ -n "\$card_id" ] && printf '%s' "\$prompt" > "$PROMPT_DIR/\${card_id}.txt"
sleep "\${WORKER_DELAY:-1}"

PI() { AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" "\$@"; }

case "\$mode" in
  decompose-3)
    for i in 1 2 3; do
      cid=\$(PI card-create --pair-key "\$pair_key" --title "child-\$i of \$card_id" \
        --created-by "\$label" --splittable=true --format json \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
      PI card-add-dep --blocker "\$card_id" --blockee "\$cid" --as "\$label" >/dev/null
    done
    ;;
  split-2)
    for i in 1 2; do
      cid=\$(PI card-create --pair-key "\$pair_key" --title "split-\$i of \$card_id" \
        --created-by "\$label" --format json \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
      PI card-add-dep --blocker "\$cid" --blockee "\$card_id" --as "\$label" >/dev/null
    done
    PI card-comment --card "\$card_id" \
      --body "split: scope larger than expected — see children" --as "\$label" >/dev/null
    PI card-update-status --card "\$card_id" --status todo --as "\$label" >/dev/null
    ;;
  decompose-noop|"")
    : # exit cleanly without creating children
    ;;
esac
exit 0
EOF
chmod +x "$FAKE"
export WORKER_DELAY=1

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

# Wait until $1 card_runs rows for $pk reach a terminal state.
wait_terminal() {
  local pk="$1" want="$2" max="${3:-100}"
  for _ in $(seq 1 "$max"); do
    n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE pair_key='$pk' AND status IN ('completed','failed','cancelled','lost');")
    if [ "$n" -ge "$want" ]; then return 0; fi
    sleep 0.1
  done
  return 1
}

# Wait until card $1 has status $2.
wait_status() {
  local cid="$1" want="$2" max="${3:-50}"
  for _ in $(seq 1 "$max"); do
    s=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$cid;")
    if [ "$s" = "$want" ]; then return 0; fi
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
[ "$(sqlite3 "$DB" "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1")" = "14" ] \
  || fail "goose version != 14"

# Single stub agent backing the fake decomposer/worker. Pool-routed so
# the dispatch path stamps card_runs.agent_id and exercises the v3.12.4
# pool dispatcher; behavior under test is the v3.12.4.5 prompt + epic
# auto-promote layered on top.
"$PI_BIN" agent-create --label stub-decomposer --runtime claude --worker-cmd "$FAKE" >/dev/null

# --- F1: kind=epic decomposes into 3 children ---------------------------
PK1="f1-epic-$$"
"$PI_BIN" pool-add --pair-key "$PK1" --agent stub-decomposer --priority 5 >/dev/null

EPIC=$(
  "$PI_BIN" card-create --pair-key "$PK1" --title "F1 epic" \
    --body "MODE: decompose-3 — split into 3 child tasks" \
    --kind epic --created-by tester --format json \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
)
[ -n "$EPIC" ] || fail "F1 couldn't create epic"

# Sanity: card was stored with kind=epic.
got_kind=$(sqlite3 "$DB" "SELECT kind FROM cards WHERE id=$EPIC;")
[ "$got_kind" = "epic" ] || fail "F1 epic stored kind=$got_kind (want 'epic')"

start_pw
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain on --max-concurrent 1 --as f1 >/dev/null
wait_terminal "$PK1" 1 || fail "F1 epic decomposer didn't terminate"
wait_status "$EPIC" "done" 80 || {
  s=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$EPIC;")
  fail "F1 epic should be 'done', got '$s'"
}
n_children=$(sqlite3 "$DB" "SELECT COUNT(*) FROM cards WHERE pair_key='$PK1' AND id != $EPIC;")
[ "$n_children" = "3" ] || fail "F1 expected 3 children, got $n_children"
n_deps=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_dependencies WHERE blocker_id=$EPIC;")
[ "$n_deps" = "3" ] || fail "F1 expected 3 deps off epic, got $n_deps"
# Children should default splittable=1 (decomposer passed --splittable=true).
n_splittable=$(sqlite3 "$DB" "SELECT COUNT(*) FROM cards WHERE pair_key='$PK1' AND id != $EPIC AND splittable=1;")
[ "$n_splittable" = "3" ] || fail "F1 expected 3 splittable children, got $n_splittable"
"$PI_BIN" board-set --pair-key "$PK1" --auto-drain off --as f1 >/dev/null
stop_pw
ok "F1 ok — epic decomposed into 3 splittable children, auto-promoted to done"

# --- F2: splittable=true card splits mid-session into 2 children --------
PK2="f2-split-$$"
"$PI_BIN" pool-add --pair-key "$PK2" --agent stub-decomposer --priority 5 >/dev/null

PARENT=$(
  "$PI_BIN" card-create --pair-key "$PK2" --title "F2 splittable parent" \
    --body "MODE: split-2 — discovered hidden scope, split into 2" \
    --splittable=true --created-by tester --format json \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
)
[ -n "$PARENT" ] || fail "F2 couldn't create parent"

start_pw
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain on --max-concurrent 1 --as f2 >/dev/null
wait_terminal "$PK2" 1 || fail "F2 worker didn't terminate"

# Parent should be back in 'todo' (worker re-set it) with 2 open blockers.
wait_status "$PARENT" "todo" 80 || {
  s=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$PARENT;")
  fail "F2 parent should be 'todo' after split, got '$s'"
}
n_blockers=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_dependencies WHERE blockee_id=$PARENT;")
[ "$n_blockers" = "2" ] || fail "F2 expected 2 blockers on parent, got $n_blockers"
n_split_comments=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$PARENT AND kind='comment' AND body LIKE 'split:%';")
[ "$n_split_comments" = "1" ] || fail "F2 expected 1 split-rationale comment, got $n_split_comments"

# Track 1 fix #4: → todo from in_progress must release the claim. Without
# this, the parent stays claimed forever and the drainer can never re-pick
# it after children clear (the linkboard dogfood observation).
parent_claim=$(sqlite3 "$DB" "SELECT COALESCE(claimed_by,'') FROM cards WHERE id=$PARENT;")
[ -z "$parent_claim" ] || fail "F2 parent claim should be cleared after split (status flip in_progress→todo), got '$parent_claim'"
n_release_events=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$PARENT AND kind='status_change' AND meta LIKE '%\"trigger\":\"release\"%';")
[ "$n_release_events" -ge 1 ] || fail "F2 expected status_change with trigger=release, got $n_release_events"

# Drainer must NOT re-dispatch the parent until children clear.
n_runs=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE card_id=$PARENT;")
"$PI_BIN" board-set --pair-key "$PK2" --auto-drain off --as f2 >/dev/null
stop_pw
sleep 0.3
n_runs_after=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_runs WHERE card_id=$PARENT;")
[ "$n_runs" = "$n_runs_after" ] \
  || fail "F2 parent re-dispatched while blockers open ($n_runs → $n_runs_after)"
ok "F2 ok — splittable card emitted 2 children, parent reverted to todo with blockers"

# --- F3: prompt addenda are present -------------------------------------
EPIC_PROMPT="$PROMPT_DIR/${EPIC}.txt"
PARENT_PROMPT="$PROMPT_DIR/${PARENT}.txt"
[ -s "$EPIC_PROMPT" ]   || fail "F3 epic prompt missing at $EPIC_PROMPT"
[ -s "$PARENT_PROMPT" ] || fail "F3 splittable prompt missing at $PARENT_PROMPT"

# Decomposer prompt should mention "decomposer" and call out --splittable on for children.
grep -q "job decomposer"  "$EPIC_PROMPT" || fail "F3 epic prompt lacks 'job decomposer' header"
grep -q "splittable"      "$EPIC_PROMPT" || fail "F3 epic prompt lacks splittable instruction"

# Splittable addendum should mention the cap "max 5 children".
grep -q "max 5 children"  "$PARENT_PROMPT" \
  || fail "F3 splittable prompt missing 'max 5 children' cap text"
grep -q "split this card mid-session" "$PARENT_PROMPT" \
  || fail "F3 splittable prompt missing split-discipline header"
ok "F3 ok — decomposer + split-addendum prompt text reaches the worker"

# --- F4: epic with no children → stays in_progress + comment event -----
PK4="f4-noop-$$"
"$PI_BIN" pool-add --pair-key "$PK4" --agent stub-decomposer --priority 5 >/dev/null

NOOP=$(
  "$PI_BIN" card-create --pair-key "$PK4" --title "F4 noop epic" \
    --body "MODE: decompose-noop — exit without creating children" \
    --kind epic --created-by tester --format json \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
)

start_pw
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain on --max-concurrent 1 --as f4 >/dev/null
wait_terminal "$PK4" 1 || fail "F4 worker didn't terminate"

# Status should remain in_progress (not promoted to done because no children).
sleep 0.3
s=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$NOOP;")
[ "$s" = "in_progress" ] \
  || fail "F4 noop epic should stay in_progress, got '$s'"
n_children=$(sqlite3 "$DB" "SELECT COUNT(*) FROM cards WHERE pair_key='$PK4' AND id != $NOOP;")
[ "$n_children" = "0" ] || fail "F4 expected 0 children, got $n_children"
n_review_comments=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$NOOP AND kind='comment' AND body LIKE '%needs human review%';")
[ "$n_review_comments" = "1" ] \
  || fail "F4 expected 1 'needs human review' comment, got $n_review_comments"
"$PI_BIN" board-set --pair-key "$PK4" --auto-drain off --as f4 >/dev/null
stop_pw
ok "F4 ok — empty-decompose epic stays in_progress + system comment recorded"

echo "PASS cards-v312-decomposition.sh — F1-F4 4/4"
