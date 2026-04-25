#!/usr/bin/env python3
"""Claude Code Channels plugin — peer-inbox production channel.

Spawned as a stdio MCP subprocess of a `claude --channels server:peer-inbox
--dangerously-load-development-channels` session. Real-time push: when
another session's `agent-collab peer send` POSTs to our Unix socket, we
emit a `notifications/claude/channel` into the running session's context
without waiting for a user prompt.

Pairing with a session label happens lazily at `agent-collab session
register` time — the helper walks its process tree to find Claude's PID,
looks up the pending-channels registration, and binds (cwd, label) →
socket_path in sessions.db. See scripts/peer-inbox-db.py.

Fail-open: if anything goes wrong, the hook path still delivers on the
next real turn.

No external deps. Python 3.9+. Stdlib only.
"""
from __future__ import annotations

import atexit
import json
import os
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SERVER_NAME = "peer-inbox"
SERVER_VERSION = "1.0.0"
PROTOCOL_VERSION = "2025-03-26"

def _default_socket_dir() -> str:
    """Pick a short socket dir — AF_UNIX paths are capped at 104 chars on macOS.
    Prefer /tmp on POSIX, fall back to tempfile.gettempdir() otherwise.
    """
    candidate = "/tmp/peer-inbox"
    tmp_candidate = str(Path(tempfile.gettempdir()) / "peer-inbox")
    # Use /tmp if it exists and is writable; otherwise fall back.
    if os.path.isdir("/tmp") and os.access("/tmp", os.W_OK):
        return candidate
    return tmp_candidate


SOCKET_DIR = Path(os.environ.get("PEER_INBOX_SOCKET_DIR", _default_socket_dir()))
PENDING_DIR = Path(os.environ.get(
    "PEER_INBOX_PENDING_DIR",
    str(Path.home() / ".agent-collab" / "pending-channels"),
))
DB_PATH = Path(os.environ.get(
    "AGENT_COLLAB_INBOX_DB",
    str(Path.home() / ".agent-collab" / "sessions.db"),
))
MAX_BODY_BYTES = 8 * 1024

REPLY_TOOL = {
    "name": "peer_inbox_reply",
    "description": (
        "Send a message into the room. Three delivery modes, picked "
        "by `to`:\n"
        "  • omit `to`            → broadcast to the whole room\n"
        "  • `to: \"alice\"`       → 1:1 direct (side-chat or private reply)\n"
        "  • `to: [\"a\", \"b\"]`  → multicast to a named subset\n"
        "\n"
        "Optional `mention` (string or array) tags peers as primary "
        "responder. `@label` tokens in `body` are auto-parsed into "
        "mentions. Mentions compose with any delivery mode.\n"
        "\n"
        "Which mode? Depends on room state (read the incoming "
        "<channel> meta):\n"
        "\n"
        "PAIR (room_size=2): reply naturally. No ambiguity.\n"
        "\n"
        "PARTY (room_size>=3, no meta.has_mediator): like a Slack "
        "channel. Default to BROADCAST; only go 1:1 for genuine "
        "side-chats. Start threads, pull peers aside, jump into "
        "conversations that interest you. Silence is fine; so is "
        "flagging that the room could use a mediator if progress "
        "stalls on convergent work.\n"
        "\n"
        "MEDIATED (meta.has_mediator=1): reply PRIVATELY 1:1 to the "
        "mediator (`to: \"<mediator-label>\"`) when asked. Don't "
        "broadcast. Don't address other participants directly — let "
        "the mediator route attention. The mediator strips attribution "
        "in summaries, so it's safe (and useful) to disagree openly.\n"
        "\n"
        "Budget: 100-turn room cap shared across all members. A "
        "broadcast or multicast is ONE turn regardless of recipients. "
        "Don't monologue."
    ),
    "inputSchema": {
        "type": "object",
        "properties": {
            "body": {
                "type": "string",
                "description": "Message text. UTF-8, max 8 KB.",
            },
            "to": {
                "description": (
                    "Recipient(s). Omit to broadcast, pass a string for "
                    "1:1, or pass an array of labels for multicast."
                ),
                "oneOf": [
                    {"type": "string"},
                    {"type": "array", "items": {"type": "string"}, "minItems": 1},
                ],
            },
            "mention": {
                "description": (
                    "Tag peer(s) as primary responder. Orthogonal to `to` "
                    "— broadcast + mention = public @ping."
                ),
                "oneOf": [
                    {"type": "string"},
                    {"type": "array", "items": {"type": "string"}, "minItems": 1},
                ],
            },
            "pair_key": {
                "type": "string",
                "description": (
                    "Target room for this reply. Only meaningful when the "
                    "session is joined into multiple rooms over the same "
                    "channel (see the `pair_key` meta on incoming <channel> "
                    "tags). Omit to default to the room the most recent "
                    "incoming message came from; when the session is in "
                    "exactly one room, can be omitted unconditionally."
                ),
            },
        },
        "required": ["body"],
        "additionalProperties": False,
    },
}

