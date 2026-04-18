# Auto-Reply Daemon — Architecture D Validation Protocol (E5–E7)

**Purpose:** validate Topic 3 v0.1 Architecture D — CLI-native session-ID
pass-through — against live `codex` and `gemini` binaries. Confirms that
the daemon correctly captures, persists, translates (gemini), and
resumes per-CLI session identities across multiple batches.

**Why this exists:** the persistent CI gates (`tests/daemon-cli-resume-codex.sh`,
`tests/daemon-cli-resume-gemini.sh`, `tests/daemon-auto-gc-on-content-stop.sh`)
exercise the daemon against fake-CLI stubs that emit canned banner /
list-sessions output. They do **not** validate that the regex / parser
shapes match what live `codex 0.121.0` and `gemini 0.38.2` (or whatever
versions are currently installed) actually emit. CLI vendor UX drift is
a real risk surface — fixtures pin the parser shape, but live runs are
the only way to confirm fixtures still match reality.

One-off smoke tests at ship time + whenever a CLI version bumps. Not
persistent regression gates.

This protocol mirrors the shape of [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md)
(E1–E4) and is its Arch-D sibling. Run E5–E7 in addition to (not instead
of) E1–E4 when validating a v0.1 ship.

## Prerequisites

- `agent-collab` + `agent-collab-daemon` + `peer-inbox` (Go binary) all
  on `$PATH`.
- `scripts/install-global-protocol` has been run and migration 0003 is
  applied. Verify:

  ```bash
  bash scripts/doctor-global-protocol
  sqlite3 ~/.agent-collab/sessions.db "PRAGMA table_info(sessions)" \
      | grep daemon_cli_session_id
  ```

  Expect one new column on `sessions` (added by migration 0003 in commit
  `e64c867`).

- `codex` 0.121+ and `gemini` 0.38+ each installed and logged in.
  Capture their version strings BEFORE running probes:

  ```bash
  codex --version
  gemini --version
  ```

  Record both in your run log. If either version differs from the
  fixture-pinned versions in `tests/fixtures/cli-session/`, the regex /
  parser MAY have drifted — E5 + E6 are designed to catch that case.
- Three terminals: one driver (Claude), one per daemon (codex + gemini),
  one observer.
- Read [DAEMON-OPERATOR-GUIDE.md § Architecture D](./DAEMON-OPERATOR-GUIDE.md#architecture-d--cli-native-session-id-pass-through-v01-opt-in)
  first — probes assume you understand the opt-in flag, capture/resume
  flows, reset primitives, and claude asymmetry.

## Probe matrix

Three probes. E5 covers codex banner-format drift detection. E6 covers
gemini `--list-sessions` serialization drift detection. E7 captures
pinned CLI versions for future drift-comparison runs.

### Probe E5 — Codex banner regex against live `codex 0.121+`

**Intent:** the daemon's regex `(?i)session id:\s*([0-9a-f-]{36})`
correctly captures the session-ID from a real `codex exec` invocation,
and the captured UUID correctly resumes the next batch.

**Setup:**

```bash
# Terminal 1 (driver — Claude Code or operator shell):
mkdir -p /tmp/daemon-probe-e5-driver && cd /tmp/daemon-probe-e5-driver
agent-collab session register --label e5-driver --agent claude

# Terminal 2 (daemon target — register codex in daemon mode + Arch D enabled):
mkdir -p /tmp/daemon-probe-e5-target && cd /tmp/daemon-probe-e5-target
SESSION_KEY=$(uuidgen)
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab session register \
    --receive-mode daemon --agent codex \
    --label e5-codex --cwd "$PWD"

# Start the daemon with --cli-session-resume opt-in. ack-timeout/sweep-ttl
# can stay at production defaults (300s/600s) for this probe.
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab-daemon \
    --label e5-codex --cwd "$PWD" \
    --session-key "$SESSION_KEY" --cli codex \
    --cli-session-resume \
    --log-path /tmp/daemon-probe-e5-target/daemon.log
```

**Probe:**

```bash
# Terminal 1 — send a peer-inbox message that prompts a reply.
agent-collab peer send --as e5-driver --to e5-codex \
  --message "first message in this thread. please reply with the literal text: ACK1"

# Wait for daemon to spawn + complete (60-90 seconds typical).
# Verify the captured session-ID in the DB:
sqlite3 ~/.agent-collab/sessions.db \
  "SELECT label, daemon_cli_session_id FROM sessions WHERE label='e5-codex'"
# Expect: e5-codex|<UUID-format>  (NOT e5-codex|<empty>)

# Send a SECOND message to verify resume:
agent-collab peer send --as e5-driver --to e5-codex \
  --message "second message in this thread. quote my first message back to me."

# After daemon completes batch 2, inspect the daemon log for the resume invocation:
grep "spawn.argv" /tmp/daemon-probe-e5-target/daemon.log | tail -2
# Expect: batch 2 spawn argv contains "codex exec resume <UUID> --skip-git-repo-check ..."
# AND the codex reply quotes the first-message content (proves session resumed correctly).
```

**Pass criteria:**
1. After batch 1: `sessions.daemon_cli_session_id` is non-NULL with a
   UUID-shape value (36 chars including hyphens).
2. Batch 2's spawn argv (in daemon log) contains
   `codex exec resume <captured-UUID>`.
3. Batch 2's codex reply quotes content from batch 1 (proves the CLI
   session was resumed at the vendor side, not just that the daemon
   passed the right argv).

