#!/usr/bin/env bash
# peer-inbox-inject.sh — Claude UserPromptSubmit hook.
#
# Two jobs:
#   1. Log the Claude session_id seen in this cwd to
#      ~/.agent-collab/claude-sessions-seen/<cwd-hash>.log so that
#      `agent-collab session register` can later adopt it — needed when the
#      runtime (Claude Code 2.1.78) doesn't export $CLAUDE_SESSION_ID to
#      Bash-tool subprocesses.
#   2. Fetch unread peer messages as hookSpecificOutput.additionalContext
#      JSON and emit to stdout.
#
# v3.0 — hot path is the Go `peer-inbox-hook` binary when available. We
# short-circuit via an `~/.agent-collab/inbox-dirty` mtime marker so an
# empty inbox exits without any database work. Fall back to the Python
# path whenever the Go binary is missing or `AGENT_COLLAB_FORCE_PY=1`.
#
# Fail-open on every error path: the user's turn must never be blocked.

set -uo pipefail
LOG="${AGENT_COLLAB_HOOK_LOG:-$HOME/.agent-collab/hook.log}"
log() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
  printf '[%s] peer-inbox-inject: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" \
    >>"$LOG" 2>/dev/null || true
}
trap 'rc=$?; [[ $rc -ne 0 ]] && log "failed at line $LINENO exit $rc"; exit 0' ERR EXIT

command -v python3 >/dev/null 2>&1 || { log "python3 not on PATH"; exit 0; }
command -v agent-collab >/dev/null 2>&1 || { log "agent-collab not on PATH"; exit 0; }

hook_stdin="$(cat 2>/dev/null || true)"

# Extract session_id and the Claude-reported cwd from the stdin JSON.
if [[ -n "$hook_stdin" ]]; then
  parsed="$(HOOK_STDIN="$hook_stdin" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ.get("HOOK_STDIN", ""))
    if isinstance(d, dict):
        print(d.get("session_id", ""))
        print(d.get("cwd", ""))
except Exception:
    print("")
    print("")
' 2>/dev/null || true)"
  session_id="$(printf '%s' "$parsed" | sed -n '1p')"
  hook_cwd="$(printf '%s' "$parsed" | sed -n '2p')"
else
  session_id=""
  hook_cwd=""
fi

# Use hook-reported cwd when present (Claude is authoritative about where
# the session is running); fall back to shell $PWD otherwise.
use_cwd="${hook_cwd:-$PWD}"

if [[ -n "$session_id" ]]; then
  export CLAUDE_SESSION_ID="$session_id"
  # Record seen session_id so future `session register` calls can find it.
  agent-collab hook log-session --cwd "$use_cwd" --session-id "$session_id" \
    >/dev/null 2>>"$LOG" || log "hook log-session failed"
fi

# Fast-path: if the inbox-dirty marker exists and is older than the last
# per-cwd hook-ack mtime, there is nothing to read. Exit quickly.
#
# The marker is touched by every peer-send / peer-broadcast / system-event
# code path (Python today, Go in W2+). The ack file is touched by THIS
# hook after every successful fetch. Comparing the two mtimes is a
# single-stat check that skips the Go binary + SQLite open entirely on
# the common "no new messages" case.
marker="$HOME/.agent-collab/inbox-dirty"
# Ack key includes the session identifier alongside cwd so multi-label-
# per-cwd rooms (e.g. mediated 10+ label sessions) don't starve each
# other's fetches. Before this fix (A2), label `alpha` touching the ack
# made `beta`'s hook short-circuit even though beta's inbox had unread
# messages. Fall back to "nosession" when the env hasn't surfaced a key
# yet — first-ever prompt before `session register` lands.
ack_session_key="${session_id:-${AGENT_COLLAB_SESSION_KEY:-${CLAUDE_SESSION_ID:-${CODEX_SESSION_ID:-${GEMINI_SESSION_ID:-nosession}}}}}"
ack_hash="$(printf '%s::%s' "$use_cwd" "$ack_session_key" | shasum -a 256 | cut -d' ' -f1 | cut -c1-16)"
ack_dir="$HOME/.agent-collab/hook-ack"
ack_file="$ack_dir/$ack_hash"

mkdir -p "$ack_dir" 2>/dev/null || true

# If the marker doesn't exist, no one has ever sent a message — bail.
if [[ ! -f "$marker" ]]; then
  exit 0
fi

# If there's an ack file and it's at least as new as the marker, nothing
# new has arrived for this cwd since our last fetch. Bail.
if [[ -f "$ack_file" && "$ack_file" -nt "$marker" ]]; then
  exit 0
fi

# Dirty path. Invoke the Go hook binary if available and not forced to
# Python. Go binary is ~5ms end-to-end; Python is ~80-150ms.
use_go=0
if [[ "${AGENT_COLLAB_FORCE_PY:-0}" != "1" ]]; then
  if command -v peer-inbox-hook >/dev/null 2>&1; then
    use_go=1
    hook_bin="$(command -v peer-inbox-hook)"
  elif [[ -x "$HOME/.local/bin/peer-inbox-hook" ]]; then
    use_go=1
    hook_bin="$HOME/.local/bin/peer-inbox-hook"
  elif [[ -x "$(dirname "$0")/../go/bin/peer-inbox-hook" ]]; then
    use_go=1
    hook_bin="$(dirname "$0")/../go/bin/peer-inbox-hook"
  fi
fi

if [[ "$use_go" == "1" ]]; then
  if ! output="$("$hook_bin" "$use_cwd" 2>>"$LOG")"; then
    log "go hook failed; falling back to Python"
    use_go=0
  fi
fi

if [[ "$use_go" != "1" ]]; then
  # Python fallback. Same output contract: empty string on empty inbox,
  # else a hookSpecificOutput envelope on stdout.
  if ! output="$(agent-collab peer receive --cwd "$use_cwd" --format hook-json --mark-read 2>>"$LOG")"; then
    log "peer receive failed"
    exit 0
  fi
fi

# Touch ack file so the next invocation's fast-path short-circuits until a
# new marker bump arrives. Do this whether output was empty or not —
# either way, we've observed the DB state as of $marker's current mtime.
touch "$ack_file" 2>/dev/null || true

[[ -n "$output" ]] && printf '%s' "$output"
exit 0
