package main

import (
	"fmt"
	"os"
)

// runPeerSend ports cmd_peer_send. Phase 3 scope: reuse
// SQLiteLocal.Send from Phase 1 for the local write; plus federation
// routing via _peer_send_remote (HTTP POST to a remote peer-web's
// /api/send), outbound queue fallback, turn-cap termination, idempotency
// via --message-id (ULID), --to auto-infer, --json output shape.
func runPeerSend(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"peer-send: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
