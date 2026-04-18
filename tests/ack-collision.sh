#!/usr/bin/env bash
# tests/ack-collision.sh — alpha A2: UserPromptSubmit hook's per-cwd ack
# file collides across labels-in-same-cwd and silently starves all but the
# first-invoked session.
#
# Setup: two Claude sessions (labels alpha + beta) registered in one shared
# cwd — the common case for peer-inbox mediated rooms. A third session
# (sender, separate cwd) sends one targeted message to each.
#
# Current ack key: sha256(cwd)[:16] (hooks/peer-inbox-inject.sh:73-76). When
# alpha's hook fetches + mark-reads + touches the ack, the ack mtime
# advances past the marker. Beta's next hook in the same cwd hits the
# "ack_file -nt marker" short-circuit at :87 and exits before reading her
# own unread.
#
# This test:
#   1. registers alpha + beta in shared cwd, sender in its own cwd
#   2. sends one targeted message per recipient
#   3. invokes the hook as alpha (should deliver alpha's message)
#   4. invokes the hook as beta (currently silenced by ack short-circuit)
#   5. asserts BOTH labels delivered their targeted message
#
# Expected: FAILS on HEAD (ack-file collision reproduces). PASSES once the
# ack key incorporates resolved self-label (e.g. <cwd_hash>.<label>).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"

export PATH="$PROJECT_ROOT/scripts:$PATH"
# Force Python path so this test isolates the bash-level ack logic,
# independent of whether the Go binary is built. Same ack bug either way.
export AGENT_COLLAB_FORCE_PY=1

cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

DB="$TMP/sessions.db"
SHARED_CWD="$TMP/shared"
SENDER_CWD="$TMP/sender"
mkdir -p "$SHARED_CWD" "$SENDER_CWD"

AC="$PROJECT_ROOT/scripts/agent-collab"
HOOK="$PROJECT_ROOT/hooks/peer-inbox-inject.sh"

[[ -x "$AC" ]]   || fail "agent-collab dispatcher missing or not executable: $AC"
[[ -r "$HOOK" ]] || fail "hook script missing: $HOOK"
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

echo "-- ack-collision: register alpha + beta in shared cwd, sender in own cwd --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-alpha" \
  "$AC" session register --cwd "$SHARED_CWD" --label alpha --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-beta" \
  "$AC" session register --cwd "$SHARED_CWD" --label beta --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-sender" \
  "$AC" session register --cwd "$SENDER_CWD" --label sender --agent claude >/dev/null

echo "-- ack-collision: sender targets one message per recipient --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-sender" \
  "$AC" peer send --cwd "$SENDER_CWD" --to alpha --to-cwd "$SHARED_CWD" \
    --message "ack-collision probe: for ALPHA" >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-sender" \
  "$AC" peer send --cwd "$SENDER_CWD" --to beta --to-cwd "$SHARED_CWD" \
    --message "ack-collision probe: for BETA" >/dev/null

[[ -f "$HOME/.agent-collab/inbox-dirty" ]] \
  || fail "inbox-dirty marker was not touched by peer-send — test precondition broken"

# Ensure mtime ordering is unambiguous on filesystems with second-granularity.
sleep 1

echo "-- ack-collision: invoke hook as alpha --"
alpha_stdin='{"session_id":"key-alpha","cwd":"'"$SHARED_CWD"'"}'
alpha_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-alpha" \
    bash "$HOOK" <<<"$alpha_stdin" 2>/dev/null
)"
echo "alpha hook stdout: ${alpha_out:-<empty>}"
printf '%s' "$alpha_out" | grep -q "ack-collision probe: for ALPHA" \
  || fail "alpha's hook did not deliver alpha's own message (baseline broken, not the collision)"

echo "-- ack-collision: invoke hook as beta in SAME cwd --"
beta_stdin='{"session_id":"key-beta","cwd":"'"$SHARED_CWD"'"}'
beta_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-beta" \
    bash "$HOOK" <<<"$beta_stdin" 2>/dev/null
)"
echo "beta hook stdout: ${beta_out:-<empty>}"

ack_dir="$HOME/.agent-collab/hook-ack"
echo "-- ack-collision: ack directory state post-run --"
ls -la "$ack_dir" 2>/dev/null || echo "(ack dir empty)"

if ! printf '%s' "$beta_out" | grep -q "ack-collision probe: for BETA"; then
  fail "ack-file collision REPRODUCED: beta's hook was starved by alpha's ack in shared cwd (alpha A2)"
fi

echo "PASS: both labels delivered their targeted message via hook in shared cwd"
