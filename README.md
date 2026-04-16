# Agent Collaboration

Global collaboration protocol for Claude Code, Codex CLI, and Gemini CLI.

This project separates:
- global collaboration behavior
- repo-local implementation details

The goal is to make the three agents behave consistently across repos without forcing every repo to use the same file paths or helper scripts.

## What This Project Contains

- `docs/GLOBAL-PROTOCOL.md`
  Generic protocol: roles, trigger rules, rounds, escalation, challenge behavior, and subprocess rules.
- `templates/CLAUDE.md`
  Global Claude Code instructions template.
- `templates/AGENTS.md`
  Global Codex CLI instructions template.
- `templates/GEMINI.md`
  Global Gemini CLI instructions template.
- `templates/AGENT-COLLABORATION.md`
  Starter repo-local collaboration guide for consuming repositories.
- `scripts/install-global-protocol`
  Installs rendered templates into `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, `~/.gemini/GEMINI.md`, and a global `agent-collab` command.
- `scripts/agent-collab`
  Thin global runner for `challenge` and `verify` passes with safe Claude/Codex/Gemini invocation defaults. It applies hard subprocess timeouts, deduplicates overlapping guide/scope/context paths, and for Claude review passes it inlines the provided files into the prompt and only enables read-only file access when the inline budget truncates or omits content.
- `scripts/doctor-global-protocol`
  Checks the install state and the local Claude/Codex/Gemini CLI assumptions used by the protocol.
- `docs/LOCAL-INTEGRATION.md`
  Explains how consuming repositories should implement their local layer.

## Design

This project defines the global layer only.

Global layer:
- lead / challenger model
- evidence-first disagreement rules
- escalation rules
- synchronous vs parallel collaboration patterns
- subprocess invocation rules

Repo-local layer:
- actual plan file paths
- helper commands
- event logs and dashboards
- repo-specific verification commands
- repo-specific ownership splits

Global rules should guide repo behavior, but repo-local docs should define the concrete implementation.

## Triggering Collaboration

Use the collaboration protocol by default for:
- non-trivial implementation work
- risky refactors
- release or compliance checks
- architecture or contract changes
- tasks where an adversarial second opinion is valuable

Do not force collaboration for:
- trivial mechanical edits
- isolated formatting work
- obvious one-file fixes with low regression risk
- routine command execution where challenge adds no value

When a repo provides a local helper, use that.

When a repo does not provide one, the lead can use the global `agent-collab` runner or trigger collaboration manually.

Global runner examples:

```bash
agent-collab challenge \
  --challenger codex \
  --scope plans/auth-rollout.md \
  --prompt "Focus on rollback risk and missing tests."

agent-collab verify \
  --challenger claude \
  --scope plans/auth-rollout.md \
  --context app/auth.py \
  --context tests/test_auth.py
```

The runner looks for an optional repo-local `.agent-collab.env` file with values such as:

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

That file is data-only. The command parses the supported keys and does not source arbitrary shell.

For Claude challenge and verify passes, `agent-collab` reviews the contents of the files you pass with `--guide`, `--scope`, and `--context` directly. That is intentional: path-only prompts were noticeably slower and more likely to hit timeouts because Claude had to spend the review pass reading the repo on its own. The Claude runner deduplicates overlapping paths before inlining them, and it only enables read-only `Read` tool access when the inline prompt budget truncates or omits file contents. Claude effort remains configurable and defaults to `low`.

The runner enforces a hard timeout internally. It prefers `timeout`, falls back to `gtimeout` or `python3` when needed, and can be tuned with repo config or `--timeout-seconds`.

Manual fallback:

Use the canonical manual trigger patterns in [docs/GLOBAL-PROTOCOL.md](docs/GLOBAL-PROTOCOL.md#manual-trigger-patterns). That document is the single source of truth for raw Claude/Codex/Gemini shell invocation shapes.

## Install

```bash
cd ~/Development/agent-collaboration
./scripts/install-global-protocol
```

The installer:
- creates `~/.claude/`, `~/.codex/`, and `~/.gemini/` if needed
- installs `agent-collab` to `~/.local/bin/` by default
- preserves non-managed content and only updates a marked collaboration block
- backs up target files before changing them
- validates required templates before writing
- supports `--uninstall` to remove the managed block

Set `AGENT_COLLAB_BIN_DIR` before running the installer if you want a different command location.

After installation, repo-local docs should point back to this project when they implement a concrete workflow.
If this project moves, rerun the installer so the rendered protocol reference path stays correct.

## Doctor

```bash
cd ~/Development/agent-collaboration
./scripts/doctor-global-protocol
```

The doctor script checks:

- whether the global Claude, Codex, and Gemini files exist
- whether they contain the managed block from this project
- whether the referenced protocol doc still exists
- whether the global `agent-collab` command is installed and on `PATH`
- whether `claude`, `codex`, and `gemini` are installed
- whether each CLI's help output still supports the manual trigger assumptions in this protocol

The last tested CLI baseline is surfaced by `./scripts/doctor-global-protocol` and referenced in [docs/GLOBAL-PROTOCOL.md](docs/GLOBAL-PROTOCOL.md).
If your installed versions differ, rerun the doctor for surface-compatibility checks and manually revalidate the trigger patterns before treating warnings as false positives.

## Smoke Test

```bash
cd ~/Development/agent-collaboration
bash tests/smoke.sh
```

The smoke test exercises:

- install into a temporary `HOME`
- preservation of existing global files
- doctor against the temporary install
- `agent-collab` dry-run in a sample repo
- uninstall cleanup of the managed block and command

## Suggested Repo Integration

Each repo should still have its own implementation doc, for example:
- `CLAUDE.md`
- `AGENTS.md`
- `AGENT-COLLABORATION.md`

Those repo docs should:
- link back to the global protocol when helpful
- define repo-specific paths and helper commands
- not weaken the global safety rules

See [docs/LOCAL-INTEGRATION.md](docs/LOCAL-INTEGRATION.md) for the recommended local layer and [templates/AGENT-COLLABORATION.md](templates/AGENT-COLLABORATION.md) for a starter local guide.

## License

This project is licensed under the [MIT License](LICENSE).

## Important Rule

The agents should never “collaborate” by pretending agreement exists when it does not.

If the challenger does not return, the lead must record that fact and proceed conservatively instead of inventing review output.
