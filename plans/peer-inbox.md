# Peer Inbox — Cross-Session Agent Collaboration

**Status:** draft v3 (post round-2 challenge — implementation-ready) · **Owner:** DJ · **Lead:** Claude · **Challenger:** Codex

## Changelog

- **v3** — Responds to round-2 Codex challenge
  (`.agent-collab/reviews/peer-inbox-challenge-02.md`). Sync-ups for partials
  (1, 3, 4, 5, 7): interface sections match the resolver + transaction +
  archive-policy contracts; "prepends"/"reflexive poll" language purged.
  New majors resolved: python3 stated as hard prereq (removes "no new
  runtime deps" claim); hook emits JSON via python3, drops `jq` entirely
  and adds fail-open logging to `~/.agent-collab/hook.log`; doctor checks
  **python-linked** sqlite version (`python3 -c "import sqlite3;
  print(sqlite3.sqlite_version)"`), not machine sqlite3; Gemini auto-install
  cut from v1 and moved to documented manual path (v1.1 once exact config
  shape is pinned); archive table deferred entirely — v1 keeps all messages
  in inbox (storage is trivial, audit via raw DB).
- **v2** — Responds to round-1 Codex challenge
  (`.agent-collab/reviews/peer-inbox-challenge-01.md`). Accepted all 7
  findings.

## Goal

Let two (or more) Claude Code, Codex CLI, or Gemini CLI sessions collaborate
while each retains its own accumulated context.

Today, when session A (backend) and session B (frontend) need to merge work,
the user writes a handoff document from one session and loads it in the other.
This is lossy, one-directional, and can't be iterated.

This plan adds an append-only peer inbox shared between live sessions, plus
bash subcommands and a hook that auto-injects unread messages. Each agent
answers peer questions from its own live context window — so fidelity is high
and bloat is low (messages are kilobytes, not megabytes; extraction happens
inside the contextful agent).

## Non-goals (explicit out-of-scope for v1)

- **Synchronous peer-ask** with blocking round-trips. V1 is async at turn
  boundaries. A future `peer_ask` can layer on top.
- **Cross-machine / networked inbox.** V1 is stdio + local SQLite on one host.
- **Auto-label suggestion** from branch or cwd. V1 requires explicit labels.
- **MCP server wrapper.** V1 is bash dispatch + python3 DB helper only. A thin
  optional MCP server exposing the same SQLite as tools is a v2 consideration.
- **Pushing into an active turn.** Process isolation makes this impossible
  without Anthropic runtime changes. V1 delivers at next-turn boundary.

## Design

### Storage — one SQLite file

Path: `~/.agent-collab/sessions.db`. Created on first `session register`.
Ownership: user's home, mode `0600` on the file and `0700` on the directory.
Configurable via `AGENT_COLLAB_INBOX_DB`.

**All DB access goes through a python3 helper** (`scripts/peer-inbox-db.py`)
using the stdlib `sqlite3` module, not the `sqlite3` CLI. Reasons:

- True parameter binding. `sqlite3` CLI's `.parameter set` treats values as
  SQL expressions before falling back to quoted text — unsafe for
  attacker-controlled strings (message bodies, labels).
- Stable concurrency contract (see below) — WAL, busy_timeout, transaction
  modes applied consistently per call.
- Already-blessed dependency: `scripts/agent-collab` uses inline python3 for
  its timeout fallback (lines 348-424). No new external dep.

Bash subcommands dispatch to the python helper with argv flags; the helper
does the SQL and returns exit codes + formatted output.

```sql
CREATE TABLE sessions (
  cwd           TEXT NOT NULL,
  label         TEXT NOT NULL,
  agent         TEXT NOT NULL,           -- claude | codex | gemini
  role          TEXT,                    -- lead | challenger | peer | reviewer
  started_at    TEXT NOT NULL,           -- ISO-8601 UTC
  last_seen_at  TEXT NOT NULL,
  PRIMARY KEY (cwd, label)
);

CREATE TABLE inbox (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  to_cwd        TEXT NOT NULL,
  to_label      TEXT NOT NULL,
  from_cwd      TEXT NOT NULL,
  from_label    TEXT NOT NULL,
  body          TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  read_at       TEXT                     -- NULL = unread
);

CREATE INDEX idx_inbox_to_unread  ON inbox(to_cwd, to_label, read_at);
CREATE INDEX idx_sessions_last_seen ON sessions(last_seen_at);
```

**Why `(cwd, label)` as the identity**, not a UUID:

- Labels are human-meaningful addressing. Agents and users should never read
  UUIDs.
- Scope is naturally project-local — `backend` in `dj/` is a distinct row from
  `backend` in some other repo.
- No CLI-specific session ID to depend on. Works identically across Claude,
  Codex, Gemini.
- Re-registering the same (cwd, label) is idempotent — it refreshes
  `last_seen_at` instead of creating a duplicate row.

**Timestamps are ISO-8601 text** (SQLite convention, bash-friendly via
`date -u +%Y-%m-%dT%H:%M:%SZ`). All comparisons are lexicographic.

### Concurrency contract

Two (or more) shells may hit the DB simultaneously. To prevent races and
duplicate delivery:

1. **WAL mode set at DB create**, once: `PRAGMA journal_mode=WAL`. WAL
   permits concurrent readers and one writer; writers serialize.
2. **Every connection sets `PRAGMA busy_timeout=5000`** (5s). Default busy
   handler is NULL, so without this lock contention returns SQLITE_BUSY
   immediately. 5s covers realistic concurrent turn overlap; longer is
   pathological.
3. **Writes run under `BEGIN IMMEDIATE`** — acquires the reserved lock up
   front rather than escalating mid-transaction. Prevents the
   `SQLITE_BUSY_SNAPSHOT` upgrade failure mode.
4. **`peer receive --mark-read` uses atomic claim-and-mark in one
   transaction** via `UPDATE inbox SET read_at = ? WHERE id IN (...)
   RETURNING id, to_cwd, to_label, from_cwd, from_label, body, created_at`.
   The RETURNING clause guarantees no other reader can grab the same row
   between select and mark. (Requires SQLite ≥ 3.35.0, released 2021-03-12;
   the doctor script version-gates.)
5. **Two-process hammer test** in smoke.sh: fork N=10 concurrent
   `peer send` + `peer receive --mark-read` pairs, assert no duplicate
   delivery, no dropped messages, no SQLITE_BUSY errors leak to callers.

### Self-identity across Bash tool calls

Problem: each `Bash` tool call is a fresh shell. Environment variables don't
persist across calls, so the session's self-label can't live in an env var
*set by register*. But runtimes like Claude Code export their own session
identity to every subprocess, so we can use that as the discriminator.

Solution:

1. **Single `resolve_cwd()` function** used by every subcommand. Resolves
   `$PWD` to absolute path with symlinks resolved. Newlines in cwd are
   rejected. Never use raw `$PWD` for DB operations.
2. **Per-session marker files** in `<canonical-root>/.agent-collab/sessions/`:
   one file per registered session named `<sha256-of-session-key[:16]>.json`
   containing `{"cwd": "<canonical-root>", "label": "<label>", "session_key":
   "<key>"}`. Keying markers by session key — not by label — means two
   sessions in the same repo (the canonical use case: backend + frontend
   both in `dj/`) don't collide.
3. **Session key discovery** on every invocation (for register and for
   self-resolution), in priority order:
   1. Explicit `--session-key <key>` arg, or `--as <label>` override which
      skips session-key resolution entirely.
   2. `$AGENT_COLLAB_SESSION_KEY` env var (user-controlled).
   3. `$CLAUDE_SESSION_ID`, `$CODEX_SESSION_ID`, `$GEMINI_SESSION_ID` — the
      runtime's own session identifier if it exports one.
   4. If none: single-marker convenience (if exactly one marker exists
      under the walk-parents-found sessions dir, use it). Else error.
4. **Walk-parents discovery** from current canonical cwd upward looking for
   `.agent-collab/sessions/<hash>.json` where hash is derived from the
   current session key. Works from any subdirectory.
5. **Claude's `UserPromptSubmit` hook** reads `session_id` from the hook
   stdin JSON (per Anthropic's hook contract) and exports it as
   `CLAUDE_SESSION_ID` before calling `agent-collab`. No user action needed
   on the Claude side.
6. **Codex and Gemini (v1)** rely on explicit `--session-key <k>` or
   `$AGENT_COLLAB_SESSION_KEY` until their runtime session IDs are wired up
   via hooks (Gemini v1.1).
7. **`--as <label>` overrides** marker discovery entirely.
8. **`--to-cwd` defaults to the caller's canonical root**, not raw `$PWD`.

```
# Session A in the same repo:
$ CLAUDE_SESSION_ID=session-A-uuid agent-collab session register --label backend --agent claude
registered: backend (claude, peer) at /Users/deeJ/Development/dj [session_key=b93d6d111c27fa16]

# Session B in the same repo (different terminal):
$ CLAUDE_SESSION_ID=session-B-uuid agent-collab session register --label frontend --agent codex
registered: frontend (codex, peer) at /Users/deeJ/Development/dj [session_key=740c3f47e3d48a01]

$ ls /Users/deeJ/Development/dj/.agent-collab/sessions/
740c3f47e3d48a01.json   b93d6d111c27fa16.json

# From any subdir, hook or session-key env resolves self:
$ cd platform/agents
$ CLAUDE_SESSION_ID=session-A-uuid agent-collab peer send --to frontend --message "auth TTL is 15m now"
sent to frontend
```

Worktree behavior: each git worktree is a distinct canonical path, so
labels don't collide across worktrees of the same repo.

### Subcommand surface

Bash dispatch in `scripts/agent-collab` delegates all DB work to
`scripts/peer-inbox-db.py`. **python3 (≥ 3.9) is a hard prerequisite** for
peer-inbox functionality. It was already required by `agent-collab`'s
timeout fallback (`scripts/agent-collab:348-424`), so this is not a new
system-level dep — but it is newly *mandatory* for peer-inbox commands
rather than optional. The doctor script gates both the python3 presence
and its linked sqlite library version.

| Subcommand | Purpose | Writes |
|---|---|---|
| `session register --label <l> --agent <a> [--role <r>]` | Claim label for canonical cwd, upsert row. | DB + `<canonical-root>/.agent-collab/my-label` (JSON) |
| `session close [--label <l>]` | Remove row and marker. | DB |
| `session list [--all-cwds]` | Show active sessions for this cwd (default). | none |
| `peer send --to <label> [--as <label>] --message <text>` | Append to peer's inbox. | DB |
| `peer receive [--as <label>] [--format hook\|plain\|json] [--mark-read]` | Read unread messages addressed to me. | DB (if `--mark-read`) |
| `peer list [--as <label>]` | Show peers in my cwd with activity state. | none |

Heartbeat: every subcommand bumps `last_seen_at` for the caller.

Activity state derived from `last_seen_at`:

- **active**: < 5 min
- **idle**: 5–30 min
- **stale**: > 30 min (excluded from `peer list`, sends return
  `peer offline: <label>`)

### Message format

Each inbox row stores a UTF-8 body. Sender can include structured payloads
(e.g., a JSON snippet) inside the body; the inbox is payload-agnostic. Body is
limited to 8 KB per message (enforced server-side). Overflow is a caller
error — split the message or attach a file path.

Rendered format from `peer receive --format hook`:

```
<peer-inbox messages="2" from-session-labels="frontend,infra">
[frontend @ 2026-04-17T15:02:11Z]
auth token TTL is 15m now — make sure backend refresh path handles it

[infra @ 2026-04-17T15:04:33Z]
staging db migrated. you're clear to deploy.
</peer-inbox>
```

The `<peer-inbox>` wrapper is explicit so agents can tell peer messages apart
from the user's prompt. The format is stable; document it in templates so
agents know what they're seeing.

### Bloat discipline

Four caps, all enforced by the bash subcommand so the agent can't accidentally
blow the budget:

1. **Per-message cap**: 8 KB body. Rejected with a clear error if exceeded.
2. **Per-turn hook cap**: 4 KB total across all injected messages. Overflow
   replaced with `[+N more messages truncated; run agent-collab peer receive
   to view]`.
3. **V1 inbox retention**: unbounded. Messages stay in `inbox` with a
   non-null `read_at` once read. Storage is trivial (kilobytes per
   message, most interactions are a few messages). Auditing is straight
   SQL against `inbox`. V2 may introduce an archive table with a pinned
   retention + retrieval spec; v1 avoids the complexity.
4. **Stale session silencing**: sends to a session not seen in > 30 min
   return `peer offline`, preventing one-way shouting into the void.

### Hooks — per-runtime

Three runtimes have three surfaces. Contract is "inject unread peer messages
as context at turn start"; implementation varies.

#### Claude Code — `UserPromptSubmit` hook

Claude's hook contract (per [Anthropic hooks docs](https://code.claude.com/docs/en/hooks)):
- Hook receives JSON on stdin.
- Preferred output: JSON on stdout with
  `hookSpecificOutput.additionalContext` — this is the normative structured
  channel for injecting context.
- Exit-0 stdout text is also added as context (fallback contract), but JSON
  `additionalContext` is the documented normative path.

The hook script is minimal. The python3 helper emits the full JSON
envelope directly (via `--format hook-json`) — the hook itself is mostly
plumbing for stdin parsing + fail-open logging. No `jq` dependency.

```bash
#!/usr/bin/env bash
# ~/.agent-collab/hooks/peer-inbox-inject.sh
set -uo pipefail
LOG="${AGENT_COLLAB_HOOK_LOG:-$HOME/.agent-collab/hook.log}"
log() { printf '[%s] peer-inbox-inject: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "$LOG" 2>/dev/null || true; }
trap 'rc=$?; [[ $rc -ne 0 ]] && log "failed at line $LINENO exit $rc"; exit 0' ERR EXIT

command -v agent-collab >/dev/null 2>&1 || { log "agent-collab not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { log "python3 not on PATH"; exit 0; }

# Claude provides hook metadata as JSON on stdin; extract session_id and
# export as CLAUDE_SESSION_ID so self-resolution picks the right marker.
hook_stdin="$(cat 2>/dev/null || true)"
if [[ -n "$hook_stdin" ]]; then
  extracted="$(HOOK_STDIN="$hook_stdin" python3 -c 'import json,os,sys
try: print(json.loads(os.environ.get("HOOK_STDIN","")).get("session_id",""))
except Exception: pass' 2>/dev/null || true)"
  [[ -n "$extracted" ]] && export CLAUDE_SESSION_ID="$extracted"
fi

if ! output="$(agent-collab peer receive --format hook-json --mark-read 2>>"$LOG")"; then
  log "peer receive failed"; exit 0
fi
[[ -n "$output" ]] && printf '%s' "$output"
exit 0
```

All failures log to `~/.agent-collab/hook.log`; every exit path is 0.

Installed via `~/.claude/settings.json` as a managed block:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "~/.agent-collab/hooks/peer-inbox-inject.sh" }
        ]
      }
    ]
  }
}
```

`scripts/install-global-protocol` (a) copies the hook script to
`~/.agent-collab/hooks/`, (b) adds the managed JSON block to
`~/.claude/settings.json` idempotently with backup. The managed block
references **only** the installed home-directory path — never the
repo-checkout path.

#### Gemini CLI — manual install in v1 (automated in v1.1)

Gemini CLI documents `BeforeAgent` and `SessionStart` hooks
(ref [geminicli.com/docs/hooks](https://geminicli.com/docs/hooks/)). The
same hook script works there — it's shell-only with no Gemini-specific
syntax. But the exact Gemini config file path and schema have not been
verified against the installed CLI version yet, so **v1 ships manual
install instructions in `templates/GEMINI.md` only**. The installer does
not touch Gemini config.

v1.1 adds automated Gemini hook installation once the config shape is
pinned and covered by doctor checks. This is a deliberate scope cut to
ship v1 without a placeholder automation step.

#### Codex CLI — wrapper-driven polling

Codex does not have a documented automatic turn-start hook. `AGENTS.md` is
instructional context, not a lifecycle event. Two fallback paths:

1. **Wrapper-driven**: wrap the user's `codex` invocation with a tiny shell
   wrapper (`agent-collab codex-wrap ...`) that runs `peer receive` once at
   session start and prepends results to the prompt. Not per-turn, but
   better than nothing.
2. **Manual**: document in `templates/AGENTS.md` that when working in a
   cross-session context, the user can explicitly prompt
   "*check peer inbox*" and the agent runs `peer receive`. Honest about
   the limitation.

V1 documents the manual path and ships the optional wrapper as
`scripts/agent-collab-codex-wrap`. Do not promise automatic per-turn
polling. If Codex later grows a hook surface, a follow-up plan adds it.

### Discovery flow

1. User opens two terminals, starts `claude` in each.
2. First prompt in session A: *"this is the backend session, register"*
   → agent runs `agent-collab session register --label backend --agent claude --role lead`.
3. First prompt in session B: same with `--label frontend`.
4. Either agent can run `agent-collab peer list` to see the other.
5. Messages flow via `peer send`. On each UserPromptSubmit in Claude, the
   hook pulls unread messages and adds them to Claude's context via
   `hookSpecificOutput.additionalContext`. On Gemini, the equivalent
   behavior is wired to `BeforeAgent` via manual install (v1) or the
   installer (v1.1).

No UUIDs ever surface to the user or agents.

## Interfaces in detail

### `session register`

```
agent-collab session register --label <label>
                              --agent {claude|codex|gemini}
                              [--role <role>]
                              [--cwd <path>]          # defaults to resolved canonical $PWD
                              [--session-key <key>]   # else env: AGENT_COLLAB_SESSION_KEY,
                                                      #   CLAUDE_SESSION_ID, CODEX_SESSION_ID,
                                                      #   GEMINI_SESSION_ID (first set wins)
                              [--force]               # override active-label collision
