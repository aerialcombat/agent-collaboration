# Post-v0.3 Handoff — Session State Snapshot

**Status:** Topic 3 v0.3 (pi-as-universal-harness collapse) shipped + empirically validated. v3.1 [PROMPT] patch shipped. v3.1 recommended-models table shipped. v3.1 roadmap brainstorm post-v0.3 update landed (7-lane reviewer aggregation + drift-check amendments). Room `fluffy-beaver-bc6f` has 5 active daemon peers running v0.3 SOFT SHIM or pi-native. Context-budget growing; owner requested fresh-start handoff. · **Session:** fluffy-beaver-bc6f (holding open)

Load this doc + referenced plan docs to resume. Full transcript not required.

## Required reading (in order)

1. `plans/post-topic-3-v0.1.2-handoff.md` — prior handoff (pre-v0.2). Resumes at v0.2 scoping-in-progress state.
2. `plans/post-topic-3-v0-handoff.md` — predecessor to that.
3. `plans/v3.x-topic-3-v0.2-pi-scoping.md` — v0.2 scope v2 (shipped through v0.2.1 correction at `b66476a`).
4. `plans/v3.x-topic-3-v0.3-collapse-scoping.md` — **v0.3 scope v2 at `304bbad`** (SOFT SHIM + Shape β WIDE + path-shape guard; all 5 reviewer-round findings absorbed).
5. `plans/v3.1-roadmap-brainstorm.md` — roadmap with new §14 "Post-Topic-3-collapse state" at `38e7244` + drift-check amendments at `7b5a7fb`.
6. `docs/DAEMON-OPERATOR-GUIDE.md` — operator-facing Arch D guide with pi-universal + v3.1 recommended-models table + v0.4 HARD RETIRE note.
7. `docs/DAEMON-CLI-SESSION-VALIDATION.md` — probe protocol; §E5-E10 with last-run annotations through v0.3 ship.

## Closure stack (tags on origin/main)

```
v3.x-option-j-shipped         (b5f9485)  Option J hook-delivery baseline
v3.x-topic-3-v0-shipped       (ab09f81)  Arch B auto-reply daemon + §3.4 contract
v3.x-topic-3-v0.1-shipped     (371609b)  Arch D code + fake-binary gates (NOT empirically functional)
v3.x-topic-3-v0.1.2-shipped   (6ec4b23)  Arch D EMPIRICALLY FUNCTIONAL (first real-CLI passing ship)
v3.x-topic-3-v0.2-shipped     (ab61c24)  pi as 4th Arch D CLI (path-as-identity)
v3.x-topic-3-v0.2.1-shipped   (b66476a)  zai-glm env-var plugin-blindness correction
v3.x-topic-3-v0.3-shipped     (4d65bf3)  pi-as-universal-harness collapse (codex/gemini → pi SOFT SHIM; -1690 net LoC)
```

## Since post-topic-3-v0.1.2 handoff — what happened

### Topic 3 v0.2 (pi as 4th Arch D CLI) — shipped

`5c13748..ab61c24`, 5 commits. Scope-doc v1 + v2 absorption of reviewer round (test-engineer + reason + gamma substantive). Commits 1 (feat: spawnPi + path-as-identity + Shape β agent-gate + file-delete reset) + 2 (docs: install template + §E8 probe). Ride-along `ab61c24` for §E8 Prerequisites HOME-isolation note. Post-tag `66ef703` E8 last-run annotation.

**Key empirical evidence:** E8 real-CLI probe 5/5 PASS against pi 0.67.68 + zai-glm/glm-4.6. Session file cross-spawn retention validated ("cornflower" → "cornflower").

### Topic 3 v0.2.1 (env-var plugin-blindness correction) — shipped

`b66476a` + `b5a3a7f`. Single doc-only + test-fixture commit correcting v2 absorption's incorrect "fix" of `ZAI_GLM_API_KEY → ZAI_API_KEY`. Root cause: `zai-glm` is a third-party pi plugin (`pi-zai-glm` v0.1.1 by ross-jill-ws), NOT a pi-mono built-in. Plugin reads `ZAI_GLM_API_KEY`; built-in `zai` provider reads `ZAI_API_KEY`. Verified at `/opt/homebrew/lib/node_modules/pi-zai-glm/extensions/zai_glm_provider.ts:12`.

**Meta-lesson captured:** plugin-provider env vars require reading the plugin's `registerProvider()` source, NOT `pi --help` alone. `pi --help` is blind to plugin providers.

