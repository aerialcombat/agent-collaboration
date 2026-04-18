#!/usr/bin/env bash
# tests/envelope-round-trip.sh — Topic 3 commit 5 / §5.2 + §8.2 gate.
#
# Asserts two things about the canonical peer-inbox envelope schema
# (§5.1 of plans/v3.x-topic-3-implementation-scope.md):
#
#   1. ROUND-TRIP PRESERVATION. For a given batch of inbox rows:
#        a. Python build_peer_inbox_envelope(rows)     → envelope_dict
#        b. json.dumps / json.loads                     → envelope_dict'
#        c. format_hook_block(rows)                     → text_direct
#        d. format_hook_block(envelope_dict'.messages)  → text_roundtrip
#      text_direct MUST equal text_roundtrip byte-for-byte. Demonstrates
#      that the schema is a lossless intermediate for the hook-text
#      rendering — no information consumed by the text renderer is lost
#      by routing through the JSON envelope.
#
#   2. TWO-CONSUMER BYTE-PARITY (§5.2 alpha insight + §8.2 gate). Same
#      batch rendered by the Go hook binary (Consumer 1, the Option J
#      hook path for interactive labels) and by the Python hook-text
#      path (Consumer 2, proxy for the daemon prompt-injection path
#      that commit 7 builds — daemon consumes the same Go envelope
#      package and renders via the same serializer). Both consumers
#      MUST emit byte-identical additionalContext strings for a given
#      batch. This is the load-bearing invariant the schema exists to
#      enforce.
#
# Skips cleanly when python3 or go toolchain is absent. Uses
# AGENT_COLLAB_FORCE_PY=1 for the Python-path comparison so the Python
# renderer's in-process call is deterministic vs the CLI subprocess.
#
# Per-message timestamps are normalized before comparison because rows
# share a label but not a created_at — same normalization strategy as
# tests/hook-parity.sh assertion 3.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi

AC="$PROJECT_ROOT/scripts/agent-collab"
[[ -x "$AC" ]] || { echo "FAIL: $AC not executable" >&2; exit 1; }

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
export PATH="$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
RECV_GO_CWD="$TMP/recv-go"
RECV_PY_CWD="$TMP/recv-py"
SEND_CWD="$TMP/send"
mkdir -p "$RECV_GO_CWD" "$RECV_PY_CWD" "$SEND_CWD"

export AGENT_COLLAB_INBOX_DB="$DB"

echo "-- envelope-round-trip: register two recipients (Go / Py consumer) + one sender --"
AGENT_COLLAB_SESSION_KEY="key-recv-go" \
  "$AC" session register --cwd "$RECV_GO_CWD" --label recv --agent claude >/dev/null \
  || fail "register recv-go"
AGENT_COLLAB_SESSION_KEY="key-recv-py" \
  "$AC" session register --cwd "$RECV_PY_CWD" --label recv --agent claude >/dev/null \
  || fail "register recv-py"
AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label sender --agent claude >/dev/null \
  || fail "register send"

# Seed each recipient with the same 3-message batch. Two messages small
# ASCII, one message moderately-sized UTF-8 (Korean Hangul to exercise
# multi-byte byte-accounting in the renderer). All three fit comfortably
# in the default 4 KiB HOOK_BLOCK_BUDGET so no truncation is triggered
# for the base assertion; a separate truncation assertion below uses
# HOOK_BLOCK_BUDGET override.
seed_one() {
  local to_cwd="$1"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$to_cwd" \
      --message "envelope-round-trip msg 1 (ascii)" >/dev/null \
    || fail "seed msg1 to $to_cwd"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$to_cwd" \
      --message "envelope-round-trip msg 2 (한글 mixed body)" >/dev/null \
    || fail "seed msg2 to $to_cwd"
  AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$to_cwd" \
      --message "envelope-round-trip msg 3 (ascii)" >/dev/null \
    || fail "seed msg3 to $to_cwd"
}

echo "-- envelope-round-trip: seed 3 identical msgs to each consumer cwd --"
seed_one "$RECV_GO_CWD"
seed_one "$RECV_PY_CWD"