```

Behavior:

- Resolves `cwd` via the canonical resolver (`$PWD` → absolute, symlinks
  resolved) and records current ISO-8601 UTC time.
- Validates label against allowlist `[a-z0-9][a-z0-9_-]{0,63}`.
- Upserts `sessions` row on `(canonical_cwd, label)`. If label already
  claimed by another active session (< 5 min) and `--force` not set:
  error, suggest `session close` or a different label.
- Writes `<canonical_cwd>/.agent-collab/my-label` as JSON:
  `{"cwd":"<canonical_cwd>","label":"<label>"}`.
- Adds `.agent-collab/my-label` to `.gitignore` if a `.gitignore` exists
  and entry is absent (idempotent).
- Prints confirmation including the canonical cwd.

Exit codes: 0 success, 1 label collision, 2 config error, 3 validation error.

### `peer send`

```
agent-collab peer send --to <label>
                       [--as <label>]          # defaults to marker label via walk-parents discovery
                       [--to-cwd <path>]       # defaults to caller's canonical cwd (resolver)
                       --message <text>
                       [--message-file <path>] # alternative to --message
```

Behavior:

- Resolves caller's canonical cwd via the single `resolve_cwd()` function.
- Resolves self-label from `--as`, else reads marker JSON found by walking
  parent directories from the canonical cwd. If marker's recorded `cwd`
  doesn't match the resolved cwd (path drift), error with clear diagnostic.
- Default `--to-cwd` is the caller's canonical cwd (same resolver). Never
  raw `$PWD`.
- Looks up target by `(canonical_to_cwd, to_label)`. If row missing: error.
  If target's `last_seen_at` > 30 min: error `peer offline: <label>`.
- Validates body size (≤ 8 KB). Rejects otherwise.
- Inserts `inbox` row using a parameterized prepared statement.
- Bumps sender's `last_seen_at` under `BEGIN IMMEDIATE`.

### `peer receive`

```
agent-collab peer receive [--as <label>]
                          [--format hook|plain|json]
                          [--mark-read]
                          [--since <iso8601>]
