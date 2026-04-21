# Handoff — 2026-04-21 — Orange deploy shipped + federation brainstorm

**Status:** v3.2 frontend-go-rewrite shipped (laptop dashboard) + orange Linux server deploy shipped (remote agents live) + federation-across-hosts brainstorm in progress. · **Session:** `grand-nebula-88df` (owner ↔ claude-rewrite, context-heavy; owner opening fresh session to continue) · **Next session's job:** pick a federation shape for "laptop agents ↔ orange agents in one room."

Load this doc + referenced files to resume. Full transcript unnecessary.

## Required reading (in order)

1. `plans/v3.2-frontend-go-rewrite-scoping.md` — Go peer-web rewrite spec + §13 landed record. Explains the web dashboard + API surface that everything else layers on.
2. `plans/v3.x-server-deployment-scoping.md` — server-deploy spec. §7 closure checklist captures what's done vs pending.
3. `docs/PEER-WEB-GUIDE.md` — operator doc for the Go dashboard (routes, flags, safety, architecture).
4. This doc's §Brainstorm below — federation options A–G + how oozoo layering plays in.

## Commits today (origin/main)

```
fc1e5dd..7150de1
```

In order:
- `f11dbc3` feat(go): peer-web multi-room Go dashboard (v3.2 frontend rewrite)
- `da3f3bb` plan(deploy): v3.x server-deployment scoping
- `642f2d5` feat(deploy): bootstrap script + systemd units + env template
- `64f76fe` feat(deploy): port preflight + user-scoped Go + configurable bind
- `fc1e5dd` feat(deploy): install Node 20 + agent CLIs in bootstrap
- `7150de1` feat(peer-inbox): pi continuing-session peer + composer shell-out + 'human' agent

Not committed as a plans/ doc yet but captured in this handoff:
- Federation brainstorm (§Brainstorm below).
- Orange-specific operational gotchas hit during deploy (§Runtime state notes).

## Runtime state

### Orange (remote host)

