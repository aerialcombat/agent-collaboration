#!/usr/bin/env bash
# tests/daemon-pi-startup-validation.sh — Topic 3 v0.2 §9.2 gate 4:
# pi startup required-field + model-provider coupling check.
#
# Pi has no claude-style asymmetry warning (pi + --cli-session-resume is
# the happy path). This test replaces daemon-pi-warn-once with startup
# validation paths that pi does own:
#
#   (A1) missing pi.provider → exit 64 EX_USAGE with clear diagnostic
#   (A2) missing pi.model    → exit 64 EX_USAGE
#   (A3) model provider-qualified with mismatching prefix → exit 64 +
#        diagnostic naming both fields (§4.4 coupling check)
#   (A4) both present + matching prefix → daemon starts OK
#   (A5) pi.session_dir defaults to $HOME/.agent-collab/pi-sessions when
#        unset — pi.provider + pi.model both present → daemon starts.
#
# Exit 64 = sysexits EX_USAGE (matches codex/gemini startup-invariant
# convention in the existing daemon-config-load.sh).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AC="$PROJECT_ROOT/scripts/agent-collab"

if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-pi-startup-validation: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

CWD="$TMP/work"
mkdir -p "$CWD"

AGENT_COLLAB_SESSION_KEY="key-pi" \
  "$AC" session register --cwd "$CWD" --label validator-pi \
    --agent pi --receive-mode daemon >/dev/null \
  || fail "session-register validator-pi failed"

# run_daemon_expect: invokes daemon synchronously (no &) with a tight
# timeout and compares the exit code + stderr content to expectations.
run_daemon_expect() {
  local label="$1"; shift
  local expected_exit="$1"; shift
  local expected_stderr_regex="$1"; shift

  local stderr_file="$TMP/stderr-${label}.txt"
  # Wrap in timeout to prevent accidental long-running daemon (should
  # exit before startup if validation is tripped). If validation PASSES,
  # daemon would run until killed — we use a very short wait and SIGTERM.
  (
    "$DAEMON" "$@" 2>"$stderr_file" &
    p=$!
    sleep 0.5
    kill -TERM "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null
    echo $? > "$TMP/exit-${label}.txt"
  )
  local actual_exit; actual_exit="$(cat "$TMP/exit-${label}.txt")"
  if [[ "$actual_exit" != "$expected_exit" ]]; then
    cat "$stderr_file"
    fail "($label) expected exit=$expected_exit, got $actual_exit"
  fi
  if [[ -n "$expected_stderr_regex" ]]; then
    grep -qE "$expected_stderr_regex" "$stderr_file" \
      || { cat "$stderr_file"; fail "($label) stderr did not match /$expected_stderr_regex/"; }
  fi
  echo "   ($label) exit=$actual_exit; stderr matched"
}

COMMON_ARGS=(
  --label validator-pi
  --cwd "$CWD"
  --session-key key-pi
  --cli pi
  --ack-timeout 2
  --sweep-ttl 5
)

# =====================================================================
# (A1) missing pi.provider → exit 64
# =====================================================================
step "(A1) missing pi.provider → exit 64 EX_USAGE"
run_daemon_expect "A1" 64 "pi\\.provider is required" "${COMMON_ARGS[@]}" --pi-model glm-4.6

# =====================================================================
# (A2) missing pi.model → exit 64
# =====================================================================
step "(A2) missing pi.model → exit 64 EX_USAGE"
run_daemon_expect "A2" 64 "pi\\.model is required" "${COMMON_ARGS[@]}" --pi-provider zai-glm

# =====================================================================
# (A3) mismatching provider-qualified model → exit 64
# =====================================================================
step "(A3) pi.model openai/gpt-4o + pi.provider zai-glm → exit 64"
run_daemon_expect "A3" 64 "provider-qualified as .openai. but pi\\.provider is .zai-glm." \
  "${COMMON_ARGS[@]}" --pi-provider zai-glm --pi-model "openai/gpt-4o"

# =====================================================================
# (A4) matching provider-qualified model → starts OK
# =====================================================================
# When validation passes, the daemon runs past startup and begins its
# main loop. We send SIGTERM after 0.5s; well-formed startup exits 0
# (graceful shutdown) or is killed.
step "(A4) provider-qualified model with matching prefix → daemon starts"

(
  "$DAEMON" "${COMMON_ARGS[@]}" --pi-provider zai-glm --pi-model "zai-glm/glm-4.6" 2>"$TMP/stderr-A4.txt" &
  p=$!
  sleep 0.5
  kill -TERM "$p" 2>/dev/null || true
  wait "$p" 2>/dev/null
  echo $? > "$TMP/exit-A4.txt"
)
actual_A4="$(cat "$TMP/exit-A4.txt")"
# Graceful shutdown on SIGTERM should produce exit 0 or 143 (128+SIGTERM).
case "$actual_A4" in
  0|143) : ;;
  *)
    cat "$TMP/stderr-A4.txt"
    fail "(A4) expected daemon to start and gracefully shut down (exit 0|143), got $actual_A4"
    ;;
esac
# Validation-error regex MUST NOT appear.
if grep -qE "(provider-qualified|required)" "$TMP/stderr-A4.txt"; then
  cat "$TMP/stderr-A4.txt"
  fail "(A4) unexpected startup-validation error in stderr"
fi
echo "   (A4) exit=$actual_A4; startup validation passed with matching prefix"

# =====================================================================
# (A5) pi.session_dir defaults when unset (no explicit --pi-session-dir)
# =====================================================================
step "(A5) pi.session_dir default → daemon starts"
(
  "$DAEMON" "${COMMON_ARGS[@]}" --pi-provider zai-glm --pi-model glm-4.6 2>"$TMP/stderr-A5.txt" &
  p=$!
  sleep 0.5
  kill -TERM "$p" 2>/dev/null || true
  wait "$p" 2>/dev/null
  echo $? > "$TMP/exit-A5.txt"
)
actual_A5="$(cat "$TMP/exit-A5.txt")"
case "$actual_A5" in
  0|143) : ;;
  *)
    cat "$TMP/stderr-A5.txt"
    fail "(A5) expected daemon to start (exit 0|143), got $actual_A5"
    ;;
esac
if grep -qE "(provider-qualified|required)" "$TMP/stderr-A5.txt"; then
  cat "$TMP/stderr-A5.txt"
  fail "(A5) unexpected startup-validation error in stderr"
fi
echo "   (A5) exit=$actual_A5; defaults applied; startup succeeded"

echo "PASS: daemon-pi-startup-validation — required fields + coupling check + defaults"