```

Behavior:

- Resolves self via canonical cwd + marker discovery.
- **Without `--mark-read`**: plain SELECT of unread (or `--since`-filtered)
  messages addressed to self. Read-only; safe to call repeatedly.
- **With `--mark-read`**: single atomic claim-and-mark transaction under
  `BEGIN IMMEDIATE`:
  ```sql
  UPDATE inbox
  SET read_at = ?
  WHERE to_cwd = ? AND to_label = ? AND read_at IS NULL
  RETURNING id, from_cwd, from_label, body, created_at;
  ```
  The RETURNING clause yields only rows this call claimed. No other
  receiver can see those same rows as unread — `UPDATE…RETURNING` under
  `BEGIN IMMEDIATE` eliminates the select-then-mark race. There is no
  separate SELECT step.
- Emits in requested format. `hook` produces the `<peer-inbox>…</peer-inbox>`
  block capped at 4 KB (overflow replaced with hint). `plain` is
  human-readable. `json` is a machine-readable array.
- **V1 does not archive or delete messages.** Read messages stay in
  `inbox` with a non-null `read_at`. Storage is trivial and this avoids
  introducing an archive lifecycle before it's needed. Archiving is a v2
  consideration with an explicit retention + retrieval spec.
- Bumps self's `last_seen_at` under the same transaction.

### `peer list`

```
agent-collab peer list [--as <label>] [--include-stale] [--json]
```

Behavior:

- Returns other sessions in the same cwd (self excluded). Stale (> 30 min)
  filtered unless `--include-stale`.
- Columns: label, agent, role, activity (active/idle/stale), last_seen_at.

## Integration touch-points

| File | Change |
|---|---|
| `scripts/agent-collab` | Extend MODE dispatch at lines 680-689; add `session` and `peer` subcommand handlers that dispatch to `peer-inbox-db.py`. Extend config loader (lines 126-177) with `AGENT_COLLAB_INBOX_DB` key. ~200 lines added. |
| `scripts/peer-inbox-db.py` (new) | Python3 stdlib helper for all DB ops. Uses sqlite3 module with parameter binding; applies WAL + busy_timeout + BEGIN IMMEDIATE; implements atomic claim-and-mark via `UPDATE…RETURNING`. ~300 lines. |
| `scripts/install-global-protocol` | Install `hooks/peer-inbox-inject.sh` to `~/.agent-collab/hooks/`; add managed hook block to `~/.claude/settings.json` (idempotent, backed up). Gemini install deferred to v1.1 — v1 documents manual install in `templates/GEMINI.md`. Support `--uninstall` removal for Claude only in v1. |
| `scripts/doctor-global-protocol` | New checks: python3 ≥ 3.9 on PATH; **python-linked** SQLite version ≥ 3.35.0 via `python3 -c "import sqlite3; print(sqlite3.sqlite_version)"` (the python build's linked library, not the machine `sqlite3` CLI); inbox DB path writable; hook script executable and readable; hook registered in Claude `~/.claude/settings.json`. Gemini install check added in v1.1. |
| `hooks/peer-inbox-inject.sh` (new) | Bash wrapper emitting Claude's `hookSpecificOutput.additionalContext` JSON via inline python3 (no jq dep). Fail-open with logging to `~/.agent-collab/hook.log`. |
| `scripts/agent-collab-codex-wrap` (new, optional) | Wrapper around `codex exec` that runs `peer receive` once and prepends results to the prompt. Session-start only, not per-turn. |
| `templates/CLAUDE.md` | Add "Cross-Session Coordination" section: register, how the hook injects peer messages, when to use `peer send`. |
| `templates/AGENTS.md` | Same, but document Codex as **manual or wrapper-driven** — no automatic polling promise. |
| `templates/GEMINI.md` | Same as CLAUDE.md but includes a manual-install snippet for the `BeforeAgent` hook (v1). Automated install lands in v1.1. |
| `docs/GLOBAL-PROTOCOL.md` | New "Cross-Session Coordination" section after "Parallel Path" (~line 86). Covers inbox semantics, labels, bloat discipline, lead/challenger applicability, per-runtime hook status. |
| `docs/LOCAL-INTEGRATION.md` | Add `.agent-collab/my-label` marker and inbox DB to the "may also provide" list at line 21-27. |
| `.agent-collab.env` | Document optional `AGENT_COLLAB_INBOX_DB` override. |
| `tests/smoke.sh` | Add two-session round-trip, label collision, stale expiry, hook JSON format, bloat cap, two-process contention hammer, symlink/subdir identity resolution, malicious message content (quotes, newlines, semicolons, `readfile('/etc/passwd')`, Unicode), archive-read-only policy. |
| `AGENT-COLLABORATION.md` (this repo) | Add peer-inbox to list of surfaces that trigger collaboration on changes. |

## Protocol fit — lead/challenger still applies

The peer inbox doesn't replace the lead/challenger protocol. It lets two
already-contextful sessions use lead/challenger with *less context loss*.

Today's flow:

> Claude (lead) → `agent-collab challenge --challenger codex --scope plans/X.md`
> Codex (challenger, fresh) reads the scope file and responds.

New flow (Claude ↔ Gemini, or Claude ↔ Claude):

> Claude (lead in session A) → `peer send --to reviewer --message "here's
> scope plans/X.md — challenge focus on rollback risk"`
> Gemini (challenger in session B, already contextful on repo) receives
> via `BeforeAgent` hook → responds with `peer send --to backend
> --message "..."`
> Claude reads reply on next turn via `UserPromptSubmit` hook.

