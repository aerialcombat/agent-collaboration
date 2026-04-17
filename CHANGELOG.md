# Changelog

All notable changes to the peer-inbox subsystem. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); dates are UTC.

Commit SHAs reference the `agent-collaboration` repo.

---

## v1.7 — 2026-04-17

**One-sentence install: `install agent-collab` → `/agent-collab` → `/peer`.**

### Added
- `sessions.pair_key` column with a unique `(pair_key, label)` index.
  Pair keys scope peer resolution across cwds so two sessions in
  different directories can share a room. (`c582092`)
- `session register --pair-key KEY` (join) and `--new-pair` (mint a
  fresh slug). `peer send` and `peer list` automatically scope by
  `pair_key` when the caller has one set.
- `peer send --to` is inferred when exactly one live peer is in scope.
  Ambiguous or empty scopes error with the candidate list. (`fb001dc`)
- `--label` is optional on register; a memorable `adjective-noun`
  slug is generated and printed. Pair-key slugs use
  `adjective-noun-XXXX` from a 128×128 wordlist. (`fce583b`)
- Slash commands `/agent-collab` (interactive register) and `/peer`
  (send/check/list/end) drop into `~/.claude/commands/`, idempotent
  with backup. (`5db6644`)
- Skill `install-agent-collab` drops into `~/.claude/skills/` so the
  user can literally type *install agent-collab*. Skill performs
  clone → install → verify. (`875ac9b`)

### Changed
- Installer adds/removes slash commands and skills alongside the
  existing peer-inbox helper files. `install-global-protocol install`
  is still a no-op when everything is current.

### Compatibility notes
- **Additive.** All existing cwd-scoped flows work unchanged. Pair
  keys only apply when explicitly opted into via `--pair-key` or
  `--new-pair`.
- **Cross-runtime.** Claude Code receives messages through the
  `UserPromptSubmit` hook. Codex and Gemini have no equivalent hook
  — those sessions must call `agent-collab peer receive` themselves
  each turn. The DB side is symmetric (`CLAUDE_SESSION_ID`,
  `CODEX_SESSION_ID`, `GEMINI_SESSION_ID` all work as session keys);
  only the push-to-agent side is Claude-only.
- **Same machine.** Pair keys still only coordinate sessions within
  one SQLite file on one host. Cross-machine is v2.0+.

### Smoke scenarios added
- Pair-key cross-cwd send/receive.
- Pair-key peer-list scope.
- Duplicate label rejection inside a pair.
- Auto-label shape + wordlist collision bound.
- Auto-infer `--to` single-peer + ambiguity.
- Cross-runtime env-var selection (`CLAUDE/CODEX_SESSION_ID`).

### Packaging
- **peer-inbox MCP server auto-registered at user scope** in
  `~/.claude.json` by `install-global-protocol`. Previously the
  MCP declaration had to live in each repo's `.mcp.json`, so
  `claude --dangerously-load-development-channels server:peer-inbox`
  only worked in repos that happened to have the entry. Now any
  cwd works. Removed on uninstall; existing `mcpServers` entries
  (e.g. `playwright`) are preserved.
- Install skill and `/agent-collab` slash command now document
  both delivery modes clearly: hook (default, zero-config,
  arrives on next prompt) and MCP channels (real-time mid-turn,
  requires the preview flag per session).

### Codex/Gemini zero-config
- `session register --agent codex|gemini` auto-mints a session key
  when no env var is available. Re-register in the same `(cwd,
  label)` reuses the stored key so idempotent refresh works. The
  Claude path is unchanged — it still uses the `UserPromptSubmit`
  hook log. Verified via real `codex exec`: register + send +
  receive round-trip with no `CODEX_SESSION_ID` set.
- Hardened channel-pairing lookup so codex/gemini sandboxes that
  block `ps` subprocess don't crash `session register`. Channel
  pairing silently reports `[channel: none]` when lookup fails.

---

## v1.5 — 2026-04-17 · [`d8b1139`]

**Live browser view.**

### Added
- `agent-collab peer web [--as <label>] [--port <n>]` — blocking HTTP
  server serving a dark-themed live chat log at
  `http://127.0.0.1:<port>`. Browser-side JS polls
  `/messages.json?after=<id>` every second and appends new rows.
  Auto-scroll with opt-out, "↓ new messages" button when scrolled up,
  title flash on arrival. Same pastel-pill styling as `peer replay`.
  Self-contained: inline HTML/CSS/JS, Python stdlib only.

