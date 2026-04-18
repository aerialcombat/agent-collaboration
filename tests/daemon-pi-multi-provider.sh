#!/usr/bin/env bash
# tests/daemon-pi-multi-provider.sh — Topic 3 v0.2 §9.2 gate 2:
# argv-correctness parameterized across pi providers. Single provider-
# agnostic fake-pi stub that inspects --provider argv to vary behavior.
#
# Asserts for each of {zai-glm, openai-codex, anthropic}:
#   - argv contains --provider <P> + --model <M>
#   - argv contains EXACTLY ONE --provider + EXACTLY ONE --model occurrence
#     (guards against precedence-merge duplication bugs where config-merge
#     paths accidentally emit both default and resolved — test-engineer ADD)

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
if [[ -z "${KEEP_TMP:-}" ]]; then
  trap cleanup EXIT
fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-pi-multi-provider: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) || { echo "skip"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

SEND_CWD="$TMP/send"
mkdir -p "$SEND_CWD"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "register sender failed"

FAKE_PI="$TMP/fake-pi.sh"
cat > "$FAKE_PI" <<'EOF'
#!/usr/bin/env bash
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"
{
  echo "ARGV_COUNT=$#"
  i=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    i=$((i+1))
  done
} > "$OUT"
if [[ -n "${SESS_PATH:-}" ]]; then
  mkdir -p "$(dirname "$SESS_PATH")"
  echo '{"turn":"multi-provider-probe"}' >> "$SESS_PATH"
fi
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_PI"

PI_SESSION_DIR="$TMP/pi-sessions"
mkdir -p "$PI_SESSION_DIR"

run_for_provider() {
  local provider="$1" model="$2" label="$3"
  local cwd="$TMP/daemon-$label"
  mkdir -p "$cwd"
  AGENT_COLLAB_SESSION_KEY="key-$label" \
    "$AC" session register --cwd "$cwd" --label "$label" \
      --agent pi --receive-mode daemon >/dev/null \
    || fail "register $label failed"

  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$label" --to-cwd "$cwd" \
      --message "probe-${provider}" >/dev/null

  local outpath="$TMP/out-${provider}.txt"
  local sess_path="$PI_SESSION_DIR/${label}.jsonl"
  rm -f "$outpath" "$sess_path"
  (
    export AGENT_COLLAB_DAEMON_PI_BIN="$FAKE_PI"
    export FAKE_CLI_OUT="$outpath"
    export SESS_PATH="$sess_path"
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "key-$label" \
      --cli pi \
      --pi-provider "$provider" \
      --pi-model "$model" \
      --pi-session-dir "$PI_SESSION_DIR" \
      --ack-timeout 2 \
      --sweep-ttl 5 \
      --poll-interval 1 \
      --log-path "$TMP/daemon-${label}.log" \
      --cli-session-resume \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon.pid"
  )
  local pid; pid="$(cat "$TMP/daemon.pid")"
  local waited=0
  while (( waited < 30 )); do
    if [[ -s "$outpath" ]]; then break; fi
    sleep 0.5
    waited=$((waited+1))
  done
  sleep 0.3
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true

  [[ -s "$outpath" ]] || { cat "$TMP/daemon-${label}.log"; fail "fake-pi did not run for $provider"; }

  # argv correctness: --provider + --model present with correct values.
  grep -q "^ARGV\[.*\]=${provider}$" "$outpath" \
    || { cat "$outpath"; fail "missing provider=${provider}"; }
  grep -q "^ARGV\[.*\]=${model}$" "$outpath" \
    || { cat "$outpath"; fail "missing model=${model}"; }

  # test-engineer ADD: EXACTLY ONE --provider + EXACTLY ONE --model.
  local prov_count model_count
  prov_count="$(grep -cE '^ARGV\[[0-9]+\]=--provider$' "$outpath" || true)"
  model_count="$(grep -cE '^ARGV\[[0-9]+\]=--model$' "$outpath" || true)"
  if [[ "$prov_count" != "1" ]]; then
    cat "$outpath"
    fail "expected EXACTLY ONE --provider in argv, got $prov_count"
  fi
  if [[ "$model_count" != "1" ]]; then
    cat "$outpath"
    fail "expected EXACTLY ONE --model in argv, got $model_count"
  fi

  echo "   provider=$provider model=$model — argv correct, single occurrence each"
}

step "zai-glm / glm-4.6"
run_for_provider "zai-glm" "glm-4.6" "mp-zai"
step "openai-codex / gpt-5.1"
run_for_provider "openai-codex" "gpt-5.1" "mp-openai"
step "anthropic / claude-sonnet-4-5"
run_for_provider "anthropic" "claude-sonnet-4-5" "mp-anthropic"

echo "PASS: daemon-pi-multi-provider — 3 providers, argv correct, argv-exactly-one provider+model"
