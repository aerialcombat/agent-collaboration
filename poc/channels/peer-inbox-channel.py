#!/usr/bin/env python3
"""Minimal Claude Code Channels plugin — peer-inbox POC.

Speaks just enough MCP (stdio, JSON-RPC 2.0) to:
  - advertise capabilities.experimental['claude/channel'] so Claude Code
    treats us as a channel (not a plain tool server);
  - accept inbound HTTP POSTs on localhost:8789, gate by X-Sender
    allowlist, and emit `notifications/claude/channel` messages into
    the running Claude session as `<channel source="peer-inbox" from="...">
    body </channel>`.

No external deps (stdlib only). Meant to prove out the mechanism before
we wire it into our SQLite peer-inbox.
"""
from __future__ import annotations

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

SERVER_NAME = "peer-inbox"
SERVER_VERSION = "0.0.1"
PROTOCOL_VERSION = "2025-03-26"  # MCP version claude advertises in practice

HTTP_HOST = os.environ.get("PEER_INBOX_CHANNEL_HOST", "127.0.0.1")
HTTP_PORT = int(os.environ.get("PEER_INBOX_CHANNEL_PORT", "8789"))

# Sender allowlist — claude-plugins-official/channels-reference requires us
# to gate before emitting, since Claude Code does no sender gating.
ALLOWED_SENDERS = set(
    s.strip()
    for s in os.environ.get(
        "PEER_INBOX_ALLOWED_SENDERS", "dev,test-backend,test-front,orchestrator"
    ).split(",")
    if s.strip()
)

# stdout is the MCP transport — nothing else may write to it. Use stderr
# for diagnostics.
_stdout_lock = threading.Lock()


def stderr(*args):
    print(*args, file=sys.stderr, flush=True)


def send(msg: dict) -> None:
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
    send(msg)


def notify(method: str, params: dict) -> None:
    send({"jsonrpc": "2.0", "method": method, "params": params})


# ---- MCP request handling ---------------------------------------------------

INITIALIZED = threading.Event()


def handle_request(req: dict) -> None:
    method = req.get("method")
    req_id = req.get("id")

    if method == "initialize":
        # Advertise the channel capability so Claude treats us as a channel.
        reply(
            req_id,
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {
                    "experimental": {
                        "claude/channel": {},
                    },
                    # Tools capability left off deliberately — no reply tool
                    # in this POC; we're one-way inbox.
                },
                "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
                "instructions": (
                    "Messages arrive as <channel source=\"peer-inbox\" "
                    "from=\"...\"> body </channel>. No reply tool in this "
                    "POC — one-way inbox only."
                ),
            },
        )
        return

    if method == "notifications/initialized":
        INITIALIZED.set()
        stderr("[peer-inbox-channel] initialized")
        return

    # Be permissive on other methods the client may probe for.
    if method == "tools/list":
        reply(req_id, {"tools": []})
        return
    if method == "resources/list":
        reply(req_id, {"resources": []})
        return
    if method == "prompts/list":
        reply(req_id, {"prompts": []})
        return

    if method == "ping":
        reply(req_id, {})
        return

    if method == "shutdown":
        reply(req_id, {})
        return

    if req_id is not None:
        # Unknown method — respond with JSON-RPC error so the client doesn't
        # hang waiting for a reply.
        reply(
            req_id,
            error={
                "code": -32601,
                "message": f"method not found: {method}",
            },
        )
    # Notifications we don't recognise — ignore silently.


def stdio_loop() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            stderr(f"[peer-inbox-channel] bad JSON on stdin: {e}")
            continue
        try:
            handle_request(req)
        except Exception as e:
            stderr(f"[peer-inbox-channel] handler error: {e}")


# ---- HTTP receiver ----------------------------------------------------------


class ChannelReceiver(BaseHTTPRequestHandler):
    # Quiet default access log — use stderr for our own trace.
    def log_message(self, fmt, *args):
        pass

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0") or 0)
        body_bytes = self.rfile.read(length) if length else b""
        body = body_bytes.decode("utf-8", errors="replace")

        sender = self.headers.get("X-Sender", "").strip()
        if not sender:
            return self._json(400, {"error": "X-Sender header required"})
        if sender not in ALLOWED_SENDERS:
            stderr(f"[peer-inbox-channel] reject sender={sender!r}")
            return self._json(403, {"error": f"sender not allowed: {sender}"})

        if not INITIALIZED.is_set():
            return self._json(
                503, {"error": "channel not yet initialized by claude"}
            )

        to_label = self.headers.get("X-To", "").strip()
        path = self.path.lstrip("/") or "inbox"

        # Emit an MCP notification. Meta keys must match [A-Za-z0-9_]+ per
        # channels-reference — hyphens are silently dropped by Claude.
        meta = {"from": sender, "chat_id": path}
        if to_label:
            meta["to"] = to_label

        notify(
            "notifications/claude/channel",
            {"content": body, "meta": meta},
        )
        stderr(f"[peer-inbox-channel] push from={sender} bytes={len(body_bytes)}")
        return self._json(200, {"ok": True, "bytes": len(body_bytes)})

    def do_GET(self) -> None:
        # Simple health endpoint for testing.
        return self._json(
            200,
            {
                "server": SERVER_NAME,
                "version": SERVER_VERSION,
                "initialized": INITIALIZED.is_set(),
                "allowed_senders": sorted(ALLOWED_SENDERS),
            },
        )

    def _json(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def http_loop() -> None:
    try:
        server = HTTPServer((HTTP_HOST, HTTP_PORT), ChannelReceiver)
    except OSError as e:
        stderr(f"[peer-inbox-channel] HTTP bind failed on {HTTP_HOST}:{HTTP_PORT}: {e}")
        return
    stderr(f"[peer-inbox-channel] http listening on {HTTP_HOST}:{HTTP_PORT}")
    server.serve_forever()


def main() -> int:
    t = threading.Thread(target=http_loop, daemon=True)
    t.start()
    stdio_loop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
