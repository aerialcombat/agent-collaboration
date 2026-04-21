#!/usr/bin/env bash
# tests/parity/all.sh — run every parity fixture against every
# implementation. Reports a per-verb summary matrix; exits non-zero on
# any failure.
#
# Usage:
#   tests/parity/all.sh             # run all fixtures × all available impls
#   tests/parity/all.sh python      # only the Python impl
#   tests/parity/all.sh go          # only the Go impl
#
# Phase 0 note: the Go `peer-inbox` binary doesn't exist yet. `go` runs
# will skip with a warning rather than fail the suite.

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FIXTURES="$PROJECT_ROOT/tests/fixtures/parity"
RUN="$PROJECT_ROOT/tests/parity/run.sh"

if [ "$#" -eq 1 ]; then
  impls=("$1")
else
  impls=(python go)
fi

total=0
failed=0
skipped=0

for fixture_path in "$FIXTURES"/*/*.fixture.json; do
  [ -e "$fixture_path" ] || continue
  scenario="$(basename "$fixture_path" .fixture.json)"
  verb="$(basename "$(dirname "$fixture_path")")"

  for impl in "${impls[@]}"; do
    total=$((total + 1))
    # Let the runner decide whether to skip (missing Go binary etc).
    out="$("$RUN" "$verb" "$scenario" "$impl" 2>&1)"
    rc=$?
    case "$rc" in
      0)
        echo "$out"
        ;;
      2)
        echo "$out" | tail -1
        echo "[SKIP] $verb/$scenario ($impl)"
        skipped=$((skipped + 1))
        ;;
      *)
        echo "$out"
        failed=$((failed + 1))
        ;;
    esac
  done
done

echo
echo "parity: total=$total failed=$failed skipped=$skipped"
[ "$failed" -eq 0 ] || exit 1
