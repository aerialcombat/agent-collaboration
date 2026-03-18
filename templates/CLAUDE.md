# Global Claude Collaboration Defaults

This file provides global Claude Code defaults for collaboration with Codex CLI.

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

## Scope Of This File

This file defines global behavior only.

Repo-local files should define:
- specific plan paths
- helper commands
- event logs
- ownership tables
- verification commands

When a repo provides a local collaboration implementation, follow it as long as it does not weaken the global rules above. If it does not, a thin global runner such as `agent-collab` is an acceptable fallback.
