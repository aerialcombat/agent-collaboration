#!/usr/bin/env bash
# tests/daemon-cli-resume-claude-warn-once.sh — Topic 3 v0.1.1 gap-fill.
#
# Locks the cardinality half of the §3.4 invariant 4 contract. Scope-doc
# v3 §4.3 and the emit site at go/cmd/daemon/main.go:248 both say:
#
#   "Warn at startup so operators see it once, not per-spawn."
#
# Gate 6a in tests/daemon-cli-resume-claude-asymmetry.sh asserts the
# warning string APPEARS once a daemon has started. It does not assert
# the warning appears EXACTLY ONCE regardless of spawn count — a
# refactor that accidentally moved the Warn() call into the per-spawn
# construction path (e.g., into the claude helper's arg-builder) would
# leave 6a green while emitting one warn per batch. Operators tailing
# stderr would see log spam + noisy alert channels.
#
# This test drives multiple spawns through a single daemon lifecycle and
# asserts the warning appears EXACTLY ONCE in the captured stderr. It
# also asserts the JSON-log event `daemon.cli_session_resume.claude_
# asymmetry` appears exactly once in the log file (belt-and-suspenders:
# stderr is for operators, the JSONL log is for monitoring pipelines).
#
# Pattern mirrors tests/daemon-cli-resume-claude-asymmetry.sh — same
# fake claude, same daemon invocation, same settings-file dance. The
# delta is: we seed N messages (not 1) and grep -c on the warning.

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
cleanup() {
  if [[ -f "$TMP/daemon.pid" ]]; then
    local p; p="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  fi
  rm -rf "$TMP"
}
if [[ -z "${KEEP_TMP:-}" ]]; then
  trap cleanup EXIT
else
  trap 'echo "KEEP_TMP set: preserving TMP=$TMP"' EXIT
fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-cli-resume-claude-warn-once: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab" "$HOME/.claude"
echo '{}' > "$HOME/.claude/settings.json"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

# Tight TTLs, preserve 2× ratio (§3.4 (c)).
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

DAEMON_CWD="$TMP/daemon-claude"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

EXPECTED_WARNING='Claude has no cross-process session-resume; --cli-session-resume is a no-op for this daemon (see Arch B asymmetry note in operator guide).'
EXPECTED_LOG_EVENT='daemon.cli_session_resume.claude_asymmetry'

# Number of messages to drive through the daemon. Each is its own batch
# under the tight TTLs — gives at least N spawns in a single daemon
# lifecycle. Chosen small enough that the test runs in <15s.
N_SPAWNS=3

step "register daemon claude recipient + interactive sender"
AGENT_COLLAB_SESSION_KEY="key-claude" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-claude \
    --agent claude --receive-mode daemon >/dev/null \
  || fail "session-register daemon-claude failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"

# Fake claude: bump a spawn counter, emit JSONL ack, exit.
FAKE_CLAUDE="$TMP/fake-claude.sh"
SPAWN_COUNTER="$TMP/spawn-count.txt"
echo "0" > "$SPAWN_COUNTER"

cat > "$FAKE_CLAUDE" <<EOF
#!/usr/bin/env bash
set -u
COUNTER="$SPAWN_COUNTER"
n=\$(cat "\$COUNTER" 2>/dev/null || echo 0)
echo \$((n+1)) > "\$COUNTER"
( read -t 1 _ ) 2>/dev/null || true
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_CLAUDE"

step "start daemon with --cli claude --cli-session-resume; capture stderr + log"
STDERR_PATH="$TMP/daemon.stderr"
LOG_PATH="$TMP/daemon.log"

(
  export AGENT_COLLAB_DAEMON_CLAUDE_BIN="$FAKE_CLAUDE"
  "$DAEMON" \
    --label daemon-claude \
    --cwd "$DAEMON_CWD" \
    --session-key "key-claude" \
    --cli claude \
    --claude-settings "$HOME/.claude/settings.json" \
    --cli-session-resume \
    --ack-timeout 2 \
    --sweep-ttl 5 \
    --poll-interval 1 \
    --log-path "$LOG_PATH" \
    >/dev/null 2>"$STDERR_PATH" &
  echo $! > "$TMP/daemon.pid"
)
DAEMON_PID="$(cat "$TMP/daemon.pid")"

# Wait briefly for the daemon startup + warn emission before sending the
# first message. Otherwise the send could race the daemon-startup log.
sleep 0.8

step "drip-send $N_SPAWNS messages one at a time; each triggers its own batch + spawn"
for i in $(seq 1 "$N_SPAWNS"); do
  pre_count="$(cat "$SPAWN_COUNTER" 2>/dev/null || echo 0)"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to daemon-claude \
      --to-cwd "$DAEMON_CWD" \
      --message "warn-once probe $i" >/dev/null \
    || fail "send $i failed"
  # Wait for this message's spawn to increment the counter.
  waited=0
  while (( waited < 30 )); do
    n="$(cat "$SPAWN_COUNTER" 2>/dev/null || echo 0)"
    if (( n > pre_count )); then break; fi
    sleep 0.5
    waited=$((waited+1))
  done
  final_n="$(cat "$SPAWN_COUNTER" 2>/dev/null || echo 0)"
  if (( final_n <= pre_count )); then
    echo "--- stderr ---"; cat "$STDERR_PATH" 2>/dev/null || true
    echo "--- log ---";    cat "$LOG_PATH"    2>/dev/null || true
    fail "spawn $i never happened (counter: pre=$pre_count post=$final_n)"
  fi
done

# Small settle for any final log flush.
sleep 0.5

kill -TERM "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true

final_spawn_count="$(cat "$SPAWN_COUNTER" 2>/dev/null || echo 0)"
if (( final_spawn_count < N_SPAWNS )); then
  echo "--- stderr ---"; cat "$STDERR_PATH" 2>/dev/null || true
  echo "--- log ---";    cat "$LOG_PATH"    2>/dev/null || true
  fail "fake claude only spawned $final_spawn_count times (needed $N_SPAWNS)"
fi

step "assertion: warning appears EXACTLY ONCE in stderr regardless of spawn count ($final_spawn_count spawns)"
stderr_warn_count="$(grep -cF "$EXPECTED_WARNING" "$STDERR_PATH" || true)"
if [[ "$stderr_warn_count" != "1" ]]; then
  echo "--- stderr (full) ---"
  cat "$STDERR_PATH"
  fail "expected EXACTLY 1 warn-line in stderr, got $stderr_warn_count (over $final_spawn_count spawns)"
fi

step "assertion: log event '$EXPECTED_LOG_EVENT' appears EXACTLY ONCE"
log_event_count="$(grep -cF "$EXPECTED_LOG_EVENT" "$LOG_PATH" || true)"
if [[ "$log_event_count" != "1" ]]; then
  echo "--- log (full) ---"
  cat "$LOG_PATH"
  fail "expected EXACTLY 1 claude_asymmetry log event, got $log_event_count (over $final_spawn_count spawns)"
fi

step "assertion: daemon_cli_session_id stayed NULL for claude label (column-no-write cross-check)"
col="$(python3 - "$DB" daemon-claude <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
r = c.execute("SELECT daemon_cli_session_id FROM sessions WHERE label = ?", (label,)).fetchone()
c.close()
print("" if (r is None or r[0] is None) else r[0])
PY
)"
if [[ -n "$col" ]]; then
  fail "expected daemon_cli_session_id NULL for claude label after $final_spawn_count spawns, got '$col'"
fi

echo "PASS: claude-asymmetry warn-once — 1 stderr warn + 1 log event across $final_spawn_count spawns; column stayed NULL"