For Claude ↔ Codex, the Codex side uses `agent-collab-codex-wrap` at
session start or the user explicitly prompts `check peer inbox` mid-session.
Automatic per-turn polling is not promised for Codex in v1.

Two-round cap still applies. Still escalate to user on disagreement. Still
record reviews as artifacts.

## Security and safety

- **DB file mode `0600`, directory `0700`.** Enforced by the python helper
  on first create. Prevents other local users from reading inbox.
- **True parameterized SQL via python3 stdlib `sqlite3` module.** All user
  input (labels, message bodies, cwd paths) is passed as `?` parameters
  to `cursor.execute()`. No string concatenation, no shell interpolation,
  no `.parameter set` CLI mechanism (which is not a safe binding surface).
- **Hook JSON emitted by the python helper** (`--format hook-json`) via
  stdlib `json.dump` which correctly escapes control characters, quotes,
  and multibyte UTF-8. The hook bash script itself only reads stdin,
  extracts `session_id`, and prints the helper's output.
- **Destructive operations never cascade.** A session close removes only
  its own row; it does not delete inbox rows sent to/from it.
- **No network.** SQLite file is local. Cross-machine work is a separate
  explicit plan.
- **Adversarial input test cases** in smoke.sh: messages containing `'`,
  `"`, `;`, `--`, newlines, `readfile('/etc/passwd')`, 4-byte UTF-8, and
  `<script>`-style tokens. V1 message bodies are UTF-8 text only; raw
  binary / `\0` is out of scope. cwd containing newline/CR is rejected
  at the resolver. Labels outside `[a-z0-9][a-z0-9_-]{0,63}` are rejected.

