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
# Fail-open on every error path: the user's turn must never be blocked.

set -uo pipefail
LOG="${AGENT_COLLAB_HOOK_LOG:-$HOME/.agent-collab/hook.log}"
log() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
  printf '[%s] peer-inbox-inject: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" \
    >>"$LOG" 2>/dev/null || true
}
trap 'rc=$?; [[ $rc -ne 0 ]] && log "failed at line $LINENO exit $rc"; exit 0' ERR EXIT

command -v agent-collab >/dev/null 2>&1 || { log "agent-collab not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { log "python3 not on PATH"; exit 0; }

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

# peer receive --format hook-json emits the full Claude UserPromptSubmit
# envelope. Empty output (no unread messages) means no injection.
if ! output="$(agent-collab peer receive --cwd "$use_cwd" --format hook-json --mark-read 2>>"$LOG")"; then
  log "peer receive failed"
  exit 0
fi

[[ -n "$output" ]] && printf '%s' "$output"
exit 0
