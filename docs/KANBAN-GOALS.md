# Kanban Goals

**Status:** DRAFT v1 · **Authored:** 2026-04-30 · **Author:** claude (drafting at owner request after the linkboard dogfood pass) · **Position:** the document we should have written before v3.12 started, written now to constrain v3.13+ deliberately rather than by drift.

> **Why this document exists.** Through v3.9 → v3.12.4.7 we accreted features (cards, decomposition, handoffs, pools, dispatch, cancel) reactively. Each sub-phase had a plan; none of them named what the kanban *as a whole* is for. This is fine when the next step is obvious. It stops being fine when the next step isn't — which is where we are now after the linkboard dogfood. This document constrains future feature decisions by naming what the kanban *is*, what it *isn't*, what success looks like, and what failure looks like.

## 1. What the kanban is for

**The kanban is a substrate for executing well-specified, low-to-medium-stakes engineering work autonomously across one or more LLM workers, with the human as occasional orchestrator and reviewer rather than continuous operator.**

That sentence is the load-bearing one. Every decision below derives from it. Specifically:

- **Substrate, not tool.** It's infrastructure other tools (decomposer prompts, pool agents, the drainer) are built on. Operators don't interact with a "kanban" — they interact with a card, a board, a worker, a review queue. The kanban is the verb-bearing layer underneath.
- **Well-specified.** The card body is the contract. If the spec is ambiguous, the worker will guess, often wrong. We optimize for cards that are unambiguous; we *don't* try to make the system handle ambiguous cards well, because the LLM has no access to "the operator's intent when they see the result."
- **Low-to-medium stakes.** Auto-merge to production is out of scope. Code that handles money, auth, or PII goes through additional review outside the kanban. The kanban is for "I want this built; I'll read the diff before merging."
- **Autonomously.** Once an epic is dispatched and the operator walks away, the system keeps making forward progress without human input until either the work completes or something genuinely needs human judgment (which surfaces via in_review, blockers, or exhausted retries).
- **One or more LLM workers.** Single-agent and pool-based dispatch are both first-class. Cross-host federation is not (deferred indefinitely; if it ever returns, the goals here may need amendment).
- **Human as orchestrator, not continuous operator.** The human writes the epic, configures the pool, reviews the in_review queue, and intervenes when alerted. They do *not* watch each card or babysit each worker.

## 2. What the kanban is explicitly NOT for

These are commitments-by-omission. Treat additions to this list as gating: any feature proposal that conflicts with a "not for" item must either remove the item from the list (with reasoning) or be rejected.

1. **Not a thinking partner.** Exploratory, conversational, "let's figure out what we want" work belongs in a Claude Code REPL session, not the kanban. The kanban executes specs; it does not co-author specs.
2. **Not a code reviewer.** The kanban hosts review queues (in_review status) but the actual judgment of "is this code good" is the operator's. We do not aspire to LLM-as-final-reviewer because the failure mode (LLM marks bad code as good) is invisible until it's expensive.
3. **Not a build system.** The kanban dispatches workers that may build, test, and ship code, but the kanban itself does not own build pipelines, deployment, secret management, or CI/CD. These belong in their own systems and the kanban integrates with them as needed.
4. **Not a chat replacement.** The peer-inbox chat layer exists for a reason (volatile coordination, clarification, banter). The kanban is the durable side. Cards have comments, but comments are not chat; they're audit trail. Discussions go in chat, decisions go on cards.
5. **Not a project management tool for humans.** Linear/Jira/Asana exist; we don't compete. The kanban's UI is for humans to watch agents work, not for humans to coordinate with each other. Multi-human assignment, ownership transfer, sprint planning, burn-down charts — all out of scope.
6. **Not a fleet manager.** A board pool is "agents that can drain this board." It's not "agents available across the enterprise that I want to manage." If you need fleet-level coordination across many boards/projects, the kanban is the wrong layer.
7. **Not a cost optimizer.** Token budgets, model selection by cost, runtime cost ceilings — useful but not in scope for the core. Specific deployments may add ceilings on top (and probably should), but the kanban itself doesn't own this concern.
8. **Not a high-stakes autonomous-shipping system.** No card transitions to "merged to main and deployed" without explicit human action. The kanban can build, test, even open a PR; it does not merge.
9. **Not a multi-tenancy platform.** Single-operator (or small team sharing trust) is the only deployment model. We do not aspire to RBAC, audit-for-compliance, tenant isolation. If those are needed, the kanban is again the wrong layer.

