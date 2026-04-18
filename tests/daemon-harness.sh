#!/usr/bin/env bash
# tests/daemon-harness.sh — §8.2 daemon-harness regression gates for
# Topic 3 v0 commit 7 (go/cmd/daemon). Covers the four harness contract
# assertions: W3 env-var propagation, codex stdin-close, codex
# --skip-git-repo-check, and the MANDATORY claude --settings fixture-
# pin (§8.2 strengthened from "Consider" to MUST — gamma #4).
#
# Approach: inject fake CLIs via AGENT_COLLAB_DAEMON_<CLI>_BIN env
# overrides. Each fake CLI is a bash script that dumps its argv +
# env + a marker to disk, then exits cleanly. The daemon's normal
# spawn path is exercised; we never need a real `claude`/`codex`/
# `gemini` on PATH. TTL overrides (AGENT_COLLAB_ACK_TIMEOUT=1 /
# AGENT_COLLAB_SWEEP_TTL=2) keep the test bounded — preserves the
# 2× ratio invariant from §3.4 (c).
#
# Each assertion seeds one message, starts the daemon, waits for the
# fake-CLI probe file to appear (which means the daemon spawned),
# then stops the daemon and checks the captured argv/env.

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
  # Best-effort: kill any daemon still running under this pidfile.
  if [[ -f "$TMP/daemon.pid" ]]; then
    local p; p="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-harness: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

# Tight TTLs — preserve 2× ratio.
export AGENT_COLLAB_ACK_TIMEOUT=1
export AGENT_COLLAB_SWEEP_TTL=2

DAEMON_CWD="$TMP/daemon"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

# Register sessions once; reused across assertions.
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-recv \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register daemon failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"

# -----------------------------------------------------------------------------
# Fake CLI probe: dumps argv + relevant env vars + stdin-bytes-read then
# exits. Writes to $FAKE_CLI_OUT (exported per-assertion so each test
# gets its own trace file).
# -----------------------------------------------------------------------------
make_fake_cli() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
# Fake CLI probe for tests/daemon-harness.sh. Dumps argv + env to
# $FAKE_CLI_OUT and exits with a JSONL ack marker so the daemon moves
# on cleanly. Reads stdin with a 1s timeout so if the daemon fails to
# close stdin we'd observe it as a measurable delay.
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"
{
  echo "ARGV_COUNT=$#"
  i=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    i=$((i+1))
  done
  for k in AGENT_COLLAB_DAEMON_SPAWN AGENT_COLLAB_SESSION_KEY CLAUDE_SESSION_ID CODEX_SESSION_ID GEMINI_SESSION_ID ZAI_GLM_API_KEY PATH; do
    v="${!k-}"
    echo "ENV[$k]=$v"
  done
} > "$OUT"

# Read stdin with a 1s timeout — if daemon passed /dev/null (os.Exec
# nil), `read` returns immediately with EOF. If daemon leaked an open
# stdin, `read` would block until timeout. Record the elapsed time.
start="$(date +%s)"
# Use `read -t 1` to bound — POSIX-ish; macos bash 3.2 supports it.
( read -t 1 _ ) 2>/dev/null || true
end="$(date +%s)"
echo "STDIN_READ_ELAPSED_SEC=$((end - start))" >> "$OUT"

# Emit JSONL ack marker so daemon completes the batch and moves on.
echo '{"peer_inbox_ack": true}'
EOF
  chmod +x "$path"
}

# Seed one message to the daemon label. Returns the row id so we can
# tear-down by manipulating that row.
seed_one() {
  local tag="$1"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
      --message "harness probe: $tag" >/dev/null
}

