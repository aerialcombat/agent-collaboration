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

### Probe E5 — Codex banner regex against live `codex 0.121+` — RETIRED v0.3 (see §E10)

> **RETIRED v0.3.** Topic 3 v0.3 collapse retires codex-direct spawning
> in favor of pi-routed (`--cli=pi --pi-provider=openai-codex`). The
> codex banner-regex capture code path this probe validated is deleted;
> pi owns path-as-identity session resume end-to-end. E5's coverage is
> subsumed by §E10 Phase 10a (pi-openai-codex through live CLI).
>
> The E5 protocol + its last-run history block are preserved below as
> empirical baseline for the v0.1-era tag annotations (per test-
> engineer F4 preservation pattern). Do NOT run E5 against v0.3+
> builds — the daemon no longer spawns `codex exec` directly.

**Original (v0.1-era) protocol follows:**


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

### Probe E6 — Gemini `--list-sessions` parser against live `gemini 0.38+` — RETIRED v0.3 (see §E10)

> **RETIRED v0.3.** Topic 3 v0.3 collapse retires gemini-direct
> spawning in favor of pi-routed (`--cli=pi --pi-provider=google-antigravity`).
> The gemini `--list-sessions` delta-snapshot + UUID→index translation
> code path this probe validated is deleted; pi owns path-as-identity
> session resume end-to-end (race-free by construction; no enumeration
> timeout surface). E6's coverage is subsumed by §E10 Phase 10b (pi-
> google-antigravity through live CLI).
>
> The E6 protocol + its last-run history block are preserved below as
> empirical baseline for the v0.1-era tag annotations (per test-
> engineer F4 preservation pattern). Do NOT run E6 against v0.3+
> builds — the daemon no longer spawns `gemini --list-sessions`.

**Original (v0.1-era) protocol follows:**


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

### Probe E8 — Pi `--session <PATH>` round-trip against live `pi 0.67+` + GLM

**Intent:** the daemon mints `$pi.session_dir/$label.jsonl`, passes it to
pi via `--session`, pi persists cross-spawn context to the file, and a
subsequent spawn recalls context via the same path. Additionally validate
pi reset deletes the file from disk (§8.1 pi-specific extension). Topic
3 v0.2 §9.2 gate 6 + test-engineer §10.1 (E) add.

**Prerequisites:** `pi --version` ≥ `0.67.68`, `ZAI_GLM_API_KEY` exported
(zai-glm is served by the `pi-zai-glm` plugin; the plugin reads
`ZAI_GLM_API_KEY`, NOT `ZAI_API_KEY` — `pi --help`'s env table
enumerates built-in providers only and is blind to plugin env vars; see
operator guide § Arch D pi sub-section for the built-in-vs-plugin
distinction. v0.2.1 correction), operator has connectivity to GLM
provider.

> **Do NOT override `$HOME` for E8 isolation** — pi reads provider-auth
> config from `~/.pi/agent/`; a re-pointed `$HOME` hides the operator's
> credentials from the daemon-spawned pi. Isolate via
> `AGENT_COLLAB_INBOX_DB=<path>` (daemon store) + `--pi-session-dir <path>`
> (daemon session files) instead. Per test-engineer E8 supervised-probe
> finding 2026-04-18.

**Setup:**

```bash
# Terminal 1 (driver):
mkdir -p /tmp/daemon-probe-e8-driver && cd /tmp/daemon-probe-e8-driver
agent-collab session register --label e8-driver --agent claude

# Terminal 2 (daemon target — pi with zai-glm):
mkdir -p /tmp/daemon-probe-e8-target && cd /tmp/daemon-probe-e8-target
SESSION_KEY=$(uuidgen)
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab session register \
    --receive-mode daemon --agent pi \
    --label e8-pi --cwd "$PWD"

# Start daemon with Arch D opt-in + required pi fields.
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab-daemon \
    --label e8-pi --cwd "$PWD" \
    --session-key "$SESSION_KEY" --cli pi \
    --pi-provider zai-glm --pi-model glm-4.6 \
    --cli-session-resume \
    --log-path /tmp/daemon-probe-e8-target/daemon.log
```

**Probe:**