## 3. What success looks like

Operationally, the kanban succeeds when the operator can:

1. **Paste an epic and walk away.** A well-specified epic is decomposed into bite-sized children, the pool drains them in dependency order, the operator returns to a queue of in_review cards to read. Wall clock to project completion is dominated by worker execution time, not operator overhead.
2. **Trust the in_review queue.** When a card lands in in_review, that means a worker thinks the work is done. The operator's job is to spot-check, not to redo. The signal that "this card needs human attention" is reliable.
3. **Recover gracefully from worker failures.** A stuck worker, a runaway worker, a confused worker — all surface to the operator quickly (notification, drawer state, audit trail) and have one-action recovery paths (cancel, edit body, re-dispatch).
4. **Run multiple projects without confusion.** Three projects on three boards do not interfere. Pool agents don't cross over. Workers land in the right project tree. The audit trail per board is complete and self-contained.
5. **Read a project's history months later and understand it.** Card events, run logs, comments, handoffs — together they reconstruct what happened, why decisions were made, and where the artifacts live.

Concretely measurable proxies:

- A typical small project (~700 LOC, like linkboard) completes from "Run" to passing acceptance with **at most 1 operator intervention** in <60 minutes wall clock.
- A medium project (~3-5k LOC, multi-day) completes with **at most 5 operator interventions per day**, all of which are real review decisions rather than bug recovery.
- The drawer is the only UI an operator opens to understand a card's state — no `sqlite3` queries, no `tail -f /tmp/peer-inbox/runners/run-N.log`, no `pkill -f`. (We're not there yet; this is aspirational.)

## 4. What failure looks like

These are anti-success states. If the kanban regularly produces these, we have a goal mismatch, not a feature gap.

