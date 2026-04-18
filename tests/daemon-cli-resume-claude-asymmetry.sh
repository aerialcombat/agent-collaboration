#!/usr/bin/env bash
# tests/daemon-cli-resume-claude-asymmetry.sh — Topic 3 v0.1.1 patch:
# claude-asymmetry regression gate per scope-doc v3 §9.3 gate 6.
#
# Locks the §3.4 invariant 4 contract (claude + cli_session_resume MUST
# be non-fatal) at three layers + a defensive spawn-construction layer
# so future refactors of either the startup warning OR the per-CLI
# spawn helpers cannot silently regress the asymmetry.
#
# Four subtests:
#
#   (6a) Exact-warning-string fixture pin. Run daemon with --cli claude
#        --cli-session-resume; assert the daemon's stderr contains the
#        EXACT documented operator-facing string. Locks the wording so
#        operators grepping for the warning don't break on a refactor
#        that mutates message text.
#
#   (6b) Arch B proceeds. Daemon with --cli claude + flag spawns
#        normally; no exit-78, no crash, no skipped batch — the spawn
#        completes via the JSONL ack path identically to a no-flag run.
#        Locks "warn + fallback to Arch B" rather than fail-loud.
#
#   (6c) Column no-write. After the spawn completes, sessions.daemon_
#        cli_session_id stays NULL for the claude label. Locks the
#        spawn-helper contract that claude NEVER touches the column
#        (no capture-attempt, no SetDaemonCLISessionID call, regardless
#        of stdout banner content).
#
#   (6d) Defensive spawn-construction. Pre-populate sessions.daemon_
#        cli_session_id with a bogus UUID via SQL; start daemon with
#        --cli claude + flag; assert spawn argv contains NO --resume
#        / --session-id / -r / equivalent. Locks the asymmetry at the
#        spawn-construction layer, not just the warning layer — even
#        if a future regression accidentally wired claude into the
#        column-read path, this test catches it before the spawn argv
#        ships the cached UUID.
#
# Pattern: mirrors tests/daemon-cli-resume-codex.sh fake-CLI approach.
# A single fake-claude stub captures argv per-invocation; subtests
# assert against the captured argv + the daemon's stderr capture +
# the SQL column state.
#
# §3.4 invariant 1 (envelope load-bearing per batch) is honored by the
# spawn path: the fake claude argv dump includes the prompt as the
# final positional in every subtest's invocation. Subtests don't
# explicitly assert the prompt — that's tests/daemon-harness.sh's lane.

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
step() { echo "-- daemon-cli-resume-claude-asymmetry: $*"; }

mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/agent-collab-daemon ./cmd/daemon ) \
  || { echo "skip: go build daemon failed"; exit 0; }

DAEMON="$PROJECT_ROOT/go/bin/agent-collab-daemon"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab" "$HOME/.claude"
# Minimal claude settings file so the daemon's MANDATORY --settings
# fixture-pin path resolves without the test caring about its contents.
echo '{}' > "$HOME/.claude/settings.json"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

# Tight TTLs, preserve 2x ratio (§3.4 (c)).
export AGENT_COLLAB_ACK_TIMEOUT=2
export AGENT_COLLAB_SWEEP_TTL=5

DAEMON_CWD="$TMP/daemon-claude"
SEND_CWD="$TMP/send"
mkdir -p "$DAEMON_CWD" "$SEND_CWD"

# Exact warning string the daemon emits per go/cmd/daemon/main.go:252+254.
# Pin to the EXACT bytes so refactors that mutate wording are caught.
EXPECTED_WARNING='Claude has no cross-process session-resume; --cli-session-resume is a no-op for this daemon (see Arch B asymmetry note in operator guide).'

# ----------------------------------------------------------------------
# Register sessions: one daemon-mode claude recipient + one sender.
# ----------------------------------------------------------------------
step "register daemon claude recipient + interactive sender"
AGENT_COLLAB_SESSION_KEY="key-claude" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-claude \
    --agent claude --receive-mode daemon >/dev/null \
  || fail "session-register daemon-claude failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"

