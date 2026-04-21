package main

import (
	"context"
	"errors"
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

// runPeerSend ports cmd_peer_send from scripts/peer-inbox-db.py. v3.4
// Phase 3 scope: local-path unicast send, reusing SQLiteLocal.Send (Phase
// 1) for the atomic inbox write + idempotency dedup + turn cap + channel
// push. Flag surface matches the Python argparser in full — --cwd, --as,
// --to, --to-cwd, --mention (repeatable), --message/--message-file/
// --message-stdin, --message-id, --json.
//
// Federation short-circuit (Python checks peer_rooms.home_host against
// self_host()) is detected here and surfaces as an internal-error
// fallback: v3.3 already ships /api/send in Go, but the CLI federation
// client that POSTS there lives in Python. Until the client is ported,
// remote-routed rooms get a clear error rather than silently writing
// locally. Parity fixtures exercise only the local path (home_host NULL
// or == self_host), which is where day-to-day peer-send traffic lands.
//
// Validation-error stderr shape matches Python's err() helper: the
// literal "error: <msg>\n" written to stderr before exit. Exit codes
// mirror the Python EXIT_ constants (see err() + top-of-file constants in
// peer-inbox-db.py).
func runPeerSend(args []string) int {
	fs := flag.NewFlagSet("peer-send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd          = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl        = fs.String("as", "", "session label (default: resolve via marker / env)")
		to           = fs.String("to", "", "recipient label (inferred when exactly one peer in scope)")
		toCWD        = fs.String("to-cwd", "", "override recipient cwd (legacy cross-cwd sends)")
		message      = fs.String("message", "", "message body (mutually exclusive with --message-file / --message-stdin)")
		messageFile  = fs.String("message-file", "", "read body from path")
		messageStdin = fs.Bool("message-stdin", false, "read body from stdin")
		messageID    = fs.String("message-id", "", "client-generated idempotency id (ULID); auto-generated when empty")
		jsonOut      = fs.Bool("json", false, "emit single-line JSON object on stdout instead of the human summary")
	)
	var mentions stringSlice
	fs.Var(&mentions, "mention", "tag a peer as primary responder; repeatable. Body @label tokens are auto-detected.")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot resolve cwd: %v\n", err)
		return exitConfigError
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	// Resolve self (cwd, label) — mirrors Python resolve_self.
	self, err := resolveSelfForSend(ctx, st, resolvedCWD, *asLbl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitConfigError
	}

	// Load pair_key for the sender; needed for room-key computation,
	// federation routing, and --to inference.
	selfPairKey, err := st.GetSessionPairKey(ctx, self.CWD, self.Label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitInternal
	}

	// --to is optional — infer when exactly one live peer is in scope.
	toLabel := *to
	if toLabel == "" {
		inferred, err := st.InferPeerLabel(ctx, self.CWD, self.Label, selfPairKey, time.Now().UTC())
		if err != nil {
			var missing *sqlitestore.ErrPeerLabelMissing
			var ambiguous *sqlitestore.ErrPeerLabelAmbiguous
			switch {
			case errors.As(err, &missing):
				fmt.Fprintf(os.Stderr,
					"error: no live peer in %s; pass --to <label> or register another session first\n",
					missing.Scope)
				return exitNotFound
			case errors.As(err, &ambiguous):
				fmt.Fprintf(os.Stderr,
					"error: cannot infer --to: %d live peers in %s (%v); pass --to <label> to disambiguate\n",
					len(ambiguous.Candidates), ambiguous.Scope, ambiguous.Candidates)
				return exitValidation
			default:
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return exitInternal
			}
		}
		toLabel = inferred
	}
	if err := validateLabel(toLabel); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitValidation
	}

	// Read body per the Python precedence: --message-stdin > --message-file
	// > --message. All three missing is an EXIT_VALIDATION error, matching
	// the Python err() call.
	body, err := readBody(*messageStdin, *messageFile, *message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitValidation
	}

	// Body size + emptiness checks — SQLiteLocal.Send already enforces
	// these, but Python surfaces them with exact phrasings. Replicate here
	// so stderr byte-parity holds.
	bodyBytes := []byte(body)
	maxBytes := maxBodyBytesFromEnv()
	if len(bodyBytes) > maxBytes {
		fmt.Fprintf(os.Stderr, "error: message too large: %d bytes > %d cap\n", len(bodyBytes), maxBytes)
		return exitValidation
	}
	if len(bodyBytes) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty message rejected")
		return exitValidation
	}

	// v3.3 federation short-circuit: if the room's home_host is a remote,
	// POST to that remote's /api/send instead of writing locally.
	// Mirrors Python cmd_peer_send's _peer_send_remote path.
	if selfPairKey != "" {
		homeHost, err := st.GetRoomHomeHost(ctx, "pk:"+selfPairKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return exitInternal
		}
		if homeHost != "" && homeHost != selfHostLabel() {
			msgID := *messageID
			if msgID == "" {
				msgID = sqlitestore.NewULID(time.Now().UTC())
			}
			return runPeerSendRemote(ctx, st, remoteSendArgs{
				pairKey:   selfPairKey,
				homeHost:  homeHost,
				messageID: msgID,
				toLabel:   toLabel,
				body:      body,
				selfLabel: self.Label,
				jsonOut:   *jsonOut,
			})
		}
	}

	// Hand the actual write to SQLiteLocal.Send. It handles dedup probe,
	// target-session lookup (with ErrPeerNotFound / ErrPeerOffline), room
	// state checks (ErrRoomTerminated / ErrTurnCapExceeded), server_seq
	// assignment, bump_room / bump_last_seen, and the best-effort channel
	// push after commit.
	nowUTC := time.Now().UTC()
	// --to-cwd override: honor caller's explicit target regardless of
	// pair-key membership. Matches Python cmd_peer_send's resolve_cwd
	// normalization.
	var resolvedToCWD string
	if *toCWD != "" {
		rc, cerr := resolveCWD(*toCWD)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resolve --to-cwd: %v\n", cerr)
			return exitConfigError
		}
		resolvedToCWD = rc
	}

	params := sqlitestore.SendParams{
		SenderCWD:   self.CWD,
		SenderLabel: self.Label,
		ToLabel:     toLabel,
		Body:        body,
		MessageID:   *messageID,
		PairKey:     selfPairKey,
		TargetCWD:   resolvedToCWD,
		Mentions:    []string(mentions),
		Now:         nowUTC,
	}

	result, err := st.Send(ctx, params)
	if err != nil {
		return mapSendError(err, toLabel)
	}

	emitSendOutput(result, toLabel, *jsonOut)
	return exitOK
}

