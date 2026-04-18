#!/usr/bin/env bash
# tests/hook-parity.sh — shape-2 hook-parity regression gate for v3.x Option J.
#
# Verifies that the unified hooks/peer-inbox-inject.sh emits a correct
# hookSpecificOutput envelope for each of the three supported CLIs (Claude
# Code, Codex CLI, Gemini CLI) when fed the canonical stdin fixture for
# that CLI.
#
# Assertions per fixture:
#   1. stdout is non-empty and parses as valid JSON
#   2. hookSpecificOutput.hookEventName matches the fixture's hook_event_name:
#        - Claude: "UserPromptSubmit"
#        - Codex:  "UserPromptSubmit"
#        - Gemini: "BeforeAgent"
#   3. hookSpecificOutput.additionalContext contains the seeded probe body
#
# Shape-2 byte-equality assertion (load-bearing for Option J):
#   Given three sessions registered with identical (cwd, label) but keyed
#   by each fixture's session_id, all receiving a byte-identical probe
#   body, the `additionalContext` string across all three fixtures is
#   byte-identical after normalizing per-message timestamps and per-CLI
#   hookEventName. Catches regressions where the block renderer diverges
#   per CLI.
#
# Runs against the Python fallback path by default (AGENT_COLLAB_FORCE_PY=1)
# for maximum determinism. Rebuilding the Go binary and running through it
# is covered by alpha/gamma's review-round empirical checks + owner-
# supervised end-to-end probes in docs/HOOK-PARITY-VALIDATION.md.
#
# Skips cleanly when python3 or agent-collab is not on PATH.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi

AC="$PROJECT_ROOT/scripts/agent-collab"
HOOK="$PROJECT_ROOT/hooks/peer-inbox-inject.sh"
FIXTURE_DIR="$PROJECT_ROOT/tests/fixtures/hook-stdin"

[[ -x "$AC" ]]            || { echo "FAIL: $AC not executable" >&2; exit 1; }
[[ -r "$HOOK" ]]          || { echo "FAIL: $HOOK missing" >&2; exit 1; }
[[ -d "$FIXTURE_DIR" ]]   || { echo "FAIL: fixture dir $FIXTURE_DIR missing" >&2; exit 1; }

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"

# Isolate PATH so agent-collab resolves to the repo's dispatcher.
export PATH="$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
# One cwd per fixture (same label "recv" cannot collide across different cwds).
CLAUDE_CWD="$TMP/recv-claude"
CODEX_CWD="$TMP/recv-codex"
GEMINI_CWD="$TMP/recv-gemini"
SEND_CWD="$TMP/send"
mkdir -p "$CLAUDE_CWD" "$CODEX_CWD" "$GEMINI_CWD" "$SEND_CWD"

export AGENT_COLLAB_INBOX_DB="$DB"

# Force Python fallback so the test is deterministic regardless of whether
# the Go hook binary has been built. The emitter path on the hot path is
# identical in contract; Python is the CI-safe baseline.
export AGENT_COLLAB_FORCE_PY=1

# Extract session_id values from each fixture to drive session registration.
extract_session_id() {
  local fixture="$1"
  python3 -c "
import json, sys
with open(sys.argv[1]) as f:
    print(json.load(f)['session_id'])
" "$FIXTURE_DIR/$fixture.json"
}

CLAUDE_KEY="$(extract_session_id claude)"
CODEX_KEY="$(extract_session_id codex)"
GEMINI_KEY="$(extract_session_id gemini)"
SEND_KEY="key-sender-parity"

# Register one recipient per fixture in its own cwd (label collision
# within a single cwd is prevented by design — the A2 fix keyed ack-file
# by session, not label, but label uniqueness per cwd is still enforced
# at session-register time). Different cwds + same label lets each
# recipient receive a broadcast copy identical body-wise, and post-
# normalization byte-equality across the three renders asserts no
# per-CLI divergence in the hook block renderer.
echo "-- hook-parity: register 3 recipients (one per cwd) + 1 sender --"
AGENT_COLLAB_SESSION_KEY="$CLAUDE_KEY" \
  "$AC" session register --cwd "$CLAUDE_CWD" --label recv --agent claude >/dev/null \
  || fail "session register failed for claude key"
AGENT_COLLAB_SESSION_KEY="$CODEX_KEY" \
  "$AC" session register --cwd "$CODEX_CWD" --label recv --agent codex >/dev/null \
  || fail "session register failed for codex key"
AGENT_COLLAB_SESSION_KEY="$GEMINI_KEY" \
  "$AC" session register --cwd "$GEMINI_CWD" --label recv --agent gemini >/dev/null \
  || fail "session register failed for gemini key"
AGENT_COLLAB_SESSION_KEY="$SEND_KEY" \
  "$AC" session register --cwd "$SEND_CWD" --label sender --agent claude >/dev/null \
  || fail "session register failed for sender"

# Send the same canonical probe body to each recipient. Each receives
# an identical body; post-normalization byte-equality across the three
# renders asserts no per-CLI divergence in the hook block renderer.
PROBE_BODY="hook-parity canonical probe"
echo "-- hook-parity: send probe to each recv session --"
for cwd_pair in "$CLAUDE_CWD" "$CODEX_CWD" "$GEMINI_CWD"; do
  AGENT_COLLAB_SESSION_KEY="$SEND_KEY" \
    "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$cwd_pair" \
      --message "$PROBE_BODY" >/dev/null \
    || fail "peer send failed for recv in $cwd_pair"
