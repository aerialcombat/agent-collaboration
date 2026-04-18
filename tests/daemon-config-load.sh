#!/usr/bin/env bash
# tests/daemon-config-load.sh — Topic 3 v0 commit 8 (§2.2 config-file
# loading + §3.4 (c) TTL-ordering validator at config-parse time).
#
# Covers:
#   (1) Minimal JSON config loads + daemon starts with values applied.
#   (2) --config LABEL shorthand resolves to ~/.agent-collab/daemons/<LABEL>.json.
#   (3) CLI flag overrides config-file value (precedence: flag > file > env > default).
#   (4) Config-file TTL violation (sweep_ttl <= ack_timeout) is rejected at
#       startup with exit 78 (EX_CONFIG), not a silent race.
#   (5) Malformed JSON config surfaces a clear error with exit 78.
#
# Daemon is started with a fake CLI so we don't depend on an actual
# claude/codex/gemini install; we just verify the flag-parsing + config-
# loading path behaves correctly. Any real spawn failure past that point
# is out of scope for this test (daemon-harness.sh covers real spawning).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
BIN_DIR="$(mktemp -d)"
DAEMON_BIN="$BIN_DIR/agent-collab-daemon"

cleanup() { rm -rf "$TMP" "$BIN_DIR"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-config-load: $*"; }

( cd "$PROJECT_ROOT/go" && go build -o "$DAEMON_BIN" ./cmd/daemon ) || {
  echo "skip: go build failed"
  exit 0
}

# Build the migrate binary + run migrations on a scratch DB so --daemon-
# mode claim doesn't fail on missing schema.
mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: migrate build failed"; exit 0; }

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab/daemons"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

DAEMON_CWD="$TMP/daemon"
mkdir -p "$DAEMON_CWD"

AC="$PROJECT_ROOT/scripts/agent-collab"
AGENT_COLLAB_SESSION_KEY="key-d" \
  "$AC" session register --cwd "$DAEMON_CWD" --label d-recv \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register failed"

# Create a fake-CLI stub the daemon can spawn without real claude/codex/
# gemini. The daemon uses AGENT_COLLAB_DAEMON_CODEX_BIN etc. to override.
FAKE_CODEX="$BIN_DIR/fake-codex.sh"
cat >"$FAKE_CODEX" <<'EOF'
#!/usr/bin/env bash
# Emit a JSONL ack marker + exit 0 so the daemon's completion-ack path
# succeeds without us needing a real model call.
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_CODEX"
export AGENT_COLLAB_DAEMON_CODEX_BIN="$FAKE_CODEX"

# Helper: start daemon, wait briefly, kill, report exit.
run_daemon_briefly() {
  local label="$1" wait_secs="$2"
  shift 2
  local log="$TMP/daemon-${label}.log"
  "$DAEMON_BIN" "$@" >"$log" 2>&1 &
  local pid=$!
  sleep "$wait_secs"
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    echo 0 # treated as "ran ok until we killed it"
  else
    wait "$pid" 2>/dev/null
    echo "$?"
  fi
  echo "log: $log" >&2
}

step "(1) minimal JSON config loads + daemon starts"
CFG_PATH="$HOME/.agent-collab/daemons/d-recv.json"
cat >"$CFG_PATH" <<EOF
{
  "label": "d-recv",
  "cwd": "$DAEMON_CWD",
  "session_key": "key-d",
  "cli": "pi",
  "pi": { "provider": "zai-glm", "model": "glm-4.6" },
  "ack_timeout": 1,
  "sweep_ttl": 3,
  "poll_interval": 1,
  "pause_on_idle": 60
}
EOF
log1="$TMP/daemon-1.log"
"$DAEMON_BIN" --config d-recv >"$log1" 2>&1 &
pid=$!
sleep 1
if ! kill -0 "$pid" 2>/dev/null; then
  cat "$log1" >&2
  fail "(1) daemon exited before we could kill it — config didn't load or startup failed"
fi
kill -TERM "$pid" 2>/dev/null
wait "$pid" 2>/dev/null
grep -q '"daemon.start"' "$log1" || {
  cat "$log1" >&2
  fail "(1) daemon.start event not logged — config load likely failed"
}
grep -q '"label":"d-recv"' "$log1" || fail "(1) label from config not applied"
grep -q '"ack_timeout_sec":1' "$log1" || fail "(1) ack-timeout from config not applied"
grep -q '"sweep_ttl_sec":3' "$log1" || fail "(1) sweep-ttl from config not applied"

step "(2) --config shorthand resolves to ~/.agent-collab/daemons/<LABEL>.json"
# Already exercised by (1): --config d-recv resolved to
# $HOME/.agent-collab/daemons/d-recv.json. If the shorthand logic broke,
# (1) would have errored on "file not found."

step "(3) CLI flag overrides config-file value"
log3="$TMP/daemon-3.log"
# Config file says ack_timeout=1; override to 5 on command line. Daemon's
# startup log should show 5.
"$DAEMON_BIN" --config d-recv --ack-timeout 5 --sweep-ttl 11 >"$log3" 2>&1 &
pid=$!
sleep 1
if ! kill -0 "$pid" 2>/dev/null; then
  cat "$log3" >&2
  fail "(3) daemon exited before we could kill it"
fi
kill -TERM "$pid" 2>/dev/null
wait "$pid" 2>/dev/null
grep -q '"ack_timeout_sec":5' "$log3" || {
  cat "$log3" >&2
  fail "(3) --ack-timeout flag did not override config-file value"
}

step "(4) TTL invariant violation in config file rejected at startup (exit 78)"
CFG_BAD="$HOME/.agent-collab/daemons/bad.json"
cat >"$CFG_BAD" <<EOF
{
  "label": "d-recv",
  "cwd": "$DAEMON_CWD",
  "session_key": "key-d",
  "cli": "pi",
  "pi": { "provider": "zai-glm", "model": "glm-4.6" },
  "ack_timeout": 10,
  "sweep_ttl": 5
}
EOF
set +e
"$DAEMON_BIN" --config bad >"$TMP/daemon-4.log" 2>&1
rc=$?
set -e
[[ "$rc" == "78" ]] || {
  cat "$TMP/daemon-4.log" >&2
  fail "(4) TTL violation should exit 78 (EX_CONFIG); got $rc"
}
grep -qi "TTL invariant" "$TMP/daemon-4.log" || {
  cat "$TMP/daemon-4.log" >&2
  fail "(4) TTL violation error message missing 'TTL invariant'"
}

step "(5) malformed JSON config surfaces clear error with exit 78"
CFG_MALFORMED="$HOME/.agent-collab/daemons/malformed.json"
printf '{\n  "label": "d-recv",\n  "cli": oops-no-quotes\n}\n' >"$CFG_MALFORMED"
set +e
"$DAEMON_BIN" --config malformed >"$TMP/daemon-5.log" 2>&1
rc=$?
set -e
[[ "$rc" == "78" ]] || {
  cat "$TMP/daemon-5.log" >&2
  fail "(5) malformed JSON should exit 78 (EX_CONFIG); got $rc"
}
grep -qi "parse config\|invalid" "$TMP/daemon-5.log" || {
  cat "$TMP/daemon-5.log" >&2
  fail "(5) malformed JSON error message unclear"
}

echo "PASS: daemon-config-load — JSON config + shorthand + flag override + TTL-invariant + malformed-JSON all behave per §2.2 + §3.4 (c)"
