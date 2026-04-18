#!/usr/bin/env bash
# tests/daemon-termination.sh — §8.2 L1/L2/L3 termination-stack gates
# for go/cmd/daemon.
#
# Covers:
#   (L1) Content-stop sentinel — daemon completes the current batch
#        then sets sessions.daemon_state='closed' + stops claiming.
#   (L2) Claim-time daemon_state='closed' preflight (§3.4 (e)) — when
#        daemon_state is externally flipped to 'closed', next claim
#        returns empty with no inbox mutation.
#   (L3a) Empty-response terminates — when the spawned CLI emits
#         empty stdout, daemon marks the batch complete anyway (no
#         respawn, no sweeper-requeue loop).
#   (L3b) Pause-on-idle — after --pause-on-idle <short> seconds of
#         no activity, daemon reduces poll frequency (log emits a
#         Warn event) but stays alive.
#
# Uses fake CLIs via AGENT_COLLAB_DAEMON_<CLI>_BIN overrides so the
# test is self-contained. TTLs kept minimal (ack=1, sweep=2) to bound
# runtime while preserving the 2× invariant.

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
step() { echo "-- daemon-termination: $*"; }

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

# Fake CLI that emits a JSONL ack marker + keeps a counter of how
# many times it's been spawned. Used to assert L3-empty vs L1
# behavior.
FAKE_ACK="$TMP/fake-ack.sh"
cat > "$FAKE_ACK" <<'EOF'
#!/usr/bin/env bash
# Ack-emitting fake: increments a counter file on each spawn, emits
# JSONL ack, exits 0.
COUNTER="${FAKE_CLI_COUNTER:-/dev/null}"
if [[ "$COUNTER" != "/dev/null" ]]; then
  local_n="$(cat "$COUNTER" 2>/dev/null || echo 0)"
  echo "$((local_n + 1))" > "$COUNTER"
fi
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_ACK"

# Fake CLI that emits ONLY empty stdout.
FAKE_EMPTY="$TMP/fake-empty.sh"
cat > "$FAKE_EMPTY" <<'EOF'
#!/usr/bin/env bash
COUNTER="${FAKE_CLI_COUNTER:-/dev/null}"
if [[ "$COUNTER" != "/dev/null" ]]; then
  local_n="$(cat "$COUNTER" 2>/dev/null || echo 0)"
  echo "$((local_n + 1))" > "$COUNTER"
fi
# Emit whitespace only — daemon should treat as "nothing to add".
printf '   \n\n'
exit 0
EOF
chmod +x "$FAKE_EMPTY"

