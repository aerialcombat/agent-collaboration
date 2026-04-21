# Peer Inbox — Architecture

Technical reference for hacking on the peer-inbox system. For user-facing
operation, see [PEER-INBOX-GUIDE.md](./PEER-INBOX-GUIDE.md).

---

## System overview

```
┌─────────────────────────────────────────────────────────────────┐
│  ~/.agent-collab/sessions.db   (SQLite: source of truth)        │
│  ┌────────────────┐   ┌────────────────┐   ┌────────────────┐   │
│  │ sessions       │   │ inbox          │   │ peer_pairs     │   │
│  │ (cwd, label,   │   │ (from, to,     │   │ (turn count,   │   │
│  │  session_key,  │   │  body, read_at)│   │  terminated_at)│   │
│  │  channel_sock) │   └────────────────┘   └────────────────┘   │
│  └────────────────┘                                             │
└──────────────▲──────────────────────────────────────────────────┘
               │
     ┌─────────┴────────────┬───────────────────────────┐
     │                      │                           │
┌────┴─────────┐     ┌──────┴──────────┐       ┌────────┴────────┐
│ agent-collab │     │ peer-inbox-     │       │ peer-inbox-     │
│   (bash)     │     │ inject.sh       │       │ channel.py      │
│ dispatcher   │     │ (hook)          │       │ (MCP server)    │
└──────┬───────┘     └──────┬──────────┘       └────────┬────────┘
       │                    │                           │
       ▼                    ▼                           ▼
 peer-inbox-db.py   UserPromptSubmit             stdio JSON-RPC
 (all DB logic)     hook, triggered by               (to Claude)
                    Claude Code at every            + Unix socket
                    user prompt                     (from peers)
```

Three surfaces call `peer-inbox-db.py`:
- The **bash dispatcher** (`agent-collab <group> <verb>`) — user-facing.
- The **hook** (`peer-inbox-inject.sh`) — runs at every `UserPromptSubmit`.
- The **channel server** (`peer-inbox-channel.py`) — runs as an MCP subprocess of each Claude session.

All three share one SQLite database.

---

## File layout

### Installed (user-local)

```
~/.local/bin/agent-collab                        # CLI dispatcher (bash)
~/.agent-collab/
├── scripts/
│   ├── peer-inbox-db.py                         # Python helper (all DB ops)
│   └── peer-inbox-channel.py                    # MCP channel server
├── hooks/
│   └── peer-inbox-inject.sh                     # UserPromptSubmit hook
├── sessions.db                                  # SQLite DB (0600)
├── hook.log                                     # Hook failure log
├── claude-sessions-seen/<cwd-hash>.log          # Hook-logged session IDs
└── pending-channels/<claude-pid>.json           # Live channel registrations
                                                 #   (per running claude process)
```

### Per-repo (project-local)

```
<repo>/
├── .agent-collab/
│   └── sessions/<session-key-hash>.json         # Per-session marker
│                                                # {"cwd", "label", "session_key"}
├── .gitignore                                   # Auto-extended with .agent-collab/sessions/
└── .mcp.json                                    # Contains peer-inbox server entry
                                                 #   (only if using channels)
```

### Ephemeral

```
/tmp/peer-inbox/<channel-pid>.sock               # AF_UNIX socket per channel server
```

---

## Data model

### `sessions`

```sql
CREATE TABLE sessions (
  cwd            TEXT NOT NULL,
  label          TEXT NOT NULL,
  agent          TEXT NOT NULL,            -- claude | codex | gemini
  role           TEXT,                     -- free-form: lead, peer, reviewer...
  session_key    TEXT,                     -- discriminator from env (CLAUDE_SESSION_ID etc.)
  channel_socket TEXT,                     -- Unix socket path if a channel is paired
  started_at     TEXT NOT NULL,            -- ISO-8601 UTC
  last_seen_at   TEXT NOT NULL,
  PRIMARY KEY (cwd, label)
);
CREATE INDEX idx_sessions_last_seen ON sessions(last_seen_at);
CREATE INDEX idx_sessions_key ON sessions(session_key);
```

