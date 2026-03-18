# Global Codex Collaboration Defaults

This file provides global Codex CLI defaults for collaboration with Claude Code.

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

When calling Claude from Codex in subprocess mode, pipe the prompt via stdin.

Preferred global form:

```bash
printf '%s' "$prompt" | claude -p --permission-mode plan --output-format text
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
