#!/usr/bin/env bash
# tests/daemon-completion-ack.sh — §7.2 completion-ack mechanisms +
# §8.2 stdout-marker false-positive resistance gates.
#
# Covers:
#   (M1) PRIMARY: fake CLI shells out to `peer-inbox daemon-complete`
#        as its final action. Daemon's own DaemonModeComplete returns
#        ErrStaleClaim (benign) and the batch stays completed.
#   (M2) FALLBACK: fake CLI emits a JSONL ack marker `{"peer_inbox_ack":
#        true}` as its final line. Daemon parses the structural fence
#        + calls DaemonModeComplete.
#   (FP) False-positive resistance: fake CLI emits agent-prose that
#        MENTIONS peer-inbox (including the literal string "<peer-
#        inbox-ack>") but without a JSONL structural fence. Daemon
#        must NOT false-positive detect ack from substring / tag-like
#        content. Batch falls through to ack-timeout → sweeper-
#        requeue, proving the structural-fence discipline.
#   (TO) Ack-timeout abandonment: fake CLI sleeps past the ack-
#        timeout without emitting any marker; daemon abandons
#        internally (does NOT call DaemonModeComplete). Sweeper
#        requeue on next run restores the row for re-delivery.

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
step() { echo "-- daemon-completion-ack: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"
PI="$PROJECT_ROOT/go/bin/peer-inbox"

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

# -----------------------------------------------------------------------------
# Fake CLI (M1): calls `peer-inbox daemon-complete` as its final
# action, simulating the agent's tool-call path.
# -----------------------------------------------------------------------------
FAKE_M1="$TMP/fake-m1.sh"
cat > "$FAKE_M1" <<EOF
#!/usr/bin/env bash
echo "agent: got the messages, done"
# Simulate the agent's final tool-call to peer-inbox daemon-complete.
AGENT_COLLAB_INBOX_DB="$DB" "$PI" daemon-complete \\
  --cwd "$DAEMON_CWD" --as daemon-recv --format json >/dev/null 2>&1
exit 0
EOF
chmod +x "$FAKE_M1"

# -----------------------------------------------------------------------------
# Fake CLI (M2): emits a JSONL ack marker as its final line.
# -----------------------------------------------------------------------------
FAKE_M2="$TMP/fake-m2.sh"
cat > "$FAKE_M2" <<'EOF'
#!/usr/bin/env bash
echo "agent: replying to peer"
echo "agent: another thought"
echo '{"peer_inbox_ack": true, "reason": "done"}'
exit 0
EOF
chmod +x "$FAKE_M2"

# -----------------------------------------------------------------------------
# Fake CLI (FP): agent prose that DISCUSSES peer-inbox (including a
# non-JSONL "<peer-inbox-ack>" substring) but no structural fence.
# Daemon must NOT treat this as an ack.
# -----------------------------------------------------------------------------
FAKE_FP="$TMP/fake-fp.sh"
cat > "$FAKE_FP" <<'EOF'
#!/usr/bin/env bash
cat <<PROSE
Let me walk through the peer-inbox protocol. When a batch completes,
the daemon looks for a <peer-inbox-ack> marker. The spec says:
"emit {peer_inbox_ack: true} as the final JSON line". Note that a
bare <peer-inbox> tag in prose is NOT a valid ack — the parser must
fence on JSONL structure.
Also, plain text like peer_inbox_ack: true (without JSON braces)
should not trigger ack either.
PROSE
# Deliberately block until killed by the ack-timeout so the daemon
# has to abandon rather than ack via stdout.
sleep 30
exit 0
EOF
chmod +x "$FAKE_FP"

# -----------------------------------------------------------------------------
# Fake CLI (TO): sleeps past ack-timeout emitting nothing.
# -----------------------------------------------------------------------------
FAKE_TO="$TMP/fake-to.sh"
cat > "$FAKE_TO" <<'EOF'
#!/usr/bin/env bash
sleep 30
exit 0
EOF
chmod +x "$FAKE_TO"

# Common helpers.
reset_inbox() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"DELETE FROM inbox\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-recv'\")
try: c.execute(\"DELETE FROM peer_rooms\")
except sqlite3.OperationalError: pass
c.commit(); c.close()
"
}

seed_one() {
  local tag="$1"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
      --message "ack-probe: $tag" >/dev/null
}

start_daemon_bg() {
  local fake="$1" log="$2" ack_timeout="${3:-5}"
  (
    # v0.3 SOFT SHIM: --cli=codex routes through spawnPi → fake bin
    # binds to PI_BIN; pi-model required per shim preflight.
    export AGENT_COLLAB_DAEMON_PI_BIN="$fake"
    "$DAEMON" \
      --label daemon-recv \
      --cwd "$DAEMON_CWD" \
      --session-key "key-daemon" \
      --cli "codex" \
      --pi-model gpt-5.3-codex \
      --pi-session-dir "$TMP/pi-sessions" \
      --ack-timeout "$ack_timeout" \
      --sweep-ttl $((ack_timeout * 2 + 2)) \
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

# =============================================================================
# (M1) PRIMARY — fake CLI calls peer-inbox daemon-complete itself.
# =============================================================================
step "(M1) PRIMARY: fake CLI calls peer-inbox daemon-complete"
reset_inbox
seed_one "m1"

start_daemon_bg "$FAKE_M1" "$TMP/d-m1.log" 5

# Wait up to 8s for the row to complete.
done_count=0
waited=0
while (( waited < 16 )); do
  done_count="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND completed_at IS NOT NULL AND body='ack-probe: m1'\").fetchone()
print(r[0])
")"
  if [[ "$done_count" == "1" ]]; then break; fi
  sleep 0.5
  waited=$((waited+1))
done
stop_daemon

[[ "$done_count" == "1" ]] \
  || { cat "$TMP/d-m1.log"; fail "M1: row should be completed via daemon-complete from fake CLI"; }

# The daemon's own DaemonModeComplete call should have hit
# ErrStaleClaim (benign — mechanism-1 already completed).
grep -q 'complete_stale\|"daemon.complete"' "$TMP/d-m1.log" \
  || { cat "$TMP/d-m1.log"; fail "M1: daemon log should record complete or complete_stale event"; }
echo "   M1: direct peer-inbox daemon-complete path wired end-to-end"

# =============================================================================
# (M2) FALLBACK — JSONL marker on stdout.
# =============================================================================
step "(M2) FALLBACK: JSONL peer_inbox_ack marker on stdout"
reset_inbox
seed_one "m2"

start_daemon_bg "$FAKE_M2" "$TMP/d-m2.log" 5

done_count=0
waited=0
while (( waited < 16 )); do
  done_count="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND completed_at IS NOT NULL AND body='ack-probe: m2'\").fetchone()
print(r[0])
")"
  if [[ "$done_count" == "1" ]]; then break; fi
  sleep 0.5
  waited=$((waited+1))
done
stop_daemon

[[ "$done_count" == "1" ]] \
  || { cat "$TMP/d-m2.log"; fail "M2: row should be completed via JSONL stdout marker"; }
grep -q 'mechanism2_jsonl_marker' "$TMP/d-m2.log" \
  || { cat "$TMP/d-m2.log"; fail "M2: daemon log should record mechanism2_jsonl_marker reason"; }
echo "   M2: JSONL structural-fence ack parsing wired end-to-end"

# =============================================================================
# (FP) False-positive resistance — prose that mentions peer-inbox
#      without a JSONL fence should NOT trigger ack. The fake CLI
#      also sleeps past ack-timeout so the daemon abandons via TO.
# =============================================================================
step "(FP) stdout-marker false-positive resistance (no ack on agent prose)"
reset_inbox
seed_one "fp"

# Use short ack-timeout so the test finishes quickly.
start_daemon_bg "$FAKE_FP" "$TMP/d-fp.log" 2

# Wait for ack-timeout abandonment to log.
waited=0
while (( waited < 20 )); do
  if grep -q 'ack_timeout_abandoned' "$TMP/d-fp.log" 2>/dev/null; then break; fi
  sleep 0.5
  waited=$((waited+1))
done
stop_daemon

grep -q 'ack_timeout_abandoned' "$TMP/d-fp.log" \
  || { cat "$TMP/d-fp.log"; fail "FP: daemon should have abandoned via ack-timeout (prose is not a valid ack)"; }

# Row must NOT be completed — false-positive would have marked it.
fp_done="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COUNT(*) FROM inbox WHERE to_label='daemon-recv' AND completed_at IS NOT NULL AND body='ack-probe: fp'\").fetchone()
print(r[0])
")"
[[ "$fp_done" == "0" ]] \
  || fail "FP: agent prose containing peer-inbox strings must NOT false-positive ack (completed=$fp_done)"
echo "   FP: agent prose with <peer-inbox-ack> substring correctly rejected; batch abandoned via ack-timeout"

# =============================================================================
# (TO) Ack-timeout abandonment — daemon does NOT call DaemonModeComplete;
#      row stays claimed until sweeper requeues.
# =============================================================================
step "(TO) ack-timeout abandonment → sweeper requeue chain"
reset_inbox
seed_one "to"

start_daemon_bg "$FAKE_TO" "$TMP/d-to.log" 2

# Wait for the ack-timeout abandonment log.
waited=0
while (( waited < 20 )); do
  if grep -q 'ack_timeout_abandoned' "$TMP/d-to.log" 2>/dev/null; then break; fi
  sleep 0.5
  waited=$((waited+1))
done
stop_daemon

grep -q 'ack_timeout_abandoned' "$TMP/d-to.log" \
  || { cat "$TMP/d-to.log"; fail "TO: daemon should have logged ack_timeout_abandoned"; }

# After abandonment, the row is claimed + not completed (daemon did
# NOT call DaemonModeComplete per contract). Verify.
abandoned_state="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT claimed_at IS NOT NULL AS claimed, completed_at IS NOT NULL AS done FROM inbox WHERE body='ack-probe: to'\").fetchone()
print(f'claimed={r[0]}, done={r[1]}')
")"
[[ "$abandoned_state" == "claimed=1, done=0" ]] \
  || fail "TO: abandoned row should stay claimed+uncompleted per §3.4 (c)/(d); got $abandoned_state"

# Run sweeper; TTL is 2s — sleep past it then sweep.
sleep 3
sweep_out="$("$PI" daemon-sweep --sweep-ttl 2 --format json 2>&1)" \
  || fail "sweeper failed: $sweep_out"
reaped="$(printf '%s' "$sweep_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["reaped"]))')"
[[ "$reaped" -ge "1" ]] \
  || fail "TO: sweeper should have reaped the abandoned claim (reaped=$reaped)"
echo "   TO: ack-timeout → abandon → sweeper reclaims (reaped=$reaped row(s))"

echo "PASS: daemon-completion-ack — M1 direct-complete + M2 JSONL-marker + FP false-positive resistance + TO ack-timeout abandon"