Why `(cwd, label)` PK, not a UUID: human labels are the addressing
surface; scoping by cwd prevents label collisions across projects;
auditing by `WHERE cwd = ?` is natural.

### `inbox`

```sql
CREATE TABLE inbox (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  to_cwd        TEXT NOT NULL,
  to_label      TEXT NOT NULL,
  from_cwd      TEXT NOT NULL,
  from_label    TEXT NOT NULL,
  body          TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  read_at       TEXT                       -- NULL = unread
);
CREATE INDEX idx_inbox_to_unread ON inbox(to_cwd, to_label, read_at);
```

Append-only. `read_at IS NULL` is "unread"; atomic claim-and-mark sets
it to now.

### `peer_pairs`

```sql
CREATE TABLE peer_pairs (
  cwd            TEXT NOT NULL,
  a_label        TEXT NOT NULL,
  b_label        TEXT NOT NULL,            -- canonical: sorted (a <= b)
  turn_count     INTEGER NOT NULL DEFAULT 0,
  terminated_at  TEXT,
  terminated_by  TEXT,                     -- label that emitted [[end]]
  PRIMARY KEY (cwd, a_label, b_label)
);
```

Pair identity is canonical: `(backend, frontend)` and `(frontend, backend)`
key the same row. Used for turn-cap + termination enforcement.

---

## Identity resolution

Each session has three identifiers:
1. **Label** — `backend` — how others address it.
2. **Session key** — the thing that discriminates multiple sessions in the
   same cwd. Sourced (in precedence order) from `--session-key`,
   `AGENT_COLLAB_SESSION_KEY`, `CLAUDE_SESSION_ID`,
   `CODEX_SESSION_ID`, `GEMINI_SESSION_ID`, or the hook-logged seen-log
   as a last-resort fallback.
3. **Canonical cwd** — `$PWD` → absolute → symlinks resolved. Newlines
   rejected.

### The session-key problem (and why the hook logs IDs)

Claude Code **does not export `CLAUDE_SESSION_ID` to Bash-tool
subprocesses** (confirmed empirically as of 2.1.78). The session ID is
only visible inside hook stdin JSON. So:

1. At every `UserPromptSubmit`, the hook captures `session_id` from
   stdin and appends it to
   `~/.agent-collab/claude-sessions-seen/<cwd-hash>.log`.
2. When the user later runs `session register`, the helper:
   - Reads `--session-key` if given.
   - Reads env vars if set.
   - **As a fallback, picks the most recent unregistered session_id
     from the seen-log** and uses that as the session key.
3. Once registered, `session register` also binds `sessions.session_key`
   to this value, so future hook invocations (which already know the
   session_id) can resolve self by matching it against the DB.

### Marker files

Per-session marker at `<cwd>/.agent-collab/sessions/<sha256(key)[:16]>.json`:

```json
{"cwd": "/Users/deeJ/Development/dj", "label": "backend",
 "session_key": "47bfd9ea-d651-4788-ae6e-52727d20b420"}
```

Used by **walk-parents discovery**: a subcommand invoked from any
subdirectory finds its own session by walking up parent directories
looking for this file keyed on the session-key-derived hash.

### Path-drift detection

When a marker's recorded `cwd` doesn't match its filesystem location
(e.g., someone `rsync`ed the dir), registration/send errors with
`path drift`. Prevents stale markers from routing messages to nonsensical
cwds.

---

## Delivery paths

### Hook path (default, always available)

```
┌────────────────────────────────────────────────────────┐
│ Recipient's Claude session                             │
│                                                        │
│   ┌──────────┐                                         │
│   │ User     │──── user types a prompt                 │
│   │ prompt   │                                         │
│   └────┬─────┘                                         │
│        │                                               │
│        ▼                                               │
│   UserPromptSubmit event (JSON on stdin)               │
│        │                                               │
│        ▼                                               │
│   peer-inbox-inject.sh                                 │
│     1. extract session_id from stdin JSON              │
│     2. export CLAUDE_SESSION_ID=<id>                   │
│     3. `agent-collab hook log-session` — log to seen   │
│     4. `agent-collab peer receive --format hook-json   │
│                                   --mark-read`         │
│     5. emit {hookSpecificOutput.additionalContext=...} │
│        │                                               │
│        ▼                                               │
│   Claude sees <peer-inbox>...</peer-inbox> in context  │
│                                                        │
└────────────────────────────────────────────────────────┘
```

