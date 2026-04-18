#!/usr/bin/env bash
# tests/daemon-pi-help-fixture.sh — Topic 3 v0.2 §9.2 gate 5:
# pi --help output + fixture grep-pattern + version-marker assertion.
#
# Per §10 Q2 ratification: grep-pattern NOT full-diff. Asserts presence
# of the daemon-required flags (--provider, --model, --session,
# --no-session, -p) in LIVE `pi --help` output. Asserts the fixture
# file has a "pi-mono-version:" line so drift-detect has a stable
# identifier to compare against.
#
# Skips cleanly if `pi` binary is not on PATH (CI systems without
# pi-mono installed skip this gate rather than fail).

set -uo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE="$PROJECT_ROOT/tests/fixtures/cli-session/pi-help.txt"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "-- daemon-pi-help-fixture: $*"; }

if ! command -v pi >/dev/null 2>&1; then
  echo "skip: pi binary not on PATH"
  exit 0
fi

[[ -f "$FIXTURE" ]] || fail "fixture missing: $FIXTURE"

# (1) Fixture must carry a pi-mono-version: marker line. This is the
# drift-detect identifier the §E8 probe compares against when auditing
# cosmetic --help changes vs structural ones.
step "(1) fixture has pi-mono-version: marker line"
grep -qE '^#?\s*pi-mono-version:\s*[0-9]+\.[0-9]+\.[0-9]+' "$FIXTURE" \
  || { head -15 "$FIXTURE"; fail "(1) fixture missing pi-mono-version: marker"; }
PINNED_VERSION="$(grep -oE 'pi-mono-version:\s*[0-9]+\.[0-9]+\.[0-9]+' "$FIXTURE" | head -1 | awk '{print $2}')"
echo "   (1) fixture pinned to pi-mono $PINNED_VERSION"

# (2) Live `pi --help` must contain all flags the daemon's spawnPi
# helper uses. If any one is missing, pi-mono renamed/removed a flag
# and the daemon's spawn argv would break.
step "(2) live pi --help contains daemon-required flags"
HELP_OUT="$(pi --help 2>&1)"
for flag in "--provider" "--model" "--session" "--no-session" "-p" "--session-dir"; do
  if ! grep -qE "(^| )$flag( |\$|,)" <<<"$HELP_OUT"; then
    echo "$HELP_OUT" | head -40 >&2
    fail "(2) live pi --help missing required flag: $flag"
  fi
done
echo "   (2) all daemon-required flags present in pi --help"

# (3) Live pi version SHOULD match the pinned fixture version. Warn
# (not fail) on mismatch — drift is expected between fixture-bumps.
step "(3) live pi --version vs fixture pin"
LIVE_VERSION="$(pi --version 2>&1 | head -1 | tr -d ' ')"
if [[ "$LIVE_VERSION" == "$PINNED_VERSION" ]]; then
  echo "   (3) live pi $LIVE_VERSION matches fixture pin"
else
  echo "   (3) NOTE: live pi $LIVE_VERSION ≠ fixture pin $PINNED_VERSION — fixture drift detected"
  echo "        update fixture + re-run §E8 probe + bump version marker if probe PASS"
fi

echo "PASS: daemon-pi-help-fixture — grep-pattern + version-marker gate per §9.2 gate 5 + §10 Q2"
