#!/usr/bin/env python3
"""peer-inbox-db: SQLite-backed inbox for cross-session agent collaboration.

Design: scripts/agent-collab bash dispatches `session` and `peer` subcommands
to this helper. All SQLite work happens here with prepared statements, WAL
mode, busy_timeout, and atomic UPDATE...RETURNING for claim-and-mark.

See plans/peer-inbox.md for the full contract.
"""
from __future__ import annotations

import argparse
import hashlib
import html as html_lib
import json
import os
import re
import sqlite3
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional

DEFAULT_DB = Path.home() / ".agent-collab" / "sessions.db"
DEFAULT_CLAUDE_SESSION_LOG_DIR = Path.home() / ".agent-collab" / "claude-sessions-seen"
DEFAULT_PENDING_CHANNELS_DIR = Path.home() / ".agent-collab" / "pending-channels"
SESSIONS_DIR_REL = ".agent-collab/sessions"
DEFAULT_MAX_PAIR_TURNS = 100
TERMINATION_TOKEN = "[[end]]"
LABEL_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")
PAIR_KEY_RE = re.compile(r"^[a-z0-9][a-z0-9-]{1,63}$")
VALID_AGENTS = {"claude", "codex", "gemini"}

# Wordlists for pair-key slugs (adjective-noun-XXXX) and auto-generated
# labels (adjective-noun). 128 × 128 × 65536 ≈ 1 billion pair-key combos
# and 16384 label combos — comfortable for any realistic number of live
# pairs on one machine.
PAIR_KEY_ADJECTIVES = (
    "amber", "ancient", "arctic", "azure", "bold", "brave", "brisk", "bright",
    "bronze", "busy", "calm", "candid", "chilly", "chipper", "clear", "clever",
    "cosmic", "crimson", "crisp", "curious", "dainty", "daring", "dashing",
    "deft", "dewy", "dusty", "eager", "earnest", "easy", "ebon", "eerie",
    "elated", "electric", "emerald", "epic", "fancy", "fearless", "fertile",
    "fierce", "fleet", "floral", "fluffy", "fond", "fresh", "frosty", "furry",
    "gentle", "glad", "gleaming", "glowing", "golden", "gracious", "grand",
    "happy", "hardy", "hazy", "hearty", "honest", "humble", "idle", "inky",
    "jade", "jazzy", "jolly", "jovial", "keen", "kind", "lavender", "lean",
    "lively", "lucky", "lush", "magnetic", "merry", "mindful", "misty",
    "modest", "noble", "nimble", "ochre", "peaceful", "perky", "placid",
    "playful", "plucky", "polite", "proud", "quick", "quiet", "radiant",
    "rapid", "rosy", "royal", "rugged", "sage", "serene", "silent", "silky",
    "silver", "snowy", "soft", "solar", "solemn", "sparkly", "spry", "stellar",
    "sturdy", "sunny", "swift", "tangy", "tender", "tidy", "tranquil",
    "trusty", "upbeat", "urban", "valiant", "velvet", "vibrant", "vivid",
    "warm", "wild", "windy", "witty", "woodland", "zany", "zealous", "zesty",
)
PAIR_KEY_NOUNS = (
    "acorn", "anchor", "archer", "arrow", "aspen", "badger", "basalt", "beacon",
    "beaver", "birch", "bison", "bluff", "boulder", "breeze", "brook", "canyon",
    "cedar", "chestnut", "cliff", "clover", "comet", "compass", "coral",
    "cove", "creek", "crest", "crystal", "cypress", "daisy", "delta", "dove",
    "eagle", "ember", "fable", "falcon", "feather", "fern", "fjord", "flame",
    "forest", "fox", "galaxy", "glade", "glow", "goose", "grove", "gull",
    "harbor", "harvest", "haven", "hazel", "heath", "hedge", "heron", "hill",
    "holly", "horizon", "island", "ivy", "jaguar", "juniper", "kestrel",
    "lagoon", "lake", "lantern", "lark", "laurel", "lilac", "lion", "lotus",
    "lynx", "maple", "meadow", "mesa", "monsoon", "moon", "moss", "mountain",
    "nebula", "oak", "oasis", "orchid", "otter", "owl", "palm", "peak",
    "peony", "pine", "plum", "poppy", "prairie", "quartz", "quail", "rainbow",
    "ranger", "raven", "reef", "ridge", "river", "robin", "sable", "saffron",
    "salmon", "sequoia", "shore", "slate", "sparrow", "spring", "spruce",
    "star", "storm", "stream", "summit", "swallow", "thicket", "thistle",
    "tide", "tiger", "trail", "tulip", "valley", "vista", "willow", "wolf",
    "wren", "zebra", "zenith", "zephyr",
)
MAX_BODY_BYTES = 8 * 1024
HOOK_BLOCK_BUDGET = 4 * 1024
STALE_THRESHOLD_SECS = 30 * 60
IDLE_THRESHOLD_SECS = 5 * 60
SQLITE_MIN_VERSION = (3, 35, 0)
PYTHON_MIN = (3, 9)
SESSION_KEY_ENV_CANDIDATES = (
    "AGENT_COLLAB_SESSION_KEY",
    "CLAUDE_SESSION_ID",
    "CODEX_SESSION_ID",
    "GEMINI_SESSION_ID",
)

EXIT_OK = 0
EXIT_LABEL_COLLISION = 1
EXIT_CONFIG_ERROR = 2
EXIT_VALIDATION = 3
EXIT_PEER_OFFLINE = 4
EXIT_PATH_DRIFT = 5
EXIT_NOT_FOUND = 6


def err(msg: str, code: int) -> None:
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(code)


def warn(msg: str) -> None:
    print(f"warn: {msg}", file=sys.stderr)


def now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_iso(s: str) -> datetime:
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)


def seconds_since(ts: str) -> float:
    return (datetime.now(timezone.utc) - parse_iso(ts)).total_seconds()


def activity_state(last_seen: str) -> str:
    age = seconds_since(last_seen)
    if age < IDLE_THRESHOLD_SECS:
        return "active"
    if age < STALE_THRESHOLD_SECS:
        return "idle"
    return "stale"


def check_sqlite_version() -> None:
    parts = tuple(int(p) for p in sqlite3.sqlite_version.split("."))
    if parts < SQLITE_MIN_VERSION:
        err(
            f"python-linked sqlite is {sqlite3.sqlite_version}, need >= "
            f"{'.'.join(str(p) for p in SQLITE_MIN_VERSION)} for UPDATE...RETURNING",
            EXIT_CONFIG_ERROR,
        )


def resolve_cwd(raw: Optional[str] = None) -> Path:
    p = Path(raw) if raw else Path.cwd()
    try:
        resolved = p.resolve(strict=True)
    except (FileNotFoundError, RuntimeError) as e:
        err(f"cannot resolve cwd {p!r}: {e}", EXIT_CONFIG_ERROR)
    if "\n" in str(resolved) or "\r" in str(resolved):
        err(f"cwd contains newline/carriage-return: {resolved!r}", EXIT_VALIDATION)
    return resolved


def discover_session_key() -> Optional[str]:
    """Pick a session key from env. Caller may pass None to the subcommand flow."""
    for name in SESSION_KEY_ENV_CANDIDATES:
        val = os.environ.get(name)
        if val:
            return val
    return None


def cwd_log_path(cwd: Path) -> Path:
    """Path where the hook logs recent Claude session_ids seen in a cwd."""
    cwd_hash = hashlib.sha256(str(cwd).encode("utf-8")).hexdigest()[:16]
    return DEFAULT_CLAUDE_SESSION_LOG_DIR / f"{cwd_hash}.log"


def log_seen_session(cwd: Path, session_id: str) -> None:
    """Append (timestamp, session_id) to the per-cwd seen log.

    Called by the hook on every UserPromptSubmit so `session register` and
    `session adopt` can match a Claude session that never exported
    $CLAUDE_SESSION_ID to the agent's Bash-tool subprocess.
    """
    if not session_id:
        return
    log = cwd_log_path(cwd)
    log.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        log.parent.chmod(0o700)
    except OSError:
        pass
    # Append only — easy to audit, no races beyond standard O_APPEND guarantees.
    with log.open("a", encoding="utf-8") as f:
        f.write(f"{now_iso()}\t{session_id}\n")
    try:
        log.chmod(0o600)
    except OSError:
        pass


def recent_seen_sessions(cwd: Path, limit: int = 20) -> list[str]:
    """Return the most recent Claude session_ids seen in this cwd, newest first,
    deduplicated."""
    log = cwd_log_path(cwd)
    if not log.is_file():
        return []
    seen: list[str] = []
    dedupe: set[str] = set()
    with log.open("r", encoding="utf-8") as f:
        for line in reversed(f.readlines()):
            line = line.strip()
            if not line:
                continue
            parts = line.split("\t", 1)
            if len(parts) != 2:
                continue
            sid = parts[1].strip()
            if not sid or sid in dedupe:
                continue
            dedupe.add(sid)
            seen.append(sid)
            if len(seen) >= limit:
                break
    return seen


def session_key_hash(key: str) -> str:
    """Stable short filename fragment derived from a session key."""
    return hashlib.sha256(key.encode("utf-8")).hexdigest()[:16]


def validate_label(label: str) -> None:
    if not LABEL_RE.match(label):
        err(
            f"invalid label {label!r} (allowed: [a-z0-9][a-z0-9_-]{{0,63}})",
            EXIT_VALIDATION,
        )


def validate_pair_key(key: str) -> None:
    if not PAIR_KEY_RE.match(key):
        err(
            f"invalid pair key {key!r} (allowed: [a-z0-9][a-z0-9-]{{1,63}})",
            EXIT_VALIDATION,
        )


def generate_pair_key() -> str:
    """Return a fresh memorable pair key (adjective-noun-XXXX)."""
    import secrets
    adj = secrets.choice(PAIR_KEY_ADJECTIVES)
    noun = secrets.choice(PAIR_KEY_NOUNS)
    suffix = secrets.token_hex(2)  # 4 hex chars
    return f"{adj}-{noun}-{suffix}"


def generate_label() -> str:
    """Return a fresh memorable session label (adjective-noun)."""
    import secrets
    adj = secrets.choice(PAIR_KEY_ADJECTIVES)
    noun = secrets.choice(PAIR_KEY_NOUNS)
    return f"{adj}-{noun}"


def validate_agent(agent: str) -> None:
    if agent not in VALID_AGENTS:
        err(
            f"invalid agent {agent!r} (allowed: {sorted(VALID_AGENTS)})",
            EXIT_VALIDATION,
        )


def db_path() -> Path:
    return Path(os.environ.get("AGENT_COLLAB_INBOX_DB", str(DEFAULT_DB)))


def init_db(path: Path) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        path.parent.chmod(0o700)
    except OSError:
        pass
    conn = sqlite3.connect(str(path), isolation_level=None)
    try:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA busy_timeout=5000")
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS sessions (
              cwd           TEXT NOT NULL,
              label         TEXT NOT NULL,
              agent         TEXT NOT NULL,
              role          TEXT,
              session_key   TEXT,
              started_at    TEXT NOT NULL,
              last_seen_at  TEXT NOT NULL,
              PRIMARY KEY (cwd, label)
            );
            CREATE TABLE IF NOT EXISTS inbox (
              id            INTEGER PRIMARY KEY AUTOINCREMENT,
              to_cwd        TEXT NOT NULL,
              to_label      TEXT NOT NULL,
              from_cwd      TEXT NOT NULL,
              from_label    TEXT NOT NULL,
              body          TEXT NOT NULL,
              created_at    TEXT NOT NULL,
              read_at       TEXT
            );
            CREATE INDEX IF NOT EXISTS idx_inbox_to_unread
              ON inbox(to_cwd, to_label, read_at);
            CREATE INDEX IF NOT EXISTS idx_sessions_last_seen
              ON sessions(last_seen_at);
            CREATE INDEX IF NOT EXISTS idx_sessions_key
              ON sessions(session_key);
            """
        )
    finally:
        conn.close()
    try:
        path.chmod(0o600)
    except OSError:
        pass


def migrate_sessions_session_key(conn: sqlite3.Connection) -> None:
    """Idempotent add of session_key column for upgrades from earlier installs."""
    cols = {row["name"] for row in conn.execute("PRAGMA table_info(sessions)")}
    if "session_key" not in cols:
        conn.execute("ALTER TABLE sessions ADD COLUMN session_key TEXT")
        conn.execute("CREATE INDEX IF NOT EXISTS idx_sessions_key ON sessions(session_key)")


def migrate_sessions_channel_socket(conn: sqlite3.Connection) -> None:
    """Add channel_socket column for sessions paired with a Channels MCP plugin."""
    cols = {row["name"] for row in conn.execute("PRAGMA table_info(sessions)")}
    if "channel_socket" not in cols:
        conn.execute("ALTER TABLE sessions ADD COLUMN channel_socket TEXT")


def migrate_sessions_pair_key(conn: sqlite3.Connection) -> None:
    """Add pair_key column + lookup index. Pair keys scope peer resolution
    across cwds so two sessions in different directories can share a room."""
    cols = {row["name"] for row in conn.execute("PRAGMA table_info(sessions)")}
    if "pair_key" not in cols:
        conn.execute("ALTER TABLE sessions ADD COLUMN pair_key TEXT")
    conn.execute(
        "CREATE INDEX IF NOT EXISTS idx_sessions_pair_key "
        "ON sessions(pair_key) WHERE pair_key IS NOT NULL"
    )
    # (pair_key, label) must be unique when pair_key is set — a pair has
    # exactly one session per label.
    conn.execute(
        "CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_pair_key_label "
        "ON sessions(pair_key, label) WHERE pair_key IS NOT NULL"
    )


def migrate_inbox_room_key(conn: sqlite3.Connection) -> None:
    """Stamp each inbox row with its room_key so room views can filter
    by room identity, not just by (sender_label, receiver_label) — which
    bleeds in unrelated 1:1 history when labels get reused across rooms.
    """
    cols = {row["name"] for row in conn.execute("PRAGMA table_info(inbox)")}
    if "room_key" not in cols:
        conn.execute("ALTER TABLE inbox ADD COLUMN room_key TEXT")
    conn.execute(
        "CREATE INDEX IF NOT EXISTS idx_inbox_room_key "
        "ON inbox(room_key) WHERE room_key IS NOT NULL"
    )


def migrate_peer_rooms(conn: sqlite3.Connection) -> None:
    """Create peer_rooms table for room-level turn counting + termination.

    Room identity:
      - pair_key-scoped rooms: room_key = "pk:<pair_key>"
      - cwd-only degenerate rooms: room_key = "cwd:<cwd>#<a>+<b>" where (a,b)
        is the canonical (lexicographically sorted) pair of labels.

    The prior per-edge peer_pairs table from v1.7 is dropped — the room
    table handles its cases as the degenerate N=2 room. Keeping the
    legacy table would diverge the two code paths.
    """
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS peer_rooms (
          room_key       TEXT PRIMARY KEY,
          pair_key       TEXT,
          turn_count     INTEGER NOT NULL DEFAULT 0,
          terminated_at  TEXT,
          terminated_by  TEXT
        );
        DROP TABLE IF EXISTS peer_pairs;
        """
    )


