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
agent-collab challenge --challenger claude --scope AGENT-COLLABORATION.md
agent-collab verify --challenger codex --scope AGENT-COLLABORATION.md
```

If you need to preview what the runner will do before invoking a challenger:

```bash
agent-collab challenge --challenger claude --scope AGENT-COLLABORATION.md --dry-run
```

If the repo has no local helper or runner setup, fall back to the repo-local docs or the global protocol reference for manual shell invocation patterns.

## Scope Of This File

This file defines global behavior only.

Repo-local files should define:
- specific plan paths
- helper commands
- event logs
- ownership tables
- verification commands

When a repo provides a local collaboration implementation, follow it as long as it does not weaken the global rules above. If it does not, a thin global runner such as `agent-collab` is an acceptable fallback.
