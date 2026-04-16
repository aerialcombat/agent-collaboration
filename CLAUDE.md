# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Purpose

This repo defines a **global collaboration protocol** for Claude Code, Codex CLI, and Gemini CLI. It separates global collaboration behavior (roles, review rules, escalation) from repo-local implementation details (paths, helpers, commands). The output is rendered instruction files installed into `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, and `~/.gemini/GEMINI.md`.

## Repository Structure

- `AGENT-COLLABORATION.md` — This repo's local collaboration guide for using the global runner against this project itself.
- `.agent-collab.env` — Data-only local defaults for `agent-collab` in this repo.
- `docs/GLOBAL-PROTOCOL.md` — The canonical protocol specification: roles (lead/challenger), trigger rules, synchronous vs parallel paths, review focus areas, evidence rules, round limits, escalation, and subprocess invocation patterns.
- `templates/CLAUDE.md` — Template for global Claude Code instructions. Contains `{{PROJECT_ROOT}}` placeholder.
- `templates/AGENTS.md` — Template for global Codex CLI instructions. Contains `{{PROJECT_ROOT}}` placeholder.
- `templates/GEMINI.md` — Template for global Gemini CLI instructions. Contains `{{PROJECT_ROOT}}` placeholder.
- `templates/AGENT-COLLABORATION.md` — Starter template for a consuming repo's local collaboration guide.
- `scripts/install-global-protocol` — Bash installer that renders templates (replacing `{{PROJECT_ROOT}}`) and writes them to `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, and `~/.gemini/GEMINI.md`, backing up existing files with timestamps.
- `scripts/doctor-global-protocol` — Validates install state and the CLI capability assumptions used by the global protocol.
- `docs/LOCAL-INTEGRATION.md` — Guide for adding the repo-local layer in consuming repositories.

## Install

```bash
./scripts/install-global-protocol
./scripts/doctor-global-protocol
```

## Architecture: Two-Layer Design

**Global layer** (this repo): Defines behavior — lead/challenger model, evidence-first disagreement, escalation after two rounds, subprocess rules, review expectations.

**Repo-local layer** (each consuming repo): Defines implementation — plan file paths, helper commands, event logs, ownership tables, verification commands. Repo-local docs may extend but must not weaken global rules.

This repo also self-hosts a thin local layer so the global runner can be used against the protocol project itself.

## Key Protocol Concepts

- **Asymmetric collaboration**: One lead (coordinates, decides), one challenger (reviews adversarially). Not peer-to-peer co-editing.
- **Evidence rule**: Neither agent accepts the other's position without support from code, tool output, tests, or repo rules.
- **Two-round max**: If agents don't converge after two challenge rounds, escalate to human.
- **Failure honesty**: If challenger doesn't return, record that fact — never invent review content.
- **Subprocess rule for Codex→Claude**: Pipe prompt via stdin, apply a hard timeout, and prefer the same read-only Claude flags the runner uses so subprocess review cannot hang indefinitely.
- **Subprocess rule for Gemini**: Pipe prompt via stdin with `-p ""` flag, use `--approval-mode plan` for read-only review, capture stdout to review file.

## Template Variable

Templates use `{{PROJECT_ROOT}}` which the install script replaces with the absolute path to this repo via `sed`.