def open_db() -> sqlite3.Connection:
    path = db_path()
    if not path.exists():
        init_db(path)
    conn = sqlite3.connect(str(path), isolation_level=None)
    # Apply runtime pragmas on every open. WAL persists per-DB but setting it
    # here is idempotent and cheap; busy_timeout is per-connection.
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA busy_timeout=5000")
    conn.row_factory = sqlite3.Row
    migrate_sessions_session_key(conn)
    migrate_sessions_channel_socket(conn)
    migrate_sessions_pair_key(conn)
    migrate_inbox_room_key(conn)
    migrate_peer_rooms(conn)
    return conn


# ---- Channel pairing --------------------------------------------------------


def _ppid_of(pid: int) -> int:
    """Return the parent PID of `pid`, or 0 if unknown."""
    proc_status = Path(f"/proc/{pid}/status")
    if proc_status.is_file():
        for line in proc_status.read_text().splitlines():
            if line.startswith("PPid:"):
                try:
                    return int(line.split()[1])
                except (ValueError, IndexError):
                    return 0
    # macOS / BSD fallback
    import subprocess
    try:
        out = subprocess.check_output(
            ["ps", "-o", "ppid=", "-p", str(pid)],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
        return int(out) if out else 0
    except (subprocess.CalledProcessError, ValueError, OSError):
        # OSError covers FileNotFoundError and sandbox PermissionError (e.g.
        # codex exec's Seatbelt blocks spawning `ps`). Channel pairing is
        # best-effort; never let it crash register.
        return 0


def _pending_channels_dir() -> Path:
    return Path(
        os.environ.get("PEER_INBOX_PENDING_DIR", str(DEFAULT_PENDING_CHANNELS_DIR))
    )


def find_pending_channel_for_self(max_depth: int = 12) -> Optional[dict]:
    """Walk own process tree looking for a pending channel registration.

    The channel MCP server is spawned by Claude at session start and writes
    `.../<claude-pid>.json`. Bash-tool subprocesses are descendants of
    Claude, so walking up our own PID chain will cross Claude's PID if we
    were invoked from inside a Claude session.
    """
    pending_dir = _pending_channels_dir()
    if not pending_dir.is_dir():
        return None
    pid = os.getpid()
    for _ in range(max_depth):
        if pid <= 1:
            break
        candidate = pending_dir / f"{pid}.json"
        if candidate.is_file():
            try:
                data = json.loads(candidate.read_text())
            except json.JSONDecodeError:
                return None
            sock = data.get("socket_path")
            if not sock or not Path(sock).exists():
                return None
            data["pending_file"] = str(candidate)
            return data
        pid = _ppid_of(pid)
    return None


def _send_over_unix_socket(socket_path: str, payload: dict, timeout: float = 2.0) -> tuple[int, str]:
    """POST JSON to a peer channel's Unix socket. Returns (http_status, body)."""
    import socket as _socket
    body = json.dumps(payload).encode("utf-8")
    request = (
        "POST / HTTP/1.1\r\n"
        "Host: localhost\r\n"
        "Content-Type: application/json\r\n"
        f"Content-Length: {len(body)}\r\n"
        "Connection: close\r\n\r\n"
    ).encode("ascii") + body
    try:
        s = _socket.socket(_socket.AF_UNIX, _socket.SOCK_STREAM)
        s.settimeout(timeout)
        s.connect(socket_path)
        s.sendall(request)
        data = b""
        while True:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
        s.close()
    except (OSError, _socket.timeout) as e:
        return 0, f"socket error: {e}"
    head, _, resp_body = data.partition(b"\r\n\r\n")
    try:
        status_line = head.decode("ascii", errors="replace").split("\r\n", 1)[0]
        code = int(status_line.split(" ", 2)[1])
    except (IndexError, ValueError):
        code = 0
    return code, resp_body.decode("utf-8", errors="replace")


# ---- Room state helpers -----------------------------------------------------


def _pair_key(a: str, b: str) -> tuple[str, str]:
    """Canonicalize a label pair so (backend, frontend) == (frontend, backend)."""
    return (a, b) if a <= b else (b, a)


def _room_key_for(cwd: str, a: str, b: str, pair_key: Optional[str]) -> str:
    """Build the room identifier for accounting.

    - pair_key-scoped rooms share a single counter under "pk:<pair_key>".
    - cwd-only pairs degenerate to a per-edge room keyed by
      "cwd:<cwd>#<a>+<b>" (canonicalised) — each two-person conversation
      gets its own budget, matching v1.7 behaviour.
    """
    if pair_key:
        return f"pk:{pair_key}"
    ka, kb = _pair_key(a, b)
    return f"cwd:{cwd}#{ka}+{kb}"


def _get_room(conn: sqlite3.Connection, room_key: str) -> Optional[sqlite3.Row]:
    return conn.execute(
        "SELECT turn_count, terminated_at, terminated_by FROM peer_rooms "
        "WHERE room_key = ?",
        (room_key,),
    ).fetchone()


def _bump_room(
    conn: sqlite3.Connection, room_key: str, pair_key: Optional[str]
) -> None:
    conn.execute(
        """
        INSERT INTO peer_rooms (room_key, pair_key, turn_count)
        VALUES (?, ?, 1)
        ON CONFLICT(room_key) DO UPDATE SET turn_count = turn_count + 1
        """,
        (room_key, pair_key),
    )


def _terminate_room(
    conn: sqlite3.Connection, room_key: str, pair_key: Optional[str], by: str
) -> None:
    conn.execute(
        """
        INSERT INTO peer_rooms
          (room_key, pair_key, turn_count, terminated_at, terminated_by)
        VALUES (?, ?, 0, ?, ?)
        ON CONFLICT(room_key) DO UPDATE SET
          terminated_at = excluded.terminated_at,
          terminated_by = excluded.terminated_by
        """,
        (room_key, pair_key, now_iso(), by),
    )


def _max_room_turns() -> int:
    """Per-room turn budget. Env var keeps its v1.7 name for continuity."""
    try:
        return int(os.environ.get("AGENT_COLLAB_MAX_PAIR_TURNS", str(DEFAULT_MAX_PAIR_TURNS)))
    except ValueError:
        return DEFAULT_MAX_PAIR_TURNS


# ---- Mention helpers -------------------------------------------------------

_MENTION_RE = re.compile(r"@([a-z0-9][a-z0-9_-]{0,63})")


def _room_members(
    self_cwd: str, self_label: str, pair_key: Optional[str]
) -> set[str]:
    """Return the full label roster for the sender's scope (self included)."""
    conn = open_db()
    try:
        if pair_key:
            rows = conn.execute(
                "SELECT label FROM sessions WHERE pair_key = ?",
                (pair_key,),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT label FROM sessions WHERE cwd = ?",
                (self_cwd,),
            ).fetchall()
    finally:
        conn.close()
    members = {r["label"] for r in rows}
    members.add(self_label)
    return members


def _resolve_mentions(
    body: str,
    explicit: Optional[list[str]],
    members: set[str],
    self_label: str,
) -> list[str]:
    """Merge explicit --mention args with @label tokens parsed from body.

    Only returns labels that are actual room members and are not the
    sender (can't mention yourself). Sorted, de-duplicated.
    """
    found: set[str] = set()
    for m in _MENTION_RE.findall(body or ""):
        if m in members and m != self_label:
            found.add(m)
    for m in explicit or []:
        if not m:
            continue
        validate_label(m)
        if m == self_label:
            continue
        if m not in members:
            err(f"mention target not in room: {m!r}", EXIT_NOT_FOUND)
        found.add(m)
    return sorted(found)


def _get_session_pair_key(cwd: str, label: str) -> Optional[str]:
    """Return the pair_key stored for (cwd, label), or None."""
    conn = open_db()
    try:
        row = conn.execute(
            "SELECT pair_key FROM sessions WHERE cwd = ? AND label = ?",
            (cwd, label),
        ).fetchone()
    finally:
        conn.close()
    if row is None:
        return None
    return row["pair_key"]


def _lookup_pair_peer_cwd(pair_key: str, label: str) -> Optional[str]:
    """Return the cwd of the session registered under (pair_key, label).

    Uses the unique (pair_key, label) index from migrate_sessions_pair_key,
    so there is at most one row. Missing entry returns None and lets the
    caller fall back to same-cwd lookup with a clearer error downstream.
    """
    conn = open_db()
    try:
        row = conn.execute(
            "SELECT cwd FROM sessions WHERE pair_key = ? AND label = ?",
            (pair_key, label),
        ).fetchone()
    finally:
        conn.close()
    if row is None:
        return None
    return row["cwd"]


def _infer_peer_label(self_cwd: str, self_label: str, self_pair_key: Optional[str]) -> str:
    """Return the single peer label in scope (pair or cwd), or error.

    Only active/idle peers are considered. Stale rows are filtered so a
    long-abandoned session can't steal an inferred send. Errors include
    the list of candidates so the caller knows what to pass explicitly.
    """
    conn = open_db()
    try:
        if self_pair_key is not None:
            rows = conn.execute(
                "SELECT label, last_seen_at FROM sessions "
                "WHERE pair_key = ? AND NOT (cwd = ? AND label = ?)",
                (self_pair_key, self_cwd, self_label),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT label, last_seen_at FROM sessions "
                "WHERE cwd = ? AND NOT (cwd = ? AND label = ?)",
                (self_cwd, self_cwd, self_label),
            ).fetchall()
    finally:
        conn.close()

    live = [r["label"] for r in rows if activity_state(r["last_seen_at"]) != "stale"]
    if len(live) == 1:
        return live[0]
    scope = f"pair_key={self_pair_key}" if self_pair_key else f"cwd={self_cwd}"
    if not live:
        err(
            f"no live peer in {scope}; pass --to <label> or register "
            "another session first",
            EXIT_NOT_FOUND,
        )
    err(
        f"cannot infer --to: {len(live)} live peers in {scope} "
        f"({sorted(live)}); pass --to <label> to disambiguate",
        EXIT_VALIDATION,
    )


def sessions_dir(cwd: Path) -> Path:
    return cwd / SESSIONS_DIR_REL


def marker_path_for(cwd: Path, session_key: str) -> Path:
    return sessions_dir(cwd) / f"{session_key_hash(session_key)}.json"


def find_marker_by_key(start: Path, session_key: str) -> Optional[tuple[Path, dict]]:
    """Walk up from start looking for .agent-collab/sessions/<hash>.json."""
    target_name = f"{session_key_hash(session_key)}.json"
    cur = start
    while True:
        candidate = cur / SESSIONS_DIR_REL / target_name
        if candidate.is_file():
            try:
                data = json.loads(candidate.read_text())
            except json.JSONDecodeError as e:
                err(f"corrupt marker at {candidate}: {e}", EXIT_CONFIG_ERROR)
            if not isinstance(data, dict) or "cwd" not in data or "label" not in data:
                err(
                    f"corrupt marker at {candidate}: missing cwd or label",
                    EXIT_CONFIG_ERROR,
                )
            return candidate, data
        if cur.parent == cur:
            return None
        cur = cur.parent


def find_any_sessions_dir(start: Path) -> Optional[Path]:
    """Walk up from start looking for an .agent-collab/sessions dir (any contents)."""
    cur = start
    while True:
        candidate = cur / SESSIONS_DIR_REL
        if candidate.is_dir():
            return candidate
        if cur.parent == cur:
            return None
        cur = cur.parent


def write_marker(cwd: Path, label: str, session_key: str) -> None:
    marker = marker_path_for(cwd, session_key)
    marker.parent.mkdir(mode=0o755, exist_ok=True, parents=True)
    marker.write_text(
        json.dumps(
            {"cwd": str(cwd), "label": label, "session_key": session_key},
            ensure_ascii=False,
        )
    )
    try:
        marker.chmod(0o644)
    except OSError:
        pass
    gitignore = cwd / ".gitignore"
    if gitignore.is_file():
        content = gitignore.read_text()
        if ".agent-collab/sessions" not in content and ".agent-collab/" not in content:
            suffix = "" if content.endswith("\n") else "\n"
            gitignore.write_text(content + suffix + ".agent-collab/sessions/\n")


def delete_marker(cwd: Path, session_key: str) -> None:
    marker = marker_path_for(cwd, session_key)
    if marker.is_file():
        marker.unlink()


def sweep_stale_markers_for_label(cwd: Path, label: str, keep_session_key: str) -> None:
    """Remove any other markers in this cwd that claim the same label.

    Prevents the "multiple sessions registered ... labels: ['joseph']" error
    that happens when a session re-registers with a new session key — the
    old marker file (keyed on the old key hash) would otherwise linger and
    break resolve_self's single-marker convenience path.
    """
    sess_dir = sessions_dir(cwd)
    if not sess_dir.is_dir():
        return
    keep_filename = f"{session_key_hash(keep_session_key)}.json"
    for marker in sess_dir.iterdir():
        if not (marker.is_file() and marker.suffix == ".json"):
            continue
        if marker.name == keep_filename:
            continue
        try:
            data = json.loads(marker.read_text())
        except (json.JSONDecodeError, OSError):
            continue
        if isinstance(data, dict) and data.get("label") == label:
            try:
                marker.unlink()
            except OSError:
                pass


def resolve_self(as_label: Optional[str], cwd: Path) -> tuple[Path, str]:
    """Return (canonical_cwd, label) for the calling session.

    Resolution order:
      1. If --as <label> given, use it and caller's resolved cwd.
      2. Else, read session key from env (CLAUDE/CODEX/GEMINI_SESSION_ID or
         AGENT_COLLAB_SESSION_KEY); walk parents to find a per-session marker
         keyed on that session key. Use marker owner as canonical cwd.
      3. Else, if exactly one marker exists in a walk-parents-found sessions
         dir, use it (standalone / single-session convenience).
      4. Else, error requesting --as or a session key env var.
    """
    if as_label is not None:
        validate_label(as_label)
        return cwd, as_label

    key = discover_session_key()
    if key is not None:
        found = find_marker_by_key(cwd, key)
        if found is not None:
            return _verify_and_return_marker(found)
        # Session key env var is set but no marker — fall through to
        # single-marker convenience, then hard error.

    sess_dir = find_any_sessions_dir(cwd)
    if sess_dir is not None:
        markers = sorted(p for p in sess_dir.iterdir() if p.is_file() and p.suffix == ".json")
        if len(markers) == 1:
            try:
                data = json.loads(markers[0].read_text())
            except json.JSONDecodeError as e:
                err(f"corrupt marker at {markers[0]}: {e}", EXIT_CONFIG_ERROR)
            if not isinstance(data, dict) or "cwd" not in data or "label" not in data:
                err(f"corrupt marker at {markers[0]}: missing cwd or label", EXIT_CONFIG_ERROR)
            return _verify_and_return_marker((markers[0], data))
        if len(markers) > 1:
            labels = sorted({
                json.loads(m.read_text()).get("label", "?") for m in markers
                if m.read_text().strip().startswith("{")
            })
            err(
                f"multiple sessions registered in {sess_dir.parent} "
                f"({len(markers)} markers; labels: {labels}); pass --as <label> "
                "or set a session key env var (AGENT_COLLAB_SESSION_KEY, "
                "CLAUDE_SESSION_ID, CODEX_SESSION_ID, GEMINI_SESSION_ID)",
                EXIT_CONFIG_ERROR,
            )

    err(
        f"no session registered for {cwd} or any parent "
        "(run 'agent-collab session register' or pass --as)",
        EXIT_CONFIG_ERROR,
    )


def _verify_and_return_marker(found: tuple[Path, dict]) -> tuple[Path, str]:
    marker_file, data = found
    marker_owner = marker_file.parent.parent.parent  # <cwd>/.agent-collab/sessions/<file>
    try:
        marker_owner_resolved = marker_owner.resolve(strict=True)
    except (FileNotFoundError, RuntimeError):
        marker_owner_resolved = marker_owner
    recorded_cwd = Path(data["cwd"])
    try:
        recorded_cwd_resolved = recorded_cwd.resolve(strict=True)
    except (FileNotFoundError, RuntimeError):
        recorded_cwd_resolved = recorded_cwd
    if recorded_cwd_resolved != marker_owner_resolved:
        err(
            f"path drift: marker at {marker_owner_resolved} records "
            f"cwd {recorded_cwd_resolved} — marker was moved or copied; "
            "re-run 'agent-collab session register'",
            EXIT_PATH_DRIFT,
        )
    return marker_owner_resolved, data["label"]


def bump_last_seen(conn: sqlite3.Connection, cwd: str, label: str) -> None:
    conn.execute(
        "UPDATE sessions SET last_seen_at = ? WHERE cwd = ? AND label = ?",
        (now_iso(), cwd, label),
    )


def emit_system_event(
    self_cwd: str,
    self_label: str,
    pair_key: str,
    kind: str,
    body: str,
    extra_meta: Optional[dict[str, str]] = None,
) -> None:
    """Fan-out a system-flavored message to every other live peer in a room.

    Writes inbox rows (so non-channel peers see it via the hook) and
    pushes to channel sockets for live peers. Sets meta.system=kind so
    receivers can distinguish join/leave/etc from regular messages.
    Does NOT bump the room turn counter — system events are free.
    """
    room_key = f"pk:{pair_key}"
    now = now_iso()

    conn = open_db()
    try:
        rows = conn.execute(
            "SELECT cwd, label, last_seen_at, channel_socket "
            "FROM sessions WHERE pair_key = ? "
            "AND NOT (cwd = ? AND label = ?)",
            (pair_key, self_cwd, self_label),
        ).fetchall()
    finally:
        conn.close()

    live = [
        r for r in rows
        if seconds_since(r["last_seen_at"]) <= STALE_THRESHOLD_SECS
    ]
    if not live:
        return

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        for r in live:
            conn.execute(
                """
                INSERT INTO inbox
                  (to_cwd, to_label, from_cwd, from_label, body, created_at, room_key)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (r["cwd"], r["label"], self_cwd, self_label, body, now, room_key),
            )
        conn.execute("COMMIT")
    finally:
        conn.close()

    push_meta_base: dict[str, str] = {"system": kind}
    if extra_meta:
        for k, v in extra_meta.items():
            if all(c.isalnum() or c == "_" for c in k):
                push_meta_base[k] = str(v)

    for r in live:
        sock = r["channel_socket"]
        if not sock or not Path(sock).exists():
            continue
        push_meta = dict(push_meta_base)
        push_meta["to"] = r["label"]
        _send_over_unix_socket(
            sock,
            {"from": self_label, "body": body, "meta": push_meta},
        )


# ---- Subcommands ----


def cmd_session_register(args: argparse.Namespace) -> int:
    if args.pair_key and args.new_pair:
        err("--pair-key and --new-pair are mutually exclusive", EXIT_VALIDATION)
    if args.pair_key:
        validate_pair_key(args.pair_key)

    # Auto-generate a label if the caller didn't provide one. Slash commands
    # prompt the user to confirm before invoking; direct CLI callers see the
    # generated label printed below so they know what to reference.
    if args.label:
        validate_label(args.label)
    else:
        args.label = generate_label()
        validate_label(args.label)

    validate_agent(args.agent)
    cwd = resolve_cwd(args.cwd)
    now = now_iso()

    session_key = args.session_key or discover_session_key()
    if session_key is None:
        # Fallback: consult the hook-seen-sessions log. The UserPromptSubmit
        # hook records each Claude session_id it sees in this cwd, so after
        # any user prompt we can match an unregistered session_id to this
        # registration. Pick the most recent session_id not yet registered.
        seen = recent_seen_sessions(cwd)
        if seen:
            conn = open_db()
            try:
                already = {
                    r["session_key"]
                    for r in conn.execute(
                        "SELECT session_key FROM sessions WHERE cwd = ? AND session_key IS NOT NULL",
                        (str(cwd),),
                    ).fetchall()
                }
            finally:
                conn.close()
            for sid in seen:
                if sid not in already:
                    session_key = sid
                    break
    if session_key is None and args.agent in {"codex", "gemini"}:
        # Codex and Gemini do not export a session id to the shell tool's
        # subprocess env, and they have no UserPromptSubmit hook. Reuse the
        # session_key from the existing (cwd, label) row if any — makes
        # re-register idempotent — else mint a fresh UUID so first register
        # succeeds.
        import uuid as _uuid
        conn = open_db()
        try:
            row = conn.execute(
                "SELECT session_key FROM sessions WHERE cwd = ? AND label = ?",
                (str(cwd), args.label),
            ).fetchone()
        finally:
            conn.close()
        if row is not None and row["session_key"]:
            session_key = row["session_key"]
        else:
            session_key = f"auto-{args.agent}-{_uuid.uuid4()}"
    if session_key is None:
        err(
            "no session key available. This session's runtime did not export "
            "a session ID env var, and the UserPromptSubmit hook has not yet "
            "logged one for this cwd. Either (a) type any prompt in this "
            "Claude session first so the hook logs its session_id, then retry "
            "register, or (b) set AGENT_COLLAB_SESSION_KEY explicitly, or "
            "(c) pass --session-key <key>.",
            EXIT_CONFIG_ERROR,
        )

    # Attempt to pair with a live channel MCP server (Claude Code spawned it
    # at session start). Walks our process tree for a claude-pid-keyed
    # pending-channels registration. Silently no-op if no channel live.
    pending = find_pending_channel_for_self()
    channel_socket = pending.get("socket_path") if pending else None

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            "SELECT agent, role, last_seen_at, session_key, pair_key FROM sessions "
            "WHERE cwd = ? AND label = ?",
            (str(cwd), args.label),
        ).fetchone()
        if row is not None and not args.force:
            age = seconds_since(row["last_seen_at"])
            existing_key = row["session_key"]
            # Same session re-registering is always fine (idempotent refresh).
            # Different session claiming same label within idle window is a
            # collision worth flagging.
            if existing_key != session_key and age < IDLE_THRESHOLD_SECS:
                conn.execute("ROLLBACK")
                err(
                    f"label {args.label!r} already active in {cwd} "
                    f"(owned by a different session, last seen {int(age)}s ago); "
                    "use --force or a different label",
                    EXIT_LABEL_COLLISION,
                )

        # Pair-key resolution:
        #   --pair-key KEY  → use KEY (join the room)
        #   --new-pair      → mint a fresh slug
        #   neither + existing row has a pair_key → preserve it (idempotent refresh)
        #   otherwise → NULL (legacy cwd-only scope)
        if args.pair_key:
            pair_key = args.pair_key
        elif args.new_pair:
            pair_key = generate_pair_key()
        elif row is not None and row["pair_key"]:
            pair_key = row["pair_key"]
        else:
            pair_key = None

        # If joining an existing pair, ensure no different session already
        # holds this label in that pair (the unique index will catch it too,
        # but a pre-check yields a friendlier error).
        if pair_key is not None:
            dup = conn.execute(
                "SELECT cwd FROM sessions WHERE pair_key = ? AND label = ? "
                "AND NOT (cwd = ? AND label = ?)",
                (pair_key, args.label, str(cwd), args.label),
            ).fetchone()
            if dup is not None:
                conn.execute("ROLLBACK")
                err(
                    f"pair key {pair_key!r} already has a session labeled "
                    f"{args.label!r} in {dup['cwd']}; choose a different label",
                    EXIT_LABEL_COLLISION,
                )

        # Decide if this is a genuine join event worth announcing. A
        # join = (first time this label exists in this pair_key) OR
        # (a different session took over the seat). Idempotent refreshes
        # from the same session_key into the same pair stay silent.
        is_new_join = (
            pair_key is not None and (
                row is None
                or (row["pair_key"] or "") != pair_key
                or row["session_key"] != session_key
            )
        )

        conn.execute(
            """
            INSERT INTO sessions (cwd, label, agent, role, session_key, channel_socket, pair_key, started_at, last_seen_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(cwd, label) DO UPDATE SET
              agent = excluded.agent,
              role = excluded.role,
              session_key = excluded.session_key,
              channel_socket = excluded.channel_socket,
              pair_key = excluded.pair_key,
              started_at = excluded.started_at,
              last_seen_at = excluded.last_seen_at
            """,
            (str(cwd), args.label, args.agent, args.role, session_key, channel_socket, pair_key, now, now),
        )
        conn.execute("COMMIT")
    finally:
        conn.close()

    if is_new_join:
        role_tag = f", role={args.role}" if args.role else ""
        body = f"[system] {args.label} joined the room (agent={args.agent}{role_tag})"
        emit_system_event(
            self_cwd=str(cwd),
            self_label=args.label,
            pair_key=pair_key,
            kind="join",
            body=body,
            extra_meta={"agent": args.agent, "role": args.role or ""},
        )

    write_marker(cwd, args.label, session_key)
    sweep_stale_markers_for_label(cwd, args.label, session_key)
    channel_note = " [channel: paired]" if channel_socket else " [channel: none]"
    pair_note = f" [pair_key={pair_key}]" if pair_key else ""
    print(
        f"registered: {args.label} ({args.agent}, {args.role or 'peer'}) at {cwd} "
        f"[session_key={session_key_hash(session_key)}]{channel_note}{pair_note}"
    )
    return EXIT_OK


def cmd_session_close(args: argparse.Namespace) -> int:
    cwd_raw = resolve_cwd(args.cwd)
    session_key = args.session_key or discover_session_key()
    if session_key is None:
        # Same hook-log fallback register uses: Claude's UserPromptSubmit hook
        # records session_ids seen in this cwd. Pick the most recent one that
        # is currently registered (for close we want a live row, not an unused
        # id).
        seen = recent_seen_sessions(cwd_raw)
        if seen:
            conn = open_db()
            try:
                active = {
                    r["session_key"]
                    for r in conn.execute(
                        "SELECT session_key FROM sessions WHERE cwd = ? AND session_key IS NOT NULL",
                        (str(cwd_raw),),
                    ).fetchall()
                }
            finally:
                conn.close()
            for sid in seen:
                if sid in active:
                    session_key = sid
                    break

    # Resolve which session to close. Precedence: explicit --label + --cwd;
    # else discover via walk-parents using session_key; else error.
    if args.label:
        validate_label(args.label)
        label = args.label
        target_cwd = cwd_raw
    else:
        if session_key is None:
            err(
                "pass --label <label> or set a session key env var so we can "
                "find the right session to close",
                EXIT_CONFIG_ERROR,
            )
        found = find_marker_by_key(cwd_raw, session_key)
        if found is None:
            err(
                f"no registered session for this session key under {cwd_raw}",
                EXIT_NOT_FOUND,
            )
        marker_file, data = found
        label = data["label"]
        target_cwd = marker_file.parent.parent.parent.resolve(strict=True)

    # Capture pair_key BEFORE delete so we can announce the leave to peers.
    conn = open_db()
    try:
        pre_row = conn.execute(
            "SELECT pair_key FROM sessions WHERE cwd = ? AND label = ?",
            (str(target_cwd), label),
        ).fetchone()
    finally:
        conn.close()
    leaving_pair_key = pre_row["pair_key"] if pre_row else None

    # Emit leave event FIRST (while self still in sessions so meta.members
    # lists the leaver on this last push) when the session was part of a
    # pair_key room.
    if leaving_pair_key:
        emit_system_event(
            self_cwd=str(target_cwd),
            self_label=label,
            pair_key=leaving_pair_key,
            kind="leave",
            body=f"[system] {label} left the room",
        )

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        if session_key:
            cur = conn.execute(
                "DELETE FROM sessions WHERE cwd = ? AND label = ? AND "
                "(session_key = ? OR session_key IS NULL)",
                (str(target_cwd), label, session_key),
            )
        else:
            cur = conn.execute(
                "DELETE FROM sessions WHERE cwd = ? AND label = ?",
                (str(target_cwd), label),
            )
        deleted = cur.rowcount
        conn.execute("COMMIT")
    finally:
        conn.close()

    if session_key:
        delete_marker(target_cwd, session_key)

    if deleted:
        print(f"closed: {label} at {target_cwd}")
    else:
        print(f"note: {label} not in DB at {target_cwd} (marker removed)")
    return EXIT_OK


def cmd_session_list(args: argparse.Namespace) -> int:
    cwd = resolve_cwd(args.cwd)
    # When not --all-cwds, walk parents for a sessions dir and scope by its
    # owner's cwd so subdir invocations see the registered sessions. Any
    # marker will do — we only need to anchor the scope cwd.
    scope_cwd = cwd
    if not args.all_cwds:
        sess_dir = find_any_sessions_dir(cwd)
        if sess_dir is not None:
            # sess_dir = <root>/.agent-collab/sessions — want <root>
            owner = sess_dir.parent.parent
            try:
                scope_cwd = owner.resolve(strict=True)
            except (FileNotFoundError, RuntimeError):
                scope_cwd = owner
    conn = open_db()
    try:
        if args.all_cwds:
            rows = conn.execute(
                "SELECT cwd, label, agent, role, started_at, last_seen_at "
                "FROM sessions ORDER BY last_seen_at DESC"
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT cwd, label, agent, role, started_at, last_seen_at "
                "FROM sessions WHERE cwd = ? ORDER BY last_seen_at DESC",
                (str(scope_cwd),),
            ).fetchall()
    finally:
        conn.close()

    results = []
    for r in rows:
        state = activity_state(r["last_seen_at"])
        if state == "stale" and not args.include_stale:
            continue
        results.append(
            {
                "cwd": r["cwd"],
                "label": r["label"],
                "agent": r["agent"],
                "role": r["role"],
                "started_at": r["started_at"],
                "last_seen_at": r["last_seen_at"],
                "activity": state,
            }
        )

    if args.json:
        json.dump(results, sys.stdout)
        sys.stdout.write("\n")
    else:
        if not results:
            print("no sessions")
            return EXIT_OK
        for r in results:
            print(
                f"{r['label']:<20} {r['agent']:<8} {r['role'] or 'peer':<12} "
                f"{r['activity']:<7} last-seen {r['last_seen_at']}"
                + (f"  cwd={r['cwd']}" if args.all_cwds else "")
            )
    return EXIT_OK


def cmd_peer_send(args: argparse.Namespace) -> int:
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)

    self_pair_key = _get_session_pair_key(str(self_cwd), self_label)

    # Auto-infer --to when there is exactly one peer in the current scope
    # (pair if set, else cwd). Error clearly when ambiguous or empty.
    if not args.to:
        args.to = _infer_peer_label(str(self_cwd), self_label, self_pair_key)
    validate_label(args.to)

    # Resolve target cwd. Order of precedence:
    #   1. explicit --to-cwd wins (legacy cross-cwd sends).
    #   2. self has a pair_key → look up (pair_key, to_label) across cwds.
    #   3. fall back to same-cwd resolution (v1.6 behaviour).
    if args.to_cwd:
        to_cwd = resolve_cwd(args.to_cwd)
    elif self_pair_key is not None:
        resolved_cwd = _lookup_pair_peer_cwd(self_pair_key, args.to)
        to_cwd = Path(resolved_cwd) if resolved_cwd else self_cwd
    else:
        to_cwd = self_cwd

    if args.message_stdin:
        body = sys.stdin.read()
    elif args.message_file:
        body = Path(args.message_file).read_text()
    elif args.message is not None:
        body = args.message
    else:
        err("one of --message, --message-file, --message-stdin required", EXIT_VALIDATION)

    body_bytes = body.encode("utf-8")
    if len(body_bytes) > MAX_BODY_BYTES:
        err(
            f"message too large: {len(body_bytes)} bytes > {MAX_BODY_BYTES} cap",
            EXIT_VALIDATION,
        )
    if len(body_bytes) == 0:
        err("empty message rejected", EXIT_VALIDATION)

    # Room identity for accounting. pair_key-scoped rooms share a counter
    # across every member; cwd-only two-person pairs get a synthesized
    # per-edge key (degenerate N=2 room).
    room_key = _room_key_for(str(self_cwd), self_label, args.to, self_pair_key)
    reset_hint = (
        f"peer reset --pair-key {self_pair_key}"
        if self_pair_key else f"peer reset --to {args.to}"
    )

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        target = conn.execute(
            "SELECT last_seen_at, channel_socket FROM sessions WHERE cwd = ? AND label = ?",
            (str(to_cwd), args.to),
        ).fetchone()
        if target is None:
            conn.execute("ROLLBACK")
            err(f"peer not found: {args.to} in {to_cwd}", EXIT_NOT_FOUND)
        if seconds_since(target["last_seen_at"]) > STALE_THRESHOLD_SECS:
            conn.execute("ROLLBACK")
            err(
                f"peer offline: {args.to} (last seen "
                f"{int(seconds_since(target['last_seen_at']))}s ago)",
                EXIT_PEER_OFFLINE,
            )

        room = _get_room(conn, room_key)
        if room and room["terminated_at"]:
            conn.execute("ROLLBACK")
            err(
                f"room terminated at {room['terminated_at']} "
                f"by {room['terminated_by']}; "
                f"run 'agent-collab {reset_hint}' to resume",
                EXIT_VALIDATION,
            )
        current = room["turn_count"] if room else 0
        max_turns = _max_room_turns()
        if current >= max_turns:
            conn.execute("ROLLBACK")
            err(
                f"room reached max turns ({current}/{max_turns}); "
                f"set AGENT_COLLAB_MAX_PAIR_TURNS or run "
                f"'agent-collab {reset_hint}'",
                EXIT_VALIDATION,
            )

        terminates = TERMINATION_TOKEN.lower() in body.lower()

        now = now_iso()
        conn.execute(
            """
            INSERT INTO inbox
              (to_cwd, to_label, from_cwd, from_label, body, created_at, room_key)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (str(to_cwd), args.to, str(self_cwd), self_label, body, now, room_key),
        )
        _bump_room(conn, room_key, self_pair_key)
        if terminates:
            _terminate_room(conn, room_key, self_pair_key, self_label)
        bump_last_seen(conn, str(self_cwd), self_label)
        conn.execute("COMMIT")

        recipient_socket = target["channel_socket"]
    finally:
        conn.close()

    members = _room_members(str(self_cwd), self_label, self_pair_key)
    mentions = _resolve_mentions(
        body, getattr(args, "mention", None), members, self_label,
    )

    # Best-effort real-time push to the recipient's channel. If no socket
    # bound or delivery fails, the SQLite write already succeeded and the
    # hook path will pick the message up on the recipient's next turn.
    push_status = "no-channel"
    if recipient_socket and Path(recipient_socket).exists():
        push_meta: dict[str, str] = {"to": args.to}
        if mentions:
            push_meta["mentions"] = ",".join(mentions)
        code, resp = _send_over_unix_socket(
            recipient_socket,
            {"from": self_label, "body": body, "meta": push_meta},
        )
        if code == 200:
            push_status = "pushed"
        else:
            push_status = f"push-failed({code})"

    term_note = " [[end]]→room terminated" if terminates else ""
    mention_note = f" @mentions[{','.join(mentions)}]" if mentions else ""
    print(f"sent to {args.to} ({push_status}){term_note}{mention_note}")
    return EXIT_OK


def cmd_peer_broadcast(args: argparse.Namespace) -> int:
    """Fan-out send to every live peer in the sender's scope (pair_key or cwd).

    Sender-side fan-out: N unicast inbox rows + N best-effort channel pushes
    in a single transaction. Accounting is room-level (one turn regardless
    of recipients) when a pair_key is set; cwd-only broadcasts bump each
    synthesized per-edge room independently.
    """
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)
    self_pair_key = _get_session_pair_key(str(self_cwd), self_label)

    if args.message_stdin:
        body = sys.stdin.read()
    elif args.message_file:
        body = Path(args.message_file).read_text()
    elif args.message is not None:
        body = args.message
    else:
        err("one of --message, --message-file, --message-stdin required", EXIT_VALIDATION)

    body_bytes = body.encode("utf-8")
    if len(body_bytes) > MAX_BODY_BYTES:
        err(
            f"message too large: {len(body_bytes)} bytes > {MAX_BODY_BYTES} cap",
            EXIT_VALIDATION,
        )
    if len(body_bytes) == 0:
        err("empty message rejected", EXIT_VALIDATION)

    # Optional multicast filter: --to may be passed multiple times to
    # restrict delivery to a named subset of the room while still
    # counting as one room turn.
    cohort: Optional[set[str]] = None
    if getattr(args, "to", None):
        cohort = {t for t in args.to if t}
        for t in cohort:
            validate_label(t)
        if self_label in cohort:
            err(f"cannot multicast to self: {self_label!r}", EXIT_VALIDATION)

    # Resolve recipients and filter out stale sessions.
    conn = open_db()
    try:
        if self_pair_key is not None:
            rows = conn.execute(
                "SELECT cwd, label, last_seen_at, channel_socket "
                "FROM sessions WHERE pair_key = ? "
                "AND NOT (cwd = ? AND label = ?)",
                (self_pair_key, str(self_cwd), self_label),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT cwd, label, last_seen_at, channel_socket "
                "FROM sessions WHERE cwd = ? "
                "AND NOT (cwd = ? AND label = ?)",
                (str(self_cwd), str(self_cwd), self_label),
            ).fetchall()
    finally:
        conn.close()

    live = [
        r for r in rows
        if seconds_since(r["last_seen_at"]) <= STALE_THRESHOLD_SECS
    ]
    if cohort is not None:
        present = {r["label"] for r in live}
        missing = cohort - present
        if missing:
            err(
                f"unknown or stale peer(s) in multicast: {sorted(missing)}",
                EXIT_NOT_FOUND,
            )
        live = [r for r in live if r["label"] in cohort]
    if not live:
        scope = f"pair_key={self_pair_key}" if self_pair_key else f"cwd={self_cwd}"
        err(f"no live peers in {scope}", EXIT_NOT_FOUND)

    terminates = TERMINATION_TOKEN.lower() in body.lower()
    max_turns = _max_room_turns()
    now = now_iso()

    # Plan per-recipient room keys. pair_key mode collapses to a single
    # room_key; cwd mode gives each edge its own room.
    recipients: list[tuple[sqlite3.Row, str]] = [
        (r, _room_key_for(str(self_cwd), self_label, r["label"], self_pair_key))
        for r in live
    ]

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")

        # Pre-check rooms so a partial failure never leaves a half-delivered
        # broadcast. In pair_key mode this is the same row for every
        # recipient; dedup before checking.
        checked: set[str] = set()
        for _, room_key in recipients:
            if room_key in checked:
                continue
            checked.add(room_key)
            room = _get_room(conn, room_key)
            reset_hint = (
                f"peer reset --pair-key {self_pair_key}"
                if self_pair_key else f"peer reset --room-key {room_key}"
            )
            if room and room["terminated_at"]:
                conn.execute("ROLLBACK")
                err(
                    f"room {room_key} terminated at {room['terminated_at']} "
                    f"by {room['terminated_by']}; "
                    f"run 'agent-collab {reset_hint}' to resume",
                    EXIT_VALIDATION,
                )
            current = room["turn_count"] if room else 0
            if current >= max_turns:
                conn.execute("ROLLBACK")
                err(
                    f"room {room_key} reached max turns ({current}/{max_turns}); "
                    f"set AGENT_COLLAB_MAX_PAIR_TURNS or run "
                    f"'agent-collab {reset_hint}'",
                    EXIT_VALIDATION,
                )

        for r, room_key in recipients:
            conn.execute(
                """
                INSERT INTO inbox
                  (to_cwd, to_label, from_cwd, from_label, body, created_at, room_key)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (r["cwd"], r["label"], str(self_cwd), self_label, body, now, room_key),
            )

        # Bump each distinct room exactly once. In pair_key mode that's a
        # single counter regardless of recipient count.
        bumped: set[str] = set()
        for _, room_key in recipients:
            if room_key in bumped:
                continue
            bumped.add(room_key)
            _bump_room(conn, room_key, self_pair_key)
            if terminates:
                _terminate_room(conn, room_key, self_pair_key, self_label)

        bump_last_seen(conn, str(self_cwd), self_label)
        conn.execute("COMMIT")
    finally:
        conn.close()

    members = _room_members(str(self_cwd), self_label, self_pair_key)
    mentions = _resolve_mentions(
        body, getattr(args, "mention", None), members, self_label,
    )
    cohort_labels = (
        ",".join(sorted(r["label"] for r in live))
        if cohort is not None else None
    )

    statuses: list[str] = []
    for r in live:
        sock = r["channel_socket"]
        status = "no-channel"
        if sock and Path(sock).exists():
            push_meta: dict[str, str] = {
                "to": r["label"],
                "broadcast": "1",
            }
            if cohort_labels is not None:
                push_meta["cohort"] = cohort_labels
            if mentions:
                push_meta["mentions"] = ",".join(mentions)
            code, _ = _send_over_unix_socket(
                sock,
                {"from": self_label, "body": body, "meta": push_meta},
            )
            status = "pushed" if code == 200 else f"push-failed({code})"
        statuses.append(f"{r['label']} ({status})")

    term_note = " [[end]]→room terminated" if terminates else ""
    mention_note = f" @mentions[{','.join(mentions)}]" if mentions else ""
    kind = "multicast" if cohort is not None else "broadcast"
    print(f"{kind} to {len(live)} peer(s): {', '.join(statuses)}{term_note}{mention_note}")
    return EXIT_OK


def format_hook_block(rows: list[sqlite3.Row]) -> str:
    """Compose the <peer-inbox>...</peer-inbox> block, capped at HOOK_BLOCK_BUDGET."""
    if not rows:
        return ""
    labels = sorted({r["from_label"] for r in rows})
    parts = [
        f'<peer-inbox messages="{len(rows)}" '
        f'from-session-labels="{",".join(labels)}">'
    ]
    used = sum(len(p) + 1 for p in parts)
    included = 0
    truncated = 0
    for r in rows:
        entry = f"\n[{r['from_label']} @ {r['created_at']}]\n{r['body']}\n"
        if used + len(entry) > HOOK_BLOCK_BUDGET and included > 0:
            truncated = len(rows) - included
            break
        parts.append(entry)
        used += len(entry)
        included += 1
    if truncated:
        parts.append(
            f"\n[+{truncated} more messages truncated; "
            "run agent-collab peer receive to view]\n"
        )
    parts.append("</peer-inbox>")
    return "".join(parts)


def cmd_peer_receive(args: argparse.Namespace) -> int:
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)

    conn = open_db()
    try:
        if args.mark_read:
            # Atomic claim-and-mark in a writer transaction. The UPDATE ...
            # RETURNING clause guarantees no concurrent receiver can claim
            # the same rows.
            conn.execute("BEGIN IMMEDIATE")
            now = now_iso()
            if args.since:
                rows = conn.execute(
                    """
                    UPDATE inbox
                    SET read_at = ?
                    WHERE to_cwd = ? AND to_label = ?
                      AND read_at IS NULL
                      AND created_at >= ?
                    RETURNING id, from_cwd, from_label, body, created_at
                    """,
                    (now, str(self_cwd), self_label, args.since),
                ).fetchall()
            else:
                rows = conn.execute(
                    """
                    UPDATE inbox
                    SET read_at = ?
                    WHERE to_cwd = ? AND to_label = ?
                      AND read_at IS NULL
                    RETURNING id, from_cwd, from_label, body, created_at
                    """,
                    (now, str(self_cwd), self_label),
                ).fetchall()
            bump_last_seen(conn, str(self_cwd), self_label)
            conn.execute("COMMIT")
        else:
            # Read-only inspect path: plain SELECT with no writer lock. No
            # last_seen_at update here — sessions that only ever inspect
            # don't need to appear active to peers. Active participation
            # (peer send, session register, mark-read receives) bumps
            # last_seen_at; inspection is free.
            if args.since:
                rows = conn.execute(
                    """
                    SELECT id, from_cwd, from_label, body, created_at, read_at
                    FROM inbox
                    WHERE to_cwd = ? AND to_label = ?
                      AND read_at IS NULL
                      AND created_at >= ?
                    ORDER BY created_at ASC
                    """,
                    (str(self_cwd), self_label, args.since),
                ).fetchall()
            else:
                rows = conn.execute(
                    """
                    SELECT id, from_cwd, from_label, body, created_at, read_at
                    FROM inbox
                    WHERE to_cwd = ? AND to_label = ?
                      AND read_at IS NULL
                    ORDER BY created_at ASC
                    """,
                    (str(self_cwd), self_label),
                ).fetchall()
    finally:
        conn.close()

    if args.format == "json":
        payload = [
            {
                "id": r["id"],
                "from_cwd": r["from_cwd"],
                "from_label": r["from_label"],
                "body": r["body"],
                "created_at": r["created_at"],
            }
            for r in rows
        ]
        json.dump(payload, sys.stdout, ensure_ascii=False)
        sys.stdout.write("\n")
    elif args.format == "hook":
        sys.stdout.write(format_hook_block(list(rows)))
        if rows:
            sys.stdout.write("\n")
    elif args.format == "hook-json":
        block = format_hook_block(list(rows))
        envelope = {
            "hookSpecificOutput": {
                "hookEventName": "UserPromptSubmit",
                "additionalContext": block,
            }
        } if block else {}
        if envelope:
            json.dump(envelope, sys.stdout, ensure_ascii=False)
            sys.stdout.write("\n")
    else:  # plain
        if not rows:
            return EXIT_OK
        for r in rows:
            print(f"[{r['from_label']} @ {r['created_at']}]")
            print(r["body"])
            print()
    return EXIT_OK


def cmd_hook_log_session(args: argparse.Namespace) -> int:
    """Hook-only helper: record a Claude session_id seen in this cwd."""
    cwd = resolve_cwd(args.cwd)
    sid = args.session_id.strip()
    if not sid:
        return EXIT_OK  # silent no-op on empty input
    log_seen_session(cwd, sid)
    return EXIT_OK


def cmd_session_adopt(args: argparse.Namespace) -> int:
    """Re-key an existing registration to a known Claude session_id.

    Used to reconcile sessions that registered with a random key before the
    hook started logging session_ids. Rewrites the DB row's session_key and
    renames the marker file.
    """
    validate_label(args.label)
    cwd = resolve_cwd(args.cwd)
    new_key = args.session_id.strip()
    if not new_key:
        err("--session-id is empty", EXIT_VALIDATION)

    conn = open_db()
    try:
        row = conn.execute(
            "SELECT session_key FROM sessions WHERE cwd = ? AND label = ?",
            (str(cwd), args.label),
        ).fetchone()
        if row is None:
            err(
                f"no registered session for label {args.label!r} in {cwd}",
                EXIT_NOT_FOUND,
            )
        old_key = row["session_key"]
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            "UPDATE sessions SET session_key = ? WHERE cwd = ? AND label = ?",
            (new_key, str(cwd), args.label),
        )
        conn.execute("COMMIT")
    finally:
        conn.close()

    if old_key:
        old_marker = marker_path_for(cwd, old_key)
        if old_marker.is_file():
            old_marker.unlink()
    new_marker = marker_path_for(cwd, new_key)
    new_marker.parent.mkdir(mode=0o755, exist_ok=True, parents=True)
    new_marker.write_text(
        json.dumps(
            {"cwd": str(cwd), "label": args.label, "session_key": new_key},
            ensure_ascii=False,
        )
    )
    print(
        f"adopted: {args.label} at {cwd} "
        f"[session_key={session_key_hash(new_key)}]"
    )
    return EXIT_OK


def cmd_peer_web(args: argparse.Namespace) -> int:
    """Serve a Slack-shaped live view of peer traffic.

    Scope modes (mutually exclusive):
      - default: current cwd. Shows pairs whose messages stayed within cwd.
      - --pair-key KEY: v1.7 pair key. Shows the cross-cwd conversation
        between every label that joined that pair key as a single room
        with interleaved messages (v2.0 step 3).

    Endpoints:
      GET /                                  → SPA HTML
      GET /scope.json                        → {"mode": "cwd"|"pair_key", ...}
      GET /pairs.json                        → cwd-mode edge pair list
      GET /rooms.json                        → pair-key-mode room summary
      GET /messages.json?pair=a+b&after=N    → one edge pair's stream
      GET /messages.json?pair_key=K&after=N  → the room's interleaved stream
      GET /messages.json?after=N             → all scoped messages (back-compat)
    """
    from http.server import BaseHTTPRequestHandler, HTTPServer
    from urllib.parse import parse_qs, urlparse

    pair_key = getattr(args, "pair_key", None)
    if pair_key:
        validate_pair_key(pair_key)
    cwd = resolve_cwd(args.cwd)
    as_label = args.as_label
    if as_label:
        validate_label(as_label)
    port = int(args.port)

    # In pair-key mode, resolve the set of (cwd, label) the pair spans.
    # Messages in this view = inbox rows where BOTH endpoints are registered
    # under this pair_key. Sessions may live in different cwds.
    pair_members: list[tuple[str, str]] = []
    if pair_key:
        c = open_db()
        try:
            pair_members = [
                (r["cwd"], r["label"])
                for r in c.execute(
                    "SELECT cwd, label FROM sessions WHERE pair_key = ? ORDER BY label",
                    (pair_key,),
                ).fetchall()
            ]
        finally:
            c.close()
        if not pair_members:
            err(f"no sessions registered with pair_key {pair_key!r}", EXIT_NOT_FOUND)

    def _canonical_pair(a: str, b: str) -> tuple[str, str]:
        return (a, b) if a <= b else (b, a)

    def _fetch_pairs() -> list[dict]:
        """Aggregate pair metadata from inbox + sessions + peer_pairs tables."""
        c = open_db()
        try:
            # Distinct pairs ever active in this scope. cwd mode groups
            # within one cwd (v1.6 behaviour). pair-key mode groups by
            # label pairs whose endpoints both belong to the pair, across
            # any cwd.
            if pair_key:
                member_labels = tuple({lbl for (_, lbl) in pair_members})
                if not member_labels:
                    rows = []
                else:
                    q_marks = ",".join("?" for _ in member_labels)
                    rows = c.execute(
                        f"""
                        SELECT
                          CASE WHEN from_label < to_label THEN from_label ELSE to_label END AS a,
                          CASE WHEN from_label < to_label THEN to_label ELSE from_label END AS b,
                          MAX(id)              AS last_id,
                          MAX(created_at)      AS last_at,
                          COUNT(*)             AS total,
                          SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread_backend
                        FROM inbox
                        WHERE from_label IN ({q_marks}) AND to_label IN ({q_marks})
                        GROUP BY a, b
                        ORDER BY last_at DESC
                        """,
                        member_labels + member_labels,
                    ).fetchall()
            else:
                rows = c.execute(
                    """
                    SELECT
                      CASE WHEN from_label < to_label THEN from_label ELSE to_label END AS a,
                      CASE WHEN from_label < to_label THEN to_label ELSE from_label END AS b,
                      MAX(id)              AS last_id,
                      MAX(created_at)      AS last_at,
                      COUNT(*)             AS total,
                      SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread_backend
                    FROM inbox
                    WHERE to_cwd = ? AND from_cwd = ?
                    GROUP BY a, b
                    ORDER BY last_at DESC
                    """,
                    (str(cwd), str(cwd)),
                ).fetchall()

            # Per-pair session activity + termination.
            pairs = []
            for r in rows:
                a, b = r["a"], r["b"]
                if pair_key:
                    # Look up sessions by (pair_key, label) — any cwd.
                    sess_a = c.execute(
                        "SELECT last_seen_at, agent, role, channel_socket FROM sessions "
                        "WHERE pair_key = ? AND label = ?",
                        (pair_key, a),
                    ).fetchone()
                    sess_b = c.execute(
                        "SELECT last_seen_at, agent, role, channel_socket FROM sessions "
                        "WHERE pair_key = ? AND label = ?",
                        (pair_key, b),
                    ).fetchone()
                    pair_state = None  # peer_pairs is cwd-keyed; skip in pair-key mode.
                else:
                    sess_a = c.execute(
                        "SELECT last_seen_at, agent, role, channel_socket FROM sessions "
                        "WHERE cwd = ? AND label = ?",
                        (str(cwd), a),
                    ).fetchone()
                    sess_b = c.execute(
                        "SELECT last_seen_at, agent, role, channel_socket FROM sessions "
                        "WHERE cwd = ? AND label = ?",
                        (str(cwd), b),
                    ).fetchone()
                    # cwd-mode edge rooms live under "cwd:<cwd>#<a>+<b>".
                    edge_room_key = f"cwd:{cwd}#{a}+{b}"
                    pair_state = c.execute(
                        "SELECT turn_count, terminated_at, terminated_by FROM peer_rooms "
                        "WHERE room_key = ?",
                        (edge_room_key,),
                    ).fetchone()

                def _peer_info(s) -> dict:
                    if s is None:
                        return {"agent": None, "role": None, "activity": "unknown",
                                "last_seen_at": None, "channel": False}
                    return {
                        "agent": s["agent"],
                        "role": s["role"],
                        "activity": activity_state(s["last_seen_at"]),
                        "last_seen_at": s["last_seen_at"],
                        "channel": s["channel_socket"] is not None,
                    }

                ai = _peer_info(sess_a)
                bi = _peer_info(sess_b)

                # Pair activity = worst of the two (any stale → pair stale).
                def _worse(x: str, y: str) -> str:
                    order = {"unknown": -1, "active": 0, "idle": 1, "stale": 2}
                    return x if order.get(x, -1) >= order.get(y, -1) else y
                pair_activity = _worse(ai["activity"], bi["activity"])

                terminated = pair_state["terminated_at"] if pair_state else None
                if terminated:
                    pair_activity = "terminated"

                pairs.append({
                    "a": a,
                    "b": b,
                    "key": f"{a}+{b}",
                    "activity": pair_activity,
                    "total": r["total"],
                    "last_id": r["last_id"],
                    "last_at": r["last_at"],
                    "turn_count": pair_state["turn_count"] if pair_state else 0,
                    "terminated_at": terminated,
                    "terminated_by": pair_state["terminated_by"] if pair_state else None,
                    "peers": {a: ai, b: bi},
                })
            return pairs
        finally:
            c.close()

    def _fetch_rooms() -> list[dict]:
        """Return one room entry per pair_key. Pair-key mode only.

        A room aggregates every session that joined the pair_key regardless
        of cwd. The single-process web server is always scoped to one
        pair_key today, so this list has exactly one entry; the shape is
        a list to leave room for multi-room dashboards.
        """
        if not pair_key:
            return []
        c = open_db()
        try:
            member_labels = sorted({lbl for (_, lbl) in pair_members})
            if not member_labels:
                return []
            room_key_value = f"pk:{pair_key}"
            stats = c.execute(
                """
                SELECT
                  MAX(id)         AS last_id,
                  MAX(created_at) AS last_at,
                  COUNT(*)        AS total,
                  SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread
                FROM inbox
                WHERE room_key = ?
                """,
                (room_key_value,),
            ).fetchone()

            members: list[dict] = []
            best_activity = "unknown"
            order = {"unknown": -1, "stale": 0, "idle": 1, "active": 2}
            for lbl in member_labels:
                s = c.execute(
                    "SELECT cwd, last_seen_at, agent, role, channel_socket "
                    "FROM sessions WHERE pair_key = ? AND label = ?",
                    (pair_key, lbl),
                ).fetchone()
                if s is None:
                    info = {
                        "label": lbl, "cwd": None, "agent": None,
                        "role": None, "activity": "unknown",
                        "last_seen_at": None, "channel": False,
                    }
                else:
                    act = activity_state(s["last_seen_at"])
                    info = {
                        "label": lbl,
                        "cwd": s["cwd"],
                        "agent": s["agent"],
                        "role": s["role"],
                        "activity": act,
                        "last_seen_at": s["last_seen_at"],
                        "channel": s["channel_socket"] is not None,
                    }
                    if order.get(act, -1) > order.get(best_activity, -1):
                        best_activity = act
                members.append(info)

            total = stats["total"] or 0 if stats else 0
            last_id = stats["last_id"] or 0 if stats else 0
            last_at = stats["last_at"] if stats else None

            room_state = c.execute(
                "SELECT turn_count, terminated_at, terminated_by FROM peer_rooms "
                "WHERE room_key = ?",
                (f"pk:{pair_key}",),
            ).fetchone()
            activity = best_activity if members else "unknown"
            if room_state and room_state["terminated_at"]:
                activity = "terminated"
            return [{
                "pair_key": pair_key,
                "key": pair_key,
                "activity": activity,
                "total": total,
                "last_id": last_id,
                "last_at": last_at,
                "turn_count": room_state["turn_count"] if room_state else 0,
                "terminated_at": room_state["terminated_at"] if room_state else None,
                "terminated_by": room_state["terminated_by"] if room_state else None,
                "members": members,
            }]
        finally:
            c.close()

    def _fetch_messages(after: int, pair: Optional[tuple[str, str]]) -> list[sqlite3.Row]:
        c = open_db()
        try:
            if pair_key:
                room_key_value = f"pk:{pair_key}"
                if pair is not None:
                    pa, pb = pair
                    return c.execute(
                        """
                        SELECT id, from_label, to_label, body, created_at, read_at
                        FROM inbox
                        WHERE id > ? AND room_key = ?
                          AND ((from_label = ? AND to_label = ?)
                            OR (from_label = ? AND to_label = ?))
                        ORDER BY id ASC
                        """,
                        (after, room_key_value, pa, pb, pb, pa),
                    ).fetchall()
                return c.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE id > ? AND room_key = ?
                    ORDER BY id ASC
                    """,
                    (after, room_key_value),
                ).fetchall()
            if pair is not None:
                pa, pb = pair
                return c.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE id > ? AND to_cwd = ? AND from_cwd = ?
                      AND ((from_label = ? AND to_label = ?)
                        OR (from_label = ? AND to_label = ?))
                    ORDER BY id ASC
                    """,
                    (after, str(cwd), str(cwd), pa, pb, pb, pa),
                ).fetchall()
            if as_label:
                return c.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE id > ?
                      AND ((to_cwd = ? AND to_label = ?) OR (from_cwd = ? AND from_label = ?))
                    ORDER BY id ASC
                    """,
                    (after, str(cwd), as_label, str(cwd), as_label),
                ).fetchall()
            return c.execute(
                """
                SELECT id, from_label, to_label, body, created_at, read_at
                FROM inbox
                WHERE id > ? AND (to_cwd = ? OR from_cwd = ?)
                ORDER BY id ASC
                """,
                (after, str(cwd), str(cwd)),
            ).fetchall()
        finally:
            c.close()

    if pair_key:
        member_labels = sorted({lbl for (_, lbl) in pair_members})
        scope_desc = f"pair {pair_key} ({', '.join(member_labels)})"
    elif as_label:
        scope_desc = f"conversations involving {as_label} in {cwd}"
    else:
        scope_desc = f"all cross-session messages in {cwd}"
    title = f"peer-inbox — {scope_desc}"
    index_html = _PEER_WEB_HTML_TEMPLATE.replace("__TITLE__", html_lib.escape(title)) \
                                         .replace("__CWD__", html_lib.escape(scope_desc))

    def _send_json(handler, code: int, payload: dict) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        handler.send_response(code)
        handler.send_header("Content-Type", "application/json; charset=utf-8")
        handler.send_header("Content-Length", str(len(body)))
        handler.send_header("Cache-Control", "no-store")
        handler.end_headers()
        handler.wfile.write(body)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def do_GET(self) -> None:
            parsed = urlparse(self.path)
            if parsed.path in ("/", "/index.html"):
                body = index_html.encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                self.wfile.write(body)
                return

            if parsed.path == "/scope.json":
                _send_json(self, 200, {
                    "mode": "pair_key" if pair_key else "cwd",
                    "cwd": str(cwd),
                    "pair_key": pair_key,
                    "as_label": as_label,
                })
                return

            if parsed.path == "/pairs.json":
                _send_json(self, 200, {"cwd": str(cwd), "pairs": _fetch_pairs()})
                return

            if parsed.path == "/rooms.json":
                if not pair_key:
                    _send_json(self, 400, {
                        "error": "rooms.json only available in --pair-key mode",
                    })
                    return
                _send_json(self, 200, {
                    "pair_key": pair_key,
                    "rooms": _fetch_rooms(),
                })
                return

            if parsed.path == "/messages.json":
                q = parse_qs(parsed.query)
                try:
                    after = int(q.get("after", ["0"])[0])
                except ValueError:
                    after = 0
                # Accept two styles for back-compat:
                # 1. ?a=X&b=Y (preferred; avoids + → space URL-decode trap).
                # 2. ?pair=X+Y or ?pair=X:Y (for hand-crafted curls).
                # 3. ?pair_key=KEY selects the whole room (pair-key mode only).
                a_arg = q.get("a", [None])[0]
                b_arg = q.get("b", [None])[0]
                pair_arg = q.get("pair", [None])[0]
                pk_arg = q.get("pair_key", [None])[0]
                pair: Optional[tuple[str, str]] = None
                if pk_arg and pair_key and pk_arg != pair_key:
                    _send_json(self, 400, {
                        "error": f"pair_key {pk_arg!r} does not match server scope {pair_key!r}",
                    })
                    return
                if a_arg and b_arg:
                    pair = _canonical_pair(a_arg, b_arg)
                elif pair_arg:
                    for sep in (":", " ", "+"):
                        if sep in pair_arg:
                            a, b = pair_arg.split(sep, 1)
                            pair = _canonical_pair(a.strip(), b.strip())
                            break
                rows = _fetch_messages(after, pair)
                _send_json(
                    self,
                    200,
                    {
                        "cwd": str(cwd),
                        "pair_key": pair_key,
                        "pair": f"{pair[0]}+{pair[1]}" if pair else None,
                        "messages": [
                            {
                                "id": r["id"],
                                "from": r["from_label"],
                                "to": r["to_label"],
                                "body": r["body"],
                                "created_at": r["created_at"],
                                "read": r["read_at"] is not None,
                            }
                            for r in rows
                        ],
                    },
                )
                return

            self.send_response(404)
            self.end_headers()

    server = HTTPServer(("127.0.0.1", port), Handler)
    print(f"peer-inbox live view: http://127.0.0.1:{port}  (Ctrl-C to stop)", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("(stopped)", file=sys.stderr)
    finally:
        server.server_close()
    return EXIT_OK


_PEER_WEB_HTML_TEMPLATE = """<!doctype html>
<html lang=en>
<meta charset=utf-8>
<title>__TITLE__</title>
<style>
  :root {
    --bg:#0e1015; --fg:#e6e6e6; --muted:#8a8f98; --accent:#6cd9ff;
    --panel:#181b23; --border:#262a33; --sidebar-bg:#12141b;
    --hover:#1f242e; --selected:#223041; --unread:#6cd9ff;
    --terminated:#f59e0b;
  }
  * { box-sizing:border-box }
  html,body { margin:0; padding:0; background:var(--bg); color:var(--fg);
    font:14px/1.5 -apple-system,BlinkMacSystemFont,Segoe UI,system-ui,sans-serif;
    height:100%; }
  .app { display:grid; grid-template-columns:280px 1fr; height:100vh;
    overflow:hidden; }

  /* Sidebar */
  aside { background:var(--sidebar-bg); border-right:1px solid var(--border);
    overflow-y:auto; }
  aside .brand { padding:16px; border-bottom:1px solid var(--border);
    display:flex; align-items:center; gap:8px; }
  aside .brand h1 { margin:0; font-size:13px; font-weight:600;
    letter-spacing:.04em; text-transform:uppercase; color:var(--fg); }
  aside .brand .dot { width:7px; height:7px; border-radius:50%;
    background:var(--accent); box-shadow:0 0 6px var(--accent);
    animation:pulse 2s infinite; }
  @keyframes pulse { 0%,100% { opacity:1 } 50% { opacity:.35 } }
  aside .cwd { padding:0 16px 12px; color:var(--muted); font-size:11px;
    word-break:break-all; border-bottom:1px solid var(--border); }
  aside .section-label { padding:12px 16px 4px; font-size:11px;
    color:var(--muted); letter-spacing:.12em; text-transform:uppercase; }
  .pair-item { display:block; padding:8px 16px; cursor:pointer;
    border-left:2px solid transparent; color:var(--fg);
    text-decoration:none; user-select:none; }
  .pair-item:hover { background:var(--hover); }
  .pair-item.selected { background:var(--selected);
    border-left-color:var(--accent); }
  .pair-item.terminated .labels,
  .pair-item.terminated .meta-row { opacity:.55;
    text-decoration:line-through; }
  .pair-item .labels { display:flex; align-items:center; gap:6px;
    font-size:13px; font-weight:500; }
  .pair-item .labels .sep { color:var(--muted); font-size:11px; }
  .pair-item .unread-badge { margin-left:auto; min-width:18px;
    padding:1px 6px; border-radius:9px; background:var(--unread);
    color:#0e1015; font-size:10px; font-weight:600; text-align:center;
    display:none; }
  .pair-item.has-unread .unread-badge { display:inline-block }
  .pair-item .meta-row { display:flex; gap:8px; font-size:11px;
    color:var(--muted); margin-top:2px; }
  .activity-dot { width:6px; height:6px; border-radius:50%;
    display:inline-block; margin-right:4px; }
  .activity-dot.active { background:#22c55e }
  .activity-dot.idle { background:#eab308 }
  .activity-dot.stale { background:#6b7280 }
  .activity-dot.terminated { background:var(--terminated) }
  .activity-dot.unknown { background:#4b5563 }

  /* Main pane */
  main { display:flex; flex-direction:column; overflow:hidden; }
  .pair-header { padding:14px 24px; border-bottom:1px solid var(--border);
    background:var(--bg); }
  .pair-header h2 { margin:0; font-size:15px; font-weight:500;
    display:flex; align-items:center; gap:8px; }
  .pair-header .sub { color:var(--muted); font-size:12px; margin-top:4px;
    display:flex; gap:12px; flex-wrap:wrap; align-items:center; }
  .pill { color:#1a1d24; padding:2px 8px; border-radius:10px;
    font-weight:500; font-size:11px; }
  .arrow { color:var(--muted) }

  .stream { flex:1; overflow-y:auto; padding:16px 24px 40px; }
  .empty { color:var(--muted); text-align:center; padding:60px 20px;
    font-size:13px; }
  .day { color:var(--muted); text-align:center; margin:20px 0 8px;
    font-size:11px; letter-spacing:.15em; text-transform:uppercase; }
  .msg { margin:10px 0; padding:10px 14px; background:var(--panel);
    border:1px solid var(--border); border-radius:8px; max-width:760px; }
  .msg.new { animation:flash 1.2s ease-out; }
  @keyframes flash { 0% { background:#2a3340 } 100% { background:var(--panel) } }
  .msg.end-marker { border-color:var(--terminated);
    background:rgba(245,158,11,.08); }
  .meta { display:flex; gap:8px; align-items:center; flex-wrap:wrap;
    font-size:11px; color:var(--muted); margin-bottom:5px; }
  .time { margin-left:auto; font-family:SF Mono,Menlo,monospace; }
  .id { font-family:SF Mono,Menlo,monospace; opacity:.5 }
  .body { margin:0; white-space:pre-wrap; word-wrap:break-word;
    color:var(--fg); font:13px/1.55 SF Mono,Menlo,Consolas,monospace; }
  .term-banner { margin:16px 0; padding:10px 14px;
    background:rgba(245,158,11,.08); border:1px dashed var(--terminated);
    border-radius:8px; color:var(--terminated); font-size:12px;
    text-align:center; }

  .scroll-btn { position:absolute; bottom:20px; right:32px; padding:6px 12px;
    background:var(--panel); color:var(--fg); border:1px solid var(--border);
    border-radius:6px; cursor:pointer; font-size:12px; display:none; }
  .scroll-btn.show { display:block }
</style>
<body>
<div class=app>
  <aside>
    <div class=brand>
      <span class=dot></span>
      <h1>peer-inbox</h1>
    </div>
    <div class=cwd id=cwd>__CWD__</div>
    <div id=sidebar></div>
  </aside>
  <main>
    <div class=pair-header id=pairHeader>
      <h2>select a pair</h2>
      <div class=sub id=pairSub>from the sidebar →</div>
    </div>
    <div class=stream id=stream>
      <div class=empty id=emptyMain>pick a conversation on the left to view its messages</div>
    </div>
    <button class=scroll-btn id=scrollBtn>↓ new messages</button>
  </main>
</div>
<script>
(() => {
  const sidebar = document.getElementById('sidebar');
  const pairHeader = document.getElementById('pairHeader');
  const pairSub = document.getElementById('pairSub');
  const stream = document.getElementById('stream');
  const emptyMain = document.getElementById('emptyMain');
  const scrollBtn = document.getElementById('scrollBtn');

  const state = {
    mode: null,               // 'cwd' | 'pair_key'
    pairKey: null,            // scope pair_key in pair_key mode
    pairs: {},                // key -> metadata (edge pair in cwd mode, room in pair_key mode)
    selectedKey: null,
    messagesByPair: {},       // key -> [messages]
    lastIdByPair: {},         // key -> highest seen id
    lastSeenIdByPair: {},     // key -> id up to which the viewer has seen
    autoScroll: true,
  };
  const originalTitle = document.title;

  function hue(label) {
    let h = 0;
    for (const c of label || '') h = (h * 31 + c.charCodeAt(0)) % 360;
    return h;
  }
  function pill(label) {
    const el = document.createElement('span');
    el.className = 'pill';
    el.style.background = `hsl(${hue(label)}, 55%, 86%)`;
    el.textContent = label;
    return el;
  }
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function renderSidebar() {
    const items = Object.values(state.pairs);
    const byActivity = { active: [], idle: [], stale: [], terminated: [], unknown: [] };
    for (const p of items) (byActivity[p.activity] || byActivity.unknown).push(p);

    const order = ['active', 'idle', 'stale', 'terminated'];
    sidebar.innerHTML = '';
    let anyShown = false;
    const sectionTitle = state.mode === 'pair_key' ? 'room' : null;
    for (const section of order) {
      const group = byActivity[section] || [];
      if (!group.length) continue;
      anyShown = true;
      sidebar.appendChild(el('div', 'section-label', sectionTitle || section));
      for (const p of group) {
        const item = document.createElement('a');
        item.className = 'pair-item';
        item.classList.toggle('selected', p.key === state.selectedKey);
        if (section === 'terminated') item.classList.add('terminated');
        const unread = Math.max(0, p.last_id - (state.lastSeenIdByPair[p.key] || 0));
        if (unread > 0 && p.key !== state.selectedKey) item.classList.add('has-unread');
        item.href = '#' + p.key;

        const labels = el('div', 'labels');
        if (state.mode === 'pair_key') {
          const members = p.members || [];
          members.forEach((m, i) => {
            if (i > 0) labels.appendChild(el('span', 'sep', '·'));
            labels.appendChild(pill(m.label));
          });
        } else {
          labels.appendChild(pill(p.a));
          labels.appendChild(el('span', 'sep', '↔'));
          labels.appendChild(pill(p.b));
        }
        const badge = el('span', 'unread-badge', unread > 99 ? '99+' : String(unread));
        labels.appendChild(badge);
        item.appendChild(labels);

        const metaRow = el('div', 'meta-row');
        const dot = el('span', 'activity-dot ' + (p.activity || 'unknown'));
        metaRow.appendChild(dot);
        const turnLabel = state.mode === 'pair_key'
          ? p.total + ' msg'
          : p.total + ' msg · ' + p.turn_count + ' turns';
        metaRow.appendChild(el('span', '', turnLabel));
        item.appendChild(metaRow);

        item.addEventListener('click', (ev) => {
          ev.preventDefault();
          selectPair(p.key);
        });
        sidebar.appendChild(item);
      }
    }
    if (!anyShown) {
      sidebar.appendChild(el('div', 'empty',
        state.mode === 'pair_key' ? 'no room members yet' : 'no pairs yet'));
    }
  }

  function renderPairHeader(p) {
    pairHeader.innerHTML = '';
    if (!p) {
      pairHeader.appendChild(el('h2', '',
        state.mode === 'pair_key' ? 'loading room…' : 'select a pair'));
      pairHeader.appendChild(el('div', 'sub',
        state.mode === 'pair_key' ? '' : 'from the sidebar →'));
      return;
    }
    const h = document.createElement('h2');
    if (state.mode === 'pair_key') {
      const members = p.members || [];
      members.forEach((m, i) => {
        if (i > 0) h.appendChild(el('span', 'arrow', '·'));
        h.appendChild(pill(m.label));
      });
    } else {
      h.appendChild(pill(p.a));
      h.appendChild(el('span', 'arrow', '↔'));
      h.appendChild(pill(p.b));
    }
    pairHeader.appendChild(h);

    const sub = el('div', 'sub');
    const ad = el('span', 'activity-dot ' + (p.activity || 'unknown'));
    sub.appendChild(ad);
    const summary = state.mode === 'pair_key'
      ? `${p.activity} · ${p.total} messages`
      : `${p.activity} · ${p.total} messages · ${p.turn_count} turns`;
    sub.appendChild(el('span', '', summary));
    if (p.terminated_at) {
      sub.appendChild(el('span', '',
        `· ended by ${p.terminated_by} at ${p.terminated_at}`));
    }
    if (state.mode === 'pair_key') {
      for (const m of p.members || []) {
        const tag = el('span', '', m.label + ': ' + (m.agent || '?') +
          (m.channel ? ' · channel' : '') +
          (m.role ? ' · ' + m.role : ''));
        sub.appendChild(tag);
      }
    } else {
      for (const who of [p.a, p.b]) {
        const info = p.peers && p.peers[who];
        if (info) {
          const tag = el('span', '', who + ': ' + (info.agent || '?') +
            (info.channel ? ' · channel' : '') +
            (info.role ? ' · ' + info.role : ''));
          sub.appendChild(tag);
        }
      }
    }
    pairHeader.appendChild(sub);
  }

  function groupRoomMessages(messages) {
    // Collapse rows that are the same logical send (same sender, same
    // created_at, same body) into one card with a recipient list.
    // Broadcasts and multicasts arrive as N inbox rows; this makes one
    // conceptual message show as one card.
    const groups = [];
    let cur = null;
    for (const m of messages) {
      if (cur &&
          cur.from === m.from &&
          cur.created_at === m.created_at &&
          cur.body === m.body) {
        cur.recipients.push(m.to);
        cur.ids.push(m.id);
      } else {
        cur = {
          from: m.from, body: m.body, created_at: m.created_at,
          recipients: [m.to], ids: [m.id],
        };
        groups.push(cur);
      }
    }
    return groups;
  }

  function renderStream(pairKey) {
    stream.innerHTML = '';
    const messages = state.messagesByPair[pairKey] || [];
    if (!messages.length) {
      stream.appendChild(el('div', 'empty', 'no messages yet'));
      return;
    }
    const p = state.pairs[pairKey];
    const isRoom = state.mode === 'pair_key';
    const groups = isRoom ? groupRoomMessages(messages)
                          : messages.map(m => ({
                              from: m.from, body: m.body, created_at: m.created_at,
                              recipients: [m.to], ids: [m.id],
                            }));
    const roomSize = isRoom && p && p.members ? p.members.length : 0;

    let prevDay = null;
    for (const g of groups) {
      const day = (g.created_at || '').slice(0, 10);
      if (day && day !== prevDay) {
        stream.appendChild(el('div', 'day', day));
        prevDay = day;
      }
      const wrap = el('div', 'msg');
      wrap.dataset.id = g.ids[0];
      if ((g.body || '').toLowerCase().includes('[[end]]')) wrap.classList.add('end-marker');
      const meta = el('div', 'meta');
      meta.appendChild(pill(g.from || '?'));
      meta.appendChild(el('span', 'arrow', '→'));

      // Recipient rendering:
      //   1 recipient           → pill(to)
      //   everyone in room - 1  → "@room" badge
      //   otherwise             → comma-separated pills (multicast)
      if (g.recipients.length === 1) {
        meta.appendChild(pill(g.recipients[0] || '?'));
      } else if (roomSize && g.recipients.length >= roomSize - 1) {
        const room = el('span', 'pill');
        room.style.background = '#6cd9ff';
        room.textContent = '@room (' + g.recipients.length + ')';
        meta.appendChild(room);
      } else {
        g.recipients.forEach((r, i) => {
          if (i > 0) meta.appendChild(el('span', 'sep', ','));
          meta.appendChild(pill(r));
        });
      }
      meta.appendChild(el('span', 'time', g.created_at || ''));
      meta.appendChild(el('span', 'id', '#' + g.ids[0] +
        (g.ids.length > 1 ? '+' + (g.ids.length - 1) : '')));
      wrap.appendChild(meta);
      wrap.appendChild(el('pre', 'body', g.body || ''));
      stream.appendChild(wrap);
    }
    if (p && p.terminated_at) {
      const banner = el('div', 'term-banner',
        `terminated by ${p.terminated_by} at ${p.terminated_at}`);
      stream.appendChild(banner);
    }
  }

  function selectPair(key) {
    state.selectedKey = key;
    state.lastSeenIdByPair[key] = state.pairs[key] ? state.pairs[key].last_id : 0;
    renderSidebar();
    renderPairHeader(state.pairs[key]);
    renderStream(key);
    if (key && !state.messagesByPair[key]) {
      fetchPairMessages(key);
    }
    state.autoScroll = true;
    requestAnimationFrame(() => { stream.scrollTop = stream.scrollHeight; });
  }

  async function fetchScope() {
    try {
      const r = await fetch('/scope.json', { cache: 'no-store' });
      const data = await r.json();
      state.mode = data.mode;
      state.pairKey = data.pair_key;
    } catch (e) {
      state.mode = 'cwd';  // best-effort fallback; server may be older
    }
  }

  async function fetchPairs() {
    try {
      const url = state.mode === 'pair_key' ? '/rooms.json' : '/pairs.json';
      const listKey = state.mode === 'pair_key' ? 'rooms' : 'pairs';
      const r = await fetch(url, { cache: 'no-store' });
      const data = await r.json();
      const pairs = {};
      let totalUnread = 0;
      for (const p of data[listKey] || []) {
        pairs[p.key] = p;
        const unread = Math.max(0, p.last_id - (state.lastSeenIdByPair[p.key] || 0));
        if (p.key !== state.selectedKey) totalUnread += unread;
      }
      state.pairs = pairs;
      renderSidebar();
      if (state.selectedKey && pairs[state.selectedKey]) {
        renderPairHeader(pairs[state.selectedKey]);
      }
      // Pair-key mode auto-selects the single room so the user lands on
      // the stream without a click.
      if (state.mode === 'pair_key' && !state.selectedKey) {
        const only = Object.keys(pairs)[0];
        if (only) selectPair(only);
      }
      document.title = (totalUnread ? '(' + totalUnread + ') ' : '') + originalTitle;
    } catch (e) {
      console.warn('scope poll failed', e);
    }
  }

  async function fetchPairMessages(pairKey, after) {
    if (!pairKey) return;
    const since = typeof after === 'number' ? after :
                  (state.lastIdByPair[pairKey] || 0);
    const p = state.pairs[pairKey];
    if (!p) return;
    let url;
    if (state.mode === 'pair_key') {
      url = '/messages.json?pair_key=' + encodeURIComponent(p.pair_key) +
            '&after=' + since;
    } else {
      url = '/messages.json?a=' + encodeURIComponent(p.a) +
            '&b=' + encodeURIComponent(p.b) +
            '&after=' + since;
    }
    try {
      const r = await fetch(url, { cache: 'no-store' });
      const data = await r.json();
      if (!(data.messages && data.messages.length)) return;
      const arr = state.messagesByPair[pairKey] || [];
      let maxId = state.lastIdByPair[pairKey] || 0;
      const atBottom = stream.scrollHeight - stream.scrollTop <= stream.clientHeight + 40;
      for (const m of data.messages) {
        arr.push(m);
        if (m.id > maxId) maxId = m.id;
      }
      state.messagesByPair[pairKey] = arr;
      state.lastIdByPair[pairKey] = maxId;
      if (pairKey === state.selectedKey) {
        renderStream(pairKey);
        state.lastSeenIdByPair[pairKey] = maxId;
        if (state.autoScroll || atBottom) {
          requestAnimationFrame(() => { stream.scrollTop = stream.scrollHeight; });
          scrollBtn.classList.remove('show');
        } else {
          scrollBtn.classList.add('show');
        }
      }
    } catch (e) {
      console.warn('messages poll failed', e);
    }
  }

  stream.addEventListener('scroll', () => {
    const atBottom = stream.scrollHeight - stream.scrollTop <= stream.clientHeight + 40;
    state.autoScroll = atBottom;
    if (atBottom) scrollBtn.classList.remove('show');
  });
  scrollBtn.addEventListener('click', () => {
    state.autoScroll = true;
    stream.scrollTop = stream.scrollHeight;
    scrollBtn.classList.remove('show');
  });

  async function tick() {
    await fetchPairs();
    if (state.selectedKey) await fetchPairMessages(state.selectedKey);
  }

  // Support deep-link #a+b to preselect
  const initialHash = (location.hash || '').replace(/^#/, '');

  fetchScope().then(() => tick()).then(() => {
    if (initialHash && state.pairs[initialHash]) {
      selectPair(initialHash);
    }
  });
  setInterval(tick, 1000);
})();
</script>
"""


def cmd_peer_reset(args: argparse.Namespace) -> int:
    """Clear the termination flag + turn counter for a room.

    Exactly one of --to, --pair-key, --room-key must be given.
    --to         resets the cwd-only edge between self and <label>
    --pair-key   resets the whole pair_key-scoped room
    --room-key   resets an arbitrary room by its exact key (escape hatch)
    """
    provided = [bool(args.to), bool(args.pair_key), bool(args.room_key)]
    if sum(provided) != 1:
        err(
            "exactly one of --to, --pair-key, --room-key is required",
            EXIT_VALIDATION,
        )

    if args.room_key:
        room_key = args.room_key
        label_for_msg = room_key
    elif args.pair_key:
        validate_pair_key(args.pair_key)
        room_key = f"pk:{args.pair_key}"
        label_for_msg = f"pair {args.pair_key}"
    else:
        validate_label(args.to)
        cwd = resolve_cwd(args.cwd)
        self_cwd, self_label = resolve_self(args.as_label, cwd)
        room_key = _room_key_for(str(self_cwd), self_label, args.to, None)
        label_for_msg = f"({self_label}, {args.to}) at {self_cwd}"

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        cur = conn.execute(
            "DELETE FROM peer_rooms WHERE room_key = ?",
            (room_key,),
        )
        deleted = cur.rowcount
        conn.execute("COMMIT")
    finally:
        conn.close()
    if deleted:
        print(f"reset: {label_for_msg}")
    else:
        print(f"note: {label_for_msg} had no room state")
    return EXIT_OK


def cmd_peer_round(args: argparse.Namespace) -> int:
    """Mediator-facing sugar: broadcast `[Round N] <message>` to the room.

    Thin wrapper — validates that the caller is registered with
    role='mediator' (warns if not), formats the message with the round
    prefix, and invokes the normal broadcast path. State (current round,
    topic) lives entirely in messages; nothing persists in the DB.
    """
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)

    conn = open_db()
    try:
        row = conn.execute(
            "SELECT role FROM sessions WHERE cwd = ? AND label = ?",
            (str(self_cwd), self_label),
        ).fetchone()
    finally:
        conn.close()
    role = (row["role"] if row else None) or ""
    if role.lower() != "mediator":
        print(
            f"warning: {self_label} is registered with role={role!r}, not "
            "'mediator'. Broadcasting anyway — but consider re-registering "
            "with --role mediator so participants see meta.has_mediator=1.",
            file=sys.stderr,
        )

    if args.round is None:
        err("--round is required (mediator tracks the round number)", EXIT_VALIDATION)
    try:
        round_n = int(args.round)
    except (TypeError, ValueError):
        err(f"--round must be an integer, got {args.round!r}", EXIT_VALIDATION)
    if round_n < 1:
        err("--round must be >= 1", EXIT_VALIDATION)

    if args.message_stdin:
        raw = sys.stdin.read()
    elif args.message_file:
        raw = Path(args.message_file).read_text()
    elif args.message is not None:
        raw = args.message
    else:
        err("one of --message, --message-file, --message-stdin required", EXIT_VALIDATION)

    prefix = f"[Round {round_n}] "
    if args.label:
        if not re.fullmatch(r"[a-z0-9_-]{1,32}", args.label):
            err(
                "--label must be 1-32 chars of [a-z0-9_-]",
                EXIT_VALIDATION,
            )
        prefix = f"[Round {round_n}:{args.label}] "
    body = prefix + raw

    # Dispatch through the normal broadcast path by overriding the args
    # object — no code duplication.
    broadcast_args = argparse.Namespace(
        cwd=args.cwd,
        as_label=args.as_label,
        to=[],  # room-wide
        mention=list(getattr(args, "mention", []) or []),
        message=body,
        message_file=None,
        message_stdin=False,
    )
    return cmd_peer_broadcast(broadcast_args)


