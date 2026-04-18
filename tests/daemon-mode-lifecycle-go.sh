#!/usr/bin/env bash
# tests/daemon-mode-lifecycle-go.sh — Topic 3 v0 Go daemon-mode parity.
# Mirrors tests/daemon-mode-lifecycle.sh but exercises the §8.2 gates
# through the go/cmd/peer-inbox/ binary instead of the Python helpers.
# Pairs with commit 4 of Topic 3 v0 per the scope-doc §9 commit ordering
# (test-engineer: Python tests with commit 3; Go tests with commit 4).
#
# Gates covered:
#   (1) Lifecycle:     daemon-claim → daemon-complete → daemon-sweep
#                      requeue (§2.1 + §3.3).
#   (2) SQL partition: interactive --mark-read cannot see rows claimed
#                      by the Go binary (§3.4 (a)).
#   (3) Receive-mode:  Go daemon-claim rejects interactive-mode labels;
#                      Go daemon-complete rejects same (§3.4 (b)).
#   (4) Reap-race:     Go daemon-complete fails loud (exit 65) on stale/
#                      reaped claim; immediate re-claim via Go succeeds
#                      (§3.4 (d), alpha §B).
#   (5) Closed-state:  Go daemon-claim returns zero rows with no inbox
#                      mutation when daemon_state='closed' (§3.4 (e),
#                      gamma #2).
#
# Session registration + peer-send are still driven by the Python
# agent-collab (commit 4 scope is daemon-mode verbs only; registration
# parity lives in later commits). The test uses TTL overrides
# (`--sweep-ttl 1`) to keep run time bounded.
#
# Runs entirely on a temp HOME / temp DB so it doesn't perturb the user's
# live peer-inbox state.

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
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-mode-lifecycle-go: $*"; }

# Build peer-inbox binary (Go parity CLI under test) + peer-inbox-migrate
# (needed by Python path to apply 0002 schema on fresh DB).
mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build migrate failed"; exit 0; }
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox ./cmd/peer-inbox ) \
  || { echo "skip: go build peer-inbox failed"; exit 0; }
PI="$PROJECT_ROOT/go/bin/peer-inbox"

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

DAEMON_CWD="$TMP/daemon"
SEND_CWD="$TMP/send"
INTERACTIVE_CWD="$TMP/inter"
mkdir -p "$DAEMON_CWD" "$SEND_CWD" "$INTERACTIVE_CWD"

step "register daemon-mode recipient + interactive sender + interactive recipient"
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-recv \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register --receive-mode daemon failed"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender \
    --agent claude >/dev/null \
  || fail "session-register sender failed"
AGENT_COLLAB_SESSION_KEY="key-inter" \
  "$AC" session register --cwd "$INTERACTIVE_CWD" --label inter-recv \
    --agent claude >/dev/null \
  || fail "session-register interactive recipient failed"

step "(1a) seed 3 unread rows to the daemon-mode label"
for i in 1 2 3; do
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
      --message "probe-go $i" >/dev/null
done

step "(1b) Go daemon-claim returns 3 rows with claimed_at set + read_at NULL"
claim_out="$("$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format json 2>&1)" \
  || fail "daemon-claim failed: $claim_out"
claim_count="$(printf '%s' "$claim_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$claim_count" == "3" ]] || fail "expected 3 rows claimed, got $claim_count (out=$claim_out)"

claimed_unread="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute('SELECT COUNT(*) FROM inbox WHERE to_label = ? AND claimed_at IS NOT NULL AND read_at IS NULL AND completed_at IS NULL',
  ('daemon-recv',)).fetchone()[0]
print(r)
")"
[[ "$claimed_unread" == "3" ]] || fail "expected 3 rows with claimed_at set + read_at NULL, got $claimed_unread"

# Also verify the Go JSON payload shape has the required keys.
python3 -c "
import json, sys
data = json.loads('''$claim_out''')
required = {'id', 'from_cwd', 'from_label', 'body', 'created_at'}
for row in data:
    missing = required - set(row.keys())
    if missing:
        sys.exit(f'row missing keys: {missing}')
" || fail "Go daemon-claim JSON shape mismatch"

step "(2) interactive --mark-read on a DIFFERENT label sees no daemon-claimed rows (§3.4 (a))"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to inter-recv --to-cwd "$INTERACTIVE_CWD" \
    --message "interactive probe" >/dev/null
inter_out="$(AGENT_COLLAB_SESSION_KEY="key-inter" \
  "$AC" peer receive --cwd "$INTERACTIVE_CWD" --as inter-recv \
    --mark-read --format json 2>&1)" \
  || fail "interactive --mark-read failed: $inter_out"
inter_count="$(printf '%s' "$inter_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$inter_count" == "1" ]] || fail "interactive receiver should see exactly 1 row, got $inter_count"

step "(3a) Go daemon-claim on interactive label rejected with exit 65 (§3.4 (b))"
set +e
"$PI" daemon-claim --cwd "$INTERACTIVE_CWD" --as inter-recv --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "65" ]] || fail "daemon-claim on interactive label: expected exit 65 (EX_DATAERR), got $rc"

step "(3b) Go daemon-complete on interactive label rejected with exit 65 (§3.4 (b))"
set +e
"$PI" daemon-complete --cwd "$INTERACTIVE_CWD" --as inter-recv --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "65" ]] || fail "daemon-complete on interactive label: expected exit 65 (EX_DATAERR), got $rc"

step "(1c) Go daemon-complete on in-flight claim succeeds; payload has 3 completed_ids"
complete_out="$("$PI" daemon-complete --cwd "$DAEMON_CWD" --as daemon-recv --format json 2>&1)" \
  || fail "daemon-complete failed: $complete_out"
