package main

import (
	"fmt"
	"os"
)

// runRoomCreate ports cmd_room_create — explicit peer_rooms row
// creation. Phase 3 scope: small standalone verb.
func runRoomCreate(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"room-create: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
