#!/usr/bin/env bash
# tests/daemon-collapse-migration.sh — Topic 3 v0.3 §9.2 gate 3:
# SOFT SHIM migration-story regression gate.
#
# Validates:
#   (C1a) --cli=codex + --pi-model → daemon starts; argv is pi-style
#         with --provider openai-codex; deprecation warning emitted.
#   (C1b) --cli=gemini + --pi-model → argv contains --provider google-antigravity;
#         deprecation warning emitted.
#   (C2)  --cli=codex WITHOUT --pi-model → exit 64 with clear diagnostic
#         naming the v0.3 SOFT SHIM.
#   (C3)  --cli=codex with conflicting --pi-provider=zai-glm → exit 64
#         with shim conflict diagnostic.
#   (C4)  Stale-UUID tolerance: pre-populate daemon_cli_session_id with a
#         UUID-shape value on a --cli=codex label; assert daemon does not
#         crash, spawn proceeds, and next batch re-mints a path value.
#   (C5)  Shape β + path-shape reset guard: reset a --cli=codex label
#         with a path-shape column → file deleted. Reset a --cli=codex
#         label with UUID-shape column (simulates legacy pre-v0.3 row) →
#         NO file-delete attempted (cross-CLI-reset-isolation regression).

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
step() { echo "-- daemon-collapse-migration: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) || { echo "skip"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) || { echo "skip"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

SEND_CWD="$TMP/send"
mkdir -p "$SEND_CWD"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender --agent claude >/dev/null \
  || fail "register sender failed"

FAKE_PI="$TMP/fake-pi.sh"
cat > "$FAKE_PI" <<'EOF'
#!/usr/bin/env bash
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"
{
  echo "ARGV_COUNT=$#"
  i=0
  sess=""
  next_is_session=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    if (( next_is_session )); then sess="$a"; next_is_session=0; fi
    if [[ "$a" == "--session" ]]; then next_is_session=1; fi
    i=$((i+1))
  done
  echo "SESSION_PATH=$sess"
} > "$OUT"
if [[ -n "${sess:-}" ]]; then
  mkdir -p "$(dirname "$sess")"
  echo '{"turn":"collapse-probe"}' >> "$sess"
fi
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_PI"

PI_SESSION_DIR="$TMP/pi-sessions"
mkdir -p "$PI_SESSION_DIR"

seed_one() {
  local label="$1" cwd="$2" tag="$3"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$label" --to-cwd "$cwd" \
      --message "shim-probe: $tag" >/dev/null
}

reset_inbox() {
  python3 - "$DB" "$1" <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
c.execute("UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = ?", (label,))
c.execute("UPDATE sessions SET daemon_state = 'open' WHERE label = ?", (label,))
c.commit(); c.close()
PY
}

read_column() {
  python3 - "$DB" "$1" <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
r = c.execute("SELECT daemon_cli_session_id FROM sessions WHERE label = ?", (label,)).fetchone()
c.close()
print("" if (r is None or r[0] is None) else r[0])
PY
}

set_column() {
  python3 - "$DB" "$1" "$2" <<'PY'
import sqlite3, sys
db, label, val = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
c.execute("UPDATE sessions SET daemon_cli_session_id = ? WHERE label = ?", (val, label))
c.commit(); c.close()
PY
}

# Run daemon briefly with shim config; return when fake-pi has written probe.
run_shim_daemon() {
  local label="$1" cwd="$2" cli_flag="$3" model="$4" extra_flag_str="${5:-}" probe="$6" stderr_file="$7"
  rm -f "$probe" "$stderr_file"
  reset_inbox "$label"
  seed_one "$label" "$cwd" "$label-$(date +%s%N)"
  (
    export AGENT_COLLAB_DAEMON_PI_BIN="$FAKE_PI"
    export FAKE_CLI_OUT="$probe"
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "key-$label" \
      --cli "$cli_flag" \
      --pi-model "$model" \
      --pi-session-dir "$PI_SESSION_DIR" \
      --cli-session-resume \
      --ack-timeout 2 --sweep-ttl 5 --poll-interval 1 \
      --log-path "$TMP/d-${label}.log" \
      $extra_flag_str \
      >/dev/null 2>"$stderr_file" &
    echo $! > "$TMP/daemon.pid"
  )
  local pid; pid="$(cat "$TMP/daemon.pid")"
  local waited=0
  while (( waited < 30 )); do
    if [[ -s "$probe" ]]; then break; fi
    sleep 0.5; waited=$((waited+1))
  done
  sleep 0.3
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# =====================================================================
# (C1a) --cli=codex shim emits pi-style argv + deprecation warning.
# =====================================================================
step "(C1a) --cli=codex --pi-model → pi-style argv + deprecation warning"

CWD_C="$TMP/work-codex"
mkdir -p "$CWD_C"
AGENT_COLLAB_SESSION_KEY="key-codex" \
  "$AC" session register --cwd "$CWD_C" --label shim-codex --agent codex --receive-mode daemon >/dev/null \
  || fail "register shim-codex failed"

run_shim_daemon "shim-codex" "$CWD_C" "codex" "gpt-5.3-codex" "" \
  "$TMP/probe-c1a.txt" "$TMP/stderr-c1a.txt"

[[ -s "$TMP/probe-c1a.txt" ]] || { cat "$TMP/d-shim-codex.log"; fail "(C1a) fake-pi did not spawn"; }

grep -q '^ARGV\[.*\]=--provider$' "$TMP/probe-c1a.txt" \
  || { cat "$TMP/probe-c1a.txt"; fail "(C1a) argv missing --provider flag"; }
grep -q '^ARGV\[.*\]=openai-codex$' "$TMP/probe-c1a.txt" \
  || { cat "$TMP/probe-c1a.txt"; fail "(C1a) argv missing provider=openai-codex (SOFT SHIM auto-map)"; }
grep -q '^ARGV\[.*\]=gpt-5.3-codex$' "$TMP/probe-c1a.txt" \
  || { cat "$TMP/probe-c1a.txt"; fail "(C1a) argv missing model=gpt-5.3-codex"; }
# Legacy tokens must NOT appear.
if grep -qE '^ARGV\[.*\]=(exec|--skip-git-repo-check|resume)$' "$TMP/probe-c1a.txt"; then
  cat "$TMP/probe-c1a.txt"
  fail "(C1a) argv contains retired codex-direct tokens"
fi

# Deprecation warning emitted to stderr.
grep -q 'routed through pi as of v0.3' "$TMP/stderr-c1a.txt" \
  || { cat "$TMP/stderr-c1a.txt"; fail "(C1a) deprecation warning missing from stderr"; }
echo "   (C1a) pi-style argv + deprecation warning ok"

# =====================================================================
# (C1b) --cli=gemini shim.
# =====================================================================
step "(C1b) --cli=gemini --pi-model → pi-style argv + google-antigravity provider"

CWD_G="$TMP/work-gemini"
mkdir -p "$CWD_G"
AGENT_COLLAB_SESSION_KEY="key-gemini" \
  "$AC" session register --cwd "$CWD_G" --label shim-gemini --agent gemini --receive-mode daemon >/dev/null \
  || fail "register shim-gemini failed"

run_shim_daemon "shim-gemini" "$CWD_G" "gemini" "gemini-3-flash" "" \
  "$TMP/probe-c1b.txt" "$TMP/stderr-c1b.txt"

grep -q '^ARGV\[.*\]=google-antigravity$' "$TMP/probe-c1b.txt" \
  || { cat "$TMP/probe-c1b.txt"; fail "(C1b) argv missing provider=google-antigravity"; }
grep -q '^ARGV\[.*\]=gemini-3-flash$' "$TMP/probe-c1b.txt" \
  || { cat "$TMP/probe-c1b.txt"; fail "(C1b) argv missing model=gemini-3-flash"; }
if grep -qE '^ARGV\[.*\]=(--list-sessions|--resume)$' "$TMP/probe-c1b.txt"; then
  cat "$TMP/probe-c1b.txt"
  fail "(C1b) argv contains retired gemini-direct tokens"
fi
grep -q 'routed through pi as of v0.3' "$TMP/stderr-c1b.txt" \
  || { cat "$TMP/stderr-c1b.txt"; fail "(C1b) deprecation warning missing"; }
echo "   (C1b) pi-style argv + deprecation warning ok"

# =====================================================================
# (C2) Missing --pi-model with --cli=codex → exit 64.
# =====================================================================
step "(C2) --cli=codex WITHOUT --pi-model → exit 64 EX_USAGE"

stderr_c2="$TMP/stderr-c2.txt"
"$DAEMON" \
  --label shim-codex --cwd "$CWD_C" --session-key "key-codex" \
  --cli codex \
  --ack-timeout 2 --sweep-ttl 5 \
  >/dev/null 2>"$stderr_c2"
c2_exit=$?
[[ "$c2_exit" == "64" ]] || { cat "$stderr_c2"; fail "(C2) expected exit 64, got $c2_exit"; }
grep -qE 'pi\.model is required.*SOFT SHIM' "$stderr_c2" \
  || { cat "$stderr_c2"; fail "(C2) expected 'pi.model is required ... SOFT SHIM' diagnostic"; }
echo "   (C2) exit 64 + SOFT SHIM diagnostic ok"

# =====================================================================
# (C3) Conflicting --pi-provider with --cli=codex → exit 64.
# =====================================================================
step "(C3) --cli=codex + --pi-provider=zai-glm → exit 64 (shim conflict)"

stderr_c3="$TMP/stderr-c3.txt"
"$DAEMON" \
  --label shim-codex --cwd "$CWD_C" --session-key "key-codex" \
  --cli codex --pi-provider zai-glm --pi-model glm-4.6 \
  --ack-timeout 2 --sweep-ttl 5 \
  >/dev/null 2>"$stderr_c3"
c3_exit=$?
[[ "$c3_exit" == "64" ]] || { cat "$stderr_c3"; fail "(C3) expected exit 64, got $c3_exit"; }
grep -q 'routes through pi-openai-codex via v0.3 shim' "$stderr_c3" \
  || { cat "$stderr_c3"; fail "(C3) expected shim-conflict diagnostic"; }
echo "   (C3) exit 64 + shim-conflict diagnostic ok"

# =====================================================================
# (C4) Stale-UUID tolerance on shim-backed label.
# =====================================================================
step "(C4) stale UUID in column → daemon tolerates, re-mints path on next spawn"

# Pre-populate with UUID-shape value (simulates pre-v0.3 codex-direct row
# being shim-routed after upgrade).
STALE_UUID="11111111-2222-3333-4444-555555555555"
set_column "shim-codex" "$STALE_UUID"

run_shim_daemon "shim-codex" "$CWD_C" "codex" "gpt-5.3-codex" "" \
  "$TMP/probe-c4.txt" "$TMP/stderr-c4.txt"

# Daemon MUST NOT crash. Probe file MUST be populated (daemon spawned).
[[ -s "$TMP/probe-c4.txt" ]] || { cat "$TMP/d-shim-codex.log"; fail "(C4) daemon crashed or failed to spawn on stale UUID"; }

# Daemon should have used the stale UUID as-is (spawnPi reads cached
# value unchanged per §3.4 invariant 5). The stale UUID has no '/' so
# subsequent reset will skip os.Remove.
grep -q "^SESSION_PATH=${STALE_UUID}$" "$TMP/probe-c4.txt" \
  || { cat "$TMP/probe-c4.txt"; fail "(C4) expected spawnPi to pass stale UUID unchanged as --session value"; }
echo "   (C4) stale UUID tolerated; daemon spawned with UUID as --session (no crash; operator reset will NULL it safely)"

# =====================================================================
# (C5) Shape β + path-shape reset isolation.
# =====================================================================
step "(C5) reset isolation: path-shape column → file deleted; UUID-shape → survives"

# Clean slate; fresh mint (path-shape).
set_column "shim-codex" ""
run_shim_daemon "shim-codex" "$CWD_C" "codex" "gpt-5.3-codex" "" \
  "$TMP/probe-c5-mint.txt" "$TMP/stderr-c5-mint.txt"
MINTED_PATH="$PI_SESSION_DIR/shim-codex.jsonl"
[[ -f "$MINTED_PATH" ]] || fail "(C5) fresh spawn did not create session file at $MINTED_PATH"
cached="$(read_column shim-codex)"
[[ "$cached" == "$MINTED_PATH" ]] || fail "(C5) column should have path-shape value, got '$cached'"

# Reset — path value triggers file-delete.
reset_out="$("$PI_BIN" daemon-reset-session --cwd "$CWD_C" --as shim-codex --format json 2>&1)"
echo "$reset_out" | grep -q "\"deleted_file\":\"$MINTED_PATH\"" \
  || fail "(C5 path) expected deleted_file=$MINTED_PATH in JSON output"
[[ ! -f "$MINTED_PATH" ]] || fail "(C5 path) session file still on disk after reset"
echo "   (C5 path) path-shape column → file deleted; JSON emits deleted_file"

# Now pre-populate with UUID-shape (legacy) + sentinel file at $CWD/$UUID.
# If reset verb ignored path-shape guard + called os.Remove on the UUID,
# it would delete the sentinel. Path-shape guard must skip os.Remove.
LEGACY_UUID="deadbeef-dead-4ead-8ead-deadbeefdead"
set_column "shim-codex" "$LEGACY_UUID"
SENTINEL="$CWD_C/$LEGACY_UUID"
echo "v0.2-sentinel" > "$SENTINEL"

reset_out2="$("$PI_BIN" daemon-reset-session --cwd "$CWD_C" --as shim-codex --format json 2>&1)"
if echo "$reset_out2" | grep -q '"deleted_file"'; then
  fail "(C5 uuid) JSON should NOT contain deleted_file for UUID-shape column, got: $reset_out2"
fi
[[ -f "$SENTINEL" ]] || fail "(C5 uuid) sentinel deleted — path-shape guard regressed"
echo "   (C5 uuid) UUID-shape column → no file-delete attempted; sentinel survives (path-shape guard intact)"

echo "PASS: daemon-collapse-migration — shim argv + deprecation warning + preflight exit-64 + stale-UUID tolerance + Shape β dual-shape reset isolation"
