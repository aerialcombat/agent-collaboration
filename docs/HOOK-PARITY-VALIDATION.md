# Hook-Parity End-to-End Validation Protocol

**Purpose:** validate that the unified `hooks/peer-inbox-inject.sh` (Option J,
v3.x) actually surfaces peer-inbox messages in live Codex CLI and Gemini CLI
sessions — not just that the script produces correct output when fed fixture
JSON on stdin.

**Why this exists:** the CI-level gate `tests/hook-parity.sh` validates our
adapter code: same fixture in → same envelope out. It does **not** validate
that Codex or Gemini actually invoke the hook at the documented lifecycle point
and merge `hookSpecificOutput.additionalContext` into the agent's context
window on the next turn. That runtime-level verification requires actual CLIs,
actual model invocations, and human supervision to confirm the agent sees the
probe text in its own view.

This is a one-off smoke-test at ship time, not a persistent regression gate.
Re-run whenever Option J is modified, a CLI version bumps, or a CLI's
hook contract changes.

## Prerequisites

- `agent-collab` helper on `$PATH` (verify: `command -v agent-collab`)
- `scripts/install-global-protocol` already run so the hook is registered in
  `~/.claude/settings.json`, `~/.codex/hooks.json`, and
  `~/.gemini/settings.json`. Verify:
  ```bash
  bash scripts/doctor-global-protocol
  ```
  Expect three green pass lines for `peer-inbox hook registered in
  {Claude,Codex,Gemini} hook config`. If any says "not registered," re-run the
  installer before proceeding.
- Claude Code, Codex CLI, and Gemini CLI each installed and logged in
- Three terminals available (one per CLI). Owner spins these; test-engineer
  drives probes from the Claude terminal (or a fourth "driver" Claude).

## Probe matrix

Each probe sends a unique known-text string from one agent, then a human
confirms the recipient agent saw that exact string on its next turn. The
unique string lets the recipient unambiguously distinguish the probe from
unrelated context.

### Probe A — Claude → Codex (one-way pre-turn injection)

**Setup:**
```bash
# Terminal 1: Claude Code session
cd /tmp/hook-parity-probe-a-claude
agent-collab session register --label claude-sender --agent claude

# Terminal 2: Codex CLI session
cd /tmp/hook-parity-probe-a-codex
agent-collab session register --label codex-recv --agent codex
```

Pair both sessions under the same pair key via `--pair-key` or by running
`agent-collab peer list` and using the auto-generated key.

**Send:**
From Claude terminal:
```bash
agent-collab peer send --to codex-recv --message "probe-A-claude-to-codex-$(uuidgen | cut -c1-8)"
```
Record the exact UUID-suffixed string sent.

**Verify:**
In Codex terminal, type any prompt (e.g., `what's in the peer inbox?`). On the
next turn after submission, the Codex agent should report seeing a
`<peer-inbox>` block containing the exact probe string. If the Codex agent
reports "no peer messages" or never mentions the probe string, probe A fails.

**Pass criteria:** Codex agent explicitly quotes the exact probe string in its
response.

**Fail modes + diagnosis:**
- Codex never mentions the string → hook not firing in Codex's
  `UserPromptSubmit` lifecycle. Check `~/.codex/hooks.json` has the hook
  registered; check `~/.codex/config.toml` has `[features] codex_hooks = true`;
  verify via `codex --version` that the running Codex version supports hooks.
- Codex mentions a stale message → inbox has pre-existing state. Reset:
  `agent-collab peer reset --pair-key <key>`.
- JSON parse errors in Codex logs (`~/.codex/log/`) → hook is emitting
  malformed output; check `~/.agent-collab/hook.log` for Python/Go errors.

### Probe B — Claude → Gemini (one-way pre-turn injection)

Same shape as Probe A, with `gemini` substituted for `codex`. Verifies Gemini
CLI's `BeforeAgent` hook fires and surfaces `additionalContext` into the
agent's next turn.

**Setup / Send / Verify:** parallel to Probe A with `--label gemini-recv
--agent gemini` and probe string `"probe-B-claude-to-gemini-<uuid>"`.

**Fail modes unique to Gemini:**
- Gemini never mentions the string → hook not firing in Gemini's `BeforeAgent`
  lifecycle. Check `~/.gemini/settings.json` has the hook block. Note that
  gemini-peer R4a confirmed `/bin/sh -c` subprocess spawn + stdin JSON on live
  Gemini, so the shell-contract side should be solid. If failing, check
  `gemini --version` matches the `TESTED_GEMINI_VERSION` in
  `scripts/doctor-global-protocol`.

### Probe C — Bidirectional non-Claude (Codex ↔ Gemini)

Validates that the unified hook works when Claude is not the sender — i.e., a
Codex or Gemini session can originate a peer-inbox message and the recipient
(the other non-Claude CLI) surfaces it.

