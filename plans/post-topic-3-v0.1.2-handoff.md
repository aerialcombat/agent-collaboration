# Post-Topic-3-v0.1.2 Handoff — Session State Snapshot

**Status:** Topic 3 v0.1.2 shipped + empirically validated end-to-end against real CLIs. Topic 3 v0.2 (pi-as-4th-CLI) scoping draft v1 committed, challenge round opened but only partially attended. Room `fluffy-beaver-bc6f` has active daemon-backed peers (room-codex, room-gemini) running Arch D opt-in. Context-budget building up; owner requested fresh-start handoff. · **Session:** fluffy-beaver-bc6f (holding open)

Load this doc + the referenced plan docs to resume. You don't need the full transcript.

## Required reading (in order)

1. `plans/post-topic-3-v0-handoff.md` — prior handoff (pre-v0.1). Picks up where `v3.0-W1-handoff.md` + `post-option-j-handoff.md` left off.
2. `plans/v3.x-topic-3-implementation-scope.md` — Topic 3 v0 impl-scope-doc v2 (reviewer-convergent; includes v0 ship-closure amendments at `7ce9828`).
3. `plans/v3.x-topic-3-arch-d-scoping.md` — Topic 3 v0.1 Arch D scope-doc (shipped through v3 amendments via `e64c867`, `99d315e` exit-code correction, and v0.1.1/v0.1.2 patches).
4. `plans/v3.x-topic-3-v0.2-pi-scoping.md` — **Topic 3 v0.2 scoping draft v1 at `5c13748`** (reviewer-challenge-pending).
5. `docs/DAEMON-OPERATOR-GUIDE.md` — operator-facing Architecture D semantics (codex + gemini + claude-asymmetry + reset primitives + timeout tuning).
6. `docs/DAEMON-CLI-SESSION-VALIDATION.md` — manual probe protocol for Arch D real-CLI validation; contains E5/E6/E7 probes + **last-run annotation landed at `93b7397`** (2026-04-18 vs v0.1.2 @ `6ec4b23`).

## Closure stack (tags on origin/main)

```
v3.x-option-j-shipped         (b5f9485)  Option J hook-delivery baseline
v3.x-topic-3-v0-shipped       (ab09f81)  Arch B auto-reply daemon + §3.4 contract
v3.x-topic-3-v0.1-shipped     (371609b)  Arch D code + fake-binary gates (NOT yet empirically functional)
v3.x-topic-3-v0.1.2-shipped   (6ec4b23)  Arch D EMPIRICALLY FUNCTIONAL (first real-CLI passing ship)
```

`v3.x-topic-3-v0.1-shipped @ 371609b` is preserved as first-ship marker; `v0.1.2-shipped @ 6ec4b23` is the real functional closure.

## Since post-topic-3-v0 handoff — what happened

### Topic 3 v0.1 (Arch D) — CLI-native session-resume pass-through

Scope arc: observation that codex + gemini expose stable native session-resume primitives (codex `exec resume <UUID>`, gemini `--resume <N>`) led to PoC → reviewer scoping round → implementation.

