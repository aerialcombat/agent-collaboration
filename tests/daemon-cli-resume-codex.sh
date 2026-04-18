#!/usr/bin/env bash
# tests/daemon-cli-resume-codex.sh — Topic 3 v0.1 Arch D §9.3 gate 4:
# codex CLI-native session-resume end-to-end verification.
#
# Exercises spawnCodex's resume wiring (sub-task B of commit 3) via a
# fake codex stub. Four subtests:
#
#   (4a) First spawn captures session-ID from stdout banner; persisted
#        to sessions.daemon_cli_session_id. Second spawn (new daemon
#        invocation) uses argv `codex exec resume --skip-git-repo-check
#        <captured-UUID> <prompt>`. Then daemon-reset-session; third
#        spawn's argv has NO `resume` subcommand (post-reset fresh).
#   (4b) Stale-UUID-fallback: pre-populate column with a bogus UUID;
#        fake codex emits "session not found" to stdout + exits 1 when
#        resume is passed. Assert daemon clears column on the stale
#        detection; next spawn re-captures via fresh banner (§3.4
#        invariant 5).
#   (4c) Mix-mode flag-is-the-gate: --cli-session-resume flag absent
#        (defaults false) + column non-NULL in DB → spawn argv MUST NOT
#        contain `resume` subcommand. Flag is authoritative; column is
#        just persistence. Locks §9.3 gate 4c defensive assertion.
#   (4d) 2-label cross-isolation: two daemon labels in same DB
#        (reviewer-codex-a, reviewer-codex-b). Each captures its own
#        session-ID. Reset on A leaves B's column untouched. Assert
#        each spawn uses its own UUID.
#
# Pattern: mirrors tests/daemon-harness.sh fake-CLI approach. The fake
# codex stub writes its argv to a per-invocation file so we can inspect
# the exact resume flags the daemon passed. Stub emits the codex-0.121.0
# style banner with a templated session-ID drawn from an env var.
#
# §3.4 invariant 1 is honored by the daemon-spawn path itself: the
# prompt is always passed, regardless of resume. Subtest (4a) proof:
# the argv dumps include the prompt as the final arg both with and
# without `resume`.

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
  # Best-effort: kill any daemon lingering via our pidfile.
  if [[ -f "$TMP/daemon.pid" ]]; then
    local p; p="$(cat "$TMP/daemon.pid" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  fi
  rm -rf "$TMP"
}
# KEEP_TMP=1 env var preserves the scratch dir for post-mortem
# inspection (inspect $TMP/daemon-*.log, $TMP/out-*.txt). Default is
# cleanup-on-exit.
if [[ -z "${KEEP_TMP:-}" ]]; then
  trap cleanup EXIT
else
  trap 'echo "KEEP_TMP set: preserving TMP=$TMP"' EXIT
fi

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-cli-resume-codex: $*"; }

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

# Tight TTLs, preserve 2x ratio (§3.4 (c)).
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

DAEMON_CWD="$TMP/daemon-codex"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

# ----------------------------------------------------------------------
# Register sessions: one daemon-mode codex recipient + one sender.
# ----------------------------------------------------------------------
step "register daemon codex recipient + interactive sender"
AGENT_COLLAB_SESSION_KEY="key-codex" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-codex \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register daemon-codex failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"

# ----------------------------------------------------------------------
# Fake codex stub. Writes argv to $FAKE_CLI_OUT. Emits a codex-0.121.0
# style banner to stdout with session-id = $FAKE_CODEX_EMIT_UUID.
#
# If the stub is invoked with `resume <UUID>` in argv AND
# $FAKE_CODEX_STALE_UUID is set AND matches that UUID, stub emits
# "session not found" to stdout and exits 1 (simulates CLI-side
# session-store eviction). Otherwise emits the normal banner +
# JSONL ack marker + exits 0.
# ----------------------------------------------------------------------
make_fake_codex() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
# Fake codex stub for tests/daemon-cli-resume-codex.sh.
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"
UUID="${FAKE_CODEX_EMIT_UUID:-00000000-0000-0000-0000-000000000000}"
STALE="${FAKE_CODEX_STALE_UUID:-}"

# Dump argv for inspection. Overwrite prior invocation so the latest
# spawn's argv is what the test sees.
{
  echo "ARGV_COUNT=$#"
  i=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    i=$((i+1))
  done
} > "$OUT"

