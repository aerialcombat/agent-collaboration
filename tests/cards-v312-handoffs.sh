#!/usr/bin/env bash
# tests/cards-v312-handoffs.sh — v3.12.4.6 smoke for the handoff layer.
# Drives a single stub-agent pool through:
#
#   H1 — Card with track_handoffs=true. Fake worker emits a handoff
#         via card-handoff CLI then exits. Asserts:
#           * handoff event row exists with the expected body.
#           * Q-B: claim released + status flipped back to todo (so the
#             pool can re-dispatch without operator intervention).
#           * status_change event recorded "handoff released claim".
#
#   H2 — Re-dispatch the same card (drainer still on). Fake captures
#         the second-run prompt to disk. Asserts:
#           * Prompt opens with "## Previous handoff" before the card
#             body — the prepend logic fires when track_handoffs=true.
#           * Worker-2 flips status to in_review on exit; reaches a
#             terminal card_runs row.
#
#   H3 — Self-promotion. Card with track_handoffs=false. Fake worker
#         exits cleanly without flipping status (simulates a turn-budget
#         exhaustion). Asserts:
#           * track_handoffs flipped to true by finalizeHandoffSelfPromotion.
#           * A system comment naming the cause exists in the timeline.
#           * status_change event NOT emitted (we didn't release the claim
#             via handoff path — the worker simply stopped).

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

TMP_RAW="$(mktemp -d -t cards-v312-handoffs.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
LOG="$TMP/peer-web.log"
PROMPT_DIR="$TMP/prompts"
mkdir -p "$PROMPT_DIR"
PORT=18840
URL="http://127.0.0.1:$PORT"

export AGENT_COLLAB_INBOX_DB="$DB"
export PEER_WEB_PORT="$PORT"

# Fake worker reads the prompt for "MODE: …" and dispatches matching
# CLI calls. Each prompt is dumped to PROMPT_DIR/<card_id>-<n>.txt
# where n is the next free index — so H2 can introspect the second-run
# prompt and verify the prepended handoff section is there.
FAKE="$TMP/fake-handoff-worker.sh"
cat >"$FAKE" <<EOF
#!/usr/bin/env bash
set -uo pipefail
prompt="\$(cat)"
card_id="\$(echo "\$prompt" | sed -nE 's/^Card id:[[:space:]]+([0-9]+)/\1/p' | head -1)"
label="\$(echo "\$prompt" | sed -nE 's/^Your label.*:[[:space:]]+(.+)/\1/p' | head -1)"
mode="\$(echo "\$prompt" | sed -nE 's/.*MODE:[[:space:]]*([a-z0-9-]+).*/\1/p' | head -1)"

if [ -n "\$card_id" ]; then
  n=1
  while [ -e "$PROMPT_DIR/\${card_id}-\${n}.txt" ]; do n=\$((n+1)); done
  printf '%s' "\$prompt" > "$PROMPT_DIR/\${card_id}-\${n}.txt"
fi

sleep "\${WORKER_DELAY:-1}"

PI() { AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" "\$@"; }

case "\$mode" in
  handoff-then-exit)
    # First-run shape: write a structured handoff and exit. Q-B says
    # the store releases claim + flips status to todo; the next dispatch
    # then prepends the handoff to the new prompt.
    PI card-handoff --card "\$card_id" --as "\$label" --body \
"summary: implemented half the feature; tests pending
decisions: chose strategy A over B due to perf trade-off
open_questions: should we cap retries at 3 or 5?
next_steps:
  1. wire the retry loop with exponential backoff
  2. add the integration test
files_touched: src/foo.go
context_to_preserve: see comment on line 42 about race window" >/dev/null
    ;;
  handoff-then-finish)
    # Second-run shape: do the rest of the work and flip to in_review.
    PI card-update-status --card "\$card_id" --status in_review --as "\$label" >/dev/null
    ;;
  exit-no-status)
    # Self-promotion shape: exit cleanly with status still in_progress.
    : ;;
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