done

# Feed each fixture through the unified hook, capture envelope. Using
# plain variables (not associative arrays) for bash 3.2 compatibility
# (macOS default /bin/bash). Capture hookEventName + additionalContext
# into per-CLI temp files so normalization + byte-comparison stays clean.
for cli in claude codex gemini; do
  case "$cli" in
    claude) key="$CLAUDE_KEY"; recv_cwd="$CLAUDE_CWD" ;;
    codex)  key="$CODEX_KEY";  recv_cwd="$CODEX_CWD" ;;
    gemini) key="$GEMINI_KEY"; recv_cwd="$GEMINI_CWD" ;;
  esac

  # Substitute __HOOK_PARITY_CWD__ placeholder with this CLI's recv_cwd.
  stdin_json="$(sed "s|__HOOK_PARITY_CWD__|$recv_cwd|g" "$FIXTURE_DIR/$cli.json")"

  # Feed through unified hook. Clean env to avoid leaking outer session
  # state into the hook's session-key cascade.
  out="$(env -i \
    PATH="$PATH" \
    HOME="$HOME" \
    AGENT_COLLAB_INBOX_DB="$DB" \
    AGENT_COLLAB_FORCE_PY=1 \
    AGENT_COLLAB_SESSION_KEY="$key" \
    bash "$HOOK" <<<"$stdin_json" 2>/dev/null)"

  if [[ -z "$out" ]]; then
    fail "$cli: hook emitted empty output (expected hookSpecificOutput envelope)"
  fi

  # Parse envelope via python, write hookEventName to $TMP/$cli.event and
  # additionalContext to $TMP/$cli.ctx so we can diff / normalize with
  # plain-file tools.
  HOOK_OUT="$out" python3 - "$TMP" "$cli" <<'PYEOF' || fail "$cli: envelope JSON parse failed"
import json, os, sys
tmp, cli = sys.argv[1], sys.argv[2]
d = json.loads(os.environ["HOOK_OUT"])
with open(os.path.join(tmp, f"{cli}.event"), "w") as f:
    f.write(d["hookSpecificOutput"]["hookEventName"])
with open(os.path.join(tmp, f"{cli}.ctx"), "w") as f:
    f.write(d["hookSpecificOutput"]["additionalContext"])
PYEOF
done

# Assertion 1: hookEventName per CLI.
echo "-- hook-parity: assertion 1 — hookEventName per fixture --"
claude_event="$(cat "$TMP/claude.event")"
codex_event="$(cat "$TMP/codex.event")"
gemini_event="$(cat "$TMP/gemini.event")"

[[ "$claude_event" == "UserPromptSubmit" ]] \
  || fail "claude: hookEventName='$claude_event', expected UserPromptSubmit"
[[ "$codex_event" == "UserPromptSubmit" ]] \
  || fail "codex: hookEventName='$codex_event', expected UserPromptSubmit"
[[ "$gemini_event" == "BeforeAgent" ]] \
  || fail "gemini: hookEventName='$gemini_event', expected BeforeAgent"
echo "   claude:UserPromptSubmit  codex:UserPromptSubmit  gemini:BeforeAgent  all match"

# Assertion 2: each context contains the probe body.
echo "-- hook-parity: assertion 2 — probe body delivered per fixture --"
for cli in claude codex gemini; do
  if ! grep -qF "$PROBE_BODY" "$TMP/$cli.ctx"; then
    fail "$cli: additionalContext missing probe body $PROBE_BODY"
  fi
done
echo "   probe body delivered in all three"

# Assertion 3: byte-identical additionalContext across three fixtures
# after normalizing the per-message timestamp. Since all three recipients
# share label=recv and received the same probe body, the rendered block
# should differ only in timestamp (which varies per sent row).
echo "-- hook-parity: assertion 3 — additionalContext byte-identical (modulo timestamp) --"
for cli in claude codex gemini; do
  # Strip per-message timestamps [sender @ T] -> [sender @ TS].
  sed -E 's/\[sender @ [^]]+\]/[sender @ TS]/g' "$TMP/$cli.ctx" > "$TMP/$cli.norm"
done

if ! diff -q "$TMP/claude.norm" "$TMP/codex.norm" >/dev/null; then
  echo "--- claude.norm ---"; cat "$TMP/claude.norm"
  echo "--- codex.norm ---";  cat "$TMP/codex.norm"
  fail "claude and codex additionalContext diverged after timestamp normalization"
fi
if ! diff -q "$TMP/claude.norm" "$TMP/gemini.norm" >/dev/null; then
  echo "--- claude.norm ---"; cat "$TMP/claude.norm"
  echo "--- gemini.norm ---"; cat "$TMP/gemini.norm"
  fail "claude and gemini additionalContext diverged after timestamp normalization"
fi
echo "   all three renders byte-identical modulo timestamp"

echo "PASS: hook-parity — unified hook emits correct per-CLI hookEventName + identical additionalContext"