# Scan argv for a `resume ... <STALE>` pattern anywhere in the tail.
# Per §4.1 the daemon's resume argv is
#   exec resume --skip-git-repo-check <UUID> <prompt>
# so a strict `prev==resume && a==UUID` check would miss. Check
# instead: "resume" token appears somewhere AND the stale UUID
# appears somewhere.
is_stale=0
if [[ -n "$STALE" ]]; then
  has_resume=0
  has_stale=0
  for a in "$@"; do
    [[ "$a" == "resume" ]] && has_resume=1
    [[ "$a" == "$STALE" ]] && has_stale=1
  done
  if (( has_resume && has_stale )); then
    is_stale=1
  fi
fi

if (( is_stale )); then
  # v0.1.2 fix mirror: real codex 0.121.0 emits banner + diagnostics
  # (including "session not found") to STDERR. Daemon now captures BOTH
  # streams via execSpawn's io.MultiWriter+errBuf and concatenates them
  # for the regex scan in postSpawnCodexResumeHandling.
  echo "Reading additional input from stdin..." >&2
  echo "OpenAI Codex v0.121.0 (research preview)" >&2
  echo "error: session not found: $STALE" >&2
  exit 1
fi

# Normal path: emit codex-0.121.0 style banner to STDERR (matches real
# codex output stream — v0.1.2 fix). JSONL ack marker stays on STDOUT
# (it's the agent's mechanism-2 fallback ack, not codex CLI output).
echo "Reading additional input from stdin..." >&2
echo "OpenAI Codex v0.121.0 (research preview)" >&2
echo "--------" >&2
echo "workdir: /private/tmp/fake-codex" >&2
echo "model: gpt-5.4" >&2
echo "provider: openai" >&2
echo "approval: never" >&2
echo "sandbox: read-only" >&2
echo "reasoning effort: xhigh" >&2
echo "reasoning summaries: none" >&2
echo "session id: $UUID" >&2
echo "--------" >&2
echo 'user' >&2
echo 'codex' >&2
echo 'tokens used' >&2
echo '{"peer_inbox_ack": true}'
exit 0
EOF
  chmod +x "$path"
}

FAKE_CODEX="$TMP/fake-codex.sh"
make_fake_codex "$FAKE_CODEX"

seed_one() {
  local label="$1" cwd="$2" tag="$3"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to "$label" --to-cwd "$cwd" \
      --message "resume-probe: $tag" >/dev/null
}

# Reset in-flight / completed state for a label so the next daemon
# invocation claims fresh rows.
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

# Read sessions.daemon_cli_session_id for (cwd, label). Prints empty
# string when NULL. Matches by label only — tests use unique labels, and
# macOS symlink-resolution (/var → /private/var) means the daemon
# canonicalizes cwd differently from what the shell sees. Label-unique
# lookup is robust against that.
read_cli_session_id() {
  local cwd="$1" label="$2"
  python3 - "$DB" "$label" <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
r = c.execute("SELECT daemon_cli_session_id FROM sessions WHERE label = ?", (label,)).fetchone()
c.close()
print("" if (r is None or r[0] is None) else r[0])
PY
}

# Directly write a UUID into sessions.daemon_cli_session_id (test-only
# pre-populate path for subtests 4b + 4c + 4d). Matches by label only
# per the symlink note above.
set_cli_session_id() {
  local cwd="$1" label="$2" uuid="$3"
  python3 - "$DB" "$label" "$uuid" <<'PY'
import sqlite3, sys
db, label, uuid = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
c.execute("UPDATE sessions SET daemon_cli_session_id = ? WHERE label = ?", (uuid, label))
c.commit()
c.close()
PY
}

