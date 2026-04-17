# Global Gemini Collaboration Defaults

This file provides global Gemini CLI defaults for collaboration with Claude Code and Codex CLI.

Protocol reference:
- `{{PROJECT_ROOT}}/docs/GLOBAL-PROTOCOL.md`

## Global Rules

- Use a lead / challenger model by default.
- Trigger collaboration for non-trivial, risky, architectural, release, or compliance-sensitive work.
- Do not force collaboration for trivial mechanical edits where challenge adds no signal.
- The default maximum is two challenge rounds before escalation.
- Gemini is a strong default for fast, independent review passes unless repo-local rules say otherwise.
- As challenger, return substantive review text and do not converge just to reduce friction.
- Require evidence from code, tool output, tests, or repo rules before accepting another agent's position.
- As lead, own coordination and invoke Claude or Codex for challenge or verification when the task is non-trivial.
- If collaboration fails because the other agent does not return usable output, record that explicitly instead of inventing review content.
- If the other agent is unavailable on a task that should have had review, treat that as a failed challenge pass rather than silently skipping collaboration.
- Stop and escalate to the human owner if disagreement remains after the allowed rounds.

## Quick Start

When you are leading non-trivial work in a repo that has a local collaboration guide:

```bash
agent-collab challenge --challenger codex --scope AGENT-COLLABORATION.md
agent-collab verify --challenger codex --scope AGENT-COLLABORATION.md
```

If you need to preview what the runner will do before invoking Codex:

```bash
agent-collab challenge --challenger codex --scope AGENT-COLLABORATION.md --dry-run
```

If the repo has no local helper or runner setup, fall back to the repo-local docs or the global protocol reference for manual shell invocation patterns.

To invoke Claude as challenger:

```bash
agent-collab challenge --challenger claude --scope AGENT-COLLABORATION.md
agent-collab verify --challenger claude --scope AGENT-COLLABORATION.md
```

## Cross-Session Coordination (peer inbox)

You can exchange messages with peer sessions (Claude, Codex, or Gemini) on
the same machine via the peer inbox. Gemini CLI supports `BeforeAgent` and
`SessionStart` hooks, which can auto-inject peer messages at turn start —
see manual install note at the bottom of this section. Until then, poll
explicitly.

Register this session once at the start. Until v1.1 wires Gemini's
`BeforeAgent` hook to export a session ID automatically, set
`AGENT_COLLAB_SESSION_KEY` to any unique string before registering:

```bash
export AGENT_COLLAB_SESSION_KEY="gemini-$(date +%s)-$RANDOM"
agent-collab session register --label <your-label> --agent gemini --role <your-role>
```

Alternative: pass `--session-key <k>` explicitly to every peer command.

Check for unread peer messages:

```bash
agent-collab peer receive --mark-read
```

Send a message to a peer session:

```bash
agent-collab peer send --to <peer-label> --message "<text>"
```

List live peers in this repo:

```bash
agent-collab peer list
```

### Manual hook install (v1)

Automated install for Gemini lands in v1.1 once the exact config shape is
verified. Until then, to enable automatic peer-message injection, add a
`BeforeAgent` hook pointing at `~/.agent-collab/hooks/peer-inbox-inject.sh`
in your Gemini settings. Refer to the current Gemini CLI docs for the exact
config path and key names (they have changed across versions). Without the
hook, call `agent-collab peer receive --mark-read` explicitly at turn start
when cross-session context matters.

Close the session when done:

```bash
agent-collab session close
```

## Scope Of This File

This file defines global behavior only.

Repo-local files should define:
- specific plan paths
- helper commands
- event logs
- ownership tables
- verification commands

When a repo provides a local collaboration implementation, follow it as long as it does not weaken the global rules above. If it does not, a thin global runner such as `agent-collab` is an acceptable fallback.
