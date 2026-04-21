package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runSessionClose ports cmd_session_close (scripts/peer-inbox-db.py
// line 1871). Resolves the target session (by explicit --label+--cwd or
// via discovered session_key), emits a room-leave system event when
// the session was pair-key-scoped, DELETEs the sessions row, and
// removes the marker file.
//
// Phase 5.1 deferral (documented in-file):
//   - emit_system_event (channel-push) — Phase 5.1 TODO; leave message
//     is skipped silently, matching the "no channel live" case.
func runSessionClose(args []string) int {
	fs := flag.NewFlagSet("session-close", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag    = fs.String("cwd", "", "session cwd (default: process cwd)")
		label      = fs.String("label", "", "session label (either this or a session key env is required)")
		sessionKey = fs.String("session-key", "", "session key override (defaults to discoverSessionKey env chain)")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwdFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-close: resolve cwd: %v\n", err)
		return 2 // EXIT_CONFIG_ERROR
	}

	sk := *sessionKey
	if sk == "" {
		sk = discoverSessionKey()
	}
	if sk == "" {
		// Fallback: pick the most recent hook-logged session_id in this
		// cwd that's still registered. Same shape as Python
		// cmd_session_close:1879-1895.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, oerr := sqlitestore.Open(ctx)
		if oerr == nil {
			if active, aerr := st.ActiveSessionKeysInCWD(ctx, resolvedCWD); aerr == nil {
				for _, sid := range recentSeenSessions(resolvedCWD, 20) {
					if active[sid] {
						sk = sid
						break
					}
				}
			}
			st.Close()
		}
		cancel()
	}

	var (
		labelArg  = *label
		targetCWD = resolvedCWD
	)
	if labelArg != "" {
		if err := sqlitestore.ValidateLabel(labelArg); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
	} else {
		if sk == "" {
			fmt.Fprintln(os.Stderr,
				"error: pass --label <label> or set a session key env var so we can find the right session to close")
			return 2
		}
		// Walk parents looking for the marker keyed on session_key.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := sqlitestore.Open(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "session-close: open store: %v\n", err)
			return exitInternal
		}
		resolved, err := st.ResolveSelf(ctx, resolvedCWD, sk)
		st.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: no registered session for this session key under %s\n", resolvedCWD)
			return 6
		}
		labelArg = resolved.Label
		targetCWD = resolved.CWD
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-close: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	deleted, err := st.DeleteSession(ctx, targetCWD, labelArg, sk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-close: %v\n", err)
		return exitInternal
	}

	if sk != "" {
		_ = os.Remove(markerPath(targetCWD, sk))
	}

	if deleted {
		fmt.Printf("closed: %s at %s\n", labelArg, targetCWD)
	} else {
		fmt.Printf("note: %s not in DB at %s (marker removed)\n", labelArg, targetCWD)
	}
	return exitOK
}