def cmd_peer_watch(args: argparse.Namespace) -> int:
    """Tail new inbox rows addressed to self. Blocks until Ctrl-C.

    Read-only: does not mark messages read. The UserPromptSubmit hook still
    consumes them on the next real turn.
    """
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)
    interval = max(0.5, float(args.interval))

    conn = open_db()
    try:
        last_id_row = conn.execute(
            "SELECT COALESCE(MAX(id), 0) AS mx FROM inbox "
            "WHERE to_cwd = ? AND to_label = ?",
            (str(self_cwd), self_label),
        ).fetchone()
    finally:
        conn.close()
    last_id = last_id_row["mx"] if args.only_new else 0
    if args.since_id is not None:
        last_id = max(last_id, args.since_id)

    header = f"watching inbox for {self_label} at {self_cwd} (Ctrl-C to stop)"
    print(header, file=sys.stderr)
    if not args.only_new and last_id == 0:
        print("showing all history, then new messages as they arrive:",
              file=sys.stderr)

    try:
        while True:
            conn = open_db()
            try:
                rows = conn.execute(
                    """
                    SELECT id, from_label, body, created_at, read_at
                    FROM inbox
                    WHERE to_cwd = ? AND to_label = ? AND id > ?
                    ORDER BY id ASC
                    """,
                    (str(self_cwd), self_label, last_id),
                ).fetchall()
            finally:
                conn.close()
            for r in rows:
                marker = " " if r["read_at"] else "*"  # * = unread
                print(
                    f"[{r['created_at']}] {marker} {r['from_label']} → {self_label}:"
                )
                for line in str(r["body"]).splitlines() or [""]:
                    print(f"  {line}")
                print()
                last_id = r["id"]
            sys.stdout.flush()
            time.sleep(interval)
    except KeyboardInterrupt:
        print("(stopped)", file=sys.stderr)
        return EXIT_OK