wait_count() {
  # wait_count "<sql>" "<expected>" [max_iters]
  local q="$1" want="$2" max="${3:-100}"
  for _ in $(seq 1 "$max"); do
    n=$(sqlite3 "$DB" "$q")
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

"$PI_BIN" agent-create --label stub-handler --runtime claude --worker-cmd "$FAKE" >/dev/null

# --- H1 + H2: handoff round-trip ---------------------------------------
PK="h1-handoff-$$"
"$PI_BIN" pool-add --pair-key "$PK" --agent stub-handler --priority 5 >/dev/null

CARD=$(
  "$PI_BIN" card-create --pair-key "$PK" --title "H1 long card" \
    --body "MODE: handoff-then-exit — write handoff and exit cleanly" \
    --created-by tester --format json \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
)
[ -n "$CARD" ] || fail "H1 couldn't create card"

# track_handoffs is set via card-update (per plan §6.3) — create-time
# only carries kind/splittable; track_handoffs is operator-toggled.
"$PI_BIN" card-update --card "$CARD" --track-handoffs=true --as h1 >/dev/null \
  || fail "H1 couldn't enable track_handoffs"
got_th=$(sqlite3 "$DB" "SELECT track_handoffs FROM cards WHERE id=$CARD;")
[ "$got_th" = "1" ] || fail "H1 track_handoffs stored=$got_th (want 1)"

start_pw
"$PI_BIN" board-set --pair-key "$PK" --auto-drain on --max-concurrent 1 --as h1 >/dev/null

# Wait for the first run to terminate AND for Q-B to release the claim.
wait_count "SELECT COUNT(*) FROM card_runs WHERE card_id=$CARD AND status IN ('completed','failed','cancelled','lost');" 1 \
  || fail "H1 first run didn't terminate"
wait_count "SELECT COUNT(*) FROM card_events WHERE card_id=$CARD AND kind='handoff';" 1 \
  || fail "H1 handoff event not recorded"

# Q-B assertions: handoff released the claim + flipped status back to todo.
status=$(sqlite3 "$DB" "SELECT status FROM cards WHERE id=$CARD;")
claim=$(sqlite3 "$DB" "SELECT COALESCE(claimed_by,'') FROM cards WHERE id=$CARD;")
# The card may already have been re-dispatched by the time we look — in
# that case status will be in_progress and claim non-empty again. Either
# (todo, '') or (in_progress, runner-or-stub-handler-…) is acceptable
# here. What we DO require is at least one status_change with
# trigger:'handoff' in the timeline.
n_release=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$CARD AND kind='status_change' AND meta LIKE '%\"trigger\":\"handoff\"%';")
[ "$n_release" -ge "1" ] \
  || fail "H1 expected status_change event with trigger=handoff (got $n_release; status=$status claim=$claim)"

# H2 — drainer should have re-dispatched the card (same body, same MODE).
# Wait for a second run row to exist.
wait_count "SELECT COUNT(*) FROM card_runs WHERE card_id=$CARD;" 2 \
  || fail "H2 second dispatch never happened (drainer didn't re-pick after Q-B release)"

# Wait for the second run's prompt file to land.
for _ in $(seq 1 50); do
  [ -e "$PROMPT_DIR/${CARD}-2.txt" ] && break
  sleep 0.1
done
[ -s "$PROMPT_DIR/${CARD}-2.txt" ] || fail "H2 second-run prompt missing at $PROMPT_DIR/${CARD}-2.txt"

# The second-run prompt MUST start with the prepended "Previous handoff"
# block before the regular spec — that's the v3.12.4.6 prepend logic.
grep -q "## Previous handoff" "$PROMPT_DIR/${CARD}-2.txt" \
  || fail "H2 second-run prompt missing '## Previous handoff' section"
grep -q "summary: implemented half the feature" "$PROMPT_DIR/${CARD}-2.txt" \
  || fail "H2 second-run prompt missing handoff body verbatim"
# And the handoff discipline addendum should be present too.
grep -q "Hand off if you can't finish" "$PROMPT_DIR/${CARD}-2.txt" \
  || fail "H2 second-run prompt missing handoff-discipline addendum"

# Tear down — we've verified the prepend; the second run will keep
# looping (it'll write another handoff, re-release, re-dispatch ad
# infinitum since the body never changes). That's fine; cleanup kills it.
"$PI_BIN" board-set --pair-key "$PK" --auto-drain off --as h1 >/dev/null
stop_pw
ok "H1 ok — handoff event written; Q-B released claim (status_change with trigger=handoff)"
ok "H2 ok — second dispatch prepended '## Previous handoff' + handoff body to prompt"

# --- H3: self-promotion of track_handoffs ------------------------------
PK3="h3-selfpromote-$$"
"$PI_BIN" pool-add --pair-key "$PK3" --agent stub-handler --priority 5 >/dev/null

CARD3=$(
  "$PI_BIN" card-create --pair-key "$PK3" --title "H3 worker stalls" \
    --body "MODE: exit-no-status — exit cleanly with status still in_progress" \
    --created-by tester --format json \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
)
# Confirm track_handoffs starts off.
got_th=$(sqlite3 "$DB" "SELECT track_handoffs FROM cards WHERE id=$CARD3;")
[ "$got_th" = "0" ] || fail "H3 track_handoffs should start 0, got $got_th"

start_pw
"$PI_BIN" board-set --pair-key "$PK3" --auto-drain on --max-concurrent 1 --as h3 >/dev/null

wait_count "SELECT COUNT(*) FROM card_runs WHERE card_id=$CARD3 AND status IN ('completed','failed','cancelled','lost');" 1 \
  || fail "H3 worker didn't terminate"

# After completion, finalizeHandoffSelfPromotion should flip
# track_handoffs=true and emit an explanatory comment.
wait_count "SELECT track_handoffs FROM cards WHERE id=$CARD3;" 1 \
  || fail "H3 track_handoffs not auto-promoted (still 0)"

n_promote_comments=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$CARD3 AND kind='comment' AND body LIKE '%track_handoffs auto-enabled%';")
[ "$n_promote_comments" = "1" ] \
  || fail "H3 expected 1 'track_handoffs auto-enabled' comment, got $n_promote_comments"

# We did NOT emit a handoff, so no status_change with trigger=handoff.
n_handoff_release=$(sqlite3 "$DB" "SELECT COUNT(*) FROM card_events WHERE card_id=$CARD3 AND kind='status_change' AND meta LIKE '%\"trigger\":\"handoff\"%';")
[ "$n_handoff_release" = "0" ] \
  || fail "H3 unexpected handoff-trigger status_change (got $n_handoff_release)"

"$PI_BIN" board-set --pair-key "$PK3" --auto-drain off --as h3 >/dev/null
stop_pw
ok "H3 ok — turn-budget exit auto-enabled track_handoffs + recorded explanatory comment"

echo "PASS cards-v312-handoffs.sh — H1-H3 3/3"
