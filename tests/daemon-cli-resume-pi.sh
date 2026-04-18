#!/usr/bin/env bash
# tests/daemon-cli-resume-pi.sh — Topic 3 v0.2 §9.2 gate 1:
# pi CLI path-based session-resume end-to-end verification.
#
# Exercises spawnPi's path-as-identity wiring via a fake pi stub. pi is
# distinct from codex/gemini in that the daemon OWNS the session-file
# PATH (mints + persists), rather than translating an opaque vendor-
# minted UUID. See plans/v3.x-topic-3-v0.2-pi-scoping.md §§3-4.
#
# Subtests:
#   (8a-mint)      First spawn: column NULL → daemon mints
#                  $SESSION_DIR/$LABEL.jsonl, persists path, argv has
#                  --session <minted-path>.
#   (8a-reuse)     Second spawn: column non-NULL → same path reused.
#   (8a-reset)     daemon-reset-session → column NULL'd + file deleted
#                  from disk. Third spawn re-mints same deterministic
#                  path + new file.
#   (8a-stale-file) Pre-populate column; rm file out-of-band; next
#                  spawn passes cached path unchanged + pi creates file
#                  (§3.4 invariant 5 re-create-at-same-path).
#   (8a-resume-off) cli_session_resume=false → argv contains
#                  --no-session + does NOT contain --session.
#   (B-mix-mode)   flag=false + column non-NULL → --no-session (flag
#                  authoritative; column ignored). Parallel to codex 4c
#                  / gemini 5d.
#   (C-cross-iso)  2 labels with distinct providers → each gets its own
#                  --provider / path / file; reset on A leaves B intact.

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
else
  trap 'echo "KEEP_TMP set: preserving TMP=$TMP"' EXIT
fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-cli-resume-pi: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"
PI="$PROJECT_ROOT/go/bin/peer-inbox"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

DAEMON_CWD="$TMP/daemon-pi"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

PI_SESSION_DIR="$TMP/pi-sessions"
mkdir -p "$PI_SESSION_DIR"

step "register daemon pi recipient + sender"
AGENT_COLLAB_SESSION_KEY="key-pi" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-pi \
    --agent pi --receive-mode daemon >/dev/null \
  || fail "session-register daemon-pi failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"

# ----------------------------------------------------------------------
# Fake pi stub. Writes argv + $FAKE_PI_CAPTURE_MARKER to $FAKE_CLI_OUT;
# also appends a line to the --session file if passed (simulates pi's
# JSONL write). Emits a JSONL ack marker to stdout.
# ----------------------------------------------------------------------
make_fake_pi() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"

{
  echo "ARGV_COUNT=$#"
  i=0
  sess_path=""
  next_is_session=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    if (( next_is_session )); then
      sess_path="$a"
      next_is_session=0
    fi
    if [[ "$a" == "--session" ]]; then
      next_is_session=1
    fi
    i=$((i+1))
  done
  echo "SESSION_PATH=$sess_path"
} > "$OUT"

# If --session was passed, append a line to the session file (simulates
# pi's own JSONL write). Idempotent on file existence per §3.4 invariant
# 5 re-create-at-same-path.
if [[ -n "${sess_path:-}" ]]; then
  mkdir -p "$(dirname "$sess_path")"
  echo '{"turn":"fake-pi-probe"}' >> "$sess_path"
fi

# JSONL ack marker on stdout.
echo '{"peer_inbox_ack": true}'
exit 0
EOF
  chmod +x "$path"
}

FAKE_PI="$TMP/fake-pi.sh"
make_fake_pi "$FAKE_PI"

seed_one() {
  local label="$1" cwd="$2" tag="$3"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$label" --to-cwd "$cwd" \
      --message "pi-probe: $tag" >/dev/null
}

reset_inbox() {
  local label="$1"
  python3 - "$DB" "$label" <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
c.execute("UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = ?", (label,))
c.execute("UPDATE sessions SET daemon_state = 'open' WHERE label = ?", (label,))
c.commit()
c.close()
PY
}

read_cli_session_id() {
  local _cwd="$1" label="$2"
  python3 - "$DB" "$label" <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
r = c.execute("SELECT daemon_cli_session_id FROM sessions WHERE label = ?", (label,)).fetchone()
c.close()
print("" if (r is None or r[0] is None) else r[0])
PY
}

