#!/usr/bin/env bash
# tests/multi-room-v36.sh — v3.6 M1 + M3 verification.
#
# Exercises the wire-level assertion of v3.6: a Claude session joined
# into multiple rooms over a single channel socket receives pushes
# whose meta.pair_key identifies the originating room, and the channel
# MCP's peer_inbox_reply routes replies back into the right room.
#
#   M1 — two rooms / same host / same socket: every incoming push
#        carries correct meta.pair_key; reply with no explicit
#        pair_key targets the room of the last incoming; reply with
#        explicit pair_key overrides.
#   M3 — ambiguity error path: multi-room session with no prior
#        incoming and no explicit pair_key → reply errors with a
#        disambiguation hint.
#
# M2 (federation) is exercised by the existing federation-smoke.sh
# gateway passthrough path plus manual live-topology verification —
# see plans/v3.6-multi-room.md.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
CHANNEL_PY="$PROJECT_ROOT/scripts/peer-inbox-channel.py"

if [ ! -x "$PI_BIN" ]; then
  echo "skip: peer-inbox binary missing at $PI_BIN (run 'go build ./cmd/peer-inbox')"
  exit 0
fi
if [ ! -f "$CHANNEL_PY" ]; then
  echo "skip: channel MCP script missing at $CHANNEL_PY"
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"; exit 0
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "skip: sqlite3 not on PATH"; exit 0
fi

MIGRATE_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
if [ ! -x "$MIGRATE_BIN" ]; then
  echo "skip: peer-inbox-migrate missing at $MIGRATE_BIN"
  exit 0
fi

TMP_RAW="$(mktemp -d -t v36-multiroom.XXXXXX)"
# Canonicalize: macOS aliases /var → /private/var and /tmp → /private/tmp,
# and peer-inbox stores the realpath in sessions.cwd. Use the realpath
# form everywhere so sqlite assertions match.
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/sessions.db"
# Keep unix socket dirs short — AF_UNIX paths cap at 104 chars on
# macOS, and $TMP resolves under /private/var/folders/... (~75 chars).
SOCK_DIR="/tmp/v36mr-$$-a"
SOCK2_DIR="/tmp/v36mr-$$-b"
rm -rf "$SOCK_DIR" "$SOCK2_DIR"
mkdir -p "$SOCK_DIR" "$SOCK2_DIR"
CWD_RECV="$TMP/recv"; mkdir -p "$CWD_RECV"
CWD_A="$TMP/peerA"; mkdir -p "$CWD_A"
CWD_B="$TMP/peerB"; mkdir -p "$CWD_B"
MCP_LOG="$TMP/mcp.log"
MCP_OUT="$TMP/mcp.stdout"
MCP_IN="$TMP/mcp.stdin"
mkfifo "$MCP_IN"

# Bootstrap schema: mint the Python-owned legacy tables, then goose-migrate.
# Matches tests/parity/run.sh:88-126 so the Go binaries see a fully-migrated
# DB without having to spin up the Python impl first.
sqlite3 "$DB" >/dev/null <<'SQL'
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS sessions (
  cwd             TEXT NOT NULL,
  label           TEXT NOT NULL,
  agent           TEXT NOT NULL,
  role            TEXT,
  session_key     TEXT,
  channel_socket  TEXT,
  pair_key        TEXT,
  started_at      TEXT NOT NULL,
  last_seen_at    TEXT NOT NULL,
  PRIMARY KEY (cwd, label)
);
CREATE TABLE IF NOT EXISTS inbox (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  to_cwd      TEXT NOT NULL,
  to_label    TEXT NOT NULL,
  from_cwd    TEXT NOT NULL,
  from_label  TEXT NOT NULL,
  body        TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  read_at     TEXT,
  room_key    TEXT
);
CREATE TABLE IF NOT EXISTS peer_rooms (
  room_key       TEXT PRIMARY KEY,
  pair_key       TEXT,
  turn_count     INTEGER NOT NULL DEFAULT 0,
  terminated_at  TEXT,
  terminated_by  TEXT
);
SQL
"$MIGRATE_BIN" -driver sqlite -dsn "$DB" \
  -dir "$PROJECT_ROOT/migrations/sqlite" up >/dev/null 2>&1 \
  || { echo "schema bootstrap failed for $DB" >&2; exit 2; }

