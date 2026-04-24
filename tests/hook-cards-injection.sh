#!/usr/bin/env bash
# tests/hook-cards-injection.sh — v3.9 hook kanban injection.
#
# Verifies that the Go hook binary (go/cmd/hook) surfaces the agent's
# card queue alongside the peer-inbox envelope. Four scenarios:
#
#   H1 — session with a claimed + role-pullable card emits a
#        <kanban> block in additionalContext.
#   H2 — session with no pair_key room emits nothing kanban-related
#        (fail-silent — no claims, no pullable, no noise).
#   H3 — pullable requires role match: a card needing a different role
#        stays hidden.
#   H4 — blocked-but-claimed card still shows in claimed (status is
#        the gate for "claimed by you", not readiness).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOOK_BIN="$PROJECT_ROOT/go/bin/peer-inbox-hook"
MIG_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
PI_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
MIG_DIR="$PROJECT_ROOT/migrations/sqlite"

for b in "$HOOK_BIN" "$MIG_BIN" "$PI_BIN"; do
  [ -x "$b" ] || { echo "skip: missing $b"; exit 0; }
done
command -v sqlite3 >/dev/null 2>&1 || { echo "skip: sqlite3 not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

TMP_RAW="$(mktemp -d -t hook-cards.XXXXXX)"
TMP="$(cd "$TMP_RAW" && pwd -P)"
DB="$TMP/db.sqlite"
export AGENT_COLLAB_INBOX_DB="$DB"

cleanup() { rm -rf "$TMP_RAW"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

sqlite3 "$DB" >/dev/null <<'SQL'
PRAGMA journal_mode=WAL;
CREATE TABLE sessions (cwd TEXT NOT NULL, label TEXT NOT NULL, agent TEXT NOT NULL,
  role TEXT, session_key TEXT, channel_socket TEXT, pair_key TEXT,
  started_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, PRIMARY KEY (cwd, label));
CREATE TABLE inbox (id INTEGER PRIMARY KEY AUTOINCREMENT, to_cwd TEXT NOT NULL,
  to_label TEXT NOT NULL, from_cwd TEXT NOT NULL, from_label TEXT NOT NULL,
  body TEXT NOT NULL, created_at TEXT NOT NULL, read_at TEXT, room_key TEXT);
CREATE TABLE peer_rooms (room_key TEXT PRIMARY KEY, pair_key TEXT,
  turn_count INTEGER NOT NULL DEFAULT 0, terminated_at TEXT, terminated_by TEXT);
SQL
"$MIG_BIN" -driver sqlite -dsn "$DB" -dir "$MIG_DIR" up >/dev/null 2>&1 \
  || fail "schema migrate"

NOW="$(python3 -c 'import datetime; print(datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))')"

# Helper: write a session marker so ResolveSelf finds the session.
write_marker() {
  local cwd="$1" key="$2" label="$3"
  mkdir -p "$cwd/.agent-collab/sessions"
  python3 -c "
import hashlib, json, os, sys
cwd, key, label = sys.argv[1], sys.argv[2], sys.argv[3]
h = hashlib.sha256(key.encode()).hexdigest()[:16]
open(os.path.join(cwd, '.agent-collab', 'sessions', h + '.json'), 'w').write(
    json.dumps({'cwd':cwd,'label':label,'session_key':key}))
" "$cwd" "$key" "$label"
}

run_hook() {
  local cwd="$1" key="$2"
  CLAUDE_SESSION_ID="$key" \
    "$HOOK_BIN" "$cwd" 2>/dev/null
}

# ---------- H1: claimed + pullable → kanban block emitted -------------------
CWD1="$(cd "$(mktemp -d)" && pwd -P)"
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO peer_rooms (room_key, pair_key, turn_count, home_host)
  VALUES ('pk:H1-pk','H1-pk',0,'testhost');
INSERT INTO sessions (cwd,label,agent,role,session_key,channel_socket,pair_key,
                     started_at,last_seen_at,receive_mode,auth_token,auth_token_rotated_at)
  VALUES ('$CWD1','backend-claude','claude','backend','h1-key',NULL,'H1-pk',
          '$NOW','$NOW','interactive','tok-e8d138be7debdc91','$NOW');
SQL
"$PI_BIN" card-create --pair-key H1-pk --title "H1 claimed card" \
    --created-by owner --needs-role backend --priority 1 >/dev/null
"$PI_BIN" card-claim --id 1 --as backend-claude >/dev/null
"$PI_BIN" card-create --pair-key H1-pk --title "H1 pullable card" \
    --created-by owner --needs-role backend >/dev/null
write_marker "$CWD1" "h1-key" "backend-claude"

out="$(run_hook "$CWD1" "h1-key")"
echo "$out" | grep -q '"hookSpecificOutput"' \
  || fail "H1 envelope missing (out=$out)"
echo "$out" | grep -q '<kanban ' \
  || fail "H1 kanban block missing (out=$out)"
echo "$out" | grep -q 'claimed by you' \
  || fail "H1 claimed section missing (out=$out)"
echo "$out" | grep -q 'ready for role=backend' \
  || fail "H1 pullable section missing (out=$out)"
echo "$out" | grep -q '#1 \[in_progress\]' \
  || fail "H1 claimed card id/status missing (out=$out)"
echo "$out" | grep -q '#2 H1 pullable card' \
  || fail "H1 pullable card missing (out=$out)"
echo "H1 ok — claimed + pullable surfaced in hook envelope"

# ---------- H2: no pair_key → no kanban ------------------------------------
CWD2="$(cd "$(mktemp -d)" && pwd -P)"
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO sessions (cwd,label,agent,role,session_key,channel_socket,pair_key,
                     started_at,last_seen_at,receive_mode,auth_token,auth_token_rotated_at)
  VALUES ('$CWD2','rogue-claude','claude','backend','h2-key',NULL,NULL,
          '$NOW','$NOW','interactive','tok-012881fb19301727','$NOW');
SQL
write_marker "$CWD2" "h2-key" "rogue-claude"

out="$(run_hook "$CWD2" "h2-key")"
echo "$out" | grep -q '<kanban' \
  && fail "H2 kanban block should not emit for pair_key-less session (out=$out)"
echo "H2 ok — session without pair_key emits no kanban block"

# ---------- H3: pullable requires role match -------------------------------
CWD3="$(cd "$(mktemp -d)" && pwd -P)"
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO peer_rooms (room_key, pair_key, turn_count, home_host)
  VALUES ('pk:H3-pk','H3-pk',0,'testhost');
INSERT INTO sessions (cwd,label,agent,role,session_key,channel_socket,pair_key,
                     started_at,last_seen_at,receive_mode,auth_token,auth_token_rotated_at)
  VALUES ('$CWD3','web-claude','claude','web','h3-key',NULL,'H3-pk',
          '$NOW','$NOW','interactive','tok-162fba7f8355810e','$NOW');
SQL
# An ios card should NOT appear on web's hook queue.
"$PI_BIN" card-create --pair-key H3-pk --title "H3 ios-only" \
    --created-by owner --needs-role ios >/dev/null
write_marker "$CWD3" "h3-key" "web-claude"

out="$(run_hook "$CWD3" "h3-key")"
# With no claimed and no role-match pullable, nothing should emit at all.
[ -z "$out" ] \
  || ! echo "$out" | grep -q 'H3 ios-only' \
  || fail "H3 role-mismatched card leaked into web-claude's queue (out=$out)"
echo "H3 ok — role-mismatched card hidden"

# ---------- H4: blocked-but-claimed still shows in claimed ------------------
CWD4="$(cd "$(mktemp -d)" && pwd -P)"
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO peer_rooms (room_key, pair_key, turn_count, home_host)
  VALUES ('pk:H4-pk','H4-pk',0,'testhost');
INSERT INTO sessions (cwd,label,agent,role,session_key,channel_socket,pair_key,
                     started_at,last_seen_at,receive_mode,auth_token,auth_token_rotated_at)
  VALUES ('$CWD4','wip-claude','claude','backend','h4-key',NULL,'H4-pk',
          '$NOW','$NOW','interactive','tok-c2c82a1a4cdd73e5','$NOW');
SQL
# blocker, blockee; blockee claimed by wip-claude but blocked (status stays todo).
"$PI_BIN" card-create --pair-key H4-pk --title "H4 blocker" \
    --created-by owner --needs-role backend >/dev/null
"$PI_BIN" card-create --pair-key H4-pk --title "H4 blockee (I'm stuck on it)" \
    --created-by owner --needs-role backend >/dev/null
blocker_id="$(sqlite3 "$DB" "SELECT id FROM cards WHERE pair_key='H4-pk' AND title='H4 blocker'")"
blockee_id="$(sqlite3 "$DB" "SELECT id FROM cards WHERE pair_key='H4-pk' AND title LIKE 'H4 blockee%'")"
"$PI_BIN" card-add-dep --blocker "$blocker_id" --blockee "$blockee_id" --as owner >/dev/null
"$PI_BIN" card-claim --id "$blockee_id" --as wip-claude >/dev/null
write_marker "$CWD4" "h4-key" "wip-claude"

out="$(run_hook "$CWD4" "h4-key")"
echo "$out" | grep -q "#$blockee_id \[in_progress\]" \
  || fail "H4 claimed (in_progress) card missing from hook (out=$out)"
echo "H4 ok — claimed card surfaces even when it depends on other work"

echo "PASS hook-cards-injection.sh — 4/4"
