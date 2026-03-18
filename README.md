# Agent Collaboration

Global collaboration protocol for Claude Code and Codex CLI.

This project separates:
- global collaboration behavior
- repo-local implementation details

The goal is to make the two agents behave consistently across repos without forcing every repo to use the same file paths or helper scripts.

## What This Project Contains

- `docs/GLOBAL-PROTOCOL.md`
  Generic protocol: roles, trigger rules, rounds, escalation, challenge behavior, and subprocess rules.
- `templates/CLAUDE.md`
  Global Claude Code instructions template.
- `templates/AGENTS.md`
  Global Codex CLI instructions template.
- `templates/AGENT-COLLABORATION.md`
  Starter repo-local collaboration guide for consuming repositories.
- `scripts/install-global-protocol`
  Installs rendered templates into `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, and a global `agent-collab` command.
- `scripts/agent-collab`
  Thin global runner for `challenge` and `verify` passes with safe Claude/Codex invocation defaults.
- `scripts/doctor-global-protocol`
  Checks the install state and the local Claude/Codex CLI assumptions used by the protocol.
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
```

That file is data-only. The command parses the supported keys and does not source arbitrary shell.

Manual fallback:

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

## Install

```bash
cd ~/Development/agent-collaboration
./scripts/install-global-protocol
```

The installer:
- creates `~/.claude/` and `~/.codex/` if needed
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

- whether the global Claude and Codex files exist
- whether they contain the managed block from this project
- whether the referenced protocol doc still exists
- whether the global `agent-collab` command is installed and on `PATH`
- whether `claude` and `codex` are installed
- whether the CLI help output still supports the manual trigger assumptions in this protocol

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

## Important Rule

The two agents should never “collaborate” by pretending agreement exists when it does not.

If the challenger does not return, the lead must record that fact and proceed conservatively instead of inventing review output.
