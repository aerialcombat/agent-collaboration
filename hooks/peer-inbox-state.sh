#!/usr/bin/env bash
# peer-inbox-state.sh — Claude Code Stop-hook that flips the session to
# state=idle when the agent's turn completes. Paired with the Go
# UserPromptSubmit hook binary (go/cmd/hook) which flips to state=active.
#
# Claude Code invokes Stop hooks after every assistant turn (including
# tool-use cycles). We don't need sub-ms latency here — the hook fires
# when the user is about to see the result, not on the hot prompt
# submission path.
#
# Stdin (from Claude Code): {"session_id": "...", "cwd": "...", ...}.
# We read session_id + cwd, invoke peer-inbox session-state idle, exit 0
# regardless of outcome. Fail-open: the user's view must never be
# blocked by agent-collab bookkeeping.

set -uo pipefail

LOG="${AGENT_COLLAB_HOOK_LOG:-$HOME/.agent-collab/hook.log}"
log() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
  printf '[%s] peer-inbox-state: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" \
    >>"$LOG" 2>/dev/null || true
}
trap 'exit 0' ERR EXIT

# Resolve the binary. Prefer PATH install, then $HOME/.local/bin, then
# a dev-repo sibling (go/bin/peer-inbox).
resolve_bin() {
  command -v peer-inbox 2>/dev/null && return
  [[ -x "$HOME/.local/bin/peer-inbox" ]] && { printf '%s\n' "$HOME/.local/bin/peer-inbox"; return; }
  local self_dir
  self_dir="$(cd "$(dirname "$0")" && pwd -P 2>/dev/null || true)"
  [[ -n "$self_dir" && -x "$self_dir/../go/bin/peer-inbox" ]] \
    && { printf '%s\n' "$self_dir/../go/bin/peer-inbox"; return; }
}

peer_inbox_bin="$(resolve_bin)"
if [[ -z "$peer_inbox_bin" ]]; then
  log "peer-inbox binary not found; no-op"
  exit 0
fi

hook_stdin="$(cat 2>/dev/null || true)"
session_id=""
hook_cwd=""
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
fi

# Export the session id so the Go binary's env chain picks it up.
[[ -n "$session_id" ]] && export CLAUDE_SESSION_ID="$session_id"

# Prefer session-key path (cheap, no cwd resolve) since the hook carries
# it natively. Go CLI handles "key doesn't resolve to a row" as no-op.
args=(session-state idle --quiet)
[[ -n "$session_id" ]] && args+=(--session-key "$session_id")
[[ -n "$hook_cwd" ]] && args+=(--cwd "$hook_cwd")

"$peer_inbox_bin" "${args[@]}" >/dev/null 2>>"$LOG" || log "session-state idle failed"
exit 0
