package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-collaboration/go/pkg/store"
	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runPeerRound ports cmd_peer_round from scripts/peer-inbox-db.py.
// Thin wrapper around peer-broadcast that (a) warns when the caller is
// not registered with role=mediator, (b) validates --round is an
// integer >= 1, (c) prefixes the body with "[Round N] " (or
// "[Round N:<label>] " when --label is given), then re-dispatches
// through runPeerBroadcast so the fan-out path stays single-sourced.
//
// State (current round number, topic, mediator identity) lives entirely
// in the message bodies — nothing about "which round are we in" is
// persisted, matching Python's stateless design.
//
// Exit codes:
//
//	0   EXIT_OK                   (successful broadcast)
//	3   EXIT_VALIDATION           (bad --round / --label / missing body)
//	64  EXIT_USAGE                (flag parse failure)
//
// Warning-but-continue case (role != mediator) writes to stderr with
// the "warning:" prefix but still dispatches — matches Python's intent
// to keep the verb useful even when a mediator hasn't re-registered.
var roundLabelRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func runPeerRound(args []string) int {
	fs := flag.NewFlagSet("peer-round", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag      = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl        = fs.String("as", "", "session label (default: resolve via marker / env)")
		round        = fs.String("round", "", "round number (integer >= 1)")
		roundLabel   = fs.String("label", "", "optional phase label, e.g. 'topic', 'summary', 'converged'")
		message      = fs.String("message", "", "message body")
		messageFile  = fs.String("message-file", "", "read body from file")
		messageStdin = fs.Bool("message-stdin", false, "read body from stdin")
	)
	var mentionList stringsFlag
	fs.Var(&mentionList, "mention", "tag a peer as primary responder")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// --- Validate --round (Python argparse marks it required; we keep
	// explicit handling so the message matches Python's err() path). ---
	if *round == "" {
		fmt.Fprintln(os.Stderr,
			"error: --round is required (mediator tracks the round number)")
		return 3
	}
	roundN, err := strconv.Atoi(*round)
	if err != nil {
		// Python's f"--round must be an integer, got {args.round!r}"
		// emits the raw value single-quoted (the !r conversion).
		fmt.Fprintf(os.Stderr, "error: --round must be an integer, got '%s'\n", *round)
		return 3
	}
	if roundN < 1 {
		fmt.Fprintln(os.Stderr, "error: --round must be >= 1")
		return 3
	}

	// --- Validate optional --label ------------------------------------
	if *roundLabel != "" && !roundLabelRE.MatchString(*roundLabel) {
		fmt.Fprintln(os.Stderr, "error: --label must be 1-32 chars of [a-z0-9_-]")
		return 3
	}

	// --- Read raw body (Python precedence: stdin > file > message) ---
	sourcesSet := 0
	if *messageStdin {
		sourcesSet++
	}
	if *messageFile != "" {
		sourcesSet++
	}
	if *message != "" {
		sourcesSet++
	}
	if sourcesSet == 0 {
		fmt.Fprintln(os.Stderr,
			"error: one of --message, --message-file, --message-stdin required")
		return 3
	}
	var raw string
	switch {
	case *messageStdin:
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read stdin: %v\n", err)
			return exitInternal
		}
		raw = string(buf)
	case *messageFile != "":
		buf, err := os.ReadFile(*messageFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read %s: %v\n", *messageFile, err)
			return exitInternal
		}
		raw = string(buf)
	default:
		raw = *message
	}

	// --- Role warning (non-fatal) -------------------------------------
	// Python resolves self via resolve_self(args.as_label, cwd), reads
	// the role, prints a warning when it isn't 'mediator' (case-
	// insensitive). We mirror that exactly, including the warning text.
	resolvedCWD, err := resolveCWD(*cwdFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-round: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-round: open store: %v\n", err)
		return exitInternal
	}

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	if self.Label == "" {
		resolved, err := st.ResolveSelf(ctx, resolvedCWD, discoverSessionKey())
		if err != nil {
			st.Close()
			fmt.Fprintf(os.Stderr, "peer-round: resolve self: %v (pass --as <label> or set a session key env)\n", err)
			return exitInternal
		}
		self = resolved
	}

	role, err := st.GetSessionRole(ctx, self.CWD, self.Label)
	st.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-round: %v\n", err)
		return exitInternal
	}
	if !strings.EqualFold(role, "mediator") {
		// Python: print(f"warning: {self_label} is registered with
		// role={role!r}, not 'mediator'. Broadcasting anyway — but
		// consider re-registering with --role mediator so participants
		// see meta.has_mediator=1.", file=sys.stderr)
		//
		// Python's f"{role!r}" renders None as ``None`` (no quotes) and
		// any string as ``'value'``. Empty-string role → ``''``.
		fmt.Fprintf(os.Stderr,
			"warning: %s is registered with role=%s, not 'mediator'. "+
				"Broadcasting anyway \u2014 but consider re-registering with --role "+
				"mediator so participants see meta.has_mediator=1.\n",
			self.Label, pyRoleRepr(role))
	}

	// --- Compose prefixed body + re-enter broadcast flow --------------
	prefix := fmt.Sprintf("[Round %d] ", roundN)
	if *roundLabel != "" {
		prefix = fmt.Sprintf("[Round %d:%s] ", roundN, *roundLabel)
	}
	finalBody := prefix + raw

	// Translate state to a flat arg list so we go through the same
	// flag.Parse path runPeerBroadcast uses. Repeated --mention is
	// preserved; --to is intentionally empty (room-wide only, matching
	// Python's fixed `to=[]` override at line 4720).
	fwd := []string{
		"--cwd", resolvedCWD,
		"--as", self.Label,
		"--message", finalBody,
	}
	for _, m := range mentionList {
		fwd = append(fwd, "--mention", m)
	}
	return runPeerBroadcast(fwd)
}

// pyRoleRepr mirrors Python's f"{role!r}" for the role warning.
// None → "None"; string → "'value'". Our Go GetSessionRole returns ""
// for both no-row and NULL-column cases; Python's code reads
// `(row["role"] if row else None) or ""` which also collapses to "",
// then formats as `''` — so we render empty as `''` too.
func pyRoleRepr(r string) string {
	return "'" + r + "'"
}