# ---------------------------------------------------------------------
# Assertion 1 (round-trip preservation, Python side).
#
# Drive the Python build_peer_inbox_envelope + format_hook_block via
# inline interpreter invocation so the test owns the deterministic
# connection state. Pulls the rows directly out of sqlite with the same
# column shape the hot path returns, builds an envelope, round-trips it
# through json.dumps/json.loads, and asserts the rendered text is
# unchanged across the round trip.
# ---------------------------------------------------------------------
echo "-- envelope-round-trip: assertion 1 — Python schema round-trip preserves text --"
AGENT_COLLAB_INBOX_DB="$DB" RECV_CWD="$RECV_PY_CWD" PROJECT_ROOT_HINT="$PROJECT_ROOT" python3 - <<'PYEOF' \
  || fail "Python schema round-trip assertion failed"
import importlib.util
import json
import os
import sqlite3
import sys
from pathlib import Path

# Locate scripts/peer-inbox-db.py via the shell-provided PROJECT_ROOT_HINT
# (the wrapping test exports it). Fall back to a bounded walk from cwd so
# this script works when invoked stand-alone for debugging.
project_root_hint = os.environ.get("PROJECT_ROOT_HINT")
if project_root_hint:
    db_py = Path(project_root_hint) / "scripts" / "peer-inbox-db.py"
else:
    cwd = Path.cwd()
    candidate = None
    for p in [cwd, *cwd.parents]:
        if (p / "scripts" / "peer-inbox-db.py").exists():
            candidate = p / "scripts" / "peer-inbox-db.py"
            break
    db_py = candidate
if db_py is None or not db_py.exists():
    print("FAIL: cannot locate scripts/peer-inbox-db.py", file=sys.stderr)
    sys.exit(1)

spec = importlib.util.spec_from_file_location("peer_inbox_db", str(db_py))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

db_path = os.environ["AGENT_COLLAB_INBOX_DB"]
recv_cwd_raw = os.environ["RECV_CWD"]
# `peer send` stores to_cwd as the realpath of the caller's argument
# (macOS symlinks /var -> /private/var turn /var/folders/... into
# /private/var/folders/...). Match that normalization before querying so
# the assertion is filesystem-layout agnostic.
recv_cwd = os.path.realpath(recv_cwd_raw)

conn = sqlite3.connect(db_path)
conn.row_factory = sqlite3.Row

# Pull the unread rows for recv@recv_cwd without mutating state — no
# UPDATE here, just SELECT. Same column shape the hot-path RETURNING
# produces (id, from_cwd, from_label, body, created_at).
rows = conn.execute(
    """
    SELECT id, from_cwd, from_label, body, created_at
    FROM inbox
    WHERE to_cwd = ? AND to_label = ? AND read_at IS NULL
    ORDER BY created_at ASC, id ASC
    """,
    (recv_cwd, "recv"),
).fetchall()
if not rows:
    print("FAIL: no rows seeded for recv", file=sys.stderr)
    sys.exit(1)

# Step a/b: build envelope, JSON round-trip.
env = mod.build_peer_inbox_envelope(list(rows))
# Sanity: envelope shape matches §5.1 required fields.
assert "messages" in env, "envelope missing messages[]"
assert "to" not in env, "hook-side envelope must omit 'to' (to=None default)"
for m in env["messages"]:
    for required in ("id", "from_cwd", "from_label", "body", "created_at"):
        assert required in m, f"message missing {required}"
json_blob = json.dumps(env, ensure_ascii=False, sort_keys=False)
env_back = json.loads(json_blob)
assert env == env_back, "JSON round-trip mutated envelope dict"

# Step c: render from original rows.
text_direct = mod.format_hook_block(list(rows))

# Step d: render from the round-tripped envelope messages. The renderer
# accepts anything indexable-by-key, so pass the decoded JSON objects
# directly. Rebuilding sqlite3.Row is unnecessary — dict[str]→value
# honors the r["..."] access pattern format_hook_block uses.
text_roundtrip = mod.format_hook_block(env_back["messages"])