Latency: recipient's next user prompt. Works for any Claude session
with the default install. Codex/Gemini currently have no equivalent
turn-start hook, so they use manual `peer receive`.

### Channel path (real-time push, opt-in)

```
┌──────────────────┐     POST JSON       ┌──────────────────┐
│ Sender's         │ ───────────────────▶│ Recipient's      │
│ Claude session   │  via Unix socket    │ Claude session   │
│                  │                     │                  │
│ peer send        │                     │ peer-inbox-chan  │
│  1. SQLite write │                     │  (MCP subproc)   │
│  2. POST to      │                     │   │              │
│     recipient's  │                     │   │ stdio        │
│     channel      │                     │   ▼              │
│     socket       │                     │ emit             │
└──────────────────┘                     │ notifications/   │
                                         │ claude/channel   │
                                         │   │              │
                                         │   ▼              │
                                         │ Claude sees      │
                                         │ <channel>...</>  │
                                         │ in live context  │
                                         │ — wakes to a     │
                                         │ new turn         │
                                         │ without prompt   │
                                         └──────────────────┘
```

- Each Claude session spawns its own `peer-inbox-channel.py` subprocess
  via `.mcp.json` + the `--dangerously-load-development-channels
  server:peer-inbox` flag.
- At startup, the channel creates its socket at
  `/tmp/peer-inbox/<pid>.sock` and writes
  `~/.agent-collab/pending-channels/<claude-pid>.json` — keyed on its
  **parent** PID (Claude) so `session register` can find it via
  process-tree walk.
- `session register` walks up from its own PID looking for a
  pending-channels file. When found, binds `sessions.channel_socket`.
- `peer send` looks up `channel_socket` on the recipient row; if present,
  POSTs the message over HTTP/1.1 on the Unix socket; channel server
  emits the MCP notification.
- Delivery is **additive**: SQLite write is authoritative; channel
  push is a signal. If the push fails, the hook path still delivers.

### pi extension path (real-time push, pi only)

pi (pi-coding-agent) has no MCP-channel story; instead the bundled
extension `extensions/peer-inbox-pi.ts` (installed at
`~/.pi/agent/extensions/peer-inbox-pi.ts`) makes pi a first-class
continuing-session peer.

```
┌──────────────────┐    POST JSON        ┌──────────────────────┐
│ Sender (any      │ ───────────────────▶│ pi session           │
│ peer-inbox role) │  via Unix socket    │                      │
│                  │                     │ extension HTTP/1.1   │
│ peer send        │                     │ listener on          │
│  1. SQLite write │                     │ /tmp/peer-inbox-pi-  │
│  2. POST to      │                     │   <pid>-<label>.sock │
│     channel_sock │                     │   │                  │
│     on recipient │                     │   ▼                  │
│     row          │                     │ wrap as              │
└──────────────────┘                     │ <peer-inbox from=…   │
                                         │  meta…>body</…>      │
                                         │   │                  │
                                         │   ▼                  │
                                         │ pi.sendUserMessage(  │
                                         │   envelope,          │
                                         │   {deliverAs:        │
                                         │    "followUp"})      │
                                         │   │                  │
                                         │   ▼                  │
                                         │ pi LLM sees it as a  │
                                         │ user follow-up turn  │
                                         │ and replies in-      │
                                         │ context              │
                                         │   │                  │
                                         │   ▼ turn_end         │
                                         │ extension auto-      │
                                         │ relays first text    │
                                         │ block back via       │
                                         │ peer-send to the     │
                                         │ original sender      │
                                         └──────────────────────┘
```

- The extension itself calls `peer-inbox-db.py session-register` to
  register under `--agent pi`, then opens the socket and
  `UPDATE sessions SET channel_socket=...` for its own row. No
  separate `agent-collab session register` step needed from the user.
