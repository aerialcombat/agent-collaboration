#!/usr/bin/env python3
"""Exercise the peer-inbox-channel server over stdio:
  1. Spawn it as a subprocess.
  2. Send an `initialize` request and verify the response advertises
     capabilities.experimental['claude/channel'].
  3. Send `notifications/initialized` so the HTTP door opens.
  4. POST a message to localhost:8789 with a valid X-Sender.
  5. Verify a `notifications/claude/channel` line lands on the server's stdout.

Run: python3 test-mcp-handshake.py
Exits 0 on success, nonzero on failure.
"""
from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

SERVER = Path(__file__).parent / "peer-inbox-channel.py"

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)
    print(f"FAIL: {msg}", file=sys.stderr)


def main() -> int:
    proc = subprocess.Popen(
        [sys.executable, str(SERVER)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env={"PEER_INBOX_CHANNEL_PORT": "8799", "PEER_INBOX_ALLOWED_SENDERS": "tester", **__import__("os").environ},
    )

    notifications: list[dict] = []

    def drain_stdout() -> None:
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue
            # Notifications have no "id".
            if "id" not in msg and msg.get("method"):
                notifications.append(msg)
            elif "id" in msg:
                received_replies.append(msg)

    received_replies: list[dict] = []
    threading.Thread(target=drain_stdout, daemon=True).start()

    def send(msg: dict) -> None:
        assert proc.stdin is not None
        proc.stdin.write(json.dumps(msg) + "\n")
        proc.stdin.flush()

    # Allow HTTP bind to settle.
    time.sleep(0.2)

    # 1. Initialize
    send(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": {"name": "poc-tester", "version": "0.0.1"},
            },
        }
    )
    time.sleep(0.3)
    init_reply = next((r for r in received_replies if r.get("id") == 1), None)
    if init_reply is None:
        fail("no initialize reply")
    else:
        caps = init_reply.get("result", {}).get("capabilities", {})
        if "claude/channel" not in (caps.get("experimental") or {}):
            fail(f"initialize reply missing experimental.claude/channel: {caps}")
        else:
            print("PASS: initialize advertises experimental.claude/channel")

    # 2. Signal initialized
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    time.sleep(0.2)

    # 3. POST an allowed message
    req = urllib.request.Request(
        "http://127.0.0.1:8799/test",
        data=b"hello over channel",
        headers={"X-Sender": "tester", "X-To": "peer-inbox-test"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=2) as resp:
        if resp.status != 200:
            fail(f"HTTP status {resp.status}")
        else:
            print("PASS: HTTP POST accepted (status 200)")

    # 4. POST with unauthorized sender
    req_bad = urllib.request.Request(
        "http://127.0.0.1:8799/test",
        data=b"unauthorized attempt",
        headers={"X-Sender": "attacker"},
        method="POST",
    )
    try:
        urllib.request.urlopen(req_bad, timeout=2)
        fail("unauthorized sender was accepted")
    except urllib.error.HTTPError as e:
        if e.code == 403:
            print("PASS: unauthorized sender rejected with 403")
        else:
            fail(f"unauthorized sender got HTTP {e.code}, expected 403")

    # 5. Verify a channel notification landed on stdout
    time.sleep(0.3)
    channel_notifs = [
        n
        for n in notifications
        if n.get("method") == "notifications/claude/channel"
    ]
    if not channel_notifs:
        fail("no notifications/claude/channel received on stdout")
    else:
        payload = channel_notifs[-1].get("params", {})
        content = payload.get("content")
        meta = payload.get("meta") or {}
        if content != "hello over channel":
            fail(f"channel content wrong: {content!r}")
        elif meta.get("from") != "tester":
            fail(f"channel meta.from wrong: {meta}")
        elif meta.get("to") != "peer-inbox-test":
            fail(f"channel meta.to wrong: {meta}")
        else:
            print("PASS: channel notification has correct content+meta")

    # 6. Shutdown cleanly
    send({"jsonrpc": "2.0", "id": 99, "method": "shutdown"})
    time.sleep(0.2)
    proc.terminate()
    try:
        proc.wait(timeout=2)
    except subprocess.TimeoutExpired:
        proc.kill()

    if FAILURES:
        print(f"\n{len(FAILURES)} failures", file=sys.stderr)
        return 1
    print("\nALL POC CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
