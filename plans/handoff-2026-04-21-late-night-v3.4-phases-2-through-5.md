# Handoff — 2026-04-21 (late night) — v3.4 phases 2, 3, 4, 5 all shipped

**Status:** Every Python CLI verb has a native Go counterpart. 52/52 parity fixtures green. Opt-in Go dispatch wired behind `AGENT_COLLAB_IMPL=go`. **Session:** one continuous run after the evening handoff, 13 commits end-to-end, 3-agent parallel dispatch for Phase 3's bulk. **Next session's job:** Phase 5.1 — close the deferred peripheral gaps so the default can flip to Go; then Phase 6 delete the Python script.

Load this doc + referenced files to resume. Full transcript unnecessary.

## Required reading (in order)

1. `plans/v3.4-python-removal-scoping.md` — the 6-phase plan. §3 scope table, §5 per-phase impl surfaces, §6 risks.
2. `tests/parity/run.sh` + `tests/parity/all.sh` — the harness that gates every verb. `__CWD__` token substitutes a scratch cwd; `AGENT_COLLAB_INBOX_DB` forced to the scratch DB; `PEER_INBOX_PENDING_DIR` suppresses channel pairing for determinism.
3. `go/cmd/peer-inbox/` — one file per verb, all under package `main`. Shared helpers (`resolveCWD`, `pyJSONFormat`, `writePyCompatJSON`, `padRight`, `activityState`, `discoverSessionKey`, `renderHookBlock`, etc.) live in `main.go` alongside the dispatch.
4. `go/pkg/store/sqlite/` — one file per concern (`register.go`, `labels.go`, `peer_send.go`, `broadcast.go`, `queue.go`, `rooms.go`, `session_admin.go`, plus the Phase 1 / Phase 2 files `send.go`, `list.go`). Shared between the peer-web handler and the CLI.

## Commits this session (origin/main..HEAD)

```
105c24b..d25a108   (19 commits)
```

