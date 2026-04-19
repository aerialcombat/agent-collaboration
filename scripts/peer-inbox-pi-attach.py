#!/usr/bin/env python3
"""peer-inbox-pi-attach: tap into a running peer-inbox-pi-bridge session.

Connects to the bridge's operator socket (/tmp/peer-inbox/pi-bridge-<pid>.op.sock),
streams pi's event log live, and lets you send prompts from stdin — each line
becomes a pi RPC `prompt` on the same live session the bridge is managing.
Replies still auto-relay through peer-inbox because the bridge's stdout-reader
is unchanged; you're just an additional subscriber.

Resolve the bridge automatically by --label (looks up sessions.channel_socket,
derives the .op.sock path), or pass --socket explicitly.
"""

import argparse
import json
import os
import socket
import sqlite3
import sys
import threading
from pathlib import Path


def db_path() -> Path:
    return Path(
        os.environ.get(
            "AGENT_COLLAB_INBOX_DB",
            str(Path.home() / ".agent-collab" / "sessions.db"),
        )
    )


def resolve_op_socket_by_label(label: str) -> str:
    conn = sqlite3.connect(str(db_path()))
    try:
        row = conn.execute(
            "SELECT channel_socket FROM sessions WHERE label = ? AND agent = 'pi' "
            "AND channel_socket IS NOT NULL AND channel_socket != '' "
            "ORDER BY last_seen_at DESC LIMIT 1",
            (label,),
        ).fetchone()
    finally:
        conn.close()
    if not row or not row[0]:
        sys.exit(f"no live channel_socket for label {label!r}")
    chan = row[0]
    if not chan.endswith(".sock"):
        sys.exit(f"unexpected channel_socket format: {chan!r}")
    op = chan[:-5] + ".op.sock"
    if not Path(op).exists():
        sys.exit(f"op socket not found at {op} (bridge may be older version)")
    return op


FILTER_EVENT_TYPES = {
    "op:hello", "op:ack", "op:error", "op:relay", "op:relay_error",
    "response", "agent_start", "agent_end", "agent_error",
    "turn_start",
}
PRINT_TEXT_DELTAS = True


def render_event(obj):
    t = obj.get("type", "?")
    if t == "op:hello":
        return (
            f"[attached] label={obj.get('label')} pid={obj.get('pid')} "
            f"pair_key={obj.get('pair_key')} last_sender={obj.get('last_sender')}"
        )
    if t == "op:ack":
        return f"[ack] {json.dumps({k: v for k, v in obj.items() if k != 'type'})}"
    if t == "op:error":
        return f"[error] {obj.get('error')}"
    if t == "op:relay":
        return f"[relayed → {obj.get('to')}: {obj.get('bytes')} bytes]"
    if t == "op:relay_error":
        return f"[relay failed → {obj.get('to')}: {obj.get('error')}]"
    if t == "agent_start":
        return "--- agent_start ---"
    if t == "agent_end":
        # Extract the final assistant text
        for msg in reversed(obj.get("messages", [])):
            if msg.get("role") == "assistant":
                for c in msg.get("content", []):
                    if c.get("type") == "text":
                        text = (c.get("text") or "").strip()
                        if text:
                            return f"[pi]\n{text}\n--- agent_end ---"
        return "--- agent_end ---"
    if t == "agent_error":
        return f"[pi error] {obj.get('error') or obj}"
    if t == "response":
        return f"[response] {obj.get('command')}={obj.get('success')}"
    if t == "message_update" and PRINT_TEXT_DELTAS:
        ev = obj.get("assistantMessageEvent") or {}
        if ev.get("type") == "text_delta":
            return ev.get("delta", "")
    return None


def reader(sock):
    buf = b""
    while True:
        try:
            chunk = sock.recv(65536)
        except OSError:
            return
        if not chunk:
            print("[disconnected]", file=sys.stderr)
            return
        buf += chunk
        while b"\n" in buf:
            line, _, buf = buf.partition(b"\n")
            if not line.strip():
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                print(f"[raw] {line.decode('utf-8', 'replace')}")
                continue
            rendered = render_event(obj)
            if rendered is None:
                continue
            if rendered.endswith("\n") or "\n" in rendered:
                print(rendered, flush=True)
            else:
                print(rendered, flush=True)


def main():
    p = argparse.ArgumentParser(prog="peer-inbox-pi-attach")
    p.add_argument("--label", help="bridge label (e.g. pi-bridge)")
    p.add_argument("--socket", dest="socket_path",
                   help="explicit operator socket path")
    p.add_argument("--cmd", choices=["prompt", "steer", "follow_up"],
                   default="prompt",
                   help="command type for stdin lines (default: prompt)")
    args = p.parse_args()

    if not args.socket_path and not args.label:
        sys.exit("--label or --socket required")
    op_path = args.socket_path or resolve_op_socket_by_label(args.label)

    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.connect(op_path)
    print(f"[connected] {op_path}", file=sys.stderr)
    print(f"[input mode] {args.cmd} (each stdin line becomes a {args.cmd!r})", file=sys.stderr)

    threading.Thread(target=reader, args=(s,), daemon=True).start()

    try:
        for line in sys.stdin:
            line = line.rstrip("\n")
            if not line:
                continue
            msg = json.dumps({"type": args.cmd, "message": line})
            try:
                s.sendall(msg.encode("utf-8") + b"\n")
            except OSError as e:
                print(f"[send failed] {e}", file=sys.stderr)
                break
    except KeyboardInterrupt:
        pass
    finally:
        try:
            s.close()
        except OSError:
            pass


if __name__ == "__main__":
    main()