MCP_PID=""
cleanup() {
  if [ -n "$MCP_PID" ] && kill -0 "$MCP_PID" 2>/dev/null; then
    kill "$MCP_PID" 2>/dev/null
    wait "$MCP_PID" 2>/dev/null
  fi
  if [ -n "${MCP2_PID:-}" ] && kill -0 "$MCP2_PID" 2>/dev/null; then
    kill "$MCP2_PID" 2>/dev/null
    wait "$MCP2_PID" 2>/dev/null
  fi
  rm -rf "$TMP_RAW" "$SOCK_DIR" "$SOCK2_DIR"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  [ -f "$MCP_LOG" ] && { echo "--- MCP stderr ---" >&2; cat "$MCP_LOG" >&2; }
  exit 1
}

# ---------------------------------------------------------------------------
# Spawn the channel MCP under a temp PEER_INBOX_SOCKET_DIR so we can read
# its socket path deterministically from stderr. AGENT_COLLAB_BIN pins
# the wrapper the MCP uses for peer_inbox_reply → peer-send dispatch so
# the test DB is routed to, not the developer's installed binary.
PEER_INBOX_SOCKET_DIR="$SOCK_DIR" \
PEER_INBOX_PENDING_DIR="$TMP/pending" \
AGENT_COLLAB_INBOX_DB="$DB" \
AGENT_COLLAB_BIN="$PROJECT_ROOT/scripts/agent-collab" \
AGENT_COLLAB_PEER_INBOX_BIN="$PI_BIN" \
  python3 "$CHANNEL_PY" <"$MCP_IN" >"$MCP_OUT" 2>"$MCP_LOG" &
MCP_PID=$!
# Keep the fifo open so the MCP's stdin stays open across our writes.
exec 9>"$MCP_IN"

# Handshake: initialize + initialized so the MCP accepts socket pushes.
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' >&9
printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}' >&9

# Wait for the socket to appear.
SOCK=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  SOCK="$(ls "$SOCK_DIR"/*.sock 2>/dev/null | head -1 || true)"
  [ -n "$SOCK" ] && break
  sleep 0.2
done
[ -n "$SOCK" ] || fail "MCP socket never appeared in $SOCK_DIR"

# ---------------------------------------------------------------------------
# Seed two rooms with distinct pair_keys and register:
#   - the receiver (label=recv) into BOTH rooms, sharing the same socket
#   - peerA (label=peer-a) into R1
#   - peerB (label=peer-b) into R2
PK_A="room-a-$(date +%s)"
PK_B="room-b-$(date +%s)"

