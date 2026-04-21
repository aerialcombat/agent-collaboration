package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runPeerReset ports cmd_peer_reset (scripts/peer-inbox-db.py
// line 4612). Clears room state by DELETE'ing the peer_rooms row —
// despite the verb name, this is a hard delete in the Python impl
// (reversible only by re-registering or re-creating). Exactly one
// of --to / --pair-key / --room-key must be given. Byte-parity with
// Python is required by tests/parity.
func runPeerReset(args []string) int {
	fs := flag.NewFlagSet("peer-reset", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl   = fs.String("as", "", "")
		toLbl   = fs.String("to", "",
			"peer label; resets the cwd-only edge room between self and <label>")
		pairKey = fs.String("pair-key", "",
			"resets the whole pair_key-scoped room")
		roomKey = fs.String("room-key", "",
			"reset an arbitrary room by its exact key")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	provided := 0
	if *toLbl != "" {
		provided++
	}
	if *pairKey != "" {
		provided++
	}
	if *roomKey != "" {
		provided++
	}
	if provided != 1 {
		// Python: err("exactly one of --to, --pair-key, --room-key is required", EXIT_VALIDATION)
		fmt.Fprintln(os.Stderr,
			"error: exactly one of --to, --pair-key, --room-key is required")
		return 3 // EXIT_VALIDATION
	}

	var (
		resolvedKey string
		labelForMsg string
	)
	switch {
	case *roomKey != "":
		resolvedKey = *roomKey
		labelForMsg = *roomKey
	case *pairKey != "":
		if err := sqlitestore.ValidatePairKey(*pairKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
		resolvedKey = "pk:" + *pairKey
		labelForMsg = "pair " + *pairKey
	default: // --to
		if err := sqlitestore.ValidateLabel(*toLbl); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
		resolvedCWD, err := resolveCWD(*cwdFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-reset: resolve cwd: %v\n", err)
			return exitInternal
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		st, err := sqlitestore.Open(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-reset: open store: %v\n", err)
			return exitInternal
		}
		selfCWD := resolvedCWD
		selfLabel := *asLbl
		if selfLabel == "" {
			self, err := st.ResolveSelf(ctx, resolvedCWD, discoverSessionKey())
			if err != nil {
				st.Close()
				fmt.Fprintf(os.Stderr,
					"peer-reset: resolve self: %v (pass --as <label> or set a session key env)\n",
					err)
				return exitInternal
			}
			selfCWD = self.CWD
			selfLabel = self.Label
		}
		st.Close()
		resolvedKey = cwdEdgeRoomKey(selfCWD, selfLabel, *toLbl)
		labelForMsg = fmt.Sprintf("(%s, %s) at %s", selfLabel, *toLbl, selfCWD)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-reset: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	deleted, err := st.DeleteRoom(ctx, resolvedKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-reset: %v\n", err)
		return exitInternal
	}
	if deleted {
		fmt.Printf("reset: %s\n", labelForMsg)
	} else {
		fmt.Printf("note: %s had no room state\n", labelForMsg)
	}
	return exitOK
}

// cwdEdgeRoomKey mirrors Python's _room_key_for(cwd, a, b, None) —
// canonicalises the pair (sorted) and emits "cwd:<cwd>#<a>+<b>".
func cwdEdgeRoomKey(cwd, self, peer string) string {
	a, b := self, peer
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("cwd:%s#%s+%s", cwd, a, b)
}
