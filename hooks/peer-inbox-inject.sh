#!/usr/bin/env bash
# peer-inbox-inject.sh — Claude Code UserPromptSubmit hook (also works for
# Gemini BeforeAgent once install is wired in v1.1).
#
# Contract: emits JSON on stdout with hookSpecificOutput.additionalContext
# containing unread peer-inbox messages, or empty output when there are none.
# Fails open on every error — a broken inbox must never block the user's turn.
#
# Installed to: ~/.agent-collab/hooks/peer-inbox-inject.sh
# Logs failures to: ~/.agent-collab/hook.log (override via AGENT_COLLAB_HOOK_LOG)

set -uo pipefail

LOG="${AGENT_COLLAB_HOOK_LOG:-$HOME/.agent-collab/hook.log}"

log() {
  # best-effort logging; never fail the hook over a log write
  mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
  printf '[%s] peer-inbox-inject: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" \
    >>"$LOG" 2>/dev/null || true
}

# Fail open on every exit path.
trap 'rc=$?; [[ $rc -ne 0 ]] && log "failed at line $LINENO exit $rc"; exit 0' ERR EXIT

if ! command -v agent-collab >/dev/null 2>&1; then
  log "agent-collab not on PATH; skipping"
  exit 0
fi

# Claude Code passes hook metadata as JSON on stdin. Extract session_id and
# export as CLAUDE_SESSION_ID so peer-inbox can resolve self-identity when
# multiple sessions share a cwd. Fail open if stdin or parsing fails.
if ! command -v python3 >/dev/null 2>&1; then
  log "python3 not on PATH; skipping"
  exit 0
fi

hook_stdin="$(cat 2>/dev/null || true)"
if [[ -n "$hook_stdin" ]]; then
  extracted="$(HOOK_STDIN="$hook_stdin" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ.get("HOOK_STDIN", ""))
    print(d.get("session_id", ""))
except Exception:
    pass
' 2>/dev/null || true)"
  if [[ -n "$extracted" ]]; then
    export CLAUDE_SESSION_ID="$extracted"
  fi
fi

# peer receive --format hook-json emits the full Claude UserPromptSubmit
# envelope. Empty output (no unread messages) means no injection.
if ! output="$(agent-collab peer receive --format hook-json --mark-read 2>>"$LOG")"; then
  log "peer receive failed; skipping"
  exit 0
fi

if [[ -n "$output" ]]; then
  printf '%s' "$output"
fi
exit 0