// emitSendOutput writes the success line (plain or JSON) on stdout to
// match Python's byte-for-byte output. Plain: `sent to <to> (<push>)
// [[end]]->room terminated @mentions[x,y] [deduped]` (conditionals as
// per Python). JSON: ok/to/message_id/server_seq/push_status/dedup_hit/
// terminates/mentions object with Python's insertion order + separators.
func emitSendOutput(r sqlitestore.SendResult, toLabel string, jsonMode bool) {
	// Python plain path keeps push_status="no-channel" on dedup (the
	// push code block is behind `if recipient_socket`, and recipient_socket
	// is only populated on the non-dedup branch). JSON path, however,
	// explicitly sets push_status to "deduped" on dedup. Match both.
	plainPush := r.PushStatus
	jsonPush := r.PushStatus
	if r.DedupHit {
		plainPush = "no-channel"
		jsonPush = "deduped"
	}
	mentions := r.Mentions
	if mentions == nil {
		mentions = []string{}
	}

	if jsonMode {
		// Build a struct (not map) so encoding/json preserves Python's
		// insertion-ordered dict key order. Strings match Python keys.
		payload := struct {
			Ok         bool     `json:"ok"`
			To         string   `json:"to"`
			MessageID  string   `json:"message_id"`
			ServerSeq  int64    `json:"server_seq"`
			PushStatus string   `json:"push_status"`
			DedupHit   bool     `json:"dedup_hit"`
			Terminates bool     `json:"terminates"`
			Mentions   []string `json:"mentions"`
		}{
			Ok:         true,
			To:         toLabel,
			MessageID:  r.MessageID,
			ServerSeq:  r.ServerSeq,
			PushStatus: jsonPush,
			DedupHit:   r.DedupHit,
			Terminates: r.Terminates,
			Mentions:   mentions,
		}
		if err := writePyCompatJSON(payload); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode json: %v\n", err)
		}
		return
	}

	// Plain summary line — matches Python:
	//   f"sent to {to} ({push_status}){term_note}{mention_note}{dedup_note}"
	// where term_note is " [[end]]->room terminated" (note the → arrow
	// is UTF-8) when terminates, mention_note is " @mentions[a,b]", and
	// dedup_note is " [deduped]".
	var termNote, mentionNote, dedupNote string
	if r.Terminates {
		termNote = " [[end]]\u2192room terminated"
	}
	if len(mentions) > 0 {
		mentionNote = " @mentions[" + strings.Join(mentions, ",") + "]"
	}
	if r.DedupHit {
		dedupNote = " [deduped]"
	}
	fmt.Printf("sent to %s (%s)%s%s%s\n", toLabel, plainPush, termNote, mentionNote, dedupNote)
}