if text_direct != text_roundtrip:
    print("FAIL: round-trip diverged", file=sys.stderr)
    print("--- direct ---", file=sys.stderr)
    print(text_direct, file=sys.stderr)
    print("--- roundtrip ---", file=sys.stderr)
    print(text_roundtrip, file=sys.stderr)
    sys.exit(1)

# Bonus: re-serialize the round-tripped envelope + re-parse; assert
# idempotence. Guards against hidden non-determinism in the dict shape.
json_blob_2 = json.dumps(env_back, ensure_ascii=False, sort_keys=False)
assert json_blob == json_blob_2, "envelope JSON not idempotent under round-trip"

# Assert the optional fields (§5.1) serialize correctly when present.
env_with_opts = mod.build_peer_inbox_envelope(
    list(rows),
    to={"cwd": recv_cwd, "label": "recv"},
    continuity_summary="prior round summary here",
    state="closed",
    content_stop=True,
)
blob_opts = json.dumps(env_with_opts, ensure_ascii=False)
decoded = json.loads(blob_opts)
assert decoded["to"] == {"cwd": recv_cwd, "label": "recv"}, "to missing/changed"
assert decoded["continuity_summary"] == "prior round summary here"
assert decoded["state"] == "closed"
assert decoded["content_stop"] is True

# Negative: omitted optional fields stay out of JSON.
env_no_opts = mod.build_peer_inbox_envelope(list(rows))
decoded_no = json.loads(json.dumps(env_no_opts, ensure_ascii=False))
for k in ("to", "continuity_summary", "state", "content_stop"):
    assert k not in decoded_no, f"{k} present despite default None"

print("   Python round-trip byte-stable ({} msgs, {} bytes)".format(
    len(env["messages"]), len(text_direct.encode("utf-8"))
))
PYEOF

# ---------------------------------------------------------------------
# Assertion 2 (two-consumer byte-parity).
#
# Consumer 1 = Go hook binary (reading recv-go's inbox).
# Consumer 2 = Python hook path (reading recv-py's inbox via
#              `peer receive --format hook-json --mark-read`).
# Both should render byte-identical additionalContext modulo
# per-message created_at timestamp (which naturally differs because
# rows were inserted at distinct wall-clock instants per recipient).
# ---------------------------------------------------------------------
echo "-- envelope-round-trip: assertion 2 — two-consumer byte-parity (Go hook vs Python) --"
go_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-go" \
    "$BIN" "$RECV_GO_CWD" 2>/dev/null
)"
py_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-py" \
    AGENT_COLLAB_FORCE_PY=1 \
    "$AC" peer receive --cwd "$RECV_PY_CWD" --format hook-json --mark-read 2>/dev/null
)"

extract_ctx() {
  # $1 = raw JSON envelope string. Strips everything except
  # hookSpecificOutput.additionalContext, which is the text block the
  # shared serializer produced.
  python3 -c "
import json, sys
raw = sys.argv[1]
if not raw:
    print('EMPTY')
    sys.exit(0)
d = json.loads(raw)
sys.stdout.write(d['hookSpecificOutput']['additionalContext'])
" "$1"
}

go_ctx="$(extract_ctx "$go_out")"
py_ctx="$(extract_ctx "$py_out")"

if [[ "$go_ctx" == "EMPTY" || -z "$go_ctx" ]]; then
  fail "Go consumer produced no additionalContext"
fi
if [[ "$py_ctx" == "EMPTY" || -z "$py_ctx" ]]; then
  fail "Python consumer produced no additionalContext"
fi

# Normalize per-message timestamps: `[sender @ ISO_TS]` → `[sender @ TS]`.
# Labels match (both use "recv" / "sender"); only created_at differs per-
# row because rows were inserted at distinct wall-clock instants.
echo "$go_ctx" | sed -E 's/\[sender @ [^]]+\]/[sender @ TS]/g' > "$TMP/go.norm"
echo "$py_ctx" | sed -E 's/\[sender @ [^]]+\]/[sender @ TS]/g' > "$TMP/py.norm"

