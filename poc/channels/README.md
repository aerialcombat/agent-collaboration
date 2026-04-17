# Peer-Inbox Channels POC

Proof-of-concept demonstrating that Claude Code's [Channels](https://code.claude.com/docs/en/channels)
(research-preview) can deliver real-time push into a running session —
the mechanism we'd need for self-sustaining peer-inbox conversations.

## What this POC proves

- A stdlib-only Python MCP server advertising
  `capabilities.experimental['claude/channel']` is accepted by Claude
  Code as a channel plugin.
- External HTTP POSTs (with `X-Sender` allowlist gating) translate
  directly into `<channel source="peer-inbox" from="...">body</channel>`
  tags in the running session's live context.
- The offline handshake + sender-gating + notification emission all work
  (`test-mcp-handshake.py` exercises the full path without needing a
  real Claude session).

This is **not** integrated with the SQLite peer-inbox yet. If this POC
holds up in a real Claude session, v1.4 wires the Channels path to the
existing `peer send` so both storage and push delivery happen on every
send.

## Files

- `peer-inbox-channel.py` — the MCP server. Stdio JSON-RPC + localhost
  HTTP on :8789 (override via `PEER_INBOX_CHANNEL_PORT`). No external
  deps; Python 3.9+.
- `.mcp.json` — spawn manifest Claude Code reads from the project root.
- `test-mcp-handshake.py` — offline test harness. Spawns the server,
  does an init handshake, POSTs a message, verifies the channel
  notification lands.

## Launch (test against a real Claude session)

From a **new** terminal (not the one you're talking to me in):

```bash
cd ~/Development/agent-collaboration/poc/channels
claude --dangerously-load-development-channels server:peer-inbox
```

Then from a **third** terminal:

```bash
# Default allowlist includes: dev, test-backend, test-front, orchestrator
curl -v -H 'X-Sender: dev' -H 'X-To: live-session' \
  -d 'hello from curl, live' \
  http://127.0.0.1:8789/greeting
```

Expected behavior in the Claude session — no user prompt needed — the
next thing Claude processes should include:

```
<channel source="peer-inbox" from="dev" chat_id="greeting" to="live-session">
hello from curl, live
</channel>
```

Verify by asking the session *"did you just receive a peer message?"*
— it should quote the content and the `from` attribute.

## Verify offline (no real Claude session required)

```bash
python3 test-mcp-handshake.py
```

Should print:

```
PASS: initialize advertises experimental.claude/channel
PASS: HTTP POST accepted (status 200)
PASS: unauthorized sender rejected with 403
PASS: channel notification has correct content+meta

ALL POC CHECKS PASSED
```

## Env-var knobs

| Variable | Default | Purpose |
|---|---|---|
| `PEER_INBOX_CHANNEL_HOST` | `127.0.0.1` | HTTP bind host |
| `PEER_INBOX_CHANNEL_PORT` | `8789` | HTTP bind port |
| `PEER_INBOX_ALLOWED_SENDERS` | `dev,test-backend,test-front,orchestrator` | Comma-separated sender allowlist |

## Notes on Channels (research-preview as of 2026-04)

- `--channels` and `--dangerously-load-development-channels` exist in
  the Claude Code binary but are not listed in `--help` — this is the
  expected behavior for research-preview features.
- Channels require **claude.ai login** (OAuth). API-key / Bedrock /
  Vertex / Foundry auth do not enable channels.
- For Team/Enterprise accounts, the managed setting `channelsEnabled:
  true` is required. Personal Pro/Max bypasses this.
- Meta keys must match `[A-Za-z0-9_]+`; hyphens/dots are silently
  dropped by Claude. Our POC uses snake_case only.
- Security note from the binary itself: *"inbound messages will be
  pushed into this session, this carries prompt injection risks."*
  The `X-Sender` allowlist is the load-bearing mitigation.

## Next step if this works

Wire `agent-collab peer send` to also POST to the recipient's channel
HTTP endpoint when the recipient is known to be live-registered with
a channel. The SQLite write remains authoritative; the HTTP poke is
the push signal. Sessions not on channels fall back to the hook path
cleanly.
