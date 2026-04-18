// Command peer-inbox — Go parity for the Topic 3 daemon-mode verbs that
// landed on scripts/peer-inbox-db.py in commit 9410604. Exposes three
// subcommands:
//
//	peer-inbox daemon-claim    --cwd DIR --as LABEL [--format json|plain]
//	peer-inbox daemon-complete --cwd DIR --as LABEL [--format json|plain]
//	peer-inbox daemon-sweep    [--cwd DIR] [--sweep-ttl SECS] [--format json|plain]
//
// All three operate on the same shared ~/.agent-collab/sessions.db that
// Python also writes. The (commit 7) daemon binary consumes this CLI —
// no Python subprocess appears in the daemon call path (scope-doc §9
// commit 4 resolution; gamma #6 + alpha endorse).
//
// Output shapes match Python byte-for-byte when `--format json` is
// selected so downstream consumers / tests that parse Python JSON output
// also parse Go output:
//
//	daemon-claim:    [{"id":N,"from_cwd":...,"from_label":...,"body":...,"created_at":...}, ...]
//	daemon-complete: {"completed_ids":[N,...]}
//	daemon-sweep:    {"sweep_ttl_seconds":N,"cutoff":"...","reaped":[{"id":N,"to_cwd":...,"to_label":...,"claim_owner":...}, ...]}
//
// Exit codes (match Python's EXIT_USAGE / EXIT_CONTRACT_VIOLATION):
//
//	0   success
//	64  EX_USAGE — CLI invoked with bad flags / missing required flags
//	65  EX_DATAERR — Topic 3 fail-loud contract violation
//	        (receive-mode mismatch, stale-claim --complete).
//
// Any other non-zero exit indicates an unexpected internal error
// (Open failure, SQL error). Callers should treat those as retryable.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"agent-collaboration/go/pkg/store"
	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

const (
	exitOK       = 0
	exitUsage    = 64 // sysexits EX_USAGE
	exitDataErr  = 65 // sysexits EX_DATAERR — Topic 3 fail-loud contract violation
	exitNotFound = 6  // matches Python EXIT_NOT_FOUND — session row not found by (cwd, label)
	exitInternal = 1

	// defaultSweepTTLSeconds mirrors Python's
	// `_default_sweep_ttl_seconds` fallback (10 minutes = 2× the 5-minute
	// daemon ack-timeout; Topic 3 §3.3 ratio-invariant default).
	defaultSweepTTLSeconds = 600
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "daemon-claim":
		return runClaim(rest)
	case "daemon-complete":
		return runComplete(rest)
	case "daemon-sweep":
		return runSweep(rest)
	case "daemon-reset-session":
		return runResetSession(rest)
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "peer-inbox: unknown subcommand %q\n", sub)
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: peer-inbox <daemon-claim|daemon-complete|daemon-sweep|daemon-reset-session> [flags]

  daemon-claim         --cwd DIR --as LABEL [--format json|plain]
  daemon-complete      --cwd DIR --as LABEL [--format json|plain]
  daemon-sweep         [--cwd DIR] [--sweep-ttl SECS] [--format json|plain]
  daemon-reset-session --cwd DIR --as LABEL [--format json|plain]

