# Local Integration Guide

This guide explains how a consuming repository should add its local layer on top of the global Claude Code + Codex CLI collaboration protocol.

The global layer defines behavior.
The local layer defines implementation.

## Minimum Local Layer

A consuming repository should provide at least:

- `CLAUDE.md`
  Repo-local instructions for Claude Code.
- `AGENTS.md`
  Repo-local instructions for Codex CLI.
- `GEMINI.md`
  Repo-local instructions for Gemini CLI.
- `AGENT-COLLABORATION.md`
  The concrete workflow for this repo: paths, helper commands, ownership rules, and verification flow.

The repo may also provide:

- plan files such as `plans/<scope>.md`
- state or event records such as `plans/<scope>.state.json` or `plans/<scope>.events.jsonl`
- helper scripts such as `infra/agent`
- a data-only `.agent-collab.env` file for the global `agent-collab` runner
- dashboards, logs, or hooks

Those are recommended, not globally required.

What matters is that the local implementation is explicit and repo-visible.

## What Stays Global

Keep these in the global protocol:

- lead / challenger model
- evidence-first disagreement rules
- default two-round limit
- escalation to the human owner
- failure honesty when the challenger does not return
- safe subprocess guidance, especially Codex calling Claude through stdin piping

These should be consistent across repositories.

## What Must Be Local

Keep these repo-specific:

- exact plan and log paths
- helper command names and flags
- ownership format
- test and verification commands
- dashboards and metrics
- archival or cleanup rules
- product- or org-specific release gates

Do not put those details in the global layer.

## Recommended Local Documents

### `CLAUDE.md`

Should say:

- this repo follows the global protocol
- where the local collaboration guide lives
- what files or directories Claude should treat as the local source of truth
- any repo-specific constraints for Claude

### `AGENTS.md`

Should say:

- this repo follows the global protocol
- where the local collaboration guide lives
- what files or directories Codex should treat as the local source of truth
- any repo-specific constraints for Codex

### `GEMINI.md`

Should say:

- this repo follows the global protocol
- where the local collaboration guide lives
- what files or directories Gemini should treat as the local source of truth
- any repo-specific constraints for Gemini

### `AGENT-COLLABORATION.md`

Should define:

- when collaboration is required in this repo
- the synchronous path
- the parallel path, if supported
- how ownership is represented
- how plans, decisions, and reviews are recorded
- how one agent should invoke the other
- where logs or monitoring live
- how unresolved disagreements escalate

## Minimum Safe Workflow

If the repo does not provide helper scripts yet, the local guide should still make the workflow concrete:

1. Lead writes or updates a scope plan.
2. Lead invokes the challenger with a read-only prompt.
3. Challenger returns review text only.
4. Lead records the review and decision.
5. Lead implements or explicitly splits work.
6. Lead invokes the challenger again for verification if needed.
7. If disagreement survives two rounds, escalate.

That is enough to get started safely.

## Recommended Local Helpers

A good local helper usually covers:

- scope initialization
- ownership claim or handoff
- review request
- state inspection or dashboard
- archival

Keep the helper thin. It should make the protocol easier to follow, not replace the protocol.

If the repo wants to use the global `agent-collab` command instead of a custom wrapper, add a `.agent-collab.env` file at the repo root. Supported keys:

```bash
AGENT_COLLAB_GUIDE=AGENT-COLLABORATION.md
AGENT_COLLAB_REVIEW_DIR=.agent-collab/reviews
AGENT_COLLAB_DEFAULT_CHALLENGER=claude
AGENT_COLLAB_TIMEOUT_SECONDS=300
AGENT_COLLAB_CLAUDE_TIMEOUT_SECONDS=300
AGENT_COLLAB_CODEX_TIMEOUT_SECONDS=300
AGENT_COLLAB_GEMINI_TIMEOUT_SECONDS=300
AGENT_COLLAB_CLAUDE_EFFORT=low
```

Keep that file data-only. Do not rely on shell execution in repo config.

When using `agent-collab` with Claude as the challenger, prefer passing the actual guide, scope, and changed files that matter. The runner can inline those files into the prompt, which is faster and more reliable than asking Claude to discover them from path names during the review pass. The runner also supports hard timeout defaults and per-challenger timeout overrides so the lead can bound challenge latency explicitly.

## Manual Trigger Examples

If no local helper exists, start from the canonical manual trigger patterns in [GLOBAL-PROTOCOL.md](GLOBAL-PROTOCOL.md#manual-trigger-patterns) and adapt only the repo-specific paths and prompt text.

## Adoption Checklist

Use this checklist when adding the local layer to a repo:

- add `CLAUDE.md`
- add `AGENTS.md`
- add `GEMINI.md`
- add `AGENT-COLLABORATION.md`
- link local docs to the global protocol
- choose plan and review artifacts
- choose whether parallel work is supported
- define ownership and escalation rules
- define verification commands
- add any helper scripts only after the workflow is clear on paper

## Common Mistakes

- making the global layer carry repo-local paths
- forcing collaboration on trivial edits
- allowing both agents to co-edit the same file without an explicit handoff rule
- pretending a challenge pass happened when the challenger timed out
- adding too much automation before the repo has a clear written workflow