# ----------------------------------------------------------------------
# Fake claude stub. Writes argv to $FAKE_CLI_OUT. Emits a JSONL ack
# marker on stdout so the daemon completes the batch via mechanism (2)
# (§7.2 fallback). Does NOT emit a session-id banner — claude doesn't
# have one in -p mode, and we want to verify the spawn-helper makes no
# capture attempt regardless of stdout content.
# ----------------------------------------------------------------------
make_fake_claude() {
  local path="$1"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
# Fake claude stub for tests/daemon-cli-resume-claude-asymmetry.sh.
set -u
OUT="${FAKE_CLI_OUT:-/dev/null}"

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

# Drain stdin with a short timeout so the daemon's /dev/null piping
# completes cleanly even on macos bash 3.2.
( read -t 1 _ ) 2>/dev/null || true

# JSONL ack marker so the daemon completes via mechanism (2) §7.2.
echo '{"peer_inbox_ack": true}'
exit 0
EOF
  chmod +x "$path"
}

FAKE_CLAUDE="$TMP/fake-claude.sh"
make_fake_claude "$FAKE_CLAUDE"

seed_one() {
  local tag="$1"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to daemon-claude \
      --to-cwd "$DAEMON_CWD" \
      --message "claude-asymmetry probe: $tag" >/dev/null
}

reset_inbox() {
  python3 - "$DB" daemon-claude <<'PY'
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
  python3 - "$DB" daemon-claude <<'PY'
import sqlite3, sys
db, label = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
r = c.execute("SELECT daemon_cli_session_id FROM sessions WHERE label = ?", (label,)).fetchone()
c.close()
print("" if (r is None or r[0] is None) else r[0])
PY
}

set_cli_session_id() {
  local uuid="$1"
  python3 - "$DB" daemon-claude "$uuid" <<'PY'
import sqlite3, sys
db, label, uuid = sys.argv[1], sys.argv[2], sys.argv[3]
c = sqlite3.connect(db)
c.execute("UPDATE sessions SET daemon_cli_session_id = ? WHERE label = ?", (uuid, label))
c.commit()
c.close()
PY
}

# Run the daemon briefly. Captures stderr to $TMP/daemon-${tag}.stderr
# so subtest 6a can grep it for the warning string.
# Args: tag, flag-cli-session-resume ("1"|"0").
run_daemon_briefly() {
  local tag="$1" flag="$2"

  local outpath="$TMP/argv-${tag}.txt"
  local stderr_path="$TMP/daemon-${tag}.stderr"
  rm -f "$outpath" "$stderr_path"

  reset_inbox
  seed_one "$tag"

  local flag_str=""
  if [[ "$flag" == "1" ]]; then
    flag_str="--cli-session-resume"
  fi

  (
    export AGENT_COLLAB_DAEMON_CLAUDE_BIN="$FAKE_CLAUDE"
    export FAKE_CLI_OUT="$outpath"
    # shellcheck disable=SC2086
    "$DAEMON" \
      --label daemon-claude \
      --cwd "$DAEMON_CWD" \
      --session-key "key-claude" \
      --cli claude \
      --claude-settings "$HOME/.claude/settings.json" \
      --ack-timeout 2 \
      --sweep-ttl 5 \
      --poll-interval 1 \
      --log-path "$TMP/daemon-${tag}.log" \
      $flag_str \
      >/dev/null 2>"$stderr_path" &
    echo $! > "$TMP/daemon.pid"
  )
  local pid; pid="$(cat "$TMP/daemon.pid")"

  local waited=0
  while (( waited < 30 )); do
    if [[ -s "$outpath" ]]; then break; fi
    sleep 0.5
    waited=$((waited+1))
  done

  # Beat for any post-spawn handler — though for claude there isn't one
  # (the spawn-helper makes no capture attempt). Match the codex test's
  # cadence so timing variance is consistent.
  sleep 0.4

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true

  if [[ ! -s "$outpath" ]]; then
    echo "--- daemon stderr (${tag}) ---"
    cat "$stderr_path" 2>/dev/null || echo "(no stderr)"
    echo "--- daemon log (${tag}) ---"
    cat "$TMP/daemon-${tag}.log" 2>/dev/null || echo "(no log)"
    fail "fake claude never ran for tag=$tag — daemon did not spawn"
  fi
}

