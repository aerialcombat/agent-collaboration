#!/usr/bin/env python3
"""peer-inbox-pi-bridge: give a pi session a live channel_socket.

Registers a pi-agent session in the peer-inbox DB, stands up a Unix socket
that speaks the same HTTP/JSON wire format as peer-inbox-channel.py, writes
that socket path into sessions.channel_socket, and wires inbound pushes into
pi --mode rpc over stdin. When pi emits agent_end, the final assistant text
is relayed back to the last sender via peer-inbox-db peer-send.

Minimal MVP — DM reply only (to last sender), no broadcast handling, no crash
recovery. Shut down with SIGINT/SIGTERM; that clears channel_socket and kills
pi cleanly.
"""

import argparse
import json
import os
import signal
import socket
import sqlite3
import subprocess
import sys
import threading
import uuid
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
PEER_INBOX_DB_PY = SCRIPT_DIR / "peer-inbox-db.py"
SOCKET_DIR = Path(os.environ.get("PEER_INBOX_SOCKET_DIR", "/tmp/peer-inbox"))


def db_path() -> Path:
    return Path(
        os.environ.get(
            "AGENT_COLLAB_INBOX_DB",
            str(Path.home() / ".agent-collab" / "sessions.db"),
        )
    )


def read_http_request(conn):
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            break
        data += chunk
        if len(data) > 1024 * 1024:
            raise ValueError("request too large")
    head, _, rest = data.partition(b"\r\n\r\n")
    lines = head.decode("utf-8", errors="replace").split("\r\n")
    request_line = lines[0]
    headers = {}
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


def write_http_response(conn, code, payload):
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


