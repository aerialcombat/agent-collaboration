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
        },
        "required": ["body"],
        "additionalProperties": False,
    },
}

_stdout_lock = threading.Lock()
_initialized = threading.Event()
_socket_path: Path | None = None
_pending_path: Path | None = None


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
                    "Acknowledge if relevant and carry on.\n\n"
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
                    "`peer send --to a --message ...`."
                ),
            },
        )
        return
    if method == "notifications/initialized":
        _initialized.set()
        stderr(f"[peer-inbox-channel] initialized; socket={_socket_path}")
        return
    if method == "tools/list":
        reply(req_id, {"tools": [REPLY_TOOL]})
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


def _room_roster() -> tuple[str | None, list[str], str | None] | None:
    """Look up the receiver's pair_key, roster, and mediator label.

    Returns (pair_key, members, mediator_label). `members` is sorted,
    self-inclusive. `mediator_label` is the label of whatever session
    in the room registered with role='mediator', or None if the room
    has no appointed mediator (party mode).
    """
    resolved = _resolve_self_from_socket()
    if resolved is None:
        return None
    self_cwd, self_label, self_pair_key = resolved
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


def _resolve_self_from_socket() -> tuple[str, str, str | None] | None:
    """Look up (cwd, label, pair_key) for the session bound to this channel."""
    if not _socket_path or not DB_PATH.exists():
        return None
    try:
        conn = sqlite3.connect(str(DB_PATH))
        conn.row_factory = sqlite3.Row
        try:
            row = conn.execute(
                "SELECT cwd, label, pair_key FROM sessions "
                "WHERE channel_socket = ?",
                (str(_socket_path),),
            ).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as e:
        stderr(f"[peer-inbox-channel] sqlite error in resolve: {e}")
        return None
    if row is None:
        return None
    return row["cwd"], row["label"], row["pair_key"]


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


def _tool_error(req_id, text: str) -> None:
    reply(req_id, {
        "content": [{"type": "text", "text": text}],
        "isError": True,
    })


def handle_tools_call(req_id, params: dict) -> None:
    name = params.get("name")
    if name != REPLY_TOOL["name"]:
        reply(req_id, error={
            "code": -32602,
            "message": f"unknown tool: {name}",
        })
        return
    arguments = params.get("arguments") or {}
    if not isinstance(arguments, dict):
        _tool_error(req_id, "error: arguments must be an object")
        return

    body = arguments.get("body")
    to = arguments.get("to")
    mention = arguments.get("mention")
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

    resolved = _resolve_self_from_socket()
    if resolved is None:
        _tool_error(
            req_id,
            "error: no session bound to this channel yet — "
            "run `agent-collab session register` first.",
        )
        return
    self_cwd, self_label, self_pair_key = resolved

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

    # Enrich with room context so the receiving agent knows whether this
    # is a 1:1 pair or a group chat, and can see the full roster + any
    # appointed mediator before choosing reply/broadcast/multicast.
    # Resolution looks up the receiver's own session row via
    # channel_socket; if registration is still pending we just skip
    # enrichment.
    roster = _room_roster()
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
