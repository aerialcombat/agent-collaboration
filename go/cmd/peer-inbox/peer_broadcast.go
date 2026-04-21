package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"agent-collaboration/go/pkg/store"
	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// runPeerBroadcast ports cmd_peer_broadcast from scripts/peer-inbox-db.py.
// Fan-out send to every live peer in the sender's scope (pair_key or
// cwd), or to a named subset via repeated --to (multicast). Reuses the
// Phase-1 SQLiteLocal.BroadcastLocal for the transactional write; the
// CLI layer owns the body / cohort / mention validation so error text
// + exit codes stay byte-identical to Python.
//
// Exit codes (mirror Python):
//
//	0   EXIT_OK
//	3   EXIT_VALIDATION      — bad body / multicast-to-self / room-cap
//	6   EXIT_NOT_FOUND       — no live peers / missing cohort / mention
//	64  EXIT_USAGE           — flag parse errors
//
// stdout (success):
//
//	broadcast to N peer(s): alpha (no-channel), beta (pushed)
//	multicast to N peer(s): ...
//	<line> [[end]]→room terminated                 (if body contains [[end]])
//	<line> @mentions[label1,label2]                (if mentions resolved)
//
// stderr on error: `error: <message>\n` — matches Python's err() prefix.
func runPeerBroadcast(args []string) int {
	fs := flag.NewFlagSet("peer-broadcast", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwdFlag        = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl          = fs.String("as", "", "session label (default: resolve via marker / env)")
		message        = fs.String("message", "", "message body (one of --message | --message-file | --message-stdin is required)")
		messageFile    = fs.String("message-file", "", "read body from file")
		messageStdin   = fs.Bool("message-stdin", false, "read body from stdin")
	)
	var toList stringsFlag
	var mentionList stringsFlag
	fs.Var(&toList, "to", "restrict delivery to a subset (multicast); repeat per label")
	fs.Var(&mentionList, "mention", "tag a peer as primary responder; body @label tokens are auto-detected")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// --- Body resolution (matches Python's arg precedence) -------------
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
	// Python accepts exactly one; if none, err with the "required" msg.
	// Multiple are disallowed only implicitly — Python's if-elif picks
	// the first true branch (stdin > file > message). For byte-parity
	// with Python we take the same precedence silently. Callers passing
	// more than one still see a consistent body.
	if sourcesSet == 0 {
		fmt.Fprintln(os.Stderr,
			"error: one of --message, --message-file, --message-stdin required")
		return 3 // EXIT_VALIDATION
	}

	var body string
	switch {
	case *messageStdin:
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read stdin: %v\n", err)
			return exitInternal
		}
		body = string(raw)
	case *messageFile != "":
		raw, err := os.ReadFile(*messageFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read %s: %v\n", *messageFile, err)
			return exitInternal
		}
		body = string(raw)
	default:
		body = *message
	}

	if len(body) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty message rejected")
		return 3
	}
	bodyCap := maxBodyBytesCap()
	if len(body) > bodyCap {
		fmt.Fprintf(os.Stderr, "error: message too large: %d bytes > %d cap\n", len(body), bodyCap)
		return 3
	}

	// --- Resolve self --------------------------------------------------
	resolvedCWD, err := resolveCWD(*cwdFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-broadcast: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-broadcast: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	if self.Label == "" {
		resolved, err := st.ResolveSelf(ctx, resolvedCWD, discoverSessionKey())
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-broadcast: resolve self: %v (pass --as <label> or set a session key env)\n", err)
			return exitInternal
		}
		self = resolved
	} else {
		if err := sqlitestore.ValidateLabel(self.Label); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
	}

	pairKey, err := st.GetSessionPairKey(ctx, self.CWD, self.Label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-broadcast: %v\n", err)
		return exitInternal
	}

	// --- Cohort validation (matches Python's set-based filter) ---------
	cohort := map[string]bool{}
	for _, t := range toList {
		if t == "" {
			continue
		}
		if err := sqlitestore.ValidateLabel(t); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
		cohort[t] = true
	}
	if cohort[self.Label] {
		// Python: f"cannot multicast to self: {self_label!r}" — the !r
		// conversion quotes the value with single-quotes.
		fmt.Fprintf(os.Stderr, "error: cannot multicast to self: '%s'\n", self.Label)
		return 3
	}

	// --- Live peer enumeration -----------------------------------------
	live, err := st.LivePeersInScope(ctx, self.CWD, self.Label, pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-broadcast: %v\n", err)
		return exitInternal
	}

	if len(cohort) > 0 {
		present := map[string]bool{}
		for _, p := range live {
			present[p.Label] = true
		}
		var missing []string
		for lbl := range cohort {
			if !present[lbl] {
				missing = append(missing, lbl)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			fmt.Fprintf(os.Stderr,
				"error: unknown or stale peer(s) in multicast: %s\n",
				pyListRepr(missing))
			return 6 // EXIT_NOT_FOUND
		}
		// Filter live down to cohort.
		filtered := live[:0]
		for _, p := range live {
			if cohort[p.Label] {
				filtered = append(filtered, p)
			}
		}
		live = filtered
	}

	if len(live) == 0 {
		scope := fmt.Sprintf("cwd=%s", self.CWD)
		if pairKey != "" {
			scope = fmt.Sprintf("pair_key=%s", pairKey)
		}
		fmt.Fprintf(os.Stderr, "error: no live peers in %s\n", scope)
		return 6
	}

	// --- Mention validation (explicit --mention only) ------------------
	members, err := st.RoomMembers(ctx, self.CWD, self.Label, pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-broadcast: %v\n", err)
		return exitInternal
	}
	for _, m := range mentionList {
		if m == "" || m == self.Label {
			continue
		}
		if err := sqlitestore.ValidateLabel(m); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		}
		if !members[m] {
			fmt.Fprintf(os.Stderr, "error: mention target not in room: '%s'\n", m)
			return 6
		}
	}

	// --- Fan-out via Phase-1 BroadcastLocal ----------------------------
	// BroadcastLocal accepts a comma-joined cohort via ToLabel for the
	// multicast subset filter; empty string = full room. It also re-
	// filters stale and re-computes recipients inside the write tx, so
	// the pre-flight window above is informational only (matches Python
	// two-phase shape).
	var cohortArg string
	if len(cohort) > 0 {
		names := make([]string, 0, len(cohort))
		for n := range cohort {
			names = append(names, n)
		}
		sort.Strings(names)
		cohortArg = strings.Join(names, ",")
	}

	results, err := st.BroadcastLocal(ctx, sqlitestore.SendParams{
		SenderCWD:   self.CWD,
		SenderLabel: self.Label,
		ToLabel:     cohortArg,
		Body:        body,
		PairKey:     pairKey,
		Mentions:    []string(mentionList),
		Now:         time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrRoomTerminated):
			// Python includes terminated_at/by + a reset hint. Our Go
			// error wraps the room_key only; surface as a compatible
			// shape (EXIT_VALIDATION=3) with the sentinel text. Parity
			// fixtures that exercise this path should seed a fresh room.
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		case errors.Is(err, sqlitestore.ErrTurnCapExceeded):
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		case errors.Is(err, sqlitestore.ErrBodyTooLarge), errors.Is(err, sqlitestore.ErrEmptyBody):
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3
		default:
			fmt.Fprintf(os.Stderr, "peer-broadcast: %v\n", err)
			return exitInternal
		}
	}

	// --- Compose Python-identical stdout line --------------------------
	// Python joins statuses in the natural iteration order of `live`,
	// which in our seed-then-query path matches sessions.label (SQL
	// without ORDER BY yields rowid order == insert order, and fixtures
	// seed alphabetically). BroadcastLocal's results are sort-by-label;
	// we keep that for determinism on the Go side and rely on fixture
	// authors to seed Python matching insert-order == alphabetical.
	statuses := make([]string, 0, len(results))
	for _, r := range results {
		statuses = append(statuses, fmt.Sprintf("%s (%s)", r.To, r.PushStatus))
	}
	kind := "broadcast"
	if len(cohort) > 0 {
		kind = "multicast"
	}
	termNote := ""
	if len(results) > 0 && results[0].Terminates {
		termNote = " [[end]]\u2192room terminated"
	}
	mentionNote := ""
	if len(results) > 0 && len(results[0].Mentions) > 0 {
		mentionNote = " @mentions[" + strings.Join(results[0].Mentions, ",") + "]"
	}
	fmt.Printf("%s to %d peer(s): %s%s%s\n",
		kind, len(results), strings.Join(statuses, ", "), termNote, mentionNote)
	return exitOK
}

// stringsFlag implements flag.Value for repeated string-append flags.
// Mirrors argparse(action="append"). Lives here (not main.go) to keep
// the broadcast-only reuse local per the DO-NOT-EDIT main.go hard rule.
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

// maxBodyBytesCap mirrors Python's MAX_BODY_BYTES env-overridable cap.
// Kept out of the sqlite package because it's a CLI-layer concern — the
// Go store checks the same cap inside BroadcastLocal via validateBody.
func maxBodyBytesCap() int {
	if v := os.Getenv("AGENT_COLLAB_MAX_BODY_BYTES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 8 * 1024
}

// pyListRepr renders a []string the way Python's str(list) does:
// ``['a', 'b', 'c']`` — single-quoted elements, comma+space separator.
// We use this to match the byte-exact shape of Python err() messages
// that include ``{sorted(missing)}`` or ``({sorted(live)})``.
func pyListRepr(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = "'" + x + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