# Helper: run the daemon in the background for N seconds then kill.
# Stdin passed as nil; stderr + stdout combined to the log path.
start_daemon_bg() {
  local cli="$1" fake="$2" log="$3" pause="${4:-1800}"
  # v0.3 SOFT SHIM: codex + gemini route through spawnPi, so the fake
  # binary binds to PI_BIN for those paths. Claude unchanged.
  local bin_env_key
  local extra_args_str=""
  case "$cli" in
    claude) bin_env_key="AGENT_COLLAB_DAEMON_CLAUDE_BIN" ;;
    codex)
      bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN"
      extra_args_str="--pi-model gpt-5.3-codex --pi-session-dir $TMP/pi-sessions"
      ;;
    gemini)
      bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN"
      extra_args_str="--pi-model gemini-3-flash --pi-session-dir $TMP/pi-sessions"
      ;;
    pi)
      bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN"
      extra_args_str="--pi-provider zai-glm --pi-model glm-4.6 --pi-session-dir $TMP/pi-sessions"
      ;;
  esac
  (
    export "$bin_env_key=$fake"
    export FAKE_CLI_COUNTER="$TMP/fake-counter.txt"
    echo 0 > "$FAKE_CLI_COUNTER"
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label daemon-recv \
      --cwd "$DAEMON_CWD" \
      --session-key "key-daemon" \
      --cli "$cli" \
      --ack-timeout 5 \
      --sweep-ttl 10 \
      --poll-interval 1 \
      --pause-on-idle "$pause" \
      --log-path "$log" \
      $extra_args_str \
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
  # Reset inbox rows, daemon_state back to 'open', AND any room
  # termination records so subsequent peer send calls don't hit the
  # room-terminated guard in scripts/peer-inbox-db.py (which the L1
  # sentinel test triggers legitimately but that bleeds into later
  # tests). rooms table is optional (may not exist on fresh DB).
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"DELETE FROM inbox\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-recv'\")
try:
    c.execute(\"DELETE FROM peer_rooms\")
except sqlite3.OperationalError:
    pass
c.commit(); c.close()
"
}

# =============================================================================
# (L1) Content-stop sentinel — daemon completes batch then transitions
#      daemon_state='closed'. Next claim-time preflight returns empty
#      (validated by a second row pre-seeded directly into the DB
#      after daemon has transitioned, since peer send also terminates
#      the room on the sender side — we bypass the send path by
#      writing the post-close row via direct SQL).
# =============================================================================
step "(L1) content-stop sentinel triggers daemon_state='closed' post-batch"
reset_inbox

# Build the sentinel from parts so the test file itself doesn't
# contain the literal token (avoids false triggers under source-
# scanning tools). Token is the same one _terminate_room /
# TERMINATION_TOKEN in scripts/peer-inbox-db.py recognizes.
SENTINEL="$(printf '%s' '[' '[' 'end' ']' ']')"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "closing thoughts $SENTINEL" >/dev/null

start_daemon_bg "codex" "$FAKE_ACK" "$TMP/d-l1.log"

# Wait up to 10s for the daemon to spawn + transition closed.
state=""
waited=0
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

# Write a post-close probe row directly into the DB (bypasses peer
# send's room-terminated guard). The daemon should NOT claim it
# because daemon_state='closed' per §3.4 (e).
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"\"\"
  INSERT INTO inbox (to_cwd, to_label, from_cwd, from_label, body, created_at)
  VALUES (?, 'daemon-recv', ?, 'sender', 'post-close probe', datetime('now'))
\"\"\", ('$DAEMON_CWD', '$SEND_CWD'))
c.commit(); c.close()
"
sleep 2

# The post-close row should still be unclaimed (closed daemon
# preflight blocks the claim per §3.4 (e)).
unclaimed="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND claimed_at IS NULL AND body='post-close probe'\").fetchone()
print(r[0])
")"
stop_daemon

[[ "$state" == "closed" ]] \
  || { echo "--- daemon log ---"; cat "$TMP/d-l1.log"; fail "L1: daemon_state never transitioned to 'closed' (final=$state)"; }
[[ "$unclaimed" == "1" ]] \
  || fail "L1: post-close row should remain unclaimed, got $unclaimed"

# Verify the sentinel-carrying row was completed (drain-then-dormant
# per §6 Layer 1 wording).
completed="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND completed_at IS NOT NULL AND body LIKE 'closing thoughts%'\").fetchone()
print(r[0])
")"
[[ "$completed" == "1" ]] \
  || fail "L1: sentinel-carrying batch should be drained-then-dormant (completed=1), got $completed"
echo "   L1: sentinel batch completed, daemon_state='closed', post-close row untouched"

# =============================================================================
# (L2) Claim-time daemon_state='closed' preflight — seed rows with
#      daemon_state pre-set to 'closed'; daemon should not claim.
#      This is a pure claim-layer test (§3.4 (e)); the underlying
#      behavior is exercised by tests/daemon-mode-lifecycle-go.sh
#      already. Here we verify the DAEMON (not just the CLI verb)
#      honors the preflight.
# =============================================================================
step "(L2) externally-closed daemon_state blocks claims at DaemonModeClaim"
reset_inbox
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'closed' WHERE label = 'daemon-recv'\")
c.commit(); c.close()
"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "L2 probe" >/dev/null

start_daemon_bg "codex" "$FAKE_ACK" "$TMP/d-l2.log"
sleep 3
stop_daemon

unclaimed_l2="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND claimed_at IS NULL AND body='L2 probe'\").fetchone()
print(r[0])
")"
[[ "$unclaimed_l2" == "1" ]] \
  || fail "L2: row should remain unclaimed under daemon_state='closed'; got $unclaimed_l2"
counter_l2="$(cat "$TMP/fake-counter.txt" 2>/dev/null || echo 0)"
[[ "$counter_l2" == "0" ]] \
  || fail "L2: fake CLI should never spawn under closed daemon; got count=$counter_l2"
echo "   L2: closed daemon_state held firm; no claims, no spawns"

# =============================================================================
# (L3a) Empty-response terminates — fake CLI emits only whitespace;
#       daemon marks batch complete without respawning.
# =============================================================================
step "(L3a) empty-response terminates without respawn"
reset_inbox
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "L3a probe" >/dev/null

start_daemon_bg "codex" "$FAKE_EMPTY" "$TMP/d-l3a.log"

# Wait up to 6s for the row to be completed.
waited=0
while (( waited < 12 )); do
  done_count="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND completed_at IS NOT NULL AND body='L3a probe'\").fetchone()
print(r[0])
")"
  if [[ "$done_count" == "1" ]]; then break; fi
  sleep 0.5
  waited=$((waited+1))
done

# Give a beat for a potential spurious respawn, then stop.
sleep 2
counter_l3a="$(cat "$TMP/fake-counter.txt" 2>/dev/null || echo 0)"
stop_daemon

[[ "$done_count" == "1" ]] \
  || { cat "$TMP/d-l3a.log"; fail "L3a: empty-response row was not marked completed"; }
# Counter should be 1 (single spawn, no respawn loop).
[[ "$counter_l3a" == "1" ]] \
  || fail "L3a: expected exactly 1 spawn, got $counter_l3a (respawn loop detected)"

# The log should contain the layer3_empty_response event.
grep -q 'layer3_empty_response' "$TMP/d-l3a.log" \
  || { cat "$TMP/d-l3a.log"; fail "L3a: daemon did not log layer3_empty_response event"; }
echo "   L3a: empty-response → single spawn, row completed, no respawn loop"

# =============================================================================
# (L3b) Pause-on-idle — short pause-on-idle window; after idle, the
#       daemon logs a pause_on_idle event. Daemon stays alive (no
#       exit), just slows poll.
# =============================================================================
step "(L3b) pause-on-idle emits warning after idle window"
reset_inbox
# Use --pause-on-idle 2 so the test observes the transition quickly.
start_daemon_bg "codex" "$FAKE_ACK" "$TMP/d-l3b.log" 2

# Wait up to 8s for the pause_on_idle log event.
waited=0
observed=0
while (( waited < 16 )); do
  if grep -q 'pause_on_idle' "$TMP/d-l3b.log" 2>/dev/null; then
    observed=1
    break
  fi
  sleep 0.5
  waited=$((waited+1))
done

# Check daemon is still alive (didn't exit).
pid_check="$(cat "$TMP/daemon.pid")"
if ! kill -0 "$pid_check" 2>/dev/null; then
  cat "$TMP/d-l3b.log"
  fail "L3b: daemon exited unexpectedly during pause-on-idle test"
fi

stop_daemon

[[ "$observed" == "1" ]] \
  || { cat "$TMP/d-l3b.log"; fail "L3b: pause_on_idle event not observed in $waited * 0.5s window"; }
echo "   L3b: pause_on_idle event logged, daemon stayed alive"

echo "PASS: daemon-termination — L1 content-stop / L2 closed-state / L3a empty-response / L3b pause-on-idle"
