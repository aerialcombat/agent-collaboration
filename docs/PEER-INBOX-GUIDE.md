# Peer Inbox — User Guide

Cross-session messaging for Claude Code, Codex CLI, Gemini CLI, and
pi (pi-coding-agent). Two (or more) agent sessions on the same
machine exchange labeled messages; each answers from its own live
context so fidelity stays high and there's no manual copy-paste.

> **Continuing-session model.** As of v2.0+, **pi is the preferred
> continuing-session peer** alongside channels-enabled Claude. codex-cli
> and gemini-cli are not used for continuing sessions — they lack a
> mid-turn push surface (codex #18056, gemini #3052), so the daemon
> path spawns a fresh `codex exec` / `gemini -p` per batch with no
> live context continuity. pi, by contrast, stays running as a single
> process and receives peer messages as follow-up user turns via the
> native extension (`extensions/peer-inbox-pi.ts`). For anything
> resembling a long-lived conversation with a non-Claude agent, join a
> pi session into the room.

**Current version:** v1.5
**Architecture details:** [ARCHITECTURE.md](./PEER-INBOX-ARCHITECTURE.md)
**Version history:** [CHANGELOG.md](../CHANGELOG.md)

---

## Table of contents

1. [TL;DR](#tldr)
2. [Install](#install)
3. [Core concepts](#core-concepts)
4. [Subcommand reference](#subcommand-reference)
   - [Sessions](#sessions-register-close-list)
   - [Messaging](#messaging-send-receive-list)
   - [Observability](#observability-watch-web-replay)
   - [Flow control](#flow-control-reset)
5. [Delivery modes](#delivery-modes)
   - [Hook (default)](#hook-default)
   - [Channels (real-time push, opt-in)](#channels-real-time-push-opt-in)
   - [Auto-reply daemons](#auto-reply-daemons)
6. [Per-runtime cheatsheet](#per-runtime-cheatsheet)
7. [Examples](#examples)
8. [Troubleshooting](#troubleshooting)
9. [Configuration reference](#configuration-reference)
10. [Limits and non-goals](#limits-and-non-goals)

---

## TL;DR

```bash
# Terminal 1:
agent-collab session register --label backend --agent claude --role lead
agent-collab peer send --to frontend --message "auth token TTL is 15m now"

# Terminal 2:
agent-collab session register --label frontend --agent claude --role peer
# Messages from backend auto-inject into your context on every turn (Claude only).
# For Codex/Gemini, or to check without a prompt:
agent-collab peer receive --mark-read
```

For self-sustaining conversations (recipient wakes without a user prompt),
see [Channels](#channels-real-time-push-opt-in).

---

## Install

One-time, per machine:

```bash
cd ~/Development/agent-collaboration
./scripts/install-global-protocol
./scripts/doctor-global-protocol
```

Expected doctor output: `Summary: N PASS, 0 WARN, 0 BLOCK` (CLI-version
drift warnings are harmless).

The installer places:

| Path | Purpose |
|---|---|
| `~/.local/bin/agent-collab` | Main CLI (added to PATH via `~/.local/bin`) |
| `~/.agent-collab/scripts/peer-inbox-db.py` | Python helper — all SQLite work |
| `~/.agent-collab/scripts/peer-inbox-channel.py` | MCP channel server (v1.4+, opt-in) |
| `~/.agent-collab/hooks/peer-inbox-inject.sh` | `UserPromptSubmit` hook |
| `~/.claude/settings.json` | Gets a managed `UserPromptSubmit` hook entry (existing hooks preserved) |
| `~/.agent-collab/sessions.db` | SQLite store (created on first `session register`) |

Uninstall reverses all of it cleanly:

```bash
./scripts/install-global-protocol uninstall
# ~/.agent-collab/sessions.db is intentionally NOT deleted — user data.
```

---

## Core concepts

**Session.** One running `claude` / `codex` / `gemini` / `pi` process.
Each registers with a short human label. `pi` sessions join via the
bundled extension (`/peer-join`) or env-driven auto-join
(`PEER_INBOX_LABEL`, `PEER_INBOX_PAIR_KEY`) — no separate
`agent-collab session register` step needed.

**Label.** How a session is addressed by peers. Pattern
`[a-z0-9][a-z0-9_-]{0,63}`. You pick it; it's scoped to your canonical
repo root (so `backend` in `dj/` is distinct from `backend` elsewhere).

**Canonical cwd.** The session's repo root, symlinks resolved. Walk-parents
discovery means you can invoke subcommands from any subdirectory.

**Session key.** Per-session discriminator used to tell same-cwd
sessions apart. Claude's hook provides it automatically (from the
session's stdin JSON); Codex/Gemini users set
`AGENT_COLLAB_SESSION_KEY` explicitly.

**Inbox.** Single SQLite file at `~/.agent-collab/sessions.db`.
Two tables matter to users:
- `sessions` — one row per registered session: `(cwd, label, agent, role, session_key, channel_socket, last_seen_at)`.
- `inbox` — every message, append-only: `(to_cwd, to_label, from_cwd, from_label, body, created_at, read_at)`.

**Delivery.** Messages always land in SQLite; how they reach the
*recipient's live context* depends on delivery mode. See
[Delivery modes](#delivery-modes).

---

## Subcommand reference

All commands are `agent-collab <group> <verb> [options]`. Groups:
`session`, `peer`. Run `agent-collab help` for the full summary.

### Sessions: register / close / list

#### `agent-collab session register`

```
--label <l>          required; [a-z0-9][a-z0-9_-]{0,63}
--agent <a>          required; claude | codex | gemini
--role <r>           optional; free-form (lead, peer, reviewer, …)
--cwd <path>         defaults to resolved canonical $PWD
--session-key <k>    optional; else env: AGENT_COLLAB_SESSION_KEY,
                     CLAUDE_SESSION_ID, CODEX_SESSION_ID, GEMINI_SESSION_ID;
                     else picked from the hook-logged seen-sessions
                     in this cwd
--force              override active-label collision
```

Creates a `sessions` row, writes a per-session marker at
`<cwd>/.agent-collab/sessions/<sha256-of-key>.json`, and (when a channel
is live in the ancestor process tree) binds the channel socket.

Output includes `[channel: paired]` or `[channel: none]`.

#### `agent-collab session close`

```
--label <l>          optional; inferred from marker via walk-parents
--session-key <k>    optional
```

Removes the session row and its marker. Inbox messages are preserved
for audit.

#### `agent-collab session list`

```
--all-cwds           show sessions in every cwd (default: this cwd only)
--include-stale      include sessions not seen in > 6 hr
--json               machine-readable output
```

Activity states: `active` (<5 min), `idle` (5 min – 6 hr), `stale` (excluded
by default).

### Messaging: send / receive / list

#### `agent-collab peer send`

```
--to <label>                 required; recipient's label
--as <label>                 optional; override self-resolution
--to-cwd <path>              optional; for cross-cwd sends
--message <text> |
  --message-file <path> |    one-of required
  --message-stdin
```

Rules enforced:
- Body cap 8 KB.
- Recipient inactive >6 hr → error `peer offline: <label>`.
- Pair `[[end]]`-terminated → rejected (see [reset](#agent-collab-peer-reset)).
- Pair turn count `>= AGENT_COLLAB_MAX_PAIR_TURNS` (default 500) → rejected.
- Body containing `[[end]]` (case-insensitive) marks pair terminated.

Output prefixed with push status: `(pushed)`, `(no-channel)`, or
`(push-failed(<code>))`.

#### `agent-collab peer receive`

```
--as <label>               optional; override self-resolution
--format hook|hook-json|plain|json     default: plain
--mark-read                atomic claim-and-mark (UPDATE … RETURNING)
--since <iso8601>          only messages newer than this timestamp
```

Without `--mark-read`: read-only, repeatable. Does not bump `last_seen_at`.
With `--mark-read`: atomic claim under `BEGIN IMMEDIATE` — no race
between concurrent receivers.

Format notes:
- `plain` — human-readable text.
- `json` — array of message objects.
- `hook` — just the `<peer-inbox>…</peer-inbox>` block (for embedding).
- `hook-json` — full Claude `UserPromptSubmit` hook envelope:
  `{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit",
  "additionalContext": "<peer-inbox>…</peer-inbox>"}}`.

#### `agent-collab peer list`

```
--as <label>          optional
--include-stale       include peers not seen in > 6 hr
--json                machine-readable output
```

Other sessions in the caller's cwd (self excluded).

### Observability: watch / web / replay

#### `agent-collab peer watch`

```
--as <label>          optional
--interval <sec>      default 1.0
--only-new            skip history; print only messages arriving after launch
--since-id <n>        start from inbox id
```

Blocks until Ctrl-C. Prints each incoming message with sender label,
timestamp, read-state marker (`*` = unread). Read-only; does not
mark-read. Good for a tmux side-pane.

#### `agent-collab peer web`

```
--as <label>          optional; narrow to label's conversations
--port <n>            default 8787
```

Blocks until Ctrl-C. Serves `http://127.0.0.1:<port>` — Slack-shaped
two-pane UI:

- **Left sidebar**: every pair (canonical `a < b`) grouped by activity
  (active / idle / stale / terminated). Unread badge on pairs that
  aren't currently selected. Click to switch.
- **Main pane**: selected pair's message stream. Pastel-pill sender
  labels, day separators, `[[end]]` renders with a warning border plus
  a "terminated" banner at the bottom.
- **Header**: peer details (agent type, role, channel pairing), turn
  count, activity dot.
- **Title flash** with total unread across non-focused pairs.
- `#pair=a+b` URL hash preselects a pair on load.

Endpoints (for scripting / dashboards):
- `GET /pairs.json` — every pair's metadata in this cwd.
- `GET /messages.json?a=backend&b=frontend&after=<id>` — messages for
  one pair, with optional delta cursor.
- `GET /messages.json?after=<id>` — all messages (back-compat with v1.5).

#### `agent-collab peer replay`

```
--as <label>          optional; narrow to label's conversations
--since <iso8601>     only messages newer than this
--out <path>          default: <cwd>/.agent-collab/replay-<ts>.html
```

Emits a **self-contained** HTML file — inline CSS, no external assets,
opens in any browser. Deterministic pastel color per sender, day
grouping, html-escaped bodies. Good for post-mortems or archiving a
specific handoff.

### Flow control: reset

#### `agent-collab peer reset`

```
--to <label>          required; the other side of the pair
--as <label>          optional
```

Clears both the turn counter and the `[[end]]` termination flag for the
pair `(self, other)` in the current cwd. Use after hitting the cap or
after a conversation was intentionally ended.

### Other helpers

| Command | Purpose |
|---|---|
| `agent-collab session adopt --label <l> --session-id <uuid>` | Re-key an existing registration to a specific Claude session_id. Use when the hook can't resolve self-identity because register happened before the hook logged the real session_id. |
| `agent-collab hook log-session --cwd <c> --session-id <id>` | Internal: called by the `UserPromptSubmit` hook on every turn to record the seen Claude session_id. Don't invoke manually. |

---

## Delivery modes

Every `peer send` always writes to SQLite (the source of truth). How
the message reaches the recipient's *live agent context* depends on
which delivery mode is active for the recipient.

### Hook (default)

On every `UserPromptSubmit` event, Claude's installed hook runs
`peer receive --format hook-json --mark-read` and prepends unread
messages to the agent's next turn as a `<peer-inbox>…</peer-inbox>`
block.

Characteristics:
- Requires no launch-flag change; works out of the box after install.
- Latency: **next user prompt in the recipient's session.**
- Doesn't self-sustain — each round-trip needs a human prompt in
  both sessions.
- Codex and Gemini have no `UserPromptSubmit` equivalent yet; use
  manual `peer receive` in those runtimes.

### Channels (real-time push, opt-in)

Uses Claude Code's [Channels](https://code.claude.com/docs/en/channels)
research-preview feature. Each session spawns a channel MCP subprocess
that listens on a Unix socket; `peer send` POSTs the message and
Claude sees it as `<channel source="peer-inbox" from="…">…</channel>`
in its running context **without a user prompt**.

#### Enable channels for a repo

1. Ensure `.mcp.json` in the repo root contains the peer-inbox entry:

   ```json
   {
     "mcpServers": {
       "peer-inbox": {
         "command": "python3",
         "args": ["/Users/<you>/.agent-collab/scripts/peer-inbox-channel.py"]
       }
     }
   }
   ```

2. Launch each session with the channels flag:

   ```bash
   cd ~/Development/dj
   claude --dangerously-load-development-channels server:peer-inbox
   ```

3. Register normally. Look for `[channel: paired]` in the output.
   `register` walks its process tree to find Claude's pending-channel
   file and binds the socket path.

4. Send as usual. Status `(pushed)` means the recipient's channel
   received it.

#### Characteristics

- Real-time push; no turn-boundary wait.
- Self-sustaining — two agents can converse without human prompts.
- Requires Claude Code 2.1.78+ with the research-preview flag.
- `channelsEnabled: true` is required on Team/Enterprise plans.
- Sessions not on channels keep working — their sends fall back to
  the hook path.

#### Guardrails shipped by default

- **Max turns per pair.** Default 500 (`AGENT_COLLAB_MAX_PAIR_TURNS`).
  Further sends return an error with reset hint.
- **Explicit termination.** Any `peer send` whose body contains
  `[[end]]` (case-insensitive) marks the pair as terminated; subsequent
  sends in either direction are blocked until `peer reset`.
- `peer_pairs.terminated_by` records who ended the exchange.

### Auto-reply daemons

Topic 3 v0 adds an **auto-reply daemon** (`agent-collab-daemon`,
Go binary at `go/cmd/daemon/`) that gives Codex CLI and Gemini CLI
peer-inbox auto-reply parity with Claude Code. A daemon is an
OS-local process that watches the inbox for messages addressed to a
specific label and autonomously services them by spawning a fresh
`codex exec` / `gemini -p` / `claude -p` invocation per batch,
injecting the envelope as the spawn's user prompt, and waiting for a
completion-ack signal before moving to the next batch.

**When to use a daemon.** You have an interactive Codex or Gemini
session (or even a Claude session) and you want it to keep
autonomously replying to peer-inbox messages between your human
prompts — without you having to type anything in that session's
terminal. The daemon enables "send a message to Codex, walk away,
come back to see the reply" workflows that previously required a
Claude-Channels-paired session.

**When NOT to use a daemon.**
- Your Claude Code session already has the hook installed —
  messages auto-inject on every `UserPromptSubmit`. No daemon needed
  for turn-boundary delivery.
- Your Claude Code session is launched with Channels — messages
  push in real-time mid-turn. Daemon is turn-boundary-driven (fresh
  LLM per batch, W3 worker shape) and doesn't improve on this for
  Claude.
- Cross-host coordination. The daemon is single-machine-local;
  cross-host sync is deferred to v3.3+.

**Full operator guide:** [DAEMON-OPERATOR-GUIDE.md](./DAEMON-OPERATOR-GUIDE.md).
Covers config flags, architecture, the four-layer termination
stack, the completion-ack contract, troubleshooting, and the
security + cost model. **Topic 3 v0.1 adds Architecture D** (opt-in
CLI-native session-ID pass-through for cross-spawn context continuity
on codex + gemini; claude asymmetry documented) — see the operator
guide's Arch D section.

**Validation protocols:** [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md)
covers v0 E2E probes (E1-E4) for live-CLI delivery;
[DAEMON-CLI-SESSION-VALIDATION.md](./DAEMON-CLI-SESSION-VALIDATION.md)
covers v0.1 Arch D probes (E5-E7) for codex banner regex + gemini
`--list-sessions` parser drift detection. Both run at ship time to
complement the shape-2 CI gates at `tests/daemon-*.sh`.

---

## Per-runtime cheatsheet

| Need to… | Claude Code | pi (pi-coding-agent) | Codex CLI | Gemini CLI |
|---|---|---|---|---|
| Register | bare `session register` (hook provides session key) | `/peer-join <label>` in TUI, or `PEER_INBOX_LABEL` env for headless — extension does the DB register itself | bare `session register` (hook provides session key) | bare `session register` (hook provides session key) |
| Auto-inject on turn start | Yes, via `UserPromptSubmit` hook | Yes — extension wraps inbound pushes as `<peer-inbox>` follow-up user turns | Yes, via `UserPromptSubmit` hook | Yes, via `BeforeAgent` hook |
| Real-time push (self-sustain) | `--dangerously-load-development-channels server:peer-inbox` | Yes — native extension opens a per-session Unix socket, `sessions.channel_socket` is bound automatically | Not yet (see [Codex issue #18056](https://github.com/openai/codex/issues/18056) for upstream MCP notifications tracking) | Not supported (architectural; see Gemini issue #3052) |
| **Continuing-session peer** (long-lived conversation, preserved context) | **Yes** (channels mode) | **Yes — preferred for non-Claude** (single long-running process) | **No** (daemon spawns fresh `codex exec` per batch) | **No** (daemon spawns fresh `gemini -p` per batch) |
| Auto-reply via daemon | Yes (`agent-collab-daemon --cli claude`; usually unnecessary given hook + Channels coverage) | Yes (`agent-collab-daemon --cli pi`) — current canonical daemon CLI | Yes (`agent-collab-daemon --cli codex`) — fresh-LLM-per-batch | Yes (`agent-collab-daemon --cli gemini`) — fresh-LLM-per-batch |
| Cross-spawn context (Arch D opt-in) | No — `claude -p` has no stable cross-process session-resume | Yes — daemon mints `$pi.session_dir/$label.jsonl`, passed via `--session`; `pi reset` clears the file | Yes — `codex exec resume <UUID>` | Yes — `gemini --resume <index>` (UUID stored, translated to current index at resume time) |
| Peer send | identical | identical (LLM calls `peer_send` tool; operator can also `/peer-send`) | identical | identical |

Installing the hook on all three CLIs is a single `scripts/install-global-protocol` run — it detects which CLI homes exist (`~/.claude`, `~/.codex`, `~/.gemini`) and registers the unified `hooks/peer-inbox-inject.sh` into each. Codex additionally gets `[features] codex_hooks = true` appended to `~/.codex/config.toml` (required by Codex for hooks to fire). The same installer copies `extensions/peer-inbox-pi.ts` into `~/.pi/agent/extensions/` so pi picks it up on next launch.

---

## pi as continuing-session peer

When you want a non-Claude agent to hold a conversation across many
rounds without losing context, use **pi**. The bundled extension
(`extensions/peer-inbox-pi.ts`, installed to `~/.pi/agent/extensions/`)
makes pi a first-class peer-inbox member.

### Operator commands (interactive TUI)

| Slash command | Effect |
|---|---|
| `/peer-join <label> [pair-key]` | Register this pi session under `<label>`, join a `pair_key` room if given, open a Unix socket listener, bind `sessions.channel_socket` |
| `/peer-leave` | Close listener, clear channel_socket, remove registration |
| `/peer-status` | Show current label / pair_key / socket path |
| `/peer-send <label> <msg>` | DM another peer |
| `/peer-broadcast <msg>` | Fan-out to the whole room |

### LLM tools (pi's model can call these directly)

The extension also registers `peer_join`, `peer_leave`, `peer_status`,
`peer_send`, and `peer_broadcast` as pi tools, so the model itself
can address peers without operator intervention. Tool guidelines tell
it to prefer `peer_send(to, message)` over shelling out to bash.

### Delivery

Inbound messages POST over HTTP/1.1 to pi's Unix socket (`/tmp/peer-inbox-pi-<pid>-<label>.sock`). The extension wraps them in
`<peer-inbox from="..." meta-key="..." ...>body</peer-inbox>` and calls
`pi.sendUserMessage(envelope, { deliverAs: "followUp" })` — pi sees the
message as a follow-up user turn and replies in-context. On
`turn_end`, the extension auto-relays pi's first text block back to
the original sender via `peer-send`, so round-trips happen without
operator action.

### Headless auto-join

Set env vars before launching pi (headless modes like `--mode rpc` /
`--mode json`, or any launch):

```bash
PEER_INBOX_LABEL=pi-peer PEER_INBOX_PAIR_KEY=<room-key> pi ...
```

The extension's `session_start` handler auto-runs `join(...)` with
these values — no `/peer-join` command required.

### Relationship to the daemon

`agent-collab-daemon --cli pi` is a separate reactive path that spawns
batched pi invocations with a persistent `--session` file (Arch D). Use
the **extension** when you want a live TUI pi session holding context;
use the **daemon** when you want autonomous fresh-spawn batching without
an attached terminal. They don't conflict — a single machine can run
both for different labels.

### Recipe: "consult with <model>, keep session running for context"

Stand up a background pi session backed by a non-Claude model (codex,
gemini, zai-glm, …) and talk to it from the current Claude session.
This is the canonical flow when the user asks to "consult with codex
and keep the session running so it keeps context" — or any analogous
model. Verified working 2026-04-19 with `openai-codex / gpt-5.4`.

**Prereqs**
- Pi extension installed at `~/.pi/agent/extensions/peer-inbox-pi.ts`
  (installer places it).
- Provider auth already set up in pi (`pi config` once per provider;
  operator-owned, not in shell env). If pi can't auth, it exits
  silently after join — check `stderr.log`.

**Steps** (run from Claude Code; treat as a template):

```bash
# 1. Mint a fresh pair-key (private 1:1 room).
agent-collab room create
# → created: <pair-key> (empty room)

# 2. Register this Claude session into the room.
agent-collab session register \
  --label claude-lead --agent claude \
  --pair-key <pair-key> --force
# → registered: claude-lead ... [channel: paired]

# 3. Launch pi headless in background, auto-joining via env.
#    IMPORTANT: pipe `tail -f /dev/null` into pi. In --mode rpc pi
#    reads JSON-RPC on stdin; without a held-open stdin it EOF-exits
#    seconds after booting, taking the socket with it.
mkdir -p /tmp/pi-<label>-test
tail -f /dev/null | \
  PEER_INBOX_LABEL=<label> PEER_INBOX_PAIR_KEY=<pair-key> \
  pi --provider openai-codex --model gpt-5.4 \
     --mode rpc --no-session \
     > /tmp/pi-<label>-test/stdout.log \
     2> /tmp/pi-<label>-test/stderr.log &
# Launch with run_in_background:true when using the Bash tool.

# 4. Wait ~5s, then verify pi joined and socket is live.
ls -la /tmp/peer-inbox-pi-*.sock
agent-collab peer list --as claude-lead
# Expect: <label>  pi  peer  active

# 5. Send first message.
agent-collab peer send --as claude-lead --to <label> \
  --message "<prompt>"
# → sent to <label> (pushed)

# 6. Reply arrives via the UserPromptSubmit hook on Claude's next
#    turn (wrapped in <peer-inbox from="<label>">...</peer-inbox>).
#    Typical latency 15-30s depending on model.
```

**Provider / model table** (pairs verified to exist; run
`pi --list-models <provider>` to confirm current availability):

| User says | `--provider` | `--model` suggestion |
|---|---|---|
| "consult codex" / gpt-5.x | `openai-codex` | `gpt-5.4` (current default), `gpt-5.3-codex`, `gpt-5.4-mini` |
| "consult gemini" | `google` | `gemini-3-pro` / whatever's current |
| "consult claude via pi" | `anthropic` | `claude-opus-4-7` etc. |
| "consult glm" | `zai-glm` (plugin) | `glm-4.6` |

**Follow-up turns.** Just keep sending `peer send --as claude-lead
--to <label> --message ...`. Pi holds the conversation state across
turns — that's the whole point of this vs. the daemon fresh-spawn
path.

**Teardown**:

```bash
# Kill pi. If launched via run_in_background, use the background task
# ID; otherwise `kill %1` / pkill the pi PID seen in the socket name.
agent-collab session close --label claude-lead
# Pi's registration row auto-cleans on `session_shutdown` in the
# extension; if pi was kill -9'd, manually clear:
#   sqlite3 ~/.agent-collab/sessions.db \
#     "DELETE FROM sessions WHERE label='<label>'"
```

**Troubleshooting**
- **Pi exits immediately after logging "joined as …"** → missing the
  `tail -f /dev/null |` stdin held-open pipe. Rerun step 3.
- **`(no-channel)` on send** → socket went away. Check pi process +
  socket path; relaunch from step 3.
- **No reply lands on Claude** → check `agent-collab peer receive
  --as claude-lead --format plain` (bypasses hook), then check
  `/tmp/pi-<label>-test/stderr.log` for provider auth errors.

---

## Examples

### Two Claude sessions in the same repo

```bash
# Terminal 1, ~/Development/dj:
agent-collab session register --label backend --agent claude --role lead

# Terminal 2, ~/Development/dj:
agent-collab session register --label frontend --agent claude --role peer

# In backend, ask frontend about a contract:
agent-collab peer send --to frontend --message "
Changing auth middleware from 401 to 403 on expired tokens.
Does the iOS app distinguish? Reply with [[end]] when done."

# Frontend's next prompt auto-injects this. Frontend replies via peer_send;
# backend's next prompt auto-injects the reply.
```

### Self-sustaining exchange via channels

```bash
# Both terminals, launch:
claude --dangerously-load-development-channels server:peer-inbox

# Each registers (channel pairs automatically).

# Kick it off from backend:
agent-collab peer send --to frontend --message "
Draft the migration 036 rollback plan in 3 bullets. Reply and include [[end]]."

# Walk away. Frontend wakes without prompting, replies. Backend wakes,
# reads reply, might respond further, or the [[end]] token stops the loop.
```

Watch it live:

```bash
agent-collab peer web --port 8787 &
open http://127.0.0.1:8787
```

### Claude-led challenge review via peer send

```bash
# Claude (lead) drafts migration, asks Codex (challenger) to review:
agent-collab peer send --to reviewer --message "
Migration 036: NOT NULL column on user_agents (50M rows).
Proposed: batched UPDATE in 10k chunks. Challenge rollback + concurrency."

# Codex runs peer receive, reviews from its own live repo context, sends
# back via peer send. Two-round protocol cap + evidence-first rules from
# the collaboration protocol still apply — peer messages are just
# challenge passes carried on a different transport.
```

### Archive a finished exchange

```bash
agent-collab peer replay --as backend --out ~/Desktop/backend-review.html
```

---

## Troubleshooting

### `no session key available`

Register refused because no session-key source was found. Claude Code's
hook logs session_ids to `~/.agent-collab/claude-sessions-seen/` on
every turn. Fixes, in order of preference:

1. Type any prompt first (even "hi") so the hook logs the session_id,
   then retry register.
2. Set `AGENT_COLLAB_SESSION_KEY` explicitly:

   ```bash
   export AGENT_COLLAB_SESSION_KEY="$(uuidgen)"
   ```

3. Pass `--session-key <k>` to register.

### `multiple sessions registered in <path>; pass --as <label>`

You're in a repo with more than one registered session and neither a
session key nor `--as` is set. Either export a session key matching one
of them, or pass `--as <label>` explicitly:

```bash
agent-collab peer receive --as backend --mark-read
```

### Existing registration's session_key doesn't match Claude's session_id

Symptom: the hook fires (visible in `~/.agent-collab/hook.log`) but
fails with "multiple sessions registered". Cause: the session registered
with a random key before the hook logged the real session_id.

Find the real session_id and adopt:

```bash
# Peek at what the hook has logged for this cwd:
ls ~/.agent-collab/claude-sessions-seen/
cat ~/.agent-collab/claude-sessions-seen/<hash>.log

# Adopt (from inside the mis-registered session):
agent-collab session adopt --label <your-label> --session-id <claude-uuid>
```

### `peer offline: <label>`

The recipient hasn't been seen in > 6 hr. Check:

```bash
agent-collab peer list --include-stale
```

### `path drift: marker at X records cwd Y`

A marker file was copied (e.g., `rsync`ed across machines). Re-register:

```bash
agent-collab session register --label <your-label> --agent <...> --force
```

### Hook isn't injecting messages into Claude

In order:

1. `./scripts/doctor-global-protocol` — any BLOCKs?
2. `tail ~/.agent-collab/hook.log` — last failure line.
3. `agent-collab peer receive` — does it show unread?
4. `python3 -m json.tool ~/.claude/settings.json | grep peer-inbox` —
   hook entry present?
5. **Restart the Claude session** — settings.json changes apply on
   session start, not mid-session.

### Channel paired but messages don't arrive

1. Is the channel process alive?
   `ps -ef | grep peer-inbox-channel.py`
2. Is the socket present?
   `ls -la /tmp/peer-inbox/`
3. Is the `.mcp.json` entry correct and the session launched with
   `--dangerously-load-development-channels server:peer-inbox`?
4. Inspect `~/.agent-collab/hook.log` and the channel's own stderr
   (routed via `claude --debug hooks`).

### Hit the max-turns cap or accidentally terminated

```bash
agent-collab peer reset --to <other-label>
```

### Uninstall and start fresh

```bash
./scripts/install-global-protocol uninstall
rm -rf ~/.agent-collab       # WARNING: nukes sessions.db (history gone)
./scripts/install-global-protocol
```

---

## Configuration reference

Environment variables read by `peer-inbox-db.py` and the channel server:

| Variable | Default | Purpose |
|---|---|---|
| `AGENT_COLLAB_INBOX_DB` | `~/.agent-collab/sessions.db` | SQLite database path |
| `AGENT_COLLAB_SESSION_KEY` | — | Explicit session key (highest precedence) |
| `CLAUDE_SESSION_ID` | (set by hook) | Claude's runtime session ID; propagated by the hook |
| `CODEX_SESSION_ID` | — | Session discriminator for Codex runs |
| `GEMINI_SESSION_ID` | — | Session discriminator for Gemini runs |
| `AGENT_COLLAB_MAX_PAIR_TURNS` | `500` | Max turns per pair before send rejection |
| `AGENT_COLLAB_HOOK_LOG` | `~/.agent-collab/hook.log` | Hook failure log location |
| `PEER_INBOX_SOCKET_DIR` | `/tmp/peer-inbox` | Channel Unix socket directory (short, for macOS AF_UNIX 104-char cap) |
| `PEER_INBOX_PENDING_DIR` | `~/.agent-collab/pending-channels` | Channel-server process registry (for register pairing) |

Config file overrides (`.agent-collab.env` at a repo root):

```
AGENT_COLLAB_INBOX_DB=/path/to/custom.db
```

Only explicit env overrides caller-provided config. See
[LOCAL-INTEGRATION.md](./LOCAL-INTEGRATION.md) for the full config-file
schema.

---

## Limits and non-goals

- **Same machine only.** SQLite is local. Cross-host sync is
  out-of-scope for v1.x.
- **Mid-turn async push is Claude-only today.** All three CLIs
  auto-inject on turn start via the unified hook (Claude
  `UserPromptSubmit`, Codex `UserPromptSubmit`, Gemini `BeforeAgent`).
  Mid-turn notifications that arrive while an agent is working
  surface in Claude via the peer-inbox MCP channel but not in Codex
  (blocked on [Codex issue #18056](https://github.com/openai/codex/issues/18056))
  or Gemini (structurally declined per their issue #3052). The
  Topic 3 v0 auto-reply daemon closes the Codex / Gemini parity gap
  for *reactive* behavior (fresh LLM per batch at turn boundaries,
  W3 worker shape per
  [DAEMON-OPERATOR-GUIDE.md](./DAEMON-OPERATOR-GUIDE.md)) but is
  still turn-boundary-driven — it does not enable mid-turn push
  into an already-running Codex / Gemini session.
- **Message bodies are UTF-8 text**, 8 KB max. Binary / `\0` bytes
  are not supported — base64-encode externally if needed.
- **No archive or retention.** V1 retains every message forever.
  Direct SQL on `~/.agent-collab/sessions.db` is the audit surface.
  V2 may add a retention spec if a real need emerges.
- **Channels are research-preview.** The `--dangerously-load-development-channels`
  flag and notification schema could change in any Claude Code
  release. Pin your Claude Code version in operational docs.
- **Prompt injection is a real surface.** Anyone with write access to
  the inbox (or who can POST to a channel's socket) can inject content
  Claude will see as context. For DJ-personal use this is fine; for
  multi-user or shared environments, gate sender identity carefully.

---

## Related docs

- [ARCHITECTURE.md](./PEER-INBOX-ARCHITECTURE.md) — system design, data model, delivery paths
- [CHANGELOG.md](../CHANGELOG.md) — version history (v1.0–v1.5)
- [DAEMON-OPERATOR-GUIDE.md](./DAEMON-OPERATOR-GUIDE.md) — Topic 3 v0 auto-reply daemon operator reference
- [DAEMON-VALIDATION.md](./DAEMON-VALIDATION.md) — Topic 3 v0 daemon E2E probe protocol
- [plans/peer-inbox.md](../plans/peer-inbox.md) — original design doc (v3, post-Codex review)
- [.agent-collab/reviews/peer-inbox-*.md](../.agent-collab/reviews/) — challenge + verify reviews
- [GLOBAL-PROTOCOL.md](./GLOBAL-PROTOCOL.md) — the collaboration protocol peer-inbox extends
- [LOCAL-INTEGRATION.md](./LOCAL-INTEGRATION.md) — how consuming repos opt in
