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
	"strings"
	"time"

	"agent-collaboration/go/pkg/envelope"
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
	case "session-list":
		return runSessionList(rest)
	case "peer-list":
		return runPeerList(rest)
	case "peer-receive":
		return runPeerReceive(rest)
	case "session-register":
		return runSessionRegister(rest)
	case "peer-send":
		return runPeerSend(rest)
	case "peer-broadcast":
		return runPeerBroadcast(rest)
	case "peer-round":
		return runPeerRound(rest)
	case "room-create":
		return runRoomCreate(rest)
	case "peer-reset":
		return runPeerReset(rest)
	case "peer-queue":
		return runPeerQueue(rest)
	case "hook-log-session":
		return runHookLogSession(rest)
	case "session-adopt":
		return runSessionAdopt(rest)
	case "session-close":
		return runSessionClose(rest)
	case "session-state":
		return runSessionState(rest)
	case "card-create":
		return runCardCreate(rest)
	case "card-list":
		return runCardList(rest)
	case "card-get":
		return runCardGet(rest)
	case "card-claim":
		return runCardClaim(rest)
	case "card-update-status":
		return runCardUpdateStatus(rest)
	case "card-update":
		return runCardUpdate(rest)
	case "card-comment":
		return runCardComment(rest)
	case "card-add-dep":
		return runCardAddDep(rest)
	case "card-remove-dep":
		return runCardRemoveDep(rest)
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
	fmt.Fprintf(os.Stderr, `usage: peer-inbox <daemon-claim|daemon-complete|daemon-sweep|daemon-reset-session|session-list> [flags]

  daemon-claim         --cwd DIR --as LABEL [--format json|plain]
  daemon-complete      --cwd DIR --as LABEL [--format json|plain]
  daemon-sweep         [--cwd DIR] [--sweep-ttl SECS] [--format json|plain]
  daemon-reset-session --cwd DIR --as LABEL [--format json|plain]
  session-list         [--cwd DIR] [--all-cwds] [--include-stale] [--json]
  peer-list            [--cwd DIR] [--as LABEL] [--include-stale] [--json]
  peer-receive         [--cwd DIR] [--as LABEL] [--since ISO] [--format plain|json|hook|hook-json]

v3.4 Phase 3 landed (native Go):
  session-register, peer-send, peer-broadcast, peer-round,
  room-create, peer-reset, peer-queue.

v3.4 Phase 4 edge verbs:
  hook-log-session, session-adopt, session-close.

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
//
// Topic 3 v0.2 §8.1 pi-extension: DeletedFile is populated (non-empty
// string) when the reset targeted a pi-managed label AND a session file
// existed on disk and was removed. Empty otherwise (non-pi agent, or
// pi with NULL column, or pi with non-existent file).
type resetSessionPayload struct {
	Reset       bool   `json:"reset"`
	Label       string `json:"label"`
	DeletedFile string `json:"deleted_file,omitempty"`
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

	// Topic 3 v0.2 §8.1 pi-extension: read agent + cached path BEFORE
	// clearing the column so we can gate the file-delete side-effect on
	// sessions.agent == 'pi'. Reading both in a single SELECT avoids a
	// window where the agent check could observe a different row than
	// the subsequent Clear.
	agent, cachedPath, err := st.GetSessionAgentAndCLISessionID(ctx, self)
	if err != nil {
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

	if err := st.ClearDaemonCLISessionID(ctx, self); err != nil {
		// Defensive branch: GetSessionAgentAndCLISessionID succeeded, so
		// the row exists. Any Clear error at this point is a SQL fault,
		// not a missing-row. Still handle sql.ErrNoRows for safety.
		if errors.Is(err, sql.ErrNoRows) || stringContains(err.Error(), "no session row") {
			fmt.Fprintf(os.Stderr,
				"daemon-reset-session: no session row for cwd=%q label=%q\n",
				resolvedCWD, *asLbl,
			)
			return exitNotFound
		}
		fmt.Fprintf(os.Stderr, "daemon-reset-session: %v\n", err)
		return exitInternal
	}

	// Topic 3 v0.2 §8.1 pi-specific file-delete side-effect, widened
	// for v0.3 §3.3 Shape β WIDE + path-shape guard:
	//   - Agent gate: sessions.agent IN {pi, codex, gemini}. Covers
	//     shim-backed rows (--cli=codex / --cli=gemini routed through
	//     spawnPi in v0.3) while excluding claude-direct (which stays
	//     NULL-only per v0.1 §4.3 asymmetry).
	//   - Path-shape guard: only invoke os.Remove when cachedPath
	//     contains "/" (path-like). Pre-v0.3 legacy UUID values in
	//     codex/gemini rows don't contain "/" and are skipped — this
	//     preserves the v0.2 4e/5h cross-CLI-reset-isolation invariants
	//     (a UUID pre-populated via test fixture stays untouched).
	// NotExist tolerance on os.Remove handles the "path existed but
	// file was already deleted" case; §3.4 invariant 3 guarantees
	// reset is safe-to-spam.
	deletedPath := ""
	shimmableAgent := agent == "pi" || agent == "codex" || agent == "gemini"
	if shimmableAgent && cachedPath != "" && strings.Contains(cachedPath, "/") {
		if err := os.Remove(cachedPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// File existed, Remove failed (permission, etc.) — log
				// to stderr but keep the reset SUCCESS. The column is
				// already NULL'd; the operator can manually `rm` the
				// file per §8.3 TERTIARY. Non-fatal.
				fmt.Fprintf(os.Stderr,
					"daemon-reset-session: warning: column cleared but os.Remove(%q) failed: %v\n",
					cachedPath, err,
				)
			}
			// NotExist is the idempotent path.
		} else {
			deletedPath = cachedPath
		}
	}

	if fmtStr == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(resetSessionPayload{Reset: true, Label: *asLbl, DeletedFile: deletedPath}); err != nil {
			fmt.Fprintf(os.Stderr, "daemon-reset-session: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}
	// plain — clear, operator-facing message; consistent wording with
	// the Python verb + the doc'd SQL escape hatch (§8.3). Pi adds an
	// extra line naming the deleted file when applicable.
	fmt.Printf("daemon-reset-session: cleared daemon_cli_session_id for (%s, %s); "+
		"next spawn will allocate a fresh CLI session\n", resolvedCWD, *asLbl)
	if deletedPath != "" {
		fmt.Printf("daemon-reset-session: deleted session file: %s\n", deletedPath)
	}
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

// ---------------------------------------------------------------------------
// session-list (v3.4 Phase 2 — read-only verb port)
// ---------------------------------------------------------------------------
//
// Mirrors scripts/peer-inbox-db.py::cmd_session_list byte-for-byte. Parity
// gate is tests/parity/run.sh session-list <scenario> go; diverging from
// the Python output shape (column widths, role-default "peer", trailing
// "  cwd=..." on --all-cwds, JSON array shape) breaks the harness.

// idleThresholdSecs + staleThresholdSecs mirror the Python constants.
// Any bump needs to land on both sides in lockstep; the parity harness
// will surface drift on the next CI run.
const (
	idleThresholdSecs  = 5 * 60
	staleThresholdSecs = 24 * 60 * 60
)

// sessionListRowJSON is the per-row JSON shape Python emits. Keys and
// order match the dict built in cmd_session_list.
type sessionListRowJSON struct {
	CWD        string `json:"cwd"`
	Label      string `json:"label"`
	Agent      string `json:"agent"`
	Role       any    `json:"role"` // null when empty, else string — Python stores NULL as None
	StartedAt  string `json:"started_at"`
	LastSeenAt string `json:"last_seen_at"`
	Activity   string `json:"activity"`
}

// activityState mirrors Python's activity_state(last_seen). Uses strptime-
// compatible parsing (`2006-01-02T15:04:05Z`); malformed timestamps fall
// back to "stale" so a bad row can't crash the list verb.
func activityState(lastSeen string) string {
	t, err := time.Parse("2006-01-02T15:04:05Z", lastSeen)
	if err != nil {
		return "stale"
	}
	age := time.Since(t).Seconds()
	if age < idleThresholdSecs {
		return "active"
	}
	if age < staleThresholdSecs {
		return "idle"
	}
	return "stale"
}

// resolveScopeCWD mirrors the "not --all-cwds" branch of Python's
// cmd_session_list: walk up from cwd looking for .agent-collab/sessions,
// and if found, scope the list to that owner's resolved cwd. Otherwise
// fall back to the passed-in cwd.
func resolveScopeCWD(cwd string) string {
	sessDir := findSessionsDirForList(cwd)
	if sessDir == "" {
		return cwd
	}
	// sessDir = <owner>/.agent-collab/sessions; owner is 2 parents up.
	owner := filepath.Dir(filepath.Dir(sessDir))
	if resolved, err := filepath.EvalSymlinks(owner); err == nil {
		return resolved
	}
	return owner
}

// findSessionsDirForList walks up from cwd looking for any
// .agent-collab/sessions directory. Mirrors Python's
// find_any_sessions_dir; inline-defined here because the existing Go
// helper with the same semantics lives in the sqlite package and isn't
// exported (and keeping the CLI deps minimal).
func findSessionsDirForList(cwd string) string {
	cur := cwd
	for {
		candidate := filepath.Join(cur, ".agent-collab", "sessions")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func runSessionList(args []string) int {
	fs := flag.NewFlagSet("session-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd          = fs.String("cwd", "", "session cwd (default: process cwd)")
		allCWDs      = fs.Bool("all-cwds", false, "list sessions across all cwds")
		includeStale = fs.Bool("include-stale", false, "include sessions with stale last-seen timestamps")
		jsonOut      = fs.Bool("json", false, "emit JSON array instead of the human-readable table")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-list: resolve cwd: %v\n", err)
		return exitInternal
	}

	scopeCWD := resolvedCWD
	if !*allCWDs {
		scopeCWD = resolveScopeCWD(resolvedCWD)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-list: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	filterCWD := ""
	if !*allCWDs {
		filterCWD = scopeCWD
	}
	rows, err := st.ListSessions(ctx, filterCWD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-list: %v\n", err)
		return exitInternal
	}

	results := make([]sessionListRowJSON, 0, len(rows))
	for _, r := range rows {
		state := activityState(r.LastSeenAt)
		if state == "stale" && !*includeStale {
			continue
		}
		var roleVal any
		if r.Role != "" {
			roleVal = r.Role
		}
		results = append(results, sessionListRowJSON{
			CWD: r.CWD, Label: r.Label, Agent: r.Agent,
			Role: roleVal, StartedAt: r.StartedAt,
			LastSeenAt: r.LastSeenAt, Activity: state,
		})
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(os.Stderr, "session-list: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}

	if len(results) == 0 {
		fmt.Println("no sessions")
		return exitOK
	}

	for _, r := range results {
		role := "peer"
		if s, ok := r.Role.(string); ok && s != "" {
			role = s
		}
		// Python: f"{label:<20} {agent:<8} {role:<12} {activity:<7} last-seen {last_seen}"
		line := fmt.Sprintf("%s %s %s %s last-seen %s",
			padRight(r.Label, 20),
			padRight(r.Agent, 8),
			padRight(role, 12),
			padRight(r.Activity, 7),
			r.LastSeenAt,
		)
		if *allCWDs {
			line += "  cwd=" + r.CWD
		}
		fmt.Println(line)
	}
	return exitOK
}

// padRight emulates Python's `f"{s:<N}"` — left-justify in a field of
// width N, padding with spaces. If len(s) >= N, returns s unchanged.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// pyJSONFormat rewrites a compact JSON byte slice into Python's default
// json.dump spacing: ", " between elements and ": " between key and
// value. Required for byte-parity with scripts/peer-inbox-db.py whose
// JSON paths all call json.dump with default separators. Walks the
// bytes tracking in-string state so separators inside string literals
// are left alone.
func pyJSONFormat(compact []byte) []byte {
	out := make([]byte, 0, len(compact)+len(compact)/10)
	inString := false
	escape := false
	for _, b := range compact {
		out = append(out, b)
		if escape {
			escape = false
			continue
		}
		if inString {
			if b == '\\' {
				escape = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ',' || b == ':' {
			out = append(out, ' ')
		}
	}
	return out
}

// writePyCompatJSON marshals v with Go's encoder (HTML escapes disabled
// to match Python's default), then rewrites the spacing to Python's
// json.dump default so downstream byte diffs don't trigger on
// formatting. Trailing newline to mirror Python's explicit
// `sys.stdout.write("\n")`.
func writePyCompatJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// json.Marshal escapes HTML by default; Python's dump(ensure_ascii=
	// False) does not. Reverse the escapes inline rather than plumb
	// SetEscapeHTML(false) through every call site.
	raw = unescapeHTMLJSON(raw)
	spaced := pyJSONFormat(raw)
	if _, err := os.Stdout.Write(spaced); err != nil {
		return err
	}
	_, err = os.Stdout.Write([]byte("\n"))
	return err
}

// unescapeHTMLJSON reverses json.Marshal's HTML escaping (\u003c \u003e
// \u0026) so the output matches Python's ensure_ascii=False default.
// Conservative: only touches these three escape sequences; other
// \uXXXX escapes inside strings are left alone.
func unescapeHTMLJSON(b []byte) []byte {
	replacements := []struct {
		from, to string
	}{
		{`\u003c`, "<"},
		{`\u003e`, ">"},
		{`\u0026`, "&"},
	}
	s := string(b)
	for _, r := range replacements {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	return []byte(s)
}

// ---------------------------------------------------------------------------
// peer-list (v3.4 Phase 2 — read-only verb port)
// ---------------------------------------------------------------------------

// peerListRowJSON mirrors Python's dict shape in cmd_peer_list. Role
// renders as null when NULL/empty (Python's sqlite3 Row -> None -> JSON
// null); include-stale gating still runs in the aggregation loop.
type peerListRowJSON struct {
	Label      string `json:"label"`
	Agent      string `json:"agent"`
	Role       any    `json:"role"`
	Activity   string `json:"activity"`
	LastSeenAt string `json:"last_seen_at"`
}

// discoverSessionKey mirrors Python's discover_session_key env-var
// probe order. Used only when --as is omitted.
func discoverSessionKey() string {
	for _, name := range []string{
		"CLAUDE_SESSION_ID", "CODEX_SESSION_ID", "GEMINI_SESSION_ID",
		"AGENT_COLLAB_SESSION_KEY",
	} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func runPeerList(args []string) int {
	fs := flag.NewFlagSet("peer-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd          = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl        = fs.String("as", "", "session label (default: resolve via marker / env)")
		includeStale = fs.Bool("include-stale", false, "include peers with stale last-seen")
		jsonOut      = fs.Bool("json", false, "emit JSON array instead of the human table")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	selfCWD := resolvedCWD
	selfLabel := *asLbl
	if selfLabel == "" {
		self, err := st.ResolveSelf(ctx, resolvedCWD, discoverSessionKey())
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-list: resolve self: %v (pass --as <label> or set a session key env)\n", err)
			return exitInternal
		}
		selfCWD = self.CWD
		selfLabel = self.Label
	}

	pairKey, err := st.GetSessionPairKey(ctx, selfCWD, selfLabel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: %v\n", err)
		return exitInternal
	}

	rows, err := st.ListPeers(ctx, selfCWD, selfLabel, pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: %v\n", err)
		return exitInternal
	}

	results := make([]peerListRowJSON, 0, len(rows))
	for _, r := range rows {
		state := activityState(r.LastSeenAt)
		if state == "stale" && !*includeStale {
			continue
		}
		var roleVal any
		if r.Role != "" {
			roleVal = r.Role
		}
		results = append(results, peerListRowJSON{
			Label: r.Label, Agent: r.Agent, Role: roleVal,
			Activity: state, LastSeenAt: r.LastSeenAt,
		})
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(os.Stderr, "peer-list: encode json: %v\n", err)
			return exitInternal
		}
		return exitOK
	}

	if len(results) == 0 {
		fmt.Println("no peers")
		return exitOK
	}

	for _, r := range results {
		role := "peer"
		if s, ok := r.Role.(string); ok && s != "" {
			role = s
		}
		fmt.Printf("%s %s %s %s last-seen %s\n",
			padRight(r.Label, 20),
			padRight(r.Agent, 8),
			padRight(role, 12),
			padRight(r.Activity, 7),
			r.LastSeenAt,
		)
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// peer-receive (v3.4 Phase 2 — read-only inspect path)
// ---------------------------------------------------------------------------
//
// Ports the non-mutating path of cmd_peer_receive in
// scripts/peer-inbox-db.py: no --mark-read, no --daemon-mode/--complete/
// --sweep/--reset-session (those route to the dedicated daemon verbs).
// Mutually-exclusive checks still run so invoking the wrong flag
// produces the same exit64 Python does; the actual mutation-mode
// handlers are reserved for Phase 3 (write-verb port).

// peerReceiveRowJSON mirrors Python's per-row JSON shape in the --format
// json branch of cmd_peer_receive. read_at is omitted (Python's shape).
type peerReceiveRowJSON struct {
	ID        int64  `json:"id"`
	FromCWD   string `json:"from_cwd"`
	FromLabel string `json:"from_label"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// hookBudgetFromEnv mirrors Python's HOOK_BLOCK_BUDGET env override with
// a 4 KiB default. Matches hookBlockBudgetBytes in go/cmd/hook/main.go.
func hookBudgetFromEnv() int {
	if v := os.Getenv("AGENT_COLLAB_HOOK_BLOCK_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 4 * 1024
}

func runPeerReceive(args []string) int {
	fs := flag.NewFlagSet("peer-receive", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		cwd      = fs.String("cwd", "", "session cwd (default: process cwd)")
		asLbl    = fs.String("as", "", "session label (default: resolve via marker / env)")
		format   = fs.String("format", "plain", "output format: plain | json | hook | hook-json")
		since    = fs.String("since", "", "ISO-8601 UTC; skip messages older than this")
		markRead = fs.Bool("mark-read", false, "(Phase 3) interactive claim-and-mark; not yet ported in Go")
		daemon   = fs.Bool("daemon-mode", false, "(use `peer-inbox daemon-claim` instead)")
		complete = fs.Bool("complete", false, "(use `peer-inbox daemon-complete` instead)")
		sweep    = fs.Bool("sweep", false, "(use `peer-inbox daemon-sweep` instead)")
		reset    = fs.Bool("reset-session", false, "(use `peer-inbox daemon-reset-session` instead)")
	)
	// Consumed only for mutex validation + error text.
	_ = fs.Int("sweep-ttl", -1, "ignored by peer-receive (use daemon-sweep --sweep-ttl)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// Mutex check mirrors Python's len(sub_modes) > 1 validation.
	modeCount := 0
	for _, m := range []bool{*markRead, *daemon, *complete, *sweep, *reset} {
		if m {
			modeCount++
		}
	}
	if modeCount > 1 {
		fmt.Fprintln(os.Stderr,
			"peer receive: --mark-read, --daemon-mode, --complete, --sweep, --reset-session are mutually exclusive")
		return exitUsage
	}

	// Phase-2 scope: the mutating / daemon-delegated modes are not
	// reimplemented here. They are already exposed as top-level
	// subcommands on this binary (daemon-claim / daemon-complete /
	// daemon-sweep / daemon-reset-session) or deferred to Phase 3
	// (--mark-read). Route the operator to the correct verb rather
	// than silently diverging from Python.
	if *markRead {
		fmt.Fprintln(os.Stderr,
			"peer-receive --mark-read: not yet ported to Go (Phase 3 write-verb port); "+
				"fall back to the Python CLI for this mode.")
		return exitInternal
	}
	if *daemon {
		fmt.Fprintln(os.Stderr,
			"peer-receive --daemon-mode: use `peer-inbox daemon-claim` (already ported).")
		return exitUsage
	}
	if *complete {
		fmt.Fprintln(os.Stderr,
			"peer-receive --complete: use `peer-inbox daemon-complete` (already ported).")
		return exitUsage
	}
	if *sweep {
		fmt.Fprintln(os.Stderr,
			"peer-receive --sweep: use `peer-inbox daemon-sweep` (already ported).")
		return exitUsage
	}
	if *reset {
		fmt.Fprintln(os.Stderr,
			"peer-receive --reset-session: use `peer-inbox daemon-reset-session` (already ported).")
		return exitUsage
	}

	switch *format {
	case "plain", "json", "hook", "hook-json":
	default:
		fmt.Fprintf(os.Stderr, "peer-receive: --format must be one of: plain, json, hook, hook-json (got %q)\n", *format)
		return exitUsage
	}

	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-receive: resolve cwd: %v\n", err)
		return exitInternal
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-receive: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	self := store.Session{CWD: resolvedCWD, Label: *asLbl}
	if self.Label == "" {
		resolved, err := st.ResolveSelf(ctx, resolvedCWD, discoverSessionKey())
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-receive: resolve self: %v (pass --as <label> or set a session key env)\n", err)
			return exitInternal
		}
		self = resolved
	}

	rows, err := st.ListUnreadForSelf(ctx, self, *since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-receive: %v\n", err)
		return exitInternal
	}

	switch *format {
	case "json":
		payload := make([]peerReceiveRowJSON, 0, len(rows))
		for _, r := range rows {
			payload = append(payload, peerReceiveRowJSON{
				ID: r.ID, FromCWD: r.FromCWD, FromLabel: r.FromLabel,
				Body: r.Body, CreatedAt: r.CreatedAt,
			})
		}
		if err := writePyCompatJSON(payload); err != nil {
			fmt.Fprintf(os.Stderr, "peer-receive: encode json: %v\n", err)
			return exitInternal
		}
	case "hook":
		block := renderHookBlock(rows)
		if block != "" {
			// Python: stdout.write(block) then stdout.write("\n")
			fmt.Print(block)
			fmt.Println()
		}
	case "hook-json":
		block := renderHookBlock(rows)
		if block == "" {
			return exitOK
		}
		eventName := os.Getenv("AGENT_COLLAB_HOOK_EVENT_NAME")
		if eventName == "" {
			eventName = "UserPromptSubmit"
		}
		// Struct (not map) so field order matches Python's insertion-
		// ordered dict. Go's encoding/json sorts map keys alphabetically,
		// which breaks byte-parity against hookEventName-first Python.
		type hookEnv struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		}
		envlp := struct {
			HookSpecificOutput hookEnv `json:"hookSpecificOutput"`
		}{HookSpecificOutput: hookEnv{HookEventName: eventName, AdditionalContext: block}}
		if err := writePyCompatJSON(envlp); err != nil {
			fmt.Fprintf(os.Stderr, "peer-receive: encode hook-json: %v\n", err)
			return exitInternal
		}
	default: // plain
		for _, r := range rows {
			fmt.Printf("[%s @ %s]\n", r.FromLabel, r.CreatedAt)
			fmt.Println(r.Body)
			fmt.Println()
		}
	}
	return exitOK
}

// renderHookBlock reuses the shared envelope renderer so peer-receive's
// --format hook / hook-json paths emit byte-identical blocks to both
// the Python cmd_peer_receive path and the Go hook binary.
func renderHookBlock(rows []store.InboxMessage) string {
	if len(rows) == 0 {
		return ""
	}
	msgs := make([]envelope.Message, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, envelope.Message{
			ID: r.ID, FromCWD: r.FromCWD, FromLabel: r.FromLabel,
			Body: r.Body, CreatedAt: r.CreatedAt, RoomKey: r.RoomKey,
		})
	}
	env := envelope.BuildFromHookRows(msgs, nil)
	return envelope.RenderText(env, hookBudgetFromEnv())
}