// mapSendError translates store-level sentinel errors into the exit
// codes + stderr phrasings Python cmd_peer_send uses. Anything unmapped
// falls through to exitInternal with a generic stderr prefix.
func mapSendError(err error, toLabel string) int {
	_ = toLabel // reserved for future parity phrasings that include the label
	msg := err.Error()
	switch {
	case errors.Is(err, sqlitestore.ErrPeerNotFound):
		// SQLiteLocal.Send wraps ErrPeerNotFound with ": <label> in <cwd>".
		// Trim the sentinel prefix so the stderr looks like Python's
		// "peer not found: bob in /tmp/cwd".
		fmt.Fprintf(os.Stderr, "error: %s\n", trimSentinelPrefix(msg, sqlitestore.ErrPeerNotFound))
		return exitNotFound
	case errors.Is(err, sqlitestore.ErrPeerOffline):
		fmt.Fprintf(os.Stderr, "error: %s\n", trimSentinelPrefix(msg, sqlitestore.ErrPeerOffline))
		return exitPeerOffline
	case errors.Is(err, sqlitestore.ErrRoomTerminated):
		// Python adds a "run 'agent-collab peer reset ...'" suffix. The
		// store-level error only knows the room_key. Keep the message
		// short + correct rather than forge a reset hint we can't
		// compute without re-fetching room state.
		fmt.Fprintf(os.Stderr, "error: %s\n", trimSentinelPrefix(msg, sqlitestore.ErrRoomTerminated))
		return exitValidation
	case errors.Is(err, sqlitestore.ErrTurnCapExceeded):
		fmt.Fprintf(os.Stderr, "error: %s\n", trimSentinelPrefix(msg, sqlitestore.ErrTurnCapExceeded))
		return exitValidation
	case errors.Is(err, sqlitestore.ErrBodyTooLarge):
		fmt.Fprintf(os.Stderr, "error: %s\n", trimSentinelPrefix(msg, sqlitestore.ErrBodyTooLarge))
		return exitValidation
	case errors.Is(err, sqlitestore.ErrEmptyBody):
		fmt.Fprintln(os.Stderr, "error: empty message rejected")
		return exitValidation
	default:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitInternal
	}
}

// trimSentinelPrefix strips the Go-style `sentinel.Error(): ` prefix from
// a wrapped error's string so the stderr message reads like Python's
// (which uses its own f-strings instead of sentinel wrapping).
func trimSentinelPrefix(msg string, sentinel error) string {
	prefix := sentinel.Error() + ": "
	if strings.HasPrefix(msg, prefix) {
		return msg[len(prefix):]
	}
	return msg
}

