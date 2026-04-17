Send or receive messages across peer sessions via agent-collab.

User-provided arguments: $ARGUMENTS

## What to do

First check the helper exists:

```bash
command -v agent-collab >/dev/null 2>&1 || {
  echo "agent-collab not found on PATH. Run /agent-collab first, or install from ~/Development/agent-collaboration/scripts/install-global-protocol."
  exit 1
}
```

Parse $ARGUMENTS as one of these forms. The first token after `/peer` selects the action.

### `send <to> <message...>` (also: `tell`, `msg`)

Example: `/peer send frontend the TTL changed to 15m` or `/peer tell frontend TTL is now 15m`.

```bash
agent-collab peer send --to "<to>" --message "<message>"
```

If the user omitted `<to>` and there is exactly one peer in the current pair, you may infer it — but confirm once before sending.

### `check` (also: `inbox`, `receive`)

Example: `/peer check`.

```bash
agent-collab peer receive --mark-read
```

Print the messages verbatim. If empty: "no new messages."

### `list` (also: `who`)

```bash
agent-collab peer list
```

### `end`

Terminate the pair. Same as sending `[[end]]`.

```bash
agent-collab peer send --to "<peer>" --message "[[end]]"
```

Ask the user to confirm the peer label before ending if there are multiple.

### Anything else

If the first token doesn't match, treat the whole $ARGUMENTS as a message and ask the user who to send it to.

## Failure modes (fail open)

- `command not found` → print install hint and stop.
- `error: no session registered` → tell the user to run `/agent-collab` first.
- `error: peer offline` → say so; don't retry.
- Any other non-zero exit → surface the stderr verbatim and stop.

## Don't

- Don't invent a pair key or a label. Ask.
- Don't mark messages read without `check` — `peer receive` without `--mark-read` is a preview only.
- Don't chain sends without the user asking.
