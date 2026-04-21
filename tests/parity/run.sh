#!/usr/bin/env bash
# tests/parity/run.sh — v3.4 Python-removal parity harness.
#
# Runs one fixture against one implementation and diffs the result
# against the expected output. Byte-level equality is the gate.
#
# Usage:
#   tests/parity/run.sh <verb> <scenario> <impl>
#
# Args:
#   verb:     subcommand name, e.g. "session-list"
#   scenario: fixture name without .fixture.json suffix
#   impl:     "python" or "go"
#
# Exit:
#   0 if actual output matches expected byte-for-byte.
#   1 on any diff; writes <scenario>.actual.json for inspection.
#   2 on harness errors (missing fixture, missing binary, etc).
#
# Fixture format (tests/fixtures/parity/<verb>/<scenario>.fixture.json):
#   {
#     "description": "...",
#     "verb":        "session-list",        // informational
#     "args":        ["--cwd", "/tmp/x"],
#     "stdin":       "",
#     "env":         {"AGENT_COLLAB_SELF_HOST": "testhost"},
#     "pre_sql":     "INSERT INTO sessions ..."
#   }
#
# Expected format (tests/fixtures/parity/<verb>/<scenario>.expected.json):
#   {
#     "stdout":     "...",
#     "stderr":     "",
#     "exit":       0,
#     "verify_sql": [{"query": "SELECT ...", "expected": "row1\nrow2"}, ...]
#   }
#
# Design notes:
# - pre_sql + verify_sql keep the harness dependency-free of Python/jq
#   for seeding and snapshotting. `sqlite3` is the only non-bash tool.
# - `env` pairs set process env for the impl invocation; harness own env
#   (PATH, HOME) is inherited.
# - AGENT_COLLAB_INBOX_DB is always forced to the scratch DB path so
#   fixtures never need to set it.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FIXTURES="$PROJECT_ROOT/tests/fixtures/parity"

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <verb> <scenario> <impl>" >&2
  exit 2
fi
verb="$1"
scenario="$2"
impl="$3"

fixture_json="$FIXTURES/$verb/$scenario.fixture.json"
expected_json="$FIXTURES/$verb/$scenario.expected.json"

[ -f "$fixture_json" ] || { echo "missing fixture: $fixture_json" >&2; exit 2; }
[ -f "$expected_json" ] || { echo "missing expected: $expected_json" >&2; exit 2; }

for tool in sqlite3 jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "required tool not on PATH: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d -t parity.XXXXXX)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

DB="$TMP/sessions.db"
actual_stdout="$TMP/stdout.txt"
actual_stderr="$TMP/stderr.txt"
actual_exit=""
MIGRATE_BIN="$PROJECT_ROOT/go/bin/peer-inbox-migrate"
[ -x "$MIGRATE_BIN" ] || { echo "missing binary: $MIGRATE_BIN (run 'go build -o bin/peer-inbox-migrate ./cmd/migrate')" >&2; exit 2; }

# 1. Bootstrap schema.
# peer-inbox-db.py's open_db() creates the DB + runs Python-side legacy
# migrations + goose. To avoid starting from the Python side (which would
# defeat the point for Go-impl runs), we mint an empty DB with just the
# Python-owned legacy tables, then apply goose migrations via the Go
# binary. This matches what open_db() does internally.
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

"$MIGRATE_BIN" \
  -driver sqlite \
  -dsn "$DB" \
  -dir "$PROJECT_ROOT/migrations/sqlite" \
  up >/dev/null 2>&1 \
  || { echo "migrations failed against $DB" >&2; exit 2; }

# 2. Apply fixture pre_sql (seed state).
pre_sql="$(jq -r '.pre_sql // ""' "$fixture_json")"
if [ -n "$pre_sql" ]; then
  printf '%s\n' "$pre_sql" | sqlite3 "$DB" \
    || { echo "pre_sql failed" >&2; exit 2; }
fi

# 3. Build env from fixture.env + forced AGENT_COLLAB_INBOX_DB.
env_args=("AGENT_COLLAB_INBOX_DB=$DB")
while IFS=$'\t' read -r k v; do
  [ -z "$k" ] && continue
  env_args+=("$k=$v")
done < <(jq -r '.env // {} | to_entries[] | "\(.key)\t\(.value)"' "$fixture_json")

# 4. Build args array. macOS bash 3.2 has no readarray; use a loop.
verb_args=()
while IFS= read -r line; do
  verb_args+=("$line")
