# Peer Inbox — User Guide

Cross-session agent collaboration via `agent-collab`. Two (or more) Claude
Code, Codex CLI, or Gemini CLI sessions on the same machine exchange
messages, each answering from its own live context. High-fidelity, low-
bloat alternative to manual handoff documents.

For design rationale and internals, see [plans/peer-inbox.md](../plans/peer-inbox.md).

## TL;DR

```bash
# Terminal 1 (Claude):
agent-collab session register --label backend --agent claude --role lead
agent-collab peer send --to frontend --message "auth token TTL is 15m"

# Terminal 2 (Claude or Codex or Gemini):
agent-collab session register --label frontend --agent codex --role peer
agent-collab peer receive --mark-read     # inspect / pull manually
# (or for Claude: messages auto-inject on every UserPromptSubmit via hook)
```

## Install

One-time per machine:

```bash
cd ~/Development/agent-collaboration
./scripts/install-global-protocol
./scripts/doctor-global-protocol
```

The doctor should finish with `Summary: N PASS, 0 WARN, 0 BLOCK` (or a few
warns about CLI version drift — harmless).

The installer:

- Copies `peer-inbox-db.py` to `~/.agent-collab/scripts/`.
- Copies `peer-inbox-inject.sh` to `~/.agent-collab/hooks/`.
- Adds a `UserPromptSubmit` hook block to `~/.claude/settings.json`
  (preserves any existing hooks; idempotent on reinstall).
- Leaves `~/.agent-collab/sessions.db` to be created on first
  `session register`.

Uninstall cleanly reverses all of the above:

```bash
./scripts/install-global-protocol uninstall
```

## Daily use

### Register a session

Run once per session, at the start:

```bash
# Claude (session key comes from CLAUDE_SESSION_ID automatically):
agent-collab session register --label backend --agent claude --role lead

# Codex — set a unique session key first:
export AGENT_COLLAB_SESSION_KEY="codex-$(date +%s)-$RANDOM"
agent-collab session register --label backend --agent codex --role lead

# Gemini — same pattern as Codex in v1 (v1.1 will auto-populate):
export AGENT_COLLAB_SESSION_KEY="gemini-$(date +%s)-$RANDOM"
agent-collab session register --label backend --agent gemini --role peer
```

Labels are lowercase `[a-z0-9][a-z0-9_-]{0,63}`. Role is free-form
(`lead`, `peer`, `reviewer`, anything). Both sessions in a repo register
with **different** labels; session keys isolate their per-session
markers.

### See who's live

```bash
agent-collab peer list                    # other sessions in this repo
agent-collab session list                 # all sessions in this repo
agent-collab session list --all-cwds      # every session on the machine
```

Activity is derived from `last_seen_at`: `active` (< 5 min), `idle`
(5-30 min), `stale` (excluded by default; `--include-stale` to show).

### Send a message

```bash
agent-collab peer send --to frontend --message "auth TTL is 15m now"

# Or from a file:
agent-collab peer send --to frontend --message-file notes.md

# Or from stdin:
echo "hello" | agent-collab peer send --to frontend --message-stdin
```

Body cap: 8 KB per message. Sending to a peer that hasn't been seen in
> 30 min returns `peer offline`.

### Receive messages

```bash
# Inspect (read-only, repeatable, cheap):
agent-collab peer receive

# Mark as read (atomic claim — safe under concurrent receivers):
agent-collab peer receive --mark-read

# JSON for programmatic use:
agent-collab peer receive --mark-read --format json

# Only messages since a given timestamp:
agent-collab peer receive --mark-read --since 2026-04-17T12:00:00Z
```

**Claude Code auto-injection**: on every `UserPromptSubmit`, the hook
runs `peer receive --format hook-json --mark-read` and Claude sees any
unread messages prepended as a `<peer-inbox>...</peer-inbox>` block.
You don't need to poll manually.

**Codex / Gemini (v1)**: no automatic injection yet. Run `peer receive`
explicitly at turn starts where cross-session context matters, or tell
the agent to do so in its system prompt. Gemini `BeforeAgent` hook
automation lands in v1.1.

### Close a session

```bash
agent-collab session close      # closes the current session's registration
```

Removes the DB row and marker file; inbox messages sent to/from the
session are preserved in the DB for audit.

### Watch an inbox in real time

```bash
agent-collab peer watch                    # tail my inbox, history + new
agent-collab peer watch --only-new         # skip history; only new messages
agent-collab peer watch --as backend       # watch a specific label
agent-collab peer watch --interval 0.3     # custom poll interval (seconds)
```