- **Host:** `orange-1.tail48d298.ts.net` (tailnet-only) — IP `104.234.174.120`, Ubuntu 24.04, runs owner's SaaS (viib.in, formdrop.io, …) alongside agent-collab.
- **Repo:** `/opt/agent-collab` at HEAD `7150de1`.
- **agent-collab user:** system user `agent-collab`, home `/var/lib/agent-collab`. Go 1.25.5 installed user-scoped at `~agent-collab/go/` (host's system Go 1.22 untouched).
- **Binaries:** `/usr/local/bin/{agent-collab-daemon, peer-web, peer-inbox, peer-inbox-hook, peer-inbox-migrate, agent-collab, claude, pi}` all symlinked/installed.
- **Systemd:**
  - `agent-collab-peer-web.service` — active, listens `127.0.0.1:8787`
  - `agent-collab-daemon@alpha.service` — active, polls every 2s, spawns claude on peer-inbox events
- **Tailscale:** `tailscale serve --bg --https=443 http://localhost:8787` — peer-web reachable at `https://orange-1.tail48d298.ts.net/`
- **Cloudflare / nginx:** the `collab.c0d3r2.dev` + `agents.c0d3r2.dev` CNAMEs were wired through nginx earlier then torn down. `sites-available/collab.c0d3r2.dev` retained for easy re-enable; sites-enabled symlink removed. Public URL currently returns nginx 404.
- **Secrets:** `/etc/agent-collab/env` (0640 root:agent-collab) has `CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...`. Token was exposed in prior session context — recommend rotating via `claude setup-token` when convenient.
- **Room in flight:** `perky-dove-be3a` — members `alpha` (daemon claude), `beta` (interactive claude; was pointing at a remote laptop session during brainstorm), `owner` (human).

### Laptop (owner's Mac)

- Repo at HEAD `7150de1`, up to date.
- `~/.agent-collab/sessions.db` — separate from orange's. Has laptop-local rooms (fluffy-beaver-bc6f, lavender-swallow-ac19, lush-jaguar-bf28, mindful-cedar-c5a6, …).
- Owner's remote-claude session uses the peer-web HTTP API (`/api/send`, `/api/messages`) against orange, NOT local agent-collab CLI.
- `grand-nebula-88df` pair-room on laptop.db (claude-rewrite + owner) — used as the working channel during this session.

### Operational gotchas discovered during deploy

These surfaced during first-ever deploy on orange. Some are already patched in the bootstrap; others are runtime quirks to be aware of.

- **git safe.directory** — `/opt/agent-collab` is joe-owned; `install-global-protocol` runs as `agent-collab`. Go's build-VCS stamping trips "dubious ownership." Fix: `sudo -u agent-collab git config --global --add safe.directory /opt/agent-collab` before first bootstrap. Not yet baked into bootstrap — **patch candidate**.
- **Apt default Node is 18; pi needs 20+.** Fixed in `fc1e5dd` via NodeSource.
- **agent-collab CLI not on default PATH.** `install-global-protocol` writes to `~agent-collab/.local/bin/`. Daemon's spawned claude runs with minimal PATH and can't find `agent-collab peer send`. Fix: symlink `/usr/local/bin/agent-collab` → `.local/bin/agent-collab`. **Not yet baked into bootstrap — patch candidate.**
- **claude permissions.** Spawned claude rejects `Bash(agent-collab peer send)` without approval. Fix: add `permissions.allow: ["Bash(agent-collab peer send:*)", "Bash(agent-collab peer broadcast:*)", "Bash(agent-collab peer receive:*)", "Bash(agent-collab peer list:*)"]` to `~agent-collab/.claude/settings.json`. **Not yet baked into install-global-protocol — patch candidate.**
- **Session marker needed at daemon's cwd.** `agent-collab peer send` resolves self via marker files in `<cwd>/.agent-collab/sessions/`. Daemon's configured cwd must have one (produced by `session register --cwd <cwd>`).
- **receive_mode=daemon required.** Daemon exits 78/CONFIG if the session isn't registered with `--receive-mode daemon`. Our Python CLI documents this only in `docs/DAEMON-OPERATOR-GUIDE.md`; first deploys trip it.
- **Claude setup-token needs a TTY.** Headless ssh + systemd-run fail with Ink raw-mode error. Workaround: `tmux new-session -d` + `tmux send-keys` to drive interactively from a remote orchestrator. Or just ssh in with `-t` and paste yourself.
- **HTTP API only auto-registers `owner`.** My `handleSend` in `go/cmd/peer-web/server/send.go` hard-codes auto-register for label `owner` (human, pair-key mode). Other labels get 404. For beta (remote session) we registered manually via ssh. **Enhancement candidate.**
- **Composer + `human` agent.** Python CLI's `VALID_AGENTS` didn't accept `"human"` until `7150de1`. If a new host clones before that tag it'll hit the same bug.

### Session context (peer-inbox rooms)

`grand-nebula-88df` has accumulated rich conversation between owner and claude-rewrite covering: v3.2 rewrite dogfood, clear-inactive UX iteration, orange deploy debugging, federation brainstorm. That transcript is the authoritative record for today's decisions if anything here is unclear.

## Federation brainstorm (to be continued in fresh session)

Owner wants: **laptop agents and orange agents talking in the same room.** Today they can't — each host has its own `sessions.db` and they're unaware of each other.

Seven architectural shapes considered (explained in plain-English "post office" terms during the session):

| | Shape | One-liner | Effort |
|---|---|---|---|
| A | **Host-aware CLI routing** | Laptop's `agent-collab` checks a `remotes.json` config; remote rooms → HTTP to orange's `/api/send`. Orange remains source of truth. | **Days.** Ship next week. |
| B | **Full gossip federation** | Each host has its own DB; a sync daemon replicates inbox rows with UUIDs + vector clocks. Survives offline windows. | Weeks. Real DB engineering. |
| C | **Broker/pub-sub relay** | Dedicated real-time signaling layer; DBs optional. | Medium. Single point of failure. |
| D | **Shared DB over tailnet (sshfs/NFS)** | Laptop mounts orange's DB file. Zero code changes. | Hours. Breaks on SQLite locking. |
| E | **Swap SQLite → Postgres** | Orange runs Postgres, all hosts connect via tailnet. Real multi-client DB. | Weeks. pgx driver already in go.mod. |
| F | **Append-only event log** | Shared log; hosts replay to build local view. Perfect federation shape theoretically. | Overkill for 2–3 hosts. |
| G | **Use oozoo WebSocket hub as relay** | Repurpose existing oozoo infra as the transport. | Medium. Data-model shoehorn (oozoo is user↔agent dyads, peer-inbox is rooms). |

### Drafter's recommendation

- **If goal = "laptop ↔ orange agent convo this week":** A (host-aware CLI routing).
- **If goal = "my collective should survive orange being offline":** E (Postgres) — not raw federation.
- **B is rarely the right answer** for a someone-else's-problem; it's for building the federation platform itself.

### Oozoo integration layering

Key insight: **oozoo is a presentation layer, not a data layer.** Two orthogonal patterns:

- **Pattern 1 — oozoo as viewer.** Native-mobile peer-web. Doesn't force any federation choice.
- **Pattern 2 — peer-inbox agents registered as oozoo suppliers.** Product-shape: agents appear in oozoo's agent store, users install + chat dyadically. Peer-inbox becomes the agents' private coordination channel; oozoo is the public-facing store+chat. Oozoo's webhook → bridge → peer-inbox → agent → webhook back.

Pattern 2 is the real fusion of owner's two projects. Neither pattern forces A vs E vs etc.

### Open questions for next session

1. **What's the specific failure mode you want to solve first?** "Laptop agent talks to orange agent" (A is enough) vs. "collective keeps working when a node is down" (E is the real answer)?
2. **Is the #hosts likely to stay ≤2 (laptop + orange) or grow (laptop + orange + oozoo's node + phone + future VPS)?** If ≤2, A suffices indefinitely. If growing, E's operating cost amortizes.
3. **Does oozoo integration point toward Pattern 1 or Pattern 2?** Drives whether oozoo needs its own agent-supplier entries in peer-inbox or just API access.
4. **Near-term patch queue:** the four "not yet baked into bootstrap" items in §Runtime-state-notes above — when convenient, fold into bootstrap script:
   - git safe.directory for agent-collab user
   - /usr/local/bin/agent-collab symlink
   - permissions.allow block in settings.json
   - One-shot helper for registering a daemon config (replacing the manual `cp example.json.disabled + edit + systemctl enable` dance)

## Picking up the thread

Suggested fresh-session opening:

> Load `plans/handoff-2026-04-21-orange-and-federation.md`.
>
> Recap: orange is live as our agent host. Federation across laptop + orange is the open question. Drafter leans A (host-aware CLI routing) if goal is "one collective across hosts this week", E (Postgres) if goal is "node-loss resilience." Oozoo layers on top of either. Which shape are we picking + what's the failure mode we're optimizing for?

## References

- **Session's working room** (transcript of today's decisions): peer-inbox `grand-nebula-88df` on laptop.db — owner ↔ claude-rewrite.
- **Deploy-dogfood room** (what a real agent interaction looks like on orange): peer-inbox `perky-dove-be3a` on orange.db — alpha + beta + owner.
- **Scoping docs:** `plans/v3.2-frontend-go-rewrite-scoping.md`, `plans/v3.x-server-deployment-scoping.md`.
- **Operator doc:** `docs/PEER-WEB-GUIDE.md` (new in v3.2).
- **Runtime config on orange:** `/etc/agent-collab/env`, `/var/lib/agent-collab/.agent-collab/daemons/alpha.json`, `/var/lib/agent-collab/.claude/settings.json`.
