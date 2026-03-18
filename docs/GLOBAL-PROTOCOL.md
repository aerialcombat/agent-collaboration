# Global Protocol

This document defines the repo-agnostic collaboration contract for Claude Code and Codex CLI.

It does not define repo-specific plan paths, ownership rules, or verification commands.

## Principles

- One lead, one challenger.
- The lead is responsible for coordination.
- The challenger returns substantive review, not courtesy agreement.
- Final decisions must be backed by code, tests, tool output, or repo rules.
- If disagreement remains after the allowed rounds, escalate to the human owner.
- The default maximum is two challenge rounds.

## Roles

### Lead

The lead:
- defines the scope
- writes or updates the plan
- invokes the challenger
- decides whether feedback is accepted
- owns implementation or explicit work split

### Challenger

The challenger:
- reviews the plan or implementation
- tries to break weak reasoning
- returns text only unless a repo-local protocol explicitly assigns a bounded write scope
- does not recursively call the other agent during a challenge pass

The lead may capture the challenger's text output in a temp file or repo-local review artifact. That does not change the challenger's write scope.

## Default Collaboration Model

Use asymmetric collaboration by default:
- one lead
- one challenger
- one source of truth for the active decision

This is preferred over peer-to-peer co-editing.

## When To Trigger Collaboration

Use the protocol by default for:
- non-trivial implementation work
- architecture changes
- risky refactors
- release and compliance readiness work
- tasks where a second opinion could catch a costly mistake

Skip collaboration by default for:
- trivial or mechanical edits
- isolated formatting or copy changes
- low-risk one-file fixes where review adds no meaningful signal
- routine command execution without product or engineering judgment

When in doubt, bias toward collaboration for irreversible or user-facing changes.

Useful heuristics:
- if the change touches multiple modules, public contracts, auth, billing, launch, or safety boundaries, collaborate
- if the change is an obvious one-file fix with straightforward verification, solo work is usually fine
- if a failed decision would be expensive to unwind, collaborate

## Two Paths

### 1. Synchronous Path

Use this for most non-trivial work.

Flow:
1. Lead creates or updates the scope plan.
2. Lead invokes challenger.
3. Challenger returns review text.
4. Lead records the review and decision.
5. Lead implements.
6. Lead may invoke challenger again for verification.

The lead is the scheduler in this path.

### 2. Parallel Path

Use this only when:
- the write scopes are disjoint
- ownership is explicit
- the repo has a concrete local mechanism for coordination

Never do concurrent same-file editing by default.

## Review Rules

Challenge reviews should focus on:
- false confidence
- simpler alternatives
- regressions
- missing verification
- policy or requirement mismatch
- boundary conditions

The challenger should not soften a real objection just to reduce friction.

If no scope plan exists yet, the challenger should review the task brief, proposed approach, or changed files instead.

## Evidence Rule

Neither agent should accept the other agent's position without support from at least one of:
- code reality
- tool output
- test results
- explicit repo rules
- explicit external requirements when the task depends on them

The evidence must be relevant to the contested point. Token evidence or unrelated test output does not satisfy this rule.

## Maximum Rounds

Default maximum:
- two challenge rounds

If the agents still do not converge:
- stop
- summarize the disagreement
- escalate to the human owner

## Failure Handling

If the challenger does not return usable output:
- do not invent a review
- record that the challenge failed or timed out
- proceed conservatively only if the remaining work is low-risk, locally verifiable, and reversible
- otherwise stop and escalate

If the other agent or CLI is unavailable, treat that the same way as a failed challenge pass. Do not silently drop collaboration on a task that should have had review.

## Global Subprocess Rule

When Codex calls Claude in subprocess mode, pipe the prompt through stdin instead of passing the full prompt only as a positional argument.

Recommended pattern:

```bash
printf '%s' "$prompt" | claude -p --permission-mode plan --output-format text
```

This avoids a known subprocess hang pattern where Claude blocks on stdin.

When Claude calls Codex, use the repo-local preferred invocation. If a read-only challenge mode exists, prefer it.

## Manual Trigger Patterns

If a repo has a local collaboration helper, use it.

If it does not, the lead may use a thin global runner such as `agent-collab` or trigger collaboration directly.

Example global runner shapes:

```bash
agent-collab challenge --challenger codex --scope plans/<scope>.md
agent-collab verify --challenger claude --scope plans/<scope>.md --context <changed-file>
```

The global runner should remain thin:
- safe subprocess invocation
- prompt shaping
- review file creation
- explicit failure handling

It should not become the place where repo-specific plan paths, ownership rules, or test commands live.

Claude leading:

```bash
review_file="$(mktemp "${TMPDIR:-/tmp}/codex-review.XXXXXX.md")"
codex exec -C "$PWD" -s read-only -o "$review_file" \
  "Read the relevant scope docs and reply in markdown only. Do not modify files. Focus on risks, regressions, simpler alternatives, and missing tests."
```

Codex leading:

```bash
prompt='Read the relevant scope docs and reply in markdown only. Do not modify files. Focus on risks, regressions, simpler alternatives, and missing tests.'
review_file="$(mktemp "${TMPDIR:-/tmp}/claude-review.XXXXXX.md")"
printf '%s' "$prompt" | claude -p --permission-mode plan --output-format text > "$review_file"
```

The lead should append or summarize the resulting review inside the repo-local plan or decision record.
These command shapes are defaults, not standards. Prefer repo-local wrappers when they exist, and adjust for the installed CLI version when needed.

## Global Prompt Shape

Challenge prompts should usually say:
- read the scope plan
- reply in markdown only
- do not modify files
- focus on risks, alternatives, regressions, and missing tests

Verification prompts should usually say:
- read the scope plan and resulting changes
- reply in markdown only
- do not modify files
- focus on requirement mismatch, regressions, and missing verification

## Global vs Local Responsibility

Global protocol defines:
- behavior
- escalation
- review expectations
- safe subprocess rules

Repo-local docs define:
- concrete paths
- helper commands
- ownership format
- dashboards / logs
- test commands
- how scope files are archived

Repo-local implementations may extend the protocol, but they should not weaken the global rules.

## Recommended Defaults

When no repo-specific rule says otherwise:
- Claude is a strong default lead for planning and orchestration
- Codex is a strong default challenger for adversarial review and verification

Swap roles when the task favors it.

Role selection heuristics:
- prefer Claude as lead for ambiguous planning, broad orchestration, or documentation-heavy work
- prefer Codex as lead for bounded implementation, CLI/tooling work, or verification-heavy changes
- do not open nested challenge loops from inside a challenge pass; finish the current round first, then let the lead decide whether a new scope is needed

## Human Tie-Breaker

The human owner is the final arbiter when:
- the agents cannot converge
- the repo-local toolchain is broken
- the review evidence is conflicting
- product tradeoffs are subjective rather than technical
