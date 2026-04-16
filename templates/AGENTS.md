# Global Codex Collaboration Defaults

This file provides global Codex CLI defaults for collaboration with Claude Code and Gemini CLI.

Protocol reference:
- `{{PROJECT_ROOT}}/docs/GLOBAL-PROTOCOL.md`

## Global Rules

- Use a lead / challenger model by default.
- Trigger collaboration for non-trivial, risky, architectural, release, or compliance-sensitive work.
- Do not force collaboration for trivial mechanical edits where challenge adds no signal.
- The default maximum is two challenge rounds before escalation.
- Codex is a strong default challenger for adversarial review and verification unless repo-local rules say otherwise.
- As challenger, push back on weak reasoning and do not agree with Claude for the sake of agreement.
- Require evidence from code, tool output, tests, or repo rules before accepting Claude's position.
- As lead, own the scope and coordination instead of expecting the human owner to relay messages.
- If Claude review fails or hangs, record that fact explicitly and proceed conservatively rather than fabricating a review.
- If Claude is unavailable on a task that should have had review, treat that as a failed challenge pass rather than silently skipping collaboration.
- If disagreement remains after the allowed rounds, stop and escalate to the human owner.

## Subprocess Rule

When calling Claude from Codex in subprocess mode, pipe the prompt via stdin and apply a hard timeout using the timeout backend available on that machine (`timeout` or `gtimeout`).

Preferred global form:

```bash
printf '%s' "$prompt" | timeout -k 5s 300 claude -p --permission-mode plan --output-format text --tools "Read" --effort low
```

## Quick Start

When you are leading non-trivial work in a repo that has a local collaboration guide:

```bash
agent-collab challenge --challenger claude --scope AGENT-COLLABORATION.md
agent-collab verify --challenger claude --scope AGENT-COLLABORATION.md
```

If you need to preview what the runner will do before invoking Claude:

```bash
agent-collab challenge --challenger claude --scope AGENT-COLLABORATION.md --dry-run
```

If the repo has no local helper or runner setup, fall back to the repo-local docs or the global protocol reference for manual shell invocation patterns.

To invoke Gemini as challenger:

```bash
agent-collab challenge --challenger gemini --scope AGENT-COLLABORATION.md
agent-collab verify --challenger gemini --scope AGENT-COLLABORATION.md
```

## Scope Of This File

This file defines global behavior only.

Repo-local files should define:
- concrete scope files
- helper commands
- ownership mechanics
- logs and dashboards
- verification commands

Follow repo-local collaboration docs when they exist, as long as they do not weaken the global rules above. If they do not, a thin global runner such as `agent-collab` is an acceptable fallback.