```bash
# Terminal 1 — batch 1: seed context.
agent-collab peer send --as e8-driver --to e8-pi \
  --message "first message. remember the word: cornflower. reply only with ACK1."

# Wait for daemon batch 1 completion.
CAPTURED_PATH=$(sqlite3 ~/.agent-collab/sessions.db \
  "SELECT daemon_cli_session_id FROM sessions WHERE label='e8-pi'")
echo "Captured path: $CAPTURED_PATH"
# Expect: path ending in /pi-sessions/e8-pi.jsonl

# Session file should exist on disk and contain pi turn JSONL.
ls -l "$CAPTURED_PATH"
wc -l "$CAPTURED_PATH"

# Batch 2 — test context recall.
agent-collab peer send --as e8-driver --to e8-pi \
  --message "what word did I ask you to remember? reply with just that word."

# Inspect daemon log: batch 2 spawn argv must reuse the cached path.
grep 'daemon.spawn.exec' /tmp/daemon-probe-e8-target/daemon.log | tail -2
# Expect: argv has "--session /path/to/e8-pi.jsonl" matching $CAPTURED_PATH.

# Batch 3 — reset, then verify file is GONE.
peer-inbox daemon-reset-session \
  --cwd /tmp/daemon-probe-e8-target --as e8-pi --format json
# Expect JSON payload: {"reset":true,"label":"e8-pi","deleted_file":"<path>"}
ls "$CAPTURED_PATH" 2>&1 || echo "CONFIRMED: file deleted"

# Batch 4 — after reset, context must NOT persist.
agent-collab peer send --as e8-driver --to e8-pi \
  --message "what word did I ask you to remember earlier? reply with just that word or NONE."
# Expect: pi replies "NONE" (or similar non-cornflower), proving reset actually reset.
```

**Pass criteria:**
1. After batch 1: `daemon_cli_session_id` non-NULL with path shape
   (contains `/`) pointing to an existing JSONL file.
2. Session file contains ≥1 JSONL line after batch 1 (pi wrote turn).
3. Batch 2's spawn argv contains `--session <same-path>` AND pi's reply
   quotes "cornflower" (proves vendor-side context preserved).
4. `daemon-reset-session --format json` payload contains a
   `"deleted_file": "<path>"` field + the file is GONE from disk.
5. Batch 4's reply does NOT contain "cornflower" — reset actually reset.

**Fail handling:**
- If (1) column is NULL: capture failed. Inspect daemon log for
  `pi_set_failed` or `pi_mkdir_failed` warnings.
- If (2) file missing but column set: pi's own write path broke. Check
  pi binary, provider auth, and `pi --help` for `--session` flag.
- If (3) argv missing `--session` or pi reply does NOT recall context:
  resume-on form is broken. Inspect `spawnPi` branch in
  `go/cmd/daemon/main.go`.
- If (4) file remains after reset: agent-gate or os.Remove broken.
  Inspect `runResetSession` in `go/cmd/peer-inbox/main.go`.
- If (5) reply still recalls "cornflower" after reset: pi is reading a
  stale file (rare; check that file path is truly deleted and no
  backup/`.deleted` artifact exists).

### Probe E10 — Collapsed-path shim: codex via pi-openai-codex + gemini via pi-google-antigravity

