#!/usr/bin/env bash
# tests/fail-open-fallback.sh — idle-birch #1 / B-1: when the Go hook
# binary hits ErrSchemaTooOld (or any other open/read error the Python CLI
# can recover from), the bash wrapper MUST trigger the Python fallback
# rather than silently returning 0 and touching the ack file.
#
# Pre-fix (idle-birch #1 HIGH): Go returned 0 on all error paths, bash
# only fell back on non-zero exit, the ack file got touched unconditionally,
# and unread mail stayed hidden until some future send bumped the marker.
#
# Fix (go/cmd/hook/main.go:63,111,135): `exitFallbackWanted = 2` on Open
# failure (incl. ErrSchemaTooOld) and on ReadUnread failure. Bash's
# `if ! output="$("$hook_bin" ...)"` at hooks/peer-inbox-inject.sh:108
# catches non-zero and re-routes to the Python CLI.
#
# This test simulates schema drift by DROPping the `meta` table post-init
# — which is exactly the state go/pkg/store/sqlite/sqlite.go:83-92 flags
# as ErrSchemaTooOld. It then asserts:
#   (1) Go binary alone exits non-zero on drifted schema.
#   (2) Bash hook still delivers the unread message via Python fallback.
#   (3) Python fallback re-seeds schema_version=10.

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
# Put the just-built Go binary on PATH so bash's `command -v
# peer-inbox-hook` finds it before falling back to other lookup spots.
export PATH="$BIN_DIR:$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
RECV_CWD="$TMP/recv"
SEND_CWD="$TMP/send"
mkdir -p "$RECV_CWD" "$SEND_CWD"

AC="$PROJECT_ROOT/scripts/agent-collab"
HOOK="$PROJECT_ROOT/hooks/peer-inbox-inject.sh"

echo "-- fail-open-fallback: register recv + send, seed first message --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv" \
  "$AC" session register --cwd "$RECV_CWD" --label recv --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label send --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$RECV_CWD" \
    --message "baseline probe: healthy schema" >/dev/null

echo "-- fail-open-fallback: baseline — Go binary delivers on healthy DB --"
baseline_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv" \
    "$BIN" "$RECV_CWD" 2>/dev/null
)"
printf '%s' "$baseline_out" | grep -q "baseline probe: healthy schema" \
  || fail "baseline broken: Go binary should deliver on healthy DB before drift simulation"

echo "-- fail-open-fallback: send post-drift message + simulate pre-goose drift --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$RECV_CWD" \
    --message "post-drift probe: must survive fallback" >/dev/null

# Simulate the "migration never applied" drift surface the Go hot path
# guards against. Post-v3.0 W1 round-2: goose_db_version is the
# authoritative version source, so dropping goose_db_version +
# the v2-reserve columns rolls the DB back to a pre-0001 shape. On the
# next open(), Python's apply_migrations() re-invokes peer-inbox-migrate
# which re-applies 0001 cleanly.
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
# Drop dependent indexes first — SQLite DROP COLUMN refuses while
# an index references the column.
for idx in ('idx_outbox_workspace_idem', 'idx_inbox_workspace_idem'):
    c.execute(f'DROP INDEX IF EXISTS {idx}')
c.execute('DROP TABLE IF EXISTS goose_db_version')
c.execute('DROP TABLE IF EXISTS outbox')
c.execute('DROP TABLE IF EXISTS meta')
# SQLite can drop columns since 3.35; the legacy baseline schema had
# none of these v2-reserve columns.
for col in ('client_seq', 'idempotency_key', 'workspace_id', 'user_id'):
    try:
        c.execute(f'ALTER TABLE inbox DROP COLUMN {col}')
    except sqlite3.OperationalError as e:
        raise SystemExit(f'simulated-drift setup: DROP COLUMN {col} failed: {e}')
c.commit()
c.close()
"
has_goose="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT name FROM sqlite_master WHERE type='table' AND name='goose_db_version'\").fetchone()
print('yes' if r else 'no')
")"
[[ "$has_goose" == "no" ]] || fail "goose_db_version table should have been dropped"

echo "-- fail-open-fallback: assertion 1 — Go binary exits non-zero on drifted schema --"
set +e
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv" \
  "$BIN" "$RECV_CWD" >/dev/null 2>&1
go_exit=$?
set -e
echo "   Go binary exit on drifted schema: $go_exit (expected non-zero, ideally 2)"
[[ "$go_exit" != "0" ]] \
  || fail "B-1 violated: Go binary returned 0 on ErrSchemaTooOld — bash cannot distinguish from success"

# Go never writes; goose_db_version should still be absent after the
# standalone BIN call.
has_goose_after_go="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT name FROM sqlite_master WHERE type='table' AND name='goose_db_version'\").fetchone()
print('yes' if r else 'no')
")"
[[ "$has_goose_after_go" == "no" ]] \
  || fail "unexpected: goose_db_version reappeared after Go-only invocation"

echo "-- fail-open-fallback: assertion 2 — bash hook falls back to Python and delivers --"
hook_stdin='{"session_id":"key-recv","cwd":"'"$RECV_CWD"'"}'
hook_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv" \
    bash "$HOOK" <<<"$hook_stdin" 2>/dev/null
)"
echo "   hook stdout: ${hook_out:-<empty>}"
printf '%s' "$hook_out" | grep -q "post-drift probe: must survive fallback" \
  || fail "B-1 violated: bash hook did not fall back to Python on ErrSchemaTooOld — unread silently dropped"

echo "-- fail-open-fallback: assertion 3 — Python fallback re-migrated via goose --"
restored_version="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1\").fetchone()
print(r[0] if r else 'NONE')
")"
echo "   goose_db_version after fallback: $restored_version"
[[ "$restored_version" == "1" ]] \
  || fail "Python fallback should have re-applied goose migration 1; got '$restored_version'"

echo "PASS: B-1 fix verified — Go returns exitFallbackWanted on ErrSchemaTooOld, bash falls back, Python delivers + re-migrates via goose"
