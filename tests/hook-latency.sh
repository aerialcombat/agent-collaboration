#!/usr/bin/env bash
# hook-latency.sh — in-process regression gate on the UserPromptSubmit
# hook's Open+ResolveSelf+ReadUnread+Close inner loop (budget <10ms p99,
# 200 iterations). This is NOT the DoD surface — that's
# tests/hook-latency-exec.sh (wall-clock fresh-exec, <15ms). A regression
# here predicts one at exec scale.
#
# Runs under the project-local Go bench harness (TestHotPathP99Regression
# in go/cmd/hook/hook_bench_test.go). We keep this shell wrapper so that
# the smoke suite and CI can invoke a single command and get a clean exit
# status; the Go test captures the actual histogram.

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH" >&2
  exit 0
fi

cd "$PROJECT_ROOT/go"

echo "-- hook-latency: running Go bench (200 iterations, asserts p99 < 10ms regression budget) --"
if ! go test -v -run TestHotPathP99Regression ./cmd/hook/ 2>&1 | tee /tmp/hook-latency.log; then
  echo "FAIL: hook-latency bench exceeded budget" >&2
  exit 1
fi

echo "-- hook-latency: bench passed --"