# =============================================================================
# (6a) Exact-warning-string fixture pin
# =============================================================================
step "(6a) exact-warning-string emitted on --cli claude --cli-session-resume"
run_daemon_briefly "6a" "1"

stderr_path="$TMP/daemon-6a.stderr"
if ! grep -F "$EXPECTED_WARNING" "$stderr_path" >/dev/null; then
  echo "--- expected warning ---"
  echo "$EXPECTED_WARNING"
  echo "--- actual stderr ---"
  cat "$stderr_path"
  fail "(6a) exact warning string NOT found in daemon stderr"
fi
echo "   (6a) exact warning string emitted on stderr"

# =============================================================================
# (6b) Arch B proceeds — daemon spawns normally + completes batch
# =============================================================================
step "(6b) Arch B fallback — spawn proceeds, no exit-78, no crash"
# Reuse the (6a) artifacts: if the fake claude ran, the daemon spawned
# normally + accepted the JSONL ack. Verify the inbox row got marked
# completed (proves the spawn went through, not just that argv was
# constructed).
completed_count="$(python3 - "$DB" <<'PY'
import sqlite3, sys
db = sys.argv[1]
c = sqlite3.connect(db)
n = c.execute(
  "SELECT COUNT(*) FROM inbox WHERE to_label='daemon-claude' AND completed_at IS NOT NULL"
).fetchone()[0]
c.close()
print(n)
PY
)"
[[ "$completed_count" -ge 1 ]] \
  || fail "(6b) expected at least one completed inbox row; got $completed_count — Arch B did not proceed"
echo "   (6b) Arch B proceeded — $completed_count completed row(s) on daemon-claude label"

# =============================================================================
# (6c) Column no-write — daemon_cli_session_id stays NULL for claude label
# =============================================================================
step "(6c) sessions.daemon_cli_session_id stays NULL for claude label after spawn"
got="$(read_cli_session_id)"
[[ -z "$got" ]] \
  || fail "(6c) expected daemon_cli_session_id NULL for claude label; got '$got' — claude spawn-helper wrote to column (regression)"
echo "   (6c) column stayed NULL — claude spawn-helper did not touch daemon_cli_session_id"

# =============================================================================
# (6d) Defensive spawn-construction — pre-populate column + assert NO
#      resume / --session-id in argv
# =============================================================================
step "(6d) pre-populate column with bogus UUID; spawn argv MUST NOT contain --resume or --session-id"
BOGUS_UUID="deadbeef-dead-beef-dead-beefdeadbeef"
set_cli_session_id "$BOGUS_UUID"

# Verify the SQL pre-populate landed.
got="$(read_cli_session_id)"
[[ "$got" == "$BOGUS_UUID" ]] \
  || fail "(6d) pre-populate failed — set $BOGUS_UUID, read '$got'"

run_daemon_briefly "6d" "1"

argv_path="$TMP/argv-6d.txt"
# Assert argv does NOT contain any resume-related flag/value forms.
if grep -E '^ARGV\[[0-9]+\]=(--resume|-r|resume|--session-id|--continue|-c|--last)$' "$argv_path" >/dev/null; then
  echo "--- argv ---"
  cat "$argv_path"
  fail "(6d) DEFENSIVE: claude spawn argv contained a resume-style flag or value with --cli-session-resume + populated column — asymmetry leaked at spawn-construction layer"
fi
# Also assert the bogus UUID itself is not present anywhere in argv —
# catches an accidental --resume <UUID> wire even if the flag form is
# unusual. The cwd / settings paths are deterministic and don't contain
# UUIDs.
if grep -F "$BOGUS_UUID" "$argv_path" >/dev/null; then
  echo "--- argv ---"
  cat "$argv_path"
  fail "(6d) DEFENSIVE: bogus UUID '$BOGUS_UUID' appeared in claude spawn argv — column was read into argv (regression)"
fi
echo "   (6d) DEFENSIVE: claude spawn argv contained NO --resume / --session-id / bogus-UUID — asymmetry locked at spawn-construction layer"

echo "PASS: daemon-cli-resume-claude-asymmetry — 6a exact-warning + 6b Arch B proceeds + 6c column no-write + 6d defensive spawn-construction (§3.4 invariant 4)"
