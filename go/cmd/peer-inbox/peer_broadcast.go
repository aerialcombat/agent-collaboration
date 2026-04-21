package main

import (
	"fmt"
	"os"
)

// runPeerBroadcast ports cmd_peer_broadcast — fan-out to all peers in
// the room, or to a subset via repeated --to. Phase 3 scope: reuse
// SQLiteLocal.BroadcastLocal from Phase 1.
func runPeerBroadcast(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"peer-broadcast: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
