#!/usr/bin/env bash
# tests/utf8-budget-parity.sh — idle-birch #3: Go's formatHookBlock
# (go/cmd/hook/main.go:186-233) measures the per-entry budget in BYTES
# (`len(entry)` on a Go string is byte count), while Python's
# format_hook_block (scripts/peer-inbox-db.py:1734-1760) measures it in
# CHARACTERS (`len(entry)` on a Python str is char count).
#
# For ASCII content the two agree. For multi-byte UTF-8 content (Korean,
# Chinese, emoji, accented European, etc.), a given message entry occupies
# ~3× more budget in Go than in Python. This means the same unread-inbox
# state renders DIFFERENT truncation behavior depending on which runtime
# the hook selected — users see different additionalContext depending on
# whether the Go binary or the Python fallback handled the prompt.
#
# This test:
#   1. Registers two receivers in two separate cwds (recv-go, recv-py) so
#      each sees an identical unread set without cross-interference.
#   2. Sends the same 3-message sequence (tiny ASCII → large Korean body
#      → tiny ASCII) to both.
#   3. Renders one via the Go hook binary, the other via Python's
#      `peer receive --format hook-json`.
#   4. Asserts whether the two outputs match — they MUST for true parity.
#
# Expected result on HEAD: outputs DIVERGE. Go truncates at the large
# Korean body because 6000 UTF-8 bytes > (remaining byte budget); Python
# measures the same body as 2000 chars and stays under-budget. Test
# documents the bug; fix is one of: (a) align Go to Python's char
# semantics, (b) align Python to Go's byte semantics. Idle-birch #3
# flagged this as coder's choice.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
BIN_DIR="$(mktemp -d)"
BIN="$BIN_DIR/peer-inbox-hook"

cleanup() { rm -rf "$TMP" "$BIN_DIR"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

( cd "$PROJECT_ROOT/go" && go build -o "$BIN" ./cmd/hook ) || {
  echo "skip: go build failed"
  exit 0
}

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
export PATH="$BIN_DIR:$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
RECV_GO_CWD="$TMP/recv-go"
RECV_PY_CWD="$TMP/recv-py"
SEND_CWD="$TMP/send"
mkdir -p "$RECV_GO_CWD" "$RECV_PY_CWD" "$SEND_CWD"

AC="$PROJECT_ROOT/scripts/agent-collab"

# Korean Hangul syllable "가" = 3 UTF-8 bytes × 2000 = 6000 bytes.
# Well under MAX_BODY_BYTES (8192) — allowed at send time.
BIG_KOREAN="$(python3 -c 'print("가" * 2000, end="")')"

echo "-- utf8-budget-parity: register recv-go, recv-py, send --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-go" \
  "$AC" session register --cwd "$RECV_GO_CWD" --label recv-go --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-py" \
  "$AC" session register --cwd "$RECV_PY_CWD" --label recv-py --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label send --agent claude >/dev/null

# Identical 3-message seed to each recipient.
seed_one() {
  local to_label="$1" to_cwd="$2"
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$to_label" --to-cwd "$to_cwd" \
      --message "small ASCII msg 1" >/dev/null
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$to_label" --to-cwd "$to_cwd" \
      --message "$BIG_KOREAN" >/dev/null
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$to_label" --to-cwd "$to_cwd" \
      --message "small ASCII msg 3" >/dev/null
}

echo "-- utf8-budget-parity: seed 3 identical msgs to recv-go and recv-py --"
seed_one recv-go "$RECV_GO_CWD"
seed_one recv-py "$RECV_PY_CWD"

echo "-- utf8-budget-parity: render via Go hook binary --"
go_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-go" \
    "$BIN" "$RECV_GO_CWD" 2>/dev/null
)"

echo "-- utf8-budget-parity: render via Python peer receive --format hook-json --"
py_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-py" \
    "$AC" peer receive --cwd "$RECV_PY_CWD" --format hook-json --mark-read 2>/dev/null
)"

# Extract the additionalContext <peer-inbox> block from each so the
# comparison focuses on render parity, not label-in-header differences.
extract_block() {
  python3 -c "
import json, sys
raw = sys.argv[1]
if not raw:
    print('EMPTY')
    sys.exit()
try:
    d = json.loads(raw)
    print(d['hookSpecificOutput']['additionalContext'])
except Exception as e:
    print(f'PARSE_ERROR: {e!r} input={raw[:120]!r}')
" "$1"
}

go_block="$(extract_block "$go_out")"
py_block="$(extract_block "$py_out")"

echo ""
echo "== GO HOOK BLOCK =="
printf '%s\n' "$go_block" | head -c 400; echo
echo "   ... ($(printf '%s' "$go_block" | wc -c | tr -d ' ') bytes total)"

echo ""
echo "== PYTHON HOOK BLOCK =="
printf '%s\n' "$py_block" | head -c 400; echo
echo "   ... ($(printf '%s' "$py_block" | wc -c | tr -d ' ') bytes total)"

# Normalize by stripping the from-session-labels attribute (which
# legitimately differs: "send" vs "send" is the same — but keep logic
# robust in case future work diversifies labels).
norm_go="$(printf '%s' "$go_block" | sed -E 's/from-session-labels="[^"]*"/from-session-labels="_"/')"
norm_py="$(printf '%s' "$py_block" | sed -E 's/from-session-labels="[^"]*"/from-session-labels="_"/')"
# Also strip per-message timestamps since they differ across two separate sends.
norm_go="$(printf '%s' "$norm_go" | sed -E 's/\[send @ [^]]+\]/[send @ T]/g')"
norm_py="$(printf '%s' "$norm_py" | sed -E 's/\[send @ [^]]+\]/[send @ T]/g')"

# Capture truncation markers for diagnostic output.
go_trunc="$(printf '%s' "$go_block" | grep -o '\[+[0-9]* more messages truncated' || true)"
py_trunc="$(printf '%s' "$py_block" | grep -o '\[+[0-9]* more messages truncated' || true)"

echo ""
echo "-- utf8-budget-parity: truncation state --"
echo "   Go  trunc marker: ${go_trunc:-<none>}"
echo "   Py  trunc marker: ${py_trunc:-<none>}"

if [[ "$norm_go" == "$norm_py" ]]; then
  echo ""
  echo "PASS: Go and Python rendered the same <peer-inbox> block (UTF-8 budget parity)"
  exit 0
fi

echo ""
echo "FAIL: Go and Python rendered DIFFERENT <peer-inbox> blocks under identical multi-byte UTF-8 input."
echo "      This is idle-birch #3 — Go measures len(entry) in bytes (~3× per Korean char);"
echo "      Python measures len(entry) in chars. Same DB state, divergent user-visible render."
echo "      Fix: pick one semantic (bytes or chars) and converge. Per the plan doc, Go's byte-count"
echo "      is the Go-idiomatic choice; Python would need to encode first. Or vice versa."
exit 1