## Testing plan

1. **Unit-style (smoke.sh additions)**:
   - register → marker JSON written with canonical cwd + label, DB row
     present with canonical cwd
   - register collision (< 5 min) → error with clear message
   - `--force` re-register → old marker replaced, DB row updated
   - send → inbox row with correct (to, from, body)
   - send > 8 KB → rejected with size error
   - receive with no messages → empty output, exit 0
   - receive with messages → hook JSON matches Anthropic
     `hookSpecificOutput.additionalContext` schema
   - `--mark-read` → subsequent receive is empty
   - v1 retention: all messages (read and unread) stay in `inbox`; no
     archive table exists; direct SQL over `inbox` is the audit surface
   - stale session send (> 30 min inactive peer) → `peer offline` error
   - peer list excludes self; includes other active and idle; excludes stale

2. **Concurrency hammer** (smoke.sh additions):
   - Fork N=10 concurrent `peer send` calls to same recipient → all 10
     messages present, unique IDs, no SQLITE_BUSY leaks.
   - Fork N=5 concurrent `peer receive --mark-read` calls on same inbox
     with 20 pending messages → each message delivered exactly once, no
     duplicates, no gaps.
   - Fork N=10 concurrent writes interleaved with reads → WAL keeps
     readers unblocked; writes serialize within the 5s busy_timeout.