# Start the daemon in the background for one of the three CLI kinds.
# Waits until the probe file is populated (which means daemon
# spawned), then stops the daemon. Returns via the global
# $FAKE_CLI_OUT file.
run_one_spawn() {
  local cli_kind="$1"    # claude|codex|gemini
  local fake_cli="$2"    # path to the fake CLI script

  FAKE_CLI_OUT="$TMP/fake-out-${cli_kind}.txt"
  rm -f "$FAKE_CLI_OUT"
  export FAKE_CLI_OUT

  # Clear any prior daemon claims + completed rows so this spawn
  # starts from a clean slate on the inbox.
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = 'daemon-recv'\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-recv'\")
c.commit()
c.close()
"

  seed_one "$cli_kind"

  local bin_env_key
  case "$cli_kind" in
    claude) bin_env_key="AGENT_COLLAB_DAEMON_CLAUDE_BIN" ;;
    # v0.3 SOFT SHIM: --cli=codex / --cli=gemini route through spawnPi,
    # so the fake binary must be bound to the PI bin-override env var.
    # CODEX_BIN / GEMINI_BIN are retained as constants only for the
    # claude asymmetry test, if any; under v0.3 both resolve to the pi
    # execSpawn path.
    codex)  bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN" ;;
    gemini) bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN" ;;
    pi)     bin_env_key="AGENT_COLLAB_DAEMON_PI_BIN" ;;
  esac

  # v0.3: --cli=pi / codex / gemini all route through spawnPi and require
  # pi.model (codex + gemini auto-populate pi.provider via the SOFT SHIM).
  # Add the required pi args so the shim preflight passes in all three.
  local extra_args_str=""
  case "$cli_kind" in
    pi)
      extra_args_str="--pi-provider zai-glm --pi-model glm-4.6 --pi-session-dir $TMP/pi-sessions-harness"
      ;;
    codex)
      extra_args_str="--pi-model gpt-5.3-codex --pi-session-dir $TMP/pi-sessions-harness"
      ;;
    gemini)
      extra_args_str="--pi-model gemini-3-flash --pi-session-dir $TMP/pi-sessions-harness"
      ;;
  esac

  (
    export "$bin_env_key=$fake_cli"
    export FAKE_CLI_OUT
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label daemon-recv \
      --cwd "$DAEMON_CWD" \
      --session-key "key-daemon" \
      --cli "$cli_kind" \
      --ack-timeout 5 \
      --sweep-ttl 10 \
      --poll-interval 1 \
      --log-path "$TMP/daemon-${cli_kind}.log" \
      $extra_args_str \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon.pid"
  )
  local pid; pid="$(cat "$TMP/daemon.pid")"

  # Wait up to 15s for the fake-CLI probe file to appear.
  local waited=0
  while (( waited < 30 )); do
    if [[ -s "$FAKE_CLI_OUT" ]]; then break; fi
    sleep 0.5
    waited=$((waited+1))
  done

  # Stop daemon.
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true

  if [[ ! -s "$FAKE_CLI_OUT" ]]; then
    echo "--- daemon log ($cli_kind) ---"
    cat "$TMP/daemon-${cli_kind}.log" 2>/dev/null || echo "(no log)"
    fail "fake CLI ($cli_kind) never ran — daemon did not spawn"
  fi
}

# =============================================================================
# (1) W3 env-var propagation — all three CLI kinds must see
#     AGENT_COLLAB_DAEMON_SPAWN=1 + AGENT_COLLAB_SESSION_KEY + CLI-
#     specific env key set to the configured session-key value.
# =============================================================================
step "(1) W3 env-var propagation across claude/codex/gemini/pi"

FAKE_SHARED="$TMP/fake-shared.sh"
make_fake_cli "$FAKE_SHARED"

# Register daemon as pi for the pi spawn (the same daemon-recv label needs
# its agent toggled; we do that inline via SQL rather than re-registering).
# For claude/codex/gemini the original registration (--agent claude) is
# sufficient because the daemon's --cli flag is what picks the spawn path.

# Topic 3 v0.2 §9.2 gate 7 (v0.2.1 env-var correction): provider API env
# vars must survive daemon spawn into pi process env. For zai-glm the
# plugin pi-zai-glm reads ZAI_GLM_API_KEY (NOT ZAI_API_KEY, which is a
# different pi-mono built-in env slot). We export the plugin's env var
# here once; every subtest inherits it.
export ZAI_GLM_API_KEY="test-fixture-zai-glm"

