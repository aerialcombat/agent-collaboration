package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runHookLogSession ports cmd_hook_log_session (scripts/peer-inbox-db.py
// line 3205). Appends "<iso-ts>\t<session_id>\n" to a per-cwd log file
// under ~/.agent-collab/claude-sessions-seen/<sha16(cwd)>.log so the
// Claude UserPromptSubmit hook can surface the session_id to later
// session-register / session-close invocations. Silent no-op on empty
// --session-id.
func runHookLogSession(args []string) int {
	fs := flag.NewFlagSet("hook-log-session", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag   = fs.String("cwd", "", "session cwd (required per Python argparser)")
		sessionID = fs.String("session-id", "", "Claude session id to log (required)")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cwdFlag == "" {
		fmt.Fprintln(os.Stderr, "hook-log-session: --cwd is required")
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwdFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook-log-session: resolve cwd: %v\n", err)
		return 2 // EXIT_CONFIG_ERROR
	}

	sid := strings.TrimSpace(*sessionID)
	if sid == "" {
		return exitOK
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// Non-fatal: hook path fail-opens. Python silently fails here
		// because mkdir(..., exist_ok=True) swallows errors; match that.
		return exitOK
	}
	cwdHash := sha256.Sum256([]byte(resolvedCWD))
	logPath := filepath.Join(home, ".agent-collab", "claude-sessions-seen",
		hex.EncodeToString(cwdHash[:])[:16]+".log")

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return exitOK
	}
	_ = os.Chmod(filepath.Dir(logPath), 0o700)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return exitOK
	}
	defer f.Close()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, _ = fmt.Fprintf(f, "%s\t%s\n", ts, sid)
	_ = os.Chmod(logPath, 0o600)
	return exitOK
}
