#!/usr/bin/env bash
# tests/daemon-prompt-interpolation.sh — v3.1 [PROMPT] patch regression gate.
#
# Asserts buildStaticPrompt's three v3.1 fixes landed correctly:
#   DEFECT 1 — no literal <YOUR_CWD> / <YOUR_LABEL> placeholders in emitted prompt
#   DEFECT 2 — no mention of the retired `peer-inbox daemon-complete` ack path
#   DEFECT 3 — reply-path instruction present + MY_LABEL interpolated
#
# Approach: spawn the daemon with a fake CLI that dumps its stdin-prompt
# to a probe file; the test then greps the captured prompt for the three
# invariants above. Two distinct daemon configs (A/B) verify
# interpolation varies with (label, cwd) — regression catch if someone
# accidentally reverts buildStaticPrompt to a static literal.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AC="$PROJECT_ROOT/scripts/agent-collab"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
cleanup() {
  if [[ -f "$TMP/daemon.pid" ]]; then
    local p; p="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  fi
  rm -rf "$TMP"
}
if [[ -z "${KEEP_TMP:-}" ]]; then trap cleanup EXIT; fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-prompt-interpolation: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) || { echo "skip"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
export AGENT_COLLAB_INBOX_DB="$TMP/sessions.db"
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

# Fake claude captures the prompt (last argv) into $PROMPT_PROBE.
FAKE_CLAUDE="$TMP/fake-claude.sh"
cat > "$FAKE_CLAUDE" <<'EOF'
#!/usr/bin/env bash
set -u
last="${*: -1}"
echo "$last" > "${PROMPT_PROBE:-/dev/null}"
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_CLAUDE"

# Sender cwd (one shared across both daemon configs).
SEND_CWD="$TMP/sender"
mkdir -p "$SEND_CWD"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender --agent claude >/dev/null \
  || fail "register sender failed"

run_one() {
  local label="$1" cwd="$2" probe="$3"
  mkdir -p "$cwd"
  AGENT_COLLAB_SESSION_KEY="key-$label" \
    "$AC" session register --cwd "$cwd" --label "$label" \
      --agent claude --receive-mode daemon >/dev/null \
    || fail "register $label failed"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$label" --to-cwd "$cwd" \
      --message "probe for $label" >/dev/null

  rm -f "$probe"
  (
    export AGENT_COLLAB_DAEMON_CLAUDE_BIN="$FAKE_CLAUDE"
    export PROMPT_PROBE="$probe"
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "key-$label" \
      --cli claude \
      --ack-timeout 2 --sweep-ttl 5 --poll-interval 1 \
      --log-path "$TMP/d-${label}.log" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon.pid"
  )
  local pid; pid="$(cat "$TMP/daemon.pid")"
  local waited=0
  while (( waited < 30 )); do
    if [[ -s "$probe" ]]; then break; fi
    sleep 0.5; waited=$((waited+1))
  done
  sleep 0.2
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  [[ -s "$probe" ]] || { cat "$TMP/d-${label}.log"; fail "probe never captured prompt for $label"; }
}

# --- Config A ----------------------------------------------------------
step "config A: label=reviewer-a cwd=/tmp/work-a"
CWD_A="$TMP/work-a"
PROBE_A="$TMP/prompt-a.txt"
run_one "reviewer-a" "$CWD_A" "$PROBE_A"

# DEFECT 1 assertion: no literal placeholders.
if grep -qE '<YOUR_(CWD|LABEL)>' "$PROBE_A"; then
  cat "$PROBE_A"
  fail "(DEFECT 1) emitted prompt still contains <YOUR_CWD> or <YOUR_LABEL> placeholder"
fi
echo "   (DEFECT 1) no <YOUR_*> placeholders"

# DEFECT 2 assertion: no retired mechanism-1 language.
if grep -q 'peer-inbox daemon-complete' "$PROBE_A"; then
  cat "$PROBE_A"
  fail "(DEFECT 2) emitted prompt still references retired mechanism-1 'peer-inbox daemon-complete'"
fi
echo "   (DEFECT 2) no mechanism-1 'peer-inbox daemon-complete' reference"

# DEFECT 3 assertion: reply-path present with MY_LABEL interpolated.
if ! grep -qF "agent-collab peer send --as reviewer-a --to <THEIR_LABEL>" "$PROBE_A"; then
  cat "$PROBE_A"
  fail "(DEFECT 3) reply-path instruction missing or MY_LABEL not interpolated"
fi
echo "   (DEFECT 3) reply-path instruction present with MY_LABEL interpolated"

# MY_LABEL + CWD identity assertions.
grep -qF '"reviewer-a"' "$PROBE_A" || fail "MY_LABEL not quoted in prompt"
grep -qF "$CWD_A" "$PROBE_A"        || fail "CWD not interpolated in prompt"
echo "   MY_LABEL + CWD both interpolated"

# --- Config B ----------------------------------------------------------
# Regression catch: if buildStaticPrompt became static-literal again,
# two different daemons would emit byte-identical prompts. We assert
# they differ.
step "config B: label=reviewer-b cwd=/tmp/work-b"
CWD_B="$TMP/work-b"
PROBE_B="$TMP/prompt-b.txt"
run_one "reviewer-b" "$CWD_B" "$PROBE_B"
grep -qF "reviewer-b" "$PROBE_B" || fail "config B MY_LABEL not interpolated"
grep -qF "$CWD_B"     "$PROBE_B" || fail "config B CWD not interpolated"

# Cross-label bleed check: A's prompt must NOT contain B's label, and
# vice versa. Byte-level diff would false-positive on per-batch envelope
# timestamps / message UUIDs; label-bleed is the structural assertion.
if grep -qF "reviewer-b" "$PROBE_A"; then
  fail "regression: A's prompt contains B's label — cross-daemon bleed"
fi
if grep -qF "reviewer-a" "$PROBE_B"; then
  fail "regression: B's prompt contains A's label — cross-daemon bleed"
fi
echo "   per-daemon interpolation isolated (no cross-label bleed)"

echo "PASS: daemon-prompt-interpolation — DEFECT 1/2/3 patches + per-daemon interpolation regression gate"