**Fail handling:**
- If (1) is NULL: regex did not match. Capture `codex exec` stdout banner
  manually (`codex exec --skip-git-repo-check 'echo OK' 2>&1 | head -20`)
  and compare to `tests/fixtures/cli-session/codex-banner.txt`. If banner
  format changed, update the regex in `go/cmd/daemon/main.go` AND
  refresh the fixture file. File a v0.1.x bump.
- If (1) succeeds but (2) fails: argv-construction bug — inspect daemon
  source `spawnCodex` resume branch.
- If (1)+(2) succeed but (3) fails: vendor-side issue (codex's resume
  semantic broke or session-state expired between batches). Re-run with
  shorter delay; if still failing, escalate to codex vendor.

### Probe E6 — Gemini `--list-sessions` parser against live `gemini 0.38+`

**Intent:** the daemon's `--list-sessions` parser correctly extracts
UUIDs from real gemini output, the delta-snapshot capture identifies
the daemon's new session, and the UUID→index translation at resume
time uses the CURRENT index (not the stale capture-time index).

**Setup (recommend `GEMINI_CONFIG_DIR` isolation per operator guide):**

```bash
# Terminal 1 (driver):
mkdir -p /tmp/daemon-probe-e6-driver && cd /tmp/daemon-probe-e6-driver
agent-collab session register --label e6-driver --agent claude

# Terminal 2 (daemon target — gemini, isolated session store):
mkdir -p /tmp/daemon-probe-e6-target && cd /tmp/daemon-probe-e6-target
SESSION_KEY=$(uuidgen)
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab session register \
    --receive-mode daemon --agent gemini \
    --label e6-gemini --cwd "$PWD"

# Start daemon with GEMINI_CONFIG_DIR isolation + Arch D opt-in.
GEMINI_CONFIG_DIR=$HOME/.gemini-daemon-e6 \
  AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab-daemon \
    --label e6-gemini --cwd "$PWD" \
    --session-key "$SESSION_KEY" --cli gemini \
    --cli-session-resume \
    --log-path /tmp/daemon-probe-e6-target/daemon.log
```

**Probe:**