# Point pending-channels at a test-only empty dir so session-register
# calls that don't pass --channel-uri can't accidentally latch onto the
# developer's real claude channels in ~/.agent-collab/pending-channels.
EMPTY_PENDING="$TMP/empty-pending"; mkdir -p "$EMPTY_PENDING"
pi() { PEER_INBOX_PENDING_DIR="$EMPTY_PENDING" AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" "$@"; }

pi room-create --pair-key "$PK_A" >/dev/null || fail "room-create A"
pi room-create --pair-key "$PK_B" >/dev/null || fail "room-create B"

# Register receiver into BOTH rooms — same socket, distinct pair_keys.
# Use --channel-uri to pin the socket (pending-channels file would be
# consumed by only the first register).
pi session-register \
  --cwd "$CWD_RECV" --label recv --agent claude --role peer \
  --session-key "recv-a-$$" --pair-key "$PK_A" \
  --channel-uri "$SOCK" --force >/dev/null || fail "register recv/A"
pi session-register \
  --cwd "$CWD_RECV" --label recv-b --agent claude --role peer \
  --session-key "recv-b-$$" --pair-key "$PK_B" \
  --channel-uri "$SOCK" --force >/dev/null || fail "register recv/B"

# Senders — one per room. No --channel-uri; with EMPTY_PENDING they'll
# register with channel_socket=NULL (no incoming push support needed).
pi session-register \
  --cwd "$CWD_A" --label peer-a --agent claude --role peer \
  --session-key "peer-a-$$" --pair-key "$PK_A" --force >/dev/null || fail "register peerA"
pi session-register \
  --cwd "$CWD_B" --label peer-b --agent claude --role peer \
  --session-key "peer-b-$$" --pair-key "$PK_B" --force >/dev/null || fail "register peerB"

# ---------------------------------------------------------------------------
# M1a — push from peer-a (room A) → recv. Expect meta.pair_key=$PK_A on
# the resulting notifications/claude/channel line in MCP stdout.
pi peer-send \
  --cwd "$CWD_A" --as peer-a --to recv --message "hello-from-A" >/dev/null \
  || fail "peer-send A"

# Push from peer-b (room B) → recv-b. Expect meta.pair_key=$PK_B.
pi peer-send \
  --cwd "$CWD_B" --as peer-b --to recv-b --message "hello-from-B" >/dev/null \
  || fail "peer-send B"

# Give the MCP a moment to drain both pushes into stdout.
sleep 0.5

assert_meta_pair_key_for_body() {
  local body_substr="$1" want_pk="$2"
  # Find the notifications/claude/channel line whose content contains body_substr.
  local line
  line="$(grep -F "$body_substr" "$MCP_OUT" | head -1 || true)"
  [ -n "$line" ] || fail "no channel notification found for body '$body_substr'"
  if ! python3 - "$line" "$want_pk" <<'PY'; then
import json, sys
line, want = sys.argv[1], sys.argv[2]
obj = json.loads(line)
meta = (obj.get("params") or {}).get("meta") or {}
got = meta.get("pair_key")
if got != want:
    print(f"pair_key mismatch: got={got!r} want={want!r}", file=sys.stderr)
    sys.exit(1)
PY
    fail "meta.pair_key mismatch for body '$body_substr' (want $want_pk)"
  fi
}

assert_meta_key_for_body() {
  local body_substr="$1" want_key="$2" want_val="$3"
  local line
  line="$(grep -F "$body_substr" "$MCP_OUT" | head -1 || true)"
  [ -n "$line" ] || fail "no channel notification found for body '$body_substr'"
  if ! python3 - "$line" "$want_key" "$want_val" <<'PY'; then
import json, sys
line, want_key, want_val = sys.argv[1], sys.argv[2], sys.argv[3]
obj = json.loads(line)
meta = (obj.get("params") or {}).get("meta") or {}
got = meta.get(want_key)
if got != want_val:
    print(f"meta.{want_key} mismatch: got={got!r} want={want_val!r} (body={line!r})", file=sys.stderr)
    sys.exit(1)
PY
    fail "meta.$want_key mismatch for body '$body_substr' (want $want_val)"
  fi
}

assert_meta_pair_key_for_body "hello-from-A" "$PK_A"
assert_meta_pair_key_for_body "hello-from-B" "$PK_B"

echo "OK  M1a — meta.pair_key stamped per room on incoming pushes"

# ---------------------------------------------------------------------------
# M1b — peer_inbox_reply with explicit pair_key routes into the right room.
# Two concurrent reply tool calls; observe which --as / --cwd invocation
# peer-send sees. We can't easily intercept the subprocess, but we can
# verify the reply lands in the correct inbox row via sqlite.

call_reply_tool() {
  local id="$1" body="$2" pk="$3" to="$4"
  local pk_arg=""
  if [ -n "$pk" ]; then
    pk_arg=", \"pair_key\": \"$pk\""
  fi
  local to_arg=""
  if [ -n "$to" ]; then
    to_arg=", \"to\": \"$to\""
  fi
  printf '{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"peer_inbox_reply","arguments":{"body":"%s"%s%s}}}\n' \
    "$id" "$body" "$to_arg" "$pk_arg" >&9
}

# Explicit pair_key A → reply routed into room A, lands in peer-a's inbox.
call_reply_tool 10 "reply-into-A" "$PK_A" "peer-a"
# Explicit pair_key B → reply routed into room B, lands in peer-b's inbox.
call_reply_tool 11 "reply-into-B" "$PK_B" "peer-b"

sleep 1.0

assert_inbox_has() {
  local to_cwd="$1" to_label="$2" body="$3"
  local got
  got="$(sqlite3 "$DB" "SELECT body FROM inbox WHERE to_cwd = '$to_cwd' AND to_label = '$to_label' AND body = '$body' LIMIT 1")"
  [ "$got" = "$body" ] || fail "inbox missing ($to_label): want=$body got=$got"
}

assert_inbox_has "$CWD_A" peer-a "reply-into-A"
assert_inbox_has "$CWD_B" peer-b "reply-into-B"

echo "OK  M1b — explicit pair_key routes replies into correct room"

# ---------------------------------------------------------------------------
# M1c — reply with NO pair_key defaults to last-incoming room.
# Last incoming was "hello-from-B" (we sent B second), so the reply
# should target room B / peer-b. Send a distinctive body.
call_reply_tool 12 "reply-default-B" "" "peer-b"
sleep 0.6
assert_inbox_has "$CWD_B" peer-b "reply-default-B"

# Now push another message into room A; default should switch.
pi peer-send \
  --cwd "$CWD_A" --as peer-a --to recv --message "second-A" >/dev/null \
  || fail "peer-send A 2nd"
sleep 0.4
call_reply_tool 13 "reply-default-A" "" "peer-a"
sleep 0.6
assert_inbox_has "$CWD_A" peer-a "reply-default-A"

echo "OK  M1c — default reply tracks last-incoming pair_key"

# ---------------------------------------------------------------------------
# M2 — gateway passthrough. A POST to /api/channel-push?pair_key=X with
# a legacy payload (no meta.pair_key) must inject pair_key=X into the
# forwarded unix-socket push so the channel MCP can stamp the correct
# room. Verifies handleChannelPush at cmd/peer-web/server/channel_push.go.
PW_BIN="$PROJECT_ROOT/go/bin/peer-web"
if [ ! -x "$PW_BIN" ]; then
  echo "skip: peer-web binary missing — M2 gateway passthrough not verified"
else
  if ! command -v curl >/dev/null 2>&1; then
    echo "skip: curl not on PATH — M2 gateway passthrough not verified"
  else
    PW_PORT=$((20000 + RANDOM % 10000))
    PW_LOG="$TMP/peer-web.log"
    AGENT_COLLAB_INBOX_DB="$DB" \
      "$PW_BIN" --port "$PW_PORT" --cwd "$CWD_RECV" >"$PW_LOG" 2>&1 &
    PW_PID=$!
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      sleep 0.2
      curl -sf "http://127.0.0.1:$PW_PORT/" >/dev/null 2>&1 && break
    done

    # Legacy payload (no meta.pair_key) — gateway must synthesize it.
    curl -sS -X POST \
      "http://127.0.0.1:$PW_PORT/api/channel-push?label=recv&pair_key=$PK_A" \
      -H 'Content-Type: application/json' \
      --data '{"from":"remote-peer","body":"via-gateway-legacy","meta":{}}' \
      >/dev/null || fail "gateway POST failed"

    sleep 0.3
    assert_meta_pair_key_for_body "via-gateway-legacy" "$PK_A"

    # Sender-supplied meta.pair_key must win over query-string fallback.
    curl -sS -X POST \
      "http://127.0.0.1:$PW_PORT/api/channel-push?label=recv&pair_key=$PK_A" \
      -H 'Content-Type: application/json' \
      --data "{\"from\":\"remote-peer\",\"body\":\"sender-override\",\"meta\":{\"pair_key\":\"$PK_B\"}}" \
      >/dev/null || fail "gateway override POST failed"

    sleep 0.3
    assert_meta_pair_key_for_body "sender-override" "$PK_B"

    kill "$PW_PID" 2>/dev/null || true
    wait "$PW_PID" 2>/dev/null || true

    echo "OK  M2 — gateway injects pair_key; sender meta wins when provided"
  fi
fi


# ---------------------------------------------------------------------------
# M3 — ambiguity error path. Spawn a SECOND MCP whose session has two
# rooms registered but no incoming yet, call peer_inbox_reply with no
# pair_key, expect an error.

MCP2_LOG="$TMP/mcp2.log"
MCP2_OUT="$TMP/mcp2.stdout"
MCP2_IN="$TMP/mcp2.stdin"
mkfifo "$MCP2_IN"

PEER_INBOX_SOCKET_DIR="$SOCK2_DIR" \
PEER_INBOX_PENDING_DIR="$TMP/pending2" \
AGENT_COLLAB_INBOX_DB="$DB" \
AGENT_COLLAB_BIN="$PROJECT_ROOT/scripts/agent-collab" \
AGENT_COLLAB_PEER_INBOX_BIN="$PI_BIN" \
  python3 "$CHANNEL_PY" <"$MCP2_IN" >"$MCP2_OUT" 2>"$MCP2_LOG" &
MCP2_PID=$!
exec 8>"$MCP2_IN"
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' >&8
printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}' >&8

