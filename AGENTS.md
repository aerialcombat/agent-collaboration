# Repository Guidelines

## Project Structure & Module Organization

This repository defines a global collaboration layer, not repo-local implementation details.

- `docs/GLOBAL-PROTOCOL.md`: canonical protocol for roles, evidence rules, escalation, and subprocess behavior.
- `AGENT-COLLABORATION.md`: this repo's local collaboration guide.
- `.agent-collab.env`: local defaults for the global runner in this repo.
- `templates/CLAUDE.md`, `templates/AGENTS.md`, `templates/GEMINI.md`, and `templates/AGENT-COLLABORATION.md`: starter templates for global and repo-local docs.
- `scripts/agent-collab`: global runner for `challenge` and `verify` passes.
- `scripts/install-global-protocol`: installs templates and the global command.
- `scripts/doctor-global-protocol`: validates install state and CLI assumptions.
- `tests/smoke.sh`: end-to-end shell smoke test for install, doctor, runner dry-run, and uninstall.
- `README.md`: project overview and install guidance.
- `docs/LOCAL-INTEGRATION.md`: guidance for implementing the local layer in a consuming repository.

Keep global behavior in `docs/` and `templates/`. Do not add repo-specific paths or dashboards here.

## Build, Test, and Development Commands

- `./scripts/install-global-protocol`: install rendered templates plus the global `agent-collab` command.
- `./scripts/agent-collab help`: inspect subcommands and repo-local config keys.
- `./scripts/doctor-global-protocol`: validate the global install state.
- `bash tests/smoke.sh`: run the end-to-end smoke test in a temporary HOME.
- `bash -n scripts/install-global-protocol`: syntax-check the installer.
- `bash -n scripts/agent-collab`: syntax-check the global runner before committing shell changes.
- `bash -n scripts/doctor-global-protocol`: syntax-check the doctor script.
- `sed -n '1,160p' docs/GLOBAL-PROTOCOL.md`: quickly review the authoritative protocol text while editing templates or README content.

There is no compile step or packaged application in this repo.

## Coding Style & Naming Conventions

Use concise Markdown with ATX headings (`##`) and short bullet lists. Match the existing tone: direct and repo-agnostic.

For Bash:

- keep `#!/usr/bin/env bash` and `set -euo pipefail`
- use two-space indentation inside functions
- prefer uppercase variable names for paths and derived constants
- quote expansions, for example `"$PROJECT_ROOT"`

Name scripts in kebab-case (example: `install-global-protocol`). Keep top-level protocol files uppercase where they represent canonical docs (`AGENTS.md`, `CLAUDE.md`, `GLOBAL-PROTOCOL.md`).

## Testing Guidelines

This snapshot does not include an automated test suite. Validate changes with targeted manual checks:

- run `bash -n` on edited shell scripts
- execute `./scripts/install-global-protocol` and confirm backup and install output
- execute `./scripts/agent-collab challenge --challenger claude --prompt "dry run" --dry-run`
- execute `./scripts/doctor-global-protocol` and confirm the expected PASS/WARN/BLOCK summary
- execute `bash tests/smoke.sh`
- verify rendered files replace `{{PROJECT_ROOT}}` correctly

## Commit & Pull Request Guidelines

Git history is not available in this checkout, so use short imperative commit subjects such as `docs: clarify challenge escalation` or `scripts: preserve backup behavior`.

Pull requests should include:

- a brief problem statement and the affected files
- manual verification steps and results
- sample output or screenshots when installer behavior or rendered docs change
- linked issues or follow-up work when applicable
