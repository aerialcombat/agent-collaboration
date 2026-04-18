#!/usr/bin/env bash
# tests/daemon-mode-lifecycle.sh — Topic 3 v0 Python daemon-mode verbs.
# Covers the §8.2 gates that pair with the Python daemon-mode feature commit:
#
#   (1) Lifecycle:     claim → complete → sweep requeue.
#   (2) SQL partition: interactive --mark-read cannot see daemon-claimed
#                      rows (§3.4 (a)).
#   (3) Receive-mode:  --mark-read rejects daemon-mode labels; --daemon-mode
#                      rejects interactive-mode labels (§3.4 (b)).
#   (4) Reap-race:     --complete fails loud on stale/reaped claim (§3.4 (d),
#                      alpha §B). Immediate re-claim succeeds after sweep.
#   (5) Closed-state:  --daemon-mode returns zero rows when daemon_state=
#                      'closed' without mutating inbox (§3.4 (e), gamma #2).
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
  echo "skip: go toolchain not on PATH (need peer-inbox-migrate)"
  exit 0
fi

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-mode-lifecycle: $*"; }

# Build migrate binary (needed for apply_migrations on fresh DB).
mkdir -p "$PROJECT_ROOT/go/bin"
( cd "$PROJECT_ROOT/go" && go build -o bin/peer-inbox-migrate ./cmd/migrate ) \
  || { echo "skip: go build failed"; exit 0; }

export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"
DB="$TMP/sessions.db"
export AGENT_COLLAB_INBOX_DB="$DB"

DAEMON_CWD="$TMP/daemon"
SEND_CWD="$TMP/send"
INTERACTIVE_CWD="$TMP/inter"
mkdir -p "$DAEMON_CWD" "$SEND_CWD" "$INTERACTIVE_CWD"

step "register daemon-mode recipient + interactive sender"
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" session register --cwd "$DAEMON_CWD" --label daemon-recv \
    --agent codex --receive-mode daemon >/dev/null \
  || fail "session-register with --receive-mode daemon failed"
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
      --message "probe $i" >/dev/null
done

step "(1b) --daemon-mode claim returns 3 rows with claimed_at set + read_at NULL"
claim_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --daemon-mode --format json 2>&1)" \
  || fail "--daemon-mode claim failed: $claim_out"
claim_count="$(printf '%s' "$claim_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$claim_count" == "3" ]] || fail "expected 3 rows claimed, got $claim_count"

claimed_unread="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute('SELECT COUNT(*) FROM inbox WHERE to_label = ? AND claimed_at IS NOT NULL AND read_at IS NULL AND completed_at IS NULL',
  ('daemon-recv',)).fetchone()[0]
print(r)
")"
[[ "$claimed_unread" == "3" ]] || fail "expected 3 rows with claimed_at set + read_at NULL, got $claimed_unread"

step "(2) interactive --mark-read on a DIFFERENT label sees no daemon-claimed rows"
# Send a probe to the interactive label; assert interactive mark-read only
# sees its own row, not the daemon's claimed batch.
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to inter-recv --to-cwd "$INTERACTIVE_CWD" \
    --message "interactive probe" >/dev/null
inter_out="$(AGENT_COLLAB_SESSION_KEY="key-inter" \
  "$AC" peer receive --cwd "$INTERACTIVE_CWD" --as inter-recv \
    --mark-read --format json 2>&1)" \
  || fail "interactive --mark-read failed: $inter_out"
inter_count="$(printf '%s' "$inter_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$inter_count" == "1" ]] || fail "interactive receiver should see exactly 1 row, got $inter_count"

step "(3a) --mark-read on daemon-mode label rejected with fail-loud (§3.4 (b))"
set +e
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --mark-read --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" != "0" ]] || fail "interactive --mark-read on daemon-mode label should have failed"

