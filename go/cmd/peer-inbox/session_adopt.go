package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runSessionAdopt ports cmd_session_adopt (scripts/peer-inbox-db.py
// line 3215). Re-keys an existing (cwd, label) registration to a new
// Claude session_id, renames the marker file, and prints a one-line
// confirmation. Used to reconcile rows that registered before the hook
// started logging session_ids.
func runSessionAdopt(args []string) int {
	fs := flag.NewFlagSet("session-adopt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag   = fs.String("cwd", "", "session cwd (default: process cwd)")
		label     = fs.String("label", "", "session label (required)")
		sessionID = fs.String("session-id", "", "Claude session_id to adopt as the session_key (required)")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *label == "" {
		fmt.Fprintln(os.Stderr, "session-adopt: --label is required")
		return exitUsage
	}
	if err := sqlitestore.ValidateLabel(*label); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 3 // EXIT_VALIDATION
	}
	resolvedCWD, err := resolveCWD(*cwdFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-adopt: resolve cwd: %v\n", err)
		return 2 // EXIT_CONFIG_ERROR
	}
	newKey := strings.TrimSpace(*sessionID)
	if newKey == "" {
		fmt.Fprintln(os.Stderr, "error: --session-id is empty")
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-adopt: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	oldKey, rowExists, err := st.AdoptSessionKey(ctx, resolvedCWD, *label, newKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-adopt: %v\n", err)
		return exitInternal
	}
	if !rowExists {
		fmt.Fprintf(os.Stderr, "error: no registered session for label '%s' in %s\n", *label, resolvedCWD)
		return 6 // EXIT_NOT_FOUND
	}

	// Marker rename: delete old (best-effort — may not exist), write new.
	if oldKey != "" {
		_ = os.Remove(markerPath(resolvedCWD, oldKey))
	}
	writeMarker(resolvedCWD, *label, newKey)

	fmt.Printf("adopted: %s at %s [session_key=%s]\n",
		*label, resolvedCWD, sessionKeyHash(newKey))
	return exitOK
}

// markerPath mirrors Python's marker_path_for: sha16(session_key).json
// under <cwd>/.agent-collab/sessions/. Private to the adopt + close
// verbs; session-register has its own identical construction inlined.
func markerPath(cwd, sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return filepath.Join(cwd, ".agent-collab", "sessions",
		hex.EncodeToString(sum[:])[:16]+".json")
}
