#!/usr/bin/env bash
# peer-inbox-inject.sh — unified pre-prompt hook for Claude / Codex / Gemini.
#
# v3.x Option J: one script handles all three CLIs. All three provide a
# top-level stdin JSON with `session_id`, `cwd`, and `hook_event_name`
# fields, so the schema-detection lives in the `hook_event_name` value
# (Claude/Codex="UserPromptSubmit", Gemini="BeforeAgent"). The value is
# propagated to the downstream emitter (Go binary or Python fallback) so
# the hookSpecificOutput.hookEventName in our JSON output matches the
# invoking CLI's expected event name.
#
# Two jobs:
#   1. Log the session_id seen in this cwd to
#      ~/.agent-collab/claude-sessions-seen/<cwd-hash>.log so that
#      `agent-collab session register` can later adopt it — needed when the
#      runtime doesn't export its session-id env var to Bash-tool
#      subprocesses. (File path retains the `claude-` prefix for back-compat;
#      Codex/Gemini session-ids land in the same directory.)
#   2. Fetch unread peer messages as hookSpecificOutput.additionalContext
#      JSON and emit to stdout.
#
# Hot path is the Go `peer-inbox-hook` binary when available. We
# short-circuit via an `~/.agent-collab/inbox-dirty` mtime marker so an
# empty inbox exits without any database work. Fall back to the Python
# path whenever the Go binary is missing or `AGENT_COLLAB_FORCE_PY=1`.
#
# Fail-open on every error path: the user's turn must never be blocked.

set -uo pipefail

# Topic 3 §3.4 (f): daemon-spawn short-circuit. When the daemon (W3)
# spawns a CLI, it exports AGENT_COLLAB_DAEMON_SPAWN=1 and delivers the
# peer-inbox envelope directly via prompt injection (§2.4). The hook
# must no-op so it does NOT also run the interactive receive path and
# consume inbox rows a second time. Additive to the existing
# AGENT_COLLAB_FORCE_PY=1 env-flag pattern at line ~126 below.
#
# Correctness safety net is §3.4 (a) SQL partition (`AND claimed_at
# IS NULL` on interactive reads, landed in commit c96868f) — if this
# short-circuit is ever skipped the hook still cannot double-consume
# a daemon-claimed row. This early-exit is a performance optimization
# that avoids paying a DB round-trip for nothing on daemon spawns.
if [[ "${AGENT_COLLAB_DAEMON_SPAWN:-0}" == "1" ]]; then
  exit 0
fi

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

# Extract session_id, cwd, and hook_event_name from the stdin JSON. All
# three CLIs (Claude UserPromptSubmit, Codex UserPromptSubmit, Gemini
# BeforeAgent) put these at the same top-level keys, so one parse works
# universally. hook_event_name disambiguates Gemini ("BeforeAgent") from
# Claude/Codex ("UserPromptSubmit") for output-envelope emission.
if [[ -n "$hook_stdin" ]]; then
  parsed="$(HOOK_STDIN="$hook_stdin" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ.get("HOOK_STDIN", ""))
    if isinstance(d, dict):
        print(d.get("session_id", ""))
        print(d.get("cwd", ""))
        print(d.get("hook_event_name", ""))
except Exception:
    print("")
    print("")
    print("")
' 2>/dev/null || true)"
  session_id="$(printf '%s' "$parsed" | sed -n '1p')"
  hook_cwd="$(printf '%s' "$parsed" | sed -n '2p')"
  hook_event_name="$(printf '%s' "$parsed" | sed -n '3p')"
else
  session_id=""
  hook_cwd=""
  hook_event_name=""
fi

# Use hook-reported cwd when present (the CLI is authoritative about where
# the session is running); fall back to shell $PWD otherwise.
use_cwd="${hook_cwd:-$PWD}"

# Default the emitted hookEventName to "UserPromptSubmit" (Claude + Codex
# share this value). Gemini BeforeAgent sends "BeforeAgent", which we
# preserve verbatim. Downstream emitters (Go binary, Python CLI) read
# AGENT_COLLAB_HOOK_EVENT_NAME and reflect it in the output envelope's
# hookSpecificOutput.hookEventName so the consuming runtime sees the
# value it expects.
export AGENT_COLLAB_HOOK_EVENT_NAME="${hook_event_name:-UserPromptSubmit}"

if [[ -n "$session_id" ]]; then
  export CLAUDE_SESSION_ID="$session_id"
  # Record seen session_id so future `session register` calls can find it.
  agent-collab hook log-session --cwd "$use_cwd" --session-id "$session_id" \
    >/dev/null 2>>"$LOG" || log "hook log-session failed"

  # v3.8 state=active write. Unconditional counterpart to the Stop hook's
  # idle write — must run regardless of whether the inbox fast-path below
  # short-circuits the Go binary (which previously owned this write). Uses
  # --session-key so it resolves by session_key index alone and works even
  # when no marker is reachable from $use_cwd.
  state_bin=""
  if command -v peer-inbox >/dev/null 2>&1; then
    state_bin="$(command -v peer-inbox)"
  elif [[ -x "$HOME/.local/bin/peer-inbox" ]]; then
    state_bin="$HOME/.local/bin/peer-inbox"
  fi
  if [[ -n "$state_bin" ]]; then
    "$state_bin" session-state active --session-key "$session_id" --quiet \
      >/dev/null 2>>"$LOG" || log "session-state active failed"
  fi
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