def cmd_peer_replay(args: argparse.Namespace) -> int:
    """Emit a self-contained HTML transcript of messages in this cwd.

    Scope: all messages where to_cwd OR from_cwd equals the caller's cwd.
    If --as is given, narrow to conversations involving that label.
    """
    cwd = resolve_cwd(args.cwd)
    conn = open_db()
    try:
        if args.as_label:
            validate_label(args.as_label)
            if args.since:
                rows = conn.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE ((to_cwd = ? AND to_label = ?) OR (from_cwd = ? AND from_label = ?))
                      AND created_at >= ?
                    ORDER BY created_at ASC, id ASC
                    """,
                    (str(cwd), args.as_label, str(cwd), args.as_label, args.since),
                ).fetchall()
            else:
                rows = conn.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE (to_cwd = ? AND to_label = ?) OR (from_cwd = ? AND from_label = ?)
                    ORDER BY created_at ASC, id ASC
                    """,
                    (str(cwd), args.as_label, str(cwd), args.as_label),
                ).fetchall()
        else:
            if args.since:
                rows = conn.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE (to_cwd = ? OR from_cwd = ?) AND created_at >= ?
                    ORDER BY created_at ASC, id ASC
                    """,
                    (str(cwd), str(cwd), args.since),
                ).fetchall()
            else:
                rows = conn.execute(
                    """
                    SELECT id, from_label, to_label, body, created_at, read_at
                    FROM inbox
                    WHERE to_cwd = ? OR from_cwd = ?
                    ORDER BY created_at ASC, id ASC
                    """,
                    (str(cwd), str(cwd)),
                ).fetchall()
    finally:
        conn.close()

    out_path = (
        Path(args.out)
        if args.out
        else cwd / ".agent-collab" / f"replay-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.html"
    )
    html = _render_replay_html(rows, cwd, args.as_label)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(html, encoding="utf-8")
    print(f"replay: {out_path} ({len(rows)} messages)")
    return EXIT_OK


def _color_for(label: str) -> str:
    """Deterministic pastel hue per label."""
    h = int(hashlib.sha256(label.encode("utf-8")).hexdigest()[:8], 16) % 360
    return f"hsl({h}, 55%, 86%)"


def _render_replay_html(rows, cwd: Path, as_label: Optional[str]) -> str:
    e = html_lib.escape
    scope = f"conversations involving {e(as_label)}" if as_label else "all cross-session messages"
    title = f"peer-inbox replay — {scope} in {e(str(cwd))}"
    body_rows = []
    prev_date = None
    for r in rows:
        date = r["created_at"][:10]
        if date != prev_date:
            body_rows.append(f'<div class="day">{e(date)}</div>')
            prev_date = date
        bg = _color_for(str(r["from_label"]))
        read = "read" if r["read_at"] else "unread"
        body_rows.append(
            f'<div class="msg">'
            f'  <div class="meta">'
            f'    <span class="pill" style="background:{bg}">{e(str(r["from_label"]))}</span>'
            f'    <span class="arrow">→</span>'
            f'    <span class="pill" style="background:{_color_for(str(r["to_label"]))}">{e(str(r["to_label"]))}</span>'
            f'    <span class="time">{e(r["created_at"])}</span>'
            f'    <span class="status {read}">{read}</span>'
            f'    <span class="id">#{r["id"]}</span>'
            f'  </div>'
            f'  <pre class="body">{e(str(r["body"]))}</pre>'
            f'</div>'
        )
    if not rows:
        body_rows.append('<div class="empty">no messages in scope</div>')
    return f"""<!doctype html>