# Run a daemon briefly: start, wait for fake-CLI probe file, kill, return.
# Args: label, cwd, fake-cli-out-path, fake-cli-uuid-to-emit,
#       flag-cli-session-resume ("1"|"0"), stale-uuid (optional, "" for
#       none).
run_daemon_briefly() {
  local label="$1" cwd="$2" outpath="$3" emit_uuid="$4" flag="$5" stale="${6:-}"

  rm -f "$outpath"
  reset_inbox "$label"
  seed_one "$label" "$cwd" "$label-$(date +%s%N)"

  # set -u + empty bash arrays is fragile on macOS bash 3.2; build
  # the flag-args as a space-separated string we can splat safely.
  local flag_str=""
  if [[ "$flag" == "1" ]]; then
    flag_str="--cli-session-resume"
  fi

  (
    export AGENT_COLLAB_DAEMON_CODEX_BIN="$FAKE_CODEX"
    export FAKE_CLI_OUT="$outpath"
    export FAKE_CODEX_EMIT_UUID="$emit_uuid"
    export FAKE_CODEX_STALE_UUID="$stale"
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "key-codex-$label" \
      --cli codex \
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

  # Give the post-spawn capture handler a beat to finish its SQL write
  # before we kill the daemon — SetDaemonCLISessionID runs after the
  # fake stub exits but before the daemon re-enters its poll loop.
  sleep 0.4

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true

  if [[ ! -s "$outpath" ]]; then
    echo "--- daemon log (${label}) ---"
    cat "$TMP/daemon-${label}.log" 2>/dev/null || echo "(no log)"
    fail "fake codex never ran for label=$label — daemon did not spawn"
  fi
}

assert_argv_contains_resume() {
  local out="$1" uuid="$2"
  grep -q '^ARGV\[0\]=exec$' "$out" \
    || { cat "$out"; fail "expected ARGV[0]=exec"; }
  grep -q '^ARGV\[1\]=resume$' "$out" \
    || { cat "$out"; fail "expected ARGV[1]=resume (resume form per §4.1)"; }
  grep -qE "^ARGV\[[0-9]+\]=${uuid}$" "$out" \
    || { cat "$out"; fail "expected UUID=$uuid in argv"; }
}

assert_argv_no_resume() {
  local out="$1"
  grep -q '^ARGV\[0\]=exec$' "$out" \
    || { cat "$out"; fail "expected ARGV[0]=exec"; }
  if grep -q '^ARGV\[1\]=resume$' "$out"; then
    cat "$out"
    fail "expected NO resume subcommand in argv; got ARGV[1]=resume"
  fi
}

# =====================================================================
# (4a) First-capture + resume + reset + fresh.
# =====================================================================
step "(4a) first spawn captures, second spawn resumes, reset, third spawn fresh"

UUID_A="11111111-aaaa-4aaa-8aaa-111111111111"

OUT1="$TMP/out-4a-1.txt"
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT1" "$UUID_A" "1"
# First spawn: no cached UUID; argv is the resume-disabled form.
assert_argv_no_resume "$OUT1"
cap="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ "$cap" == "$UUID_A" ]] \
  || fail "(4a-first) expected captured UUID=$UUID_A in DB, got '$cap'"
echo "   (4a-first) captured UUID=$UUID_A from stdout banner"

OUT2="$TMP/out-4a-2.txt"
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT2" "$UUID_A" "1"
# Second spawn: cached UUID present; argv is the resume form.
assert_argv_contains_resume "$OUT2" "$UUID_A"
# --skip-git-repo-check must still appear (preserved under resume).
grep -q '^ARGV\[.\]=--skip-git-repo-check$' "$OUT2" \
  || { cat "$OUT2"; fail "(4a-second) --skip-git-repo-check missing under resume"; }
# §3.4 invariant 1: prompt still passed as final arg (load-bearing).
last_idx="$(($(grep -c '^ARGV\[' "$OUT2") - 1))"
grep -q "^ARGV\[${last_idx}\]=" "$OUT2" \
  || { cat "$OUT2"; fail "(4a-second) prompt-like final arg missing"; }
echo "   (4a-second) argv = exec resume --skip-git-repo-check $UUID_A <prompt>"

# daemon-reset-session between batches.
"$PI" daemon-reset-session --cwd "$DAEMON_CWD" --as daemon-codex --format plain \
  >/dev/null 2>&1 \
  || fail "(4a) daemon-reset-session failed"
cap_post_reset="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ -z "$cap_post_reset" ]] \
  || fail "(4a) column should be NULL after reset, got '$cap_post_reset'"

# Third spawn: no cached UUID; fresh form again. Use a different emit
# UUID to verify re-capture works.
UUID_A2="22222222-bbbb-4bbb-8bbb-222222222222"
OUT3="$TMP/out-4a-3.txt"
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT3" "$UUID_A2" "1"
assert_argv_no_resume "$OUT3"
cap3="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ "$cap3" == "$UUID_A2" ]] \
  || fail "(4a-third) expected re-capture UUID=$UUID_A2, got '$cap3'"
echo "   (4a-third) post-reset re-capture = $UUID_A2"

# =====================================================================
# (4b) Stale-UUID-fallback.
# =====================================================================
step "(4b) stale-UUID: daemon clears column on 'session not found' + next re-captures"

UUID_STALE="deadbeef-dead-4ead-8ead-deadbeefdead"
UUID_FRESH="33333333-cccc-4ccc-8ccc-333333333333"

# Pre-populate with bogus UUID the fake will reject.
set_cli_session_id "$DAEMON_CWD" "daemon-codex" "$UUID_STALE"

OUT_STALE="$TMP/out-4b-stale.txt"
# Daemon will pass UUID_STALE in resume argv. Fake stub sees it matches
# FAKE_CODEX_STALE_UUID, emits "session not found" + exits 1. Daemon's
# postSpawnCodexResumeHandling detects the stale pattern and clears the
# column.
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT_STALE" "$UUID_FRESH" "1" "$UUID_STALE"
# Verify daemon DID pass the stale UUID in argv (resume form).
assert_argv_contains_resume "$OUT_STALE" "$UUID_STALE"
# Column should be NULL after stale-detection.
cap_post_stale="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ -z "$cap_post_stale" ]] \
  || fail "(4b) column should be cleared after stale-detection, got '$cap_post_stale'"
# Verify daemon log contains the stale-uuid warning event.
grep -q 'daemon.cli_session_capture.codex_stale_uuid' "$TMP/daemon-daemon-codex.log" \
  || { cat "$TMP/daemon-daemon-codex.log"; fail "(4b) expected codex_stale_uuid warning in daemon log"; }
echo "   (4b) daemon cleared column after stale-detection"

# Next batch: daemon has no cached UUID → fresh spawn → re-capture.
OUT_FRESH="$TMP/out-4b-fresh.txt"
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT_FRESH" "$UUID_FRESH" "1"
assert_argv_no_resume "$OUT_FRESH"
cap_fresh="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ "$cap_fresh" == "$UUID_FRESH" ]] \
  || fail "(4b) expected re-capture UUID=$UUID_FRESH, got '$cap_fresh'"
echo "   (4b) next batch re-captured $UUID_FRESH from fresh banner"

# =====================================================================
# (4c) Mix-mode flag-is-the-gate: flag=false + column non-NULL →
#      NO resume in argv.
# =====================================================================
step "(4c) flag=false + column non-NULL → spawn argv has NO resume"

UUID_COL="44444444-dddd-4ddd-8ddd-444444444444"
set_cli_session_id "$DAEMON_CWD" "daemon-codex" "$UUID_COL"

OUT_MIX="$TMP/out-4c.txt"
# Flag absent ("0" → no --cli-session-resume flag passed).
run_daemon_briefly "daemon-codex" "$DAEMON_CWD" "$OUT_MIX" "$UUID_COL" "0"
assert_argv_no_resume "$OUT_MIX"
# Column must remain unchanged — flag=false means daemon neither reads
# nor writes the column.
cap_mix="$(read_cli_session_id "$DAEMON_CWD" "daemon-codex")"
[[ "$cap_mix" == "$UUID_COL" ]] \
  || fail "(4c) column should be unchanged when flag=false, got '$cap_mix' expected '$UUID_COL'"
echo "   (4c) flag=false is authoritative; column ignored; argv has no resume"

# Cleanup column for subsequent subtest.
set_cli_session_id "$DAEMON_CWD" "daemon-codex" ""
python3 - "$DB" "$DAEMON_CWD" "daemon-codex" <<'PY'
import sqlite3, sys
db, cwd, label = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
c.execute("UPDATE sessions SET daemon_cli_session_id = NULL WHERE cwd = ? AND label = ?", (cwd, label))
c.commit()
c.close()
PY

# =====================================================================
# (4d) 2-label cross-isolation: two daemon labels; each captures its
#      own UUID; reset on one does not affect the other.
# =====================================================================
step "(4d) 2-label cross-isolation"

DAEMON_CWD_A="$TMP/daemon-codex-a"
DAEMON_CWD_B="$TMP/daemon-codex-b"
mkdir -p "$DAEMON_CWD_A" "$DAEMON_CWD_B"

AGENT_COLLAB_SESSION_KEY="key-codex-a" \
  "$AC" session register --cwd "$DAEMON_CWD_A" --label reviewer-codex-a \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "(4d) register reviewer-codex-a failed"
AGENT_COLLAB_SESSION_KEY="key-codex-b" \
  "$AC" session register --cwd "$DAEMON_CWD_B" --label reviewer-codex-b \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "(4d) register reviewer-codex-b failed"

UUID_AA="aaaa0000-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
UUID_BB="bbbb0000-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

OUT_A1="$TMP/out-4d-a1.txt"
run_daemon_briefly "reviewer-codex-a" "$DAEMON_CWD_A" "$OUT_A1" "$UUID_AA" "1"
OUT_B1="$TMP/out-4d-b1.txt"
run_daemon_briefly "reviewer-codex-b" "$DAEMON_CWD_B" "$OUT_B1" "$UUID_BB" "1"

cap_a="$(read_cli_session_id "$DAEMON_CWD_A" "reviewer-codex-a")"
cap_b="$(read_cli_session_id "$DAEMON_CWD_B" "reviewer-codex-b")"
[[ "$cap_a" == "$UUID_AA" ]] || fail "(4d) label-a captured '$cap_a', expected $UUID_AA"
[[ "$cap_b" == "$UUID_BB" ]] || fail "(4d) label-b captured '$cap_b', expected $UUID_BB"
echo "   (4d) each label captured its own UUID (a=$UUID_AA b=$UUID_BB)"

# Second spawn per label: each resumes its OWN UUID.
OUT_A2="$TMP/out-4d-a2.txt"
run_daemon_briefly "reviewer-codex-a" "$DAEMON_CWD_A" "$OUT_A2" "$UUID_AA" "1"
assert_argv_contains_resume "$OUT_A2" "$UUID_AA"
# Ensure A's argv does NOT contain B's UUID.
if grep -q "$UUID_BB" "$OUT_A2"; then
  cat "$OUT_A2"
  fail "(4d) label-a argv leaked label-b's UUID"
fi

OUT_B2="$TMP/out-4d-b2.txt"
run_daemon_briefly "reviewer-codex-b" "$DAEMON_CWD_B" "$OUT_B2" "$UUID_BB" "1"
assert_argv_contains_resume "$OUT_B2" "$UUID_BB"
if grep -q "$UUID_AA" "$OUT_B2"; then
  cat "$OUT_B2"
  fail "(4d) label-b argv leaked label-a's UUID"
fi
echo "   (4d) second-batch resume-argv per label uses its own UUID"

# Reset on A; B unaffected.
"$PI" daemon-reset-session --cwd "$DAEMON_CWD_A" --as reviewer-codex-a \
  --format plain >/dev/null 2>&1 \
  || fail "(4d) daemon-reset-session reviewer-codex-a failed"
cap_a_post="$(read_cli_session_id "$DAEMON_CWD_A" "reviewer-codex-a")"
cap_b_post="$(read_cli_session_id "$DAEMON_CWD_B" "reviewer-codex-b")"
[[ -z "$cap_a_post" ]] || fail "(4d) label-a should be NULL after reset, got '$cap_a_post'"
[[ "$cap_b_post" == "$UUID_BB" ]] \
  || fail "(4d) label-b should be unchanged by A reset, got '$cap_b_post'"
echo "   (4d) reset on A does not affect B (cross-isolation preserved)"

# =====================================================================
# (4e) Cross-CLI reset-isolation (Topic 3 v0.2 §9.2 gate D):
# Reset on a codex label MUST NOT invoke os.Remove — the pi-specific
# file-delete branch is gated on sessions.agent == 'pi'. Regression gate
# against a future refactor dropping the agent-check. Implementation:
# JSON-format reset output must NOT include the "deleted_file" field.
# =====================================================================
step "(4e) cross-CLI reset-isolation: codex reset leaves deleted_file unset"

# Pre-populate column with a bogus UUID so the reset has something to
# clear (matches real-world post-capture state).
set_cli_session_id "$DAEMON_CWD" "daemon-codex" "aaaaeeee-aaaa-4aaa-8aaa-aaaaaaaaeeee"

# Create a sentinel file in $DAEMON_CWD with the UUID as its name. If a
# future refactor accidentally drops the agent-gate and invokes
# os.Remove, the sentinel will be deleted (UUID string interpreted as a
# $CWD-relative path). Asserting sentinel survives = regression gate.
SENTINEL_FILE="$DAEMON_CWD/aaaaeeee-aaaa-4aaa-8aaa-aaaaaaaaeeee"
echo "sentinel-for-4e" > "$SENTINEL_FILE"

reset_json="$("$PI" daemon-reset-session --cwd "$DAEMON_CWD" --as daemon-codex --format json 2>&1)"
[[ -n "$reset_json" ]] || fail "(4e) reset verb emitted no output"

# deleted_file is omitempty on the Go side → should NOT appear in JSON
# for a non-pi agent.
if echo "$reset_json" | grep -q '"deleted_file"'; then
  fail "(4e) codex reset emitted deleted_file field (should be absent for non-pi agent): $reset_json"
fi

# Sentinel file MUST survive: regression against a future refactor
# dropping the sessions.agent == 'pi' gate.
[[ -f "$SENTINEL_FILE" ]] \
  || fail "(4e) sentinel file deleted — reset verb invoked os.Remove on codex UUID (agent-gate regression)"

echo "   (4e) reset emitted no deleted_file field; sentinel survives (agent-gate intact)"

echo "PASS: daemon-cli-resume-codex — 4a capture+resume+reset + 4b stale-fallback + 4c flag-is-gate + 4d 2-label cross-isolation + 4e cross-CLI reset-isolation"
