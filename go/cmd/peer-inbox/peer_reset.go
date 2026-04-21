package main

import (
	"fmt"
	"os"
)

// runPeerReset ports cmd_peer_reset — terminate a peer room
// (soft-archive via terminated_at + terminated_by; reversible by
// TerminateRoom's INSERT...ON CONFLICT). Phase 3 scope.
func runPeerReset(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"peer-reset: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