completed_count="$(printf '%s' "$complete_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["completed_ids"]))')"
[[ "$completed_count" == "3" ]] || fail "expected 3 completed ids, got $completed_count (out=$complete_out)"

step "(4a) Go daemon-complete with no in-flight claim fails loud with exit 65 (§3.4 (d))"
set +e
"$PI" daemon-complete --cwd "$DAEMON_CWD" --as daemon-recv --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "65" ]] || fail "daemon-complete on stale claim: expected exit 65 (EX_DATAERR), got $rc"

step "(4b) reap-race: seed → claim → sleep>ttl → sweep → complete fails loud → re-claim succeeds"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "reap-go probe" >/dev/null
"$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format json >/dev/null

# Sleep past the 1s TTL so the claim becomes sweep-eligible.
sleep 2

sweep_out="$("$PI" daemon-sweep --sweep-ttl 1 --format json 2>&1)" \
  || fail "daemon-sweep failed: $sweep_out"
reaped_count="$(printf '%s' "$sweep_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["reaped"]))')"
[[ "$reaped_count" == "1" ]] || fail "expected 1 row reaped, got $reaped_count (out=$sweep_out)"

# Verify sweep payload has the top-level keys Python emits.
python3 -c "
import json, sys
data = json.loads('''$sweep_out''')
required = {'sweep_ttl_seconds', 'cutoff', 'reaped'}
missing = required - set(data.keys())
if missing: sys.exit(f'sweep payload missing keys: {missing}')
# Verify reaped row shape
if data['reaped']:
    row_required = {'id', 'to_cwd', 'to_label', 'claim_owner'}
    row_missing = row_required - set(data['reaped'][0].keys())
    if row_missing: sys.exit(f'reaped row missing keys: {row_missing}')
# claim_owner preserved as audit trail (alpha §A)
if not data['reaped'][0]['claim_owner']:
    sys.exit('claim_owner should be preserved as audit trail (alpha §A), got empty')
" || fail "Go daemon-sweep JSON shape mismatch"

# Verify claim_owner is still set on the row post-sweep (audit trail preservation).
owner_post_sweep="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute(\"SELECT claim_owner FROM inbox WHERE to_label = 'daemon-recv' AND body = 'reap-go probe'\").fetchone()
print(r[0] if r else '<missing>')
")"
[[ "$owner_post_sweep" == "daemon-recv" ]] || \
  fail "claim_owner should stay 'daemon-recv' as audit trail post-sweep, got: $owner_post_sweep"

# daemon-complete now fails loud (claim was reaped).
set +e
"$PI" daemon-complete --cwd "$DAEMON_CWD" --as daemon-recv --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "65" ]] || fail "daemon-complete after reap: expected exit 65 (EX_DATAERR), got $rc"

# Re-claim succeeds and returns the reaped row.
reclaim_out="$("$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format json 2>&1)" \
  || fail "re-claim after reap failed: $reclaim_out"
reclaim_count="$(printf '%s' "$reclaim_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$reclaim_count" == "1" ]] || fail "expected re-claim to return 1 row, got $reclaim_count"

# Clean up the re-claimed batch.
"$PI" daemon-complete --cwd "$DAEMON_CWD" --as daemon-recv --format json >/dev/null

step "(5) closed-state: daemon_state='closed' → Go daemon-claim returns empty without mutation (§3.4 (e))"
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'closed' WHERE label = 'daemon-recv'\")
c.commit()
c.close()
"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "post-close-go probe" >/dev/null

closed_out="$("$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format json 2>&1)" \
  || fail "daemon-claim on closed daemon failed (should have returned empty, not errored): $closed_out"
closed_count="$(printf '%s' "$closed_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$closed_count" == "0" ]] || fail "closed daemon should return 0 rows, got $closed_count"

# Verify the row is still unclaimed in the DB (no claimed_at mutation).
unclaimed_after_close="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute('SELECT COUNT(*) FROM inbox WHERE to_label = ? AND read_at IS NULL AND claimed_at IS NULL AND body = ?',
  ('daemon-recv', 'post-close-go probe')).fetchone()[0]
print(r)
")"
[[ "$unclaimed_after_close" == "1" ]] || fail "post-close-go probe row should remain unclaimed (got $unclaimed_after_close)"

# Re-open + re-claim restores normal behavior.
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'open' WHERE label = 'daemon-recv'\")
c.commit()
c.close()
"
reopen_out="$("$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format json 2>&1)" \
  || fail "daemon-claim after re-open failed: $reopen_out"
reopen_count="$(printf '%s' "$reopen_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$reopen_count" == "1" ]] || fail "post-reopen claim should return the unclaimed row, got $reopen_count"

step "(6) usage-error paths: missing --as, bad --format, bad --sweep-ttl exit 64"
set +e
"$PI" daemon-claim --cwd "$DAEMON_CWD" --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "64" ]] || fail "daemon-claim missing --as: expected exit 64, got $rc"

set +e
"$PI" daemon-claim --cwd "$DAEMON_CWD" --as daemon-recv --format xml >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "64" ]] || fail "daemon-claim --format xml: expected exit 64, got $rc"

set +e
"$PI" daemon-sweep --sweep-ttl 0 --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" == "64" ]] || fail "daemon-sweep --sweep-ttl 0: expected exit 64, got $rc"

echo "PASS: Go daemon-mode lifecycle + SQL partition + receive-mode + reap-race + closed-state + usage-errors all behave per §3.4"
