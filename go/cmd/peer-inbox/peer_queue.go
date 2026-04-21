package main

import (
	"fmt"
	"os"
)

// runPeerQueue ports cmd_peer_queue — inspect + flush the
// pending_outbound federation queue (v3.3 Item 5). Phase 3 scope:
// --show prints the queue state; --flush retries to the remote
// peer-web and purges successes.
func runPeerQueue(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"peer-queue: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