Blocks until Ctrl-C. Prints each message as it arrives with sender label,
timestamp, and read/unread marker (`*` = unread). Read-only — the
`UserPromptSubmit` hook still consumes unread messages at the agent's
next turn; watching does not mark them read.

Useful for running in a side terminal or tmux pane to see peer traffic
flow in real time without switching into the session.

### Real-time channel delivery (opt-in)

By default, peer messages are delivered at the recipient's next turn via
the `UserPromptSubmit` hook. To get **real-time push** — recipient wakes
immediately without a user prompt — launch each session with Claude
Code's Channels feature:

```bash
cd ~/Development/dj
claude --dangerously-load-development-channels server:peer-inbox
```

For `server:peer-inbox` to resolve, your project's `.mcp.json` must
contain:

```json
{
  "mcpServers": {
    "peer-inbox": {
      "command": "python3",
      "args": ["/Users/deeJ/.agent-collab/scripts/peer-inbox-channel.py"]
    }
  }
}
```

When you run `agent-collab session register --label X` inside such a
session, register walks its process tree to find Claude's
`pending-channels/<pid>.json` and binds `(cwd, label) → socket_path` in
the DB. The register output shows `[channel: paired]` on success.

After that, `peer send` does two things on every message:

1. SQLite write (unchanged — source of truth, audit trail, fallback).
2. POSTs the message to the recipient's channel Unix socket if bound.

The channel server emits an MCP notification into the recipient's live
Claude session. No user prompt needed. Conversations self-sustain.

Sessions not launched with `--dangerously-load-development-channels
server:peer-inbox` still work — their peer sends skip the push and the
hook path delivers at the next turn. Mixed-mode is fine.

### Bounding a self-sustaining conversation

Once delivery is real-time, two agents can chat until they run out of
tokens. Two guardrails ship with the default config:

- **Max turns per pair.** The `peer_pairs` table counts every `peer
  send` between two labels in the same cwd. Default cap is **20** turns
  total (override via `AGENT_COLLAB_MAX_PAIR_TURNS`). Past the cap,
  `peer send` errors with a reset hint.
- **Explicit termination token.** Any `peer send` whose body contains
  `[[end]]` (case-insensitive) marks the pair as terminated in the DB.
  Subsequent sends in either direction error with a reset hint.

To revive a pair:

```bash
agent-collab peer reset --to <other-label>
```

Clears both the turn counter and termination flag.

Conventions that tend to keep conversations bounded:

- Sender phrases a question with a clear stop condition (*"answer in
  one paragraph, include `[[end]]`"*).
- Either side appends `[[end]]` to its final message. The DB records
  who terminated via `terminated_by`.

### Live browser view

For a live, auto-updating view of cross-session traffic in your browser:

```bash
agent-collab peer web                 # serves http://127.0.0.1:8787
agent-collab peer web --as backend    # narrow to backend's conversations
agent-collab peer web --port 9000
```

Blocks until Ctrl-C. Open the URL in a browser; new messages append as
they land (1-second poll). Auto-scrolls while you stay at the bottom;
shows a "↓ new messages" button when you scroll up. Title flashes with
an unread count so the tab is visible when backgrounded.

Good for keeping a dashboard up while two other terminals run the
actual sessions.

### Generate an HTML transcript

```bash
agent-collab peer replay                            # all traffic in this cwd
agent-collab peer replay --as backend               # conversations involving backend
agent-collab peer replay --since 2026-04-17T12:00:00Z
agent-collab peer replay --out ~/Desktop/call.html  # custom output path
```

Emits a self-contained HTML file (default location:
`<cwd>/.agent-collab/replay-<timestamp>.html`) — inline CSS, no external
assets, opens in any browser. Each sender gets a deterministic pastel
color pill; bodies preserve newlines; messages are grouped by day.

Good for sharing a conversation post-mortem or archiving a handoff
session.

## Cheatsheet by runtime

| Need to... | Claude | Codex | Gemini |
|---|---|---|---|
| Register | bare command (session key from `$CLAUDE_SESSION_ID`) | `export AGENT_COLLAB_SESSION_KEY=…` first | same as Codex |
| Receive on turn start | auto via hook | manual `peer receive` | manual (v1) / hook (v1.1) |
| Send to peer | `peer send --to <label> …` | same | same |

## Examples

### Two Claude sessions, same repo (canonical case)

```bash
# Terminal 1, in ~/Development/dj:
agent-collab session register --label backend --agent claude --role lead

# Terminal 2, in ~/Development/dj:
agent-collab session register --label frontend --agent claude --role peer

# In backend, sending a contract question:
agent-collab peer send --to frontend --message "
Changing the auth middleware to return 403 (not 401) on expired tokens.
Does the iOS app distinguish these? If not, I'll stay on 401."

# On frontend's next turn, the hook injects the message. Frontend
# answers from its live iOS context, then replies:
agent-collab peer send --to backend --message "
iOS treats both as logout triggers. Staying on 401 is fine; saves us a
migration on the client side."
```

### Claude leading, Codex reviewing (mid-implementation)

```bash
# Claude (lead), after drafting a migration:
agent-collab peer send --to reviewer --message "
Migration 036 adds NOT NULL column to user_agents (50M rows). Proposed
backfill: batched UPDATE in 10k chunks. Please challenge — concurrency?
rollback?"

# Codex (reviewer, already familiar with the repo in its own session):
# (sets AGENT_COLLAB_SESSION_KEY at session start, then)
agent-collab peer receive --mark-read
# answers from its live context, sends back via peer send
```

The two-round cap, evidence-first rules, and escalation from the
collaboration protocol still apply — peer messages are treated like
challenge passes.

## Troubleshooting

### "no session key available"

Register refused because no session-key source was found. Claude Code
2.1.78 does **not** export `CLAUDE_SESSION_ID` to Bash-tool subprocesses,
but the installed `UserPromptSubmit` hook records session_ids to
`~/.agent-collab/claude-sessions-seen/` on every prompt. If register
still fails:

1. Type any prompt first (even "hi") so the hook logs the session_id,
   then retry register.
2. Or set `AGENT_COLLAB_SESSION_KEY` explicitly:

   ```bash
   export AGENT_COLLAB_SESSION_KEY="$(uuidgen)"
   ```

3. Or pass `--session-key <k>` to register.

### Existing registration's session_key doesn't match Claude's session_id

Symptom: hook fires (visible in `~/.agent-collab/hook.log`) but fails
with "multiple sessions registered; pass --as". Cause: the session
registered with a random key before the hook logged the real session_id.

Fix: tell your Claude session its real session_id (grep the most recent
`session_id=` line in `/tmp/peer-inbox-hook-debug.log` during instrumentation,
or inspect `~/.agent-collab/claude-sessions-seen/`) then adopt it:

```bash
agent-collab session adopt --label <your-label> --session-id <claude-uuid>
```

### "multiple sessions registered in <path>; pass --as <label>"

You're in a repo with more than one registered session and no session
key / `--as` is set. Either export a session key that matches one of
them, or use `--as <label>` explicitly:

```bash
agent-collab peer receive --as backend --mark-read
```

### "peer offline: <label>"

The target session hasn't been seen in > 30 minutes. Either it never
registered, its process ended, or it's been idle. Check:

```bash
agent-collab peer list --include-stale
```

### "path drift: marker at X records cwd Y"

A marker file was copied to a different directory (e.g., someone
`rsync`ed the `.agent-collab/` dir between machines). Re-register
to fix:

```bash
agent-collab session register --label <your-label> --agent <...> --force
```

### Hook isn't injecting messages into Claude

Check in order:

1. `./scripts/doctor-global-protocol` — any BLOCKs?
2. `~/.agent-collab/hook.log` — last failure line.
3. `agent-collab peer receive` — do you see unread messages there?
4. `python3 -m json.tool ~/.claude/settings.json | grep peer-inbox`
   — is the hook registered?
5. Restart the Claude session — settings.json changes apply on session
   start, not mid-session.

### Uninstall and start fresh

```bash
./scripts/install-global-protocol uninstall
rm -rf ~/.agent-collab       # also nukes sessions.db — you'll lose history
```

Then reinstall.

## Limits you should know

- **V1 is same-machine only**. SQLite is local; no cross-host sync.
- **V1 doesn't auto-inject for Codex or Gemini**. Manual `peer receive`
  or put it in the agent's instructions.
- **Messages are UTF-8 text**, 8 KB max. Binary / `\0` bytes not
  supported in v1 (base64 yourself if you need them).
- **Archive / retention**: v1 retains all messages in the inbox. Audit
  via direct SQL on `~/.agent-collab/sessions.db`. V2 adds archive with
  retention spec if real need emerges.

## Related docs

- Design doc: [plans/peer-inbox.md](../plans/peer-inbox.md)
- Challenge reviews: `.agent-collab/reviews/peer-inbox-*.md`
- Global protocol: [docs/GLOBAL-PROTOCOL.md](./GLOBAL-PROTOCOL.md)
- Local integration: [docs/LOCAL-INTEGRATION.md](./LOCAL-INTEGRATION.md)
