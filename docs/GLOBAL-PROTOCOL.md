# Global Protocol

This document defines the repo-agnostic collaboration contract for Claude Code, Codex CLI, and Gemini CLI.

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

## Cross-Session Coordination (Peer Inbox)

When two or more sessions are live on the same machine — e.g. one session
editing the backend and another editing the frontend — they can exchange
messages via a labeled peer inbox. This is complementary to lead/challenger;
the difference is that both peers are already contextful, so fidelity is
high and bloat stays low (each peer answers from its own live context;
only distilled answers cross the wire).

### Identity and discovery

Each session registers once with a human-meaningful label scoped to its
canonical repo root:

```bash
agent-collab session register --label backend --agent claude --role lead
agent-collab session register --label frontend --agent codex --role peer
```

Sessions are identified by `(canonical-cwd, label)`. Subdir invocations
resolve to the session's root via a walk-parents marker discovery.
Worktrees and symlinks resolve to distinct canonical paths, so labels
never collide across worktrees of the same repo.

`agent-collab peer list` shows active peers in the caller's repo. Activity
is derived from `last_seen_at`: active (< 5 min), idle (5 min – 24 hr), stale
(> 24 hr; excluded from list by default, sends return `peer offline`).

### Message exchange

```bash
agent-collab peer send --to <label> --message "<text>"
agent-collab peer receive [--mark-read]
```

Claims under `--mark-read` are atomic: `UPDATE ... RETURNING` in a single
`BEGIN IMMEDIATE` transaction against a WAL-mode SQLite file. Two parallel
receivers cannot claim the same message.

### Per-runtime hook status

| Runtime | Auto-inject on turn start | Install |
|---|---|---|
| Claude Code | yes, via `UserPromptSubmit` hook emitting `hookSpecificOutput.additionalContext` JSON | automated by `install-global-protocol` |
| Gemini CLI | yes, via `BeforeAgent` hook (same script) | **manual in v1**, automated in v1.1 |
| Codex CLI | no documented turn-start hook as of codex-cli 0.115.0 | explicit `peer receive` at turn start, or optional session-start wrapper |

### Bloat discipline

- Per-message cap: 8 KB (rejected beyond).
- Per-turn hook injection cap: 4 KB (overflow replaced with hint).
- Hook fails open on any error — a broken inbox never blocks a turn.
  Failures log to `~/.agent-collab/hook.log`.
- V1 retains all messages in the inbox indefinitely; no archive table
  until v2 pins retention + retrieval semantics.

### Lead/challenger applicability

The two-round cap, evidence-first disagreement rules, and escalation still
apply when lead and challenger coordinate via the inbox. A `peer send`
carrying a review acts like a challenge pass; the lead records it and
applies the same accept/reject logic.

### Fit: replace, extend, or complement?

The peer inbox does not replace `agent-collab challenge`. Stateless
challenger passes (fresh perspective on a bounded scope) remain the right
tool for most reviews. The peer inbox is the right tool when two
already-contextful sessions need to merge work without losing what each
has accumulated.

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

## Global Subprocess Rules

When Codex calls Claude in subprocess mode, pipe the prompt through stdin instead of passing the full prompt only as a positional argument, and apply a hard timeout with the timeout backend available on that machine (`timeout` or `gtimeout`).

Recommended pattern:

```bash
printf '%s' "$prompt" | claude -p --permission-mode plan --output-format text
```

This avoids a known subprocess hang pattern where Claude blocks on stdin.

When Claude calls Codex, use the repo-local preferred invocation. If a read-only challenge mode exists, prefer it.

When calling Gemini from another agent, pipe the prompt through stdin with the `-p` flag for headless mode, set `--approval-mode plan` for read-only review, and apply a hard timeout.

Recommended pattern:

```bash
printf '%s' "$prompt" | gemini -p "" --approval-mode plan -o text
```

