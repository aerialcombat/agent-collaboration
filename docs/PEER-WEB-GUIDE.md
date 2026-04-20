# peer-web — Go dashboard guide

Live Slack-shaped browser view of peer-inbox traffic. Go port of the
Python `cmd_peer_web` with multi-room navigation, safety gates, and a
Python-free serving path.

## Quick start

```
agent-collab peer web
# open http://127.0.0.1:8787/
```

The `/` index lists every pair-key room on the machine. Click a room to
open the detail SPA at `/view?scope=pair_key&key=<K>`.

## Dispatch

`scripts/agent-collab peer web` picks an implementation in this order:

1. `AGENT_COLLAB_WEB_IMPL=python` — force Python fallback (bug reports).
2. `$AGENT_COLLAB_PEER_WEB_BIN` env override — explicit binary path.
3. `go/bin/peer-web` — dev builds in this repo.
4. `$HOME/.local/bin/peer-web` — installed.
5. `peer-web` on `PATH`.
6. Otherwise fall back to the Python `cmd_peer_web`.

Build locally with:

```
cd go && go build -o bin/peer-web ./cmd/peer-web/
```

## Flags

| Flag | Purpose |
|---|---|
| `--port N` | Bind port (default 8787). |
| `--pair-key K` | Deep-link shortcut — first visit redirects to room `K`. Server stays multi-room. |
| `--only-pair-key K` | Lock server to one room. Cross-scope requests get 403. |
| `--cwd PATH` | cwd-mode deep-link. |
| `--as LABEL` | Viewer label hint. |

`--pair-key` and `--only-pair-key` are mutually exclusive.

## Routes

**Pages:**

- `GET /` — multi-room index.
- `GET /view?scope=pair_key&key=K` — pair-key room detail SPA.
- `GET /view?scope=cwd&path=/abs/path` — cwd-mode detail SPA.

**API (JSON):**

- `GET /api/scope` — server config + implementation hint.
- `GET /api/index` — all pair-key rooms with summary (for the index page).
- `GET /api/pairs?pair_key=K` or `?cwd=P` — parity with Python `/pairs.json`.
- `GET /api/rooms?pair_key=K` — parity with Python `/rooms.json`.
- `GET /api/messages?pair_key=K&after=N` or `?cwd=P&a=L&b=M&after=N` — parity with Python `/messages.json`.
- `POST /api/send` + `?pair_key=K` — composer. Body: `{from, to, body}`. Shells to `peer-inbox-db.py peer-send` / `peer-broadcast`; auto-registers `owner` on first send in pair-key mode.
- `POST /api/rooms/terminate-inactive` — marks every stale room (activity=stale) as `peer_rooms.terminated_at`. Rejected under `--only-pair-key` lock.

## Features

**Multi-room navigation.** `/` lists every pair-key + activity dot +
unread badge + member pills; click to enter room. The detail page's
left sidebar also lists all rooms so you can jump between them without
returning to the index.

**Composer.** Bottom of each detail page. From/To dropdowns auto-fill
from members; `@room` broadcasts. `owner` (the human) is always
available as the From option and auto-registers on first send in
pair-key mode.

**Turn-cap meter.** Detail-page header shows `N/MAX turns (P%)`. Colors
yellow at ≥80% of `AGENT_COLLAB_MAX_PAIR_TURNS` (default 100). Warns
before you hit the cap mid-broadcast.

**Stale-member filter.** Detail-page sidebar has a `show stale members`
checkbox. Hides members whose `last_seen_at` is older than 10 min.
Default hidden. Persists across reloads via localStorage.

**Clear inactive rooms.** Index-page header has a red `clear inactive
(N)` button. Click → confirm dialog → POST `/api/rooms/terminate-inactive`.
Marks peer_rooms.terminated_at for stale rooms; data stays in DB.
Reversible via `agent-collab peer reset --pair-key K`. Disabled under
`--only-pair-key` lock.

**Terminated hidden by default.** Both index and detail sidebar hide
terminated rooms unless the header `show terminated` checkbox is
checked. Preference shared between pages via the
`peer-web:showTerminated` localStorage key. Subheader shows
`(N terminated hidden)` when filter is active.

## Safety

- **Localhost-only.** Binds `127.0.0.1` — no network exposure beyond the
  machine.
- **`--only-pair-key K`** locks the server to a single room so
  operators wanting narrow localhost exposure can replicate Python's
  single-scope semantic. Cross-scope reads, sends, and bulk-ops all
  return 400/403.
- **Clear inactive is reversible.** Terminates via
  `peer_rooms.terminated_at`; no hard deletes. Roll back with
  `agent-collab peer reset --pair-key K`.
- **Composer shells to Python** for `peer-send` / `peer-broadcast` to
  reuse validation, room-cap, termination, and push semantics 1:1 with
  the existing CLI. No duplicated send-path logic.

## Parity

The Go binary's JSON responses on `/api/scope`, `/api/pairs`,
`/api/rooms`, `/api/messages` are byte-identical to the Python
`cmd_peer_web` endpoints on the same seeded DB. Verified by
`tests/peer-web-go-parity.sh` which spins up both servers on free
ports and diffs sorted JSON.

## Architecture

- `go/cmd/peer-web/main.go` — flag parsing, server lifecycle.
- `go/cmd/peer-web/server/server.go` — HTTP routing, `embed.FS` static assets.
- `go/cmd/peer-web/server/scope.go` — `/api/scope` handler.
- `go/cmd/peer-web/server/data.go` — read endpoints (`/api/pairs`, `/api/rooms`, `/api/messages`, `/api/index`, `/api/rooms/terminate-inactive`).
- `go/cmd/peer-web/server/send.go` — composer POST handler + `owner` auto-register.
- `go/cmd/peer-web/server/views.go` — HTML page handlers with `__TITLE__` / `__CWD__` substitution.
- `go/cmd/peer-web/server/static/` — embedded `index.html` (index SPA) + `view.html` (detail SPA, ported from Python `_PEER_WEB_HTML_TEMPLATE`).
- `go/pkg/store/sqlite/web.go` — `FetchPairs`, `FetchRooms`, `FetchMessages`, `AllPairKeys`, `SenderCWD`, `ClearChannelSocket`, `TerminateRoom` methods on `SQLiteLocal`. Read-only paths; send uses Python subprocess.

Scoping rationale + decisions: `plans/v3.2-frontend-go-rewrite-scoping.md`.
