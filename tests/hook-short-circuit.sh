#!/usr/bin/env bash
# tests/hook-short-circuit.sh — Topic 3 v0 §3.4 (f) daemon-spawn hook
# short-circuit gate.
#
# Contract: when AGENT_COLLAB_DAEMON_SPAWN=1 is set in the environment,
# both hook entry points (hooks/peer-inbox-inject.sh + Go
# peer-inbox-hook) MUST exit 0 with no stdout output and MUST NOT touch
# the inbox (no read_at update, no DB write of any kind). Scope doc
# §8.2 gate "Daemon-spawn hook short-circuit":
#
#   Invoke Go hook binary AND hooks/peer-inbox-inject.sh with
#   AGENT_COLLAB_DAEMON_SPAWN=1 set. Assert: exit 0, no stdout output,
#   no read_at updates on inbox rows (no DB write at all). Also assert
#   non-daemon invocation (env unset) behaves identically to today —
#   captures the additive-only-no-hot-path-impact contract.
#
# Rationale (§2.4 + §3.4 (f)): the daemon (W3, commit 7) spawns CLIs
# with AGENT_COLLAB_DAEMON_SPAWN=1 and injects the peer-inbox envelope
# directly via prompt text. Without this short-circuit the hook would
# also run its interactive ReadUnread path; the §3.4 (a) SQL partition
# (commit c96868f) prevents double-consumption as a safety net, but
# the hook still pays a wasted DB round-trip per daemon-spawn turn.
# The env-flag short-circuit makes daemon spawns a pure no-op.
#
# Test matrix:
#   (1) Go binary + AGENT_COLLAB_DAEMON_SPAWN=1 → exit 0, no stdout, no read_at writes
#   (2) Bash hook + AGENT_COLLAB_DAEMON_SPAWN=1 → exit 0, no stdout, no read_at writes
#   (3) Go binary WITHOUT the env    → rows delivered + read_at populated (baseline)
#   (4) Bash hook WITHOUT the env    → rows delivered + read_at populated (baseline)
#
# Isolates on a temp HOME + temp DB so it doesn't perturb live peer-inbox state.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AC="$PROJECT_ROOT/scripts/agent-collab"
HOOK="$PROJECT_ROOT/hooks/peer-inbox-inject.sh"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH (need peer-inbox-hook binary)"
  exit 0
fi

[[ -x "$AC" ]]   || { echo "FAIL: agent-collab dispatcher missing or not executable: $AC" >&2; exit 1; }
[[ -r "$HOOK" ]] || { echo "FAIL: hook script missing: $HOOK" >&2; exit 1; }

TMP="$(mktemp -d)"
BIN_DIR="$(mktemp -d)"
BIN="$BIN_DIR/peer-inbox-hook"

cleanup() { rm -rf "$TMP" "$BIN_DIR"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- hook-short-circuit: $*"; }

( cd "$PROJECT_ROOT/go" && go build -o "$BIN" ./cmd/hook ) || {
  echo "skip: go build (hook) failed"
  exit 0
}

# peer-inbox-migrate is needed for first-open schema application by the
# Python CLI (session register / peer send go through agent-collab). Build
# into the canonical repo-relative location the Python loader checks.
mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) || {
  echo "skip: go build (migrate) failed"
  exit 0
}

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
# Put the just-built Go hook binary first on PATH so the bash hook's
# `command -v peer-inbox-hook` resolves to it.
export PATH="$BIN_DIR:$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
RECV_CWD="$TMP/recv"
SEND_CWD="$TMP/send"
mkdir -p "$RECV_CWD" "$SEND_CWD"

export AGENT_COLLAB_INBOX_DB="$DB"

# Helper: count unread (read_at IS NULL) rows to the recv label.
unread_count() {
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute('SELECT COUNT(*) FROM inbox WHERE to_label = ? AND read_at IS NULL', ('recv',)).fetchone()[0]
print(r)
"
}

# Helper: seed N unread rows to the recv label and report the new unread count.
seed_rows() {
  local n="$1" tag="$2"
  for i in $(seq 1 "$n"); do
    AGENT_COLLAB_SESSION_KEY="key-send" \
      "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$RECV_CWD" \
        --message "short-circuit probe ($tag) $i" >/dev/null \
      || fail "peer send failed ($tag iter $i)"
  done
}

step "register recv + send"
AGENT_COLLAB_SESSION_KEY="key-recv" \
  "$AC" session register --cwd "$RECV_CWD" --label recv --agent claude >/dev/null \
  || fail "session register recv failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label send --agent claude >/dev/null \
  || fail "session register send failed"

# =============================================================================
# (1) Go binary + AGENT_COLLAB_DAEMON_SPAWN=1 → exit 0, no stdout, no writes
# =============================================================================
step "(1) Go binary WITH AGENT_COLLAB_DAEMON_SPAWN=1: exit 0, no stdout, no DB writes"
seed_rows 3 "go-daemon-spawn"
before_count="$(unread_count)"
[[ "$before_count" == "3" ]] || fail "precondition: expected 3 unread rows, got $before_count"

set +e
go_daemon_out="$(
  AGENT_COLLAB_SESSION_KEY="key-recv" \
  AGENT_COLLAB_DAEMON_SPAWN=1 \
    "$BIN" "$RECV_CWD" 2>/dev/null
)"
go_daemon_exit=$?
set -e
echo "   exit=$go_daemon_exit stdout_bytes=${#go_daemon_out}"
[[ "$go_daemon_exit" == "0" ]] \
  || fail "Go binary with AGENT_COLLAB_DAEMON_SPAWN=1 should exit 0, got $go_daemon_exit"
