#!/usr/bin/env bash
# tests/daemon-pi-session-lifecycle.sh — Topic 3 v0.2 §9.2 gate 3:
# Full lifecycle create / reuse / L1-content-stop-auto-GC (column NULL'd
# AND session file deleted) / manual reset / re-create in one test flow.
#
# The per-step argv/column inspection is covered in daemon-cli-resume-pi.
# This test focuses on the L1-content-stop-auto-GC path: operator sends
# a message with the session-close sentinel → daemon's transitionClosed
# extension reads the cached path BEFORE Clear, then os.Remove after
# Clear. The session file MUST be gone from disk when the daemon exits
# closed; the column MUST be NULL.

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
  for p in "${DAEMON_PIDS[@]:-}"; do
    [[ -n "$p" ]] && kill "$p" 2>/dev/null || true
  done
  rm -rf "$TMP"
}
if [[ -z "${KEEP_TMP:-}" ]]; then trap cleanup EXIT; fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-pi-session-lifecycle: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) || { echo "skip"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"
export AGENT_COLLAB_ACK_TIMEOUT=5
export AGENT_COLLAB_SWEEP_TTL=10

DAEMON_CWD="$TMP/daemon-pi"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

PI_SESSION_DIR="$TMP/pi-sessions"
EXPECTED_PATH="$PI_SESSION_DIR/daemon-pi.jsonl"

AGENT_COLLAB_SESSION_KEY="key-pi" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-pi \
    --agent pi --receive-mode daemon >/dev/null \
  || fail "register daemon-pi failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "register sender failed"

# Fake pi: appends a line to the session file + emits ack marker.
FAKE_PI="$TMP/fake-pi.sh"
cat > "$FAKE_PI" <<'EOF'
#!/usr/bin/env bash
set -u
sess=""
for (( i=1; i<=$#; i++ )); do
  eval "a=\${$i}"
  if [[ "$a" == "--session" ]]; then
    nxt=$((i+1))
    eval "sess=\${$nxt}"
    break
  fi
done
if [[ -n "$sess" ]]; then
  mkdir -p "$(dirname "$sess")"
  echo '{"turn":"lifecycle-probe"}' >> "$sess"
fi
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_PI"

read_state() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT daemon_state, daemon_cli_session_id FROM sessions WHERE label='daemon-pi'\").fetchone()
if not r: print('|')
else:
    st = r[0] if r[0] is not None else ''
    cid = r[1] if r[1] is not None else ''
    print(f'{st}|{cid}')
c.close()
"
}

reset_inbox() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = 'daemon-pi'\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-pi'\")
c.commit(); c.close()
"
}

start_daemon_bg() {
  local log="$1"
  (
    export AGENT_COLLAB_DAEMON_PI_BIN="$FAKE_PI"
    "$DAEMON" \
      --label daemon-pi \
      --cwd "$DAEMON_CWD" \
      --session-key key-pi \
      --cli pi \
      --pi-provider zai-glm \
      --pi-model glm-4.6 \
      --pi-session-dir "$PI_SESSION_DIR" \
      --cli-session-resume \
      --ack-timeout 5 \
      --sweep-ttl 10 \
      --poll-interval 1 \
      --log-path "$log" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon.pid"
  )
  DAEMON_PIDS+=("$(cat "$TMP/daemon.pid")")
}

stop_daemon() {
  local pid; pid="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
  if [[ -n "$pid" ]]; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

wait_for_path() {
  local path="$1" max="$2"
  local waited=0
  while (( waited < max )); do
    if [[ -s "$path" ]]; then return 0; fi
    sleep 0.5
    waited=$((waited+1))
  done
  return 1
}

SENTINEL="$(printf '%s' '[' '[' 'end' ']' ']')"

# =====================================================================
# (create + reuse) Two ordinary messages; file persists across both.
# =====================================================================
step "create + reuse across 2 batches"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-pi --to-cwd "$DAEMON_CWD" \
    --message "lifecycle-probe-1" >/dev/null

start_daemon_bg "$TMP/d-create.log"
wait_for_path "$EXPECTED_PATH" 20 || { cat "$TMP/d-create.log"; fail "file not created"; }

# Second batch (before daemon exits).
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-pi --to-cwd "$DAEMON_CWD" \
    --message "lifecycle-probe-2" >/dev/null

# Give the daemon time to claim + process the second batch.
sleep 4
stop_daemon

line_count="$(wc -l < "$EXPECTED_PATH" | tr -d ' ')"
(( line_count >= 2 )) || fail "expected >=2 turns in session file, got $line_count"
state1="$(read_state)"
[[ "$state1" == "open|$EXPECTED_PATH" ]] \
  || fail "expected 'open|$EXPECTED_PATH' after 2 batches, got '$state1'"
echo "   file persisted across 2 batches (lines=$line_count)"

# =====================================================================
# (L1-auto-GC) Content-stop sentinel → daemon_state=closed + column NULL
#              + session file DELETED (pi-specific extension §8.2).
# =====================================================================
step "L1 content-stop → state=closed + column NULL + file DELETED"

reset_inbox
# Pre-populate column manually (daemon's capture path writes it on first
# batch anyway, but this makes the test deterministic vs flaky spawn
# timing).
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_cli_session_id = ? WHERE label = 'daemon-pi'\", ('$EXPECTED_PATH',))
c.commit(); c.close()
"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-pi --to-cwd "$DAEMON_CWD" \
    --message "closing thoughts $SENTINEL" >/dev/null

start_daemon_bg "$TMP/d-close.log"

# Wait up to 20s for daemon_state=closed.
waited=0
state=""
while (( waited < 40 )); do
  state="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT daemon_state FROM sessions WHERE label='daemon-pi'\").fetchone()
print(r[0] if r else '')
")"
  if [[ "$state" == "closed" ]]; then break; fi
  sleep 0.5
  waited=$((waited+1))
done
stop_daemon

[[ "$state" == "closed" ]] || { cat "$TMP/d-close.log"; fail "daemon never transitioned to closed"; }

post="$(read_state)"
[[ "$post" == "closed|" ]] \
  || { cat "$TMP/d-close.log"; fail "expected 'closed|' after L1 auto-GC, got '$post'"; }

[[ ! -f "$EXPECTED_PATH" ]] \
  || { cat "$TMP/d-close.log"; fail "session file should be DELETED after L1 auto-GC, still exists"; }

grep -q 'daemon.cli_session_auto_gc.pi_file_deleted' "$TMP/d-close.log" \
  || { cat "$TMP/d-close.log"; fail "expected pi_file_deleted log event"; }

echo "   state=closed | column NULL | file DELETED | pi_file_deleted event logged"

# =====================================================================
# (re-create) Reopen the daemon state externally; next spawn mints fresh.
# =====================================================================
step "reopen + fresh spawn re-mints at same deterministic path"

python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-pi'\")
# Leave prior messages as completed; only the forthcoming reopen-message
# should be unread. Resetting prior rows would re-drive the sentinel
# that triggered L1 auto-GC → daemon would immediately re-close.
try:
    c.execute(\"UPDATE peer_rooms SET terminated_at = NULL, terminated_by = NULL\")
except sqlite3.OperationalError:
    pass
c.commit(); c.close()
"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-pi --to-cwd "$DAEMON_CWD" \
    --message "lifecycle-probe-reopen" 2>"$TMP/send-err.txt" >/dev/null \
  || { cat "$TMP/send-err.txt"; fail "reopen-send failed"; }

start_daemon_bg "$TMP/d-reopen.log"
wait_for_path "$EXPECTED_PATH" 20 || { cat "$TMP/d-reopen.log"; fail "file not re-created after reopen"; }
sleep 0.5
stop_daemon

final="$(read_state)"
[[ "$final" == "open|$EXPECTED_PATH" ]] \
  || { cat "$TMP/d-reopen.log"; fail "expected 'open|$EXPECTED_PATH' after re-mint, got '$final'"; }

echo "   re-mint at same deterministic path ($EXPECTED_PATH)"

echo "PASS: daemon-pi-session-lifecycle — create/reuse/L1-auto-GC(file-deleted)/reopen-re-mint"