```bash
# Terminal 1 — send first message.
agent-collab peer send --as e6-driver --to e6-gemini \
  --message "first message. please reply with the literal text: ACK1"

# After daemon completes batch 1, capture the daemon-stored UUID:
CAPTURED_UUID=$(sqlite3 ~/.agent-collab/sessions.db \
  "SELECT daemon_cli_session_id FROM sessions WHERE label='e6-gemini'")
echo "Captured: $CAPTURED_UUID"
# Expect: a UUID-shape value (36 chars including hyphens).

# Verify gemini's view of its own sessions (in the isolated config dir):
GEMINI_CONFIG_DIR=$HOME/.gemini-daemon-e6 gemini --list-sessions | head -5
# Expect: at least one row with the captured UUID in brackets.

# Send a second message — daemon should re-query --list-sessions, find
# the captured UUID, translate to index, and resume.
agent-collab peer send --as e6-driver --to e6-gemini \
  --message "second message. quote my first message back to me."

# Inspect daemon log for the index-translation:
grep "spawn.argv" /tmp/daemon-probe-e6-target/daemon.log | tail -2
# Expect: batch 2 spawn argv contains "--resume <N> -p ..." where N is
# the current 1-based index of the captured UUID in --list-sessions.

# Optional: simulate index-shift by creating ANOTHER gemini session in
# the isolated config dir (this should push the captured UUID's index up):
GEMINI_CONFIG_DIR=$HOME/.gemini-daemon-e6 gemini -p 'noop' &
sleep 5
agent-collab peer send --as e6-driver --to e6-gemini \
  --message "third message. quote my second message back to me."
# Inspect log: batch 3 spawn argv should use the NEW index, not the stale one.
```

**Pass criteria:**
1. After batch 1: `daemon_cli_session_id` non-NULL with UUID shape.
2. `gemini --list-sessions` (in the isolated config dir) shows the
   captured UUID — proves the parser correctly identified the new
   session.
3. Batch 2's spawn argv contains `--resume <integer-index>` (NOT
   `--resume <UUID>`). Per scope §4.2 v3 amendment.
4. Batch 2's gemini reply quotes content from batch 1 (proves vendor
   resume worked).
5. (Optional index-shift) Batch 3's spawn argv uses the NEW index after
   the manual session-creation pushed the captured UUID up the list.

**Fail handling:**
- If (1) is NULL OR (2) lacks the captured UUID: parser drift. Capture
  `gemini --list-sessions` raw output and compare to
  `tests/fixtures/cli-session/gemini-list-sessions.txt`. If serialization
  format changed (regex `^\s*(\d+)\.\s+.*?\[([0-9a-f-]{36})\]\s*$`
  no longer matches each row), update parser in
  `go/cmd/daemon/main.go` AND refresh the fixture file. File a v0.1.x
  bump.
- If (3) shows `--resume <UUID>` (not integer): bug in the translation
  step — daemon is passing UUID directly. Inspect `spawnGemini` resume
  branch.
- If (5) shows the stale capture-time index: re-query at resume time
  is broken — daemon is using a cached index. Inspect
  `lookupGeminiSessionIndex`.
- Race-warning case: if daemon's `--list-sessions` AFTER snapshot shows
  multiple new UUIDs, daemon should leave column NULL + log warning;
  verify in daemon.log: `grep gemini_race_detected daemon.log`.

### Probe E7 — CLI version pinning + drift-comparison record

**Intent:** record the exact CLI versions that passed E5 + E6 so future
operators (or a v0.1.x bump auditor) can compare against and detect when
a CLI version bump introduced format drift.

**Run after E5 and E6 both pass:**

```bash
# Capture versions and basic fixture-match:
codex --version > /tmp/probe-e7-codex-version.txt
gemini --version > /tmp/probe-e7-gemini-version.txt

# Capture a fresh banner + list-sessions for comparison archival.
mkdir -p /tmp/daemon-probe-e7
codex exec --skip-git-repo-check 'echo READY' 2>&1 | head -20 > /tmp/daemon-probe-e7/codex-banner-live.txt
gemini --list-sessions 2>/dev/null | head -5 > /tmp/daemon-probe-e7/gemini-list-sessions-live-head.txt

# Diff against the repo-pinned fixtures:
diff /tmp/daemon-probe-e7/codex-banner-live.txt \
     "$REPO/tests/fixtures/cli-session/codex-banner.txt" \
  | head -20
diff /tmp/daemon-probe-e7/gemini-list-sessions-live-head.txt \
     <(head -5 "$REPO/tests/fixtures/cli-session/gemini-list-sessions.txt") \
  | head -20

# If diffs are empty (or only show data that changes per-invocation, like
# session UUIDs / timestamps): fixture format still valid. Record the
# CLI versions in your run log + the v0.1 closure tag's annotated message.
#
# If diffs show format changes: file a v0.1.x bump issue + refresh
# fixtures + verify regex / parser still match.
```

