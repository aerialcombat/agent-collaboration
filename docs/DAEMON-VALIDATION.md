# Auto-Reply Daemon — E2E Validation Protocol

**Purpose:** validate that `agent-collab-daemon` actually delivers
peer-inbox messages to live Codex CLI and Gemini CLI sessions via
autonomous spawn-and-ack — not just that shape-2 CI gates pass.

**Why this exists:** the persistent CI gates
(`tests/daemon-harness.sh`, `tests/daemon-termination.sh`,
`tests/daemon-completion-ack.sh`) exercise the daemon against
fixtures and mocked CLI spawns. They do **not** validate that a
live Codex or Gemini binary spawned by the daemon actually receives
the envelope, reasons over it, replies via `peer send`, and
completion-acks through to `DaemonModeComplete`. That runtime
verification requires real CLIs, real model invocations, and human
supervision.

One-off smoke tests at ship time, not persistent regression gates.
Re-run whenever the daemon is modified, a CLI version bumps, or the
ack mechanism changes.

## Prerequisites

- `agent-collab` and `agent-collab-daemon` both on `$PATH`.
- `scripts/install-global-protocol` has been run and migration 0002
  is applied. Verify:
  ```bash
  bash scripts/doctor-global-protocol
  sqlite3 ~/.agent-collab/sessions.db "PRAGMA table_info(inbox)" \
      | grep -E 'claimed_at|completed_at|claim_owner'
  sqlite3 ~/.agent-collab/sessions.db "PRAGMA table_info(sessions)" \
      | grep -E 'receive_mode|daemon_state'
  ```
  Expect three new columns on `inbox`, two on `sessions`.
- Claude Code, Codex CLI, Gemini CLI each installed and logged in.
- Four terminals: one driver (Claude), one per daemon, one observer.
- Read [DAEMON-OPERATOR-GUIDE.md](./DAEMON-OPERATOR-GUIDE.md) first
  — probes assume you understand register → start-daemon → send →
  observe.

## Probe matrix

Four probes. E1/E2 cover happy-path Codex + Gemini auto-reply. E3
exercises crash recovery via sweeper. E4 exercises closed-state
claim-time enforcement (scope doc §3.4 guarantee (e)).

### Probe E1 — Claude → Codex daemon (happy path)

**Intent:** a Codex daemon receives a peer-inbox message from a
Claude driver session and replies autonomously.

**Setup:**

```bash
# Terminal 1 (driver — Claude Code):
cd /tmp/daemon-probe-e1-driver
agent-collab session register --label driver --agent claude

# Register the Codex target in daemon mode; capture SESSION_KEY.
agent-collab session register \
    --receive-mode daemon --agent codex \
    --label reviewer-codex-e1 \
    --cwd /tmp/daemon-probe-e1-codex

# Terminal 3 (daemon):
agent-collab-daemon \
    --label reviewer-codex-e1 \
    --cwd /tmp/daemon-probe-e1-codex \
    --session-key <SESSION_KEY> \
    --cli codex
# Expect first-line log: "daemon started label=... cli=codex poll=5s"
```

**Send (from driver Claude via Bash):**

```bash
PROBE=$(uuidgen | cut -c1-8)
agent-collab peer send \
    --to reviewer-codex-e1 \
    --to-cwd /tmp/daemon-probe-e1-codex \
    --message "Sanity check: reply with exactly 'ack-probe-E1-$PROBE' and nothing else. Then call peer receive --complete --as reviewer-codex-e1 as your final action."
```

**Verify (within ~30s):**

Daemon log should show:
```
claim rows=1 label=reviewer-codex-e1
spawn start cli=codex ...
spawn exit code=0
complete rows=1
```

DB state:
```bash
sqlite3 ~/.agent-collab/sessions.db <<SQL
SELECT id, claim_owner,
       claimed_at IS NOT NULL AS claimed,
       completed_at IS NOT NULL AS completed
FROM inbox WHERE to_label='reviewer-codex-e1'
ORDER BY id DESC LIMIT 1;
SQL
```
Expected: `claimed=1, completed=1, claim_owner='reviewer-codex-e1'`.

Driver reply visibility:
```bash
agent-collab peer receive --as driver
```
Expected: one message from `reviewer-codex-e1` with body containing
`ack-probe-E1-<UUID>`.

