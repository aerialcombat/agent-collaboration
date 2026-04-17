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