3. **Identity resolution** (smoke.sh additions):
   - Register at `/path/to/repo`; call `peer send` from
     `/path/to/repo/sub/dir` → walks parents, finds marker, uses canonical
     cwd.
   - Register at canonical path; call from symlinked path
     `/link -> /path/to/repo` → canonical resolver produces same cwd.
   - Register in git worktree `/path/to/repo-wt-a` → distinct canonical
     cwd from main repo, labels don't collide.
   - Corrupt marker file (non-JSON) → clear error, no silent fallback.
   - Marker recorded cwd doesn't match resolved cwd → error (path drift
     detected).

4. **Adversarial inputs** (smoke.sh additions):
   - Message containing `'; DROP TABLE inbox; --` → stored verbatim,
     retrieved verbatim, no SQL executed.
   - Message containing `readfile('/etc/passwd')` → stored verbatim, never
     evaluated.
   - Message containing newlines, 4-byte UTF-8, `<script>` → round-trip
     byte-identical. **V1 message bodies are UTF-8 text only.** Raw binary
     / `\0` payloads are out of scope — `--message-file` reads via
     `Path.read_text()` and rejects invalid UTF-8. A future `--binary`
     flag can base64-encode if needed.
   - Label containing `../../etc/passwd` → rejected by label validator
     (allowlist: `[a-z0-9][a-z0-9_-]{0,63}`).
   - cwd containing a newline → canonical resolver rejects with
     EXIT_VALIDATION.