# v3.9 kanban tools. The board is the durable-work medium; chat is the
# volatile medium. Every tool is a thin wrapper around the `peer-inbox
# card-*` CLI verbs (go/cmd/peer-inbox/cards.go) so the DB layer stays
# single-sourced. Tools shell out with `--format json`, parse, and
# return the parsed object as the tool result.
CARD_TOOLS = [
    {
        "name": "card_create",
        "description": (
            "Create a kanban card — a durable, addressed work item that "
            "persists across sessions. Pair with chat messages: use chat "
            "for \"what if / heads up\", use cards for \"this is for you, "
            "do it, mark done when done.\"\n"
            "Cards have a status (todo | in_progress | in_review | done | "
            "cancelled) and can carry a role (who should claim it) plus "
            "tags (epics/groupings). Reference chat messages or files via "
            "context_refs. Use card_add_dependency afterwards to wire "
            "blockers."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "pair_key": {"type": "string", "description": "Room scope (required)."},
                "title": {"type": "string", "description": "One-line summary (required)."},
                "body": {"type": "string", "description": "Markdown spec / description."},
                "needs_role": {
                    "type": "string",
                    "description": "Role that should claim (e.g. ios, web, qa, review). Agents pull by role.",
                },
                "priority": {
                    "type": "integer",
                    "enum": [-1, 0, 1],
                    "description": "-1 low, 0 normal, 1 high.",
                },
                "tags": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Grouping labels (epics, themes). M:N: a card can have multiple.",
                },
                "context_refs": {
                    "type": "object",
                    "description": "Pointers into chat/code: {msg_ids:[], files:[], urls:[], cards:[]}.",
                },
            },
            "required": ["pair_key", "title"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_list",
        "description": (
            "List cards. Defaults to all cards in the caller's current "
            "room(s). Key filter for agents: `ready_only: true` returns "
            "status=todo cards with no pending blockers — your actionable "
            "queue. `claimed_by: <your-label>` for your in-flight work."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "pair_key": {"type": "string"},
                "status": {
                    "type": "string",
                    "enum": ["todo", "in_progress", "in_review", "done", "cancelled"],
                },
                "needs_role": {"type": "string"},
                "claimed_by": {"type": "string"},
                "ready_only": {"type": "boolean", "description": "status=todo AND no open blockers"},
                "blocked_only": {"type": "boolean", "description": "status=todo AND ≥1 open blocker"},
                "limit": {"type": "integer", "minimum": 1},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "card_get",
        "description": "Fetch one card by id. Returns full row + blocker_ids + blockee_ids + derived `ready`.",
        "inputSchema": {
            "type": "object",
            "properties": {"id": {"type": "integer", "minimum": 1}},
            "required": ["id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_claim",
        "description": (
            "Claim a card: atomically set claimed_by + bump status "
            "todo→in_progress. Fails if another label holds it (pass "
            "force=true to override)."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "id": {"type": "integer", "minimum": 1},
                "force": {"type": "boolean"},
            },
            "required": ["id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_update_status",
        "description": (
            "Transition a card's status. todo | in_progress | in_review | "
            "done | cancelled. done/cancelled terminal states stamp "
            "completed_at. No rollup — parent status isn't auto-derived."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "id": {"type": "integer", "minimum": 1},
                "status": {
                    "type": "string",
                    "enum": ["todo", "in_progress", "in_review", "done", "cancelled"],
                },
            },
            "required": ["id", "status"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_update",
        "description": (
            "Mutate non-status fields on a card: title, body (markdown), "
            "needs_role, priority, tags, context_refs. Status, claim "
            "ownership, and dependencies have their own dedicated tools. "
            "Pass only the fields you want to change. To clear a string "
            "field, pass an empty string."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "id": {"type": "integer", "minimum": 1},
                "title": {"type": "string"},
                "body": {"type": "string"},
                "needs_role": {"type": "string"},
                "priority": {"type": "integer", "minimum": -1, "maximum": 1},
                "tags": {"type": "array", "items": {"type": "string"}},
                "context_refs": {"type": "object"},
            },
            "required": ["id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_add_dependency",
        "description": (
            "Add a blocker → blockee edge. Blockee can't become ready "
            "until blocker hits done/cancelled. Cycles are rejected."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "blocker_id": {"type": "integer", "minimum": 1},
                "blockee_id": {"type": "integer", "minimum": 1},
            },
            "required": ["blocker_id", "blockee_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "card_remove_dependency",
        "description": "Drop a blocker → blockee edge. No-op if it didn't exist.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "blocker_id": {"type": "integer", "minimum": 1},
                "blockee_id": {"type": "integer", "minimum": 1},
            },
            "required": ["blocker_id", "blockee_id"],
            "additionalProperties": False,
        },
    },
]

_stdout_lock = threading.Lock()
_initialized = threading.Event()
_socket_path: Path | None = None
_pending_path: Path | None = None

# v3.6 multi-room: a single channel socket may be bound to multiple
# sessions rows (one per room the Claude joined). Incoming pushes carry
# meta.pair_key so we can (a) stamp the correct pair_key on the
# <channel> system-reminder and (b) default peer_inbox_reply back into
# the room the last message came from.
_last_incoming_lock = threading.Lock()
_last_incoming_pair_key: str | None = None


def stderr(*args):
    print(*args, file=sys.stderr, flush=True)


def send_json(msg: dict) -> None:
    line = json.dumps(msg, separators=(",", ":"), ensure_ascii=False)
    with _stdout_lock:
        sys.stdout.write(line + "\n")
        sys.stdout.flush()


def reply(req_id, result=None, error=None) -> None:
    msg = {"jsonrpc": "2.0", "id": req_id}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result if result is not None else {}
    send_json(msg)


def notify(method: str, params: dict) -> None:
    send_json({"jsonrpc": "2.0", "method": method, "params": params})


# ---- MCP handshake ----------------------------------------------------------


def handle_mcp(req: dict) -> None:
    method = req.get("method")
    req_id = req.get("id")

    if method == "initialize":
        reply(
            req_id,
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {
                    "experimental": {"claude/channel": {}},
                    "tools": {},
                },
                "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
                "instructions": (
                    "You're in a peer-inbox room. Every incoming "
                    "<channel source=\"peer-inbox\" from=\"...\"> tag "
                    "carries meta fields — read them first, pick mode.\n\n"
                    "Meta you care about:\n"
                    "  • no meta.broadcast     → DM; someone pulled you "
                    "aside. Answer.\n"
                    "  • meta.broadcast=1      → the room is talking.\n"
                    "  • meta.cohort=\"a,b,c\"   → multicast; named subset.\n"
                    "  • meta.mentions=YOU     → you're the primary "
                    "responder; lead.\n"
                    "  • meta.mentions=other   → they lead; chime in only "
                    "with substantive context.\n"
                    "  • meta.room_size / members / pair_key → roster.\n"
                    "  • meta.has_mediator=1 + meta.mediator=LABEL → a "
                    "facilitator is present; mediated mode applies.\n"
                    "  • meta.system=\"join\" → a new member just joined. "
                    "A brief greeting or a 'what brings you in?' is "
                    "natural — they may arrive with a topic or just "
                    "banter that stirs the room.\n"
                    "  • meta.system=\"leave\" → a member just left. "
                    "Acknowledge if relevant and carry on.\n"
                    "  • meta.from_role=\"owner\" OR "
                    "meta.from_agent=\"human\" → this message is from "
                    "the human running the room. Always respond — "
                    "even a broadcast without @mention — unless you "
                    "have nothing substantive to add (then a short "
                    "acknowledgement still beats silence). Party-mode "
                    "silence defaults do NOT apply to human pings.\n\n"
                    "Three modes, chosen by room state:\n\n"
                    "1) PAIR (room_size=2). It's just the two of you. "
                    "Reply naturally like any 1:1 — there's no ambiguity "
                    "about the recipient.\n\n"
                    "2) PARTY (room_size>=3, no mediator). Treat it like "
                    "a Slack channel or a party: multiple peers around, "
                    "side-chats happening, someone holding court. "
                    "Contribute naturally. Start threads, pull peers "
                    "aside, jump in when something grabs you. Default "
                    "reply is BROADCAST; only go 1:1 for genuine "
                    "side-chats. Silence is fine when you have nothing. "
                    "NOTE: party mode is productive for exploration but "
                    "struggles on convergent work. If the room is trying "
                    "to decide something and progress stalls, it's fair "
                    "to say so — 'room could use a mediator here' — "
                    "either broadcasting to the room or asking the human "
                    "to appoint one.\n\n"
                    "3) MEDIATED (meta.has_mediator=1). The named "
                    "mediator runs the round; you do NOT speak unprompted. "
                    "Specifically:\n"
                    "  • When the mediator broadcasts a topic or round "
                    "summary, read carefully and WAIT.\n"
                    "  • When the mediator DMs you, respond PRIVATELY — "
                    "reply 1:1 back with `to: \"<mediator>\"`. Do not "
                    "broadcast.\n"
                    "  • Give compressed substantive positions. Name "
                    "tradeoffs. Disagree openly — the mediator strips "
                    "attribution in summaries, so disagreement is safe.\n"
                    "  • Don't address other participants directly. The "
                    "mediator routes attention; you speak through them.\n"
                    "  • If the mediator broadcasts `[[converged]]`, the "
                    "round is closed; honor it and return to party mode.\n\n"
                    "Reply with peer_inbox_reply (preferred). Shell "
                    "fallback: `agent-collab peer broadcast --message ...`, "
                    "`peer broadcast --to a --to b --message ...`, "
                    "`peer send --to a --message ...`.\n\n"
                    "MULTI-ROOM (rare): when this session is joined into "
                    "multiple rooms at once, every <channel> tag's "
                    "meta.pair_key identifies which room it came from. "
                    "peer_inbox_reply defaults to the room of the most "
                    "recent incoming message; pass `pair_key: \"<key>\"` "
                    "to send into a different room explicitly."
                ),
            },
        )
        return
    if method == "notifications/initialized":
        _initialized.set()
        stderr(f"[peer-inbox-channel] initialized; socket={_socket_path}")
        return
    if method == "tools/list":
        reply(req_id, {"tools": [REPLY_TOOL] + CARD_TOOLS})
        return
    if method == "tools/call":
        handle_tools_call(req_id, req.get("params") or {})
        return
    if method in ("resources/list", "prompts/list"):
        key = "resources" if method == "resources/list" else "prompts"
        reply(req_id, {key: []})
        return
    if method == "ping":
        reply(req_id, {})
        return
    if method == "shutdown":
        reply(req_id, {})
        return
    if req_id is not None:
        reply(req_id, error={"code": -32601, "message": f"method not found: {method}"})


# ---- Reply tool -------------------------------------------------------------


def _agent_collab_bin() -> str:
    """Return absolute path (or PATH name) of the agent-collab driver."""
    env = os.environ.get("AGENT_COLLAB_BIN")
    if env and Path(env).is_file():
        return env
    candidate = Path.home() / ".local" / "bin" / "agent-collab"
    if candidate.is_file():
        return str(candidate)
    sibling = Path(__file__).resolve().parent / "agent-collab"
    if sibling.is_file():
        return str(sibling)
    return "agent-collab"


def _resolve_selves_from_socket() -> list[dict]:
    """Return every sessions row bound to this channel socket.

    In v3.6 a single Claude session can be registered into multiple
    rooms at once — each room gets its own (cwd, label, pair_key) row,
    all sharing the same channel_socket. Callers pick the row that
    matches the pair_key they care about (incoming meta, explicit
    reply arg, or the only-one-room fallback).
    """
    if not _socket_path or not DB_PATH.exists():
        return []
    socket_uri = f"unix://{_socket_path}"
    try:
        conn = sqlite3.connect(str(DB_PATH))
        conn.row_factory = sqlite3.Row
        try:
            # Match both the v3.5 URI form (`unix:///path/.sock`) and the
            # legacy bare-path form the Python daemon still writes.
            rows = conn.execute(
                "SELECT cwd, label, pair_key FROM sessions "
                "WHERE channel_socket = ? OR channel_socket = ? "
                "ORDER BY label",
                (str(_socket_path), socket_uri),
            ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as e:
        stderr(f"[peer-inbox-channel] sqlite error in resolve: {e}")
        return []
    return [
        {"cwd": r["cwd"], "label": r["label"], "pair_key": r["pair_key"]}
        for r in rows
    ]


def _pick_self(pair_key: str | None) -> dict | None:
    """Pick the (cwd, label, pair_key) row for the given pair_key.

    pair_key=None → return the unique row if the session is bound to
    exactly one room; otherwise return None (ambiguous). pair_key set →
    exact match; None when no such registration exists.
    """
    rows = _resolve_selves_from_socket()
    if not rows:
        return None
    if pair_key is not None:
        for r in rows:
            if r["pair_key"] == pair_key:
                return r
        return None
    if len(rows) == 1:
        return rows[0]
    return None


def _room_roster(pair_key: str | None) -> tuple[str | None, list[str], str | None] | None:
    """Look up roster + mediator for the room identified by pair_key.

    Returns (pair_key, members, mediator_label). `members` is sorted,
    self-inclusive. `mediator_label` is the label of whatever session
    in the room registered with role='mediator', or None if the room
    has no appointed mediator (party mode).

    pair_key=None falls back to "the only room this channel is bound
    to" — the v3.5-and-earlier single-room behaviour. When the session
    is in multiple rooms and no pair_key is given, we can't pick a
    roster, so we return None (the enrichment step just skips).
    """
    self_row = _pick_self(pair_key)
    if self_row is None:
        return None
    self_cwd = self_row["cwd"]
    self_label = self_row["label"]
    self_pair_key = self_row["pair_key"]
    try:
        conn = sqlite3.connect(str(DB_PATH))
        conn.row_factory = sqlite3.Row
        try:
            if self_pair_key:
                rows = conn.execute(
                    "SELECT label, role FROM sessions WHERE pair_key = ? "
                    "ORDER BY label",
                    (self_pair_key,),
                ).fetchall()
            else:
                rows = conn.execute(
                    "SELECT label, role FROM sessions WHERE cwd = ? "
                    "ORDER BY label",
                    (self_cwd,),
                ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as e:
        stderr(f"[peer-inbox-channel] sqlite error in roster: {e}")
        return (self_pair_key, [self_label], None)
    members = [r["label"] for r in rows]
    mediator = next(
        (r["label"] for r in rows if (r["role"] or "").lower() == "mediator"),
        None,
    )
    if self_label not in members:
        members.append(self_label)
    return (self_pair_key, sorted(members), mediator)


def _call_peer(
    self_cwd: str,
    self_label: str,
    subcmd: str,
    body: str,
    to: str | list[str] | None = None,
    mention: list[str] | None = None,
) -> tuple[bool, str]:
    """Invoke `agent-collab peer <subcmd>` as a subprocess. Returns (ok, message).

    `subcmd` is "send" (requires a single `to`) or "broadcast" (optional
    list of `--to` for multicast; no `--to` = room-wide).
    """
    cmd = [
        _agent_collab_bin(),
        "peer", subcmd,
        "--as", self_label,
        "--cwd", self_cwd,
    ]
    if subcmd == "send":
        if not isinstance(to, str) or not to:
            return False, "peer send requires a single `to` label"
        cmd.extend(["--to", to])
    elif subcmd == "broadcast" and isinstance(to, list):
        for t in to:
            cmd.extend(["--to", t])
    for m in mention or []:
        cmd.extend(["--mention", m])
    cmd.append("--message-stdin")
    try:
        res = subprocess.run(
            cmd, input=body, text=True, capture_output=True, timeout=10,
        )
    except (subprocess.TimeoutExpired, OSError) as e:
        return False, f"subprocess error: {e}"
    if res.returncode == 0:
        return True, (res.stdout.strip() or f"peer {subcmd} ok")
    stderr_text = (res.stderr or res.stdout).strip()
    return False, stderr_text or f"peer {subcmd} exit {res.returncode}"


# ---- Card tools (v3.9) ------------------------------------------------------


def _peer_inbox_bin() -> str:
    """Return absolute path to the Go peer-inbox binary, preferred over the
    agent-collab bash wrapper for card verbs (the wrapper currently only
    routes `peer` and `session` top-level modes — cards call the Go
    binary directly).
    """
    env = os.environ.get("AGENT_COLLAB_PEER_INBOX_BIN")
    if env and Path(env).is_file() and os.access(env, os.X_OK):
        return env
    for cand in (
        Path.home() / ".local" / "bin" / "peer-inbox",
        Path("/Users/deeJ/Development/agent-collaboration/go/bin/peer-inbox"),
    ):
        if cand.is_file() and os.access(cand, os.X_OK):
            return str(cand)
    return "peer-inbox"


def _resolve_caller_label_and_pair(arguments: dict) -> tuple[str | None, str | None]:
    """Pick the acting label + pair_key for a card tool call. Mirrors
    _pick_self logic: explicit `pair_key` arg wins, else the last
    incoming-message pair_key, else the only room this channel is in.
    Returns (label, pair_key) or (None, None) if resolution fails.
    """
    arg_pair = arguments.get("pair_key")
    selves = _resolve_selves_from_socket()
    if not selves:
        return (None, None)
    if isinstance(arg_pair, str) and arg_pair:
        chosen = next((r for r in selves if r["pair_key"] == arg_pair), None)
    else:
        chosen = _pick_self(None)
    if chosen is None:
        return (None, None)
    return (chosen["label"], chosen["pair_key"])


def _shell_card_verb(verb: str, flags: list[str]) -> tuple[bool, str, dict | list | None]:
    """Shell out to `peer-inbox <verb>` with --format json and parse the
    single-line output. Returns (ok, human_message, parsed_json_or_None).
    """
    cmd = [_peer_inbox_bin(), verb, *flags, "--format", "json"]
    try:
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=10)
    except (subprocess.TimeoutExpired, OSError) as e:
        return False, f"subprocess error: {e}", None
    if res.returncode != 0:
        return (
            False,
            (res.stderr or res.stdout).strip() or f"{verb} exit {res.returncode}",
            None,
        )
    out = (res.stdout or "").strip()
    if not out:
        return True, f"{verb} ok", None
    try:
        return True, "", json.loads(out)
    except json.JSONDecodeError as e:
        return False, f"decode error: {e}: {out[:200]}", None


def _tool_json_result(req_id, payload: dict | list | str) -> None:
    """Return a tool result as a single text block whose body is JSON.
    LLM clients render the JSON verbatim; reduces ambiguity vs.
    structured content blocks we'd need to shape per-tool."""
    if isinstance(payload, str):
        text = payload
    else:
        text = json.dumps(payload, ensure_ascii=False, indent=2)
    reply(req_id, {"content": [{"type": "text", "text": text}], "isError": False})


def _card_create(req_id, arguments: dict) -> None:
    title = arguments.get("title")
    if not isinstance(title, str) or not title:
        _tool_error(req_id, "error: `title` required")
        return

    label, pair_key = _resolve_caller_label_and_pair(arguments)
    if not pair_key:
        _tool_error(
            req_id,
            "error: no session bound to this channel — run `agent-collab session register` first.",
        )
        return

    flags = [
        "--pair-key", pair_key,
        "--title", title,
        "--created-by", label or "unknown",
    ]
    if isinstance(arguments.get("body"), str) and arguments["body"]:
        flags.extend(["--body", arguments["body"]])
    if isinstance(arguments.get("needs_role"), str) and arguments["needs_role"]:
        flags.extend(["--needs-role", arguments["needs_role"]])
    if isinstance(arguments.get("priority"), int):
        flags.extend(["--priority", str(arguments["priority"])])
    if isinstance(arguments.get("tags"), list):
        flags.extend(["--tags", json.dumps(arguments["tags"])])
    if isinstance(arguments.get("context_refs"), dict):
        flags.extend(["--context-refs", json.dumps(arguments["context_refs"])])

    ok, msg, parsed = _shell_card_verb("card-create", flags)
    if not ok:
        _tool_error(req_id, f"card_create: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else msg)


def _card_list(req_id, arguments: dict) -> None:
    label, pair_key = _resolve_caller_label_and_pair(arguments)
    # Caller can pass an explicit pair_key or none; if no session is
    # bound we still allow an unrestricted list so dashboards work
    # before registration (they just don't default to a room).
    flags: list[str] = []
    if pair_key and not arguments.get("pair_key"):
        flags.extend(["--pair-key", pair_key])
    elif isinstance(arguments.get("pair_key"), str):
        flags.extend(["--pair-key", arguments["pair_key"]])

    for k, cli in (
        ("status", "--status"),
        ("needs_role", "--needs-role"),
        ("claimed_by", "--claimed-by"),
    ):
        v = arguments.get(k)
        if isinstance(v, str) and v:
            flags.extend([cli, v])
    if arguments.get("ready_only"):
        flags.append("--ready-only")
    if arguments.get("blocked_only"):
        flags.append("--blocked-only")
    if isinstance(arguments.get("limit"), int) and arguments["limit"] > 0:
        flags.extend(["--limit", str(arguments["limit"])])

    ok, msg, parsed = _shell_card_verb("card-list", flags)
    if not ok:
        _tool_error(req_id, f"card_list: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else [])


def _card_get(req_id, arguments: dict) -> None:
    cid = arguments.get("id")
    if not isinstance(cid, int) or cid < 1:
        _tool_error(req_id, "error: `id` required and must be a positive integer")
        return
    ok, msg, parsed = _shell_card_verb("card-get", ["--id", str(cid)])
    if not ok:
        _tool_error(req_id, f"card_get: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


def _card_claim(req_id, arguments: dict) -> None:
    cid = arguments.get("id")
    if not isinstance(cid, int) or cid < 1:
        _tool_error(req_id, "error: `id` required")
        return
    label, _ = _resolve_caller_label_and_pair(arguments)
    if not label:
        _tool_error(req_id, "error: no session bound — cannot resolve claimer label")
        return
    flags = ["--id", str(cid), "--as", label]
    if arguments.get("force"):
        flags.append("--force")
    ok, msg, parsed = _shell_card_verb("card-claim", flags)
    if not ok:
        _tool_error(req_id, f"card_claim: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


def _card_update_status(req_id, arguments: dict) -> None:
    cid = arguments.get("id")
    status = arguments.get("status")
    if not isinstance(cid, int) or cid < 1:
        _tool_error(req_id, "error: `id` required")
        return
    if not isinstance(status, str) or not status:
        _tool_error(req_id, "error: `status` required")
        return
    ok, msg, parsed = _shell_card_verb(
        "card-update-status", ["--id", str(cid), "--status", status]
    )
    if not ok:
        _tool_error(req_id, f"card_update_status: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


def _card_update(req_id, arguments: dict) -> None:
    cid = arguments.get("id")
    if not isinstance(cid, int) or cid < 1:
        _tool_error(req_id, "error: `id` required (positive integer)")
        return
    flags = ["--card", str(cid)]
    if "title" in arguments:
        flags += ["--title", str(arguments["title"])]
    if "body" in arguments:
        flags += ["--body", str(arguments["body"])]
    if "needs_role" in arguments:
        flags += ["--needs-role", str(arguments["needs_role"])]
    if "priority" in arguments:
        pr = arguments["priority"]
        if not isinstance(pr, int) or pr < -1 or pr > 1:
            _tool_error(req_id, "error: `priority` must be an integer in [-1, 1]")
            return
        flags += ["--priority", str(pr)]
    if "tags" in arguments:
        tags = arguments["tags"]
        if not isinstance(tags, list):
            _tool_error(req_id, "error: `tags` must be an array of strings")
            return
        flags += ["--tags", json.dumps(tags)]
    if "context_refs" in arguments:
        refs = arguments["context_refs"]
        if not isinstance(refs, dict):
            _tool_error(req_id, "error: `context_refs` must be an object")
            return
        flags += ["--context-refs", json.dumps(refs)]
    if len(flags) == 2:
        _tool_error(req_id, "error: pass at least one mutable field "
            "(title, body, needs_role, priority, tags, context_refs)")
        return
    label, _ = _resolve_caller_label_and_pair(arguments)
    if label:
        flags += ["--as", label]
    ok, msg, parsed = _shell_card_verb("card-update", flags)
    if not ok:
        _tool_error(req_id, f"card_update: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


def _card_add_dependency(req_id, arguments: dict) -> None:
    blocker = arguments.get("blocker_id")
    blockee = arguments.get("blockee_id")
    if not (isinstance(blocker, int) and isinstance(blockee, int) and blocker > 0 and blockee > 0):
        _tool_error(req_id, "error: `blocker_id` and `blockee_id` required (positive integers)")
        return
    label, _ = _resolve_caller_label_and_pair(arguments)
    ok, msg, parsed = _shell_card_verb(
        "card-add-dep",
        ["--blocker", str(blocker), "--blockee", str(blockee), "--as", label or "unknown"],
    )
    if not ok:
        _tool_error(req_id, f"card_add_dependency: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


def _card_remove_dependency(req_id, arguments: dict) -> None:
    blocker = arguments.get("blocker_id")
    blockee = arguments.get("blockee_id")
    if not (isinstance(blocker, int) and isinstance(blockee, int) and blocker > 0 and blockee > 0):
        _tool_error(req_id, "error: `blocker_id` and `blockee_id` required (positive integers)")
        return
    ok, msg, parsed = _shell_card_verb(
        "card-remove-dep",
        ["--blocker", str(blocker), "--blockee", str(blockee)],
    )
    if not ok:
        _tool_error(req_id, f"card_remove_dependency: {msg}")
        return
    _tool_json_result(req_id, parsed if parsed is not None else {})


CARD_TOOL_HANDLERS = {
    "card_create": _card_create,
    "card_list": _card_list,
    "card_get": _card_get,
    "card_claim": _card_claim,
    "card_update_status": _card_update_status,
    "card_update": _card_update,
    "card_add_dependency": _card_add_dependency,
    "card_remove_dependency": _card_remove_dependency,
}


def _tool_error(req_id, text: str) -> None:
    reply(req_id, {
        "content": [{"type": "text", "text": text}],
        "isError": True,
    })


def handle_tools_call(req_id, params: dict) -> None:
    name = params.get("name")
    arguments = params.get("arguments") or {}
    if not isinstance(arguments, dict):
        _tool_error(req_id, "error: arguments must be an object")
        return
    if name in CARD_TOOL_HANDLERS:
        CARD_TOOL_HANDLERS[name](req_id, arguments)
        return
    if name != REPLY_TOOL["name"]:
        reply(req_id, error={
            "code": -32602,
            "message": f"unknown tool: {name}",
        })
        return

    body = arguments.get("body")
    to = arguments.get("to")
    mention = arguments.get("mention")
    pair_key_arg = arguments.get("pair_key")
    if pair_key_arg is not None and (
        not isinstance(pair_key_arg, str) or not pair_key_arg
    ):
        _tool_error(req_id, "error: `pair_key` must be a non-empty string or omitted")
        return
    if not isinstance(body, str) or not body:
        _tool_error(req_id, "error: `body` is required and must be a non-empty string")
        return
    body_bytes = len(body.encode("utf-8"))
    if body_bytes > MAX_BODY_BYTES:
        _tool_error(req_id, f"error: body too large ({body_bytes} > {MAX_BODY_BYTES} bytes)")
        return

    # Normalize `to`: None (broadcast), str (1:1), or list[str] (multicast).
    to_list: list[str] | None = None
    if to is None:
        pass
    elif isinstance(to, str):
        if not to:
            _tool_error(req_id, "error: `to` string must be non-empty")
            return
    elif isinstance(to, list):
        if not to or not all(isinstance(t, str) and t for t in to):
            _tool_error(
                req_id,
                "error: `to` array must be a non-empty list of non-empty strings",
            )
            return
        to_list = to
    else:
        _tool_error(req_id, "error: `to` must be a string, an array of strings, or omitted")
        return

    # Normalize `mention`: None | str | list[str] → list[str] | None.
    mention_list: list[str] | None = None
    if mention is None:
        pass
    elif isinstance(mention, str):
        if mention:
            mention_list = [mention]
    elif isinstance(mention, list):
        if not all(isinstance(m, str) and m for m in mention):
            _tool_error(req_id, "error: `mention` array must be non-empty strings")
            return
        mention_list = list(mention) or None
    else:
        _tool_error(req_id, "error: `mention` must be a string, an array, or omitted")
        return

    # v3.6 multi-room resolution. Precedence for picking which room to
    # reply into:
    #   1. explicit `pair_key` argument
    #   2. the pair_key of the most recent incoming message on this socket
    #   3. the unique registered room (single-room fallback; pre-v3.6 default)
    # When none of those resolves, error with a listing of available rooms
    # so the caller can retry with `pair_key=...`.
    all_selves = _resolve_selves_from_socket()
    if not all_selves:
        _tool_error(
            req_id,
            "error: no session bound to this channel yet — "
            "run `agent-collab session register` first.",
        )
        return

    chosen: dict | None = None
    if pair_key_arg is not None:
        chosen = next(
            (r for r in all_selves if r["pair_key"] == pair_key_arg), None
        )
        if chosen is None:
            keys = sorted(r["pair_key"] or "" for r in all_selves)
            _tool_error(
                req_id,
                f"error: no registered room with pair_key={pair_key_arg!r}; "
                f"known pair_keys: {keys}",
            )
            return
    else:
        with _last_incoming_lock:
            last_pk = _last_incoming_pair_key
        if last_pk:
            chosen = next(
                (r for r in all_selves if r["pair_key"] == last_pk), None
            )
        if chosen is None:
            if len(all_selves) == 1:
                chosen = all_selves[0]
            else:
                keys = sorted(r["pair_key"] or "" for r in all_selves)
                _tool_error(
                    req_id,
                    "error: session is in "
                    f"{len(all_selves)} rooms and no `pair_key` given "
                    "(and no recent incoming message to infer from). "
                    f"Pass `pair_key` (one of: {keys}) to disambiguate.",
                )
                return

    self_cwd = chosen["cwd"]
    self_label = chosen["label"]

    if isinstance(to, str):
        ok, msg = _call_peer(
            self_cwd, self_label, "send", body, to=to, mention=mention_list,
        )
    elif to_list is not None:
        ok, msg = _call_peer(
            self_cwd, self_label, "broadcast", body, to=to_list, mention=mention_list,
        )
    else:
        ok, msg = _call_peer(
            self_cwd, self_label, "broadcast", body, mention=mention_list,
        )
    reply(req_id, {
        "content": [{"type": "text", "text": msg}],
        "isError": not ok,
    })


def stdio_loop() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            stderr(f"[peer-inbox-channel] bad JSON: {e}")
            continue
        try:
            handle_mcp(req)
        except Exception as e:
            stderr(f"[peer-inbox-channel] handler error: {e}")


# ---- Unix-socket receiver ---------------------------------------------------
#
# Sessions POST to us using a tiny HTTP/1.1 request over AF_UNIX. Body is
# a JSON object: {"from": "backend", "body": "...", "meta": {...}?}.


def read_http_request(conn: socket.socket) -> tuple[str, dict, bytes]:
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            break
        data += chunk
        if len(data) > 1024 * 1024:  # 1 MiB cap on headers
            raise ValueError("request too large")

    head, _, rest = data.partition(b"\r\n\r\n")
    lines = head.decode("utf-8", errors="replace").split("\r\n")
    request_line = lines[0]
    headers: dict[str, str] = {}
    for h in lines[1:]:
        if ":" in h:
            k, v = h.split(":", 1)
            headers[k.strip().lower()] = v.strip()

    length = int(headers.get("content-length", "0") or 0)
    body = rest
    while len(body) < length:
        chunk = conn.recv(min(65536, length - len(body)))
        if not chunk:
            break
        body += chunk
    return request_line, headers, body[:length]


def write_http_response(conn: socket.socket, code: int, payload: dict) -> None:
    body = json.dumps(payload).encode("utf-8")
    headers = (
        f"HTTP/1.1 {code} OK\r\n"
        f"Content-Type: application/json\r\n"
        f"Content-Length: {len(body)}\r\n"
        f"Connection: close\r\n\r\n"
    )
    try:
        conn.sendall(headers.encode("ascii") + body)
    except OSError:
        pass


def handle_socket_client(conn: socket.socket) -> None:
    try:
        request_line, headers, body = read_http_request(conn)
    except Exception as e:
        write_http_response(conn, 400, {"error": f"bad request: {e}"})
        return

    if not _initialized.is_set():
        write_http_response(conn, 503, {"error": "channel not yet initialized"})
        return

    method = request_line.split(" ", 1)[0].upper() if request_line else ""
    if method == "GET":
        write_http_response(
            conn,
            200,
            {"server": SERVER_NAME, "version": SERVER_VERSION, "initialized": True},
        )
        return
    if method != "POST":
        write_http_response(conn, 405, {"error": "method not allowed"})
        return

    try:
        payload = json.loads(body.decode("utf-8", errors="replace") or "{}")
    except json.JSONDecodeError:
        write_http_response(conn, 400, {"error": "body must be JSON"})
        return

    content = payload.get("body") or payload.get("content") or ""
    sender = payload.get("from") or headers.get("x-sender") or ""
    meta_in = payload.get("meta") if isinstance(payload.get("meta"), dict) else {}
    # Sanitize meta keys to [A-Za-z0-9_]+ per channels-reference.
    meta: dict[str, str] = {}
    if sender:
        meta["from"] = str(sender)
    for k, v in meta_in.items():
        key = str(k)
        if all(c.isalnum() or c == "_" for c in key):
            meta[key] = str(v)

    # v3.6: if the sender stamped meta.pair_key, it authoritatively
    # identifies which of our registered rooms this message came from.
    # Remember it so peer_inbox_reply defaults back into the same room,
    # and use it to scope the roster lookup below.
    incoming_pair_key = meta.get("pair_key") or None
    if incoming_pair_key:
        with _last_incoming_lock:
            global _last_incoming_pair_key
            _last_incoming_pair_key = incoming_pair_key

    # Enrich with room context so the receiving agent knows whether this
    # is a 1:1 pair or a group chat, and can see the full roster + any
    # appointed mediator before choosing reply/broadcast/multicast.
    # Scope the roster by the incoming pair_key when available so a
    # multi-room session sees only the room this push came from.
    # Registration still pending → skip enrichment.
    roster = _room_roster(incoming_pair_key)
    if roster is not None:
        pair_key, members, mediator_label = roster
        if pair_key:
            meta.setdefault("pair_key", pair_key)
        meta.setdefault("room_size", str(len(members)))
        meta.setdefault("members", ",".join(members))
        if mediator_label:
            meta.setdefault("mediator", mediator_label)
            meta.setdefault("has_mediator", "1")

    if not content:
        write_http_response(conn, 400, {"error": "body.body (or body.content) required"})
        return
    if not sender:
        write_http_response(conn, 400, {"error": "body.from (or X-Sender header) required"})
        return

    notify(
        "notifications/claude/channel",
        {"content": str(content), "meta": meta},
    )
    stderr(f"[peer-inbox-channel] pushed from={sender} bytes={len(content)}")
    write_http_response(conn, 200, {"ok": True})


def socket_loop(sock: socket.socket) -> None:
    while True:
        try:
            conn, _ = sock.accept()
        except OSError:
            return
        try:
            handle_socket_client(conn)
        except Exception as e:
            stderr(f"[peer-inbox-channel] socket handler error: {e}")
        finally:
            try:
                conn.close()
            except OSError:
                pass


def setup_socket() -> socket.socket:
    global _socket_path
    SOCKET_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        SOCKET_DIR.chmod(0o700)
    except OSError:
        pass
    # Keep the socket filename short — AF_UNIX paths are capped at ~104 chars
    # on macOS. <pid>.sock (≤ 10 chars) plus dir stays well under.
    _socket_path = SOCKET_DIR / f"{os.getpid()}.sock"
    if _socket_path.exists():
        _socket_path.unlink()

    path_str = str(_socket_path)
    if len(path_str) > 100:
        raise RuntimeError(
            f"socket path too long ({len(path_str)} chars): {path_str!r}. "
            "Override with PEER_INBOX_SOCKET_DIR to a shorter path."
        )

    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.bind(path_str)
    os.chmod(path_str, 0o600)
    sock.listen(16)
    return sock


def write_pending_registration() -> None:
    global _pending_path
    PENDING_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        PENDING_DIR.chmod(0o700)
    except OSError:
        pass
    claude_pid = os.getppid()
    _pending_path = PENDING_DIR / f"{claude_pid}.json"
    _pending_path.write_text(
        json.dumps(
            {
                "socket_path": str(_socket_path),
                "channel_pid": os.getpid(),
                "claude_pid": claude_pid,
                "started_at": _iso_now(),
            }
        )
    )
    try:
        _pending_path.chmod(0o600)
    except OSError:
        pass


def _iso_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def cleanup() -> None:
    if _socket_path and _socket_path.exists():
        try:
            _socket_path.unlink()
        except OSError:
            pass
    if _pending_path and _pending_path.exists():
        try:
            _pending_path.unlink()
        except OSError:
            pass


def main() -> int:
    atexit.register(cleanup)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        try:
            signal.signal(sig, lambda *_: sys.exit(0))
        except Exception:
            pass

    sock = setup_socket()
    write_pending_registration()
    stderr(
        f"[peer-inbox-channel] pid={os.getpid()} claude_ppid={os.getppid()} "
        f"socket={_socket_path}"
    )

    threading.Thread(target=socket_loop, args=(sock,), daemon=True).start()
    stdio_loop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