step "(3b) --daemon-mode on interactive label rejected (§3.4 (b) symmetric)"
set +e
AGENT_COLLAB_SESSION_KEY="key-inter" \
  "$AC" peer receive --cwd "$INTERACTIVE_CWD" --as inter-recv \
    --daemon-mode --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" != "0" ]] || fail "--daemon-mode on interactive label should have failed"

step "(1c) --complete on in-flight claim succeeds; rows gain completed_at"
complete_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --complete --format json 2>&1)" \
  || fail "--complete failed: $complete_out"
completed_ids="$(printf '%s' "$complete_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["completed_ids"]))')"
[[ "$completed_ids" == "3" ]] || fail "expected 3 completed ids, got $completed_ids"

step "(4a) --complete with no in-flight claim fails loud (§3.4 (d))"
set +e
stale_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --complete --format json 2>&1)"
rc=$?
set -e
[[ "$rc" != "0" ]] || fail "--complete on no-in-flight-claim should have failed loud"

step "(4b) reap-race: seed, claim, sleep>ttl, sweep, then --complete fails loud; re-claim succeeds"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "reap probe" >/dev/null
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --daemon-mode --format json >/dev/null

# Sleep briefly so claimed_at is "in the past" by more than 1s.
sleep 2

# Sweep with a 1s TTL — the 2-second-old claim should be reaped.
sweep_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --sweep --sweep-ttl 1 --format json 2>&1)" \
  || fail "--sweep failed: $sweep_out"
reaped_count="$(printf '%s' "$sweep_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["reaped"]))')"
[[ "$reaped_count" == "1" ]] || fail "expected 1 row reaped, got $reaped_count"

# --complete now fails loud (claim was reaped).
set +e
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --complete --format json >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" != "0" ]] || fail "--complete after reap should have failed loud per §3.4 (d)"

# Re-claim succeeds and returns the reaped row.
reclaim_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --daemon-mode --format json 2>&1)" \
  || fail "re-claim after reap failed: $reclaim_out"
reclaim_count="$(printf '%s' "$reclaim_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$reclaim_count" == "1" ]] || fail "expected re-claim to return 1 row (the reaped one), got $reclaim_count"

# Complete the re-claimed batch so the daemon-mode label is clean.
AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --complete --format json >/dev/null

step "(5) closed-state: flip daemon_state='closed' + seed more rows + claim returns empty"
# Flip daemon_state directly via SQL (the session-close verb is interactive-
# focused; explicit DB manipulation mimics what the daemon binary will do
# in response to Layer-2 envelope.state='closed').
python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
c.execute(\"UPDATE sessions SET daemon_state = 'closed' WHERE label = 'daemon-recv'\")
c.commit()
c.close()
"

AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" peer send --cwd "$SEND_CWD" --to daemon-recv --to-cwd "$DAEMON_CWD" \
    --message "post-close probe" >/dev/null

closed_out="$(AGENT_COLLAB_SESSION_KEY="key-daemon" \
  "$AC" peer receive --cwd "$DAEMON_CWD" --as daemon-recv \
    --daemon-mode --format json 2>&1)" \
  || fail "--daemon-mode on closed daemon failed (should have returned empty, not errored): $closed_out"
closed_count="$(printf '%s' "$closed_out" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')"
[[ "$closed_count" == "0" ]] || fail "closed daemon should return 0 rows, got $closed_count"

# And verify the row is still unclaimed in the DB (no claimed_at mutation).
unclaimed_after_close="$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
r = c.execute('SELECT COUNT(*) FROM inbox WHERE to_label = ? AND read_at IS NULL AND claimed_at IS NULL AND body = ?',
  ('daemon-recv', 'post-close probe')).fetchone()[0]
print(r)
")"
[[ "$unclaimed_after_close" == "1" ]] || fail "post-close probe row should remain unclaimed (got $unclaimed_after_close)"

echo "PASS: daemon-mode lifecycle + SQL partition + receive-mode + reap-race + closed-state all behave per §3.4"