5. **Two-session integration** (manual, documented):
   - Open two terminals in `dj/`. Register `backend` and `frontend`.
   - `backend` sends message to `frontend`.
   - In `frontend`, trigger a UserPromptSubmit — verify hook emits JSON
     with `hookSpecificOutput.additionalContext` containing the
     `<peer-inbox>` block, and that Claude sees it.
   - `frontend` responds via `peer send`.
   - `backend` next turn: hook fires, message visible.

6. **Cross-agent** (post-v1 verification):
   - One session Claude, one Gemini. Both register via their respective
     hook installs, exchange messages. Verify Gemini `BeforeAgent` hook
     injects peer messages.
   - One session Claude, one Codex. User manually prompts Codex to
     `peer receive` mid-session (or uses `agent-collab-codex-wrap`).
     Document that automatic per-turn polling is not promised.

## Alternatives considered and rejected

- **Python + FastMCP server for v1.** Rejected: FastMCP is a pip dep the
  repo otherwise doesn't have. Stdlib `sqlite3` via python3 is already
  present; python3 is already used by `agent-collab`'s timeout fallback
  (`scripts/agent-collab:348-424`). V1 uses only what's already blessed.
  MCP server is an optional v2 additive layer.
- **Maildir-style filesystem queue** instead of SQLite. Considered for
  concurrency simplicity — one file per message, atomic rename for
  delivery, no locking. Rejected because labeled addressing, activity
  queries, and label collision checks still require a registry, which
  brings back DB or JSON-file locking. SQLite with the explicit
  concurrency contract (WAL + busy_timeout + `UPDATE…RETURNING`) is
  cleaner than maintaining two coordination mechanisms.