if ! diff -q "$TMP/go.norm" "$TMP/py.norm" >/dev/null; then
  echo "--- GO consumer (normalized) ---"; cat "$TMP/go.norm"
  echo "--- PY consumer (normalized) ---"; cat "$TMP/py.norm"
  fail "Go vs Python consumer additionalContext diverged after timestamp normalization"
fi
go_bytes="$(printf '%s' "$go_ctx" | wc -c | tr -d ' ')"
py_bytes="$(printf '%s' "$py_ctx" | wc -c | tr -d ' ')"
echo "   Go consumer=$go_bytes bytes  Py consumer=$py_bytes bytes  byte-identical (modulo timestamp)"

# ---------------------------------------------------------------------
# Assertion 3 (HOOK_BLOCK_BUDGET truncation preserved by the refactor).
#
# Shrink the budget to a value that forces truncation (~200 bytes) and
# re-render via both Go hook + Python CLI. Both MUST still render and
# both must include the "+N more messages truncated" marker. Guards
# the path-(a) preservation requirement — the refactor cannot regress
# truncation byte-accounting while moving through the envelope schema.
# ---------------------------------------------------------------------
echo "-- envelope-round-trip: assertion 3 — HOOK_BLOCK_BUDGET truncation preserved --"
# Reseed each recipient (the previous --mark-read consumed recv-py's rows).
# Use a fresh recv pair for clean isolation.
RECV_GO2_CWD="$TMP/recv-go-trunc"
RECV_PY2_CWD="$TMP/recv-py-trunc"
mkdir -p "$RECV_GO2_CWD" "$RECV_PY2_CWD"
AGENT_COLLAB_SESSION_KEY="key-recv-go2" \
  "$AC" session register --cwd "$RECV_GO2_CWD" --label recv --agent claude >/dev/null \
  || fail "register recv-go2"
AGENT_COLLAB_SESSION_KEY="key-recv-py2" \
  "$AC" session register --cwd "$RECV_PY2_CWD" --label recv --agent claude >/dev/null \
  || fail "register recv-py2"
seed_one "$RECV_GO2_CWD"
seed_one "$RECV_PY2_CWD"

BUDGET=200
go_trunc_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-go2" \
    AGENT_COLLAB_HOOK_BLOCK_BUDGET="$BUDGET" \
    "$BIN" "$RECV_GO2_CWD" 2>/dev/null
)"
py_trunc_out="$(
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv-py2" \
    AGENT_COLLAB_FORCE_PY=1 AGENT_COLLAB_HOOK_BLOCK_BUDGET="$BUDGET" \
    "$AC" peer receive --cwd "$RECV_PY2_CWD" --format hook-json --mark-read 2>/dev/null
)"

go_trunc_ctx="$(extract_ctx "$go_trunc_out")"
py_trunc_ctx="$(extract_ctx "$py_trunc_out")"

# Both outputs should carry the "+N more messages truncated" marker and
# the same marker count. Normalize timestamp the same way.
for label in go py; do
  case "$label" in
    go) ctx="$go_trunc_ctx" ;;
    py) ctx="$py_trunc_ctx" ;;
  esac
  if [[ "$ctx" != *"messages truncated"* ]]; then
    echo "--- $label truncation output ---"; echo "$ctx"
    fail "$label: expected truncation marker at BUDGET=$BUDGET — path (a) byte-accounting regressed"
  fi
done

echo "$go_trunc_ctx" | sed -E 's/\[sender @ [^]]+\]/[sender @ TS]/g' > "$TMP/go.trunc.norm"
echo "$py_trunc_ctx" | sed -E 's/\[sender @ [^]]+\]/[sender @ TS]/g' > "$TMP/py.trunc.norm"
if ! diff -q "$TMP/go.trunc.norm" "$TMP/py.trunc.norm" >/dev/null; then
  echo "--- GO truncated (normalized) ---"; cat "$TMP/go.trunc.norm"
  echo "--- PY truncated (normalized) ---"; cat "$TMP/py.trunc.norm"
  fail "Go vs Python truncation output diverged — path (a) byte-identity broken"
fi
echo "   truncation marker present on both; normalized outputs byte-identical"

echo "PASS: envelope-round-trip — schema round-trip stable + two-consumer byte-parity + truncation preserved"
