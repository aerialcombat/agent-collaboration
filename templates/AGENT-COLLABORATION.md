# Agent Collaboration

This repository follows the global Claude Code + Codex CLI collaboration protocol.

Global protocol reference:
- `<path-to-global-protocol-or-project-doc>`

This file defines the repo-local implementation.

## Local Sources Of Truth

Replace these placeholders with the real repo paths:

- local plan path: `<plans/<scope>.md or equivalent>`
- local state path: `<plans/<scope>.state.json or equivalent>`
- local events path: `<plans/<scope>.events.jsonl or equivalent>`
- local helper: `<infra/agent or equivalent>`

If the repo does not use all of these artifacts, say what replaces them.

If this repo uses the global `agent-collab` runner instead of a custom helper, add a repo-root `.agent-collab.env` with supported keys such as:

```bash
AGENT_COLLAB_GUIDE=AGENT-COLLABORATION.md
AGENT_COLLAB_REVIEW_DIR=.agent-collab/reviews
AGENT_COLLAB_DEFAULT_CHALLENGER=claude
```

## When Collaboration Is Required Here

Replace this section with repo-specific rules.

Suggested default:

- non-trivial implementation work
- risky refactors
- architecture or contract changes
- release, safety, auth, billing, or compliance-sensitive work

Suggested default skip cases:

- trivial mechanical edits
- isolated formatting work
- obvious one-file fixes with straightforward verification

## Roles

- `lead`
  Coordinates the scope, records decisions, and invokes the challenger.
- `challenger`
  Reviews critically, returns text only by default, and does not soften real objections.

If this repo allows bounded challenger write scopes, define the rule explicitly.

## Default Path: Synchronous Review

1. Lead writes or updates the scope plan.
2. Lead invokes the challenger.
3. Challenger returns review text only.
4. Lead records the review and decision.
5. Lead implements.
6. Lead may invoke the challenger again for verification.

The lead is the scheduler in this path.

## Optional Path: Parallel Work

Only enable this section if the repo has a real ownership mechanism.

Define:

- how ownership is claimed
- how conflicts are rejected
- what files or prefixes can be owned
- how handoff works
- what happens when ownership is unclear

Do not allow concurrent same-file editing by default.

## Local Helper Commands

Document the actual commands for this repo.

Example shape:

```bash
<helper> init <scope>
<helper> review-request <scope> codex "<prompt>"
<helper> status <scope>
<helper> archive <scope>
```

If the repo has no custom helper, either document `agent-collab` usage for this repo or replace this section with the direct shell commands the lead should use.

## Manual Trigger Patterns

Copy the canonical manual trigger patterns from the global protocol reference and adapt only the repo-specific paths and prompt text for this repository.

The lead should append or summarize the resulting review in the local plan or decision record.

## Verification

Define what counts as verification in this repo:

- required tests
- lint or build commands
- release or safety checks
- manual review items

If a task is too risky to move forward without challenger feedback, say so explicitly.

## Escalation

- default maximum: two challenge rounds
- if the agents still disagree, stop and escalate to the human owner
- if the challenger times out or fails, record that fact explicitly

## Notes For This Repo

Add any repo-specific constraints here:

- worktree expectations
- sandbox rules
- forbidden paths
- deployment boundaries
- additional reviewers or hooks
