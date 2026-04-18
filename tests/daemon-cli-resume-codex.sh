#!/usr/bin/env bash
# tests/daemon-cli-resume-codex.sh — RETIRED v0.3 — see daemon-collapse-migration.sh
#
# v0.1/v0.2 codex-direct session-resume gate (4a/4b/4c/4d/4e subtests)
# is superseded by Topic 3 v0.3's SOFT SHIM collapse per
# plans/v3.x-topic-3-v0.3-collapse-scoping.md §9.2 gate 1. --cli=codex
# now routes through spawnPi; capture/reuse/reset semantics are
# validated at the pi-routed level by:
#
#   - tests/daemon-cli-resume-pi.sh   — pi-native session-path flow
#   - tests/daemon-collapse-migration.sh — shim-specific assertions
#     (C1a argv + deprecation; C4 stale-UUID tolerance; C5 dual-shape
#     reset isolation covering both path-shape and UUID-shape columns,
#     which replaces v0.2's 4e cross-CLI-reset-isolation subtest).
#
# This script remains as a named gate so the aggregate test-run
# list (operator guide + CI) keeps its numbering. Running it is a
# no-op PASS; real regression coverage lives in the two scripts
# named above.
#
# When v0.4 HARD RETIRE lands, this file will be deleted outright
# (per §11 + §10 Q6 ratification).

set -u

echo "RETIRED v0.3: daemon-cli-resume-codex superseded by daemon-collapse-migration.sh + daemon-cli-resume-pi.sh"
echo "PASS: daemon-cli-resume-codex — retired-banner stub (v0.3 §9.2 gate 1 RETIRED-pattern)"
exit 0
