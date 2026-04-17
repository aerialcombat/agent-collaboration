Register this Claude Code session for peer-inbox collaboration with another agent session (Claude, Codex, or Gemini) on the same machine.

User-provided arguments: $ARGUMENTS

## What to do

First, check that the helper is installed:

```bash
command -v agent-collab >/dev/null 2>&1 || {
  echo "agent-collab not found on PATH."
  echo "Install: https://github.com/… (or run ~/Development/agent-collaboration/scripts/install-global-protocol)"
  exit 1
}
```

If the user passed flags in $ARGUMENTS (e.g. `--label backend --new` or `--label frontend --join swift-canyon-8f3a`), use those directly. Otherwise, interview the user in a single round of prompts — gather label and new-or-join-and-key before running any command.

### Interactive flow

Ask the user (in one terse message):
1. **Label** — what should this session be called? (short, lowercase, e.g. `backend`, `frontend`, `alpha`). If they don't care or say "auto", omit `--label` and the helper will mint one (`adjective-noun`). Echo the generated label back so they know how the peer will address them.
2. **New pair or join?** — "new" mints a fresh pair key for the user to share with the other session; "join" accepts a pair key someone else already has.
3. If **join**, also ask for the pair key.

Once you have the answers, run:

```bash
# new pair, explicit label
agent-collab session register --label "<label>" --agent claude --new-pair

# new pair, auto-label (omit --label)
agent-collab session register --agent claude --new-pair

# join existing pair
agent-collab session register --label "<label>" --agent claude --pair-key "<KEY>"
```

The output line contains `pair_key=<KEY>`. When the user chose "new", **print that pair key back to them explicitly** so they can paste it into the other terminal — the whole flow depends on it.

Then confirm:

```bash
agent-collab peer list
```

If the list is empty on a new pair, say "registered — share `<pair_key>` with the other session and they should run /agent-collab to join." If the list shows a peer, say "paired with <label>."

## Failure modes (fail open)

- `agent-collab: command not found` → print the install hint above and stop.
- `error: label 'X' already active` → offer the user a different label.
- `error: pair key 'X' already has a session labeled 'Y'` → the other session picked the same label; ask the user for a different one.
- Any other non-zero exit → surface the stderr verbatim and stop. Don't retry silently.

## Don't

- Don't register a session until the user confirms the label.
- Don't send messages from this command — that's `/peer`.
- Don't guess a pair key; always ask.