class Bridge:
    def __init__(self, args):
        self.label = args.label
        self.pair_key = args.pair_key
        self.provider = args.provider
        self.model = args.model
        self.session_dir = Path(
            args.session_dir or f"~/.agent-collab/pi-sessions/{args.label}"
        ).expanduser()
        self.cwd = os.getcwd()
        self.socket_path = None
        self.op_socket_path = None
        self.sock = None
        self.op_sock = None
        self.pi_proc = None
        self.last_sender = None
        self.pi_stdin_lock = threading.Lock()
        self.op_subscribers: set = set()
        self.op_lock = threading.Lock()
        self.shutting_down = False

    def log(self, msg):
        print(f"[pi-bridge] {msg}", file=sys.stderr, flush=True)

    def register_session(self):
        session_key = f"pi-bridge-{uuid.uuid4()}"
        cmd = [
            sys.executable,
            str(PEER_INBOX_DB_PY),
            "session-register",
            "--cwd", self.cwd,
            "--label", self.label,
            "--agent", "pi",
            "--role", "peer",
            "--session-key", session_key,
            "--force",
        ]
        if self.pair_key:
            cmd += ["--pair-key", self.pair_key]
        r = subprocess.run(cmd, capture_output=True, text=True)
        if r.returncode != 0:
            self.log(f"register failed: {r.stderr.strip()}")
            sys.exit(1)
        self.log(r.stdout.strip())

    def setup_socket(self):
        SOCKET_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
        try:
            SOCKET_DIR.chmod(0o700)
        except OSError:
            pass
        path = SOCKET_DIR / f"pi-bridge-{os.getpid()}.sock"
        if path.exists():
            path.unlink()
        s = str(path)
        if len(s) > 100:
            raise RuntimeError(f"socket path too long: {s!r}")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(s)
        os.chmod(s, 0o600)
        self.sock.listen(16)
        self.socket_path = s
        self.log(f"listening on {s}")

    def setup_op_socket(self):
        path = SOCKET_DIR / f"pi-bridge-{os.getpid()}.op.sock"
        if path.exists():
            path.unlink()
        s = str(path)
        if len(s) > 100:
            raise RuntimeError(f"op socket path too long: {s!r}")
        self.op_sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.op_sock.bind(s)
        os.chmod(s, 0o600)
        self.op_sock.listen(4)
        self.op_socket_path = s
        self.log(f"operator socket at {s}")

    def update_channel_socket(self, value):
        try:
            conn = sqlite3.connect(str(db_path()))
            conn.execute(
                "UPDATE sessions SET channel_socket = ? "
                "WHERE cwd = ? AND label = ?",
                (value, self.cwd, self.label),
            )
            conn.commit()
            conn.close()
        except Exception as e:
            self.log(f"update channel_socket failed: {e}")

    def spawn_pi(self):
        self.session_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        cmd = [
            "pi", "--mode", "rpc",
            "--session-dir", str(self.session_dir),
            "--no-extensions", "--no-skills",
        ]
        if self.provider:
            cmd += ["--provider", self.provider]
        if self.model:
            cmd += ["--model", self.model]
        self.log(f"spawning: {' '.join(cmd)}")
        self.pi_proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )

    def send_to_pi(self, message):
        with self.pi_stdin_lock:
            if not self.pi_proc or not self.pi_proc.stdin:
                return
            try:
                self.pi_proc.stdin.write(
                    json.dumps({
                        "type": "prompt",
                        "message": message,
                        "streamingBehavior": "followUp",
                    }) + "\n"
                )
                self.pi_proc.stdin.flush()
            except BrokenPipeError:
                self.log("pi stdin broken")

    def socket_accept_loop(self):
        while not self.shutting_down:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            try:
                self.handle_client(conn)
            except Exception as e:
                self.log(f"client error: {e}")
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

    def handle_client(self, conn):
        try:
            request_line, headers, body = read_http_request(conn)
        except Exception as e:
            write_http_response(conn, 400, {"error": f"bad request: {e}"})
            return
        method = request_line.split(" ", 1)[0].upper() if request_line else ""
        if method == "GET":
            write_http_response(
                conn, 200,
                {"server": "peer-inbox-pi-bridge", "initialized": True},
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
        meta = payload.get("meta") if isinstance(payload.get("meta"), dict) else {}
        if not content or not sender:
            write_http_response(conn, 400, {"error": "from + body required"})
            return

        envelope_parts = [f'<peer-inbox from="{sender}"']
        for k, v in meta.items():
            envelope_parts.append(f'{k}="{v}"')
        envelope = " ".join(envelope_parts) + f">\n{content}\n</peer-inbox>"

        self.last_sender = sender
        self.log(f"inbound from={sender} bytes={len(content)}")
        self.send_to_pi(envelope)
        write_http_response(conn, 200, {"ok": True})

    def pi_stdout_loop(self):
        assert self.pi_proc and self.pi_proc.stdout
        # Track sender per turn: set when we see a user message_start, consumed
        # when the matching assistant turn_end fires. Pi chains turns via the
        # followUp queue without emitting agent_end, so we relay on turn_end.
        current_user_sender = None
        for line in self.pi_proc.stdout:
            line = line.rstrip()
            if not line:
                continue
            self._op_broadcast_raw(line)
            try:
                evt = json.loads(line)
            except json.JSONDecodeError:
                continue
            t = evt.get("type")
            # Tiny progress log so operators see turn boundaries without
            # needing to tail op-subscribers for the full stdout firehose.
            # full stdout firehose (which goes to op subscribers).
            if t in ("agent_start", "turn_end", "agent_end"):
                self.log(f"pi: {t}")
            if t == "message_start":
                msg = evt.get("message", {}) or {}
                if msg.get("role") == "user":
                    current_user_sender = self._extract_envelope_sender(msg)
            elif t == "turn_end":
                msg = evt.get("message", {}) or {}
                if msg.get("role") == "assistant":
                    text = self._text_from_message(msg)
                    if text and current_user_sender:
                        self._relay_reply(text, dest=current_user_sender)
                        current_user_sender = None

    def _extract_envelope_sender(self, user_message):
        """Pull from="..." out of a wrapped <peer-inbox ...> user message."""
        for c in user_message.get("content", []):
            if c.get("type") == "text":
                text = c.get("text", "") or ""
                # naive but sufficient: look for `from="..."` in the first 200 chars
                import re
                m = re.search(r'from="([^"]+)"', text[:500])
                if m:
                    return m.group(1)
        return None

    def _text_from_message(self, message):
        for c in message.get("content", []):
            if c.get("type") == "text":
                t = (c.get("text") or "").strip()
                if t:
                    return t
        return None

    def op_accept_loop(self):
        while not self.shutting_down:
            try:
                conn, _ = self.op_sock.accept()
            except OSError:
                return
            threading.Thread(
                target=self._op_client_loop, args=(conn,), daemon=True
            ).start()

    def _op_client_loop(self, conn):
        with self.op_lock:
            self.op_subscribers.add(conn)
        self.log(f"op-client attached ({len(self.op_subscribers)} total)")
        hello = {
            "type": "op:hello",
            "label": self.label,
            "pid": os.getpid(),
            "pair_key": self.pair_key,
            "last_sender": self.last_sender,
        }
        try:
            conn.sendall((json.dumps(hello) + "\n").encode("utf-8"))
        except OSError:
            pass
        buf = b""
        try:
            while not self.shutting_down:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                buf += chunk
                while b"\n" in buf:
                    line, _, buf = buf.partition(b"\n")
                    if not line.strip():
                        continue
                    self._handle_op_command(line.decode("utf-8", "replace"), conn)
        except OSError:
            pass
        finally:
            with self.op_lock:
                self.op_subscribers.discard(conn)
            try:
                conn.close()
            except OSError:
                pass
            self.log(f"op-client detached ({len(self.op_subscribers)} remain)")

    def _handle_op_command(self, line, conn):
        try:
            cmd = json.loads(line)
        except json.JSONDecodeError as e:
            self._op_send(conn, {"type": "op:error", "error": f"bad json: {e}"})
            return
        t = cmd.get("type")
        if t in ("prompt", "steer", "follow_up"):
            msg = cmd.get("message", "")
            if not msg:
                self._op_send(conn, {"type": "op:error", "error": "message required"})
                return
            # Default streamingBehavior=followUp on `prompt` so ops aren't
            # rejected when pi is mid-stream. `steer`/`follow_up` have their
            # own semantics and don't need it.
            if t == "prompt" and "streamingBehavior" not in cmd:
                cmd["streamingBehavior"] = "followUp"
            with self.pi_stdin_lock:
                if self.pi_proc and self.pi_proc.stdin:
                    try:
                        self.pi_proc.stdin.write(json.dumps(cmd) + "\n")
                        self.pi_proc.stdin.flush()
                        self._op_send(conn, {"type": "op:ack", "forwarded": t})
                    except BrokenPipeError:
                        self._op_send(conn, {"type": "op:error", "error": "pi stdin broken"})
        elif t == "set_sender":
            self.last_sender = cmd.get("label")
            self._op_send(conn, {"type": "op:ack", "last_sender": self.last_sender})
        else:
            self._op_send(conn, {"type": "op:error", "error": f"unknown type: {t}"})

    def _op_send(self, conn, obj):
        try:
            conn.sendall((json.dumps(obj) + "\n").encode("utf-8"))
        except OSError:
            pass

    def _op_broadcast_raw(self, line):
        if not self.op_subscribers:
            return
        data = (line + "\n").encode("utf-8")
        dead = []
        with self.op_lock:
            subs = list(self.op_subscribers)
        for c in subs:
            try:
                c.sendall(data)
            except OSError:
                dead.append(c)
        if dead:
            with self.op_lock:
                for c in dead:
                    self.op_subscribers.discard(c)

    def _op_broadcast_obj(self, obj):
        self._op_broadcast_raw(json.dumps(obj))

    def _extract_text(self, agent_end_evt):
        for msg in reversed(agent_end_evt.get("messages", [])):
            if msg.get("role") == "assistant":
                for c in msg.get("content", []):
                    if c.get("type") == "text":
                        t = (c.get("text") or "").strip()
                        if t:
                            return t
        return None

    def _relay_reply(self, text, dest=None):
        dest = dest or self.last_sender
        if not dest:
            self.log(f"no dest for relay; dropping (len={len(text)})")
            return
        preview = text[:60] + ("..." if len(text) > 60 else "")
        self.log(f"relay → {dest}: {preview}")
        cmd = [
            sys.executable, str(PEER_INBOX_DB_PY), "peer-send",
            "--cwd", self.cwd,
            "--as", self.label,
            "--to", dest,
            "--message", text,
        ]
        r = subprocess.run(cmd, capture_output=True, text=True)
        if r.returncode != 0:
            self.log(f"peer-send failed: {r.stderr.strip()}")
            self._op_broadcast_obj({"type": "op:relay_error", "to": dest, "error": r.stderr.strip()})
        else:
            self._op_broadcast_obj({"type": "op:relay", "to": dest, "bytes": len(text)})

    def pi_stderr_loop(self):
        if not (self.pi_proc and self.pi_proc.stderr):
            return
        for line in self.pi_proc.stderr:
            self.log(f"pi-stderr: {line.rstrip()}")

    def shutdown(self):
        if self.shutting_down:
            return
        self.shutting_down = True
        self.update_channel_socket(None)
        if self.pi_proc:
            try:
                self.pi_proc.stdin.close()
            except Exception:
                pass
            try:
                self.pi_proc.terminate()
                self.pi_proc.wait(timeout=3)
            except Exception:
                try:
                    self.pi_proc.kill()
                except Exception:
                    pass
        if self.sock:
            try:
                self.sock.close()
            except OSError:
                pass
        if self.op_sock:
            try:
                self.op_sock.close()
            except OSError:
                pass
        for p in (self.socket_path, self.op_socket_path):
            if p and Path(p).exists():
                try:
                    Path(p).unlink()
                except OSError:
                    pass

    def run(self):
        try:
            self.register_session()
            self.setup_socket()
            self.setup_op_socket()
            self.update_channel_socket(self.socket_path)
            self.spawn_pi()
            threading.Thread(
                target=self.socket_accept_loop, daemon=True
            ).start()
            threading.Thread(
                target=self.op_accept_loop, daemon=True
            ).start()
            threading.Thread(
                target=self.pi_stderr_loop, daemon=True
            ).start()
            self.pi_stdout_loop()
        finally:
            self.shutdown()


def main():
    p = argparse.ArgumentParser(
        prog="peer-inbox-pi-bridge",
        description="Live channel_socket supervisor for a pi-agent session.",
    )
    p.add_argument("--label", required=True,
                   help="session label to register as (agent=pi)")
    p.add_argument("--pair-key", dest="pair_key",
                   help="pair key to join (default: cwd-only scope)")
    p.add_argument("--provider", help="pi provider (e.g. zai)")
    p.add_argument("--model", help="pi model (e.g. glm-4.5-flash)")
    p.add_argument("--session-dir",
                   help="pi --session-dir (default: ~/.agent-collab/pi-sessions/<label>)")
    args = p.parse_args()

    bridge = Bridge(args)

    def handle_signal(*_):
        bridge.shutdown()
        sys.exit(0)

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    bridge.run()


if __name__ == "__main__":
    main()