### Changed
- Guide documents the new subcommand.
- Smoke test `tests/smoke.sh` gains a scenario that hits `/` and
  `/messages.json?after=0` and validates live delta polling.

---

## v1.4 — 2026-04-17 · [`fa46e94`]

**Real-time channel delivery + turn cap + termination.**

Fixes the "conversation ends at turn boundary" problem by wiring
Claude Code Channels for push-into-live-session delivery.

### Added
- `scripts/peer-inbox-channel.py` (330 lines, stdlib only) —
  stdio-MCP server advertising
  `capabilities.experimental['claude/channel']`. Per-session Unix
  socket at `/tmp/peer-inbox/<pid>.sock`; writes
  `~/.agent-collab/pending-channels/<claude-pid>.json` at startup for
  session-pairing discovery.
- `sessions.channel_socket` column and new `peer_pairs` table
  (cwd, a_label, b_label, turn_count, terminated_at, terminated_by).
  Idempotent migrations.
- `session register` walks its own process tree for a
  `pending-channels/<claude-pid>.json` file; when found, binds
  `channel_socket` on the session row. Reports `[channel: paired]`
  or `[channel: none]`.
- `peer send` checks pair termination + turn cap (default 20, env
  `AGENT_COLLAB_MAX_PAIR_TURNS`), detects `[[end]]` token in body
  and marks pair terminated. Then POSTs to recipient's channel
  socket if present; SQLite write is always authoritative.
- `agent-collab peer reset --to <label>` — clears termination +
  turn counter for the pair.

### Fixed
- AF_UNIX path length on macOS — default socket dir is `/tmp/peer-inbox`
  (short), with a runtime guard that errors before bind if the path
  exceeds ~100 chars.

### Changed
- Installer copies `peer-inbox-channel.py` to
  `~/.agent-collab/scripts/`. Uninstall removes it. Doctor unchanged.
- Guide documents the channels opt-in flow, `.mcp.json` shape,
  termination semantics, and bounding conventions.
- Smoke gains three new scenarios: `[[end]]` termination, max-turns
  cap, channel pairing + socket push.

---

## v1.3 — 2026-04-17 · [`9ea621a`]

**Observability: peer watch + peer replay.**

### Added
- `agent-collab peer watch [--as <label>] [--only-new] [--interval <s>]`
  — blocking live-tail of a label's inbox. Read-only (does not
  mark-read, so the hook still delivers at next turn). Prints each
  message with sender label, timestamp, read/unread marker.
- `agent-collab peer replay [--as <label>] [--since <iso>] [--out <path>]`
  — emits a self-contained HTML transcript. Inline CSS, deterministic
  pastel color per sender, day grouping, html-escaped bodies. Default
  output `<cwd>/.agent-collab/replay-<ts>.html`.

### Changed
- Guide has a new "Observability" section covering both.
- Smoke tests verify live-tail picks up mid-watch sends, and HTML
  parses cleanly.

---

## v1.2 — 2026-04-17 · [`fd1940a`]

**Hook dispatcher fix.**

### Fixed
- The hook was invoking `agent-collab hook-log-session` (hyphen) but
  the bash dispatcher routes as `<mode> <subcommand>` (space-separated).
  Fixed to `agent-collab hook log-session`. Verified with a real hook
  invocation; seen-session log populates and `peer receive` resolves
  from the logged session ID.

---

## v1.1 — 2026-04-17 · [`0ca3ea2`]

**Auto-resolve Claude session_id when runtime doesn't export it.**

### Context
Claude Code 2.1.78 does not export `$CLAUDE_SESSION_ID` to Bash-tool
subprocesses. Sessions were registering with random keys (from the
`AGENT_COLLAB_SESSION_KEY="$(date +%s)-$RANDOM"` template fallback),
which didn't match the real session_id the hook sees on stdin. Hook
couldn't resolve self-identity and errored with "multiple sessions
registered; pass --as".

### Added
- Hook calls `agent-collab hook log-session` on every invocation,
  appending `(timestamp, session_id)` to
  `~/.agent-collab/claude-sessions-seen/<cwd-hash>.log`. The hook
  already extracts `session_id` from stdin JSON and exports
  `CLAUDE_SESSION_ID`; the append-only log lets future
  `session register` calls find the session_id across fresh
  Bash-tool shells.