1. **Operator becomes the bottleneck.** The kanban produces work faster than the operator can review, so a backlog of in_review cards accumulates indefinitely. Operator either (a) stops using the kanban because it's overwhelming, or (b) starts auto-promoting blindly because they can't keep up. Both are failure.
2. **Workers ship bad code confidently.** Cards land in done with code that's broken, insecure, or wildly wrong, and nothing in the kanban surfaced concern. The operator finds out at deployment, or worse, in production. (Note: linkboard's smoke worker catching the JSON-tag bug is the OPPOSITE of this — that's the success mode.)
3. **Operator does kanban admin instead of project work.** Tuning prompts, registering agents, debugging dispatch issues, manually unsticking cards — these grow to consume the time the operator was supposed to be saving. Threshold: if more than ~10% of operator time on the kanban is spent on the kanban itself (vs. real review and direction), we've failed.
4. **Cards drift architecturally over a multi-day project.** Day 3's worker undoes day 1's decisions because the system has no project memory. The result compiles but doesn't cohere. Refactors happen by accident. The codebase becomes worse than what one careful human would produce.
5. **Operator can't trust the audit trail.** Card says done but the work isn't done; run says completed but the worker actually crashed; agent_id says X but the work was actually done by Y. If the data lies, every higher-level decision is wrong.
6. **Recovery from stuck-state requires SQL.** The operator has to know SQLite + the schema to unstick a card, kill a worker, re-route a board. (We just hit this in the linkboard dogfood with the claim-release bug. Track 1 #4 fixes the specific instance; the principle is to not let it happen elsewhere.)

## 5. Implications for design

These follow directly from the goals + non-goals.

### 5.1 Decomposer quality is load-bearing

The decomposer is the single most important component because it determines whether the rest of the system has well-specified cards to work with. A bad decomposition cascades: vague children → guessing workers → mediocre code → operator distrust → operator stops using the system. Investments in decomposer quality have higher ROI than investments anywhere else.

Concrete implication: the decomposer's *output contract* should be enforced (cards rejected at create-time if missing acceptance criteria, edge cases, or interfaces touched). This is a future change — not in scope yet — but it's the right shape.

### 5.2 Per-card lifecycle should be opinionated

The current lifecycle (todo → in_progress → in_review → done) is correct in shape. Future lifecycle additions should be deliberate — `awaiting_challenge` for adversarial review, `awaiting_test` for paired test cards, etc. The temptation to make these "configurable" should be resisted; the system's job is to express opinions, not host policies.

### 5.3 The drawer is the operator's only window

When a feature requires the operator to open a terminal, query SQLite, or `tail -f` a log to understand state, that's a drawer gap. Track 1 #4's claim-release event with `meta.trigger='release'` is the right shape: surface the WHY in the timeline so the drawer tells the story without external tools.

### 5.4 Auto-promote is dangerous; the right shape isn't a toggle

The linkboard dogfood found that `auto_promote=on` ate the review queue mid-chain (cards 2-6 promoted before I could read them) but correctly preserved the tail (card 7 waited for review). The binary toggle is wrong; the right shape is per-card opt-in to mid-chain review (e.g., `cards.needs_review = true`) so the operator can mark "always pause for me on schema migrations / auth code / large diffs" without blocking trivial mid-chain dependencies.

### 5.5 Worker prompts are part of the system, not configuration

The decomposer prompt, worker prompt, split addendum, handoff discipline, challenger contract — these are functional code. Changes to them have outsize effects on output quality and should be versioned, tested, and reasoned about with the same rigor as schema changes. They're not "just text."

### 5.6 Failure should be loud

A card that fails dispatch, a worker that crashes, a drainer that can't find an agent — all should leave a comment event in the timeline and (eventually) trigger a notification. The current "silent fallback to AGENT_COLLAB_WORKER_CMD" was exactly this anti-pattern; Track 1 #3 fixed the specific instance. The principle is broader: silence is for the success path, never for surprise.

## 6. Open quality questions (to be settled deliberately)

These are the choices the goals above don't resolve. They need explicit answers before the next major sub-phase.

### Q1 — Where does "good code" come from?

Two structurally different answers, can't have both cheaply:

- **(A) Workers produce good code by construction.** Better prompts, codebase-aware decomposition, project-memory documents, opinionated agent specialization (planner / builder / tester / reviewer). High effort, high ceiling.
- **(B) Workers produce passable code; the system catches problems via review.** Adversarial challenger lifecycle, paired test cards, mandatory in_review checkpoints. Lower per-card effort but every card pays the review tax (3x dispatches per non-trivial card).

Our current system is closer to "neither, fast" — workers produce whatever they produce and reviews are optional. Choosing (A), (B), or "(A)-for-this-board, (B)-for-that-board" is the next big design call.

**Author recommendation:** (B) first, because it's structurally simpler and produces visible signal (challenger findings are concrete) that informs how to invest in (A) later. (B) also matches the existing repo's lead/challenger collaboration protocol — we'd be surfacing it as a kanban primitive instead of a meta-protocol.

### Q2 — How much project context should a worker get?

Today: zero project memory across cards. Each worker session is fresh. This causes drift on multi-day projects and forces operators to re-state conventions in every epic body.

Options:
- **No memory** (current). Simple, fast per-card, but architectural drift accumulates.
- **Per-board codebase summary** (operator-authored, attached to every prompt). Cheap, requires manual upkeep, prevents drift on conventions.
- **Persistent project_decisions.md** (workers append, future workers read). Self-maintaining if the prompt teaches it, but adds prompt bloat and creates a "file workers must remember to update."
- **Full session continuity** (per-card sessions persist across runs, like the v3.12.3 plan). Powerful but currently out of scope and architecturally large.

**Author recommendation:** the cheapest move is per-board codebase summary as a `board_settings.codebase_summary TEXT NULL` field plumbed into every worker prompt. Author it once per project; refresh quarterly. Solves 80% of consistency drift for ~20 lines of code. Defer project_decisions.md until we see drift WITH a summary in place.

### Q3 — How does the operator know when to look at the kanban?

Today: polling. Operator opens /cards periodically, scans, decides if anything needs them. This works for small projects but scales badly.

Options for "the kanban tells the operator when to look":
- **No notification** (current). Operator drives. Doesn't scale.
- **Webhook on specific events** (handoff written, in_review tail card, runaway worker). Configure once per board; targets Slack/Pushover/email. Per-board policy, not system-wide.
- **Aggregate review-queue UI** (one URL across all boards, showing what wants attention). Polling-based but easier to scan than per-board hunting.

**Author recommendation:** the aggregate review-queue UI first (cheap, low-risk, no integration with external systems). Webhooks second when the operator articulates "I need to know about X within 5 minutes." Don't build webhooks speculatively — the trigger predicate is the hard part and you'll get it wrong without specific operator pain.

### Q4 — What is the relationship between kanban quality features and time-to-ship?

Quality features (challenger lifecycle, paired tests, mandatory review) cost time. The same project that takes 30 minutes to ship un-reviewed takes 2-3 hours to ship reviewed. This is a real cost, not a hidden one.

Open question: do we make quality features per-board, per-card, or system-wide?

**Author recommendation:** per-board. A board for "build me a quick prototype" can have all quality features off; a board for "build me a piece of the production codebase" has them all on. The operator sets the policy once when creating the board, not per-card. This pushes the quality/speed trade up to a per-project decision where the operator has context to make it.

## 7. Decision boundaries (what changes a "no" to "yes")

For features that contradict the non-goals in §2, what would convince us to change the goals?

- **Multi-tenancy / RBAC:** would require sharing a kanban with other operators we don't fully trust. Today's single-operator assumption is a deliberate cost-saver; changing it triples the system's complexity. Decision boundary: a real shared deployment with a trust gradient (e.g., team of 3 with different review responsibilities).
- **Auto-merge / auto-deploy:** would require trust that the kanban's quality features are good enough to bypass human review. Today they aren't (we explicitly chose not to build for this). Decision boundary: Q1's option (B) ships AND produces measurably good output for >100 cards across multiple projects.
- **Cross-host federation:** today's deferral is partly because the v3.10/v3.11 federation for chat surfaced enough complexity to convince us cards shouldn't repeat the pattern. Decision boundary: an operator with legitimate need for one project to span multiple hosts (e.g., a backend on one machine, an iOS build on a Mac). Linkboard didn't need this; nothing today does.
- **Thinking-partner mode:** would conflict with "well-specified" — the kanban would have to host conversations about what to build, not just executions of what was decided. Decision boundary: the cost of bouncing between Claude Code REPL and the kanban becomes a regular friction (e.g., operator is constantly discovering specs mid-project and the kanban's static-card model becomes genuinely painful).

## 8. What this document is NOT

- **Not a roadmap.** It says what the kanban is for; it doesn't say what we build next. v3.13's plan should be drafted separately, grounded in these goals.
- **Not immutable.** Goals can change. But they should change deliberately, with a paragraph explaining why, not by drift.
- **Not a marketing document.** No one outside the operator reads it. It exists to make our future feature decisions cheaper to evaluate.

## 9. Sources

This document synthesizes:

- The implementation history in `plans/` (v3.11 cards Phase 1, v3.12 agent pool with standup, v3.12-decomposition-and-handoffs).
- The collaboration protocol in `docs/GLOBAL-PROTOCOL.md` (lead/challenger, evidence rule, escalation).
- The linkboard dogfood pass (`~/Development/test/linkboard/DOGFOOD.md`) — 5 friction observations from the first end-to-end real-project test.
- The Track 1 fixes from this session (claim release on → todo, board project_root, no-pool-match audit warning).
- The operator's "torn between three time sinks" framing (micromanage / let-it-do-everything / build-the-harness).

## 10. Operator sign-off

This is a draft. The author (claude) does not get to set goals for a system the operator owns. The next step is for the operator to read this document and either (a) accept as-is, (b) correct what doesn't match their actual intent, or (c) reject as a wrong frame and direct toward a different one.

Specific places where author guesses are most likely wrong:

- §1's framing of "low-to-medium stakes" — operator may want higher stakes (and thus structurally different quality investments).
- §2's commitment list — operator may have intended one of these as goals (especially "thinking partner" or "high-stakes autonomous shipping").
- §6's open questions — author has recommendations but they're calibrated to what the author would do; operator's calibration may differ substantially.
- §7's decision boundaries — author has named what would change the answers; operator may know of pressures already in motion that should change them.

Iterate from here.