**Intent:** Topic 3 v0.3 collapse (`plans/v3.x-topic-3-v0.3-collapse-scoping.md`)
retires codex-direct + gemini-direct in favor of pi-routed equivalents.
E10 validates the SOFT SHIM end-to-end against live CLIs + real
provider endpoints. Mandatory for v0.3 closure per §11 + the v0.1.2
meta-lesson (fake-binary gates don't validate real-CLI behavior).

Single umbrella probe, BOTH phases required. Each phase asserts
session-resume (capture + reuse + file-delete-on-reset) AND v3.1
[PROMPT] peer-send reply-path works end-to-end.

**Prerequisites:** `pi --version` ≥ `0.67.68`. Auth for both providers
available on operator install:
- `~/.pi/agent/auth.json` contains `openai-codex` OAuth entry (run
  `pi /login` once for openai-codex if not present).
- `~/.pi/agent/auth.json` contains `google-antigravity` OAuth entry
  (run `pi /login` once for google-antigravity if not present; refresh
  is auto-handled by pi-mono).

#### Phase 10a — `--cli=codex` shim → pi-openai-codex

**Setup + probe:**

```bash
mkdir -p /tmp/e10a-driver && cd /tmp/e10a-driver
agent-collab session register --label e10a-driver --agent claude

mkdir -p /tmp/e10a-target && cd /tmp/e10a-target
SESSION_KEY=$(uuidgen)
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab session register \
    --receive-mode daemon --agent codex \
    --label e10a-codex --cwd "$PWD"

# Start the SHIM daemon — --cli=codex, not --cli=pi. Daemon emits the
# deprecation warning + routes through spawnPi internally.
AGENT_COLLAB_SESSION_KEY=$SESSION_KEY \
  agent-collab-daemon \
    --label e10a-codex --cwd "$PWD" \
    --session-key "$SESSION_KEY" \
    --cli codex --pi-model gpt-5.3-codex \
    --cli-session-resume \
    --log-path /tmp/e10a-target/daemon.log
# Expect in daemon.log + stderr:
#   "--cli=codex is routed through pi as of v0.3 ..."

# Driver — batch 1: seed context + instruct reply.
agent-collab peer send --as e10a-driver --to e10a-codex \
  --message "remember the word: amethyst. reply to me with 'ACK1' using: agent-collab peer send --as e10a-codex --to e10a-driver --message 'ACK1'"

# Wait for completion. Assert:
# (a) session file created at /tmp/e10a-target/pi-sessions/e10a-codex.jsonl (or
#     $HOME/.agent-collab/pi-sessions/e10a-codex.jsonl if session_dir default).
# (b) daemon.log contains: daemon.spawn.exec with argv including
#     --provider openai-codex + --session <path>.
# (c) e10a-driver's inbox contains the "ACK1" reply from e10a-codex
#     (v3.1 peer-send reply-path assertion).

# Driver — batch 2: context recall.
agent-collab peer send --as e10a-driver --to e10a-codex \
  --message "what word did I ask you to remember? reply with just the word."

# Assert: reply quotes "amethyst" (proves vendor-side context retained
# across spawns via pi-routed path).

# Reset + negative recall.
peer-inbox daemon-reset-session --cwd /tmp/e10a-target --as e10a-codex --format json
# Expect JSON contains deleted_file.

agent-collab peer send --as e10a-driver --to e10a-codex \
  --message "what word did I ask you to remember earlier? reply with NONE if you don't remember."

# Assert: reply does NOT contain "amethyst".
```

**Phase 10a pass criteria:**
1. Deprecation warning in daemon.log + stderr.
2. Argv per `daemon.spawn.exec` contains `--provider openai-codex` +
   `--session <path>` (no legacy `exec resume` / `--skip-git-repo-check`).
3. Session file created at `$pi.session_dir/e10a-codex.jsonl`.
4. Batch 2 reply contains "amethyst" (context retained).
5. Reset JSON contains `deleted_file`; file deleted from disk.
6. Batch 4 reply does NOT contain "amethyst" (reset actually reset).
7. Peer-send reply from batch 1 lands in e10a-driver inbox
   (v3.1 peer-send reply-path assertion).

#### Phase 10b — `--cli=gemini` shim → pi-google-antigravity

Identical structure to Phase 10a with:
- `--cli=gemini --pi-model=gemini-3-flash`
- `--agent gemini` at session register
- Label `e10b-gemini` / driver `e10b-driver`

Assert: argv contains `--provider google-antigravity`. No legacy
`--list-sessions` / `--resume N` tokens. Peer-send reply lands in
driver inbox. Context recall + reset behave per Phase 10a's criteria.

**E10 overall pass criterion:** BOTH Phase 10a AND Phase 10b PASS.
Either phase failing blocks the v0.3 closure tag.

**Fail handling:**
- Argv missing `--provider` / wrong provider value: shim mapping
  regression. Inspect `shimProviderMap` in `go/cmd/daemon/main.go`
  parseFlags.
- Deprecation warning absent: `run()` startup warning wire broken.
- Peer-send reply missing from driver inbox: v3.1 [PROMPT] patch
  regression. Re-check `tests/daemon-prompt-interpolation.sh` + live
  CLI's interpretation of `{{SELF_LABEL}}` interpolation.
- `deleted_file` absent from reset JSON: path-shape guard or Shape β
  WIDE agent-gate regression. Inspect `runResetSession`.

## Probe results template

For each probe, record in your run log:

```
Probe ID: E5 (RETIRED v0.3) | E6 (RETIRED v0.3) | E7 | E8 | E10 (10a / 10b)
Date: YYYY-MM-DD
codex version: codex-cli X.Y.Z   (E5, E7)
gemini version: gemini X.Y.Z     (E6, E7)
pi version: pi-mono X.Y.Z        (E8, E10)
provider + model: zai-glm / glm-4.6 (E8); openai-codex / gpt-5.3-codex (E10a); google-antigravity / gemini-3-flash (E10b)
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

**2026-04-18 vs `v3.x-topic-3-v0.2-shipped` @ `ab61c24`** — test-engineer supervised probe.

- **pi-mono 0.67.68** — E8 (a)(b)(c)(d)(e) **PASS**
  - Captured path: `/tmp/e8-probe/pi-sessions/e8-pi.jsonl`
  - Batch-2 argv: `--session <same path>`; reply recalled "cornflower"
  - Reset emitted `deleted_file`; file gone from disk; column NULL'd
  - Post-reset recall → "NONE"; fresh session UUID (vendor-side context
    truly discarded)

This run validates v0.2's Arch D pi path (path-as-identity capture +
reuse + file-delete-on-reset + reset-actually-reset) end-to-end against
the real pi 0.67.68 CLI + ZAI GLM provider. Closes the v0.2 ship per
the v0.1.2 meta-lesson (fake-binary gates don't validate real-CLI
behavior).

§E8 Prerequisites note (ride-along at `ab61c24`): do NOT override
`$HOME` for probe isolation — pi reads provider-auth config from
`~/.pi/agent/`. Isolate via `AGENT_COLLAB_INBOX_DB=<path>` +
`--pi-session-dir <path>` instead.

**2026-04-18 vs `v3.x-topic-3-v0.2.1-shipped` @ `b66476a`** — test-engineer env-var revert verification.

- **pi-mono 0.67.68** — E8 capture+resume PASS under `ZAI_GLM_API_KEY`-only (`ZAI_API_KEY` unset)
  - Captured path: `/tmp/e8v021-probe/pi-sessions/e8v021-pi.jsonl`
  - Batch-2 reply "sapphire"; argv `--session <same path>`; mechanism2 in ~3s
  - Confirms v0.2.1 env-var revert (v2 `ZAI_API_KEY` "fix" reverted to plugin-correct `ZAI_GLM_API_KEY`)

**2026-04-19 vs v3.1 [PROMPT] patch @ `23da0c1`** — test-engineer E9 probe.

- **room-pi** (pi+zai-glm)         — self-label ✓, interpolation ✓, peer-send ✓
- **room-pi-codex** (pi+openai)    — self-label ✓, interpolation ✓, peer-send ✓
- **room-pi-gemini** (pi+google-a) — self-label ✓, interpolation ✓, peer-send ✓
- **room-codex** (codex-direct)    — self-label ✓, interpolation ✓, peer-send ✓
- **room-gemini** (gemini-direct)  — no reply; daemon log shows spawn→ack_timeout_abandoned after 180s; consistent with gemini peer-send behavioral quirk from v0.1.2 dogfood (NOT a v3.1 regression)
- round-1 deployment-gap probe confirmed daemons need restart to pick up prompt patches (meta-lesson: patch-merged ≠ patch-deployed)

## When to re-run

- Before tagging `v3.x-topic-3-v0.1-shipped` — required for closure (E5 + E6, both now RETIRED v0.3).
- Before tagging `v3.x-topic-3-v0.2-shipped` — E8 required for closure (per v0.1.2 ship-closure meta-lesson: fake-binary gates don't validate real-CLI behavior).
- Before tagging `v3.x-topic-3-v0.3-shipped` — E10 BOTH phases required for closure (Phase 10a openai-codex + Phase 10b google-antigravity; same meta-lesson).
- After bumping `pi` CLI or any provider model the shim targets (openai-codex, google-antigravity) to a new version.
- After modifying any of: `spawnPi`, `resolvePiSessionPath`, `mintPiSessionPath`, `shimProviderMap`, the shim deprecation-warning strings in `run()`, or the reset-verb Shape β + path-shape guard logic.
- Before merging any PR that touches `tests/fixtures/cli-session/*`.

## References

- [DAEMON-OPERATOR-GUIDE.md § Architecture D](./DAEMON-OPERATOR-GUIDE.md#architecture-d--cli-native-session-id-pass-through-v01-opt-in)
  — operator-facing concepts. Post-v0.3: codex + gemini sub-sections
  are RETIRED banners pointing to the pi-routed canonical form; pi
  sub-section covers all non-claude workflows; new "Migrating from v0.2
  codex-direct / gemini-direct" subsection documents operator flow.
- [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) — Topic 3 v0 E2E probes
  (E1-E4); template this protocol mirrors.
- `plans/v3.x-topic-3-arch-d-scoping.md` — v0.1 scope-doc; §6.1 + §6.2
  for capture strategy contracts.
- `plans/v3.x-topic-3-v0.2-pi-scoping.md` — v0.2 scope-doc; §3.4
  invariants pi-reading (especially invariant 5 re-create-at-same-path);
  §8.1 pi-specific reset semantics (file-delete gated on
  `sessions.agent == 'pi'`).
- `plans/v3.x-topic-3-v0.3-collapse-scoping.md` — v0.3 scope-doc;
  §3.2.b SOFT SHIM mechanism, §3.3 Shape β WIDE + path-shape guard,
  §4.1 + §4.2 codex/gemini retirement, §9.3 migration story.
- `tests/daemon-cli-resume-pi.sh`, `tests/daemon-pi-session-lifecycle.sh`,
  `tests/daemon-collapse-migration.sh` — active CI gates (pi-native +
  v0.3 shim regression). `daemon-cli-resume-codex.sh` +
  `daemon-cli-resume-gemini.sh` are retired-banner stubs post-v0.3.
- `tests/fixtures/cli-session/codex-banner.txt`,
  `tests/fixtures/cli-session/gemini-list-sessions.txt`,
  `tests/fixtures/cli-session/pi-help.txt` — pinned fixtures (pi-help
  uses grep-pattern + version-marker check per §10 Q2 ratification; NOT
  full-diff).