- Operator entry points: slash commands `/peer-join`, `/peer-leave`,
  `/peer-status`, `/peer-send`, `/peer-broadcast`.
- LLM entry points: the same verbs as registered tools (`peer_join`,
  `peer_send`, etc.) — pi's model can decide to DM / broadcast
  autonomously.
- Headless mode: env vars `PEER_INBOX_LABEL` and `PEER_INBOX_PAIR_KEY`
  trigger auto-join on `session_start`, for pi running in `--mode rpc`
  / `--mode json` without a TUI.
- Teardown: `session_shutdown` closes the server, unlinks the socket,
  and clears `channel_socket` in the DB.

This is why **pi is the preferred continuing-session peer** for
non-Claude work: unlike codex-cli (no mid-turn push) and gemini-cli
(refused upstream), pi holds a single long-running process whose
context accumulates naturally across peer round-trips. The RPC-bridge
+ operator-attach path that previously tried to deliver this was
retired (commit `f6d4ff0`) in favor of this native extension.

---

## Concurrency

All mutating SQL runs under:

```sql
PRAGMA journal_mode=WAL;     -- concurrent readers, single writer
PRAGMA busy_timeout=5000;    -- wait up to 5s on lock contention
BEGIN IMMEDIATE;             -- acquire writer lock up front
...
COMMIT;
```

The atomic claim-and-mark on `peer receive --mark-read` uses
`UPDATE ... RETURNING` under `BEGIN IMMEDIATE`:

```sql
UPDATE inbox
SET read_at = ?
WHERE to_cwd = ? AND to_label = ? AND read_at IS NULL
RETURNING id, from_label, body, created_at;
```

No race: concurrent receivers see a serial order of claims, each sees
exactly what they claimed.

---

## Hook contract

`peer-inbox-inject.sh` fulfills Claude Code's `UserPromptSubmit` contract:

- **Stdin:** JSON with at least `session_id` and `cwd`.
- **Stdout:** either empty (no injection) or a JSON object with shape
  `{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit",
  "additionalContext": "<peer-inbox>…</peer-inbox>"}}`. The body is
  escaped via Python's stdlib `json.dump` — no `jq` dependency.
- **Exit code:** always 0 (fail-open). Every error path logs to
  `~/.agent-collab/hook.log` and returns silently.

This is the load-bearing discipline: a broken inbox, missing `python3`,
corrupt DB, or anything else must never block the user's turn. Only
the turn after the break is slower.

---

## MCP channel protocol

The channel advertises a single experimental capability:

```json
{
  "capabilities": {
    "experimental": {"claude/channel": {}}
  }
}
```

