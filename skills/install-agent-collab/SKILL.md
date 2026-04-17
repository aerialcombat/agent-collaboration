---
name: install-agent-collab
description: Install or update the agent-collaboration peer-inbox toolkit on this machine. Use when the user says "install agent-collab", "set up peer-inbox", "install the cross-session collaboration helper", or wants Claude, Codex, and Gemini sessions to exchange messages.
---

# Install agent-collab

This skill installs (or updates) the peer-inbox toolkit so Claude Code, Codex, and Gemini sessions on this machine can exchange messages via `/agent-collab` and `/peer` slash commands.

## When to use

User says any of:

- "install agent-collab"
- "set up peer-inbox" / "set up cross-session collaboration"
- "I want /agent-collab to work"
- "install the peer inbox"

Skip if `command -v agent-collab` already succeeds AND `~/Development/agent-collaboration` already exists AND the user is just asking how to use it (route them to `/agent-collab` instead).

## Steps

1. **Check git + python3.** Refuse cleanly if either is missing and tell the user what to install.

   ```bash
   command -v git >/dev/null 2>&1 || { echo "git is required — install it first"; exit 1; }
   command -v python3 >/dev/null 2>&1 || { echo "python3 is required — install it first"; exit 1; }
   ```

2. **Clone the repo** (or pull if already present).

   ```bash
   REPO=~/Development/agent-collaboration
   if [ -d "$REPO/.git" ]; then
     git -C "$REPO" pull --ff-only
   else
     git clone https://github.com/aerialcombat/agent-collaboration.git "$REPO"
   fi
   ```

   If the clone URL fails, ask the user for the correct one — don't guess. Air-gapped machines: tell the user to copy the repo manually to `~/Development/agent-collaboration` and re-invoke the skill.

3. **Run the installer.**

   ```bash
   bash ~/Development/agent-collaboration/scripts/install-global-protocol install
   ```

4. **Verify.**

   ```bash
   command -v agent-collab >/dev/null 2>&1 && echo "agent-collab on PATH"
   bash ~/Development/agent-collaboration/scripts/doctor-global-protocol 2>&1 | tail -20
   ```

   If `agent-collab` is not on PATH, the installer prints a warning; tell the user to add `~/.local/bin` to their shell PATH.

5. **Confirm success.** Print:

   ```
   ✓ agent-collab installed
   ✓ slash commands: /agent-collab, /peer
   ✓ UserPromptSubmit hook installed in ~/.claude/settings.json
   ✓ peer-inbox MCP registered in ~/.claude.json (user scope)

   Default delivery: UserPromptSubmit hook — messages arrive on next prompt.
   Real-time delivery (optional): launch Claude with
       claude --dangerously-load-development-channels server:peer-inbox
   and then run /agent-collab. The flag is a Claude Code preview feature;
   without it, `/peer check` reads the inbox on demand.

   Restart Claude Code (or run /agent-collab now) to register this session.
   In another terminal, run /agent-collab there too and join with the pair key.
   ```

## Don't

- Don't run destructive commands. `install-global-protocol` is idempotent and backs up existing files automatically.
- Don't modify the user's shell rc files. If PATH needs updating, tell the user which line to add.
- Don't install on a machine where the user hasn't agreed to the clone + settings.json changes — the installer touches `~/.claude/settings.json` (for the hook) and `~/.claude/commands/` (for slash commands). Mention this before running in case the user is fine-tuned about their config.