set_cli_session_id() {
  local _cwd="$1" label="$2" val="$3"
  python3 - "$DB" "$label" "$val" <<'PY'
import sqlite3, sys
db, label, val = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
if val == "":
    c.execute("UPDATE sessions SET daemon_cli_session_id = NULL WHERE label = ?", (label,))
else:
    c.execute("UPDATE sessions SET daemon_cli_session_id = ? WHERE label = ?", (val, label))
c.commit()
c.close()
PY
}

# Run a daemon briefly. Args: label, cwd, fake-cli-out-path,
# pi-session-dir, provider, model, flag-cli-session-resume ("1"|"0").
run_daemon_briefly() {
  local label="$1" cwd="$2" outpath="$3" session_dir="$4" provider="$5" model="$6" flag="$7"

  rm -f "$outpath"
  reset_inbox "$label"
  seed_one "$label" "$cwd" "$label-$(date +%s%N)"

  local flag_str=""
  if [[ "$flag" == "1" ]]; then
    flag_str="--cli-session-resume"
  fi

  (
    export AGENT_COLLAB_DAEMON_PI_BIN="$FAKE_PI"
    export FAKE_CLI_OUT="$outpath"
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "key-pi-$label" \
      --cli pi \
      --pi-provider "$provider" \
      --pi-model "$model" \
      --pi-session-dir "$session_dir" \
      --ack-timeout 2 \
      --sweep-ttl 5 \
      --poll-interval 1 \
      --log-path "$TMP/daemon-${label}.log" \
      $flag_str \
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
  sleep 0.4

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true

  if [[ ! -s "$outpath" ]]; then
    echo "--- daemon log (${label}) ---"
    cat "$TMP/daemon-${label}.log" 2>/dev/null || echo "(no log)"
    fail "fake pi never ran for label=$label"
  fi
}

assert_argv_has_session() {
  local out="$1" expected_path="$2"
  grep -q '^ARGV\[.*\]=--session$' "$out" \
    || { cat "$out"; fail "expected --session flag in argv"; }
  grep -q "^SESSION_PATH=${expected_path}$" "$out" \
    || { cat "$out"; fail "expected SESSION_PATH=${expected_path} (per §4.4)"; }
  if grep -q '^ARGV\[.*\]=--no-session$' "$out"; then
    cat "$out"
    fail "--no-session must NOT appear when --session is used"
  fi
}

assert_argv_no_session() {
  local out="$1"
  grep -q '^ARGV\[.*\]=--no-session$' "$out" \
    || { cat "$out"; fail "expected --no-session flag (resume-off form)"; }
  if grep -q '^ARGV\[.*\]=--session$' "$out"; then
    cat "$out"
    fail "--session must NOT appear when --no-session is in argv"
  fi
}

assert_argv_has_provider_model() {
  local out="$1" provider="$2" model="$3"
  grep -q "^ARGV\[.*\]=--provider$" "$out" \
    || { cat "$out"; fail "expected --provider flag"; }
  grep -q "^ARGV\[.*\]=${provider}$" "$out" \
    || { cat "$out"; fail "expected provider=${provider}"; }
  grep -q "^ARGV\[.*\]=--model$" "$out" \
    || { cat "$out"; fail "expected --model flag"; }
  grep -q "^ARGV\[.*\]=${model}$" "$out" \
    || { cat "$out"; fail "expected model=${model}"; }
}

EXPECTED_PATH="$PI_SESSION_DIR/daemon-pi.jsonl"

# =====================================================================
# (8a-mint) First spawn mints + persists path; file created.
# =====================================================================
step "(8a-mint) first spawn mints \$SESSION_DIR/\$LABEL.jsonl + persists + creates file"

rm -f "$EXPECTED_PATH"
OUT1="$TMP/out-mint.txt"
run_daemon_briefly "daemon-pi" "$DAEMON_CWD" "$OUT1" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "1"

assert_argv_has_session "$OUT1" "$EXPECTED_PATH"
assert_argv_has_provider_model "$OUT1" "zai-glm" "glm-4.6"
[[ -f "$EXPECTED_PATH" ]] || fail "(8a-mint) session file not created at $EXPECTED_PATH"
cached="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ "$cached" == "$EXPECTED_PATH" ]] \
  || fail "(8a-mint) column should be '$EXPECTED_PATH', got '$cached'"
echo "   (8a-mint) argv=--session $EXPECTED_PATH; file created; column persisted"

# =====================================================================
# (8a-reuse) Second spawn reuses cached path.
# =====================================================================
step "(8a-reuse) second spawn reuses cached path"

OUT2="$TMP/out-reuse.txt"
run_daemon_briefly "daemon-pi" "$DAEMON_CWD" "$OUT2" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "1"
assert_argv_has_session "$OUT2" "$EXPECTED_PATH"
cached2="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ "$cached2" == "$EXPECTED_PATH" ]] \
  || fail "(8a-reuse) column should be unchanged, got '$cached2'"
# Fake pi appends on each invocation; file should now have 2 lines.
line_count="$(wc -l < "$EXPECTED_PATH" | tr -d ' ')"
[[ "$line_count" == "2" ]] \
  || fail "(8a-reuse) expected 2 lines in session file, got $line_count"
echo "   (8a-reuse) same path reused; file now has 2 turns"

# =====================================================================
# (8a-reset) daemon-reset-session deletes file + NULLs column.
# =====================================================================
step "(8a-reset) daemon-reset-session deletes file + NULLs column"

reset_out="$("$PI" daemon-reset-session --cwd "$DAEMON_CWD" --as daemon-pi --format json 2>&1)"
[[ -n "$reset_out" ]] || fail "(8a-reset) reset verb emitted no output"
echo "$reset_out" | grep -q '"deleted_file"' \
  || fail "(8a-reset) expected 'deleted_file' field in JSON, got: $reset_out"
echo "$reset_out" | grep -q "\"deleted_file\":\"$EXPECTED_PATH\"" \
  || fail "(8a-reset) expected deleted_file=$EXPECTED_PATH in JSON, got: $reset_out"
[[ ! -f "$EXPECTED_PATH" ]] || fail "(8a-reset) session file still on disk after reset"
cached_post="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ -z "$cached_post" ]] || fail "(8a-reset) column should be NULL after reset, got '$cached_post'"
echo "   (8a-reset) file deleted + column NULL'd"

# Third spawn: re-mints SAME deterministic path.
OUT3="$TMP/out-after-reset.txt"
run_daemon_briefly "daemon-pi" "$DAEMON_CWD" "$OUT3" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "1"
assert_argv_has_session "$OUT3" "$EXPECTED_PATH"
[[ -f "$EXPECTED_PATH" ]] || fail "(8a-reset) third spawn should re-create file"
echo "   (8a-reset) third spawn re-mints at same deterministic path"

# =====================================================================
# (8a-stale-file) File missing at spawn time → pi creates it; no rewrite.
# =====================================================================
step "(8a-stale-file) out-of-band rm → next spawn creates file at same path"

rm -f "$EXPECTED_PATH"
# Column still set from previous mint.
cached_before="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ "$cached_before" == "$EXPECTED_PATH" ]] \
  || fail "(8a-stale-file) precondition: column should be '$EXPECTED_PATH', got '$cached_before'"

OUT4="$TMP/out-stale.txt"
run_daemon_briefly "daemon-pi" "$DAEMON_CWD" "$OUT4" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "1"
assert_argv_has_session "$OUT4" "$EXPECTED_PATH"
[[ -f "$EXPECTED_PATH" ]] || fail "(8a-stale-file) fake-pi should have re-created the file"
cached_after="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ "$cached_after" == "$EXPECTED_PATH" ]] \
  || fail "(8a-stale-file) column should NOT be rewritten, got '$cached_after'"
echo "   (8a-stale-file) deterministic path preserved; pi re-creates; no column rewrite"

# =====================================================================
# (8a-resume-off + B-mix-mode) flag=false → --no-session; column ignored.
# =====================================================================
step "(8a-resume-off + B-mix-mode) flag=false → --no-session + column ignored"

# Precondition: column is non-NULL from previous subtest. Asserting
# the B-mix-mode invariant (flag authoritative over column).
OUT5="$TMP/out-resume-off.txt"
run_daemon_briefly "daemon-pi" "$DAEMON_CWD" "$OUT5" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "0"
assert_argv_no_session "$OUT5"
assert_argv_has_provider_model "$OUT5" "zai-glm" "glm-4.6"
# Column unchanged — flag=false means daemon does not read nor write it.
cached_off="$(read_cli_session_id "$DAEMON_CWD" "daemon-pi")"
[[ "$cached_off" == "$EXPECTED_PATH" ]] \
  || fail "(B-mix-mode) column should be unchanged when flag=false, got '$cached_off'"
echo "   (B-mix-mode) flag=false authoritative; --no-session in argv; column ignored"

# =====================================================================
# (C-cross-iso) 2 labels with distinct providers.
# =====================================================================
step "(C-cross-iso) 2 labels with distinct providers"

CWD_A="$TMP/daemon-pi-a"
CWD_B="$TMP/daemon-pi-b"
mkdir -p "$CWD_A" "$CWD_B"

AGENT_COLLAB_SESSION_KEY="key-pi-a" \
  "$AC" session register --cwd "$CWD_A" --label reviewer-pi-a \
    --agent pi --receive-mode daemon >/dev/null \
  || fail "(C-cross-iso) register reviewer-pi-a failed"
AGENT_COLLAB_SESSION_KEY="key-pi-b" \
  "$AC" session register --cwd "$CWD_B" --label reviewer-pi-b \
    --agent pi --receive-mode daemon >/dev/null \
  || fail "(C-cross-iso) register reviewer-pi-b failed"

PATH_A="$PI_SESSION_DIR/reviewer-pi-a.jsonl"
PATH_B="$PI_SESSION_DIR/reviewer-pi-b.jsonl"
rm -f "$PATH_A" "$PATH_B"

OUT_A1="$TMP/out-cross-a1.txt"
run_daemon_briefly "reviewer-pi-a" "$CWD_A" "$OUT_A1" "$PI_SESSION_DIR" "zai-glm" "glm-4.6" "1"
OUT_B1="$TMP/out-cross-b1.txt"
run_daemon_briefly "reviewer-pi-b" "$CWD_B" "$OUT_B1" "$PI_SESSION_DIR" "openai-codex" "gpt-5.1" "1"

assert_argv_has_provider_model "$OUT_A1" "zai-glm" "glm-4.6"
assert_argv_has_provider_model "$OUT_B1" "openai-codex" "gpt-5.1"
assert_argv_has_session "$OUT_A1" "$PATH_A"
assert_argv_has_session "$OUT_B1" "$PATH_B"

cap_a="$(read_cli_session_id "$CWD_A" "reviewer-pi-a")"
cap_b="$(read_cli_session_id "$CWD_B" "reviewer-pi-b")"
[[ "$cap_a" == "$PATH_A" ]] || fail "(C-cross-iso) A column '$cap_a' != $PATH_A"
[[ "$cap_b" == "$PATH_B" ]] || fail "(C-cross-iso) B column '$cap_b' != $PATH_B"
[[ -f "$PATH_A" ]] || fail "(C-cross-iso) A file missing"
[[ -f "$PATH_B" ]] || fail "(C-cross-iso) B file missing"

# Reset on A; B unaffected.
"$PI" daemon-reset-session --cwd "$CWD_A" --as reviewer-pi-a --format plain \
  >/dev/null 2>&1 || fail "(C-cross-iso) reset A failed"
[[ ! -f "$PATH_A" ]] || fail "(C-cross-iso) A file should be deleted"
[[ -f "$PATH_B" ]] || fail "(C-cross-iso) B file should remain"
cap_a_post="$(read_cli_session_id "$CWD_A" "reviewer-pi-a")"
cap_b_post="$(read_cli_session_id "$CWD_B" "reviewer-pi-b")"
[[ -z "$cap_a_post" ]] || fail "(C-cross-iso) A column should be NULL, got '$cap_a_post'"
[[ "$cap_b_post" == "$PATH_B" ]] \
  || fail "(C-cross-iso) B column should be unchanged, got '$cap_b_post'"
echo "   (C-cross-iso) per-label provider + path + file isolation preserved across reset"

echo "PASS: daemon-cli-resume-pi — 8a(mint/reuse/reset/stale-file/resume-off) + B-mix-mode + C-cross-isolation"