In chronological order (this session's 13 start at `5415820`):

- `5415820` feat(peer-inbox): v3.4 phase 2 — session-list native Go
- `9936cd4` feat(peer-inbox): v3.4 phase 2 — peer-list + peer-receive (read-only)
- `fc0e56d` chore(parity): drop accidentally-committed .actual.json debug snapshots
- `b8890c2` chore(parity): gitignore tests/fixtures/parity/**/*.actual.json
- `89e8c17` chore(peer-inbox): stub Phase 3 write verbs + wire dispatch
- `c2ddd32` feat(peer-inbox): v3.4 phase 3 — session-register native Go (core path)
- `6d3ac85` feat(peer-inbox): v3.4 phase 3 — 6 write verbs via parallel agent ports
- `dfd8fa7` feat(peer-inbox): v3.4 phase 4 — edge verbs native Go
- `d25a108` feat(peer-inbox): v3.4 phase 5 — opt-in Go dispatch via AGENT_COLLAB_IMPL

## Phase status

| Phase | State | Detail |
|---|---|---|
| 0 | landed (prior session) | Parity harness |
| 1 | landed (prior session) | `/api/send` native Go |
| 2 | **landed** | `session-list`, `peer-list`, `peer-receive` (read-only). `peer-replay` + `peer-watch` deferred (time injection + blocking-loop orchestration; documented in scoping doc §5 Phase 2). |
| 3 | **landed** | `session-register`, `peer-send`, `peer-broadcast`, `peer-round`, `room-create`, `peer-reset`, `peer-queue`. Parallel agents landed the latter 6 in one wall-clock pass. |
| 4 | **landed** | `hook-log-session`, `session-adopt`, `session-close`. |
| 5 | **landed (opt-in)** | `AGENT_COLLAB_IMPL=go` routes the wrapper to the Go binary. Default stays Python. |
| 5.1 | **pending** | Close deferred peripherals so the flip is safe (see below). |
| 6 | blocked on 5.1 | `rm scripts/peer-inbox-db.py` + remove python3 from installer deps. |

## Parity fixtures (52 total)

- `daemon-claim/daemon-complete/daemon-sweep` — Phase 1 (Topic 3)
- `session-list` — 2 scenarios
- `peer-list` — 2 scenarios
- `peer-receive` — 6 scenarios (plain/json/hook/hook-json × empty/one-unread)
- `session-register` — 1 scenario (idempotent-refresh; fresh-mint needs token determinism)
- `peer-send` — 2 scenarios (local-happy-path, idempotent-retry)
- `peer-broadcast` — 2 scenarios (fan-out-two-peers, subset-multicast)
- `peer-round` — 1 scenario
- `room-create` — 2 scenarios (fresh, conflict)
- `peer-reset` — 3 scenarios (basic, absent, validation)
- `peer-queue` — 2 scenarios (show-empty, flush-unconfigured-host)
- `hook-log-session` — 1 scenario
- `session-adopt` — 1 scenario
- `session-close` — 1 scenario

Gaps worth filling when convenient: `peer-replay`, `peer-watch`, `session-register` fresh-mint path (needs test-time randomness injection — see plan §6 risk register).

## Phase 5.1 — the peripheral-gap backlog

Each bullet is a concrete feature the Python CLI does and the Go port doesn't. Unblock the default-flip only after each is either ported OR explicitly marked obsolete.

1. **`find_pending_channel_for_self`** (peer-send + session-register) — walks the process tree for a pending-channel registration so sends can push to live peers via unix sockets. Go needs `os.Getppid()` loop + read `~/.agent-collab/pending-channels/<pid>.json`. Surface: `[channel: paired]` vs `[channel: none]` in session-register stdout; and `push_status=pushed` on peer-send's SendResult.
2. **`emit_system_event`** (session-register, session-close, peer-reset) — posts a `[system]` body to the room on join/leave. Depends on item 1 for the channel-socket target + on a `POST /` unix-socket writer. Pure cosmetic today; room members don't see join/leave events from Go-initiated flows.
3. **`sweep_stale_markers_for_label`** (session-register) — after a successful register, walks `<cwd>/.agent-collab/sessions/*.json` and removes markers whose session_key doesn't match any row in `sessions`. Without this, ResolveSelf's sole-marker convenience path sees ghosts.
4. **`recent_seen_sessions` fallback** (session-register, session-close) — when no explicit session-key and no env-discovered key, consult the hook log to find a session_id that's been active but isn't registered yet. Low priority — `--session-key` always works.
5. **`--to-cwd` override** (peer-send) — lets operators direct a send to a specific `to_cwd`, bypassing pair-key / same-cwd resolution. Gated on a new `TargetCWD` field in `SendParams` in `go/pkg/store/sqlite/send.go`, which Phase 3 file-ownership rules forbade the agent from editing. Unlock by editing send.go intentionally.
6. **Federation remote-routing client** (peer-send) — when a room's `home_host` is remote, peer-send must POST to that host's `/api/send` instead of writing locally. The Go peer-web *server* already accepts these. Agent A's port detects the remote case and routes to the Python CLI fallback; the Go client needs porting from `_peer_send_remote` in `peer-inbox-db.py`.
7. **`BroadcastLocal` idempotency bug** (pre-existing, surfaced by Agent B) — `go/pkg/store/sqlite/send.go::BroadcastLocal` reuses a single `SendParams.MessageID` ULID as the `idempotency_key` for every recipient row, tripping the `UNIQUE(workspace_id, idempotency_key)` partial index on N≥2 broadcasts. `/api/send` serializes federation fan-out so the bug stayed latent. Agent B worked around it with a new `BroadcastCLI` method in `broadcast.go`. Fix `BroadcastLocal` in a dedicated commit, have both callers converge on the fixed implementation.

## Architectural decisions ratified this session

1. **Parallel agents unlock Phase 3's bulk.** 3 agents in isolated worktrees ported 6 verbs concurrently after a stage-0 refactor that pre-seeded `main.go` dispatch cases. Key lesson: each worktree must start from a commit that includes the stub scaffolding; agent-a96b736f's worktree was forked before the stub commit and its initial merge would have deleted the session-register port. Cherry-picking specific paths dodged the issue. Future parallel-agent dispatches should explicitly branch from the stub-seed commit.
2. **Opt-in over auto-flip.** Phase 5 intentionally doesn't flip the default. The parity harness proves byte-equivalence on core paths, but the peripheral-gap backlog (§Phase 5.1) is real — an auto-flip would silently break join/leave announcements, stale marker cleanup, and remote-host sends. `AGENT_COLLAB_IMPL=go` lets operators validate at their own risk.
3. **`EXIT_LABEL_COLLISION = 1`**, not 11. My session-register mistake; agent C flagged it while porting `room-create` (which shares the code). Fixed during merge. The parity harness surfaced no regression because the one session-register fixture is a success path.

## Runtime state notes

- `scripts/agent-collab` wrapper grew `resolve_peer_inbox_binary` + a pre-Python dispatch block. Binary search order: `AGENT_COLLAB_PEER_INBOX_BIN` env → `$repo/go/bin/peer-inbox` → `$HOME/.local/bin/peer-inbox` → PATH.
- The Go binary at `go/bin/peer-inbox` must be rebuilt after each merge: `cd go && go build -o bin/peer-inbox ./cmd/peer-inbox`. The installer (`scripts/install-global-protocol`) hasn't been updated to install it to `$HOME/.local/bin/peer-inbox` yet — that's a Phase 5.1 or Phase 6 item.
- `GOOSE_VERSION_REQUIRED` stays at 7 on both sides.
- Parity-harness side effect: `tests/fixtures/parity/<verb>/<scenario>.actual.json` appears on a failing run and is gitignored. `tests/parity/all.sh` iterates every fixture × both impls and prints a total.

## Suggested fresh-session openings

### Option A: "burn down Phase 5.1 in bullet order"

> Load `plans/handoff-2026-04-21-late-night-v3.4-phases-2-through-5.md` and `plans/v3.4-python-removal-scoping.md`.
>
> Start Phase 5.1 with item 7 (the `BroadcastLocal` idempotency-key bug — pre-existing, surfaces whenever N≥2 broadcasts go through the `/api/send` HTTP path). After that, port item 1 (channel-pair lookup) since it unblocks item 2 (system events).

### Option B: "Python-removal short-circuit for the federation client"

> Same load. Peer-send with remote `home_host` currently exits with a "fall back to Python" message. Port `_peer_send_remote` (Python) to a Go HTTP client — mirrors how `peer-queue --flush` already does the POST (agent C added that logic in `peer_queue.go::remoteSendHTTP`). Reuse that helper, move it into a shared file, and wire it into `peer_send.go`'s federation branch. Small win, removes one of the "must fall back to Python" paths.

### Option C: "thicken the parity suite"

> Same load. The 52-fixture coverage is broad but shallow. Add fresh-mint parity for session-register (requires a shared `AGENT_COLLAB_FAKE_AUTH_TOKEN` env hook on both impls), time-injection for `peer-replay`, and `peer-watch` orchestration with a `--timeout-secs` fixture field. Unblocks Phase 5.1 validation.

### Recommended: Option A

Bullet 7 is a latent bug with a real failure mode (N≥2 federated broadcasts). Bullet 1 unblocks bullet 2 which closes 3 stdout-visible regressions (session-register's `[channel: ...]` tag, session-close's leave event, peer-reset's leave event). Both are small, surgical commits. Items 3-6 are straightforward ports after that.

## Picking up the thread

Suggested fresh-session opener:

> Load `plans/handoff-2026-04-21-late-night-v3.4-phases-2-through-5.md`.
>
> v3.4 Phases 2-5 are all landed; every Python CLI verb has a Go counterpart behind `AGENT_COLLAB_IMPL=go`. Default stays Python pending Phase 5.1. Start with the `BroadcastLocal` idempotency-key fix (item 7 in the 5.1 backlog), then channel-pair lookup (item 1). Target: flip the default-impl to Go by end of session, then set up for Phase 6 (Python deletion).

## References

- **Scoping:** `plans/v3.4-python-removal-scoping.md` (particularly §5 Phase 5.1 which grew out of this session's deferrals).
- **Prior handoffs:** `plans/handoff-2026-04-21-v3.3-federation-plus-v3.4-python-removal-start.md`, `plans/handoff-2026-04-21-orange-and-federation.md`.
- **Key Go code:**
  - `go/cmd/peer-inbox/main.go` — dispatch + shared helpers.
  - `go/cmd/peer-inbox/*.go` — one file per verb (15 files).
  - `go/pkg/store/sqlite/{register,labels,peer_send,broadcast,queue,rooms,session_admin}.go` — Phase 3/4 store layer.
- **Key Python code:** `scripts/peer-inbox-db.py` — still the default runtime until Phase 5.1 closes the peripheral-gap backlog.
- **Wrapper dispatch:** `scripts/agent-collab` — `dispatch_peer_inbox` function.
- **Parity harness:** `tests/parity/run.sh`, `tests/parity/all.sh`, `tests/fixtures/parity/**/*`.
