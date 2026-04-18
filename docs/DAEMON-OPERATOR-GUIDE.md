# Auto-Reply Daemon — Operator Guide

**Applies to:** Topic 3 v0 auto-reply daemon (`agent-collab-daemon`,
Go binary at `go/cmd/daemon/`).
**Source of truth:** `plans/v3.x-topic-3-implementation-scope.md`.
**Related docs:**
[PEER-INBOX-GUIDE.md](./PEER-INBOX-GUIDE.md) ·
[DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) ·
[HOOK-PARITY-VALIDATION.md](./HOOK-PARITY-VALIDATION.md)

---

## Table of contents

1. [What this is](#what-this-is)
2. [Quick start](#quick-start)
3. [Config reference](#config-reference)
4. [Architecture D — CLI-native session-ID pass-through (v0.1, opt-in)](#architecture-d--cli-native-session-id-pass-through-v01-opt-in)
5. [How it works (architecture)](#how-it-works-architecture)
6. [Termination stack](#termination-stack)
7. [Completion-ack contract](#completion-ack-contract)
8. [Troubleshooting](#troubleshooting)
9. [Security surface](#security-surface)
10. [Cost model](#cost-model)

---

## What this is

The auto-reply daemon is an OS-local process (one per managed label per
workspace) that watches the peer-inbox for messages addressed to a
specific label and autonomously services them by spawning a fresh
`claude -p` / `codex exec` / `gemini -p` invocation per batch. The
spawned CLI receives the peer-inbox envelope as its user prompt,
answers, and signals completion. The daemon marks the batch complete,
then polls for the next one.

Who this is for: operators running **Codex CLI** or **Gemini CLI** who
want peer-inbox auto-reply parity with Claude Code. Claude Code
sessions already receive messages via the `UserPromptSubmit` hook on
every turn boundary, and if launched with the Channels flag they also
receive real-time mid-turn pushes — neither the hook path nor the
Channels path requires the daemon. Who this is **not** for: users of
IDE-embedded agents (Cline / Cursor / Windsurf — out of horizon), and
cross-host coordination (deferred to v3.3+). The daemon operates on a
single machine against a local SQLite peer-inbox.

---

## Quick start

Assume you have a Codex CLI session in `/path/to/workspace` and you
want it to auto-reply to peer-inbox messages addressed to the label
`reviewer-codex`. Four commands, three terminals.

**1. Register the label in daemon receive mode.** This writes the
sessions row with `receive_mode='daemon'` so interactive receive verbs
refuse to consume this label's rows (fail-loud on misconfig per scope
doc §3.4 guarantee (b)).

```bash
# From any terminal with agent-collab on PATH:
agent-collab session register \
    --receive-mode daemon \
    --agent codex \
    --label reviewer-codex \
    --cwd /path/to/workspace
```

The register command will surface a resolved `session_key` — record
it, you'll pass it to the daemon so the spawned Codex processes match
back to the same sessions row.

**2. Confirm registration via `peer list`.**

```bash
agent-collab peer list --include-stale
```

Expected: one row for `reviewer-codex` with `receive_mode: daemon`.

**3. Start the daemon.** In a dedicated terminal (or under
`systemd` / `launchctl` / `tmux`), run:

```bash
agent-collab-daemon \
    --label reviewer-codex \
    --cwd /path/to/workspace \
    --session-key <KEY-FROM-STEP-1> \
    --cli codex
```

The daemon begins polling. First-line log should read something like
`daemon started label=reviewer-codex cli=codex poll=5s`.

**4. Send a message from any other session.**

```bash
agent-collab peer send \
    --to reviewer-codex \
    --message "sanity check: reply with a one-liner confirming you're up"
```

Within one poll interval the daemon claims the row, spawns
`codex exec`, the spawned Codex writes a reply via
`agent-collab peer send`, and completion-acks via
`agent-collab peer receive --complete --as reviewer-codex`. The
original sender sees the reply on its next turn (Claude auto-inject)
or next `agent-collab peer receive` call.

When you're done, stop the daemon with `Ctrl-C` (graceful) or signal
it to `daemon_state='closed'` — see [Termination
stack](#termination-stack).

---

## Config reference

The daemon is configured via CLI flags and environment variables.
**Flag names marked below correspond to concepts ratified in the
scope doc; the exact spellings may be refined during commit 7
(daemon binary implementation) — run `agent-collab-daemon --help`
against your installed binary for the authoritative flag list.**
Scope-doc-ratified concepts + defaults from §2.2, §3.3, §3.4, §4,
§6, §7.2:

| Concept | Default | Ratified in | Meaning |
|---|---|---|---|
| Managed `--label` | — (required) | §2.2 | The label the daemon services. One daemon per label per cwd. |
| `--cwd` | `$PWD` (canonical) | §2.2 | Workspace root used to resolve the sessions row. |
| `--session-key` (`AGENT_COLLAB_SESSION_KEY`) | — (required) | §4 item 1 | Stable session discriminator exported into every spawned CLI's env so the spawn's hook / verb resolution returns the daemon-registered row. |
| `--cli` kind | — (required) | §2.2 | Which CLI the daemon spawns per batch: `claude`, `codex`, or `gemini`. |
| `--ack-timeout` (`AGENT_COLLAB_ACK_TIMEOUT`) | `5m` (300s) | §3.3, §3.4 (c), §4 item 6 | Per-batch internal clock. When a spawn exceeds this, the daemon abandons it internally and does not call `--complete`. |
| `--sweep-ttl` (`AGENT_COLLAB_SWEEP_TTL`) | `10m` (600s) | §3.3, §3.4 (c) | Wall-clock age after which the sweeper reverts `claimed_at`. **MUST be strictly greater than `--ack-timeout`** — 2× ratio is the ratified default. |
| Poll interval | ~5s (suggested) | §3.3 conceptually | How often the daemon polls the DB for unclaimed rows. Subject to exponential backoff on rapid same-pair turns + pause-on-idle (§6 Layer 3). |
| Pause-on-idle threshold | `30m` | §6 Layer 3 | After this many seconds of inactivity, daemon reduces poll frequency and logs a pause event. Set `0` to disable. |
| Daemon log path | `~/.agent-collab/daemon-<label>.log` (suggested) | §4 item 7 implicit | Daemon's own log stream. Spawned CLI stdout/stderr is separately logged per spawn. |
| Sweeper mode | external subcommand + timer | §3.3 | Scope doc recommends external `peer-inbox sweep` on a timer over an in-daemon goroutine so a crashed daemon doesn't take out its own recovery mechanism. |
| TTL CI overrides | env vars + CLI flags | §3.3, §8.2 | `AGENT_COLLAB_ACK_TIMEOUT` / `AGENT_COLLAB_SWEEP_TTL` (and the CLI flags above) let tests run with `ack=1s, sweep=2s` preserving the 2× ratio, instead of the 5m/10m production defaults. |

### TTL ordering invariant

**`--sweep-ttl` MUST be strictly greater than `--ack-timeout`** — the
scope doc §3.4 guarantee (c) requires the sweeper's wall-clock check
to fire after the daemon's internal abandonment, so `--complete` is
never called for a would-be-reaped batch. Production defaults use a
**2× ratio** (10m / 5m); CI overrides typically use `ack-timeout=1s,
sweep-ttl=2s` preserving the same ratio.

The daemon **rejects misconfiguration at startup**: launching with
`--ack-timeout 10s --sweep-ttl 5s` (ratio inverted) exits non-zero
with a clear error message. This is a fail-loud guarantee, not a
silent race (§3.4 (c)).

### CLI-kind specifics

The daemon transparently handles the per-CLI operational requirements
catalogued in scope doc §4:

- **Codex (`--cli codex`)**: passes `< /dev/null` (stdin close) so
  `codex exec` doesn't hang waiting for input. Adds
  `--skip-git-repo-check` unconditionally so spawns in non-git
  workspaces succeed.
- **Claude (`--cli claude`)**: passes `--settings <daemon-owned
  settings.json>` so the spawn explicitly loads hook config. This is
  a **mandatory regression guard** against the future `--bare` default
  for `claude -p` (`--bare` would silently skip hooks + MCP + CLAUDE.md
  and break peer-inbox delivery to the spawned agent; scope doc §4
  bullet 5 + §8.2 mandatory fixture-pinned test).
- **Gemini (`--cli gemini`)**: nothing extra beyond the env-var export
  (Gemini's hook-timeout-in-milliseconds quirk is handled at install
  time by `scripts/install-global-protocol`; the daemon doesn't
  re-specify it).

---

## Architecture D — CLI-native session-ID pass-through (v0.1, opt-in)

**Status:** v0.1 (default OFF). Topic 3 v0 ships fresh-invocation per
batch (Architecture B). Architecture D layers an opt-in extension that
passes each CLI's native session-ID into subsequent spawn invocations,
giving cross-spawn context continuity for `codex` and `gemini` daemons
without the operational complexity of long-running CLI shells.

Scope-doc references:
- `plans/v3.x-topic-3-arch-d-scoping.md` — v0.1 scope (codex + gemini +
  claude asymmetry); v0.1.1/v0.1.2 patch changelogs appended.
- `plans/v3.x-topic-3-v0.2-pi-scoping.md` — v0.2 scope (pi as 4th Arch D
  CLI; additive path-as-identity semantics).

Manual operator probe protocol: [DAEMON-CLI-SESSION-VALIDATION.md](./DAEMON-CLI-SESSION-VALIDATION.md). Probes E5/E6 cover codex/gemini; §E8 covers pi + provider end-to-end round-trip. Required for the v0.2 closure tag per the v0.1.2 ship-closure meta-lesson (fake-binary gates don't validate real-CLI behavior).

### When to enable

Enable Arch D when:
- The daemon's spawned CLI benefits from carrying conversation/state
  across batches (multi-message threads, on-going code-review work,
  any reactive role where prior context aids responsiveness).
- You accept the trust-boundary trade: untrusted CLI-side state crosses
  spawns. One bad spawn can poison subsequent ones until a reset
  (operator opt-in is the explicit acceptance lever — scope §3.1).

Leave Arch D OFF when:
- Each batch should be independent (stateless review, fresh context per
  message).
- You're managing a `claude` daemon (asymmetry — see below).

### How to enable

Three equivalent surfaces, with precedence: CLI flag > config file > env > default false.

```bash
# CLI flag (one-off)
agent-collab-daemon --cli-session-resume --label reviewer-codex --cwd ... --cli codex --session-key ...

# Config file (~/.agent-collab/daemons/<label>.json)
{ "label": "reviewer-codex", ..., "cli_session_resume": true }

# Env var (shared-shell pattern)
AGENT_COLLAB_CLI_SESSION_RESUME=1 agent-collab-daemon --config reviewer-codex
```

### Per-CLI behavior

- **Codex (`--cli codex`)**: daemon spawns first batch with
  `codex exec --skip-git-repo-check <prompt>` and captures the
  session-ID from the stdout banner regex
  `(?i)session id:\s*([0-9a-f-]{36})`. Subsequent batches spawn
  `codex exec resume --skip-git-repo-check <UUID> <prompt>`. Session-ID
  persisted in `sessions.daemon_cli_session_id`.

- **Gemini (`--cli gemini`)**: daemon snapshots `gemini --list-sessions`
  before/after first spawn; new UUID = set-difference.
  **Important: gemini 0.38.2's `--resume` flag is documented as
  accepting `"latest"` or `<index>`, NOT UUIDs.** The daemon stores the
  UUID for stable identity but translates UUID → current-index via
  `--list-sessions` re-query at each resume invocation
  (`gemini --resume <N> -p <prompt>`). This is a v3 amendment to the
  scope doc per Checkpoint 1 finding (direct-UUID-resume works
  empirically in 0.38.2 but is undocumented; v0.1 builds on the
  documented index-addressing API).

  **Concurrent-gemini race**: if `--list-sessions` shows multiple new
  UUIDs after the daemon's first spawn (another gemini session was
  created elsewhere), the daemon **does NOT pick a winner** — it logs
  a warning and leaves the column NULL. Daemon falls through to
  Arch B fresh-invocation that batch (recoverable). To prevent this
  by construction, set `GEMINI_CONFIG_DIR=$HOME/.gemini-daemon-<LABEL>`
  in the daemon's environment so its session store is isolated from
  any operator interactive gemini sessions.

  ```bash
  # Race-by-construction prevention (recommended for production)
  GEMINI_CONFIG_DIR=$HOME/.gemini-daemon-reviewer-gemini \
    AGENT_COLLAB_CLI_SESSION_RESUME=1 \
    agent-collab-daemon --config reviewer-gemini
  ```

  **Tuning the `--list-sessions` timeout (v0.1.2 fix).** Real
  `gemini --list-sessions` enumeration time scales with the size of the
  session store. v0.1's hardcoded 5s timeout deterministically missed
  on operator-sized configs (E6 probe measured ~5.3s on a typical
  config). v0.1.2 raised the default to **15s** and added an env
  override:

  ```bash
  # Tune --list-sessions timeout — values in seconds. Default 15s.
  AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT=30 \
    AGENT_COLLAB_CLI_SESSION_RESUME=1 \
    agent-collab-daemon --config reviewer-gemini
  ```

  Bump higher for very-large session stores (operator's `~/.gemini/`
  with thousands of sessions); bump lower for CI / fixture runs that
  want fast-fail on enumeration. Invalid values (non-int, ≤0) silently
  fall back to the 15s default — capture-failure is non-fatal per
  §3.4 invariant 5, so a config typo here does not block the daemon.

- **Pi (`--cli pi`, Topic 3 v0.2)**: pi is the first Arch D CLI where
  the **daemon owns the session-file PATH** rather than translating an
  opaque vendor-minted UUID. Pi accepts `--session <PATH>` (a JSONL
  session file the daemon mints, ensures exists, and persists the path
  of). No regex scan, no `--list-sessions` delta, no race tolerance —
  path-as-identity by construction.

  - **Resume-off form:** `pi --provider <P> --model <M> --no-session -p <prompt>` — pi 0.67.68 ships an explicit `--no-session` flag for ephemeral invocations (parallel to Arch B for codex/gemini). Daemon emits this form when `cli_session_resume=false` regardless of any column state.
  - **Resume-on, first batch:** daemon mints `$pi.session_dir/$label.jsonl` (default `$HOME/.agent-collab/pi-sessions/$label.jsonl`), calls `os.MkdirAll` with 0700, persists the path to `sessions.daemon_cli_session_id`, spawns with `--session <PATH>`. Pi creates the JSONL file on first write.
  - **Resume-on, subsequent batches:** daemon reads cached path; spawns with `--session <cached-path>`. Pi appends turns to the same file.
  - **File-missing-at-spawn:** if the cached path's file was deleted out-of-band, the daemon passes the cached path unchanged; pi creates the file fresh at that path on first write. Deterministic-path invariant preserved; no column rewrite.

  **Required config (when `cli=pi`):**

  ```json
  {
    "cli": "pi",
    "cli_session_resume": true,
    "pi": {
      "provider": "zai-glm",
      "model": "glm-4.6",
      "session_dir": "$HOME/.agent-collab/pi-sessions"
    }
  }
  ```

  `pi.provider` + `pi.model` are MANDATORY — missing either → daemon
  startup exits 64 (EX_USAGE) with a clear diagnostic. `pi.session_dir`
  is optional (default shown). CLI flag fallbacks: `--pi-provider`,
  `--pi-model`, `--pi-session-dir`. Env overrides:
  `AGENT_COLLAB_DAEMON_PI_PROVIDER`, `AGENT_COLLAB_DAEMON_PI_MODEL`,
  `AGENT_COLLAB_DAEMON_PI_SESSION_DIR`.

  **Model-provider coupling check (§4.4):** if `pi.model` is
  provider-qualified (`"openai/gpt-4o"`) AND the prefix does NOT match
  `pi.provider`, daemon startup rejects with exit 64 and a diagnostic
  naming both. Prevents silent mis-routes.

  **Provider auth (operator responsibility):** pi-mono routes to the
  provider specified by `--provider`. The daemon inherits the operator
  shell environment via `os.Environ()` — export the provider-specific
  auth env var per pi-mono's published table (run `pi --help` for the
  canonical list on your pi version). Common mappings (confirmed
  against pi 0.67.68):

  | provider            | env var                                      |
  |---------------------|----------------------------------------------|
  | `zai-glm`           | `ZAI_API_KEY`                                |
  | `openai-codex`      | `OPENAI_API_KEY`                             |
  | `anthropic`         | `ANTHROPIC_API_KEY` or `ANTHROPIC_OAUTH_TOKEN` |
  | `google` (Gemini)   | `GEMINI_API_KEY`                             |
  | `google-antigravity`| OAuth (no direct env var); see pi-mono docs |
  | `groq`              | `GROQ_API_KEY`                               |
  | `xai`               | `XAI_API_KEY`                                |
  | `openrouter`        | `OPENROUTER_API_KEY`                         |
  | `mistral`           | `MISTRAL_API_KEY`                            |

  **Reset semantics for pi** (extension of the PRIMARY / SECONDARY /
  TERTIARY ladder below): because the daemon owns the path, pi reset
  also **deletes the session file from disk**. The operator verb + L1
  content-stop auto-GC both gate the file-delete on `sessions.agent ==
  'pi'` — codex/gemini reset stays NULL-only. `os.Remove` is
  `NotExist`-tolerant (idempotent per §3.4 invariant 3). A cross-CLI
  reset-isolation regression gate (`tests/daemon-cli-resume-{codex,gemini}.sh`
  subtest 4e/5h) locks the agent-gate so refactors can't accidentally
  drop it.

  ```bash
  # pi reset → column NULL'd + session file deleted
  peer-inbox daemon-reset-session --cwd <CWD> --as <pi-label> --format plain
  # Output:
  #   daemon-reset-session: cleared daemon_cli_session_id for (...); next spawn will allocate a fresh CLI session
  #   daemon-reset-session: deleted session file: /home/.../pi-sessions/<label>.jsonl
  ```

  With `--format json`, the payload gains a `"deleted_file": "<path>"`
  field (omitted when the agent is not `pi` OR the column was already
  NULL OR the file was already absent).

### Known asymmetry: Claude

`claude -p` (the form `agent-collab-daemon` uses for spawns) does not
expose a stable cross-process session-resume mechanism analogous to
`codex exec resume <UUID>` or `gemini --resume <N>`. `claude --continue`
operates per-process within a single CLI run, not across separate `-p`
invocations.

Setting `--cli-session-resume` on a Claude daemon is **non-fatal** — the
daemon emits a one-time warning at startup and proceeds in Arch B
fresh-invocation mode:

```
Claude has no cross-process session-resume; --cli-session-resume is a no-op for this daemon (see Arch B asymmetry note in operator guide).
```

This ergonomics decision (warn-not-fail) lets operators apply a single
`cli_session_resume: true` field across a config used by multiple
daemons of mixed CLI kinds. Claude daemons silently ignore the flag;
codex + gemini honor it. Scope §4.3 + §3.4 invariant 4.

If `claude -p` gains a stable cross-process session-resume in a future
release, v1+ will extend Arch D to include claude.

### Reset primitives

When the captured CLI session goes wrong (poisoned context, vendor-side
expiry, operator wants a clean slate), three reset paths in order of
preference:

1. **PRIMARY — operator verb (recommended):**

   ```bash
   # Go fast-path
   peer-inbox daemon-reset-session --cwd <CWD> --as <LABEL>

   # Python equivalent
   agent-collab peer receive --reset-session --as <LABEL>
   ```

   Both NULL `sessions.daemon_cli_session_id` for the named label. The
   next daemon spawn allocates a fresh CLI vendor session-ID and
   captures it per the per-CLI flow above. Idempotent — safe to spam
   even when the column is already NULL.

   Exit codes: `0` success; `64` missing/invalid `--as`; `6` no session
   row for `(cwd, label)`.

2. **SECONDARY — auto-GC on L1 content-stop:** when a delivered batch
   contains the L1 content-stop sentinel (see Termination stack §
   below), the daemon clears `daemon_cli_session_id` automatically as
   part of the same `transitionClosed` operation that flips
   `daemon_state='closed'`. Reopening the daemon (operator flips
   `daemon_state` back to `'open'`) gets a fresh CLI session by
   construction.

3. **TERTIARY — SQL escape hatch (always-available recovery):**

   ```sql
   -- Drop the captured CLI session-ID for a daemon-managed label.
   -- Next spawn will allocate a fresh session.
   UPDATE sessions
   SET daemon_cli_session_id = NULL
   WHERE cwd = '/path/to/cwd' AND label = 'reviewer-codex';
   ```

   No new code needed; safe fallback if the Go binary or Python verb is
   unreachable.

**Important: external SQL flip of `daemon_state='closed'` does NOT
auto-GC the captured CLI session-ID.** L2 dormancy and Arch D capture
are distinct concerns: `daemon_state` controls whether the daemon
claims new batches; `daemon_cli_session_id` controls whether the next
claim resumes or starts fresh. To clear both at once, run the operator
reset verb in addition to the SQL flip.

### Trust boundary trade

Architecture D moves cross-spawn untrusted-state carryover from
"impossible by construction" (Arch B) to "operator-acceptable behind an
opt-in flag." The daemon **owns the pointer** (`sessions.daemon_cli_session_id`)
but does **not introspect the contents** (`~/.codex/sessions/<UUID>/*`
or equivalent for gemini are vendor-internal — daemon never reads
them). Per scope §3.4 invariants:

1. The envelope payload is still load-bearing per batch — the daemon
   ALWAYS serializes and passes the full claimed-batch envelope as the
   spawn's prompt. Resume affects argv shape, not prompt content.
   Prevents the "CLI remembers, daemon skips delivery" bug class.
2. Daemon never opens or parses CLI-vendor session-store files.
3. Reset is idempotent (safe-to-spam, no error on already-NULL state).
4. `--cli claude --cli-session-resume` is non-fatal (warn + Arch B).
5. Capture-failure is non-fatal (regex no-match, gemini race, etc. all
   fall through to Arch B for that batch — daemon never blocks).

A richer alternative — daemon-controlled `ContinuitySummary` envelope-
bridging where the daemon manages context content directly — is
deferred to v1+. Arch D is the lighter-weight intermediate that uses
each CLI's own session-management, with the trust trade made explicit.

---

## How it works (architecture)

One page. The daemon is a long-running "worker shell" (W3 shape) that
hosts a loop:

```
loop:
    poll DB for unclaimed rows matching (to_cwd, to_label)
    if daemon_state = 'closed' → sleep; continue
    if rows returned:
        DaemonModeClaim(rows)           # atomic UPDATE ... RETURNING
        envelope = buildEnvelope(rows)   # canonical JSON serializer
        spawn CLI with envelope as prompt + env vars set
        wait for completion-ack (primary) OR ack-timeout (fallback)
        if ack received:
            DaemonModeComplete(claim_owner=self)
        else (timeout):
            log abandonment; do NOT call --complete
            sweeper will revert claimed_at after sweep-ttl
    else:
        apply quiescence primitives (backoff / pause-on-idle)
        sleep(poll-interval)
```

Two invariants make this correct:

**Path-separation at the SQL layer (§3.4 guarantee (a)).** The
interactive receive path in both Python (`scripts/peer-inbox-db.py`)
and Go (`go/pkg/store/sqlite/sqlite.go`) WHERE clauses includes `AND
claimed_at IS NULL`. Daemon-claimed rows are invisible to any
interactive receiver. This holds by construction, even if the daemon
forgets to set the hook short-circuit env flag — it covers the Go
hook hot-path (the production-default fast path for all three CLIs).

**Hook short-circuit for daemon spawns (§3.4 optimization (f)).** The
daemon exports `AGENT_COLLAB_DAEMON_SPAWN=1` into every spawned CLI's
environment. Both the bash hook (`hooks/peer-inbox-inject.sh`) and
the Go hook (`go/cmd/hook/main.go`) check this env flag at entry and
exit 0 with no output if set. This is a performance optimization
(skip the hook's DB round-trip), not a correctness guarantee —
(a) above is the correctness belt; (f) is the suspenders.

**Why daemon doesn't use the hook for delivery.** Option J's hook path
(`peer receive --format hook-json --mark-read`) consumes rows via the
interactive verb. If the daemon let its spawned CLIs use that path,
the interactive verb would see daemon-claimed rows (still `read_at IS
NULL` under old SQL) and consume them — collapsing the recovery
semantics of `claimed_at` / `completed_at`. Instead, the daemon
serializes the envelope itself (via the same canonical JSON
serializer that backs the hook's output — scope doc §5.2 two-consumer
byte-parity) and pipes it into the spawned CLI as the user prompt.

**The five-guarantee contract (scope doc §3.4).** Summarized here for
operator orientation; read the scope doc for the full rationale:

| # | Guarantee | Enforced where |
|---|---|---|
| (a) | SQL partition: interactive reads skip daemon-claimed rows | Migration 0002 + runtime WHERE clauses |
| (b) | `receive_mode` verb-entry gate | Python `cmd_peer_receive`; Go verb dispatch |
| (c) | TTL ordering invariant (`sweep-ttl > ack-timeout`) | Daemon startup validator |
| (d) | Claim-identity fail-loud on `--complete` | `DaemonModeComplete` SQL predicate |
| (e) | Closed-state preflight at claim-time | `DaemonModeClaim` reads `sessions.daemon_state` |
| (f) | Hook short-circuit (optimization, not correctness) | `AGENT_COLLAB_DAEMON_SPAWN=1` in hook entry |

---

## Termination stack

Four layers, each with distinct semantics. Wind a daemon down using
the shallowest layer that accomplishes your goal.

### Layer 1 — Content-stop sentinel

A peer sends a message whose body contains the **double-bracket
end-of-room sentinel token** (the token form is documented in
`docs/PEER-INBOX-GUIDE.md`; see the "Explicit termination" subsection
under [Channels](./PEER-INBOX-GUIDE.md#channels-real-time-push-opt-in)).
The daemon recognizes the sentinel in the incoming batch, completes
the current turn normally (ack the batch), then transitions the
managed label to a "drain then dormant" state — no new claims until
state clears externally.

This layer is the **peer-originated signal for "this exchange is
done"**. Semantically distinct from Layer 2 ("daemon, go dormant"):
Layer 1 says the conversation is over; Layer 2 says the daemon should
stop working.

### Layer 2 — `sessions.daemon_state = 'closed'`

To put a daemon dormant, flip its sessions-table row to
`daemon_state='closed'`. Operators can do this directly against
SQLite:

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "UPDATE sessions SET daemon_state='closed' \
     WHERE cwd='/path/to/workspace' AND label='reviewer-codex'"
```

On the next claim cycle, the daemon's `DaemonModeClaim` preflights
the sessions row, sees `daemon_state='closed'`, and returns zero rows
without claiming anything (§3.4 guarantee (e)). Sweeper-requeued rows
cannot trigger a respawn on a closed daemon — this is the fix for the
sweeper-re-queue-then-respawn footgun.

Also, **on receive of an `envelope.state: closed` message**, the
daemon itself sets `daemon_state='closed'` for the managed label and
unclaims any in-flight pre-spawn batch (`UPDATE inbox SET claimed_at
= NULL WHERE claim_owner = ? AND completed_at IS NULL`). This is the
"mediator tells the daemon to stop" pathway.

To **re-open**: flip `daemon_state` back to `'open'`. A daemon
restart is always available as a fallback. A dedicated `peer receive
--reopen --as <label>` verb is deferred to v0+ per scope doc
§10 item 9.

Observability: `daemon_state` is visible via `peer list` (read from
the sessions table).

### Layer 3 — Quiescence primitives

Three independent runtime checks, all on by default, all
configurable. Their purpose is to prevent runaway cost and to
gracefully pause during quiet periods. None of them terminate the
daemon; they back off:

- **Exponential backoff on rapid same-pair turns.** If the daemon
  sees > 3 turns with the same peer inside 60 seconds, it lengthens
  its poll interval. Counter resets on quiet period.
- **Empty-response terminates.** If the spawned LLM produces a
  whitespace-only response, the daemon does not re-spawn for that
  batch. Treated as an implicit "I have nothing to add."
- **Pause-on-idle timer.** After `--pause-on-idle` seconds of
  inactivity, the daemon reduces poll frequency and logs a pause
  event. New inbound messages wake it on the next reduced-cadence
  poll.

### Layer 4 — Heartbeat / liveness

**Deferred to the v3.1 papercut at commit `712121c`** (scope doc §6
Layer 4). Topic 3 v0 depends on heartbeat landing as a pre-req so
`peer list` accurately distinguishes "daemon alive" from "daemon
crashed mid-batch." Until heartbeat ships, `peer list` reflects
`bump_last_seen` (which fires on receive regardless of whether the
spawn downstream actually delivered content). See
[Troubleshooting](#troubleshooting) for the "daemon appears alive but
not replying" diagnostic.

---

## Completion-ack contract

When the daemon spawns a CLI with an envelope, it needs a signal
that the spawn is finished so it can call `DaemonModeComplete` and
move on. Two mechanisms ship in v0 (scope doc §7.2), ordered by
preference:

### Primary — agent calls `peer receive --complete`

The daemon's prompt template instructs the spawned agent to call
`agent-collab peer receive --complete --as <label>` as its **final
action** (via Bash tool, shell escape, or equivalent). This verb
issues a targeted `UPDATE inbox SET completed_at = ?` matching on
`claim_owner = ? AND claimed_at IS NOT NULL AND completed_at IS
NULL`. No batch-id parameter needed — v0 enforces single-batch-
at-a-time per daemon, so `claim_owner` alone correlates the ack
(scope doc §7.3).

**Pros:** no stdout parsing; agent is the authoritative source for
completion; aligns with the auto-reply-driver pattern already used
elsewhere. **Cons:** the agent prompt template must stay static and
scoped — peer content is never interpolated into the instruction
text (§7.1 trusted-template constraint, and §8 security surface
below).

Fail-loud semantics (§3.4 guarantee (d)): if the sweeper reaped the
claim mid-spawn (clock skew, pathological GC pause), the `--complete`
UPDATE matches 0 rows and exits non-zero. The daemon logs the
rejection; the reaped rows are re-delivered on the next claim cycle.
The at-most-once-processing invariant holds even when the TTL
ordering invariant fails.

### Fallback — JSONL stdout marker

For environments where tool-call permission isn't available to the
agent (CLI invoked without Bash tool, permission-revoked session,
etc.), the daemon also scans the spawned CLI's stdout for a
JSONL-formatted ack marker:

```
{"peer_inbox_ack": true}
```

(Optionally with metadata: `{"peer_inbox_ack": true, "notes": "..."}`.)

**The marker form is structural, not stylistic.** Previous drafts
considered HTML-comment form (`<!-- peer-inbox-ack -->`) or raw
XML-like tags; both were rejected (scope doc gamma #3, §7.2):

- HTML comments get stripped or reformatted by agent prose.
- Raw `<peer-inbox-ack>` tags false-positive against agent
  discussions of the peer-inbox protocol itself (agents talking
  *about* peer-inbox routinely emit `<peer-inbox>` strings in their
  natural responses).

The JSONL marker is the least-collision-prone convention. The daemon
parses stdout line-by-line, JSONL-decodes each line, and accepts any
line with `peer_inbox_ack: true` as ack. Regression test
(`tests/daemon-completion-ack.sh`) asserts no false-positive against
agent fixtures that discuss peer-inbox.

### Mixed-mode behavior

If both signals arrive (agent calls `--complete` AND emits the JSONL
marker), the daemon treats the second signal as a benign no-op —
`--complete`'s SQL is idempotent on already-completed claims (it
matches against `claim_owner + claimed_at IS NOT NULL`, not first-
writer of `completed_at`). Whichever arrives first wins; the other
becomes a harmless echo.

If **neither** arrives within `--ack-timeout`, the daemon abandons
the batch internally and does not call `--complete`. The sweeper
reverts `claimed_at` after `--sweep-ttl`, and the next daemon claim
picks up the same rows.

---

## Troubleshooting

### Daemon started but nothing happens on `peer send`

Check the receive mode on the sessions row:

```bash
sqlite3 ~/.agent-collab/sessions.db \
    "SELECT label, receive_mode, daemon_state, session_key \
     FROM sessions WHERE label='reviewer-codex'"
```

Expected: `receive_mode='daemon'`, `daemon_state='open'`,
`session_key` matches the value you passed to the daemon. Common
fixes:

- If `receive_mode='interactive'`: the session was registered before
  daemon mode was requested. Re-register with `--receive-mode
  daemon --force`, or `UPDATE` the row directly.
- If `daemon_state='closed'`: someone flipped the state (either an
  earlier `envelope.state: closed` message, or a direct UPDATE).
  Flip it back to `'open'` or restart the daemon.
- If `session_key` doesn't match: the daemon's spawned CLI resolves
  to a different row; see the "Existing registration's session_key
  doesn't match" section in `docs/PEER-INBOX-GUIDE.md`.

Also tail the daemon log:

```bash
tail -f ~/.agent-collab/daemon-reviewer-codex.log
```

### Messages keep re-delivering / double-processing

Symptom: same message body keeps showing up in the daemon log across
multiple spawns. Cause candidates:

- **TTL ordering violated** — daemon somehow started with
  `sweep-ttl <= ack-timeout`. Validator should have rejected at
  startup; if a stale daemon is running, restart with corrected
  flags.
- **Stale claim left behind** — check:

  ```bash
  sqlite3 ~/.agent-collab/sessions.db \
      "SELECT id, from_label, claim_owner, claimed_at, completed_at \
       FROM inbox \
       WHERE claim_owner='reviewer-codex' AND completed_at IS NULL"
  ```

  Rows with `claimed_at IS NOT NULL AND completed_at IS NULL` older
  than `sweep-ttl` indicate the sweeper isn't running. Run
  `agent-collab peer-inbox sweep` manually; verify the external
  sweeper timer is installed.

- **Agent calls `--complete` but it fails silently** — check the
  daemon log for "claim no longer held" errors. If present, TTL
  ordering is too tight for the real spawn latency; raise
  `--ack-timeout` (and `--sweep-ttl` to keep the 2× ratio).

### Claude spawn no longer sees peer-inbox content after CLI upgrade

Symptom: daemon spawns `claude -p`, Claude says "I don't see any
peer-inbox messages." Daemon log shows the spawn exited cleanly with
no ack.

Almost certainly: `claude -p` rolled forward to a version where
`--bare` became the default, and the daemon's Claude-spawn argv
doesn't include the explicit `--settings` opt-in. Under `--bare`,
hooks + MCP + CLAUDE.md are all skipped — peer-inbox injection
silently stops firing. **The daemon passes `--settings` unconditionally
to guard against this**, and the CI test
`tests/daemon-harness.sh` pins the Claude-spawn argv as a fixture so
any drift fails loudly.

If you hit this anyway: check the daemon's actual spawn argv in its
log. If `--settings` isn't there, the daemon build is stale — rebuild
from a current commit.

### Codex spawn hangs

Symptom: daemon shows `codex exec` started but never exits; eventually
hits `--ack-timeout` and the sweeper reaps.

Almost certainly: stdin wasn't closed. The daemon passes `</dev/null`
(or its Go `exec.Cmd.Stdin = nil` equivalent) for all Codex spawns.
If you're running a custom wrapper around the daemon, ensure the
wrapper doesn't reopen stdin. Test outside the daemon:

```bash
# Should exit cleanly within a few seconds:
codex exec "hello" < /dev/null
```

If that hangs, Codex itself is upgraded to a version with a
different stdin semantic — file an issue upstream and pin your
Codex CLI version.

### Daemon appears alive in `peer list` but not replying

Symptom: `peer list` shows the daemon label as `active`, but no
spawns appear in the daemon log for recent messages.

Background: `bump_last_seen` (which drives `peer list` activity
state) fires on receive path regardless of whether the downstream
spawn state is healthy. This is the liveness-ambiguity gap the
heartbeat papercut at `712121c` closes.

**Until heartbeat ships**, diagnose via daemon log instead of
`peer list`:

```bash
tail -50 ~/.agent-collab/daemon-reviewer-codex.log
```

Look for recent `spawn start` / `spawn exit` lines. If the log shows
no spawn activity for 5+ minutes but `peer list` shows `active`, the
daemon process may be stuck in a DB-read retry loop. Check for stale
SQLite locks via `lsof ~/.agent-collab/sessions.db`. Restart the
daemon as a last resort.

### Daemon crashed mid-spawn; will the batch be lost?

No. The spawned CLI and the daemon are separate processes; the
daemon dying mid-spawn does not kill the CLI. When the daemon
restarts, its `DaemonModeClaim` reads unclaimed rows and ignores any
rows still claimed-but-not-completed. After `--sweep-ttl` elapses,
the sweeper reverts `claimed_at` on those rows, and the next
`DaemonModeClaim` picks them up. The original spawned CLI's output
is orphaned (it'll call `--complete` against a stale claim and get a
fail-loud error — that's the at-most-once-processing invariant at
guarantee (d) working as designed).

Probe E3 in [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) exercises
this crash-recovery path.

---

## Security surface

Peer-inbox content is **untrusted-by-construction**. Any peer (or
anyone with write access to the SQLite file) can insert arbitrary
text into the inbox; the daemon's spawned CLIs read that text as
input. Two mitigations ship in v0:

1. **Static prompt template.** The daemon's prompt template
   (the text wrapped around the envelope before the spawn) is
   hard-coded in the daemon binary. Peer content is never
   interpolated into the template — it's concatenated as a clearly-
   labeled external payload block (`<peer-inbox>...</peer-inbox>`).
   The spawned agent reads peer content as external data, not as
   instruction.

2. **Envelope content_stop and state fields as first-class.** Stop
   signals live in envelope metadata fields, not in body-string
   conventions parsed by the agent. The agent does not need to
   "understand" stop semantics; the daemon handles them at the
   envelope layer.

**What the v0 design does not defend against**: an adversarial peer
sending a message whose body says "ignore your instructions and do
X." The spawned agent may or may not comply — that's a prompt-
injection surface inherent to every LLM-driven agent. Operators
should gate peer-inbox write access (by machine user, by filesystem
permission on `~/.agent-collab/sessions.db`, or by higher-level
identity) if trust across peers is a concern.

Scope doc §7.1 item 3 ("peer content as untrusted payload")
documents the full threat model.

---

## Cost model

Daemon cost scales with message rate. Per-batch cost is approximately:

```
cost = (envelope tokens) × (inbound + outbound token rate) × (CLI token price)
```

Where `envelope tokens` is the serialized `<peer-inbox>` block plus
the daemon's static prompt template (roughly 500-2,000 tokens
depending on batch size — each message is up to 8 KB of body text).
The spawned CLI's own reasoning / reply token usage is the dominant
variable cost.

Cost-reducing levers the daemon provides:

- **`--pause-on-idle`**: during quiet periods, daemon doesn't poll
  as aggressively. Default 30min-of-silence threshold, user-
  overridable.
- **`--ack-timeout`**: bounds runaway cost per hung spawn. A batch
  that loops or stalls can't burn more than `ack-timeout`'s worth of
  CLI runtime before the daemon abandons it and the sweeper reaps.
- **Empty-response terminates** (Layer 3 quiescence): when the
  spawned agent has nothing to add, it can emit an empty response
  and the daemon won't re-spawn for that batch.

**What the v0 design does not provide**: an authoritative cost model
backed by provider API usage data. The rough multiplier above is
useful for capacity-planning heuristics, not for billing reconciliation.
A richer cost model tied to provider API usage is anticipated for
Topic 2 (API-backed workers) — this guide will forward-reference it
when Topic 2 ships.

---

## References

- `plans/v3.x-topic-3-implementation-scope.md` — ratified source of
  truth for daemon behavior (§2.2 binary shape, §3.4 five-guarantee
  contract, §4 harness requirements, §6 termination stack, §7.2
  completion-ack).
- `plans/v3.x-topic-3-arch-d-scoping.md` — Arch D (v0.1) scope-doc:
  CLI-native session-ID pass-through, opt-in flag, per-CLI capture +
  resume strategies, claude asymmetry, reset primitives.
- [PEER-INBOX-GUIDE.md](./PEER-INBOX-GUIDE.md) — per-CLI hook-parity
  baseline; auto-reply daemon section.
- [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) — owner-supervised
  E2E probe protocol for daemon-mode shipping (E1-E4).
- [DAEMON-CLI-SESSION-VALIDATION.md](./DAEMON-CLI-SESSION-VALIDATION.md)
  — Arch D operator probe protocol (E5-E7) for codex banner drift +
  gemini --list-sessions serialization + CLI-version drift detection.
- [HOOK-PARITY-VALIDATION.md](./HOOK-PARITY-VALIDATION.md) — Option J
  E2E probe protocol (template for the daemon probes).
- `tests/daemon-harness.sh`, `tests/daemon-termination.sh`,
  `tests/daemon-completion-ack.sh` — shape-2 CI gates (persistent).
- `tests/daemon-cli-resume-codex.sh`, `tests/daemon-cli-resume-gemini.sh`,
  `tests/daemon-auto-gc-on-content-stop.sh` — Arch D shape-2 CI gates.