On incoming socket POST, it emits:

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/claude/channel",
  "params": {
    "content": "<message body>",
    "meta": {"from": "backend", "to": "frontend"}
  }
}
```

Claude Code renders this as `<channel source="peer-inbox" from="backend"
to="frontend">body</channel>` in the running agent's context.

Per the [channels reference](https://code.claude.com/docs/en/channels-reference),
meta keys must match `[A-Za-z0-9_]+` — the channel server sanitizes
caller-supplied meta to this allowlist (non-conforming keys dropped).

---

## Design decisions

### Why SQLite for the store

- Single file, zero setup, built into Python stdlib.
- WAL + `UPDATE ... RETURNING` solves the concurrent-claim problem.
- Direct SQL is the audit surface; no separate retention logic needed
  in v1.
- Messages are KB-scale; storage is free for years of operation.

### Why Unix sockets for the channel push (not HTTP on localhost)

- Unix sockets are faster, require no port coordination, have tight
  filesystem permissions (`0600`).
- Per-session sockets in `/tmp/peer-inbox/` are trivially cleaned up on
  process exit.
- macOS's 104-char path limit on AF_UNIX — kept under via short
  paths `/tmp/peer-inbox/<pid>.sock`.

### Why process-tree walk for channel pairing

- Each Claude session spawns its channel subprocess; the channel's
  parent PID **is** Claude's PID.
- `session register` runs as a descendant of the same Claude process;
  walking up its own PID chain crosses Claude's PID deterministically.
- Claude doesn't export its PID (or session ID) to Bash subprocesses,
  so this is the most reliable way to pair a register call with the
  right channel without a central daemon.

### Why fallback hook + push additive (not push-only)

- Claude's channel flag is research-preview — schemas may change.
- Codex/Gemini don't support channels yet.
- Hook path has zero launch-flag cost; channel path is pure upgrade.
- Mixed deployments (one session channel-enabled, one not) should
  still work.

### What's deliberately deferred

- **Archive / retention spec.** V1 retains everything. Storage is
  cheap; retention adds complexity without a concrete user need.
- **Cross-machine sync.** SQLite is local. Cross-host would need a
  network daemon and auth — separate design phase.
- **Auto-inject for Codex/Gemini.** Blocked on each runtime's hook
  surface. Gemini `BeforeAgent` is doable for v1.6; Codex has no
  documented turn-start hook as of codex-cli 0.115.0.
- **MCP server wrapper exposing peer send as a tool.** Agents currently
  call `agent-collab peer send` via the Bash tool. A Tools-layer
  wrapper would remove that indirection but isn't blocking any use
  case today.

### What was considered and rejected

- **Full transcript import (pull peer's JSONL).** 75MB worst case,
  Claude-only, lossy anyway because the agent re-reads noise. The
  peer-inbox's mini-message model is higher-fidelity per byte.
- **Shared event log (JSONL, no inbox).** Close second to the inbox;
  rejected because addressing semantics (who is the message for?)
  was ambiguous in an event log.
- **Remote-control bridge transport** (Anthropic's official
  human↔session relay). It's hub-and-spoke via Anthropic's cloud, no
  peer-to-peer primitive; enqueue endpoint is undocumented and
  reverse-engineering-only. Channels are the right fit.
- **tmux `send-keys` injection** (ensemble.claude's approach). Works
  but: requires tmux-managed panes, races with user typing, input
  interpreted as user prompts (provenance blur). Channel notifications
  are the cleaner mechanism.

---

## Extension points

If you want to build on peer-inbox:

- **New subcommand?** Add a `cmd_<group>_<verb>` function in
  `peer-inbox-db.py`, register in `build_parser()`, extend
  `agent-collab` usage text.
- **New delivery path?** Write a new receiver that shares the SQLite
  DB and emits via your transport (websocket, HTTP push, Slack, etc.).
  Pair it at register time by extending `sessions` schema with your
  transport's endpoint column.
- **New runtime?** Add a `session register --agent <newname>`, teach
  the register auto-key discovery to read the runtime's session-ID
  env var, document the hook (or lack thereof) in the guide.
- **New test scenario?** Add to `tests/smoke.sh` under
  `run_peer_inbox_tests()`. Each scenario should be self-contained
  with its own isolated DB in `$TMP_ROOT`.

---

## Operational runbook

### Install health check

```bash
./scripts/doctor-global-protocol
```

Expected: `43 PASS, N WARN, 0 BLOCK`. Any BLOCK means the install is
broken.

### See what's in the DB

```bash
sqlite3 ~/.agent-collab/sessions.db <<SQL
.headers on
.mode column
SELECT cwd, label, agent, last_seen_at FROM sessions;
SELECT COUNT(*) AS total, SUM(read_at IS NULL) AS unread FROM inbox;
SELECT cwd, a_label, b_label, turn_count, terminated_at FROM peer_pairs;
SQL
```

### Nuke and reset (destructive)

```bash
./scripts/install-global-protocol uninstall
rm -rf ~/.agent-collab
./scripts/install-global-protocol
```

### Debug a not-delivering message

1. Confirm DB has the message: `sqlite3 ~/.agent-collab/sessions.db
   'SELECT * FROM inbox ORDER BY id DESC LIMIT 5'`.
2. Confirm recipient is registered and not stale: `agent-collab
   session list --all-cwds`.
3. For hook path: check `~/.agent-collab/hook.log` for errors on the
   recipient's side.
4. For channel path: check `/tmp/peer-inbox/<pid>.sock` exists and
   `sessions.channel_socket` matches.