### v3.1 [PROMPT] patch — shipped

`23da0c1` (feat+docs; 237 lines). Three envelope-prompt DEFECTs fixed:
- **DEFECT 1:** `buildStaticPrompt()` at `go/cmd/daemon/main.go:1247` had literal `<YOUR_CWD>` / `<YOUR_LABEL>` placeholders — LLMs had to guess. Fixed via `fmt.Sprintf` interpolation of `cfg.Label` + `cfg.CWD`.
- **DEFECT 2:** Mechanism-1 `peer-inbox daemon-complete` path was unreachable — binary was never installed. Retired mechanism-1 language from envelope prompt; mechanism-2 (stdout JSON marker) is de-facto primary.
- **DEFECT 3:** No reply-path instruction in envelope. Added generic block teaching CLIs `agent-collab peer send --as <MY_LABEL> --to <THEIR_LABEL> --message '<text>'` with MY_LABEL interpolated + THEIR_LABEL templated.

New gate: `tests/daemon-prompt-interpolation.sh` (~150 LoC). E9 probe 4/5 PASS against live daemon peers; room-gemini ack_timeout (pre-existing gemini quirk). Post-tag `7fffd89` E9 annotation.

**Meta-lesson captured:** patch-merged ≠ patch-deployed. Running daemons are a deployment, not the code. Real-daemon E-probe required after any binary-behavior-affecting patch.

### v3.1 recommended-models table — shipped

`16d0ff1` (doc-only). Added canonical "Recommended models by provider" table to `docs/DAEMON-OPERATOR-GUIDE.md` — single source of truth for operators setting `pi.model`:
- `openai-codex` → `gpt-5.3-codex` (OAuth via `~/.pi/agent/auth.json`)
- `google-antigravity` → `gemini-3-flash` (OAuth same)
- `zai-glm` (plugin) → `glm-4.6` (env `ZAI_GLM_API_KEY`)
- `anthropic` → not yet probed (v0.4+ scoping)

### Topic 3 v0.3 (pi-as-universal-harness collapse) — shipped

`304bbad..4d65bf3` + ride-along + post-tag:
- `304bbad` scope-doc v2 (5 reviewer-round findings absorbed: test-engineer F1 path-shape guard, room-codex harness-assertion-break + §1 wording narrowing, test-engineer F2/F3 gate-wording + stale-UUID subtest, test-engineer F4 E5/E6 preservation banner, test-engineer F5 E10 peer-send-reply assertion)
- `d5445ac` commit 1: feat(daemon/collapse). Removed `spawnCodex` + `postSpawnCodexResumeHandling` + `spawnGemini` + 4 helpers + 3 regex vars. Net **-1690 LoC**. SOFT SHIM mapping: `--cli=codex` → pi-openai-codex, `--cli=gemini` → pi-google-antigravity. Shape β WIDE agent-gate (`sessions.agent IN {pi, codex, gemini}`) + path-shape guard (`strings.Contains(cachedPath, "/")`). Deprecation warning on startup. `tests/daemon-collapse-migration.sh` new (~350 LoC, 5 subtests). v0.2 4e/5h extended for dual-shape.
- `4d65bf3` commit 2: docs(collapse). Operator-guide rewrite + §E10 protocol + install template update + §9.3 migration story.
- `16d0ff1` v3.1 recommended-models ride-along (above)
- `20205dc` post-tag E10 last-run annotation

**Key empirical evidence:** E10 both phases PASS against live pi 0.67.68 + OAuth providers.
- Phase 10a (codex → pi-openai-codex): batch-2 recall "indigo", post-reset "NONE" ✓
- Phase 10b (gemini → pi-google-antigravity): batch-2 recall "amber", **post-reset "amber" via OOB inbox-DB reconstruction** — gemini-3-flash proactive tool-use reconstructed pre-reset context from SQLite; fresh session UUID confirms Arch D reset contract intact, but behavioral property of model-persona + tool-access-scope.

**OPTION A partial collapse:** codex + gemini retired as direct paths (SOFT SHIM compat through v0.3 → HARD RETIRE v0.4). claude-direct preserved (claude-via-pi-anthropic requires separate auth setup; blocks on owner evaluation of auth-provenance shift; deferred to v0.4+ scoping per §4.3).

### v3.1 roadmap brainstorm update — landed

