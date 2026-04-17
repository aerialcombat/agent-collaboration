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
import json
import os
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional

DEFAULT_DB = Path.home() / ".agent-collab" / "sessions.db"
DEFAULT_CLAUDE_SESSION_LOG_DIR = Path.home() / ".agent-collab" / "claude-sessions-seen"
SESSIONS_DIR_REL = ".agent-collab/sessions"
LABEL_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")
VALID_AGENTS = {"claude", "codex", "gemini"}
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
    return conn


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


# ---- Subcommands ----


def cmd_session_register(args: argparse.Namespace) -> int:
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

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            "SELECT agent, role, last_seen_at, session_key FROM sessions "
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
        conn.execute(
            """
            INSERT INTO sessions (cwd, label, agent, role, session_key, started_at, last_seen_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(cwd, label) DO UPDATE SET
              agent = excluded.agent,
              role = excluded.role,
              session_key = excluded.session_key,
              started_at = excluded.started_at,
              last_seen_at = excluded.last_seen_at
            """,
            (str(cwd), args.label, args.agent, args.role, session_key, now, now),
        )
        conn.execute("COMMIT")
    finally:
        conn.close()

    write_marker(cwd, args.label, session_key)
    print(
        f"registered: {args.label} ({args.agent}, {args.role or 'peer'}) at {cwd} "
        f"[session_key={session_key_hash(session_key)}]"
    )
    return EXIT_OK


def cmd_session_close(args: argparse.Namespace) -> int:
    cwd_raw = resolve_cwd(args.cwd)
    session_key = args.session_key or discover_session_key()

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
    validate_label(args.to)

    to_cwd = resolve_cwd(args.to_cwd) if args.to_cwd else self_cwd

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

    conn = open_db()
    try:
        conn.execute("BEGIN IMMEDIATE")
        target = conn.execute(
            "SELECT last_seen_at FROM sessions WHERE cwd = ? AND label = ?",
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
        now = now_iso()
        conn.execute(
            """
            INSERT INTO inbox
              (to_cwd, to_label, from_cwd, from_label, body, created_at)
            VALUES (?, ?, ?, ?, ?, ?)
            """,
            (str(to_cwd), args.to, str(self_cwd), self_label, body, now),
        )
        bump_last_seen(conn, str(self_cwd), self_label)
        conn.execute("COMMIT")
    finally:
        conn.close()

    print(f"sent to {args.to}")
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


def cmd_peer_list(args: argparse.Namespace) -> int:
    cwd = resolve_cwd(args.cwd)
    self_cwd, self_label = resolve_self(args.as_label, cwd)

    conn = open_db()
    try:
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
    sr.add_argument("--label", required=True)
    sr.add_argument("--agent", required=True)
    sr.add_argument("--role")
    sr.add_argument("--session-key", dest="session_key")
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
    ps.add_argument("--to", required=True)
    ps.add_argument("--to-cwd")
    ps.add_argument("--message")
    ps.add_argument("--message-file")
    ps.add_argument("--message-stdin", action="store_true")
    ps.set_defaults(func=cmd_peer_send)

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