<html lang=en>
<meta charset=utf-8>
<title>{e(title)}</title>
<style>
  :root {{
    --bg:#0e1015; --fg:#e6e6e6; --muted:#8a8f98; --accent:#6cd9ff;
    --panel:#181b23; --border:#262a33;
  }}
  * {{ box-sizing:border-box }}
  html,body {{ margin:0; padding:0; background:var(--bg); color:var(--fg);
    font:14px/1.5 -apple-system,BlinkMacSystemFont,Segoe UI,system-ui,sans-serif; }}
  header {{ padding:16px 24px; border-bottom:1px solid var(--border);
    position:sticky; top:0; background:var(--bg); z-index:1; }}
  header h1 {{ font-size:15px; margin:0; color:var(--fg); font-weight:500 }}
  header .sub {{ color:var(--muted); font-size:12px; margin-top:4px }}
  main {{ padding:16px 24px; max-width:880px }}
  .day {{ color:var(--muted); text-align:center; margin:24px 0 8px;
    font-size:11px; letter-spacing:.15em; text-transform:uppercase; }}
  .msg {{ margin:12px 0; padding:12px 14px; background:var(--panel);
    border:1px solid var(--border); border-radius:8px; }}
  .meta {{ display:flex; gap:8px; align-items:center; flex-wrap:wrap;
    font-size:12px; color:var(--muted); margin-bottom:6px; }}
  .pill {{ color:#1a1d24; padding:2px 8px; border-radius:10px;
    font-weight:500; font-size:11px; }}
  .arrow {{ color:var(--muted) }}
  .time {{ margin-left:auto; font-family:SF Mono,Menlo,monospace; }}
  .status.unread {{ color:var(--accent) }}
  .status.read {{ color:var(--muted) }}
  .id {{ font-family:SF Mono,Menlo,monospace; opacity:.5 }}
  .body {{ margin:0; white-space:pre-wrap; word-wrap:break-word;
    color:var(--fg); font:13px/1.55 SF Mono,Menlo,Consolas,monospace; }}
  .empty {{ color:var(--muted); text-align:center; padding:40px 0; }}