`38e7244` (+394/-1) + `7b5a7fb` (+58/-22). New §14 "Post-Topic-3-collapse state" (12 subsections) + surgical edits inside §13. 7-lane reviewer aggregation folded: coder (M1-M7 arc + P1-P9 papercut queue + L1/L2 ladder), test-engineer (β-i audit correction + β-ii simplification + meta-lesson trio + §E-probe ladder), room-codex (daemon-peer evidence), room-pi (Q1/Q2 + v3.2 staleness cross-ref), room-gemini (citation-rot escalation 4163 LoC), gamma (doc-citation-rot confirmed-compounding append + reorder-challenge definitive closure + drift-check amendments).

### Live dogfood validations — today

Run post-v0.3 to empirically validate the collapse:
1. **Headless batch-retention test (3 agents in parallel):**
   - room-pi (pi + zai-glm + glm-4.6): "OLIVE" stored + recalled ✓
   - room-codex (SOFT SHIM → pi-openai-codex + gpt-5.3-codex): "BRONZE" stored + recalled ✓
   - room-gemini (SOFT SHIM → pi-google-antigravity + gemini-3-flash): "CRIMSON" stored + recalled ✓
   All 3 reactive within 12s; all 3 recalled within 6s on second spawn.

2. **Claude ↔ daemon-peer conversations (3 parallel):**
   - mediator ↔ room-pi (GLM): auth env + session file + plugin-vs-OAuth tradeoff
   - coder ↔ room-codex (codex-shim): room-codex self-identified SOFT SHIM via 3 evidence sources (PPID=pi, PI_CODING_AGENT=true, daemon log `cli_collapse.v03_shim`)
   - test-engineer ↔ room-gemini (gemini-shim): model explained E10b OOB-reconstruction as Antigravity-persona + tool-scope, not protocol gap

## Current infrastructure state

### Working tree at handoff

```
origin/main @ 7b5a7fb
(local main = origin/main; nothing ahead, nothing staged)
Untracked: .claude/ (mediator session fixtures; gitignored)
```

### Running daemon processes (background, v0.3 binary)

All 5 launched at ~23:30 JST with v3.1 [PROMPT] patch binary + SOFT SHIM / canonical pi:
- PID 47203: `room-pi` — pi + zai-glm/glm-4.6 → `/tmp/room-pi-v03.log`
- PID 47204: `room-codex` — SOFT SHIM → pi-openai-codex/gpt-5.3-codex → `/tmp/room-codex-v03.log`
- PID 47205: `room-gemini` — SOFT SHIM → pi-google-antigravity/gemini-3-flash → `/tmp/room-gemini-v03.log`

Earlier stale daemons (PIDs 61042, 61043, 54545, 82474, 82475) were killed via `pkill -f "daemon.*label room-"` pre-relaunch.

**On resume:** owner can keep running (dogfood continuity) or stop (`pkill -f "daemon.*label room-"`). All consume tokens on any peer-inbox traffic.

### Peer-inbox room `fluffy-beaver-bc6f`

Live peers (last_seen within ~30min of handoff):
- `mediator` (claude, lead) — this session
- `coder` (claude, peer) — implementation; standing by
- `test-engineer` (claude, reviewer) — probe + gate-lane; standing by
- `gamma` (claude, reviewer) — drift-check lane; standing by
- `room-codex` (codex-via-SOFT-SHIM) — daemon-backed
- `room-gemini` (gemini-via-SOFT-SHIM) — daemon-backed (behavioral quirk: ack_timeout under certain prompt shapes; OOB-reconstruction post-reset)
- `room-pi` (pi + zai-glm) — daemon-backed