- **sqlite3 CLI with `.parameter set`.** Rejected after round-1 review:
  `.parameter set VALUE` evaluates SQL expressions before falling back to
  quoted text, so it is not safe parameter binding for attacker-controlled
  strings. Python3 stdlib `sqlite3` prepared statements are true binding.
- **Full JSONL transcript mining** (`~/.claude/projects/*.jsonl` scan).
  Rejected as primary: 75 MB worst-case, noisy, Claude-only. Useful as a
  separate v2 `peer transcript --from <label>` subcommand. Not in v1.
- **Auto-label from branch/cwd.** Rejected: intent doesn't map reliably to
  filesystem state. Explicit labels force intentional naming.
- **Shared event log (append-only JSONL) instead of inbox.** Close second.
  Inbox chosen because it has clear recipient semantics. Could add
  broadcast as `peer send --to '*'` later.
- **Session UUIDs as primary key.** Rejected: adds indirection with no
  benefit. `(canonical-cwd, label)` is sufficient for single-machine v1.
- **Hook emits plain text stdout instead of JSON.** Rejected after round-1
  review: Anthropic docs treat stdout as the fallback path and JSON
  `hookSpecificOutput.additionalContext` as the normative structured
  channel. Using JSON is more durable and explicit.

## Verification commands

- `bash -n scripts/agent-collab` — syntax check after edits
- `./scripts/doctor-global-protocol` — checks sqlite3, DB path, hook install
- `bash tests/smoke.sh` — existing suite plus new cases
- manual two-session test per "Testing plan" section 2

## Implementation prerequisites (resolved in v2)

- **Claude Code hook contract**: confirmed. Hook receives JSON on stdin;
  emits `hookSpecificOutput.additionalContext` JSON on stdout for
  normative context injection (fallback: plain stdout). Source:
  [code.claude.com/docs/en/hooks](https://code.claude.com/docs/en/hooks).
- **Gemini hook contract**: `BeforeAgent` and `SessionStart` hooks exist.
  Source: [geminicli.com/docs/hooks](https://geminicli.com/docs/hooks/).
  Exact config path/schema not yet verified — v1 ships manual install in
  `templates/GEMINI.md`; v1.1 automates after pinning.
- **Codex hook contract**: no documented automatic turn-start hook as of
  codex-cli 0.115.0. Plan documents manual/wrapper paths honestly.
- **SQLite version floor**: `UPDATE…RETURNING` requires SQLite ≥ 3.35.0
  (2021-03-12). The gate checks **python3's linked sqlite library**
  (`python3 -c "import sqlite3; print(sqlite3.sqlite_version)"`), not the
  machine `sqlite3` CLI, because the helper runs via python.
- **python3 ≥ 3.9**: hard prerequisite for peer-inbox. Already required by
  `agent-collab`'s timeout fallback. Doctor checks.
- **No `jq` dependency.** Hook emits JSON via inline python3. Removing jq
  eliminates an install step and a silent-failure surface.

## Rollout

Single branch on `agent-collaboration` repo. Merge sequence:

1. Plan merged to main (this document).
2. Codex challenge pass on plan — iterate up to 2 rounds.
3. Implementation: subcommands → hook → templates → docs → tests, in that
   order, committed incrementally.
4. Codex verify pass on implementation.
5. Reinstall via `./scripts/install-global-protocol` to activate the hook.
6. Two-session smoke test in `dj/`.
7. Document rollout in this repo's AGENT-COLLABORATION.md section.

## Resolved design questions (post round-1)

1. **Identity**: `(canonical-cwd, label)` with symlink resolution and
   walk-parents marker discovery. Worktrees naturally isolate via distinct
   canonical paths.
2. **Hook cap**: 4 KB. Keeps compounding turns below ~16 KB/hour worst-case.
   Can relax if real-world use shows under-budget.
3. **Read semantics**: `peer receive` default is read-only (must opt in with
   `--mark-read`). The hook always uses `--mark-read` so unread truly means
   unseen. Manual `peer receive` lets user inspect without marking.
4. **Archive vs. retain**: v1 retains everything in `inbox` (read or
   unread) indefinitely. Storage is trivial; audit is direct SQL. V2
   adds an archive table with pinned retention + retrieval spec if a
   real need emerges.
5. **Hook failure mode**: fail open, degrade silently. A broken inbox must
   not block the user's turn. Failures log to `~/.agent-collab/hook.log`
   for diagnosis.
6. **Stale threshold**: 30 min for the default "online" window is fine
   for active co-editing. Overnight async is handled by the unread-never-
   archived policy — senders see `peer offline` and can choose to
   `--force-send` to queue the message anyway (future flag, not v1).