done < <(jq -r '.args[]?' "$fixture_json")

# 5. Build stdin.
stdin_content="$(jq -r '.stdin // ""' "$fixture_json")"

# 6. Select impl invocation.
case "$impl" in
  python)
    invoke=(python3 "$PROJECT_ROOT/scripts/peer-inbox-db.py" "$verb")
    ;;
  go)
    GO_BIN="$PROJECT_ROOT/go/bin/peer-inbox"
    [ -x "$GO_BIN" ] || { echo "missing binary: $GO_BIN (Phase 0 runs python only; this is expected pre-Phase 2)" >&2; exit 2; }
    invoke=("$GO_BIN" "$verb")
    ;;
  *)
    echo "unknown impl: $impl (expected python|go)" >&2
    exit 2
    ;;
esac

# 7. Run. Expand verb_args only when non-empty (set -u + bash 3.2 treat
# "${arr[@]}" as unbound if the array is empty).
if [ "${#verb_args[@]}" -eq 0 ]; then
  if [ -n "$stdin_content" ]; then
    env -i HOME="$HOME" PATH="$PATH" "${env_args[@]}" \
      "${invoke[@]}" \
      <<<"$stdin_content" \
      >"$actual_stdout" 2>"$actual_stderr"
  else
    env -i HOME="$HOME" PATH="$PATH" "${env_args[@]}" \
      "${invoke[@]}" \
      </dev/null \
      >"$actual_stdout" 2>"$actual_stderr"
  fi
else
  if [ -n "$stdin_content" ]; then
    env -i HOME="$HOME" PATH="$PATH" "${env_args[@]}" \
      "${invoke[@]}" "${verb_args[@]}" \
      <<<"$stdin_content" \
      >"$actual_stdout" 2>"$actual_stderr"
  else
    env -i HOME="$HOME" PATH="$PATH" "${env_args[@]}" \
      "${invoke[@]}" "${verb_args[@]}" \
      </dev/null \
      >"$actual_stdout" 2>"$actual_stderr"
  fi
fi
actual_exit=$?

# 8. Compare.
expected_stdout="$(jq -r '.stdout // ""' "$expected_json")"
expected_stderr="$(jq -r '.stderr // ""' "$expected_json")"
expected_exit="$(jq -r '.exit // 0' "$expected_json")"

# Accumulate failures so we report all mismatches at once, not just the first.
failed=0
report_diff() {
  local field="$1" expected="$2" actual_file="$3"
  local actual; actual="$(cat "$actual_file")"
  if [ "$expected" != "$actual" ]; then
    echo "[FAIL] $verb/$scenario ($impl): $field mismatch" >&2
    diff <(printf '%s' "$expected") <(printf '%s' "$actual") >&2 || true
    failed=1
  fi
}

report_diff stdout "$expected_stdout" "$actual_stdout"
report_diff stderr "$expected_stderr" "$actual_stderr"

if [ "$expected_exit" != "$actual_exit" ]; then
  echo "[FAIL] $verb/$scenario ($impl): exit code expected=$expected_exit actual=$actual_exit" >&2
  failed=1
fi

# 9. Verify post-state via SELECT queries.
n="$(jq -r '.verify_sql // [] | length' "$expected_json")"
i=0
while [ "$i" -lt "$n" ]; do
  query="$(jq -r ".verify_sql[$i].query" "$expected_json")"
  expected_rows="$(jq -r ".verify_sql[$i].expected" "$expected_json")"
  actual_rows="$(sqlite3 "$DB" "$query")"
  if [ "$expected_rows" != "$actual_rows" ]; then
    echo "[FAIL] $verb/$scenario ($impl): verify_sql[$i] mismatch" >&2
    echo "  query: $query" >&2
    diff <(printf '%s' "$expected_rows") <(printf '%s' "$actual_rows") >&2 || true
    failed=1
  fi
  i=$((i + 1))
done

if [ "$failed" -eq 0 ]; then
  echo "[OK]   $verb/$scenario ($impl)"
  exit 0
fi

# 10. On failure, dump actual as JSON for debugging.
actual_out="$FIXTURES/$verb/$scenario.actual.json"
jq -n \
  --rawfile stdout "$actual_stdout" \
  --rawfile stderr "$actual_stderr" \
  --arg exit "$actual_exit" \
  '{stdout: $stdout, stderr: $stderr, exit: ($exit | tonumber)}' \
  >"$actual_out"
echo "  actual snapshot: $actual_out" >&2
exit 1