SOCK2=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  SOCK2="$(ls "$SOCK2_DIR"/*.sock 2>/dev/null | head -1 || true)"
  [ -n "$SOCK2" ] && break
  sleep 0.2
done
[ -n "$SOCK2" ] || fail "MCP2 socket never appeared"

PK_X="room-x-$(date +%s)"
PK_Y="room-y-$(date +%s)"
CWD_RECV2="$TMP/recv2"; mkdir -p "$CWD_RECV2"

AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" room-create --pair-key "$PK_X" >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" room-create --pair-key "$PK_Y" >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-register \
  --cwd "$CWD_RECV2" --label recv2 --agent claude --role peer \
  --session-key "recv2-x-$$" --pair-key "$PK_X" --channel-uri "$SOCK2" --force >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" "$PI_BIN" session-register \
  --cwd "$CWD_RECV2" --label recv2-y --agent claude --role peer \
  --session-key "recv2-y-$$" --pair-key "$PK_Y" --channel-uri "$SOCK2" --force >/dev/null

# Call reply with no pair_key and no prior incoming — expect error.
printf '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"peer_inbox_reply","arguments":{"body":"ambiguous","to":"someone"}}}\n' >&8
sleep 0.6

if ! grep -F '"id":99' "$MCP2_OUT" | grep -q 'no `pair_key` given'; then
  fail "M3: expected ambiguity error, got: $(grep '"id":99' "$MCP2_OUT" || echo '(no id=99 response)')"
