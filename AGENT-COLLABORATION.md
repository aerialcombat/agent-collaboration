# Agent Collaboration

This repository follows the global Claude Code + Codex CLI collaboration protocol, but its local implementation is intentionally light.

Global protocol reference:
- `docs/GLOBAL-PROTOCOL.md`

Local integration reference:
- `docs/LOCAL-INTEGRATION.md`

## Local Sources Of Truth

- local collaboration guide: `AGENT-COLLABORATION.md`
- global protocol spec: `docs/GLOBAL-PROTOCOL.md`
- repo integration guide: `docs/LOCAL-INTEGRATION.md`
- global runner: `scripts/agent-collab`
- installer: `scripts/install-global-protocol`
- doctor: `scripts/doctor-global-protocol`
- review artifacts: `.agent-collab/reviews/`
- peer-inbox auto-reply daemon (Topic 3 v0): `docs/DAEMON-OPERATOR-GUIDE.md` (operator reference) and `docs/DAEMON-VALIDATION.md` (E2E probe protocol)

This repo does not use plan/state/event files by default because most work here is doc-heavy and low-risk.

## When Collaboration Is Required Here

Use a challenge pass by default for:

- changes to `docs/GLOBAL-PROTOCOL.md`
- changes to `scripts/install-global-protocol`
- changes to `scripts/agent-collab`
- changes to `scripts/peer-inbox-db.py`
- changes to `scripts/doctor-global-protocol`
- changes to `hooks/peer-inbox-inject.sh`
- changes that blur the global/local boundary
- changes that alter default trigger rules or escalation behavior

Skip collaboration by default for:

- obvious typo fixes
- isolated copy edits with no behavioral change
- purely mechanical formatting changes

## Roles

- `lead`
  Coordinates the change, invokes the challenger, and decides what feedback is accepted.
- `challenger`
  Returns text only, focuses on unsafe defaults and boundary mistakes, and does not recursively call back.

## Default Path

1. Lead updates the relevant docs or scripts.
2. Lead runs `agent-collab challenge` under the configured hard timeout.
3. Challenger reviews the changed files in read-only mode.
4. Lead applies justified fixes.
5. Lead runs `agent-collab verify` or a second challenge pass if needed, again under the configured hard timeout.
6. If disagreement survives two rounds, escalate.

## Local Helper Commands

Use the global runner directly in this repo:

```bash
agent-collab challenge --challenger claude --scope docs/GLOBAL-PROTOCOL.md
agent-collab verify --challenger codex --context scripts/install-global-protocol
agent-collab challenge --challenger gemini --scope docs/GLOBAL-PROTOCOL.md
agent-collab verify --challenger gemini --context scripts/install-global-protocol
```

The repo-local defaults come from `.agent-collab.env`, including the default hard timeout budget. Use `--timeout-seconds` when a specific challenger or scope needs a different ceiling.

## Timeout Policy

- every `challenge` and `verify` run in this repo must use a hard timeout
- the repo default is tuned for Claude as the default challenger
- Codex uses a shorter default timeout here to fail fast on broad review scopes; raise it explicitly with `--timeout-seconds` when the Codex scope is intentionally larger
- a timeout counts as a failed challenge pass and must be recorded, not retried silently
- `--timeout-seconds 0` is debugging-only and does not count as a valid collaboration run for this repo

## Review Focus For This Repo

Challenge reviews in this repo should focus on:

- destructive installer behavior
- stale CLI assumptions
- unsafe subprocess defaults
- global/local boundary confusion
- documentation that promises behavior the scripts do not enforce

## Verification

Minimum verification for changes here:

- `bash -n` for edited shell scripts
- `./scripts/doctor-global-protocol`
- rerun `./scripts/install-global-protocol` when installer or templates change
- inspect the rendered globals if install behavior changed
- exercise at least one timeout success path and one timeout failure path when changing runner timeout behavior

## Escalation

- default maximum: two challenge rounds
- if the challenger fails or returns unusable output, record that fact
- if the agents still disagree after two rounds, escalate to the human owner
