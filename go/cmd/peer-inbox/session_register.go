package main

import (
	"fmt"
	"os"
)

// runSessionRegister ports cmd_session_register from
// scripts/peer-inbox-db.py. Phase 3 scope: owner row + session_key +
// pair_key / --new-pair / --home-host federation seed + --receive-mode +
// auth-token mint (v3.3 Item 7). See plans/v3.4-python-removal-scoping.md
// §5 Phase 3 for the full contract.
//
// Until ported, callers route to the Python CLI via the bash wrapper.
func runSessionRegister(args []string) int {
	_ = args
	fmt.Fprintln(os.Stderr,
		"session-register: not yet ported to Go (v3.4 Phase 3); "+
			"use the Python CLI (AGENT_COLLAB_IMPL=python) until this verb lands.")
	return exitInternal
}