// readBody picks one of --message-stdin / --message-file / --message,
// following Python's if/elif/elif precedence. Returns an error with the
// exact Python wording when none are set.
func readBody(stdinMode bool, fileArg, msgArg string) (string, error) {
	switch {
	case stdinMode:
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(raw), nil
	case fileArg != "":
		raw, err := os.ReadFile(fileArg)
		if err != nil {
			return "", fmt.Errorf("read message file: %w", err)
		}
		return string(raw), nil
	case msgArg != "":
		return msgArg, nil
	default:
		return "", fmt.Errorf("one of --message, --message-file, --message-stdin required")
	}
}

// maxBodyBytesFromEnv mirrors scripts/peer-inbox-db.py MAX_BODY_BYTES:
// env AGENT_COLLAB_MAX_BODY_BYTES or 8 KiB default.
func maxBodyBytesFromEnv() int {
	const fallback = 8 * 1024
	if v := os.Getenv("AGENT_COLLAB_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// selfHostLabel mirrors the Python self_host() helper via the Go port
// already in store/sqlite/send.go. Kept here as a thin re-export so the
// federation home_host check stays in the CLI file.
func selfHostLabel() string {
	// Replicate Python's precedence: AGENT_COLLAB_SELF_HOST env wins,
	// else os.Hostname() sanitized to [a-z0-9-], else "localhost".
	if v := os.Getenv("AGENT_COLLAB_SELF_HOST"); v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "localhost"
	}
	return out
}

// resolveSelfForSend mirrors Python's resolve_self: if --as was set we
// trust it and use the caller's resolved cwd; otherwise walk up from cwd
// looking for a session marker. Unlike the daemon verbs (which use the
// sqlite ResolveSelf directly), peer-send validates the label shape so
// CLI-entered --as values get the same rejection Python applies.
func resolveSelfForSend(ctx context.Context, st *sqlitestore.SQLiteLocal, cwd, asLbl string) (store.Session, error) {
	if asLbl != "" {
		if err := validateLabel(asLbl); err != nil {
			return store.Session{}, err
		}
		return store.Session{CWD: cwd, Label: asLbl}, nil
	}
	sess, err := st.ResolveSelf(ctx, cwd, discoverSessionKey())
	if err != nil {
		if errors.Is(err, store.ErrNoSession) {
			return store.Session{}, fmt.Errorf(
				"no session registered for %s or any parent "+
					"(run 'agent-collab session register' or pass --as)", cwd)
		}
		return store.Session{}, err
	}
	return sess, nil
}

// validateLabel mirrors Python's LABEL_RE check:
// ^[a-z0-9][a-z0-9_-]{0,63}$. Rejects uppercase, leading non-alnum, and
// characters outside the allowed set.
var labelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validateLabel(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("invalid label %q (allowed: [a-z0-9][a-z0-9_-]{0,63})", label)
	}
	return nil
}

// stringSlice is a small flag.Value impl for repeatable flags like
// --mention <label>. `flag` stdlib has no built-in "append" action, so
// each use appends to the slice.
type stringSlice []string

func (s *stringSlice) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Exit-code constants used by peer-send but absent from main.go (main.go
// only exposes the daemon-verb codes). Mirrors scripts/peer-inbox-db.py
// exit constants so fixtures can assert on integer values:
//
//	EXIT_CONFIG_ERROR = 2 (cwd / marker resolution failed)
//	EXIT_VALIDATION   = 3 (bad body, bad label, turn cap, termination)
//	EXIT_PEER_OFFLINE = 4 (last_seen_at too stale)
//
// EXIT_NOT_FOUND (6) and EXIT_USAGE (64) already live in main.go.
const (
	exitConfigError = 2
	exitValidation  = 3
	exitPeerOffline = 4
)
