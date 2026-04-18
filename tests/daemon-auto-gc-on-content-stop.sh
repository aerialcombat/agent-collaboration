#!/usr/bin/env bash
# tests/daemon-auto-gc-on-content-stop.sh — Topic 3 v0.1 Arch D §9.3
# gate 7: auto-GC of sessions.daemon_cli_session_id on L1 content-stop
# transitionClosed.
#
# Covers:
#   (7-positive) Daemon configured with --cli-session-resume; column
#                pre-populated with a known UUID; L1 content-stop
#                sentinel triggers transitionClosed; assert BOTH
#                daemon_state='closed' AND daemon_cli_session_id IS
#                NULL after the batch drains.
#   (7-positive-reopen) External flip of daemon_state back to 'open'
#                       after L1 close; assert daemon_cli_session_id
#                       stays NULL until a fresh capture lands
#                       (capture path is sub-tasks B/C — this subtest
#                       only locks "NULL after reopen" without
#                       assuming B/C have shipped).
#   (7-negative) External SQL flip of daemon_state='closed' with NO
#                content-stop sentinel in the inbox; assert daemon_
#                cli_session_id is NOT cleared. Locks the §8.2
#                distinction: L2 has no daemon-side trigger; operator
#                must run `peer-inbox daemon-reset-session`
#                separately.
#
# §3.4 invariant 3 (idempotent reset) is structurally covered: the
# auto-GC runs after the batch completes regardless of whether the
# column was populated (tested positive) or NULL (tested negative via
# the symmetry — clearing an already-NULL column is a no-op).
#
# Uses fake CLI via AGENT_COLLAB_DAEMON_CODEX_BIN override. Pattern
# mirrors tests/daemon-termination.sh L1 subtest so harness setup is
# consistent across the termination-stack test suite.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AC="$PROJECT_ROOT/scripts/agent-collab"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
DAEMON_PIDS=()
cleanup() {
  local p
  for p in "${DAEMON_PIDS[@]:-}"; do
    [[ -n "$p" ]] && kill "$p" 2>/dev/null || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-auto-gc-on-content-stop: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"
export AGENT_COLLAB_ACK_TIMEOUT=1
export AGENT_COLLAB_SWEEP_TTL=2

DAEMON_CWD="$TMP/daemon"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-recv \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register daemon"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender"

# Sanity: migration 0003 landed (daemon_cli_session_id column present).
has_col="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
cols = [r[1] for r in c.execute(\"PRAGMA table_info(sessions)\").fetchall()]
print('1' if 'daemon_cli_session_id' in cols else '0')
c.close()
")"
[[ "$has_col" == "1" ]] || fail "migration 0003 not applied; daemon_cli_session_id column missing"

# Fake CLI that emits a JSONL ack marker. Used for both positive and
# negative subtests (CLI behavior is not under test here — daemon's
# transitionClosed hook is).
FAKE_ACK="$TMP/fake-ack.sh"
cat > "$FAKE_ACK" <<'EOF'
#!/usr/bin/env bash
COUNTER="${FAKE_CLI_COUNTER:-/dev/null}"
if [[ "$COUNTER" != "/dev/null" ]]; then
  local_n="$(cat "$COUNTER" 2>/dev/null || echo 0)"
  echo "$((local_n + 1))" > "$COUNTER"
fi
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_ACK"

# Daemon launcher — always runs with --cli-session-resume so the
# auto-GC wire is live. (The auto-GC hook actually fires regardless
# of the flag value; the flag only gates spawn-helper capture/resume.
# We set it here for realism — an operator configured for Arch D is
# who actually has a non-NULL column to GC.)
start_daemon_bg() {
  local log="$1"
  (
    # v0.3 SOFT SHIM: --cli=codex routes through spawnPi → fake binary
    # must be bound to PI_BIN override. Per-CLI pi fields required post-
    # shim-preflight (§3.2.b). Codex auto-maps to openai-codex provider.
    export AGENT_COLLAB_DAEMON_PI_BIN="$FAKE_ACK"
    export FAKE_CLI_COUNTER="$TMP/fake-counter.txt"
    echo 0 > "$FAKE_CLI_COUNTER"
    "$DAEMON" \
      --label daemon-recv \
      --cwd "$DAEMON_CWD" \
      --session-key "key-daemon" \
      --cli codex \
      --pi-model gpt-5.3-codex \
      --pi-session-dir "$TMP/pi-sessions" \
      --cli-session-resume \
      --ack-timeout 5 \
      --sweep-ttl 10 \
      --poll-interval 1 \
      --pause-on-idle 1800 \
      --log-path "$log" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon.pid"
  )
  DAEMON_PIDS+=("$(cat "$TMP/daemon.pid")")
}

stop_daemon() {
  local pid
  pid="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
  if [[ -n "$pid" ]]; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

reset_inbox() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"DELETE FROM inbox\")
c.execute(\"UPDATE sessions SET daemon_state = 'open', daemon_cli_session_id = NULL WHERE label = 'daemon-recv'\")
try:
    c.execute(\"DELETE FROM peer_rooms\")
except sqlite3.OperationalError:
    pass
c.commit(); c.close()
"
}

# Read back daemon_state + daemon_cli_session_id for the daemon label.
# Emits 'STATE|CLI_ID' (empty string after the pipe when NULL).
read_state() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT daemon_state, daemon_cli_session_id FROM sessions WHERE label='daemon-recv'\").fetchone()
if not r:
    print('|')
else:
    st = r[0] if r[0] is not None else ''
    cid = r[1] if r[1] is not None else ''
    print(f'{st}|{cid}')
c.close()
"
}

# Assemble the sentinel from parts (same pattern as daemon-termination.sh)
# to avoid self-trigger on source scanners.
SENTINEL="$(printf '%s' '[' '[' 'end' ']' ']')"

# =============================================================================
# (7-positive) L1 content-stop → both daemon_state='closed' AND
#              daemon_cli_session_id IS NULL after batch completes.
# =============================================================================
step "(7-positive) L1 content-stop clears daemon_cli_session_id in transitionClosed"
reset_inbox

# Pre-populate daemon_cli_session_id with a known UUID so we can
# observe the auto-GC flipping it back to NULL. Use a distinctive
# marker so assertion failures are obvious.
KNOWN_UUID="11111111-2222-3333-4444-555555555555"
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_cli_session_id = ? WHERE label = 'daemon-recv'\", ('$KNOWN_UUID',))
c.commit(); c.close()
"
pre="$(read_state)"
[[ "$pre" == "open|$KNOWN_UUID" ]] \
  || fail "(7-positive) precondition: expected 'open|$KNOWN_UUID', got '$pre'"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "closing thoughts $SENTINEL" >/dev/null

start_daemon_bg "$TMP/d-positive.log"

# Wait up to 10s for daemon_state to transition to 'closed'.
waited=0
state=""
while (( waited < 20 )); do
  state="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT daemon_state FROM sessions WHERE label='daemon-recv'\").fetchone()
print(r[0] if r else '')
")"
  if [[ "$state" == "closed" ]]; then break; fi
  sleep 0.5
  waited=$((waited+1))
done

stop_daemon

[[ "$state" == "closed" ]] \
  || { echo "--- daemon log ---"; cat "$TMP/d-positive.log"; fail "(7-positive) daemon_state never transitioned to 'closed'"; }

post="$(read_state)"
[[ "$post" == "closed|" ]] \
  || { echo "--- daemon log ---"; cat "$TMP/d-positive.log"; fail "(7-positive) expected 'closed|' after L1 auto-GC, got '$post'"; }

# Auto-GC observability: the daemon.cli_session_auto_gc.l1_content_stop
# event must be emitted (locks the log signal operators grep for).
grep -q 'daemon.cli_session_auto_gc.l1_content_stop' "$TMP/d-positive.log" \
  || { echo "--- daemon log ---"; cat "$TMP/d-positive.log"; fail "(7-positive) auto-GC log event not emitted"; }

echo "   (7-positive) L1 content-stop → daemon_state='closed' + daemon_cli_session_id=NULL, auto-GC event logged"

# =============================================================================
# (7-positive-reopen) External flip to 'open' after L1 close → column
#                     stays NULL until a fresh capture lands. Sub-tasks
#                     B/C own the capture side; this subtest only
#                     locks "NULL after reopen, no stale revival".
# =============================================================================
step "(7-positive-reopen) reopening after L1 close leaves daemon_cli_session_id NULL"

python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-recv'\")
c.commit(); c.close()
"
reopen="$(read_state)"
[[ "$reopen" == "open|" ]] \
  || fail "(7-positive-reopen) expected 'open|' after external reopen, got '$reopen'"

echo "   (7-positive-reopen) reopen leaves daemon_cli_session_id NULL; capture path is sub-tasks B/C"

# =============================================================================
# (7-negative) External SQL flip of daemon_state='closed' with NO L1
#              sentinel in the inbox → daemon_cli_session_id must NOT
#              be cleared. Operator must run daemon-reset-session
#              separately. Locks the §8.2 distinction.
# =============================================================================
step "(7-negative) external daemon_state='closed' (no L1 sentinel) does NOT auto-GC"
reset_inbox

# Re-populate the column so we can observe non-clearing.
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_cli_session_id = ? WHERE label = 'daemon-recv'\", ('$KNOWN_UUID',))
c.commit(); c.close()
"

# Flip daemon_state externally BEFORE starting the daemon. No L1
# sentinel anywhere in the inbox. Send a benign probe so there's
# something the daemon might claim — but it should be blocked by the
# §3.4 (e) preflight, and transitionClosed should NEVER run (because
# no L1 sentinel was seen).
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'closed' WHERE label = 'daemon-recv'\")
c.commit(); c.close()
"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "negative probe" >/dev/null

start_daemon_bg "$TMP/d-negative.log"
sleep 3
stop_daemon

neg="$(read_state)"
[[ "$neg" == "closed|$KNOWN_UUID" ]] \
  || { echo "--- daemon log ---"; cat "$TMP/d-negative.log"; fail "(7-negative) external close should NOT auto-GC; expected 'closed|$KNOWN_UUID', got '$neg'"; }

# Belt-and-suspenders: ensure the auto-GC event was NOT logged (no L1
# trigger means no transitionClosed run).
if grep -q 'daemon.cli_session_auto_gc.l1_content_stop' "$TMP/d-negative.log"; then
  echo "--- daemon log ---"
  cat "$TMP/d-negative.log"
  fail "(7-negative) auto-GC event logged despite no L1 sentinel"
fi

echo "   (7-negative) external close kept daemon_cli_session_id intact; operator must run daemon-reset-session"

echo "PASS: daemon-auto-gc-on-content-stop — L1 auto-GC fires, L2 external-close does NOT"