**Setup:** Codex session + Gemini session, paired under the same key. No
Claude session required for this probe.

**Send Codex → Gemini:**
In Codex terminal, via Codex's own MCP-tool call or `!` bash escape:
```bash
agent-collab peer send --to gemini-recv --message "probe-C-codex-to-gemini-<uuid>"
```

**Verify in Gemini terminal:** next prompt should see the probe string.

**Send Gemini → Codex:** reverse direction, same shape. Use probe string
`"probe-C-gemini-to-codex-<uuid>"`.

**Pass criteria:** both directions deliver within one turn of the next prompt.

**Why this matters:** the fixture-based test
(`tests/hook-parity.sh`) feeds stdin-level fixtures under Claude-configured
env. Probe C exercises the live Codex/Gemini stdin schema in both the sender
(via their peer-inbox tool call or shell escape) and the receiver (via their
own hook). This is the only probe that catches per-CLI stdin-schema drift
independent of Claude runtime.

### Probe D — Idempotency across repeated prompts

Validates that a delivered message isn't re-delivered on every subsequent
prompt. Tests the mtime-marker + per-session ack-file short-circuit across
all three CLIs.

**Setup:** one of the recipients from Probe A or B (either Codex or Gemini,
both shapes should pass).

**Steps:**
1. Send a probe via `agent-collab peer send --to <label> --message "probe-D-$(uuidgen)"`.
2. In the recipient terminal, submit any prompt. Verify the probe is surfaced
   in context (same verification as Probe A/B).
3. In the same recipient terminal, submit another unrelated prompt. The
   `<peer-inbox>` block should NOT reappear — the ack-file has been touched
   and the mtime-marker fast-path short-circuits.
4. Submit a third prompt with yet another body. `<peer-inbox>` should still
   not reappear.

**Pass criteria:** `<peer-inbox>` appears exactly once across the three
prompts, on the first prompt after the send.

**Fail mode:** `<peer-inbox>` reappears on prompts 2 or 3 → ack-file
short-circuit not firing. Check:
- `ls ~/.agent-collab/hook-ack/` — expect a file per `(cwd, session_key)` pair
- `stat ~/.agent-collab/inbox-dirty` — marker should be older than the
  ack-file after the first prompt
- Check `~/.agent-collab/hook.log` for ack-file-touch failures

## Running the protocol

1. Run `tests/hook-parity.sh` first — the fixture-based CI gate passes before
   investing in live-CLI probes:
   ```bash
   bash tests/hook-parity.sh
   ```
   Expect `PASS: hook-parity — ...` at the end. If this fails, fix the hook
   script before running probes.
2. Run `scripts/doctor-global-protocol` — all three CLI hook-registration
   checks should be green.
3. Execute probes A, B, C, D in order. Each probe is independent; a failure
   in one does not invalidate the others, but A/B failing is a blocker for
   Option J closure while C is arguably a v3.1 B1-scope concern (non-Claude
   env-var naming drift — see `hooks/peer-inbox-inject.sh:84`).
4. Report results as a table (CLI × probe → pass/fail/skip) in the review-
   round closure broadcast.

## What probes do NOT validate

- **Mid-turn push (notifications pushed while agent is mid-turn):** out of
  scope for Option J. Claude-only feature today; Codex blocked on upstream
  issue #18056; Gemini structurally blocked per #3052. See
  `plans/v3.x-push-asymmetry-research.md` §8 for the scoping decision.
- **Hook firing under `claude -p` / `codex exec` / `gemini -p` non-interactive
  mode:** partially verified by S1 research (Claude yes, Codex + Gemini
  unresolved). Out of scope for Option J review round; relevant for the
  Headless Delegation brainstorm (v3.1+ candidate work).
- **Idle-birch Codex-idiom preference (hook vs MCP tool-call):** Track C
  §8.6 #4 amendment protocol still standing. If idle-birch surfaces and
  prefers Option C over Option J for Codex, these probes need re-running
  after re-scope.

## Cleanup after probes

```bash
agent-collab peer reset --pair-key <probe-pair-key>
agent-collab session close    # in each terminal
rm -rf /tmp/hook-parity-probe-*-*
```

## References

- `hooks/peer-inbox-inject.sh` — unified hook script under test
- `tests/fixtures/hook-stdin/{claude,codex,gemini}.json` — canonical stdin
  fixtures per CLI, consumed by `tests/hook-parity.sh`
- `tests/hook-parity.sh` — shape-2 CI regression gate
- `scripts/doctor-global-protocol` — installation sanity checks
- `plans/v3.x-push-asymmetry-research.md` — scoping + synthesis for Option J
- `plans/v3.x-option-j-scoping.md` — Option J deliverables
