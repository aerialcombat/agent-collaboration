#!/usr/bin/env bash
# tests/peer-web-go-parity.sh — v3.2 frontend-go-rewrite commit-1 gate.
# Asserts the Go peer-web binary's JSON responses on /api/scope,
# /api/pairs, /api/rooms, /api/messages are byte-identical to the
# Python cmd_peer_web endpoints (/scope.json, /pairs.json, /rooms.json,
# /messages.json) when run against the same seeded SQLite DB.
#
# Strategy: seed a scratch DB with one pair-key room (3 members,
# 5 broadcast messages) + one cwd-scoped pair; spin up Go server on
# port A, Python server on port B (one per scope); diff sorted JSON.
# Cleanup on exit.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AC_PY="$PROJECT_ROOT/scripts/peer-inbox-db.py"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"; exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"; exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "skip: curl not on PATH"; exit 0
fi

TMP="$(mktemp -d -t peer-web-parity.XXXXXX)"
export AGENT_COLLAB_INBOX_DB="$TMP/sessions.db"

# Kernel-assigned free ports (avoid collisions with unrelated local services).
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()'
}
GO_PORT="$(free_port)"
PY_PORT="$(free_port)"

cleanup() {
  local rc=$?
  [[ -n "${GO_PID:-}" ]] && kill "$GO_PID" 2>/dev/null || true
  [[ -n "${PY_PID:-}" ]] && kill "$PY_PID" 2>/dev/null || true
  wait 2>/dev/null
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup EXIT INT TERM

# 1. Build the Go binary. Python auto-migrates on first open so no
#    explicit migrate invocation is needed — the seed path below
#    triggers it.
cd "$PROJECT_ROOT/go"
go build -o "$TMP/peer-web-go" ./cmd/peer-web/

# 2. Seed the DB. Uses the Python CLI because its register / send
#    verbs are the canonical write path; Go has parity on reads only
#    for this cluster.
SEED_CWD="$TMP/cwd-a"
mkdir -p "$SEED_CWD"
PK="parity-test-pk"

# Mint empty room first so members can join.
python3 "$AC_PY" room-create --pair-key "$PK" >/dev/null

# Register 3 members under the pair_key.
for lbl in alpha beta gamma; do
  python3 "$AC_PY" session-register \
    --cwd "$SEED_CWD" --label "$lbl" --agent claude \
    --pair-key "$PK" --session-key "parity-$lbl-key" --force >/dev/null
done

# Broadcast 3 messages from alpha.
for msg in "msg-one" "msg-two" "msg-three"; do
  python3 "$AC_PY" peer-broadcast \
    --cwd "$SEED_CWD" --as alpha --message "$msg" >/dev/null
done

# Also register a cwd-scoped edge pair (no pair_key) for cwd-mode tests.
CWD_B="$TMP/cwd-b"
mkdir -p "$CWD_B"
python3 "$AC_PY" session-register \
  --cwd "$CWD_B" --label solo-a --agent claude \
  --session-key "solo-a-key" --force >/dev/null
python3 "$AC_PY" session-register \
  --cwd "$CWD_B" --label solo-b --agent claude \
  --session-key "solo-b-key" --force >/dev/null
python3 "$AC_PY" peer-send \
  --cwd "$CWD_B" --as solo-a --to solo-b --message "cwd-hello" >/dev/null

# 3. Start both servers. Go is run from SEED_CWD so its default
#    s.cfg.CWD matches Python's process cwd (Python's /pairs.json
#    echoes str(resolve_cwd(args.cwd)) which symlink-resolves via
#    Path.cwd().resolve(), so we realpath SEED_CWD for Go).
SEED_CWD_REAL="$(python3 -c "import os; print(os.path.realpath('$SEED_CWD'))")"
(cd "$SEED_CWD_REAL" && "$TMP/peer-web-go" --port "$GO_PORT") \
  >"$TMP/go.log" 2>&1 &
GO_PID=$!
(cd "$SEED_CWD_REAL" && python3 "$AC_PY" peer-web --pair-key "$PK" --port "$PY_PORT") \
  >"$TMP/py.log" 2>&1 &
PY_PID=$!

# Wait for both to bind.
for i in {1..50}; do
  if curl -sf "http://127.0.0.1:$GO_PORT/api/scope" >/dev/null 2>&1 && \
     curl -sf "http://127.0.0.1:$PY_PORT/scope.json" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
# Guard: bail loudly if either server didn't come up.
if ! curl -sf "http://127.0.0.1:$GO_PORT/api/scope" >/dev/null 2>&1; then
  echo "FAIL: Go server did not bind on port $GO_PORT"
  cat "$TMP/go.log"
  exit 1
fi
if ! curl -sf "http://127.0.0.1:$PY_PORT/scope.json" >/dev/null 2>&1; then
  echo "FAIL: Python server did not bind on port $PY_PORT"
  cat "$TMP/py.log"
  exit 1
fi

FAIL=0
check_diff() {
  local name="$1" go_url="$2" py_url="$3"
  local go_body py_body
  go_body="$(curl -s "$go_url" | python3 -m json.tool --sort-keys 2>/dev/null || true)"
  py_body="$(curl -s "$py_url" | python3 -m json.tool --sort-keys 2>/dev/null || true)"
  if [[ "$go_body" != "$py_body" ]]; then
    echo "FAIL: $name"
    diff <(echo "$go_body") <(echo "$py_body") | head -20
    FAIL=1
  else
    echo "OK:   $name"
  fi
}

check_diff "rooms (pair-key)" \
  "http://127.0.0.1:$GO_PORT/api/rooms?pair_key=$PK" \
  "http://127.0.0.1:$PY_PORT/rooms.json"

check_diff "pairs (pair-key)" \
  "http://127.0.0.1:$GO_PORT/api/pairs?pair_key=$PK" \
  "http://127.0.0.1:$PY_PORT/pairs.json"

check_diff "messages (pair-key)" \
  "http://127.0.0.1:$GO_PORT/api/messages?pair_key=$PK&after=0" \
  "http://127.0.0.1:$PY_PORT/messages.json?after=0"

# 4. cwd-mode tests. Restart both servers rooted in CWD_B so
#    response cwd fields match. Python's peer-web scopes by process
#    cwd when --pair-key is absent.
CWD_B_REAL="$(python3 -c "import os; print(os.path.realpath('$CWD_B'))")"
kill $GO_PID $PY_PID 2>/dev/null; wait 2>/dev/null
GO_PORT="$(free_port)"; PY_PORT="$(free_port)"
(cd "$CWD_B_REAL" && "$TMP/peer-web-go" --port "$GO_PORT") \
  >"$TMP/go.log" 2>&1 &
GO_PID=$!
(cd "$CWD_B_REAL" && python3 "$AC_PY" peer-web --port "$PY_PORT") \
  >"$TMP/py.log" 2>&1 &
PY_PID=$!
for i in {1..50}; do
  if curl -sf "http://127.0.0.1:$GO_PORT/api/scope" >/dev/null 2>&1 && \
     curl -sf "http://127.0.0.1:$PY_PORT/scope.json" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

check_diff "pairs (cwd)" \
  "http://127.0.0.1:$GO_PORT/api/pairs?cwd=$CWD_B_REAL" \
  "http://127.0.0.1:$PY_PORT/pairs.json"

check_diff "messages (cwd)" \
  "http://127.0.0.1:$GO_PORT/api/messages?cwd=$CWD_B_REAL&after=0" \
  "http://127.0.0.1:$PY_PORT/messages.json?after=0"

if [[ $FAIL -ne 0 ]]; then
  echo
  echo "go.log:"; sed 's/^/  /' <"$TMP/go.log"
  echo "py.log:"; sed 's/^/  /' <"$TMP/py.log"
  exit 1
fi

echo "all parity checks passed"
exit 0