Go parity for the Python daemon-mode verbs in scripts/peer-inbox-db.py.
Exit 64 = usage error, 65 = Topic 3 fail-loud contract violation,
6 = session not found.
`)
}

// resolveCWD mirrors Python's resolve_cwd(args.cwd): explicit --cwd
// wins; otherwise process cwd. Resolves symlinks via
// filepath.EvalSymlinks so the returned path matches what Python
// stored at session-register time (Python uses Path.resolve(strict=True),
// which also resolves symlinks — e.g. macOS /var → /private/var).
// Falls back to the cleaned absolute path if EvalSymlinks fails
// (e.g. a non-existent cwd), mirroring the readMarker resolveOrSelf
// pattern.
func resolveCWD(explicit string) (string, error) {
	raw := explicit
	if raw == "" {
		c, err := os.Getwd()
		if err != nil {
			return "", err
		}
		raw = c
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("abs cwd: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// parseFormat enforces Python's `--format {json, plain}` choice set.
// Unlike Python's argparse this is explicit-validated so bad values fail
// with exitUsage and a clear message.
func parseFormat(v string) (string, error) {
	switch v {
	case "json", "plain":
		return v, nil
	default:
		return "", fmt.Errorf("--format must be one of: json, plain (got %q)", v)
	}
}

// ---------------------------------------------------------------------------
// daemon-claim
// ---------------------------------------------------------------------------

// claimRowJSON is the JSON byte-shape Python emits for each claimed row
// when `--format json` is set (see `_cmd_peer_receive_daemon_mode`'s
// `payload = [...]` construction). Omitting RoomKey keeps parity; the
// interactive hook path is the only consumer of room_key.
type claimRowJSON struct {
	ID        int64  `json:"id"`
	FromCWD   string `json:"from_cwd"`
	FromLabel string `json:"from_label"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func runClaim(args []string) int {
	fs := flag.NewFlagSet("daemon-claim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd    = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl  = fs.String("as", "", "session label to claim as (required)")
		format = fs.String("format", "plain", "output format: json | plain")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *asLbl == "" {
		fmt.Fprintln(os.Stderr, "daemon-claim: --as <label> is required")
		return exitUsage
	}
	fmtStr, err := parseFormat(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-claim: %v\n", err)
		return exitUsage
	}
	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-claim: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-claim: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	rows, err := st.DaemonModeClaim(ctx, self)
	if err != nil {
		if errors.Is(err, store.ErrReceiveModeMismatch) {
			fmt.Fprintf(os.Stderr,
				"receive-mode mismatch: session (%s, %s) is not a daemon-mode session; "+
					"daemon-claim requires receive_mode='daemon' "+
					"(register with `agent-collab session register --receive-mode daemon`; "+
					"Topic 3 §3.4 (b) verb-entry gate)\n",
				resolvedCWD, *asLbl,
			)
			return exitDataErr
		}
		fmt.Fprintf(os.Stderr, "daemon-claim: %v\n", err)
		return exitInternal
	}

	if fmtStr == "json" {
		payload := make([]claimRowJSON, 0, len(rows))
		for _, r := range rows {
			payload = append(payload, claimRowJSON{
				ID: r.ID, FromCWD: r.FromCWD, FromLabel: r.FromLabel,
				Body: r.Body, CreatedAt: r.CreatedAt,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "daemon-claim: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}
	// plain
	for _, r := range rows {
		fmt.Printf("[%s @ %s]\n%s\n\n", r.FromLabel, r.CreatedAt, r.Body)
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// daemon-complete
// ---------------------------------------------------------------------------

// completePayload mirrors Python's `{"completed_ids": [...]}` byte shape.
type completePayload struct {
	CompletedIDs []int64 `json:"completed_ids"`
}

func runComplete(args []string) int {
	fs := flag.NewFlagSet("daemon-complete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd    = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl  = fs.String("as", "", "session label (required)")
		format = fs.String("format", "plain", "output format: json | plain")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *asLbl == "" {
		fmt.Fprintln(os.Stderr, "daemon-complete: --as <label> is required")
		return exitUsage
	}
	fmtStr, err := parseFormat(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-complete: %v\n", err)
		return exitUsage
	}
	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-complete: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-complete: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	ids, err := st.DaemonModeComplete(ctx, self)
	if err != nil {
		if errors.Is(err, store.ErrReceiveModeMismatch) {
			fmt.Fprintf(os.Stderr,
				"receive-mode mismatch: session (%s, %s) is not a daemon-mode session; "+
					"daemon-complete requires receive_mode='daemon' "+
					"(Topic 3 §3.4 (b) verb-entry gate)\n",
				resolvedCWD, *asLbl,
			)
			return exitDataErr
		}
		if errors.Is(err, store.ErrStaleClaim) {
			fmt.Fprintf(os.Stderr,
				"stale claim: no in-flight claim held by (%s, %s) — "+
					"claim was reaped by sweeper (TTL-expired) or already completed. "+
					"Topic 3 §3.4 (d): fail-loud on stale claim; caller should log "+
					"rejected work and proceed.\n",
				resolvedCWD, *asLbl,
			)
			return exitDataErr
		}
		fmt.Fprintf(os.Stderr, "daemon-complete: %v\n", err)
		return exitInternal
	}

	if fmtStr == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(completePayload{CompletedIDs: asJSONIDSlice(ids)}); err != nil {
			fmt.Fprintf(os.Stderr, "daemon-complete: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}
	// plain
	fmt.Printf("completed: %d row(s); ids=%v\n", len(ids), asJSONIDSlice(ids))
	return exitOK
}

// asJSONIDSlice returns a non-nil slice so encoding/json emits `[]`
// rather than `null` when no ids are present. (DaemonModeComplete
// returns ErrStaleClaim in the zero-rows case today, so len(ids) > 0 in
// the success path, but this keeps the JSON shape robust.)
func asJSONIDSlice(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// ---------------------------------------------------------------------------
// daemon-sweep
// ---------------------------------------------------------------------------

// sweepReapJSON mirrors Python's per-reaped-row JSON shape.
type sweepReapJSON struct {
	ID         int64  `json:"id"`
	ToCWD      string `json:"to_cwd"`
	ToLabel    string `json:"to_label"`
	ClaimOwner string `json:"claim_owner"`
}

// sweepPayload mirrors Python's top-level JSON byte shape:
//
//	{"sweep_ttl_seconds": N, "cutoff": "...", "reaped": [...]}
type sweepPayload struct {
	SweepTTLSeconds int             `json:"sweep_ttl_seconds"`
	Cutoff          string          `json:"cutoff"`
	Reaped          []sweepReapJSON `json:"reaped"`
}

// resolveSweepTTL resolves TTL from (in order) --sweep-ttl, the
// AGENT_COLLAB_SWEEP_TTL env var, then the 10-minute production default.
// Matches Python's `_default_sweep_ttl_seconds` precedence + the explicit
// `sweep_ttl <= 0 → EXIT_USAGE` check in `_cmd_peer_receive_sweep`.
//
// Sentinel convention: caller passes -1 when --sweep-ttl was unset;
// 0 or negative passed explicitly triggers the "must be positive" usage
// error.
func resolveSweepTTL(explicit int) (int, error) {
	if explicit == -1 {
		// unset: fall through to env / default
		if v := os.Getenv("AGENT_COLLAB_SWEEP_TTL"); v != "" {
			n, err := strconv.Atoi(v)
			if err == nil && n > 0 {
				return n, nil
			}
		}
		return defaultSweepTTLSeconds, nil
	}
	if explicit <= 0 {
		return 0, fmt.Errorf("--sweep-ttl must be a positive integer (seconds)")
	}
	return explicit, nil
}

func runSweep(args []string) int {
	fs := flag.NewFlagSet("daemon-sweep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		// cwd is accepted for CLI symmetry even though sweep operates
		// globally (it doesn't need a session — it scans all claimed
		// rows). Python's --sweep ignores --cwd / --as too.
		_      = fs.String("cwd", "", "ignored (sweep is global)")
		format = fs.String("format", "plain", "output format: json | plain")
	)
	// Use sentinel -1 so we can distinguish "flag unset" from "flag set
	// to 0" (Python would reject 0 as EXIT_USAGE; we mirror that).
	sweepTTL := -1
	fs.IntVar(&sweepTTL, "sweep-ttl", -1, "sweep TTL in seconds (default: AGENT_COLLAB_SWEEP_TTL env or 600)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	fmtStr, err := parseFormat(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-sweep: %v\n", err)
		return exitUsage
	}
	ttlSecs, err := resolveSweepTTL(sweepTTL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-sweep: %v\n", err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-sweep: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	ttl := time.Duration(ttlSecs) * time.Second
	reaped, cutoff, err := st.DaemonModeSweepWithCutoff(ctx, ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-sweep: %v\n", err)
		return exitInternal
	}

	if fmtStr == "json" {
		payload := sweepPayload{
			SweepTTLSeconds: ttlSecs,
			Cutoff:          cutoff,
			Reaped:          make([]sweepReapJSON, 0, len(reaped)),
		}
		for _, r := range reaped {
			payload.Reaped = append(payload.Reaped, sweepReapJSON{
				ID: r.ID, ToCWD: r.ToCWD, ToLabel: r.ToLabel,
				ClaimOwner: r.ClaimOwner,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "daemon-sweep: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}
	// plain
	fmt.Printf("swept: %d row(s) reaped (ttl=%ds, cutoff=%s)\n", len(reaped), ttlSecs, cutoff)
	for _, r := range reaped {
		fmt.Printf("  id=%d to=(%s, %s) prev_claim_owner=%s\n",
			r.ID, r.ToCWD, r.ToLabel, r.ClaimOwner)
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// daemon-reset-session
// ---------------------------------------------------------------------------

// resetSessionPayload mirrors the Python verb's JSON byte shape: emits
// {"reset": true, "label": "..."}` so downstream observability can
// trivially confirm the operation succeeded.
type resetSessionPayload struct {
	Reset bool   `json:"reset"`
	Label string `json:"label"`
}

// runResetSession implements `peer-inbox daemon-reset-session --cwd DIR
// --as LABEL [--format json|plain]` per Topic 3 v0.1 (Architecture D)
// §8.1 PRIMARY reset primitive. NULLs sessions.daemon_cli_session_id
// for the named label so the next daemon spawn allocates a fresh CLI
// vendor session-ID.
//
// Idempotent per §3.4 invariant 3: safe to spam — clearing an already-
// NULL column returns success with the same payload.
//
// Exit codes:
//
//	0  success (reset applied; column may have been NULL or non-NULL)
//	64 missing/invalid --as
//	6  no session row for (cwd, label) — operator misconfigured the label
func runResetSession(args []string) int {
	fs := flag.NewFlagSet("daemon-reset-session", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd    = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl  = fs.String("as", "", "session label to reset (required)")
		format = fs.String("format", "plain", "output format: json | plain")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *asLbl == "" {
		fmt.Fprintln(os.Stderr, "daemon-reset-session: --as <label> is required")
		return exitUsage
	}
	fmtStr, err := parseFormat(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-reset-session: %v\n", err)
		return exitUsage
	}
	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-reset-session: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-reset-session: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	if err := st.ClearDaemonCLISessionID(ctx, self); err != nil {
		// Distinguish "no session row" from other SQL errors so the
		// operator gets a clean exit code per §8.1.
		if errors.Is(err, sql.ErrNoRows) || stringContains(err.Error(), "no session row") {
			fmt.Fprintf(os.Stderr,
				"daemon-reset-session: no session row for cwd=%q label=%q — "+
					"operator misconfigured the label, OR the session was never "+
					"registered. Run `agent-collab session register --cwd %s --label %s "+
					"--receive-mode daemon` first.\n",
				resolvedCWD, *asLbl, resolvedCWD, *asLbl,
			)
			return exitNotFound
		}
		fmt.Fprintf(os.Stderr, "daemon-reset-session: %v\n", err)
		return exitInternal
	}

	if fmtStr == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(resetSessionPayload{Reset: true, Label: *asLbl}); err != nil {
			fmt.Fprintf(os.Stderr, "daemon-reset-session: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}
	// plain — clear, operator-facing message; consistent wording with
	// the Python verb + the doc'd SQL escape hatch (§8.3).
	fmt.Printf("daemon-reset-session: cleared daemon_cli_session_id for (%s, %s); "+
		"next spawn will allocate a fresh CLI session\n", resolvedCWD, *asLbl)
	return exitOK
}

// stringContains is a tiny helper to avoid importing "strings" just for
// substring checks in error-path branches. Kept inline to match the file's
// minimal-deps style.
func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