**Pass criteria:**
1. Both `codex --version` and `gemini --version` recorded.
2. `diff` against pinned fixtures shows only per-invocation variability
   (session UUIDs, timestamps) — not structural format changes.
3. Operator log entry recording the pinned versions for future
   drift-comparison.

## Probe results template

For each probe, record in your run log:

```
Probe ID: E5
Date: YYYY-MM-DD
codex version: codex-cli X.Y.Z
gemini version: <if applicable>
Outcome: PASS | FAIL | PARTIAL
Notes: <free-form observations, especially: did the regex / parser match?
        did vendor resume work? any unexpected stderr output? any
        operational caveats observed during the probe?>
Daemon log excerpt: <key lines from /tmp/daemon-probe-eN-target/daemon.log>
```

Persist these records alongside the v0.1 closure tag annotation OR in
the v0.1 handoff doc — they become the empirical baseline that a future
v0.1.x bump auditor compares against.

## Last-run

**2026-04-18 vs `v3.x-topic-3-v0.1.2-shipped` @ `6ec4b23`** — test-engineer supervised probe.

- **codex-cli 0.121.0** — E5 (a)(b)(c) **PASS**
  - UUID captured: `019da07f-5faf-7402-9842-b49e0f0cb6da`
  - Batch-2 argv: `exec resume 019da07f-5faf-7402-9842-b49e0f0cb6da --skip-git-repo-check <prompt>`
  - Batch-2 reply correctly recalled seeded context ("vermilion")
- **gemini 0.38.2** — E6 (a)(b)(c) **PASS**
  - UUID captured: `c52b718e-c38c-4f89-9554-91968921e0aa`
  - UUID→index translation via `--list-sessions` re-query → argv `--resume 1 -p <prompt>`
  - Batch-2 reply correctly recalled seeded context ("42")
- **claude 2.1.114** — out-of-scope (Arch B asymmetry per §4.3 of scope-doc)
- **E7 version record + fixture diff:** PASS (no structural drift in codex banner format or gemini `--list-sessions` output)

Probe dirs preserved at `/tmp/e5v2-probe`, `/tmp/e6v2-probe` for the
v0.1.2 baseline run. This run validates the v0.1.2 fixes
(codex stdout+stderr concat capture + gemini `--list-sessions`
timeout bump to 15s with `AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT`
env override) work end-to-end against real CLIs — the gap that v0.1.1
shipped without closing.

## When to re-run

- Before tagging `v3.x-topic-3-v0.1-shipped` — required for closure.
- After bumping `codex` or `gemini` CLI to a new version (covers the
  fixture-pin contract).
- After modifying any of: `spawnCodex`, `spawnGemini`,
  `parseGeminiListSessions`, `codexSessionIDRE`, or related helpers in
  `go/cmd/daemon/main.go`.
- Before merging any PR that touches `tests/fixtures/cli-session/*`.

## References

- [DAEMON-OPERATOR-GUIDE.md § Architecture D](./DAEMON-OPERATOR-GUIDE.md#architecture-d--cli-native-session-id-pass-through-v01-opt-in)
  — operator-facing concepts.
- [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) — Topic 3 v0 E2E probes
  (E1-E4); template this protocol mirrors.
- `plans/v3.x-topic-3-arch-d-scoping.md` — v0.1 scope-doc; §6.1 + §6.2
  for capture strategy contracts.
- `tests/daemon-cli-resume-codex.sh`, `tests/daemon-cli-resume-gemini.sh`
  — shape-2 CI gates that fixtures pin against.
- `tests/fixtures/cli-session/codex-banner.txt`,
  `tests/fixtures/cli-session/gemini-list-sessions.txt` — pinned fixtures.