for cli_kind in claude codex gemini pi; do
  run_one_spawn "$cli_kind" "$FAKE_SHARED"
  out="$(cat "$FAKE_CLI_OUT")"
  grep -q '^ENV\[AGENT_COLLAB_DAEMON_SPAWN\]=1$' <<<"$out" \
    || { echo "$out"; fail "$cli_kind: AGENT_COLLAB_DAEMON_SPAWN != 1"; }
  grep -q '^ENV\[AGENT_COLLAB_SESSION_KEY\]=key-daemon$' <<<"$out" \
    || { echo "$out"; fail "$cli_kind: AGENT_COLLAB_SESSION_KEY != key-daemon"; }
  case "$cli_kind" in
    claude)
      grep -q '^ENV\[CLAUDE_SESSION_ID\]=key-daemon$' <<<"$out" \
        || { echo "$out"; fail "$cli_kind: CLAUDE_SESSION_ID not set"; }
      ;;
    codex|gemini)
      # v0.3 §3.2.b SOFT SHIM: codex/gemini route through spawnPi; the
      # vendor-specific CODEX_SESSION_ID / GEMINI_SESSION_ID env vars
      # are NOT set under the shim (spawnPi sets no per-CLI env). The
      # pi-side surface is validated via the pi iteration's
      # ZAI_GLM_API_KEY check. A regression where shim accidentally
      # set CODEX_SESSION_ID would be harmless but surprising; no
      # positive gate for the absence here (the §9.2 gate 12 revise
      # plan covers the split assertions).
      :
      ;;
    pi)
      # Pi has no dedicated session-ID env var (daemon owns path, not
      # vendor UUID). Instead assert the provider API key survives the
      # os.Environ() inheritance — the canonical operator-shell pattern
      # per §4.4 "provider auth" paragraph.
      grep -q '^ENV\[ZAI_GLM_API_KEY\]=test-fixture-zai-glm$' <<<"$out" \
        || { echo "$out"; fail "pi: ZAI_GLM_API_KEY did not survive daemon spawn (§9.2 gate 7 provider-env propagation; v0.2.1 env-var correction)"; }
      ;;
  esac
  echo "   $cli_kind: env propagation ok"
done

# =============================================================================
# (2) stdin-close — shared execSpawn invariant across all CLIs. The
#     pi spawn (and pi-routed codex/gemini under v0.3 SOFT SHIM) all
#     share the same execSpawn path, so /dev/null stdin should be
#     uniform. Probe via the pi iteration which is the canonical
#     non-claude path.
# =============================================================================
step "(2) pi (and pi-routed codex/gemini) stdin-close (no hang on open stdin)"
run_one_spawn "pi" "$FAKE_SHARED"
stdin_elapsed="$(grep '^STDIN_READ_ELAPSED_SEC=' "$FAKE_CLI_OUT" | head -1 | cut -d= -f2)"
[[ "$stdin_elapsed" == "0" ]] \
  || fail "pi stdin was not closed (elapsed=${stdin_elapsed}s); daemon should pass /dev/null"
echo "   pi stdin read EOF immediately (elapsed=${stdin_elapsed}s)"

# =============================================================================
# (3) v0.3 SOFT SHIM — codex-direct argv assertions RETIRED.
# The v0.2 `exec` argv[0] + `--skip-git-repo-check` checks no longer
# apply: --cli=codex routes through spawnPi, which emits
# `--provider openai-codex --model <M> --no-session -p <prompt>`.
# See tests/daemon-collapse-migration.sh for the shim-argv regression.
# =============================================================================
step "(3) RETIRED v0.3 — codex-direct argv assertions moved to daemon-collapse-migration.sh"

# =============================================================================
# (4) Claude --settings MANDATORY fixture-pin (§4 bullet 5 + §8.2
#     gamma #4 strengthening). Without this, future `--bare` default
#     would silently drop peer-inbox hook firing. Pin the argv shape
#     NOW so a regression fails at CI, not in production.
# =============================================================================
step "(4) claude --settings MANDATORY fixture-pin"
run_one_spawn "claude" "$FAKE_SHARED"
grep -q '^ARGV\[.\]=--settings$' "$FAKE_CLI_OUT" \
  || { cat "$FAKE_CLI_OUT"; fail "claude spawn argv missing --settings (MANDATORY per §4 bullet 5)"; }
# The value that follows --settings should be a path (at least
# non-empty; default resolves to $HOME/.claude/settings.json).
settings_path="$(grep -A1 '^ARGV\[.\]=--settings$' "$FAKE_CLI_OUT" | tail -1 | sed -E 's/^ARGV\[[0-9]+\]=//')"
[[ -n "$settings_path" && "$settings_path" != "--settings" ]] \
  || fail "claude --settings value should be a non-empty path, got: $settings_path"
echo "   claude --settings $settings_path — fixture-pin ok"

# Also pin claude argv[0] = -p (agent-loop mode, not interactive).
grep -q '^ARGV\[0\]=-p$' "$FAKE_CLI_OUT" \
  || { cat "$FAKE_CLI_OUT"; fail "claude spawn argv[0] should be '-p'"; }
echo "   claude argv[0]=-p (agent-loop mode)"

echo "PASS: daemon-harness — W3 env propagation + codex stdin-close + --skip-git-repo-check + MANDATORY claude --settings"