- `session register` falls back to the most recent unregistered
  session_id from the seen-log when no `--session-key` or env var
  is set. The error message points users at how to unblock (prompt
  first, then retry).
- `session adopt --label X --session-id Y` — re-key an existing
  registration to the real Claude session_id. Useful for reconciling
  sessions that registered before the hook logged their session_id.
- `agent-collab hook <subcommand>` top-level mode in the bash dispatcher.

### Changed
- Hook also uses the hook-reported `cwd` (from stdin JSON) when
  present, so `peer receive` targets the right repo scope even from
  odd working directories.

---

## v1.0 — 2026-04-17 · [`00151c0`]

**Initial peer-inbox release.**

### Added
- `scripts/peer-inbox-db.py` — python3 stdlib helper. SQLite with WAL
  and `busy_timeout=5000`; parameterized SQL throughout;
  `UPDATE … RETURNING` under `BEGIN IMMEDIATE` for atomic
  claim-and-mark. Canonical cwd resolver with newline rejection and
  path-drift detection. Per-session markers at
  `<cwd>/.agent-collab/sessions/<sha256-of-session-key>.json` keyed
  by `CLAUDE_SESSION_ID` / `CODEX_SESSION_ID` / `GEMINI_SESSION_ID` /
  `AGENT_COLLAB_SESSION_KEY` so two sessions in the same repo don't
  collide.
- `hooks/peer-inbox-inject.sh` — fail-open `UserPromptSubmit` hook.
  Extracts `session_id` from Claude stdin JSON and propagates as
  `CLAUDE_SESSION_ID` for the subsequent `peer receive` call. Emits
  `hookSpecificOutput.additionalContext` JSON envelope via the
  helper. Logs failures to `~/.agent-collab/hook.log`.
- `scripts/agent-collab` — new `session` and `peer` top-level
  subcommands dispatched to the python helper. Config loader reads
  `AGENT_COLLAB_INBOX_DB`. Usage text documents `--session-key`
  resolution order.
- `scripts/install-global-protocol` — surgically merges the hook
  block into `~/.claude/settings.json` (preserves existing matchers,
  idempotent reinstall, surgical uninstall removes only our entry).
  Installs helper + hook to `~/.agent-collab/`.
- `scripts/doctor-global-protocol` — blocks on `python3 < 3.9` and
  python-linked sqlite `< 3.35.0`; verifies hook install.
- `tests/smoke.sh` — 13 peer-inbox scenarios including the
  two-sessions-same-cwd canonical case, adversarial SQL content,
  15-way concurrent-send hammer, 5-way mark-read atomicity,
  walk-parents from subdirs, path drift, hook stdin JSON extraction.
- `docs/PEER-INBOX-GUIDE.md` — user-facing install + usage guide.
- `docs/GLOBAL-PROTOCOL.md` + `docs/LOCAL-INTEGRATION.md` —
  protocol-level "Cross-Session Coordination" section.
  `templates/{CLAUDE,AGENTS,GEMINI}.md` per-runtime session-key
  instructions.
- `plans/peer-inbox.md` — design doc v3 (two challenge rounds + two
  verify rounds per the collaboration protocol).

### Protocol trail

- **Round 1 challenge** (`.agent-collab/reviews/peer-inbox-challenge-01.md`):
  2 BLOCKING + 5 MAJOR, all addressed in plan v2.
- **Round 2 challenge** (`.../peer-inbox-challenge-02.md`): 2
  RESOLVED + 5 PARTIAL + 5 NEW MAJOR, addressed in plan v3.
- **Round 1 verify** (`.../peer-inbox-verify-01.md`): 2 BLOCKING +
  5 MAJOR + 1 MINOR + 3 PASS — triggered the major refactor (per-session
  markers keyed by session key, surgical install, lock-free reads,
  `AGENT_COLLAB_INBOX_DB` config wiring, python 3.9 gate, cwd
  newline rejection, hook stdin session_id extraction).
- **Round 2 verify** (`.../peer-inbox-verify-02.md`): 6 RESOLVED +
  2 PARTIAL + 5 NEW — partials addressed; `--session-key`
  documentation gap closed in bash usage + templates + plan.