[[ -z "$go_daemon_out" ]] \
  || fail "Go binary with AGENT_COLLAB_DAEMON_SPAWN=1 should emit no stdout, got: $go_daemon_out"

after_count="$(unread_count)"
[[ "$after_count" == "3" ]] \
  || fail "Go daemon-spawn short-circuit should not mutate inbox; unread went 3 → $after_count"

# =============================================================================
# (2) Bash hook + AGENT_COLLAB_DAEMON_SPAWN=1 → exit 0, no stdout, no writes
# =============================================================================
step "(2) Bash hook WITH AGENT_COLLAB_DAEMON_SPAWN=1: exit 0, no stdout, no DB writes"
bash_stdin='{"session_id":"key-recv","cwd":"'"$RECV_CWD"'","hook_event_name":"UserPromptSubmit"}'

set +e
bash_daemon_out="$(
  AGENT_COLLAB_SESSION_KEY="key-recv" \
  AGENT_COLLAB_DAEMON_SPAWN=1 \
    bash "$HOOK" <<<"$bash_stdin" 2>/dev/null
)"
bash_daemon_exit=$?
set -e
echo "   exit=$bash_daemon_exit stdout_bytes=${#bash_daemon_out}"
[[ "$bash_daemon_exit" == "0" ]] \
  || fail "bash hook with AGENT_COLLAB_DAEMON_SPAWN=1 should exit 0, got $bash_daemon_exit"
[[ -z "$bash_daemon_out" ]] \
  || fail "bash hook with AGENT_COLLAB_DAEMON_SPAWN=1 should emit no stdout, got: $bash_daemon_out"

after_count2="$(unread_count)"
[[ "$after_count2" == "3" ]] \
  || fail "bash daemon-spawn short-circuit should not mutate inbox; unread went 3 → $after_count2"

# The bash hook's early-exit must fire BEFORE the log-session side-effect
# (which lives after the shebang / log func / python3 guards). Verify the
# session-log directory was never created — if the early-exit is positioned
# wrong and falls through to `agent-collab hook log-session`, this dir would
# have been populated with a <cwd-hash>.log entry.
log_dir="$HOME/.agent-collab/claude-sessions-seen"
if [[ -d "$log_dir" ]]; then
  log_entries="$(ls -A "$log_dir" 2>/dev/null)"
  [[ -z "$log_entries" ]] \
    || fail "short-circuit fired too late: claude-sessions-seen was populated ($log_entries); early-exit must precede log-session call"
fi

# =============================================================================
# (3) Go binary WITHOUT the env → rows delivered + read_at populated (baseline)
# =============================================================================
step "(3) Go binary WITHOUT env: normal delivery path unchanged (additive contract)"
# The three seeded rows from step (1) + (2) are still unread. Invoke the
# Go hook directly as the production hot path would. The unread count
# should drop to 0 because MarkRead-on-Read consumes them.
set +e
go_normal_out="$(
  AGENT_COLLAB_SESSION_KEY="key-recv" \
    "$BIN" "$RECV_CWD" 2>/dev/null
)"
go_normal_exit=$?
set -e
echo "   exit=$go_normal_exit stdout_bytes=${#go_normal_out}"
[[ "$go_normal_exit" == "0" ]] \
  || fail "baseline broken: Go binary without env should exit 0, got $go_normal_exit"
[[ -n "$go_normal_out" ]] \
  || fail "baseline broken: Go binary without env should emit envelope for 3 unread rows"
printf '%s' "$go_normal_out" | grep -q "short-circuit probe" \
  || fail "baseline broken: Go binary envelope should contain the seeded probe body"

after_count3="$(unread_count)"
[[ "$after_count3" == "0" ]] \
  || fail "baseline broken: Go binary should mark rows read (expected 0 unread, got $after_count3)"

# =============================================================================
# (4) Bash hook WITHOUT the env → rows delivered + read_at populated (baseline)
# =============================================================================
step "(4) Bash hook WITHOUT env: normal delivery path unchanged (additive contract)"
# Seed fresh rows so the bash hook has something to deliver.
seed_rows 2 "bash-normal"
before_count4="$(unread_count)"
[[ "$before_count4" == "2" ]] || fail "precondition: expected 2 unread rows, got $before_count4"

# Bump the inbox-dirty marker's mtime past any prior hook-ack so the
# fast-path check doesn't short-circuit. peer send already touches the
# marker, but the Go run in step (3) also wrote an ack file, so nudge
# the marker forward to be safe.
sleep 1
touch "$HOME/.agent-collab/inbox-dirty"

set +e
bash_normal_out="$(
  AGENT_COLLAB_SESSION_KEY="key-recv" \
    bash "$HOOK" <<<"$bash_stdin" 2>/dev/null
)"
bash_normal_exit=$?
set -e
echo "   exit=$bash_normal_exit stdout_bytes=${#bash_normal_out}"
[[ "$bash_normal_exit" == "0" ]] \
  || fail "baseline broken: bash hook without env should exit 0, got $bash_normal_exit"
[[ -n "$bash_normal_out" ]] \
  || fail "baseline broken: bash hook without env should emit envelope for 2 unread rows"
printf '%s' "$bash_normal_out" | grep -q "short-circuit probe" \
  || fail "baseline broken: bash hook envelope should contain the seeded probe body"

after_count4="$(unread_count)"
[[ "$after_count4" == "0" ]] \
  || fail "baseline broken: bash hook should mark rows read (expected 0 unread, got $after_count4)"

echo "PASS: §3.4 (f) daemon-spawn short-circuit: Go + bash hooks both no-op under AGENT_COLLAB_DAEMON_SPAWN=1; unchanged baseline delivery when unset"