</style>
<header>
  <h1>peer-inbox replay</h1>
  <div class=sub>{e(scope)} · {e(str(cwd))} · {len(rows)} messages · generated {e(now_iso())}</div>
</header>
<main>
  {''.join(body_rows)}
</main>
"""


def cmd_peer_list(args: argparse.Namespace) -> int:
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)

    self_pair_key = _get_session_pair_key(str(self_cwd), self_label)

    conn = open_db()
    try:
        if self_pair_key is not None:
            rows = conn.execute(
                "SELECT cwd, label, agent, role, last_seen_at FROM sessions "
                "WHERE pair_key = ? AND NOT (cwd = ? AND label = ?) "
                "ORDER BY last_seen_at DESC",
                (self_pair_key, str(self_cwd), self_label),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT cwd, label, agent, role, last_seen_at FROM sessions "
                "WHERE cwd = ? AND NOT (cwd = ? AND label = ?) "
                "ORDER BY last_seen_at DESC",
                (str(self_cwd), str(self_cwd), self_label),
            ).fetchall()
    finally:
        conn.close()

    results = []
    for r in rows:
        state = activity_state(r["last_seen_at"])
        if state == "stale" and not args.include_stale:
            continue
        results.append(
            {
                "label": r["label"],
                "agent": r["agent"],
                "role": r["role"],
                "activity": state,
                "last_seen_at": r["last_seen_at"],
            }
        )

    if args.json:
        json.dump(results, sys.stdout)
        sys.stdout.write("\n")
    else:
        if not results:
            print("no peers")
            return EXIT_OK
        for r in results:
            print(
                f"{r['label']:<20} {r['agent']:<8} {r['role'] or 'peer':<12} "
                f"{r['activity']:<7} last-seen {r['last_seen_at']}"
            )
    return EXIT_OK


# ---- Arg parsing ----


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="peer-inbox-db")
    sub = p.add_subparsers(dest="cmd", required=True)

    sr = sub.add_parser("session-register")
    sr.add_argument("--cwd")
    sr.add_argument("--label",
                    help="session label (generated if omitted)")
    sr.add_argument("--agent", required=True)
    sr.add_argument("--role")
    sr.add_argument("--session-key", dest="session_key")
    sr.add_argument("--pair-key", dest="pair_key",
                    help="join the pair identified by KEY (share with another session)")
    sr.add_argument("--new-pair", dest="new_pair", action="store_true",
                    help="mint a fresh pair key and print it")
    sr.add_argument("--force", action="store_true")
    sr.set_defaults(func=cmd_session_register)

    hl = sub.add_parser("hook-log-session")
    hl.add_argument("--cwd", required=True)
    hl.add_argument("--session-id", required=True, dest="session_id")
    hl.set_defaults(func=cmd_hook_log_session)

    ad = sub.add_parser("session-adopt")
    ad.add_argument("--cwd")
    ad.add_argument("--label", required=True)
    ad.add_argument("--session-id", required=True, dest="session_id",
                    help="Claude session_id to adopt as the registered session's session_key")
    ad.set_defaults(func=cmd_session_adopt)

    sc = sub.add_parser("session-close")
    sc.add_argument("--cwd")
    sc.add_argument("--label")
    sc.add_argument("--session-key", dest="session_key")
    sc.set_defaults(func=cmd_session_close)

    sl = sub.add_parser("session-list")
    sl.add_argument("--cwd")
    sl.add_argument("--all-cwds", action="store_true")
    sl.add_argument("--include-stale", action="store_true")
    sl.add_argument("--json", action="store_true")
    sl.set_defaults(func=cmd_session_list)

    ps = sub.add_parser("peer-send")
    ps.add_argument("--cwd")
    ps.add_argument("--as", dest="as_label")
    ps.add_argument("--to",
                    help="recipient label (inferred when exactly one peer in scope)")
    ps.add_argument("--to-cwd")
    ps.add_argument(
        "--mention", action="append", default=[],
        help="tag a peer as primary responder; surfaces as meta.mentions in "
             "the channel push. Body @label tokens are auto-detected.",
    )
    ps.add_argument("--message")
    ps.add_argument("--message-file")
    ps.add_argument("--message-stdin", action="store_true")
    ps.set_defaults(func=cmd_peer_send)

    pb = sub.add_parser("peer-broadcast")
    pb.add_argument("--cwd")
    pb.add_argument("--as", dest="as_label")
    pb.add_argument(
        "--to", action="append", default=[],
        help="restrict delivery to a subset (multicast); repeat per label. "
             "omit for room-wide broadcast",
    )
    pb.add_argument(
        "--mention", action="append", default=[],
        help="tag a peer as primary responder; body @label tokens are "
             "auto-detected.",
    )
    pb.add_argument("--message")
    pb.add_argument("--message-file")
    pb.add_argument("--message-stdin", action="store_true")
    pb.set_defaults(func=cmd_peer_broadcast)

    pro = sub.add_parser(
        "peer-round",
        help="(mediator) broadcast a round-tagged message",
    )
    pro.add_argument("--cwd")
    pro.add_argument("--as", dest="as_label")
    pro.add_argument(
        "--round", required=True,
        help="round number (integer >= 1; mediator tracks)",
    )
    pro.add_argument(
        "--label",
        help="optional phase label, e.g. 'topic', 'summary', 'converged'",
    )
    pro.add_argument("--mention", action="append", default=[])
    pro.add_argument("--message")
    pro.add_argument("--message-file")
    pro.add_argument("--message-stdin", action="store_true")
    pro.set_defaults(func=cmd_peer_round)

    pr = sub.add_parser("peer-receive")
    pr.add_argument("--cwd")
    pr.add_argument("--as", dest="as_label")
    pr.add_argument(
        "--format",
        choices=["hook", "hook-json", "plain", "json"],
        default="plain",
    )
    pr.add_argument("--mark-read", action="store_true")
    pr.add_argument("--since")
    pr.set_defaults(func=cmd_peer_receive)

    pl = sub.add_parser("peer-list")
    pl.add_argument("--cwd")
    pl.add_argument("--as", dest="as_label")
    pl.add_argument("--include-stale", action="store_true")
    pl.add_argument("--json", action="store_true")
    pl.set_defaults(func=cmd_peer_list)

    pw = sub.add_parser("peer-watch")
    pw.add_argument("--cwd")
    pw.add_argument("--as", dest="as_label")
    pw.add_argument("--interval", type=float, default=1.0)
    pw.add_argument("--only-new", action="store_true",
                    help="skip history; print only messages arriving after launch")
    pw.add_argument("--since-id", type=int)
    pw.set_defaults(func=cmd_peer_watch)

    pr = sub.add_parser("peer-replay")
    pr.add_argument("--cwd")
    pr.add_argument("--as", dest="as_label",
                    help="narrow to conversations involving this label")
    pr.add_argument("--since", help="ISO-8601 UTC; skip messages older than this")
    pr.add_argument("--out", help="output path (defaults to .agent-collab/replay-<ts>.html)")
    pr.set_defaults(func=cmd_peer_replay)

    prst = sub.add_parser("peer-reset")
    prst.add_argument("--cwd")
    prst.add_argument("--as", dest="as_label")
    prst.add_argument("--to",
                      help="peer label; resets the cwd-only edge room between "
                           "self and <label>")
    prst.add_argument("--pair-key", dest="pair_key",
                      help="resets the whole pair_key-scoped room")
    prst.add_argument("--room-key", dest="room_key",
                      help="reset an arbitrary room by its exact key")
    prst.set_defaults(func=cmd_peer_reset)

    pwb = sub.add_parser("peer-web")
    pwb.add_argument("--cwd")
    pwb.add_argument("--as", dest="as_label",
                     help="narrow to conversations involving this label")
    pwb.add_argument("--pair-key", dest="pair_key",
                     help="scope to a v1.7 pair key — shows cross-cwd conversations "
                          "between all labels registered with that key")
    pwb.add_argument("--port", default="8787",
                     help="HTTP port to bind (default 8787)")
    pwb.set_defaults(func=cmd_peer_web)

    return p


def check_python_version() -> None:
    if sys.version_info[:2] < PYTHON_MIN:
        err(
            f"python {'.'.join(str(p) for p in PYTHON_MIN)}+ required; running "
            f"{'.'.join(str(p) for p in sys.version_info[:3])}",
            EXIT_CONFIG_ERROR,
        )


def main(argv: Optional[list[str]] = None) -> int:
    check_python_version()
    check_sqlite_version()
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
