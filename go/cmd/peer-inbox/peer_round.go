package main

import (
	"fmt"
	"os"
)

// runPeerRound ports cmd_peer_round — mediator-scoped broadcast that
// tags the message with a round identifier. Phase 3 scope: thin layer
// over BroadcastLocal with round-tag metadata.
func runPeerRound(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"peer-round: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