**Pass criteria:** all three assertions hold.

**Fail modes:**

- **No claim after 30s.** Check `receive_mode='daemon'` on sessions
  row; verify `--session-key` matches; verify `daemon_state='open'`.
- **Spawn started but non-zero exit.** Inspect captured stderr from
  daemon log. Common: Codex not logged in, daemon missing
  `--skip-git-repo-check` for non-git cwd (bug — file), stdin not
  closed (bug — file).
- **Spawn exited 0, no `complete rows=1`.** Spawned Codex didn't
  call `peer receive --complete` and didn't emit the JSONL fallback
  marker. Inspect spawn stdout. If neither arrived, daemon will hit
  `--ack-timeout` (that's E3's shape, not E1's happy path).
- **Driver never sees reply.** Spawn didn't call `peer send` back to
  `driver`. Prompt-tuning work; escalate.

### Probe E2 — Claude → Gemini daemon (happy path)

Identical shape to E1 with `gemini` substituted for `codex`
throughout. Use label `reviewer-gemini-e2`, probe string
`ack-probe-E2-$UUID`:

```bash
agent-collab session register \
    --receive-mode daemon --agent gemini \
    --label reviewer-gemini-e2 \
    --cwd /tmp/daemon-probe-e2-gemini

agent-collab-daemon \
    --label reviewer-gemini-e2 \
    --cwd /tmp/daemon-probe-e2-gemini \
    --session-key <SESSION_KEY> \
    --cli gemini
```

Pass criteria identical to E1. Fail modes add: Gemini may not
surface `peer_send` reliably under `gemini -p` prompt-tuning — check
spawn stdout for Gemini's tool-call shape and adjust the probe
prompt if needed.

### Probe E3 — Crash recovery via sweeper

**Intent:** verify the sweeper reverts `claimed_at` after SIGKILL of
the spawned CLI, and the next daemon claim handles the original
batch. Exercises scope doc §3.3 + §3.4 guarantees (c) and (d).

**Setup:** same as E1 with **reduced TTLs** for test viability
(preserve the 2× ratio):

```bash
agent-collab-daemon \
    --label reviewer-codex-e3 \
    --cwd /tmp/daemon-probe-e3-codex \
    --session-key <SESSION_KEY> \
    --cli codex \
    --ack-timeout 30s --sweep-ttl 60s
```

Start an external sweeper loop (default is `external`):

```bash
while true; do agent-collab peer-inbox sweep; sleep 15; done
```

**Send:**

```bash
PROBE=$(uuidgen | cut -c1-8)
agent-collab peer send \
    --to reviewer-codex-e3 \
    --to-cwd /tmp/daemon-probe-e3-codex \
    --message "Probe E3: wait 45 seconds, then reply 'ack-probe-E3-$PROBE' and --complete."
```

**Induce crash:** when daemon log shows `spawn start`, SIGKILL the
child Codex process:

```bash
DAEMON_PID=$(pgrep -f "agent-collab-daemon.*reviewer-codex-e3")
CODEX_PID=$(pgrep -P $DAEMON_PID codex)
kill -9 $CODEX_PID
```

**Verify (after `sweep-ttl + 15s` ≈ 75s):**

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "SELECT id, claim_owner, claimed_at, completed_at \
     FROM inbox WHERE to_label='reviewer-codex-e3' \
     ORDER BY id DESC LIMIT 1"
```

Expected: `claimed_at IS NULL` (sweeper reverted), `claim_owner`
**still set** to `reviewer-codex-e3` (preserved per scope doc §3.1
alpha §A — audit trail), `completed_at IS NULL`.

Within one poll interval post-sweep, daemon log should show a
second `claim rows=1` for the same inbox id. Fresh spawn may need a
subsequent fast-reply probe to prove full happy-path recovery.

**Pass criteria:** sweeper reverts `claimed_at`; `claim_owner`
preserved; daemon re-claims the same row after sweep.

**Fail modes:**

- Sweeper not running → verify the `while true` loop is executing.
- `claim_owner` nullified by sweeper → scope doc §3.3 alpha §A
  regression; file.

### Probe E4 — Closed-state enforcement

**Intent:** verify `sessions.daemon_state='closed'` prevents claims.
Exercises §3.4 guarantee (e).

**Setup:** E1-style Codex daemon at label `reviewer-codex-e4`.

**Part A — flip state externally:**

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "UPDATE sessions SET daemon_state='closed' \
     WHERE label='reviewer-codex-e4'"

agent-collab peer send \
    --to reviewer-codex-e4 \
    --to-cwd /tmp/daemon-probe-e4-codex \
    --message "This should NOT trigger a spawn."
```

After two poll intervals (~10s), assert:

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "SELECT claimed_at, completed_at FROM inbox \
     WHERE to_label='reviewer-codex-e4' ORDER BY id DESC LIMIT 1"
# Expected: both NULL — row untouched.

pgrep -f "codex exec" || echo "no codex spawn — expected"
```

Flip back to open and verify normal claim resumes:

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "UPDATE sessions SET daemon_state='open' \
     WHERE label='reviewer-codex-e4'"
# Within one poll interval, daemon should claim and spawn normally.
```

**Part B — `envelope.state: closed` message** (skip if v0 doesn't
expose an operator verb for `--envelope-state`):

```bash
agent-collab peer send \
    --to reviewer-codex-e4 \
    --to-cwd /tmp/daemon-probe-e4-codex \
    --envelope-state closed \
    --message "Daemon, please go dormant."

sqlite3 ~/.agent-collab/sessions.db \
    "SELECT daemon_state FROM sessions WHERE label='reviewer-codex-e4'"
# Expected: 'closed'
```

**Pass criteria (A):** no spawn with `daemon_state='closed'`; row
stays `claimed_at IS NULL`; flipping back to `'open'` restores.
**Pass criteria (B, if testable):** receiving `envelope.state:
closed` sets `daemon_state='closed'`.

**Fail modes:** daemon spawned while closed → §3.4 guarantee (e)
violated, file as correctness regression.

## Running the protocol

1. **Shape-2 CI gates first** — probes assume these pass:
   ```bash
   bash tests/hook-parity.sh
   bash tests/daemon-harness.sh
   bash tests/daemon-termination.sh
   bash tests/daemon-completion-ack.sh
   ```
2. **`scripts/doctor-global-protocol`** — hook registration + schema
   preconditions.
3. **Execute E1, E2, E3, E4 in order.** Each probe is independent
   (distinct label, distinct cwd). E1 + E2 must pass for daemon-mode
   ship-readiness. E3 validates crash-recovery safety. E4 validates
   Layer-2 termination.
4. **Report results as a table** (probe × outcome → pass / fail /
   skip with notes). Attach daemon log excerpts for any fail.

## What probes do NOT validate

- **Cross-host daemon coordination** — single-machine only; v3.3+.
- **MCP-tool-backed completion-ack** — v1+ per §7.2 mechanism (3).
- **Concurrent-batch-per-daemon** — v0 enforces single-batch-at-a-
  time per §7.3.
- **API-backed cost model** — rough heuristics in operator guide;
  precise model belongs to Topic 2.
- **Cline / Cursor / Windsurf spawn support** — out of scope full
  horizon.

## Cleanup

```bash
# Ctrl-C each daemon.
agent-collab session close --label driver
agent-collab session close --label reviewer-codex-e1 \
    --cwd /tmp/daemon-probe-e1-codex
agent-collab session close --label reviewer-gemini-e2 \
    --cwd /tmp/daemon-probe-e2-gemini
agent-collab session close --label reviewer-codex-e3 \
    --cwd /tmp/daemon-probe-e3-codex
agent-collab session close --label reviewer-codex-e4 \
    --cwd /tmp/daemon-probe-e4-codex
rm -rf /tmp/daemon-probe-*
```

## References

- [DAEMON-OPERATOR-GUIDE.md](./DAEMON-OPERATOR-GUIDE.md) — full
  operator reference (config, architecture, termination stack,
  completion-ack contract, troubleshooting).
- [HOOK-PARITY-VALIDATION.md](./HOOK-PARITY-VALIDATION.md) — Option
  J probe protocol (template for these probes).
- `plans/v3.x-topic-3-implementation-scope.md` — §8.3 probe scope,
  §3.4 five-guarantee contract, §7.2 ack mechanisms, §6 termination.
- `tests/daemon-harness.sh`, `tests/daemon-termination.sh`,
  `tests/daemon-completion-ack.sh` — persistent CI gates; these
  probes complement, do not replace.
