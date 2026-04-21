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

// Python EXIT_LABEL_COLLISION — returned when a room already exists.
// Matches scripts/peer-inbox-db.py:EXIT_LABEL_COLLISION (= 1). The
// session-register port's 11 is a pre-existing inconsistency we do
// not carry forward here — parity is gated against Python output
// byte-for-byte including the exit code.
const exitLabelCollision = 1

// runRoomCreate ports cmd_room_create (scripts/peer-inbox-db.py
// line 4507). Mints a fresh peer_rooms row for a pair_key. When
// --pair-key is explicit we validate it and fail-loud on collision;
// when it's omitted we mint an adjective-noun-xxxx slug. Byte-parity
// with Python is required by tests/parity.
func runRoomCreate(args []string) int {
	fs := flag.NewFlagSet("room-create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "",
		"use this pair key instead of minting one (must be unused)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	finalPairKey := *pairKey
	if finalPairKey != "" {
		if err := sqlitestore.ValidatePairKey(finalPairKey); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3 // EXIT_VALIDATION
		}
	} else {
		finalPairKey = sqlitestore.GeneratePairKey()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "room-create: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	host := sqlitestore.SelfHost()
	if err := st.CreateRoom(ctx, finalPairKey, host); err != nil {
		if errors.Is(err, sqlitestore.ErrRoomExists) {
			// Python: err(f"room {pair_key!r} already exists", EXIT_LABEL_COLLISION)
			fmt.Fprintf(os.Stderr, "error: room '%s' already exists\n", finalPairKey)
			return exitLabelCollision
		}
		fmt.Fprintf(os.Stderr, "room-create: %v\n", err)
		return exitInternal
	}

	fmt.Printf("created: %s:%s (empty room)\n", host, finalPairKey)
	fmt.Printf("  join: agent-collab session register --pair-key %s "+
		"--agent <claude|codex|gemini|pi>\n", finalPairKey)
	return exitOK
}
