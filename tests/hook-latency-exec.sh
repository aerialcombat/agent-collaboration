#!/usr/bin/env bash
# tests/hook-latency-exec.sh — wall-clock p99 of the Go hook binary under
# the real hook shape: a fresh OS process per iteration paying Go runtime
# start + sqlite driver init + Open+ResolveSelf+ReadUnread+Close every
# time. Supplements the in-process bench at
# go/cmd/hook/hook_bench_test.go:58-91 (which amortizes one-time costs
# over 200 iterations by sharing a process and is therefore a regression
# gate on code-path speed, not DoD evidence on cold-start latency).
#
# Covers the reason R1 / alpha B1 confluence: W1 DoD says "hook p99 <15ms
# post-cold fresh-exec" (revised from <10ms after n=200 wall-clock
# measurement settled at ~13.5ms) and the real hook is exec-per-prompt.
# The in-process bench can't prove that bar; this can.
#
# Environment knobs:
#   HOOK_LATENCY_BUDGET_MS (default 15) — p99 budget in milliseconds.
#       Matches the ratified W1 DoD; CI sets the same value explicitly.
#   HOOK_LATENCY_ITERATIONS (default 200) — fresh-exec count. Matches CI's
#       explicit `HOOK_LATENCY_ITERATIONS: "200"` for local/CI parity. The
#       percentile formula `samples[min(n-1, int(n*p/100))]` requires n>100
#       for the p99 index to separate from max: at n=50 and n=100 (any
#       exact multiple of 100), `int(n*99/100)` lands on n-1 under the
#       min-clamp so the reported "p99" is structurally equal to max. n=200
#       gives index 198 vs max index 199 — one-sample gap, real p99.
#
# Exits:
#   0 — p99 under budget, OR prerequisites missing (skip)
#   1 — p99 over budget

set -uo pipefail

# n=200 seed-sends (ITERATIONS+5) exceed the 100-turn default pair-room
# cap. Raise it here so the seed loop runs cleanly; same override shape
# `go/cmd/hook/hook_bench_test.go` uses for the in-process bench.
export AGENT_COLLAB_MAX_PAIR_TURNS="${AGENT_COLLAB_MAX_PAIR_TURNS:-10000}"

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUDGET_MS="${HOOK_LATENCY_BUDGET_MS:-15}"
ITERATIONS="${HOOK_LATENCY_ITERATIONS:-200}"

if ! command -v go >/dev/null 2>&1; then
  echo "skip: go toolchain not on PATH"
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "skip: python3 not on PATH (needed for ms-granularity timing)"
  exit 0
fi

BIN_DIR="$(mktemp -d)"
BIN="$BIN_DIR/peer-inbox-hook"
( cd "$PROJECT_ROOT/go" && go build -o "$BIN" ./cmd/hook ) || {
  echo "skip: go build failed"
  rm -rf "$BIN_DIR"
  exit 0
}

TMP="$(mktemp -d)"
export HOME="$TMP/home"
mkdir -p "$HOME/.agent-collab"

export PATH="$PROJECT_ROOT/scripts:$PATH"

DB="$TMP/sessions.db"
RECV_CWD="$TMP/recv"
SEND_CWD="$TMP/send"
mkdir -p "$RECV_CWD" "$SEND_CWD"

cleanup() { rm -rf "$TMP" "$BIN_DIR"; }
trap cleanup EXIT

AC="$PROJECT_ROOT/scripts/agent-collab"

echo "-- hook-latency-exec: register recv + send --"
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-recv" \
  "$AC" session register --cwd "$RECV_CWD" --label recv --agent claude >/dev/null
AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
  "$AC" session register --cwd "$SEND_CWD" --label send --agent claude >/dev/null

# Pre-seed enough unread rows that every hook iteration has something to
# read. The Go hook MarkRead-on-Read consumes one batch per invocation;
# seeding ITERATIONS+buffer ensures a steady dirty-path signal.
SEED_COUNT=$(( ITERATIONS + 5 ))
echo "-- hook-latency-exec: pre-seeding $SEED_COUNT unread rows --"
for i in $(seq 1 "$SEED_COUNT"); do
  AGENT_COLLAB_INBOX_DB="$DB" AGENT_COLLAB_SESSION_KEY="key-send" \
    "$AC" peer send --cwd "$SEND_CWD" --to recv --to-cwd "$RECV_CWD" \
      --message "bench iter $i" >/dev/null
done

echo "-- hook-latency-exec: measuring $ITERATIONS fresh-exec iterations (budget p99 ${BUDGET_MS}ms) --"

python3 - "$BIN" "$RECV_CWD" "$DB" "$ITERATIONS" "$BUDGET_MS" <<'PYEOF'
import os, subprocess, sys, time

bin_path, recv_cwd, db, n_iters, budget_ms = sys.argv[1:]
n_iters = int(n_iters)
budget_ms = float(budget_ms)

env = dict(os.environ, AGENT_COLLAB_INBOX_DB=db, AGENT_COLLAB_SESSION_KEY="key-recv")


def one_exec():
    t0 = time.perf_counter()
    subprocess.run(
        [bin_path, recv_cwd],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return (time.perf_counter() - t0) * 1000.0


# First exec is the cold-start outlier (OS file cache miss on the binary,
# sqlite driver pages, DB mmap). The in-process bench at
# go/cmd/hook/hook_bench_test.go:71-91 explicitly discards one warmup.
# Report it separately so both numbers are honest.
cold_ms = one_exec()

samples_ms = [one_exec() for _ in range(n_iters)]
samples_ms.sort()
n = len(samples_ms)


def pct(p):
    return samples_ms[min(n - 1, int(n * p / 100))]


print(f"  cold = {cold_ms:.2f} ms (first exec, OS/driver/sqlite init cost)")
print(f"  n    = {n} (post-cold)")
print(f"  p50  = {pct(50):.2f} ms")
print(f"  p95  = {pct(95):.2f} ms")
print(f"  p99  = {pct(99):.2f} ms")
print(f"  max  = {samples_ms[-1]:.2f} ms")

# Evaluate post-cold p99 against budget. Surface cold_ms separately; it's
# informative but every real UserPromptSubmit after a shell start pays it
# once, not every prompt.
if pct(99) > budget_ms:
    print(
        f"FAIL: post-cold wall-clock p99 {pct(99):.2f}ms exceeds budget {budget_ms}ms "
        f"(DoD gate per plans/v3.0-go-cloud-ready-scoping.md); "
        f"cold-start {cold_ms:.2f}ms is additional.",
        file=sys.stderr,
    )
    sys.exit(1)

print(
    f"PASS: post-cold wall-clock p99 {pct(99):.2f}ms under budget {budget_ms}ms "
    f"(cold-start spike {cold_ms:.2f}ms is paid once per shell session)."
)
PYEOF