fi

kill "$MCP2_PID" 2>/dev/null; wait "$MCP2_PID" 2>/dev/null

echo "OK  M3 — ambiguous reply errors with helpful disambiguation message"

# ---------------------------------------------------------------------------
# M4 — v3.7 from_role / from_agent stamping. A human owner (agent=human,
# role=owner) sending into the room must stamp meta.from_role="owner" and
# meta.from_agent="human" on the channel push so receiving MCPs can
# distinguish human pings from agent chatter.
CWD_OWNER="$TMP/owner-cwd"; mkdir -p "$CWD_OWNER"
pi session-register \
  --cwd "$CWD_OWNER" --label human-owner --agent human --role owner \
  --session-key "owner-m4-$$" --pair-key "$PK_A" --force >/dev/null \
  || fail "register human-owner"

pi peer-send \
  --cwd "$CWD_OWNER" --as human-owner --to recv \
  --message "v3.7 owner ping: respond now" >/dev/null \
  || fail "human-owner peer-send"

sleep 0.4

assert_meta_key_for_body "v3.7 owner ping" "from_role" "owner"
assert_meta_key_for_body "v3.7 owner ping" "from_agent" "human"

# An agent peer send should carry from_agent but not from_role=owner.
pi peer-send \
  --cwd "$CWD_A" --as peer-a --to recv \
  --message "v3.7 agent ping: routine chatter" >/dev/null \
  || fail "peer-a agent peer-send"
sleep 0.3
assert_meta_key_for_body "v3.7 agent ping" "from_agent" "claude"
# peer-a has role="peer" (see session-register above) — stamping is
# correct regardless, what matters is it's NOT "owner".
if grep -F "v3.7 agent ping" "$MCP_OUT" | python3 -c '
import json, sys
line = sys.stdin.read().strip()
obj = json.loads(line)
meta = (obj.get("params") or {}).get("meta") or {}
if meta.get("from_role") == "owner":
    sys.exit(1)
'; then
  :
else
  fail "agent send wrongly stamped from_role=owner"
fi

echo "OK  M4 — v3.7 from_role / from_agent stamped per sender"

echo "all multi-room v3.6 + v3.7 checks passed"
