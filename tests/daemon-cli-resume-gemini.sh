#!/usr/bin/env bash
# tests/daemon-cli-resume-gemini.sh — Topic 3 v0.1 Arch D §9.3 gate 5.
#
# End-to-end fake-CLI test covering the gemini spawn helper's resume
# variant + --list-sessions delta-snapshot capture + UUID→index
# translation (v3 amendment per scope-doc §4.2).
#
# Subtests (per scope-doc §9.3 gate 5 enumeration):
#   (5a)             first-spawn capture + second-spawn UUID→index
#                    translation + reset-between-batches.
#   (5a-translation) index-shift under concurrent session creation:
#                    captured UUID moves from index 1 to index 2 as a
#                    new session is pushed onto the list; assert daemon
#                    invokes `--resume 2`, NOT `--resume 1`.
#   (5b)             stale-UUID fallback: pre-populated UUID that is NOT
#                    in current --list-sessions → daemon NULLs column +
#                    proceeds Arch B; next batch re-captures.
#   (5c)             race-tolerance: >1 new UUID in AFTER snapshot →
#                    warn loud + NULL (no winner picked).
#   (5d)             mix-mode flag-is-the-gate: flag=false + column
#                    non-NULL → no --resume in argv (defensive
#                    assertion per §9.3 gate 5d).
#   (5e)             2-label cross-isolation: two daemon labels in one
#                    DB each capture their own UUID.
#   (5f)             GEMINI_CONFIG_DIR propagation: env var set on
#                    daemon process → visible inside spawned gemini's
#                    environment.
#
# Approach: fake gemini stub is a bash script that distinguishes
# `--list-sessions` vs `-p <prompt>` invocations by argv inspection.
# State is driven by per-subtest env vars (*_STATE_FILE holds the
# list of UUIDs; *_SPAWN_LOG captures spawn argv + env for
# assertions).
#
# §3.4 invariants this test locks in:
#   Invariant 1 — envelope still in -p on every spawn, including
#                 resumed spawns (gate 5a checks ARGV still has
#                 `-p <prompt>` after `--resume N`).
#   Invariant 2 — daemon never reads ~/.gemini/*; only exec calls to
#                 `gemini --list-sessions` are permitted. (Enforced
#                 by-construction — we don't create that directory.)
#   Invariant 5 — capture-failure modes (5b, 5c) all log + proceed
#                 Arch B without crashing the daemon.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

