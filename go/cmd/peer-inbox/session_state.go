package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runSessionState implements `peer-inbox session-state <active|idle>`.
// v3.8: lets hooks (UserPromptSubmit → active, Stop → idle) or any
// other signal flip the session's busy/waiting indicator. Identity
// resolution follows the same pattern as session-close: explicit
// --cwd + --label wins, else fall back to session-key env chain
// (CLAUDE_SESSION_ID / AGENT_COLLAB_SESSION_KEY / etc.).
//
// Exit codes match the peer-inbox convention:
//
//	0   state applied (or no-op — already that state)
//	6   no session matched the (cwd, label) / session-key
//	64  usage error
//	1   internal
//
// Fail-open: if the session-key chain resolves to nothing (fresh
// prompt before session-register), we exit 0 with no action. The
// hook's job is to best-effort surface activity; it must never block
// the user's turn.
func runSessionState(args []string) int {
	// The state verb must come first (subcommand shape mirrors the
	// other verbs' flag-only usage). Peel it off before calling
	// flag.Parse — Go's `flag` stops at the first non-flag argument,
	// which would otherwise swallow "active" and reject every
	// subsequent --cwd / --label. Allow it in the trailing position
	// too (tests + humans both prefer both orders).
	var state string
	var rest []string
	for _, a := range args {
		if state == "" && (a == "active" || a == "idle") {
			state = a
			continue
		}
		rest = append(rest, a)
	}
	if state == "" {
		fmt.Fprintln(os.Stderr, "usage: peer-inbox session-state <active|idle> [--cwd DIR] [--label LBL] [--session-key K] [--quiet]")
		return exitUsage
	}

	fs := flag.NewFlagSet("session-state", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag    = fs.String("cwd", "", "session cwd (default: process cwd)")
		label      = fs.String("label", "", "session label; when set with --cwd bypasses session-key lookup")
		sessionKey = fs.String("session-key", "", "session-key override (else uses env chain)")
		quiet      = fs.Bool("quiet", false, "suppress stdout on success; exit code is authoritative")
	)
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "session-state: unexpected positional args: %v\n", fs.Args())
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		// Fail-open: DB missing / schema older than required. State
		// monitoring isn't worth blocking the caller for.
		if !*quiet {
			fmt.Fprintf(os.Stderr, "session-state: open store: %v (fail-open)\n", err)
		}
		return exitOK
	}
	defer st.Close()

	now := time.Now().UTC()

	// Path 1: explicit (cwd, label). Direct write, precise target.
	if *label != "" {
		resolved, rerr := resolveCWD(*cwdFlag)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "session-state: resolve cwd: %v\n", rerr)
			return 2
		}
		serr := st.SetSessionState(ctx, resolved, *label, state, now)
		if errors.Is(serr, sqlitestore.ErrSessionNotFound) {
			if !*quiet {
				fmt.Fprintf(os.Stderr, "session-state: no session for %s / %s\n", resolved, *label)
			}
			return exitNotFound
		}
		if serr != nil {
			fmt.Fprintf(os.Stderr, "session-state: %v\n", serr)
			return exitInternal
		}
		if !*quiet {
			fmt.Printf("session-state: %s (%s / %s)\n", state, resolved, *label)
		}
		return exitOK
	}

	// Path 2: session-key. Match the sessions row by its session_key;
	// covers the common hook case where we have CLAUDE_SESSION_ID but
	// no resolved cwd/label. SetSessionStateByKey is a single UPDATE
	// with a guard on state-differs; no-ops when the key has no row.
	sk := *sessionKey
	if sk == "" {
		sk = discoverSessionKey()
	}
	if sk == "" {
		// No identity signal at all — fail-open silently. Hook on a
		// fresh prompt before any register happened.
		return exitOK
	}
	if serr := st.SetSessionStateByKey(ctx, sk, state, now); serr != nil {
		fmt.Fprintf(os.Stderr, "session-state: %v\n", serr)
		return exitInternal
	}
	if !*quiet {
		fmt.Printf("session-state: %s (session-key=%s)\n", state, sk)
	}
	return exitOK
}