Stale (aged out during session; evidence of P8 silent-access-loss papercut):
- `alpha` (dropped earlier per owner)
- `reason` (aged out post-v0.2 challenge round; didn't return for v0.3)
- `gemini-peer` / `merry-acorn` (interactive-hook mode, superseded)
- `room-pi-codex` / `room-pi-gemini` (spawned during v0.3 validation; dropped out of active roster; not refreshed post-broadcast)

### Pair-key state

`fluffy-beaver-bc6f` turn-count was reset mid-session (owner hit 100/100 cap; raised `AGENT_COLLAB_MAX_PAIR_TURNS=500` in `~/.zshrc` + reset pair). Current turn_count small (~single-digit on fresh rows).

**⚠ Env-var trap:** `AGENT_COLLAB_MAX_PAIR_TURNS=500` in `~/.zshrc` doesn't propagate to Claude Code's bash-tool shell (same non-interactive-bash trap as `ZAI_API_KEY`). Future `peer send/broadcast` invocations from bash-tool need inline prefix: `AGENT_COLLAB_MAX_PAIR_TURNS=500 agent-collab peer ...`.

## v3.1 papercut queue (mediator-curated, post-dogfood state)

### Session-lifecycle cluster (NEW — owner-surfaced this session)

- **P8 [ROOM-ACCESS] Silent access-loss** — peers age out of active roster without owner awareness. Today alone: `alpha`, `reason`, `gamma` (twice), `room-pi-codex`, `room-pi-gemini` all dropped silently. Needs one or both: (a) auto-heartbeat refresh from active sessions, (b) owner-visible "peer went stale" surface (peer list annotation, broadcast alert). Candidate mechanism: extend daemon-mode heartbeat surface.

- **P9 [ROOM-ACCESS] Join-broadcast amplification** — on re-register, `[system] <label> joined the room` fires ONCE PER ROOM MEMBER. 13-peer room = 13 duplicates per rejoin. Clogs inboxes + visible in hook-delivered stream. Needs de-dup at broadcast-emit OR collapse joined announcements to single room-wide row.

### Today's dogfood additions

- **[ENV] Non-interactive bash doesn't source `~/.zshrc`** — recurring trap for `ZAI_API_KEY` / `ZAI_GLM_API_KEY` / `AGENT_COLLAB_MAX_PAIR_TURNS` / presumably future env vars. Candidate fixes: (a) daemon startup validates required env vars for configured provider + warn-once if absent (test-engineer's shape-2 recommendation); (b) operator-guide note on inline env prefix or wrapper-script pattern (coder's shape-1 recommendation echoing v0.1 §6.2 GEMINI_CONFIG_DIR precedent).

- **[PROMPT-DEBUG] Envelope prompt doesn't name CLI/shim routing** — coder dogfood observation. room-codex had to use PPID + env var + daemon log to prove SOFT SHIM routing. Addition to envelope prefix (e.g. "you are running as --cli=codex → SOFT SHIM → pi+openai-codex") would improve operator debuggability without breaking the static-by-design envelope rule. Not blocking.

- **[INTEGRATION-BEHAVIOR] gemini-3-flash OOB reconstruction** — E10b finding. Antigravity-persona model proactively uses `ls`/`cat`/`sqlite3` to reconstruct post-reset context. Arch D reset contract intact (fresh session UUID); but operator expectation ("reset = recall-negative guarantee") doesn't hold under proactive tool-access models. Doc-only operator-guide note needed: reset-intent ≠ recall-negative guarantee under proactive agents; if strong post-reset isolation needed, clear historical `inbox` rows OR narrow spawned CLI's tool-access scope.

### Carried forward (unchanged)

- **smoke.sh macOS fragility** — pre-existing. `chmod -R u+w "$TMP_ROOT"` before `rm -rf`, or separate `GOMODCACHE`.
- **hook-latency-exec p99** — occasional flake when run back-to-back after heavy SQLite gates. Doc-hygiene (isolation run recommendation) or 5s settle-delay.
- **reason's v0 §6 L2-bullet-1 annotation** — receive-side `envelope.state=closed` handler described-but-unshipped. Doc-only hygiene.

### Pruned

- **P7 reason #3 codex mid-run UUID rotation** — OBSOLETE post-v0.3. codex-direct is retired; the UUID-rotation code path is deleted (v0.3 §14.2 M3 + M5 use `sessions.agent` correctly). Struck out in `v3.1-roadmap-brainstorm.md:496`.

## v0.4 plan sketch

Two tracks per ratified scope-doc:

1. **HARD RETIRE the SOFT SHIM** — remove `--cli=codex` + `--cli=gemini` flag handling from daemon code. Operators with legacy configs get fail-loud error pointing at canonical pi form. Single feat commit + docs.

2. **claude-via-pi-anthropic architectural open** — separate owner evaluation required. Trade: gain Claude Arch D cross-spawn context retention (pi `--session <path>` bypasses claude CLI's no-cross-process-session-resume asymmetry). Lose: claude CLI subscription-token integration + MCP bindings + native OAuth flow. Needs fresh scoping doc; does NOT block HARD RETIRE.

Mediator-tracked papercut cluster (P8/P9/[ENV]/[PROMPT-DEBUG]/[INTEGRATION-BEHAVIOR]) could also ship in a v0.4-adjacent window if owner wants to batch. Or separate v3.1-hygiene release.

## Meta-lessons (new this session)

Added to owner's memory or worth capturing:

- **"Fake-binary gates don't validate real-CLI behavior"** (from v0.1.2). Established earlier; still holds.

- **"Plugin-provider env vars require `registerProvider()` source-read, not `pi --help` alone"** (from v0.2.1). pi's `--help` is blind to plugin-registered providers. Operator/mediator verification of provider-env-var mappings must read the plugin source at `/opt/homebrew/lib/node_modules/<plugin-name>/extensions/*.ts`.

- **"Patch-merged ≠ patch-deployed"** (from v3.1 E9 deployment-gap probe). Running daemons are deployment. When binary-behavior patches ship, running daemons still use the pre-patch binary until explicitly killed + relaunched. Test-engineer's E9 round-1 found literal `<YOUR_LABEL>` placeholders against FRESHLY-PUSHED v3.1 code because daemons were launched hours earlier.

- **"Don't double-gate ratification"** (feedback memory updated). When mediator has surfaced a recommendation and the owner hasn't pushed back, executing the recommendation IS the ratification. Asking "ratify or redirect?" after already stating "recommendation: ratify" on the same turn is a stall.

- **"Doc-citation rot is load-bearing, not merely preferable"** (gamma + §13 observation + §14.10). scripts/peer-inbox-db.py went from 942 LoC (§13 observation) to 4163 LoC (current) = 4.4x growth. Symbol-keyed references required; raw line-number citations rot mid-commit.

- **Ironic self-example:** gamma caught that §14.9 Q2 had a broken citation (pointed at nonexistent content) in the SAME commit (`38e7244`) that elevated the rot-observation to load-bearing. Fixed in `7b5a7fb`. The meta-lesson and its violation shipped together.

## Resume-by-role instructions (after context-reset)

**If you're resuming as mediator (fresh session):** read Required Reading list. Current phase = v0.3 shipped + v3.1 roadmap updated; running daemons are live; no active challenge rounds; v3.1 papercut queue has 3 new items this session (P8/P9 + [ENV] + [PROMPT-DEBUG] + [INTEGRATION-BEHAVIOR] cluster). Next owner-decision candidates: (a) v0.4 HARD RETIRE scoping, (b) claude-via-pi-anthropic architectural scoping, (c) v3.1-hygiene papercut cluster, (d) continue dogfood validation, (e) v3.2 scoping re-open.

**If you're resuming as coder:** scope-doc v2 at `304bbad`, impl at `d5445ac + 4d65bf3`, ride-alongs at `ab61c24 + 16d0ff1`, post-tag at `20205dc`, roadmap update at `38e7244 + 7b5a7fb`. Standing down post-v0.3. When v0.4 opens, the HARD RETIRE track is small (remove SOFT SHIM flag handling + operator migration note); claude-via-pi-anthropic is its own scope-doc cycle.

**If you're resuming as test-engineer:** E10 ship-closure probe complete (both phases PASS). Meta-lessons captured in §14.3. When v0.4 HARD RETIRE opens, E10 protocol should be re-run against the post-hard-retire binary to confirm SOFT SHIM removal doesn't regress the path-shape-guard + Shape β agent-gate. β-ii redaction MVP estimated 4d-end of 4-6d range post-collapse.

**If you're resuming as gamma:** drift-check + retro-correction lanes exercised this session. v0.2 sessions.cli/agent self-correction noted in §14.8 blockquote. Standing by for v0.4 HARD RETIRE drift-check when that opens (lane: citation discipline, architectural-hole scan, v0.4 HARD RETIRE surface completeness parallel to §14.8 check on v0.3).

**If you're resuming as room-pi / room-codex / room-gemini:** daemon-backed peers from v0.3 SOFT SHIM dogfood still running (PIDs 47203/47204/47205). If stale per peer list, re-register + relaunch. Stale-state itself is the P8 evidence.

**If you're resuming as room-pi-codex / room-pi-gemini:** these daemon peers spawned during v0.2 validation + v0.3 dogfood; currently aged out of active roster. Owner decides whether to re-register or leave dormant. Canonical pi form (`--cli=pi --pi-provider=<X>`) is preferred post-v0.3; SOFT SHIM is compat only.

## Handoff doc anchor

Commit this handoff in single cohesive doc-only commit on `main` before full session reset. Resume sessions anchor against this SHA + tag `v3.x-topic-3-v0.3-shipped` as functional baseline.