Gemini writes to stdout. Use shell redirection to capture the output to a review file.

## Manual Trigger Patterns

If a repo has a local collaboration helper, use it.

If it does not, the lead may use a thin global runner such as `agent-collab` or trigger collaboration directly.

Example global runner shapes:

```bash
agent-collab challenge --challenger codex --scope plans/<scope>.md
agent-collab verify --challenger claude --scope plans/<scope>.md --context <changed-file>
```

```bash
agent-collab challenge --challenger gemini --scope plans/<scope>.md
agent-collab verify --challenger gemini --scope plans/<scope>.md --context <changed-file>
```

The global runner should remain thin:
- safe subprocess invocation
- prompt shaping
- review file creation
- explicit failure handling
- explicit subprocess deadlines
- when concrete review files are already known, it may inline those file contents into the prompt instead of forcing the challenger to read them via tools

It should not become the place where repo-specific plan paths, ownership rules, or test commands live.

Claude leading:

```bash
prompt='Read the relevant scope docs and reply in markdown only. Do not modify files. Focus on risks, regressions, simpler alternatives, and missing tests.'
review_file="$(mktemp "${TMPDIR:-/tmp}/codex-review.XXXXXX.md")"
printf '%s' "$prompt" | timeout -k 5s 300 codex exec -C "$PWD" -s read-only -o "$review_file" -
```

Codex leading:

```bash
prompt='Read the relevant scope docs and reply in markdown only. Do not modify files. Focus on risks, regressions, simpler alternatives, and missing tests.'
review_file="$(mktemp "${TMPDIR:-/tmp}/claude-review.XXXXXX.md")"
printf '%s' "$prompt" | timeout -k 5s 300 claude -p --permission-mode plan --output-format text --tools "Read" --effort low > "$review_file"
```

Gemini as challenger:

```bash
prompt='Read the relevant scope docs and reply in markdown only. Do not modify files. Focus on risks, regressions, simpler alternatives, and missing tests.'
review_file="$(mktemp "${TMPDIR:-/tmp}/gemini-review.XXXXXX.md")"
printf '%s' "$prompt" | timeout -k 5s 300 gemini -p "" --approval-mode plan -o text > "$review_file"
```

If the lead already has a concrete set of files to review, prefer inlining those file contents into the prompt for Claude challenge passes rather than sending only path names. That avoids a slower repo/tool bootstrap path and is less likely to hit external timeouts. Repo-local wrappers should deduplicate overlapping file inputs before inlining them. If the wrapper also restricts Claude to read-only tool access for that bounded review, prefer enabling `Read` only when the inline prompt budget truncates or omits file contents, and keep a configurable low-effort default unless the repo needs deeper review.

On macOS without GNU coreutils, replace `timeout` with `gtimeout` in the manual examples.
The global runner can fall back to `python3` internally when neither binary is available, but the manual shell examples assume a timeout command exists.

The lead should append or summarize the resulting review inside the repo-local plan or decision record.
These command shapes are defaults, not standards. Prefer repo-local wrappers when they exist, and adjust for the installed CLI version when needed.
This repository last verified them with Claude Code `2.1.78`, `codex-cli 0.115.0`, and `@google/gemini-cli 0.38.1`.

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
- Gemini is a strong default for fast, independent review passes

Swap roles when the task favors it.

Role selection heuristics:
- prefer Claude as lead for ambiguous planning, broad orchestration, or documentation-heavy work
- prefer Codex as lead for bounded implementation, CLI/tooling work, or verification-heavy changes
- prefer Gemini as challenger for fast review passes or when a third perspective adds signal
- do not open nested challenge loops from inside a challenge pass; finish the current round first, then let the lead decide whether a new scope is needed

## Human Tie-Breaker

The human owner is the final arbiter when:
- the agents cannot converge
- the repo-local toolchain is broken
- the review evidence is conflicting
- product tradeoffs are subjective rather than technical
