#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP_ROOT="$(mktemp -d)"
TEST_HOME="$TMP_ROOT/home"
TEST_BIN="$TEST_HOME/bin"
SAMPLE_REPO="$TMP_ROOT/sample-repo"
PYTHON_ONLY_BIN="$TMP_ROOT/python-bin"

cleanup() {
  rm -rf "$TMP_ROOT"
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_file() {
  local path="$1"
  [[ -f "$path" ]] || fail "missing file: $path"
}

assert_executable() {
  local path="$1"
  [[ -x "$path" ]] || fail "missing executable: $path"
}

assert_contains() {
  local path="$1"
  local pattern="$2"
  grep -Fq "$pattern" "$path" || fail "expected '$pattern' in $path"
}

assert_glob_exists() {
  local pattern="$1"
  local matches=()

  shopt -s nullglob
  matches=($pattern)
  shopt -u nullglob

  (( ${#matches[@]} > 0 )) || fail "expected at least one match for glob: $pattern"
}

trap cleanup EXIT

run_peer_inbox_tests() {
  local inbox_home="$TMP_ROOT/peer-inbox-home"
  local db="$inbox_home/sessions.db"
  local agent_collab="$PROJECT_ROOT/scripts/agent-collab"
  local repo_a="$TMP_ROOT/peer-inbox-a"
  local repo_b="$TMP_ROOT/peer-inbox-b"
  local key_a="session-key-A-abc123"
  local key_b="session-key-B-def456"

  mkdir -p "$repo_a/sub/nested" "$repo_b" "$inbox_home"
  local repo_a_real repo_b_real
  repo_a_real="$(cd "$repo_a" && pwd -P)"
  repo_b_real="$(cd "$repo_b" && pwd -P)"

  echo "-- peer-inbox: basic register/send/receive --"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" AGENT_COLLAB_HOOK_LOG="$inbox_home/hook.log" \
    "$agent_collab" session register --cwd "$repo_a_real" --label backend --agent claude --role lead >/dev/null
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" session register --cwd "$repo_b_real" --label frontend --agent codex >/dev/null

  local marker_a="$repo_a_real/.agent-collab/sessions"
  [[ -d "$marker_a" ]] || fail "sessions dir not created for repo_a"
  local marker_file
  marker_file="$(ls "$marker_a"/*.json | head -1)"
  [[ -f "$marker_file" ]] || fail "per-session marker not written for repo_a"
  python3 -c "
import json, sys
d = json.load(open('$marker_file'))
assert d['cwd'] == '$repo_a_real', f\"marker cwd wrong: {d['cwd']}\"
assert d['label'] == 'backend', f\"marker label wrong: {d['label']}\"
assert d['session_key'] == '$key_a', f\"session_key wrong: {d['session_key']}\"
" || fail "marker JSON shape wrong"

  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" peer send --cwd "$repo_a_real" --to frontend --to-cwd "$repo_b_real" --message "hello, testing 123" >/dev/null

  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer receive --cwd "$repo_b_real" --format plain \
    | grep -q "hello, testing 123" || fail "message not received"

  echo "-- peer-inbox: two-sessions-same-cwd (canonical use case) --"
  local shared_repo="$TMP_ROOT/peer-inbox-shared"
  mkdir -p "$shared_repo"
  local shared_real
  shared_real="$(cd "$shared_repo" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" session register --cwd "$shared_real" --label be --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" session register --cwd "$shared_real" --label fe --agent codex >/dev/null
  local shared_marker_count
  shared_marker_count=$(ls "$shared_real/.agent-collab/sessions/"*.json 2>/dev/null | wc -l | tr -d ' ')
  [[ "$shared_marker_count" == "2" ]] || fail "expected 2 per-session markers in shared repo, got $shared_marker_count"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" peer send --cwd "$shared_real" --to fe --message "same-cwd message" >/dev/null
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer receive --cwd "$shared_real" --format plain --mark-read \
    | grep -q "same-cwd message" || fail "same-cwd message not delivered"

  echo "-- peer-inbox: disambiguation error when multiple sessions, no key, no --as --"
  if AGENT_COLLAB_INBOX_DB="$db" \
       "$agent_collab" peer receive --cwd "$shared_real" >/dev/null 2>&1; then
    fail "expected error for ambiguous self-resolution"
  fi

  echo "-- peer-inbox: adversarial content round-trip --"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" peer send --cwd "$repo_a_real" --to frontend --to-cwd "$repo_b_real" \
      --message "'; DROP TABLE inbox; --
quote \"double\" newline
semicolon; and readfile('/etc/passwd')" >/dev/null
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer receive --cwd "$repo_b_real" --format json --mark-read \
    | python3 -c "
import json, sys
rows = json.load(sys.stdin)
bodies = [r['body'] for r in rows]
assert any('DROP TABLE' in b for b in bodies), 'adversarial SQL content lost'
assert any('readfile' in b for b in bodies), 'readfile content lost'
" || fail "adversarial content did not round-trip"

  echo "-- peer-inbox: oversize rejection --"
  local big
  big="$(python3 -c 'print("x" * 9000)')"
  if AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
       "$agent_collab" peer send --cwd "$repo_a_real" --to frontend --to-cwd "$repo_b_real" --message "$big" >/dev/null 2>&1; then
    fail "oversize message was accepted"
  fi

  echo "-- peer-inbox: label collision from different session key rejected --"
  if AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="different-key-xyz" \
       "$agent_collab" session register --cwd "$repo_a_real" --label backend --agent claude >/dev/null 2>&1; then
    fail "duplicate label from different session key was accepted"
  fi
  echo "-- peer-inbox: same session key re-registering is idempotent --"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" session register --cwd "$repo_a_real" --label backend --agent claude >/dev/null \
    || fail "idempotent re-register by same session key failed"

  echo "-- peer-inbox: walk-parents marker discovery from subdir --"
  (
    cd "$repo_a_real/sub/nested"
    AGENT_COLLAB_INBOX_DB="$db" "$agent_collab" session list | grep -q backend \
      || fail "walk-parents discovery failed from subdir"
  )

  echo "-- peer-inbox: path drift detection --"
  local fake_repo="$TMP_ROOT/peer-inbox-fake"
  mkdir -p "$fake_repo/.agent-collab/sessions"
  cp "$marker_file" "$fake_repo/.agent-collab/sessions/"
  if AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
       "$agent_collab" peer list --cwd "$(cd "$fake_repo" && pwd -P)" >/dev/null 2>&1; then
    fail "path drift was not detected on copied marker"
  fi

  echo "-- peer-inbox: concurrent send hammer --"
  local before_count
  before_count=$(AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer receive --cwd "$repo_b_real" --format json \
    | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
  for i in $(seq 1 15); do
    AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
      "$agent_collab" peer send --cwd "$repo_a_real" --to frontend --to-cwd "$repo_b_real" --message "hammer-$i" >/dev/null &
  done
  wait
  local total
  total=$(AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer receive --cwd "$repo_b_real" --format json \
    | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
  [[ $((total - before_count)) -eq 15 ]] || fail "concurrent sends lost messages (got $((total - before_count)))"

  echo "-- peer-inbox: concurrent mark-read claim atomicity --"
  local claim_dir="$TMP_ROOT/peer-inbox-claims"
  mkdir -p "$claim_dir"
  for i in 1 2 3 4 5; do
    AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
      "$agent_collab" peer receive --cwd "$repo_b_real" --format json --mark-read > "$claim_dir/recv-$i.json" &
  done
  wait
  python3 <<PY || fail "claim atomicity violated"
import json
ids = []
for i in range(1, 6):
    rows = json.load(open("$claim_dir/recv-$i.json"))
    ids.extend(r["id"] for r in rows)
if len(ids) != len(set(ids)):
    raise SystemExit(f"duplicates: {len(ids)} claimed vs {len(set(ids))} unique")
PY

  echo "-- peer-inbox: hook emits valid JSON and passes session_id through --"
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
    "$agent_collab" peer send --cwd "$repo_a_real" --to frontend --to-cwd "$repo_b_real" --message "hook test" >/dev/null
  (
    cd "$repo_b_real"
    PATH="$PROJECT_ROOT/scripts:$PATH" \
    AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_HOOK_LOG="$inbox_home/hook.log" \
      "$PROJECT_ROOT/hooks/peer-inbox-inject.sh" <<EOF \
      | python3 -c "
import json, sys
out = sys.stdin.read()
if not out:
    raise SystemExit('hook produced empty output despite unread message')
d = json.loads(out)
assert d['hookSpecificOutput']['hookEventName'] == 'UserPromptSubmit'
assert '<peer-inbox' in d['hookSpecificOutput']['additionalContext']
"
{"session_id": "$key_b", "other": "stuff"}
EOF
  ) || fail "hook JSON invalid or missing peer-inbox block"

  echo "-- peer-inbox: cwd newline rejected --"
  if AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="temp-key-newline" \
       "$agent_collab" session register --cwd $'/tmp/newline\ndir' --label x --agent claude >/dev/null 2>&1; then
    fail "cwd with newline was accepted"
  fi

  echo "-- peer-inbox: peer watch picks up live messages --"
  local watch_out="$TMP_ROOT/peer-inbox-watch.out"
  # Backend session watches its inbox; frontend sends a message during the window.
  (
    AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_a" \
      timeout 3 "$agent_collab" peer watch --cwd "$repo_a_real" --interval 0.3 --only-new > "$watch_out" 2>&1
  ) &
  local watch_pid=$!
  sleep 0.8
  AGENT_COLLAB_INBOX_DB="$db" AGENT_COLLAB_SESSION_KEY="$key_b" \
    "$agent_collab" peer send --cwd "$repo_b_real" --to backend --to-cwd "$repo_a_real" --message "watch-test-payload" >/dev/null
  wait $watch_pid 2>/dev/null || true
  grep -q "watch-test-payload" "$watch_out" || fail "peer watch did not pick up live message (saw: $(cat "$watch_out"))"

  echo "-- peer-inbox: peer replay generates valid HTML --"
  local replay_out="$TMP_ROOT/peer-inbox-replay.html"
  AGENT_COLLAB_INBOX_DB="$db" \
    "$agent_collab" peer replay --cwd "$repo_a_real" --out "$replay_out" >/dev/null
  [[ -f "$replay_out" ]] || fail "peer replay did not produce output file"
  assert_contains "$replay_out" "<!doctype html>"
  assert_contains "$replay_out" 'class="msg"'
  python3 -c "
import html.parser, sys
class P(html.parser.HTMLParser):
    def error(self, msg): raise SystemExit('HTML parse error: ' + msg)
P().feed(open('$replay_out').read())
print('replay HTML parses OK')
" || fail "peer replay HTML failed to parse"

  echo "-- peer-inbox: [[end]] terminates pair, peer reset revives --"
  local pair_db="$TMP_ROOT/peer-inbox-pair.db"
  local pair_repo="$TMP_ROOT/peer-inbox-pair"
  mkdir -p "$pair_repo"
  local pair_real
  pair_real="$(cd "$pair_repo" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkA" \
    "$agent_collab" session register --cwd "$pair_real" --label pa --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkB" \
    "$agent_collab" session register --cwd "$pair_real" --label pb --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkA" \
    "$agent_collab" peer send --cwd "$pair_real" --to pb --message "shutting down [[end]]" >/dev/null
  if AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkA" \
       "$agent_collab" peer send --cwd "$pair_real" --to pb --message "after end" >/dev/null 2>&1; then
    fail "send after [[end]] was accepted"
  fi
  AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkA" \
    "$agent_collab" peer reset --cwd "$pair_real" --to pb >/dev/null
  AGENT_COLLAB_INBOX_DB="$pair_db" AGENT_COLLAB_SESSION_KEY="pkA" \
    "$agent_collab" peer send --cwd "$pair_real" --to pb --message "revived" >/dev/null \
    || fail "send after peer reset was rejected"

  echo "-- peer-inbox: max-turns cap blocks further sends --"
  local cap_db="$TMP_ROOT/peer-inbox-cap.db"
  local cap_repo="$TMP_ROOT/peer-inbox-cap"
  mkdir -p "$cap_repo"
  local cap_real
  cap_real="$(cd "$cap_repo" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$cap_db" AGENT_COLLAB_SESSION_KEY="ckA" \
    "$agent_collab" session register --cwd "$cap_real" --label ca --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$cap_db" AGENT_COLLAB_SESSION_KEY="ckB" \
    "$agent_collab" session register --cwd "$cap_real" --label cb --agent claude >/dev/null
  for i in 1 2 3; do
    AGENT_COLLAB_INBOX_DB="$cap_db" AGENT_COLLAB_SESSION_KEY="ckA" \
      AGENT_COLLAB_MAX_PAIR_TURNS=3 \
      "$agent_collab" peer send --cwd "$cap_real" --to cb --message "t$i" >/dev/null
  done
  if AGENT_COLLAB_INBOX_DB="$cap_db" AGENT_COLLAB_SESSION_KEY="ckA" \
       AGENT_COLLAB_MAX_PAIR_TURNS=3 \
       "$agent_collab" peer send --cwd "$cap_real" --to cb --message "t4" >/dev/null 2>&1; then
    fail "send past max-turns cap was accepted"
  fi

  echo "-- peer-inbox: channel pairing via process-tree walk + socket push --"
  # Start a channel MCP server; register a session from the same subshell
  # so process-tree walk finds the pending-channels file keyed by PPID.
  local ch_db="$TMP_ROOT/peer-inbox-ch.db"
  local ch_repo="$TMP_ROOT/peer-inbox-ch"
  # macOS caps AF_UNIX socket paths at ~104 chars, so put the socket dir
  # in a short /tmp location rather than under $TMP_ROOT (which lives
  # under /var/folders/...).
  local ch_sockets="/tmp/peer-inbox-smoke-$$"
  rm -rf "$ch_sockets"
  local ch_pending="$TMP_ROOT/peer-inbox-ch-pending"
  local ch_stdout_log="$TMP_ROOT/peer-inbox-ch-stdout.log"
  local ch_stderr_log="$TMP_ROOT/peer-inbox-ch-stderr.log"
  local ch_fifo="$TMP_ROOT/peer-inbox-ch.fifo"
  mkdir -p "$ch_repo" "$ch_sockets" "$ch_pending"
  mkfifo "$ch_fifo"
  local ch_real
  ch_real="$(cd "$ch_repo" && pwd -P)"
  (
    exec 3<>"$ch_fifo"
    AGENT_COLLAB_INBOX_DB="$ch_db" \
    PEER_INBOX_SOCKET_DIR="$ch_sockets" \
    PEER_INBOX_PENDING_DIR="$ch_pending" \
      python3 "$PROJECT_ROOT/scripts/peer-inbox-channel.py" <&3 \
        >"$ch_stdout_log" 2>"$ch_stderr_log" &
    local chpid=$!
    sleep 0.3
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}' >&3
    printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}' >&3
    sleep 0.3

    # Any pending-channels file is fine — the important check is that register
    # finds it via process-tree walk (which works from inside this subshell).
    if [[ -z "$(ls "$ch_pending"/*.json 2>/dev/null | head -1)" ]]; then
      echo "FAIL: no pending file created by channel; contents:" >&2
      ls -la "$ch_pending/" >&2 2>&1 || true
      cat "$ch_stderr_log" >&2 || true
      exit 1
    fi

    AGENT_COLLAB_INBOX_DB="$ch_db" AGENT_COLLAB_SESSION_KEY="chA" \
    PEER_INBOX_PENDING_DIR="$ch_pending" \
      "$agent_collab" session register --cwd "$ch_real" --label cha --agent claude \
      | grep -q "channel: paired" || { echo "FAIL: channel not paired on register" >&2; kill $chpid; exit 1; }

    AGENT_COLLAB_INBOX_DB="$ch_db" AGENT_COLLAB_SESSION_KEY="chB" \
      "$agent_collab" session register --cwd "$ch_real" --label chb --agent claude >/dev/null

    AGENT_COLLAB_INBOX_DB="$ch_db" AGENT_COLLAB_SESSION_KEY="chB" \
      "$agent_collab" peer send --cwd "$ch_real" --as chb --to cha --message "channel push test" \
      | grep -q "pushed" || { echo "FAIL: channel push did not report success" >&2; kill $chpid; exit 1; }
    sleep 0.3
    grep -q '"method":"notifications/claude/channel"' "$ch_stdout_log" \
      || { echo "FAIL: channel notification not emitted" >&2; kill $chpid; exit 1; }
    grep -q '"content":"channel push test"' "$ch_stdout_log" \
      || { echo "FAIL: channel notification missing body" >&2; kill $chpid; exit 1; }

    # v2.0: peer_inbox_reply MCP tool — advertised in tools/list, delivers via
    # tools/call for directed and broadcast sends.
    printf '%s\n' '{"jsonrpc":"2.0","id":100,"method":"tools/list"}' >&3
    sleep 0.2
    grep -q '"name":"peer_inbox_reply"' "$ch_stdout_log" \
      || { echo "FAIL: peer_inbox_reply missing from tools/list" >&2; kill $chpid; exit 1; }

    printf '%s\n' '{"jsonrpc":"2.0","id":101,"method":"tools/call","params":{"name":"peer_inbox_reply","arguments":{"to":"chb","body":"reply tool directed"}}}' >&3
    sleep 0.4
    AGENT_COLLAB_INBOX_DB="$ch_db" AGENT_COLLAB_SESSION_KEY="chB" \
      "$agent_collab" peer receive --as chb --cwd "$ch_real" --format plain \
      | grep -q "reply tool directed" \
      || { echo "FAIL: reply tool directed message not delivered" >&2; kill $chpid; exit 1; }

    printf '%s\n' '{"jsonrpc":"2.0","id":102,"method":"tools/call","params":{"name":"peer_inbox_reply","arguments":{"body":"reply tool broadcast"}}}' >&3
    sleep 0.4
    AGENT_COLLAB_INBOX_DB="$ch_db" AGENT_COLLAB_SESSION_KEY="chB" \
      "$agent_collab" peer receive --as chb --cwd "$ch_real" --format plain \
      | grep -q "reply tool broadcast" \
      || { echo "FAIL: reply tool broadcast did not reach peer chb" >&2; kill $chpid; exit 1; }

    kill $chpid 2>/dev/null || true
    wait $chpid 2>/dev/null || true
    exec 3>&-
  ) || { rm -rf "$ch_sockets" 2>/dev/null || true; fail "channel pairing/push subtest failed (see $ch_stderr_log)"; }
  rm -rf "$ch_sockets" 2>/dev/null || true

  echo "-- peer-inbox: peer web serves index + live JSON delta --"
  local web_db="$TMP_ROOT/peer-inbox-web.db"
  local web_repo="$TMP_ROOT/peer-inbox-web"
  local web_real
  mkdir -p "$web_repo"
  web_real="$(cd "$web_repo" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$web_db" AGENT_COLLAB_SESSION_KEY="wkA" \
    "$agent_collab" session register --cwd "$web_real" --label wa --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$web_db" AGENT_COLLAB_SESSION_KEY="wkB" \
    "$agent_collab" session register --cwd "$web_real" --label wb --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$web_db" AGENT_COLLAB_SESSION_KEY="wkA" \
    "$agent_collab" peer send --cwd "$web_real" --to wb --message "web view one" >/dev/null

  AGENT_COLLAB_INBOX_DB="$web_db" \
    "$agent_collab" peer web --cwd "$web_real" --port 8798 \
    >"$TMP_ROOT/peer-inbox-web.log" 2>&1 &
  local web_pid=$!
  sleep 0.4

  curl -s "http://127.0.0.1:8798/" | grep -q "peer-inbox" \
    || { kill $web_pid 2>/dev/null || true; fail "peer web / did not serve the index page"; }
  curl -s "http://127.0.0.1:8798/messages.json?after=0" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['messages'], 'no messages in delta'
assert d['messages'][0]['body'] == 'web view one', f'wrong body: {d}'
" || { kill $web_pid 2>/dev/null || true; fail "peer web /messages.json shape wrong"; }

  # Pair-scoped endpoints (v1.6 Slack-shaped UI)
  curl -s "http://127.0.0.1:8798/pairs.json" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['pairs'], 'no pairs in /pairs.json'
p = d['pairs'][0]
assert p['key'] == 'wa+wb', f'wrong canonical key: {p[\"key\"]}'
assert p['total'] >= 1
" || { kill $web_pid 2>/dev/null || true; fail "peer web /pairs.json shape wrong"; }

  # Pair-filtered messages
  curl -s "http://127.0.0.1:8798/messages.json?a=wa&b=wb" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['pair'] == 'wa+wb', f'canonical pair not returned: {d[\"pair\"]}'
assert all(
  (m['from'] == 'wa' and m['to'] == 'wb') or (m['from'] == 'wb' and m['to'] == 'wa')
  for m in d['messages']
), 'cross-pair bleed'
" || { kill $web_pid 2>/dev/null || true; fail "peer web pair filter wrong"; }

  AGENT_COLLAB_INBOX_DB="$web_db" AGENT_COLLAB_SESSION_KEY="wkB" \
    "$agent_collab" peer send --cwd "$web_real" --to wa --message "web view two" >/dev/null
  curl -s "http://127.0.0.1:8798/messages.json?after=1" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert len(d['messages']) == 1, f'expected 1 delta, got {len(d[\"messages\"])}'
assert d['messages'][0]['body'] == 'web view two'
" || { kill $web_pid 2>/dev/null || true; fail "peer web delta poll wrong"; }

  kill $web_pid 2>/dev/null || true
  wait $web_pid 2>/dev/null || true

  echo "-- peer-inbox: v1.7 pair-key cross-cwd send/receive --"
  local pk_db="$TMP_ROOT/peer-inbox-pk.db"
  local pk_a="$TMP_ROOT/peer-inbox-pk-a"
  local pk_b="$TMP_ROOT/peer-inbox-pk-b"
  mkdir -p "$pk_a" "$pk_b"
  local pk_a_real pk_b_real
  pk_a_real="$(cd "$pk_a" && pwd -P)"
  pk_b_real="$(cd "$pk_b" && pwd -P)"
  local pk_out
  pk_out=$(AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyA \
    "$agent_collab" session register --cwd "$pk_a_real" --label backend --agent claude --new-pair)
  local pk_key
  pk_key=$(printf '%s\n' "$pk_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  [[ -n "$pk_key" ]] || fail "--new-pair did not emit pair_key"
  AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyB \
    "$agent_collab" session register --cwd "$pk_b_real" --label frontend --agent codex --pair-key "$pk_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyA \
    "$agent_collab" peer send --cwd "$pk_a_real" --as backend --to frontend --message "cross-cwd via pair_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyB \
    "$agent_collab" peer receive --cwd "$pk_b_real" --as frontend --format plain --mark-read \
    | grep -q "cross-cwd via pair_key" || fail "pair_key cross-cwd message not delivered"

  echo "-- peer-inbox: v1.7 pair-key duplicate label rejected --"
  if AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyC \
       "$agent_collab" session register --cwd "$TMP_ROOT" --label backend --agent gemini --pair-key "$pk_key" >/dev/null 2>&1; then
    fail "duplicate label within a pair was accepted"
  fi

  echo "-- peer-inbox: v1.7 pair-key peer list scopes across cwds --"
  local pk_list
  pk_list=$(AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=pkeyB \
    "$agent_collab" peer list --cwd "$pk_b_real" --as frontend --json)
  python3 -c "
import json
rows = json.loads('''$pk_list''')
labels = sorted(r['label'] for r in rows)
assert labels == ['backend'], f'pair_key peer list wrong: {labels}'
" || fail "pair_key peer list did not include cross-cwd peer"

  echo "-- peer-inbox: v1.7 auto-label generates adj-noun --"
  local auto_label_out auto_label
  auto_label_out=$(AGENT_COLLAB_INBOX_DB="$pk_db" AGENT_COLLAB_SESSION_KEY=autoA \
    "$agent_collab" session register --cwd "$TMP_ROOT" --agent claude)
  auto_label=$(printf '%s\n' "$auto_label_out" | awk '/^registered:/ {print $2}')
  [[ "$auto_label" =~ ^[a-z]+-[a-z]+$ ]] || fail "auto-label did not match adj-noun shape: $auto_label"

  echo "-- peer-inbox: v1.7 slug uniqueness (pair-keys no collisions in 5000; labels under birthday bound) --"
  python3 <<'PY' || fail "slug collision check failed"
import importlib.util
spec = importlib.util.spec_from_file_location("p", "scripts/peer-inbox-db.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
from collections import Counter
pk_counts = Counter(m.generate_pair_key() for _ in range(5000))
if len(pk_counts) != 5000:
    raise SystemExit(f"pair-key collisions: {5000 - len(pk_counts)} dups in 5000")
# Labels live in a 128*128 = 16,384 space. Birthday paradox at 200 draws
# predicts <2% expected collisions; cap at 5% to catch a shrunk wordlist
# or biased RNG without flunking the paradox itself.
label_counts = Counter(m.generate_label() for _ in range(200))
dup_rate = 1 - (len(label_counts) / 200)
if dup_rate > 0.05:
    raise SystemExit(f"label collision rate too high: {dup_rate:.3%}")
PY

  echo "-- peer-inbox: v1.7 auto-infer --to with single peer --"
  local ai_db="$TMP_ROOT/peer-inbox-ai.db"
  local ai_a="$TMP_ROOT/peer-inbox-ai-a"
  local ai_b="$TMP_ROOT/peer-inbox-ai-b"
  mkdir -p "$ai_a" "$ai_b"
  local ai_a_real ai_b_real
  ai_a_real="$(cd "$ai_a" && pwd -P)"
  ai_b_real="$(cd "$ai_b" && pwd -P)"
  local ai_out ai_key
  ai_out=$(AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiA \
    "$agent_collab" session register --cwd "$ai_a_real" --label alpha --agent claude --new-pair)
  ai_key=$(printf '%s\n' "$ai_out" | grep -oE 'pair_key=[a-z0-9-]+' | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiB \
    "$agent_collab" session register --cwd "$ai_b_real" --label beta --agent codex --pair-key "$ai_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiA \
    "$agent_collab" peer send --cwd "$ai_a_real" --as alpha --message "inferred target" >/dev/null
  AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiB \
    "$agent_collab" peer receive --cwd "$ai_b_real" --as beta --format plain --mark-read \
    | grep -q "inferred target" || fail "auto-inferred send did not reach single peer"

  echo "-- peer-inbox: v1.7 auto-infer --to rejects ambiguity --"
  local ai_c="$TMP_ROOT/peer-inbox-ai-c"
  mkdir -p "$ai_c"
  local ai_c_real
  ai_c_real="$(cd "$ai_c" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiC \
    "$agent_collab" session register --cwd "$ai_c_real" --label gamma --agent gemini --pair-key "$ai_key" >/dev/null
  if AGENT_COLLAB_INBOX_DB="$ai_db" AGENT_COLLAB_SESSION_KEY=aiA \
       "$agent_collab" peer send --cwd "$ai_a_real" --as alpha --message "should fail" >/dev/null 2>&1; then
    fail "ambiguous auto-infer send was accepted"
  fi

  echo "-- peer-inbox: v1.7 register sweeps stale markers for same label --"
  local sw_db="$TMP_ROOT/peer-inbox-sw.db"
  local sw_cwd="$TMP_ROOT/peer-inbox-sw"
  mkdir -p "$sw_cwd"
  local sw_real
  sw_real="$(cd "$sw_cwd" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$sw_db" AGENT_COLLAB_SESSION_KEY=swA \
    "$agent_collab" session register --cwd "$sw_real" --label joseph --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$sw_db" AGENT_COLLAB_SESSION_KEY=swB \
    "$agent_collab" session register --cwd "$sw_real" --label joseph --agent claude --force >/dev/null
  local sw_count
  sw_count=$(ls "$sw_real/.agent-collab/sessions/" | wc -l | tr -d ' ')
  [[ "$sw_count" == "1" ]] || fail "expected 1 marker after re-register, got $sw_count"
  # resolve_self single-marker fallback must succeed now (not hit multi-marker error).
  AGENT_COLLAB_INBOX_DB="$sw_db" AGENT_COLLAB_SESSION_KEY=swB \
    "$agent_collab" peer list --cwd "$sw_real" --json >/dev/null \
    || fail "peer list failed after re-register; sweep didn't clean stale marker"

  echo "-- peer-inbox: v1.7 session close via hook-log fallback --"
  # No --label, no env var, but hook has logged the session_id — close should resolve.
  AGENT_COLLAB_INBOX_DB="$sw_db" \
    "$agent_collab" hook log-session --cwd "$sw_real" --session-id swB >/dev/null
  env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" AGENT_COLLAB_INBOX_DB="$sw_db" \
    "$agent_collab" session close --cwd "$sw_real" >/dev/null \
    || fail "session close should fall back to hook-log when no label and no env var"

  echo "-- peer-inbox: v1.7 codex/gemini auto session-key (no env var needed) --"
  local ak_db="$TMP_ROOT/peer-inbox-ak.db"
  local ak_cwd="$TMP_ROOT/peer-inbox-ak"
  mkdir -p "$ak_cwd"
  local ak_real
  ak_real="$(cd "$ak_cwd" && pwd -P)"
  # Run in a clean env — no session key at all. Codex path must auto-mint.
  env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" AGENT_COLLAB_INBOX_DB="$ak_db" \
    "$agent_collab" session register --cwd "$ak_real" --label ca --agent codex >/dev/null \
    || fail "codex register without env var failed (auto-key shim broken)"
  # Re-register must be idempotent (same session_key) — else user sees spurious collisions.
  local ak_first ak_second
  ak_first=$(env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" AGENT_COLLAB_INBOX_DB="$ak_db" \
    "$agent_collab" session list --json | python3 -c "import json,sys; print(sys.stdin.read())")
  env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" AGENT_COLLAB_INBOX_DB="$ak_db" \
    "$agent_collab" session register --cwd "$ak_real" --label ca --agent codex >/dev/null \
    || fail "codex re-register failed"
  # Claude without any env var must still error (no hook logs in this test).
  if env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" AGENT_COLLAB_INBOX_DB="$ak_db" \
       "$agent_collab" session register --cwd "$ak_real" --label cb --agent claude >/dev/null 2>&1; then
    fail "claude register without session key should error (auto-key is codex/gemini only)"
  fi

  echo "-- peer-inbox: v1.7 cross-runtime env vars (CLAUDE/CODEX/GEMINI_SESSION_ID) --"
  # Each runtime exports a different env var. The helper must pick any of them.
  # Here we simulate: a "claude" session reads CLAUDE_SESSION_ID; a "codex"
  # session reads CODEX_SESSION_ID. Messages flow both ways.
  local xr_db="$TMP_ROOT/peer-inbox-xr.db"
  local xr_dir="$TMP_ROOT/peer-inbox-xr"
  mkdir -p "$xr_dir"
  local xr_real
  xr_real="$(cd "$xr_dir" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$xr_db" CLAUDE_SESSION_ID=xr-claude-1 \
    "$agent_collab" session register --cwd "$xr_real" --label claude-side --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$xr_db" CODEX_SESSION_ID=xr-codex-1 \
    "$agent_collab" session register --cwd "$xr_real" --label codex-side --agent codex >/dev/null
  AGENT_COLLAB_INBOX_DB="$xr_db" CLAUDE_SESSION_ID=xr-claude-1 \
    "$agent_collab" peer send --cwd "$xr_real" --to codex-side --message "claude->codex" >/dev/null
  AGENT_COLLAB_INBOX_DB="$xr_db" CODEX_SESSION_ID=xr-codex-1 \
    "$agent_collab" peer receive --cwd "$xr_real" --format plain --mark-read \
    | grep -q "claude->codex" || fail "claude->codex message did not reach codex session via CODEX_SESSION_ID"
  AGENT_COLLAB_INBOX_DB="$xr_db" CODEX_SESSION_ID=xr-codex-1 \
    "$agent_collab" peer send --cwd "$xr_real" --to claude-side --message "codex->claude" >/dev/null
  AGENT_COLLAB_INBOX_DB="$xr_db" CLAUDE_SESSION_ID=xr-claude-1 \
    "$agent_collab" peer receive --cwd "$xr_real" --format plain --mark-read \
    | grep -q "codex->claude" || fail "codex->claude message did not reach claude session via CLAUDE_SESSION_ID"

  echo "-- peer-inbox: v2.0 peer broadcast fan-out (3-way room) --"
  local bc_db="$TMP_ROOT/peer-inbox-bc.db"
  local bc_a="$TMP_ROOT/peer-inbox-bc-a"
  local bc_b="$TMP_ROOT/peer-inbox-bc-b"
  local bc_c="$TMP_ROOT/peer-inbox-bc-c"
  mkdir -p "$bc_a" "$bc_b" "$bc_c"
  local bc_a_real bc_b_real bc_c_real
  bc_a_real="$(cd "$bc_a" && pwd -P)"
  bc_b_real="$(cd "$bc_b" && pwd -P)"
  bc_c_real="$(cd "$bc_c" && pwd -P)"
  local bc_out bc_key
  bc_out=$(AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcA \
    "$agent_collab" session register --cwd "$bc_a_real" --label alpha --agent claude --new-pair)
  bc_key=$(printf '%s\n' "$bc_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  [[ -n "$bc_key" ]] || fail "3-way broadcast: --new-pair did not emit pair_key"
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcB \
    "$agent_collab" session register --cwd "$bc_b_real" --label beta --agent codex --pair-key "$bc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcC \
    "$agent_collab" session register --cwd "$bc_c_real" --label gamma --agent gemini --pair-key "$bc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcA \
    "$agent_collab" peer broadcast --cwd "$bc_a_real" --as alpha --message "hello room" \
    | grep -q "broadcast to 2 peer(s)" || fail "broadcast did not report 2 recipients"
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcB \
    "$agent_collab" peer receive --cwd "$bc_b_real" --as beta --format plain --mark-read \
    | grep -q "hello room" || fail "broadcast did not reach beta"
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcC \
    "$agent_collab" peer receive --cwd "$bc_c_real" --as gamma --format plain --mark-read \
    | grep -q "hello room" || fail "broadcast did not reach gamma"

  echo "-- peer-inbox: v2.0 peer broadcast 4-way stress --"
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcD \
    "$agent_collab" session register --cwd "$TMP_ROOT/peer-inbox-bc-d" --label delta --agent claude --pair-key "$bc_key" >/dev/null 2>&1 \
    || { mkdir -p "$TMP_ROOT/peer-inbox-bc-d"; AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcD \
         "$agent_collab" session register --cwd "$TMP_ROOT/peer-inbox-bc-d" --label delta --agent claude --pair-key "$bc_key" >/dev/null; }
  AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY=bcA \
    "$agent_collab" peer broadcast --cwd "$bc_a_real" --as alpha --message "4-way hello" \
    | grep -q "broadcast to 3 peer(s)" || fail "4-way broadcast did not report 3 recipients"
  for label in beta gamma delta; do
    local key_var
    case "$label" in beta) key_var=bcB;; gamma) key_var=bcC;; delta) key_var=bcD;; esac
    local cwd_var
    case "$label" in
      beta) cwd_var="$bc_b_real";;
      gamma) cwd_var="$bc_c_real";;
      delta) cwd_var="$TMP_ROOT/peer-inbox-bc-d";;
    esac
    AGENT_COLLAB_INBOX_DB="$bc_db" AGENT_COLLAB_SESSION_KEY="$key_var" \
      "$agent_collab" peer receive --cwd "$cwd_var" --as "$label" --format plain --mark-read \
      | grep -q "4-way hello" || fail "4-way broadcast did not reach $label"
  done

  echo "-- peer-inbox: v2.0 peer web unified room view (--pair-key) --"
  local rw_db="$TMP_ROOT/peer-inbox-rooms.db"
  local rw_a="$TMP_ROOT/peer-inbox-rw-a"
  local rw_b="$TMP_ROOT/peer-inbox-rw-b"
  local rw_c="$TMP_ROOT/peer-inbox-rw-c"
  mkdir -p "$rw_a" "$rw_b" "$rw_c"
  local rw_a_real rw_b_real rw_c_real
  rw_a_real="$(cd "$rw_a" && pwd -P)"
  rw_b_real="$(cd "$rw_b" && pwd -P)"
  rw_c_real="$(cd "$rw_c" && pwd -P)"
  local rw_out rw_key
  rw_out=$(AGENT_COLLAB_INBOX_DB="$rw_db" AGENT_COLLAB_SESSION_KEY=rwA \
    "$agent_collab" session register --cwd "$rw_a_real" --label alpha --agent claude --new-pair)
  rw_key=$(printf '%s\n' "$rw_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$rw_db" AGENT_COLLAB_SESSION_KEY=rwB \
    "$agent_collab" session register --cwd "$rw_b_real" --label beta --agent codex --pair-key "$rw_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rw_db" AGENT_COLLAB_SESSION_KEY=rwC \
    "$agent_collab" session register --cwd "$rw_c_real" --label gamma --agent gemini --pair-key "$rw_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rw_db" AGENT_COLLAB_SESSION_KEY=rwA \
    "$agent_collab" peer broadcast --cwd "$rw_a_real" --as alpha --message "one" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rw_db" AGENT_COLLAB_SESSION_KEY=rwB \
    "$agent_collab" peer send --cwd "$rw_b_real" --as beta --to alpha --message "two" >/dev/null

  AGENT_COLLAB_INBOX_DB="$rw_db" \
    "$agent_collab" peer web --cwd "$rw_a_real" --pair-key "$rw_key" --port 8799 \
    >"$TMP_ROOT/peer-inbox-rooms.log" 2>&1 &
  local rw_pid=$!
  sleep 0.4

  curl -s "http://127.0.0.1:8799/scope.json" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['mode'] == 'pair_key', d
assert d['pair_key'] == '$rw_key', d
" || { kill $rw_pid 2>/dev/null || true; fail "/scope.json shape wrong"; }

  curl -s "http://127.0.0.1:8799/rooms.json" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['pair_key'] == '$rw_key'
assert len(d['rooms']) == 1, d
r = d['rooms'][0]
assert r['key'] == '$rw_key'
labels = sorted(m['label'] for m in r['members'])
assert labels == ['alpha', 'beta', 'gamma'], labels
assert r['total'] >= 3, r
" || { kill $rw_pid 2>/dev/null || true; fail "/rooms.json shape wrong"; }

  curl -s "http://127.0.0.1:8799/messages.json?pair_key=$rw_key&after=0" \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['pair_key'] == '$rw_key'
bodies = [m['body'] for m in d['messages']]
assert 'one' in bodies and 'two' in bodies, bodies
" || { kill $rw_pid 2>/dev/null || true; fail "/messages.json?pair_key interleave wrong"; }

  # Mismatched pair_key must 400.
  local rw_err
  rw_err=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:8799/messages.json?pair_key=bogus-key")
  [[ "$rw_err" == "400" ]] || { kill $rw_pid 2>/dev/null || true; fail "mismatched pair_key did not 400 (got $rw_err)"; }

  # rooms.json must 400 in cwd-only mode.
  AGENT_COLLAB_INBOX_DB="$web_db" \
    "$agent_collab" peer web --cwd "$web_real" --port 8800 \
    >"$TMP_ROOT/peer-inbox-rooms-cwd.log" 2>&1 &
  local rw_cwd_pid=$!
  sleep 0.3
  local rw_cwd_err
  rw_cwd_err=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:8800/rooms.json")
  [[ "$rw_cwd_err" == "400" ]] || { kill $rw_pid $rw_cwd_pid 2>/dev/null || true; fail "/rooms.json in cwd mode should 400 (got $rw_cwd_err)"; }

  kill $rw_pid $rw_cwd_pid 2>/dev/null || true
  wait $rw_pid $rw_cwd_pid 2>/dev/null || true

  echo "-- peer-inbox: v2.0 room_key scoping — pre-room history doesn't bleed --"
  local bl_db="$TMP_ROOT/peer-inbox-bleed.db"
  local bl_a="$TMP_ROOT/peer-inbox-bl-a"
  local bl_b="$TMP_ROOT/peer-inbox-bl-b"
  local bl_c="$TMP_ROOT/peer-inbox-bl-c"
  mkdir -p "$bl_a" "$bl_b" "$bl_c"
  local bl_a_real bl_b_real bl_c_real
  bl_a_real="$(cd "$bl_a" && pwd -P)"
  bl_b_real="$(cd "$bl_b" && pwd -P)"
  bl_c_real="$(cd "$bl_c" && pwd -P)"

  # Phase 1: alice and bob chat 1:1 in a simple cwd, no pair_key.
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl1 \
    "$agent_collab" session register --cwd "$bl_a_real" --label alice --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl2 \
    "$agent_collab" session register --cwd "$bl_a_real" --label bob --agent claude >/dev/null
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl1 \
    "$agent_collab" peer send --cwd "$bl_a_real" --as alice --to bob --message "private history one" >/dev/null
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl2 \
    "$agent_collab" peer send --cwd "$bl_a_real" --as bob --to alice --message "private history two" >/dev/null

  # Phase 2: close the cwd pair, drop the sessions, then stand up a new
  # 3-person pair_key room that happens to reuse the same labels.
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl1 \
    "$agent_collab" session close --cwd "$bl_a_real" --label alice >/dev/null
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl2 \
    "$agent_collab" session close --cwd "$bl_a_real" --label bob >/dev/null

  local bl_out bl_key
  bl_out=$(AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl3 \
    "$agent_collab" session register --cwd "$bl_a_real" --label alice --agent claude --new-pair)
  bl_key=$(printf '%s\n' "$bl_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl4 \
    "$agent_collab" session register --cwd "$bl_b_real" --label bob --agent claude --pair-key "$bl_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl5 \
    "$agent_collab" session register --cwd "$bl_c_real" --label carol --agent claude --pair-key "$bl_key" >/dev/null

  AGENT_COLLAB_INBOX_DB="$bl_db" AGENT_COLLAB_SESSION_KEY=bl3 \
    "$agent_collab" peer broadcast --cwd "$bl_a_real" --as alice --message "room hello" >/dev/null

  # Start the viewer scoped to the new room.
  AGENT_COLLAB_INBOX_DB="$bl_db" \
    "$agent_collab" peer web --cwd "$bl_a_real" --pair-key "$bl_key" --port 8801 \
    >"$TMP_ROOT/peer-inbox-bleed.log" 2>&1 &
  local bl_pid=$!
  sleep 0.4

  curl -s "http://127.0.0.1:8801/rooms.json" | python3 -c "
import json, sys
d = json.load(sys.stdin)
r = d['rooms'][0]
# Only the new room_hello broadcast (→ bob, → carol = 2 rows). Old
# 1:1 alice<->bob history must NOT show up.
assert r['total'] == 2, r
" || { kill $bl_pid 2>/dev/null || true; fail "old history bled into /rooms.json total"; }

  curl -s "http://127.0.0.1:8801/messages.json?pair_key=$bl_key&after=0" | python3 -c "
import json, sys
d = json.load(sys.stdin)
bodies = [m['body'] for m in d['messages']]
assert 'room hello' in bodies, bodies
assert 'private history one' not in bodies, bodies
assert 'private history two' not in bodies, bodies
" || { kill $bl_pid 2>/dev/null || true; fail "old 1:1 history leaked into room stream"; }

  kill $bl_pid 2>/dev/null || true
  wait $bl_pid 2>/dev/null || true

  echo "-- peer-inbox: v2.0 peer broadcast --to multicast (subset) --"
  local mc_db="$TMP_ROOT/peer-inbox-mc.db"
  local mc_a="$TMP_ROOT/peer-inbox-mc-a"
  local mc_b="$TMP_ROOT/peer-inbox-mc-b"
  local mc_c="$TMP_ROOT/peer-inbox-mc-c"
  local mc_d="$TMP_ROOT/peer-inbox-mc-d"
  mkdir -p "$mc_a" "$mc_b" "$mc_c" "$mc_d"
  local mc_a_real mc_b_real mc_c_real mc_d_real
  mc_a_real="$(cd "$mc_a" && pwd -P)"
  mc_b_real="$(cd "$mc_b" && pwd -P)"
  mc_c_real="$(cd "$mc_c" && pwd -P)"
  mc_d_real="$(cd "$mc_d" && pwd -P)"
  local mc_out mc_key
  mc_out=$(AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcA \
    "$agent_collab" session register --cwd "$mc_a_real" --label alpha --agent claude --new-pair)
  mc_key=$(printf '%s\n' "$mc_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcB \
    "$agent_collab" session register --cwd "$mc_b_real" --label beta --agent claude --pair-key "$mc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcC \
    "$agent_collab" session register --cwd "$mc_c_real" --label gamma --agent claude --pair-key "$mc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcD \
    "$agent_collab" session register --cwd "$mc_d_real" --label delta --agent claude --pair-key "$mc_key" >/dev/null

  # Multicast to beta + gamma only; delta must NOT receive.
  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcA \
    "$agent_collab" peer broadcast --cwd "$mc_a_real" --as alpha \
      --to beta --to gamma --message "hey subset" \
    | grep -q "broadcast to 2 peer(s)" || fail "multicast did not report 2 recipients"

  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcB \
    "$agent_collab" peer receive --cwd "$mc_b_real" --as beta --format plain --mark-read \
    | grep -q "hey subset" || fail "multicast did not reach beta"
  AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcC \
    "$agent_collab" peer receive --cwd "$mc_c_real" --as gamma --format plain --mark-read \
    | grep -q "hey subset" || fail "multicast did not reach gamma"
  if AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcD \
       "$agent_collab" peer receive --cwd "$mc_d_real" --as delta --format plain --mark-read \
       | grep -q "hey subset"; then
    fail "multicast leaked to delta (should have been excluded)"
  fi

  # Multicast counts as ONE room turn (not 2).
  local mc_turns
  mc_turns=$(sqlite3 "$mc_db" "SELECT turn_count FROM peer_rooms WHERE room_key='pk:$mc_key';")
  [[ "$mc_turns" == "1" ]] || fail "multicast should bump room by 1 turn, got $mc_turns"

  # Unknown label in multicast must error cleanly.
  if AGENT_COLLAB_INBOX_DB="$mc_db" AGENT_COLLAB_SESSION_KEY=mcA \
       "$agent_collab" peer broadcast --cwd "$mc_a_real" --as alpha \
         --to beta --to nonexistent --message "oops" >/dev/null 2>&1; then
    fail "multicast to unknown label was accepted"
  fi

  echo "-- peer-inbox: v2.0 room-level turn cap blocks every sender --"
  local rc_db="$TMP_ROOT/peer-inbox-rc.db"
  local rc_a="$TMP_ROOT/peer-inbox-rc-a"
  local rc_b="$TMP_ROOT/peer-inbox-rc-b"
  local rc_c="$TMP_ROOT/peer-inbox-rc-c"
  mkdir -p "$rc_a" "$rc_b" "$rc_c"
  local rc_a_real rc_b_real rc_c_real
  rc_a_real="$(cd "$rc_a" && pwd -P)"
  rc_b_real="$(cd "$rc_b" && pwd -P)"
  rc_c_real="$(cd "$rc_c" && pwd -P)"
  local rc_out rc_key
  rc_out=$(AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcA \
    "$agent_collab" session register --cwd "$rc_a_real" --label alpha --agent claude --new-pair)
  rc_key=$(printf '%s\n' "$rc_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcB \
    "$agent_collab" session register --cwd "$rc_b_real" --label beta --agent codex --pair-key "$rc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcC \
    "$agent_collab" session register --cwd "$rc_c_real" --label gamma --agent gemini --pair-key "$rc_key" >/dev/null
  # Cap = 3 room turns total. alpha→beta, beta→alpha, alpha→gamma = 3 turns.
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcA AGENT_COLLAB_MAX_PAIR_TURNS=3 \
    "$agent_collab" peer send --cwd "$rc_a_real" --as alpha --to beta --message "1" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcB AGENT_COLLAB_MAX_PAIR_TURNS=3 \
    "$agent_collab" peer send --cwd "$rc_b_real" --as beta --to alpha --message "2" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcA AGENT_COLLAB_MAX_PAIR_TURNS=3 \
    "$agent_collab" peer send --cwd "$rc_a_real" --as alpha --to gamma --message "3" >/dev/null
  # Fourth send from ANY sender (gamma here) must be blocked.
  if AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcC AGENT_COLLAB_MAX_PAIR_TURNS=3 \
       "$agent_collab" peer send --cwd "$rc_c_real" --as gamma --to alpha --message "4" >/dev/null 2>&1; then
    fail "room cap did not block 4th message from a different sender"
  fi
  # Broadcast past the cap is also blocked (one turn that would put us over).
  if AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcB AGENT_COLLAB_MAX_PAIR_TURNS=3 \
       "$agent_collab" peer broadcast --cwd "$rc_b_real" --as beta --message "bcast past cap" >/dev/null 2>&1; then
    fail "room cap did not block broadcast"
  fi
  # Reset by pair_key and verify a fresh send goes through.
  AGENT_COLLAB_INBOX_DB="$rc_db" \
    "$agent_collab" peer reset --pair-key "$rc_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$rc_db" AGENT_COLLAB_SESSION_KEY=rcA AGENT_COLLAB_MAX_PAIR_TURNS=3 \
    "$agent_collab" peer send --cwd "$rc_a_real" --as alpha --to beta --message "revived" >/dev/null \
    || fail "send after peer reset --pair-key was rejected"

  echo "-- peer-inbox: v2.0 [[end]] terminates the whole room --"
  local te_db="$TMP_ROOT/peer-inbox-te.db"
  local te_a="$TMP_ROOT/peer-inbox-te-a"
  local te_b="$TMP_ROOT/peer-inbox-te-b"
  local te_c="$TMP_ROOT/peer-inbox-te-c"
  mkdir -p "$te_a" "$te_b" "$te_c"
  local te_a_real te_b_real te_c_real
  te_a_real="$(cd "$te_a" && pwd -P)"
  te_b_real="$(cd "$te_b" && pwd -P)"
  te_c_real="$(cd "$te_c" && pwd -P)"
  local te_out te_key
  te_out=$(AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teA \
    "$agent_collab" session register --cwd "$te_a_real" --label alpha --agent claude --new-pair)
  te_key=$(printf '%s\n' "$te_out" | grep -oE 'pair_key=[a-z0-9-]+' | head -1 | cut -d= -f2)
  AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teB \
    "$agent_collab" session register --cwd "$te_b_real" --label beta --agent codex --pair-key "$te_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teC \
    "$agent_collab" session register --cwd "$te_c_real" --label gamma --agent gemini --pair-key "$te_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teA \
    "$agent_collab" peer send --cwd "$te_a_real" --as alpha --to beta --message "shutting down [[end]]" >/dev/null
  if AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teC \
       "$agent_collab" peer send --cwd "$te_c_real" --as gamma --to alpha --message "can i?" >/dev/null 2>&1; then
    fail "room termination did not block non-sender peers"
  fi
  AGENT_COLLAB_INBOX_DB="$te_db" \
    "$agent_collab" peer reset --pair-key "$te_key" >/dev/null
  AGENT_COLLAB_INBOX_DB="$te_db" AGENT_COLLAB_SESSION_KEY=teC \
    "$agent_collab" peer send --cwd "$te_c_real" --as gamma --to alpha --message "back" >/dev/null \
    || fail "send after room reset was rejected"

  echo "-- peer-inbox: v2.0 peer broadcast errors when no peers --"
  local solo_db="$TMP_ROOT/peer-inbox-bc-solo.db"
  local solo_repo="$TMP_ROOT/peer-inbox-bc-solo"
  mkdir -p "$solo_repo"
  local solo_real; solo_real="$(cd "$solo_repo" && pwd -P)"
  AGENT_COLLAB_INBOX_DB="$solo_db" AGENT_COLLAB_SESSION_KEY=soloA \
    "$agent_collab" session register --cwd "$solo_real" --label solo --agent claude >/dev/null
  if AGENT_COLLAB_INBOX_DB="$solo_db" AGENT_COLLAB_SESSION_KEY=soloA \
       "$agent_collab" peer broadcast --cwd "$solo_real" --as solo --message "nobody home" >/dev/null 2>&1; then
    fail "broadcast with no peers should error"
  fi

  echo "peer-inbox: all checks PASS"
}

mkdir -p "$TEST_HOME/.claude" "$TEST_HOME/.codex" "$TEST_HOME/.gemini" "$TEST_BIN" "$SAMPLE_REPO/docs"
SAMPLE_REPO_REAL="$(cd "$SAMPLE_REPO" && pwd -P)"
mkdir -p "$PYTHON_ONLY_BIN"
ln -s "$(command -v python3)" "$PYTHON_ONLY_BIN/python3"
for tool in bash basename cat date dirname grep head mkdir mktemp mv rm sleep tr wc; do
  ln -s "$(command -v "$tool")" "$PYTHON_ONLY_BIN/$tool"
done

cat > "$TEST_HOME/.claude/CLAUDE.md" <<'EOF'
# Existing Claude Defaults
keep this content
EOF

cat > "$TEST_HOME/.codex/AGENTS.md" <<'EOF'
# Existing Codex Defaults
keep this content
EOF

cat > "$TEST_HOME/.gemini/GEMINI.md" <<'EOF'
# Existing Gemini Defaults
keep this content
EOF

export HOME="$TEST_HOME"
export PATH="$TEST_BIN:$PATH"
export AGENT_COLLAB_BIN_DIR="$TEST_BIN"

"$PROJECT_ROOT/scripts/install-global-protocol" >/dev/null

assert_file "$TEST_HOME/.claude/CLAUDE.md"
assert_file "$TEST_HOME/.codex/AGENTS.md"
assert_executable "$TEST_BIN/agent-collab"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "# Existing Claude Defaults"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "<!-- BEGIN agent-collaboration -->"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "# Existing Codex Defaults"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "<!-- BEGIN agent-collaboration -->"
assert_glob_exists "$TEST_HOME/.claude/CLAUDE.md.bak.*"
assert_glob_exists "$TEST_HOME/.codex/AGENTS.md.bak.*"
assert_file "$TEST_HOME/.gemini/GEMINI.md"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "# Existing Gemini Defaults"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "<!-- BEGIN agent-collaboration -->"
assert_glob_exists "$TEST_HOME/.gemini/GEMINI.md.bak.*"

"$PROJECT_ROOT/scripts/doctor-global-protocol" >/dev/null

cat > "$SAMPLE_REPO/AGENT-COLLABORATION.md" <<'EOF'
# Agent Collaboration

Sample repo guide.
EOF

cat > "$SAMPLE_REPO/.agent-collab.env" <<'EOF'
AGENT_COLLAB_GUIDE=AGENT-COLLABORATION.md
AGENT_COLLAB_REVIEW_DIR=.agent-collab/reviews
AGENT_COLLAB_DEFAULT_CHALLENGER=claude
AGENT_COLLAB_TIMEOUT_SECONDS=15
AGENT_COLLAB_CLAUDE_TIMEOUT_SECONDS=12
AGENT_COLLAB_CODEX_TIMEOUT_SECONDS=9
AGENT_COLLAB_GEMINI_TIMEOUT_SECONDS=9
AGENT_COLLAB_CLAUDE_EFFORT=low
EOF

cat > "$SAMPLE_REPO/docs/scope.md" <<'EOF'
# Scope

Sample scope.
EOF

python3 - <<'EOF' > "$SAMPLE_REPO/docs/large-context.txt"
print("large context line")
for _ in range(3500):
  print("0123456789abcdef" * 4)
EOF

dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run." --dry-run)"
[[ "$dry_run_output" == *"challenger: claude"* ]] || fail "dry run did not use default challenger"
[[ "$dry_run_output" == *"repo_root: $SAMPLE_REPO_REAL"* ]] || fail "dry run did not detect sample repo root"
[[ "$dry_run_output" == *"Repo-local collaboration guide: AGENT-COLLABORATION.md"* ]] || fail "dry run did not include repo guide"
[[ "$dry_run_output" == *"timeout_seconds: 12"* ]] || fail "dry run did not include claude timeout override"

guide_scope_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" verify --scope AGENT-COLLABORATION.md --dry-run)"
[[ "$(grep -c '^----- BEGIN FILE: AGENT-COLLABORATION.md -----$' <<<"$guide_scope_dry_run_output")" == "1" ]] || fail "dry run should inline guide/scope overlap only once"

guide_context_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" verify --context AGENT-COLLABORATION.md --dry-run)"
[[ "$(grep -c '^----- BEGIN FILE: AGENT-COLLABORATION.md -----$' <<<"$guide_context_dry_run_output")" == "1" ]] || fail "dry run should inline guide/context overlap only once"

codex_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Dry run codex." --dry-run)"
[[ "$codex_dry_run_output" == *"timeout_seconds: 9"* ]] || fail "dry run did not include codex timeout override"

cli_override_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run override." --timeout-seconds 3 --dry-run)"
[[ "$cli_override_dry_run_output" == *"timeout_seconds: 3"* ]] || fail "dry run did not honor timeout CLI override"

disabled_timeout_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run disabled timeout." --timeout-seconds 0 --dry-run)"
[[ "$disabled_timeout_dry_run_output" == *"timeout_seconds: 0"* ]] || fail "dry run did not allow timeout disable"
[[ "$disabled_timeout_dry_run_output" == *"timeout_backend: disabled"* ]] || fail "dry run did not report disabled timeout backend"

gemini_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Dry run gemini." --dry-run)"
[[ "$gemini_dry_run_output" == *"timeout_seconds: 9"* ]] || fail "dry run did not include gemini timeout override"

mkdir -p "$TMP_ROOT/mock-bin"
cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Review the provided file contents directly."* ]] || exit 1
[[ "$stdin_payload" == *"----- BEGIN FILE: AGENT-COLLABORATION.md -----"* ]] || exit 1
[[ "$stdin_payload" == *"Sample repo guide."* ]] || exit 1
[[ "$stdin_payload" == *"----- BEGIN FILE: docs/scope.md -----"* ]] || exit 1
[[ "$stdin_payload" == *"Sample scope."* ]] || exit 1

expect_tools_arg=0
saw_tools_arg=0
saw_effort_flag=0
saw_low_effort=0
for arg in "$@"; do
  if (( expect_tools_arg )); then
    [[ "$arg" == "Read" ]] || exit 1
    saw_tools_arg=1
    expect_tools_arg=0
    continue
  fi

  if [[ "$arg" == "--tools" ]]; then
    expect_tools_arg=1
    continue
  fi

  if [[ "$arg" == "--effort" ]]; then
    saw_effort_flag=1
    continue
  fi

  if (( saw_effort_flag )); then
    [[ "$arg" == "low" ]] || exit 1
    saw_low_effort=1
    saw_effort_flag=0
  fi
done

(( ! saw_tools_arg )) || exit 1
(( saw_low_effort )) || exit 1
printf 'mock claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

claude_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Inline prompt check."
)"
[[ "$claude_run_output" == review:\ * ]] || fail "claude challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"----- BEGIN FILE: docs/large-context.txt -----"* ]] || exit 1
[[ "$stdin_payload" == *"[truncated: showing first"* ]] || exit 1
[[ "$stdin_payload" == *"Use the Read tool only if you need missing content."* ]] || exit 1

expect_tools_arg=0
saw_tools_arg=0
for arg in "$@"; do
  if (( expect_tools_arg )); then
    [[ "$arg" == "Read" ]] || exit 1
    saw_tools_arg=1
    expect_tools_arg=0
    continue
  fi

  if [[ "$arg" == "--tools" ]]; then
    expect_tools_arg=1
  fi
done

(( saw_tools_arg )) || exit 1
printf 'truncated claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

truncated_claude_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" verify --challenger claude --scope docs/scope.md --context docs/large-context.txt --prompt "Truncation check."
)"
[[ "$truncated_claude_run_output" == review:\ * ]] || fail "claude verify run did not report review output for truncated context"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Python fallback check."* ]] || exit 1
sleep 1
printf 'python fallback claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

python_fallback_dry_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Python fallback dry run." --dry-run
)"
[[ "$python_fallback_dry_run_output" == *"timeout_backend: python3"* ]] || fail "claude python fallback dry run did not force python3 backend"

python_fallback_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Python fallback check." --timeout-seconds 2
)"
[[ "$python_fallback_output" == review:\ * ]] || fail "claude challenger run did not succeed with python3 timeout fallback"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
printf '\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

if (
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Whitespace failure check."
); then
  fail "whitespace-only challenger output should fail"
fi

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
sleep 5
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

if claude_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude timeout test should fail"
fi
[[ "$claude_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude timeout failure did not report timeout"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
trap '' TERM
cat >/dev/null
sleep 20
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

SECONDS=0
if claude_hard_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude hard timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude hard-timeout test should fail"
fi
claude_hard_timeout_elapsed="$SECONDS"
[[ "$claude_hard_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude hard-timeout failure did not report timeout"
(( claude_hard_timeout_elapsed < 10 )) || fail "claude hard-timeout path took too long: ${claude_hard_timeout_elapsed}s"

SECONDS=0
if claude_python_hard_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude python hard timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude python hard-timeout test should fail"
fi
claude_python_hard_timeout_elapsed="$SECONDS"
[[ "$claude_python_hard_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude python hard-timeout failure did not report timeout"
(( claude_python_hard_timeout_elapsed < 10 )) || fail "claude python hard-timeout path took too long: ${claude_python_hard_timeout_elapsed}s"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
last=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
  last="$arg"
done

[[ -n "$out" ]] || exit 1
[[ "$last" == "-" ]] || exit 1

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Codex stdin check."* ]] || exit 1

printf 'mock codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

codex_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex stdin check."
)"
[[ "$codex_run_output" == review:\ * ]] || fail "codex challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
done

cat >/dev/null
sleep 1
printf 'python fallback codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

codex_python_fallback_dry_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python fallback dry run." --dry-run
)"
[[ "$codex_python_fallback_dry_run_output" == *"timeout_backend: python3"* ]] || fail "codex python fallback dry run did not force python3 backend"

codex_python_success_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python success check." --timeout-seconds 2
)"
[[ "$codex_python_success_output" == review:\ * ]] || fail "codex challenger run did not succeed with python3 timeout fallback"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
done

trap '' TERM
cat >/dev/null
sleep 20
printf 'late output\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

SECONDS=0
if codex_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "codex timeout test should fail"
fi
codex_timeout_elapsed="$SECONDS"
[[ "$codex_timeout_output" == *"codex review timed out after 1s"* ]] || fail "codex timeout failure did not report timeout"
(( codex_timeout_elapsed < 10 )) || fail "codex hard-timeout path took too long: ${codex_timeout_elapsed}s"

SECONDS=0
if codex_python_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "codex python timeout test should fail"
fi
codex_python_timeout_elapsed="$SECONDS"
[[ "$codex_python_timeout_output" == *"codex review timed out after 1s"* ]] || fail "codex python timeout failure did not report timeout"
(( codex_python_timeout_elapsed < 10 )) || fail "codex python hard-timeout path took too long: ${codex_python_timeout_elapsed}s"

# Direction: Claude/Codex -> Gemini (gemini as challenger)
cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini stdin check."* ]] || exit 1

printf 'mock gemini review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

gemini_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini stdin check."
)"
[[ "$gemini_run_output" == review:\ * ]] || fail "gemini challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
trap '' TERM
cat >/dev/null
sleep 20
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

SECONDS=0
if gemini_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "gemini timeout test should fail"
fi
gemini_timeout_elapsed="$SECONDS"
[[ "$gemini_timeout_output" == *"gemini review timed out after 1s"* ]] || fail "gemini timeout failure did not report timeout"
(( gemini_timeout_elapsed < 10 )) || fail "gemini hard-timeout path took too long: ${gemini_timeout_elapsed}s"

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
printf '\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

if (
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini whitespace failure check."
); then
  fail "gemini whitespace-only challenger output should fail"
fi

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
sleep 1
printf 'python fallback gemini review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

gemini_python_fallback_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini python fallback check." --timeout-seconds 2
)"
[[ "$gemini_python_fallback_output" == review:\ * ]] || fail "gemini challenger run did not succeed with python3 timeout fallback"

# Direction: Gemini -> Claude (claude as challenger, from gemini lead perspective)
cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini-led claude review."* ]] || exit 1

saw_effort_flag=0
saw_low_effort=0
for arg in "$@"; do
  if [[ "$arg" == "--effort" ]]; then
    saw_effort_flag=1
    continue
  fi
  if (( saw_effort_flag )); then
    [[ "$arg" == "low" ]] || exit 1
    saw_low_effort=1
    saw_effort_flag=0
  fi
done
(( saw_low_effort )) || exit 1

printf 'gemini-led claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

gemini_to_claude_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Gemini-led claude review."
)"
[[ "$gemini_to_claude_output" == review:\ * ]] || fail "gemini->claude direction did not report review output"

# Direction: Gemini -> Codex (codex as challenger, from gemini lead perspective)
cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
last=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
  last="$arg"
done

[[ -n "$out" ]] || exit 1
[[ "$last" == "-" ]] || exit 1

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini-led codex review."* ]] || exit 1

printf 'gemini-led codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

gemini_to_codex_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Gemini-led codex review."
)"
[[ "$gemini_to_codex_output" == review:\ * ]] || fail "gemini->codex direction did not report review output"

"$PROJECT_ROOT/scripts/install-global-protocol" --uninstall >/dev/null

assert_file "$TEST_HOME/.claude/CLAUDE.md"
assert_file "$TEST_HOME/.codex/AGENTS.md"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "# Existing Claude Defaults"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "# Existing Codex Defaults"
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.claude/CLAUDE.md"; then
  fail "managed block still present in Claude file after uninstall"
fi
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.codex/AGENTS.md"; then
  fail "managed block still present in Codex file after uninstall"
fi
assert_file "$TEST_HOME/.gemini/GEMINI.md"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "# Existing Gemini Defaults"
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.gemini/GEMINI.md"; then
  fail "managed block still present in Gemini file after uninstall"
fi
[[ ! -e "$TEST_BIN/agent-collab" ]] || fail "agent-collab command still present after uninstall"
assert_glob_exists "$TEST_BIN/agent-collab.bak.*"

run_peer_inbox_tests

echo "PASS: smoke"