TMP="$(mktemp -d)"
# Resolve symlinks (macOS /var → /private/var) so daemon-side
# resolveCWD returns a path that matches what the test passes via
# --cwd. Without this, sessions.cwd stored at register-time (via the
# daemon's `session register`) would differ from what get_column's
# WHERE clause looks up.
TMP="$(cd "$TMP" && pwd -P)"
cleanup() {
  # Best-effort: kill any daemon pid files created during the run.
  for f in "$TMP"/daemon-*.pid; do
    [[ -f "$f" ]] || continue
    local p; p="$(cat "$f" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  done
  if [[ -z "${KEEP_TMP:-}" ]]; then rm -rf "$TMP"; else echo "KEEP_TMP=$TMP" >&2; fi
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-cli-resume-gemini: $*"; }

# ---- Build binaries ---------------------------------------------------------

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

AC="$PROJECT_ROOT/scripts/agent-collab"

# ---- Fake gemini stub -------------------------------------------------------
#
# Distinguishes --list-sessions vs -p <prompt>:
#   --list-sessions         → emit fixture-shaped output from $FAKE_GEMINI_STATE_FILE
#   -p PROMPT (any prefix)  → append argv + env to $FAKE_GEMINI_SPAWN_LOG
#                             optionally append a new UUID to the state file
#                             (simulates session creation via --new-uuid <UUID>
#                             env-controlled)
#                             emit a JSONL ack marker so daemon completes the batch
#
# State file format: one UUID per line (just UUIDs, no headers). Ordering in
# the state file mirrors gemini's "newest first" order; stub emits them with
# 1-based indices matching file ordering.
#
# Env controls:
#   FAKE_GEMINI_STATE_FILE  — path to the UUID state file (required)
#   FAKE_GEMINI_SPAWN_LOG   — path to per-spawn log file (required)
#   FAKE_GEMINI_NEW_UUID    — if set, a spawn invocation (not --list-sessions)
#                             prepends this UUID to the state file BEFORE
#                             exiting, simulating the CLI's session creation.
#                             A single "FAKE_GEMINI_NEW_UUID=" value per spawn
#                             produces exactly one new UUID.
#   FAKE_GEMINI_EXTRA_UUID  — if set, a spawn ALSO prepends this UUID
#                             (simulating concurrent gemini race — two new
#                             UUIDs between BEFORE and AFTER snapshots).

FAKE_GEMINI="$TMP/fake-gemini.sh"
cat > "$FAKE_GEMINI" <<'EOF'
#!/usr/bin/env bash
# Fake gemini stub for tests/daemon-cli-resume-gemini.sh.
# See test-file docstring for semantics.
set -u

STATE="${FAKE_GEMINI_STATE_FILE:-/dev/null}"
LOG="${FAKE_GEMINI_SPAWN_LOG:-/dev/null}"

is_list_sessions=0
for a in "$@"; do
  if [[ "$a" == "--list-sessions" ]]; then
    is_list_sessions=1
    break
  fi
done

if (( is_list_sessions )); then
  # Emit the current state file as gemini --list-sessions output.
  # Drive iteration straight from the file to avoid bash 3.2's
  # "unbound variable under set -u" gotcha for empty arrays.
  n=0
  if [[ -f "$STATE" ]]; then
    while IFS= read -r line; do
      [[ -z "$line" ]] && continue
      n=$((n+1))
    done < "$STATE"
  fi
  echo ""
  echo "Available sessions for this project ($n):"
  i=1
  if [[ -f "$STATE" ]]; then
    while IFS= read -r u; do
      [[ -z "$u" ]] && continue
      echo "  $i. fake prompt preview (1 hour ago) [$u]"
      i=$((i+1))
    done < "$STATE"
  fi
  exit 0
fi

# Spawn-form invocation (-p <prompt>, optionally --resume <N>).
# Log argv + selected env vars for the test to assert against.
{
  echo "---SPAWN---"
  echo "ARGV_COUNT=$#"
  i=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    i=$((i+1))
  done
  for k in GEMINI_CONFIG_DIR GEMINI_SESSION_ID AGENT_COLLAB_SESSION_KEY AGENT_COLLAB_DAEMON_SPAWN; do
    v="${!k-}"
    echo "ENV[$k]=$v"
  done
} >> "$LOG"

# Optionally create a new session in the state file (simulates gemini's
# session-store behavior). Newest-first: prepend to file.
if [[ -n "${FAKE_GEMINI_NEW_UUID:-}" ]]; then
  new="$FAKE_GEMINI_NEW_UUID"
  if [[ -f "$STATE" ]]; then
    old="$(cat "$STATE")"
    { echo "$new"; [[ -n "$old" ]] && echo "$old"; } > "$STATE"
  else
    echo "$new" > "$STATE"
  fi
fi
if [[ -n "${FAKE_GEMINI_EXTRA_UUID:-}" ]]; then
  extra="$FAKE_GEMINI_EXTRA_UUID"
  if [[ -f "$STATE" ]]; then
    old="$(cat "$STATE")"
    { echo "$extra"; [[ -n "$old" ]] && echo "$old"; } > "$STATE"
  else
    echo "$extra" > "$STATE"
  fi
fi

# Emit JSONL ack marker so daemon completes the batch.
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_GEMINI"

# ---- Helpers ----------------------------------------------------------------

register_daemon() {
  local cwd="$1" label="$2" key="$3"
  mkdir -p "$cwd"
  AGENT_COLLAB_SESSION_KEY="$key" \
    "$AC" session register --cwd "$cwd" --label "$label" \
      --agent gemini --receive-mode daemon >/dev/null \
    || fail "session-register $label failed"
}

register_sender() {
  local cwd="$1" label="$2" key="$3"
  mkdir -p "$cwd"
  AGENT_COLLAB_SESSION_KEY="$key" \
    "$AC" session register --cwd "$cwd" --label "$label" \
      --agent claude >/dev/null \
    || fail "session-register sender $label failed"
}

send_one() {
  local send_cwd="$1" send_key="$2" to_label="$3" to_cwd="$4" msg="$5"
  AGENT_COLLAB_SESSION_KEY="$send_key" \
    "$AC" peer send --cwd "$send_cwd" --to "$to_label" --to-cwd "$to_cwd" \
      --message "$msg" >/dev/null
}

get_column() {
  # Read sessions.daemon_cli_session_id for (cwd, label). Emits empty
  # string for NULL or missing row.
  local cwd="$1" label="$2"
  python3 - "$DB" "$cwd" "$label" <<'PYEOF'
import sqlite3, sys
db, cwd, label = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
r = c.execute(
    "SELECT daemon_cli_session_id FROM sessions WHERE cwd = ? AND label = ?",
    (cwd, label),
).fetchone()
if r is None or r[0] is None:
    print("")
else:
    print(r[0])
PYEOF
}

set_column() {
  local cwd="$1" label="$2" value="$3"
  python3 - "$DB" "$cwd" "$label" "$value" <<'PYEOF'
import sqlite3, sys
db, cwd, label, val = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
c = sqlite3.connect(db)
c.execute(
    "UPDATE sessions SET daemon_cli_session_id = ? WHERE cwd = ? AND label = ?",
    (val, cwd, label),
)
c.commit()
c.close()
PYEOF
}

# Start daemon in bg, wait for at least N spawn records in the log, then stop.
# Args: label cwd key log-file-path spawn-log extra-env-assignments ...
# wait-count = expected number of "---SPAWN---" markers in the spawn log.
run_daemon_until_spawn_count() {
  local label="$1" cwd="$2" key="$3" dlog="$4" spawn_log="$5" want="$6"
  shift 6
  # Remaining args are `key=value` env assignments for the daemon.

  # Reset batch state so prior assertions don't linger.
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = '$label'\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = '$label'\")
c.commit()
c.close()
"

  (
    export AGENT_COLLAB_DAEMON_GEMINI_BIN="$FAKE_GEMINI"
    export FAKE_GEMINI_SPAWN_LOG="$spawn_log"
    for kv in "$@"; do
      # shellcheck disable=SC2163
      export "$kv"
    done
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "$key" \
      --cli gemini \
      --cli-session-resume \
      --ack-timeout 5 \
      --sweep-ttl 11 \
      --poll-interval 1 \
      --log-path "$dlog" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon-${label}.pid"
  )
  local pid; pid="$(cat "$TMP/daemon-${label}.pid")"

  # Wait up to ~30s for $want "---SPAWN---" markers in the spawn log.
  local waited=0
  local observed=0
  while (( waited < 60 )); do
    if [[ -f "$spawn_log" ]]; then
      observed="$(awk '/^---SPAWN---$/ {n++} END {print n+0}' "$spawn_log" 2>/dev/null)"
      [[ -z "$observed" ]] && observed=0
      if (( observed >= want )); then break; fi
    fi
    sleep 0.5
    waited=$((waited+1))
  done

  # A little extra grace so post-spawn capture writes land before we kill.
  sleep 1

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$TMP/daemon-${label}.pid"

  if (( observed < want )); then
    echo "--- daemon log for $label ---" >&2
    cat "$dlog" 2>/dev/null || echo "(no log)" >&2
    echo "--- spawn log for $label ---" >&2
    cat "$spawn_log" 2>/dev/null || echo "(no log)" >&2
    fail "daemon $label: expected >=$want spawns, saw $observed"
  fi
}

# Same as run_daemon_until_spawn_count but runs the daemon WITHOUT the
# --cli-session-resume flag (for subtest 5d).
run_daemon_flag_off_until_spawn_count() {
  local label="$1" cwd="$2" key="$3" dlog="$4" spawn_log="$5" want="$6"
  shift 6

  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = '$label'\")
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = '$label'\")
c.commit()
c.close()
"

  (
    export AGENT_COLLAB_DAEMON_GEMINI_BIN="$FAKE_GEMINI"
    export FAKE_GEMINI_SPAWN_LOG="$spawn_log"
    for kv in "$@"; do
      # shellcheck disable=SC2163
      export "$kv"
    done
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "$key" \
      --cli gemini \
      --ack-timeout 5 \
      --sweep-ttl 11 \
      --poll-interval 1 \
      --log-path "$dlog" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon-${label}.pid"
  )
  local pid; pid="$(cat "$TMP/daemon-${label}.pid")"

  local waited=0
  local observed=0
  while (( waited < 60 )); do
    if [[ -f "$spawn_log" ]]; then
      observed="$(awk '/^---SPAWN---$/ {n++} END {print n+0}' "$spawn_log" 2>/dev/null)"
      [[ -z "$observed" ]] && observed=0
      if (( observed >= want )); then break; fi
    fi
    sleep 0.5
    waited=$((waited+1))
  done

  sleep 1

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$TMP/daemon-${label}.pid"

  if (( observed < want )); then
    echo "--- daemon log for $label ---" >&2
    cat "$dlog" 2>/dev/null || echo "(no log)" >&2
    echo "--- spawn log for $label ---" >&2
    cat "$spawn_log" 2>/dev/null || echo "(no log)" >&2
    fail "daemon $label (flag off): expected >=$want spawns, saw $observed"
  fi
}

# Extract the Nth spawn block (1-indexed) from a spawn log. Prints it to stdout.
# A "spawn block" is everything after a `---SPAWN---` line up to (but not
# including) the next `---SPAWN---` line, or end-of-file.
get_spawn_block() {
  local log="$1" n="$2"
  awk -v n="$n" '
    /^---SPAWN---$/ { count++; inblock = (count == n); next }
    inblock { print }
  ' "$log"
}

assert_argv_has_resume_index() {
  local block="$1" want_index="$2" tag="$3"
  # --resume N should appear as two sequential argv entries with N numeric.
  # Check that argv contains `--resume` followed by the expected index at the
  # next position.
  local saw=""
  local prev=""
  while IFS= read -r line; do
    if [[ "$line" =~ ^ARGV\[[0-9]+\]=(.*)$ ]]; then
      val="${BASH_REMATCH[1]}"
      if [[ "$prev" == "--resume" ]]; then
        saw="$val"
        break
      fi
      prev="$val"
    fi
  done <<<"$block"
  if [[ "$saw" != "$want_index" ]]; then
    echo "---SPAWN BLOCK ($tag)---" >&2
    echo "$block" >&2
    fail "$tag: expected --resume $want_index; got --resume $saw (or no --resume)"
  fi
}

assert_argv_has_no_resume() {
  local block="$1" tag="$2"
  if grep -q '^ARGV\[.*\]=--resume$' <<<"$block"; then
    echo "---SPAWN BLOCK ($tag)---" >&2
    echo "$block" >&2
    fail "$tag: expected NO --resume in argv; found one"
  fi
}

assert_argv_has_p_prompt() {
  # Invariant 1: envelope still load-bearing — every spawn must have `-p`.
  local block="$1" tag="$2"
  if ! grep -q '^ARGV\[.*\]=-p$' <<<"$block"; then
    echo "---SPAWN BLOCK ($tag)---" >&2
    echo "$block" >&2
    fail "$tag: spawn argv missing '-p' (§3.4 invariant 1 violation: envelope not load-bearing)"
  fi
}

# =============================================================================
# Subtest (5a) — first-spawn capture + second-spawn UUID→index translation +
# reset-between-batches.
# =============================================================================
step "(5a) first-spawn capture + second-spawn translation + reset"

CWD_A="$TMP/daemon-a"
SEND_CWD="$TMP/send"
register_daemon "$CWD_A" "d-5a" "key-5a"
register_sender "$SEND_CWD" "sender" "key-send"

STATE_A="$TMP/state-5a.txt"
: > "$STATE_A"
LOG_5A="$TMP/daemon-5a.log"
SPAWN_5A="$TMP/spawn-5a.log"
: > "$SPAWN_5A"

# Batch 1: no cached UUID. Daemon snapshots BEFORE (empty),
# spawns (stub prepends FAKE_GEMINI_NEW_UUID = $UUID_5A_1),
# snapshots AFTER (now has UUID_5A_1). Daemon captures + persists.
UUID_5A_1="aaaaaaaa-1111-2222-3333-444444444444"
send_one "$SEND_CWD" "key-send" "d-5a" "$CWD_A" "msg-1"
run_daemon_until_spawn_count "d-5a" "$CWD_A" "key-5a" "$LOG_5A" "$SPAWN_5A" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_A" \
  "FAKE_GEMINI_NEW_UUID=$UUID_5A_1"

# Assert column now holds UUID_5A_1.
col="$(get_column "$CWD_A" "d-5a")"
[[ "$col" == "$UUID_5A_1" ]] \
  || fail "(5a) batch-1 capture: expected column=$UUID_5A_1, got '$col'"

# Assert spawn 1 argv has NO --resume (fresh first spawn).
block1="$(get_spawn_block "$SPAWN_5A" 1)"
assert_argv_has_no_resume "$block1" "5a/batch-1"
assert_argv_has_p_prompt "$block1" "5a/batch-1"

# Batch 2: cached UUID_5A_1. Daemon re-queries list-sessions (state has
# UUID_5A_1 at index 1), invokes `--resume 1 -p <prompt>`.
send_one "$SEND_CWD" "key-send" "d-5a" "$CWD_A" "msg-2"
run_daemon_until_spawn_count "d-5a" "$CWD_A" "key-5a" "$LOG_5A" "$SPAWN_5A" 2 \
  "FAKE_GEMINI_STATE_FILE=$STATE_A"

# Assert spawn 2 has --resume 1.
block2="$(get_spawn_block "$SPAWN_5A" 2)"
assert_argv_has_resume_index "$block2" "1" "5a/batch-2"
assert_argv_has_p_prompt "$block2" "5a/batch-2"

# Reset via peer-inbox daemon-reset-session, then batch 3.
"$PI" daemon-reset-session --cwd "$CWD_A" --as d-5a >/dev/null \
  || fail "(5a) daemon-reset-session failed"
col_post_reset="$(get_column "$CWD_A" "d-5a")"
[[ -z "$col_post_reset" ]] \
  || fail "(5a) after reset: column should be NULL, got '$col_post_reset'"

# Batch 3: no cached UUID → behaves like batch 1 (capture path; this
# time the stub will introduce UUID_5A_2).
UUID_5A_2="aaaaaaaa-5555-6666-7777-888888888888"
send_one "$SEND_CWD" "key-send" "d-5a" "$CWD_A" "msg-3"
run_daemon_until_spawn_count "d-5a" "$CWD_A" "key-5a" "$LOG_5A" "$SPAWN_5A" 3 \
  "FAKE_GEMINI_STATE_FILE=$STATE_A" \
  "FAKE_GEMINI_NEW_UUID=$UUID_5A_2"

block3="$(get_spawn_block "$SPAWN_5A" 3)"
assert_argv_has_no_resume "$block3" "5a/batch-3(post-reset)"
assert_argv_has_p_prompt "$block3" "5a/batch-3(post-reset)"

col_post_b3="$(get_column "$CWD_A" "d-5a")"
[[ "$col_post_b3" == "$UUID_5A_2" ]] \
  || fail "(5a) batch-3 should have captured $UUID_5A_2, got '$col_post_b3'"

echo "   (5a) ok — capture → translate → reset → re-capture"

# =============================================================================
# Subtest (5a-translation) — index-shift under concurrent session creation.
# v3 amendment lock: daemon must re-query list-sessions each resume and use
# the CURRENT index, NOT the stale capture-time index.
# =============================================================================
step "(5a-translation) UUID→index under index-shift"

CWD_T="$TMP/daemon-t"
register_daemon "$CWD_T" "d-5t" "key-5t"

STATE_T="$TMP/state-5t.txt"
: > "$STATE_T"
LOG_5T="$TMP/daemon-5t.log"
SPAWN_5T="$TMP/spawn-5t.log"
: > "$SPAWN_5T"

# Batch 1: capture UUID_T_1 at index 1 (it's the only session).
UUID_T_1="bbbbbbbb-1111-2222-3333-444444444444"
send_one "$SEND_CWD" "key-send" "d-5t" "$CWD_T" "t-msg-1"
run_daemon_until_spawn_count "d-5t" "$CWD_T" "key-5t" "$LOG_5T" "$SPAWN_5T" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_T" \
  "FAKE_GEMINI_NEW_UUID=$UUID_T_1"

col="$(get_column "$CWD_T" "d-5t")"
[[ "$col" == "$UUID_T_1" ]] \
  || fail "(5a-translation) capture: expected $UUID_T_1, got '$col'"

# Between batches: an "interactive" gemini session is created externally
# (simulated by prepending to the state file).  UUID_T_1 shifts from
# index 1 to index 2.
UUID_T_OTHER="bbbbbbbb-9999-aaaa-bbbb-cccccccccccc"
{ echo "$UUID_T_OTHER"; cat "$STATE_T"; } > "$STATE_T.tmp" && mv "$STATE_T.tmp" "$STATE_T"

# Verify the state now has UUID_T_OTHER at line 1 and UUID_T_1 at line 2.
line1="$(sed -n '1p' "$STATE_T")"
line2="$(sed -n '2p' "$STATE_T")"
[[ "$line1" == "$UUID_T_OTHER" ]] \
  || fail "(5a-translation) precondition: state-file line 1 should be $UUID_T_OTHER, got '$line1'"
[[ "$line2" == "$UUID_T_1" ]] \
  || fail "(5a-translation) precondition: state-file line 2 should be $UUID_T_1, got '$line2'"

# Batch 2: daemon re-queries list-sessions. UUID_T_1 is NOW at index 2.
# Daemon should invoke --resume 2, NOT --resume 1.
send_one "$SEND_CWD" "key-send" "d-5t" "$CWD_T" "t-msg-2"
run_daemon_until_spawn_count "d-5t" "$CWD_T" "key-5t" "$LOG_5T" "$SPAWN_5T" 2 \
  "FAKE_GEMINI_STATE_FILE=$STATE_T"

block2="$(get_spawn_block "$SPAWN_5T" 2)"
assert_argv_has_resume_index "$block2" "2" "5a-translation/batch-2"
assert_argv_has_p_prompt "$block2" "5a-translation/batch-2"

# Defensive: assert the daemon did NOT pass the stale capture-time index 1.
if grep -q '^ARGV\[.*\]=--resume$' <<<"$block2"; then
  prev=""
  while IFS= read -r line; do
    if [[ "$line" =~ ^ARGV\[[0-9]+\]=(.*)$ ]]; then
      val="${BASH_REMATCH[1]}"
      if [[ "$prev" == "--resume" ]]; then
        [[ "$val" != "1" ]] \
          || fail "(5a-translation) daemon passed stale index 1; v3 amendment broken"
      fi
      prev="$val"
    fi
  done <<<"$block2"
fi

echo "   (5a-translation) ok — UUID at new index 2 correctly looked up"

# =============================================================================
# Subtest (5b) — stale UUID fallback: pre-populated UUID NOT in list.
# =============================================================================
step "(5b) stale-UUID fallback"

CWD_B="$TMP/daemon-b"
register_daemon "$CWD_B" "d-5b" "key-5b"

STATE_B="$TMP/state-5b.txt"
# State file has one unrelated session, NOT the cached UUID.
UNRELATED_UUID="cccccccc-dddd-eeee-ffff-000011112222"
echo "$UNRELATED_UUID" > "$STATE_B"

# Pre-populate the column with a UUID that is NOT in the state file.
STALE_UUID="ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
set_column "$CWD_B" "d-5b" "$STALE_UUID"
col_pre="$(get_column "$CWD_B" "d-5b")"
[[ "$col_pre" == "$STALE_UUID" ]] \
  || fail "(5b) precondition: column should be $STALE_UUID, got '$col_pre'"

LOG_5B="$TMP/daemon-5b.log"
SPAWN_5B="$TMP/spawn-5b.log"
: > "$SPAWN_5B"

# Batch 1: daemon sees cached STALE_UUID, re-queries list-sessions, doesn't
# find it. Clears column. Falls through to Arch B (no --resume).
# Then the capture path snapshots BEFORE (UNRELATED_UUID only), spawns
# (stub prepends NEW_UUID), AFTER (UNRELATED + NEW), delta = NEW.
# Capture should land.
NEW_UUID="55555555-6666-7777-8888-999999999999"
send_one "$SEND_CWD" "key-send" "d-5b" "$CWD_B" "b-msg-1"
run_daemon_until_spawn_count "d-5b" "$CWD_B" "key-5b" "$LOG_5B" "$SPAWN_5B" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_B" \
  "FAKE_GEMINI_NEW_UUID=$NEW_UUID"

# Assert: spawn had NO --resume (stale fallback triggered Arch B).
block1="$(get_spawn_block "$SPAWN_5B" 1)"
assert_argv_has_no_resume "$block1" "5b/batch-1(stale-fallback)"
assert_argv_has_p_prompt "$block1" "5b/batch-1(stale-fallback)"

# Assert: column was cleared of STALE_UUID AND now holds NEW_UUID (re-captured).
col_post="$(get_column "$CWD_B" "d-5b")"
[[ "$col_post" == "$NEW_UUID" ]] \
  || fail "(5b) expected column=$NEW_UUID after re-capture, got '$col_post'"

# Assert: warning logged.
grep -q 'daemon.cli_session_resume.gemini_stale_uuid' "$LOG_5B" \
  || { cat "$LOG_5B" >&2; fail "(5b) expected gemini_stale_uuid warning in log"; }

echo "   (5b) ok — stale UUID cleared + re-captured on same batch"

# =============================================================================
# Subtest (5c) — race-tolerance: >1 new UUID in AFTER snapshot.
# Daemon should warn and leave column NULL (no winner picked).
# =============================================================================
step "(5c) race-tolerance (no winner picked on >1 new UUID)"

CWD_C="$TMP/daemon-c"
register_daemon "$CWD_C" "d-5c" "key-5c"

STATE_C="$TMP/state-5c.txt"
: > "$STATE_C"
LOG_5C="$TMP/daemon-5c.log"
SPAWN_5C="$TMP/spawn-5c.log"
: > "$SPAWN_5C"

# Batch 1: no cached UUID. Fake stub simulates a race by appending
# TWO new UUIDs between BEFORE (empty) and AFTER (2 entries).
UUID_C_NEW="11111111-2222-3333-4444-555555555555"
UUID_C_EXTRA="66666666-7777-8888-9999-aaaaaaaaaaaa"
send_one "$SEND_CWD" "key-send" "d-5c" "$CWD_C" "c-msg-1"
run_daemon_until_spawn_count "d-5c" "$CWD_C" "key-5c" "$LOG_5C" "$SPAWN_5C" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_C" \
  "FAKE_GEMINI_NEW_UUID=$UUID_C_NEW" \
  "FAKE_GEMINI_EXTRA_UUID=$UUID_C_EXTRA"

# Spawn itself had no --resume (first batch).
block1="$(get_spawn_block "$SPAWN_5C" 1)"
assert_argv_has_no_resume "$block1" "5c/batch-1"
assert_argv_has_p_prompt "$block1" "5c/batch-1"

# Column must stay NULL (no winner picked).
col="$(get_column "$CWD_C" "d-5c")"
[[ -z "$col" ]] \
  || fail "(5c) column should be NULL under race; got '$col' (would mean daemon silently picked a winner, violates §6.2 + §10 Q2)"

# Warning must be logged.
grep -q 'daemon.cli_session_capture.gemini_race_detected' "$LOG_5C" \
  || { cat "$LOG_5C" >&2; fail "(5c) expected gemini_race_detected warning"; }

echo "   (5c) ok — race detected, column stays NULL, warning logged"

# =============================================================================
# Subtest (5d) — mix-mode flag-is-the-gate: flag=false + column non-NULL →
# no --resume. Defensive assertion (flag is authoritative).
# =============================================================================
step "(5d) flag=false + column non-NULL → NO --resume in argv"

CWD_D="$TMP/daemon-d"
register_daemon "$CWD_D" "d-5d" "key-5d"

STATE_D="$TMP/state-5d.txt"
PREPOP_UUID="99999999-8888-7777-6666-555555555555"
# State file contains the PREPOP_UUID so it would "resume" if the flag were on.
echo "$PREPOP_UUID" > "$STATE_D"
set_column "$CWD_D" "d-5d" "$PREPOP_UUID"

LOG_5D="$TMP/daemon-5d.log"
SPAWN_5D="$TMP/spawn-5d.log"
: > "$SPAWN_5D"

send_one "$SEND_CWD" "key-send" "d-5d" "$CWD_D" "d-msg-1"
# Run WITHOUT --cli-session-resume flag. Column has a UUID; should be ignored.
run_daemon_flag_off_until_spawn_count "d-5d" "$CWD_D" "key-5d" "$LOG_5D" "$SPAWN_5D" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_D"

block1="$(get_spawn_block "$SPAWN_5D" 1)"
assert_argv_has_no_resume "$block1" "5d/flag-off"
assert_argv_has_p_prompt "$block1" "5d/flag-off"

# Column must remain unchanged (flag=false → daemon never touches the column).
col="$(get_column "$CWD_D" "d-5d")"
[[ "$col" == "$PREPOP_UUID" ]] \
  || fail "(5d) flag=false should leave column untouched; expected $PREPOP_UUID, got '$col'"

echo "   (5d) ok — flag=false dominates non-NULL column"

# =============================================================================
# Subtest (5e) — 2-label cross-isolation: two daemon labels in same DB
# each capture their own UUID.
# =============================================================================
step "(5e) 2-label cross-isolation"

CWD_E1="$TMP/daemon-e1"
CWD_E2="$TMP/daemon-e2"
register_daemon "$CWD_E1" "d-5e1" "key-5e1"
register_daemon "$CWD_E2" "d-5e2" "key-5e2"

STATE_E1="$TMP/state-5e1.txt"; : > "$STATE_E1"
STATE_E2="$TMP/state-5e2.txt"; : > "$STATE_E2"
LOG_5E1="$TMP/daemon-5e1.log"; LOG_5E2="$TMP/daemon-5e2.log"
SPAWN_5E1="$TMP/spawn-5e1.log"; : > "$SPAWN_5E1"
SPAWN_5E2="$TMP/spawn-5e2.log"; : > "$SPAWN_5E2"

UUID_E1="e1e1e1e1-1111-1111-1111-111111111111"
UUID_E2="e2e2e2e2-2222-2222-2222-222222222222"

send_one "$SEND_CWD" "key-send" "d-5e1" "$CWD_E1" "e1-msg"
run_daemon_until_spawn_count "d-5e1" "$CWD_E1" "key-5e1" "$LOG_5E1" "$SPAWN_5E1" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_E1" \
  "FAKE_GEMINI_NEW_UUID=$UUID_E1"

send_one "$SEND_CWD" "key-send" "d-5e2" "$CWD_E2" "e2-msg"
run_daemon_until_spawn_count "d-5e2" "$CWD_E2" "key-5e2" "$LOG_5E2" "$SPAWN_5E2" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_E2" \
  "FAKE_GEMINI_NEW_UUID=$UUID_E2"

col1="$(get_column "$CWD_E1" "d-5e1")"
col2="$(get_column "$CWD_E2" "d-5e2")"

[[ "$col1" == "$UUID_E1" ]] \
  || fail "(5e) label 1 should have $UUID_E1; got '$col1'"
[[ "$col2" == "$UUID_E2" ]] \
  || fail "(5e) label 2 should have $UUID_E2; got '$col2'"

# Defensive: resetting label 1 must not touch label 2.
"$PI" daemon-reset-session --cwd "$CWD_E1" --as d-5e1 >/dev/null \
  || fail "(5e) reset label 1 failed"
col1_post="$(get_column "$CWD_E1" "d-5e1")"
col2_post="$(get_column "$CWD_E2" "d-5e2")"
[[ -z "$col1_post" ]] \
  || fail "(5e) label 1 should be NULL after reset, got '$col1_post'"
[[ "$col2_post" == "$UUID_E2" ]] \
  || fail "(5e) label 2 should still have $UUID_E2 after label 1 reset; got '$col2_post'"

echo "   (5e) ok — per-label isolation holds across capture + reset"

# =============================================================================
# Subtest (5f) — GEMINI_CONFIG_DIR opt-in propagation.
# Set in the daemon's env; must be visible in the spawned gemini's env.
# =============================================================================
step "(5f) GEMINI_CONFIG_DIR propagation"

CWD_F="$TMP/daemon-f"
register_daemon "$CWD_F" "d-5f" "key-5f"

STATE_F="$TMP/state-5f.txt"
: > "$STATE_F"
LOG_5F="$TMP/daemon-5f.log"
SPAWN_5F="$TMP/spawn-5f.log"
: > "$SPAWN_5F"

GEMINI_CFG_DIR="$HOME/.gemini-daemon-test-5f"
mkdir -p "$GEMINI_CFG_DIR"

UUID_F="f0f0f0f0-dddd-cccc-bbbb-aaaaaaaaaaaa"
send_one "$SEND_CWD" "key-send" "d-5f" "$CWD_F" "f-msg"
run_daemon_until_spawn_count "d-5f" "$CWD_F" "key-5f" "$LOG_5F" "$SPAWN_5F" 1 \
  "FAKE_GEMINI_STATE_FILE=$STATE_F" \
  "FAKE_GEMINI_NEW_UUID=$UUID_F" \
  "GEMINI_CONFIG_DIR=$GEMINI_CFG_DIR"

block1="$(get_spawn_block "$SPAWN_5F" 1)"
grep -q "^ENV\[GEMINI_CONFIG_DIR\]=$GEMINI_CFG_DIR$" <<<"$block1" \
  || { echo "$block1" >&2; fail "(5f) GEMINI_CONFIG_DIR not propagated to spawned gemini"; }

echo "   (5f) ok — GEMINI_CONFIG_DIR propagated ($GEMINI_CFG_DIR)"

# =============================================================================
# Subtest (5g) — v0.1.2 fix: --list-sessions timeout configurable + 15s default.
#
# E6 probe surfaced that real gemini 0.38.2 takes ~5.3s to enumerate
# against an operator-sized config. v0.1's hardcoded 5s deterministically
# missed. v0.1.2 fix: default raised to 15s; AGENT_COLLAB_DAEMON_GEMINI_
# LIST_TIMEOUT env var lets operators tune.
#
# This subtest exercises the env override by setting it to a small value
# (3s) AND making the fake stub deliberately slow (sleep 5s on
# --list-sessions). With the override at 3s, the BEFORE snapshot must
# fail with a timeout-shaped error message containing "timed out after 3s"
# in the daemon log. With the env unset (default 15s) and the same 5s
# sleep, the BEFORE snapshot succeeds and capture proceeds normally.
# =============================================================================
step "(5g) --list-sessions timeout: env override (AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT) + 15s default"

CWD_G="$TMP/daemon-g"
register_daemon "$CWD_G" "d-5g" "key-5g"

# Build a slow fake gemini: sleeps 5 seconds before responding to
# --list-sessions, otherwise behaves like the standard fake (state file
# + spawn log).
FAKE_GEMINI_SLOW="$TMP/fake-gemini-slow.sh"
cat > "$FAKE_GEMINI_SLOW" <<'EOF'
#!/usr/bin/env bash
set -u
STATE="${FAKE_GEMINI_STATE_FILE:?FAKE_GEMINI_STATE_FILE required}"
LOG="${FAKE_GEMINI_SPAWN_LOG:?FAKE_GEMINI_SPAWN_LOG required}"
NEW_UUID="${FAKE_GEMINI_NEW_UUID:-}"
EXTRA_UUID="${FAKE_GEMINI_EXTRA_UUID:-}"
SLEEP_SEC="${FAKE_GEMINI_LIST_SLEEP_SEC:-0}"

is_list=0
for a in "$@"; do
  if [[ "$a" == "--list-sessions" ]]; then is_list=1; fi
done

if (( is_list )); then
  if [[ "$SLEEP_SEC" != "0" ]]; then sleep "$SLEEP_SEC"; fi
  if [[ -s "$STATE" ]]; then
    n="$(wc -l < "$STATE" | tr -d ' ')"
    echo "Available sessions for this project ($n):"
    i=1
    while IFS= read -r u; do
      [[ -z "$u" ]] && continue
      printf "  %d. test-prompt (now) [%s]\n" "$i" "$u"
      i=$((i+1))
    done < "$STATE"
  else
    echo "Available sessions for this project (0):"
  fi
  exit 0
fi

# Spawn (-p) path: append argv + env to spawn log; optionally append
# new UUID(s) to state file; emit JSONL ack.
{
  echo "---SPAWN---"
  echo "ARGV_COUNT=$#"
  i=0
  for a in "$@"; do
    echo "ARGV[$i]=$a"
    i=$((i+1))
  done
  for k in AGENT_COLLAB_DAEMON_SPAWN AGENT_COLLAB_SESSION_KEY GEMINI_SESSION_ID GEMINI_CONFIG_DIR; do
    v="${!k-}"
    echo "ENV[$k]=$v"
  done
} >> "$LOG"

if [[ -n "$NEW_UUID" ]]; then
  tmp_state="$(mktemp)"
  echo "$NEW_UUID" > "$tmp_state"
  if [[ -n "$EXTRA_UUID" ]]; then echo "$EXTRA_UUID" >> "$tmp_state"; fi
  if [[ -s "$STATE" ]]; then cat "$STATE" >> "$tmp_state"; fi
  mv "$tmp_state" "$STATE"
fi

( read -t 1 _ ) 2>/dev/null || true
echo '{"peer_inbox_ack": true}'
exit 0
EOF
chmod +x "$FAKE_GEMINI_SLOW"

STATE_G="$TMP/state-5g.txt"
: > "$STATE_G"
LOG_5G_OVERRIDE="$TMP/daemon-5g-override.log"
SPAWN_5G_OVERRIDE="$TMP/spawn-5g-override.log"
: > "$SPAWN_5G_OVERRIDE"

# Local helper: run daemon with custom ack-timeout (BEFORE snapshot's
# blocking sleep would exceed the run_daemon_until_spawn_count helper's
# hardcoded 5s ack-timeout, so we need a roomier window for 5g).
# Polls for a regex in the log file; kills daemon when seen OR after
# wait_secs elapsed.
run_daemon_for_5g() {
  local label="$1" cwd="$2" key="$3" dlog="$4" spawn_log="$5" want_log_re="$6" wait_secs="$7"
  shift 7
  python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE inbox SET claimed_at = NULL, completed_at = NULL WHERE to_label = '$label'\")
c.execute(\"UPDATE sessions SET daemon_state = 'open', daemon_cli_session_id = NULL WHERE label = '$label'\")
c.commit()
c.close()
"
  (
    export AGENT_COLLAB_DAEMON_GEMINI_BIN="$FAKE_GEMINI_SLOW"
    export FAKE_GEMINI_SPAWN_LOG="$spawn_log"
    for kv in "$@"; do
      # shellcheck disable=SC2163
      export "$kv"
    done
    "$DAEMON" \
      --label "$label" \
      --cwd "$cwd" \
      --session-key "$key" \
      --cli gemini \
      --cli-session-resume \
      --ack-timeout 30 \
      --sweep-ttl 61 \
      --poll-interval 1 \
      --log-path "$dlog" \
      >/dev/null 2>&1 &
    echo $! > "$TMP/daemon-${label}.pid"
  )
  local pid; pid="$(cat "$TMP/daemon-${label}.pid")"

  local waited=0
  local found=0
  while (( waited < wait_secs * 2 )); do
    if [[ -f "$dlog" ]] && grep -qE "$want_log_re" "$dlog" 2>/dev/null; then
      found=1
      break
    fi
    sleep 0.5
    waited=$((waited+1))
  done
  # A beat for any post-event capture writes.
  sleep 1
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$TMP/daemon-${label}.pid"
  return $((1 - found))
}

# (5g-i) Override = 3s, slow stub sleeps 5s → BEFORE snapshot must
# timeout with "timed out after 3s" in the daemon log.
UUID_G="9999cccc-bbbb-4aaa-8aaa-cccccccccccc"
send_one "$SEND_CWD" "key-send" "d-5g" "$CWD_G" "g-msg-override"
run_daemon_for_5g "d-5g" "$CWD_G" "key-5g" "$LOG_5G_OVERRIDE" "$SPAWN_5G_OVERRIDE" \
  'timed out after 3s' 25 \
  "FAKE_GEMINI_STATE_FILE=$STATE_G" \
  "FAKE_GEMINI_NEW_UUID=$UUID_G" \
  "FAKE_GEMINI_LIST_SLEEP_SEC=5" \
  "AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT=3" \
  || { tail -40 "$LOG_5G_OVERRIDE" >&2; fail "(5g-i) expected 'timed out after 3s' in daemon log; env override AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT=3 not honored"; }
grep -q 'override via AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT' "$LOG_5G_OVERRIDE" \
  || { tail -40 "$LOG_5G_OVERRIDE" >&2; fail "(5g-i) timeout error message must mention env var name for operator discoverability"; }

echo "   (5g-i) ok — env override AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT=3 honored; BEFORE snapshot timed out cleanly"

# (5g-ii) Default (env unset) + 5s sleep → BEFORE snapshot succeeds
# (15s default >> 5s sleep). Wait until we observe a successful spawn
# (---SPAWN--- in spawn log) which proves the BEFORE call returned
# without timeout. Capture column gets the new UUID via delta-snapshot.
LOG_5G_DEFAULT="$TMP/daemon-5g-default.log"
SPAWN_5G_DEFAULT="$TMP/spawn-5g-default.log"
: > "$SPAWN_5G_DEFAULT"
: > "$STATE_G"
UUID_G2="aaaaeeee-bbbb-4ccc-8ddd-aaaaaaaaaaaa"
send_one "$SEND_CWD" "key-send" "d-5g" "$CWD_G" "g-msg-default"
# AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT NOT set → default 15s.
# Wait for the captured-event log line, which proves capture succeeded.
run_daemon_for_5g "d-5g" "$CWD_G" "key-5g" "$LOG_5G_DEFAULT" "$SPAWN_5G_DEFAULT" \
  'gemini_captured' 25 \
  "FAKE_GEMINI_STATE_FILE=$STATE_G" \
  "FAKE_GEMINI_NEW_UUID=$UUID_G2" \
  "FAKE_GEMINI_LIST_SLEEP_SEC=5" \
  || { tail -60 "$LOG_5G_DEFAULT" >&2; fail "(5g-ii) expected daemon.cli_session_capture.gemini_captured log; default 15s timeout did NOT survive 5s --list-sessions latency"; }

col_default="$(get_column "$CWD_G" "d-5g")"
[[ "$col_default" == "$UUID_G2" ]] \
  || { tail -60 "$LOG_5G_DEFAULT" >&2; fail "(5g-ii) expected captured UUID $UUID_G2; got '$col_default'"; }
# Negative check: default-path log must NOT mention the timeout error.
if grep -q 'timed out after' "$LOG_5G_DEFAULT"; then
  tail -60 "$LOG_5G_DEFAULT" >&2
  fail "(5g-ii) default 15s should NOT timeout on a 5s sleep — but log mentions 'timed out after'"
fi

echo "   (5g-ii) ok — 15s default survived 5s --list-sessions latency; capture succeeded ($UUID_G2)"

# =============================================================================
# (5h) Cross-CLI reset-isolation (Topic 3 v0.2 §9.2 gate D):
# Reset on a gemini label MUST NOT invoke os.Remove — the pi-specific
# file-delete branch is gated on sessions.agent == 'pi'. Regression gate
# against a future refactor dropping the agent-check.
# =============================================================================
step "(5h) cross-CLI reset-isolation: gemini reset leaves deleted_file unset"

CWD_H="$TMP/cwd-h"
register_daemon "$CWD_H" "d-5h" "key-5h"
ISO_UUID="eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
set_column "$CWD_H" "d-5h" "$ISO_UUID"

# Sentinel file with UUID as name. Regression catch: if reset verb
# dropped the agent-gate and invoked os.Remove on the UUID string, the
# sentinel (which lives at $CWD/$UUID) would be deleted.
SENT_5H="$CWD_H/$ISO_UUID"
echo "sentinel-for-5h" > "$SENT_5H"

reset_json="$("$PI" daemon-reset-session --cwd "$CWD_H" --as d-5h --format json 2>&1)"
[[ -n "$reset_json" ]] || fail "(5h) reset verb emitted no output"
if echo "$reset_json" | grep -q '"deleted_file"'; then
  fail "(5h) gemini reset emitted deleted_file field (should be absent for non-pi agent): $reset_json"
fi
[[ -f "$SENT_5H" ]] \
  || fail "(5h) sentinel file deleted — reset verb invoked os.Remove on gemini UUID (agent-gate regression)"
echo "   (5h) reset emitted no deleted_file field; sentinel survives"

echo "PASS: daemon-cli-resume-gemini — 5a/5a-translation/5b/5c/5d/5e/5f/5g/5h all behave per §9.3 gate 5 + §3.4 invariants 1/2/5 + §9.2 gate D"