**v0.1 ship (4186809..371609b, 7 commits):**
- `4186809` scope-doc draft v1
- `30290e7` scope-doc v2 (reviewer challenge-round absorption: reason's §3.4 normative constraints; §6.2 warn-and-NULL; §9.1 compression framing)
- `e64c867` commit 1: migration 0003 `daemon_cli_session_id TEXT NULL` column + GOOSE 2→3 + scope v3 amendment (gemini index-translation from Checkpoint 1 finding)
- `99d315e` commit 2: store methods + reset verb (Python+Go parity; in-commit §8.1 exit-code correction 65→6 EXIT_NOT_FOUND)
- `d0b5979` commit 3 sub-task A: opt-in flag plumbing + claude warning + codex banner fixture
- `6b94fa0` commit 3 sub-tasks B/C/D (squashed): codex+gemini resume variants + capture strategies + L1 auto-GC hook
- `371609b` commit 4: install template + operator guide § Architecture D + manual probe doc + cross-links

**v0.1.1 patch (a546a5f, post-ship test-gap close):**
- `tests/daemon-cli-resume-claude-asymmetry.sh` landed with 4 subtests (6a exact-warning-string fixture pin, 6b Arch B proceeds, 6c column no-write, 6d defensive spawn-construction for non-NULL column)
- Scope §4.1 codex argv-ordering corrected (flags-first UNIX convention)
- Scope §7.3 object-form error wording corrected (natural json.Unmarshal error)

**Warn-once gap-fill (`9b1588a`, test-engineer exercise of pre-authorized test-writing discretion):**
- `tests/daemon-cli-resume-claude-warn-once.sh` — N=3 drip-send cardinality gate (warn appears EXACTLY ONCE across batches)

**v0.1.2 patch (6ec4b23, real-CLI bug-fixes from E5/E6 probes + bundle):**
- **BUG 1:** codex banner stream mismatch — daemon captured stdout only; real codex 0.121.0 emits banner to STDERR. Fix: `execSpawn` returns (stdout, stderr); `postSpawnCodexResumeHandling` scans concatenation; fake codex fixture emits on stderr.
- **BUG 2:** gemini `--list-sessions` 5s hardcoded timeout — real gemini 0.38.2 takes ~5.3s wall-clock. Fix: bump to 15s default + env override `AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT`.
- Bundle: 9b1588a ride-along + gamma N1 (symbol-keyed `transitionClosed` ref) + gamma N2 (§11 LOCKED wording) + `.gitignore` housekeeping.

**E5/E6/E7 real-CLI probes (test-engineer-executed, post-v0.1.2 at `93b7397`):**
- `codex-cli 0.121.0` E5 PASS (UUID captured, resume argv correct, batch-2 reply recalled "vermilion")
- `gemini 0.38.2` E6 PASS (UUID captured, UUID→index translation, `--resume 1`, reply recalled "42")
- `claude 2.1.114` out-of-scope per §4.3 Arch B asymmetry
- Last-run record in `docs/DAEMON-CLI-SESSION-VALIDATION.md`

**Room dogfood with v0.1.2:**
- `room-codex` + `room-gemini` daemon-backed peers registered in `fluffy-beaver-bc6f` with `--cli-session-resume=1`
- Both captured UUIDs: room-codex `019da08b-1ca0-7533-9fdb-80349d8e3dc5`, room-gemini `c680fbaa-71e0-4c2f-ac1c-3644d52d5bb8`
- Context-recall test: codex replied "starling" (correct); gemini recalled "heron" in session-file (peer-send not emitted, gemini behavioral quirk, not a protocol bug)

### Topic 3 v0.2 (pi-as-4th-CLI) — scoping in progress

Owner-surfaced pi (`badlogic/pi-mono`, `@mariozechner/pi-coding-agent`) as potential 4th Arch D CLI, specifically for GLM provider integration.

**PoC validated (mediator-run earlier):**
- `pi --provider zai-glm --model glm-4.6 --print --session <path>` two-step context recall works
- `ZAI_GLM_API_KEY` added to `~/.zshrc` (operator shell config, not daemon code)
- Syntax confirmed against official docs

**Key architectural differences from codex/gemini in Arch D:**
- **Session identity: DAEMON OWNS PATH** (no capture step, no UUID regex, no `--list-sessions` race)
- Multi-provider via `--provider <name>` (zai-glm, openai-codex, google-antigravity, anthropic)
- JSONL format, owner-controlled
- Structurally simpler: no capture, no translation, no stream-scan

**Scoping round outcomes (Q1-Q4 answered by coder + test-engineer):**
- Q1 footprint: ~500-700 LoC core + ~600-800 LoC tests (~1300-1700 total across ~10 files; 1-2 focused days)
- Q2 test surface: 7 gates enumerated by test-engineer (daemon-cli-resume-pi / daemon-pi-multi-provider / daemon-pi-session-lifecycle / daemon-pi-warn-once + fixture + E8 probe + daemon-harness addition)
- Q3 scope boundary: additive v0.2 pi-direct; collapse deferred to v0.3+ (pi-as-universal-harness replacing codex/gemini direct)
- Q4 config shape: **NESTED sub-object `pi: { provider, model, session_dir }` (owner ratified)** — coder's preference over test-engineer's flat-fields recommendation

**Draft v1 at `5c13748` (272 lines, 12 sections):**
- Mirrors `v3.x-topic-3-arch-d-scoping.md` shape
- §3.4 normative invariants re-read for pi (path-as-identity)
- §5 NO schema migration (reuse `daemon_cli_session_id` column as PATH)
- §6 NO capture step
- §8 pi-specific reset side-effect (delete session file, not just NULL column)
- §10 four open Qs for reviewer challenge:
  - Q1 file-delete vs rename-to-.deleted on reset
  - Q2 pi-help fixture strictness (full-diff vs grep-pattern)
  - Q3 required pi.model vs provider-defaulted model
  - Q4 single fake-pi binary vs per-provider fakes
- §11 tag LOCKED: `v3.x-topic-3-v0.2-shipped`

**Challenge round opened at last session turn** — multicast reached test-engineer only; gamma + reason stale. Owner paused coder mid-flow ("let's pause coder"), then un-paused ("tell coder to finish scoping"). Coder delivered the committed draft. Owner-pending decision: restart gamma + reason for v0.2 round, or proceed with test-engineer alone.

**Test-engineer v0.2 challenge findings (delivered post-pause, pre-handoff):** 5 gate ADDs + concur-all-four-§10 + 1 watchdog-proxy flag:
- (A) `daemon-pi-warn-once.sh` → rename to `daemon-pi-startup-validation.sh` (pi emits no startup warning; slot tests required-field exit-64).
- (B) Mix-mode flag-is-gate subtest in gate 1 (parallel to v0.1 codex 4c / gemini 5d) ~30 LoC. High importance.
- (C) 2-label cross-isolation with distinct providers in gate 1 (parallel to v0.1 4d/5e) ~40 LoC. High importance given multi-provider is a v0.2 headline.
- (D) Cross-CLI reset isolation subtest ~30 LoC: reset a codex/gemini label, assert column NULL'd + NO `os.Remove` attempted. Locks pi-specific file-delete at the branch point, prevents unconditional refactor regression.
- (E) E8 probe reset→file-deleted verification ~5 markdown lines.
- Gate 2 add ~5 LoC: assert argv contains EXACTLY ONE `--provider` and ONE `--model` (guard against precedence-merge duplication bugs).
- §10 CONCUR: hard-delete / grep-pattern (+ grep "pi-mono" version-marker) / both-required / single fake-pi.
- Watchdog-proxy flag: §7.1 nested-config "precedent-setting for codex/gemini future namespacing" claim is borderline cruft (same pattern as reason's v0.1 §7.3 watchdog; recommend drop precedent claim OR name a concrete v1+ codex/gemini use-case). NOT scope-blocking.
- §3.2 path-as-identity framing accurate; §6 no-capture genuinely no-code; all 5 §3.4 invariants pi-reading map cleanly.

**Gap-fill total:** ~100 LoC + 1 rename + 5 markdown lines. Within 1300-1700 envelope.

## Current infrastructure state

### Working tree at handoff
```
origin/main @ 5c13748
(local main = origin/main; nothing ahead, nothing staged)
Untracked: .claude/ (mediator session fixtures; gitignored per .gitignore updated in 6ec4b23)
```

### Running daemon processes (background)
Both launched from `v3.x-topic-3-v0.1.2-shipped @ 6ec4b23` with `--cli-session-resume`:
- PID 82474: `go run ./cmd/daemon --cli codex --label room-codex --session-key room-codex-1776514952 ...` → `/tmp/room-codex-daemon.log`
- PID 82475: `go run ./cmd/daemon --cli gemini --label room-gemini --session-key room-gemini-1776514952 ...` → `/tmp/room-gemini-daemon.log`

**On resume: owner can keep these running (functioning dogfood) or stop them (`pkill -f "cmd/daemon"`).** They'll consume tokens on any peer-inbox traffic routed to their labels.

### Peer-inbox room `fluffy-beaver-bc6f`
Live (last_seen within ~30min of handoff):
- `mediator` (claude, lead) — this session
- `coder` (claude, peer) — implementation; standing by
- `test-engineer` (claude, reviewer) — test-gate lane; responded to v0.2 Q2 challenge
- `room-codex` (codex, daemon mode) — dogfood peer with active Arch D resume
- `room-gemini` (gemini, daemon mode) — dogfood peer with active Arch D resume

Stale (last_seen 1+ hour stale; owner may restart):
- `gamma` (claude, drift-check lane) — answered v0.1 + v0.1.2 challenge rounds; NOT responded to v0.2
- `reason` (claude, synthesis/watchdog lane) — answered v0.1 + v0.1.2 challenge rounds; NOT responded to v0.2
- `alpha` (claude, contract-layer lane) — dropped per owner; not expected to return
- `gemini-peer` (gemini, reviewer) — interactive-hook mode; has no v0.1.2-code daemon
- `merry-acorn` (codex, reviewer) — interactive-hook mode; superseded by `room-codex` dogfood

## Pending owner decisions (prioritized)

1. **v0.2 reviewer challenge round completion.** test-engineer has delivered (5 gate ADDs + all §10 votes + watchdog-proxy flag on §7.1 nested-precedent). Options for remaining lanes:
   - (a) Restart gamma + reason (drift-check + synthesis lanes); route to them; catch-or-confirm test-engineer's watchdog-proxy concern on §7.1.
   - (b) Proceed with test-engineer alone; defer drift + watchdog to v2-absorption phase; test-engineer's proxy flag is enough signal.
   - (c) Skip challenge round; ratify coder's draft as-is + test-engineer ADDs; go straight to v2 + implementation.
   - Test-engineer's lean: v0.2 is compact enough that test-engineer-primary + coder-Q-responder is sufficient for first-pass; restart gamma+reason only if owner has bandwidth to validate §7.1 precedent claim concretely.
2. **Dogfood daemon disposition.** room-codex + room-gemini daemons are running; consuming tokens per peer-inbox traffic. Stop, or keep live for v0.2 dogfood continuity?
3. **v0.2 impl authorization post-challenge.** After challenge round closes + coder absorbs feedback into v2, ratify and authorize implementation (4 commits: setup, store-reuse, daemon+spawnPi, install+docs; ~1-2 days).
4. **v0.2 tag naming.** `v3.x-topic-3-v0.2-shipped` LOCKED in scope-doc §11; owner can redirect if desired before impl closes.
5. **v3.1 papercut queue curation.** Four items accumulated; see below.

## v3.1 papercut queue (mediator-curated, NOT in v0.2 scope)

- `reason #3`: codex mid-run UUID rotation unhandled (E2E probe-time awareness; fail-safe via §3.4 invariant 5).
- `smoke.sh` cleanup macOS fragility (pre-existing, not v0.1-introduced) — owner-batched; candidate fix: `chmod -R u+w "$TMP_ROOT"` before `rm -rf`, or set `GOMODCACHE` to separate dir.
- `hook-latency-exec.sh` p99 over budget when run back-to-back after heavy SQLite gates — add 5s settle-delay OR document "run this gate in isolation" in test-runner guidance.
- reason's v0 scope-doc `§6 Layer 2 bullet 1` annotation (`v1+ extension` tag) — receive-side `envelope.state=closed` handler that doesn't ship in v0/v0.1. Doc-hygiene only.

## Resume-by-role instructions (after context-reset)

**If you're resuming as mediator (fresh session):** read the Required Reading list. Current phase = Topic 3 v0.2 scoping challenge round PARTIALLY OPEN (test-engineer delivered; gamma + reason pending restart-or-bypass decision). Next action = owner picks option (a/b/c) on decision #1 above; if (a), open the DM to gamma+reason once they're live; if (b/c), route directly to coder v2 absorption or impl authorization.

**If you're resuming as coder:** scope-doc draft v1 at `5c13748` on origin/main. Standing down per mediator routing. When challenge round closes, mediator routes the clustered findings; you draft v2 absorbing them; then implement via 4 commits per §9 plan + test-engineer's 7-gate enumeration. Pre-authorized parallelism: sub-task B (store methods) is trivial (column reuse); sub-task C (spawnPi helper + capture-none logic) + sub-task D (nested config plumbing + per-provider arg dispatch) could fan out 2-agent.

**If you're resuming as test-engineer:** you delivered v0.2 Q2 (7-gate enumeration) already. Still open: Q10 voting on 4 open Qs; real-CLI E8 probe when impl lands. Test-writing discretion is pre-authorized; exercise when a substantive gap emerges in impl.

**If you're resuming as gamma:** drift-check lane. v0.2 scope-doc at `5c13748`. Specific asks: §3 trust-boundary reading (pi path-as-identity is FLIPPED from codex/gemini opacity); §3.4 invariants pi-reading; §5 schema NONE + §6 capture NONE cross-refs to v0.1.2; §8 pi-reset side-effect divergence (file-delete vs column-NULL-only) documented consistently; §9 commit-1→2→3 ordering + test-pairing matches v0.1 precedent; §10 Q3 provider+model coupling any drift-check concern.

**If you're resuming as reason:** synthesis + watchdog lane. v0.2 draft at `5c13748`. Specific asks: over-engineering check on nested config (echo of your v0.1 watchdog pass on §7.3 reservation being borderline cruft); §3 trust-boundary is pi genuinely SAFER or same-calculus-differently-packaged; architectural-hole check for unimplemented-but-still-described contracts; §3.4 normative invariants pi-reading.

**If you're resuming as alpha / merry-acorn / gemini-peer:** not in active v0.2 routing. alpha was dropped during v0.1 per owner. merry-acorn + gemini-peer are stale; their lanes are flag-for-awareness for codex-UX / gemini-sub-dependency respectively, not primary.

**If you're resuming and daemons are still running:** they will continue polling on startup. Session files at `~/.gemini/tmp/agent-collaboration/chats/session-*-c680fbaa.json` (gemini) and codex-side session store (vendor-internal) preserve their cross-spawn context per Arch D until either operator `peer-inbox daemon-reset-session --as <label>` OR L1 content-stop sentinel triggers auto-GC.

## Meta-lessons (new this session)

Added to owner's `.claude/projects/.../memory/` during session or worth capturing:

- **"Fake-binary gates don't validate real-CLI behavior."** v0.1 shipped + tagged based on fake-binary gate passes; real-CLI E5/E6 probes exposed two operationally-critical bugs (codex stderr stream, gemini 5s timeout). The manual-probe doc literally said "required for closure before tag"; the tag shipped without running it. v0.1.2 closure proved the value: both bugs were shallow but made Arch D non-functional in production.
  - Rule: *for any feature that integrates with an external-vendor surface, ship-closure requires at least one real-vendor end-to-end probe, not just fixture-pinned CI gates.* The retrievability principle ("convergence is not ratification") scales up from artifact-reading to real-system-executing.
  - Protocol update: `docs/DAEMON-CLI-SESSION-VALIDATION.md` is now the "last-run"-annotated baseline; future version bumps should refresh that annotation as part of the ship-closure checklist.

- **Dogfood within the room IS the integration test for multi-agent daemon work.** `room-codex` + `room-gemini` running live with `--cli-session-resume` proved Arch D end-to-end not just in a CI test-fixture sandbox but in the actual peer-inbox room with other peers routing messages to the daemons. This form of dogfood caught gemini's "ack-without-peer-send" behavioral quirk that no unit test would have.

## Handoff doc anchor

Commit this handoff in a single cohesive doc-only commit on `main` before full session reset. Resume sessions can then anchor against the handoff SHA + the `v3.x-topic-3-v0.1.2-shipped` tag as the functional baseline.
