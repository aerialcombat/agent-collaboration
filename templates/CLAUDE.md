# Global Claude Collaboration Defaults

This file provides global Claude Code defaults for collaboration with Codex CLI and Gemini CLI.

Protocol reference:
- `{{PROJECT_ROOT}}/docs/GLOBAL-PROTOCOL.md`

## Global Rules

- Use a lead / challenger model by default.
- Trigger collaboration for non-trivial, risky, architectural, release, or compliance-sensitive work.
- Do not force collaboration for trivial mechanical edits where challenge adds no signal.
- The default maximum is two challenge rounds before escalation.
- Claude is a strong default lead for planning and orchestration unless repo-local rules say otherwise.
- As lead, own coordination and invoke Codex for challenge or verification when the task is non-trivial.
- As challenger, return substantive review text and do not converge just to reduce friction.
- Require evidence from code, tool output, tests, or repo rules before accepting Codex's position.
- If collaboration fails because Codex does not return usable output, record that explicitly instead of inventing review content.
- If Codex is unavailable on a task that should have had review, treat that as a failed challenge pass rather than silently skipping collaboration.
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

To invoke Gemini as challenger:

```bash
agent-collab challenge --challenger gemini --scope AGENT-COLLABORATION.md
agent-collab verify --challenger gemini --scope AGENT-COLLABORATION.md
```

## Cross-Session Coordination (peer inbox)

When you are working in one Claude Code session and another session (Claude,
Codex, or Gemini) is running in the same machine, you can exchange messages
via the peer inbox.

At the start of a cross-session workflow, register this session so peers
can address you by label. Claude Code exports `CLAUDE_SESSION_ID` to
subprocess env and the installed `UserPromptSubmit` hook propagates it,
so self-identity is automatic:

```bash
agent-collab session register --label backend --agent claude --role lead
```

If `CLAUDE_SESSION_ID` isn't set (unusual), export
`AGENT_COLLAB_SESSION_KEY` to any unique string before registering, or
pass `--session-key <k>` explicitly.

List live peers in this repo:

```bash
agent-collab peer list
```

Send a message to another session:

```bash
agent-collab peer send --to frontend --message "auth token TTL is now 15m"
```

Unread peer messages are auto-injected into your context at the start of
each turn via the `UserPromptSubmit` hook installed by
`install-global-protocol`. Messages appear wrapped in a
`<peer-inbox>...</peer-inbox>` block with sender labels and timestamps.

Fidelity is maximal because the peer answers from its own live context.
Bloat stays low because only distilled answers cross the wire (not full
transcripts). Per-message cap is 8 KB; per-turn injection cap is 4 KB.

Close the session when done (removes marker, leaves audit trail in DB):

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
