package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.4.7 — card-cancel-run: operator panic button for a runaway worker.
//
//   peer-inbox card-cancel-run --card N [--as AUTHOR] [--format plain|json]
//                              [--no-release]
//
// Marks the latest 'running' card_run for this card as 'cancelled',
// SIGTERMs the worker pid (if known), and (unless --no-release) clears
// the claim + flips the card back to todo so the drainer can re-pick.
//
// Cross-host: today this only signals processes on the current host.
// If the run lives on a different host (Host column ≠ current self_host)
// we still mark it cancelled in the DB but skip the kill — the remote
// peer-web's reaper will eventually pick it up.
//
// For permanent card cancellation, use card-update-status --status=cancelled.

func runCardCancelRun(args []string) int {
	fs := flag.NewFlagSet("card-cancel-run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cardID := fs.Int64("card", 0, "card id (required)")
	author := fs.String("as", "", "author label for the audit row (defaults to 'operator')")
	format := fs.String("format", "plain", "plain|json")
	noRelease := fs.Bool("no-release", false, "skip the claim release + status flip — leave the card in_progress (rare; for testing)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID == 0 {
		fmt.Fprintln(os.Stderr, "card-cancel-run: --card required")
		return exitUsage
	}
	authorLabel := *author
	if authorLabel == "" {
		authorLabel = "operator"
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-cancel-run: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	res, err := st.CancelLatestRun(ctx, *cardID)
	if errors.Is(err, sqlitestore.ErrNoRunningRun) {
		fmt.Fprintf(os.Stderr, "card-cancel-run: no running run for card #%d\n", *cardID)
		return exitNotFound
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-cancel-run: %v\n", err)
		return exitInternal
	}

	// Best-effort SIGTERM. Pid may already be gone (FinishCardRun blanks
	// it post-exit) or the host may not be us — both are non-fatal.
	signalled := false
	signalErr := ""
	if res.PID > 0 && (res.Host == "" || res.Host == sqlitestore.SelfHost()) {
		if err := syscall.Kill(res.PID, syscall.SIGTERM); err == nil {
			signalled = true
		} else if !errors.Is(err, syscall.ESRCH) {
			// ESRCH = no such process; expected if the worker already exited
			// between our DB read and the kill. Anything else is worth showing.
			signalErr = err.Error()
		}
	}

	// Audit comment so the drawer timeline shows WHY status flipped.
	_, _ = st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: *cardID, Kind: sqlitestore.CardEventComment, Author: authorLabel,
		Body: fmt.Sprintf("operator cancelled run #%d (worker %s, pid=%d, signalled=%v)",
			res.RunID, res.WorkerLabel, res.PID, signalled),
	})

	var card *sqlitestore.Card
	if !*noRelease {
		// Tiny grace period so the SIGTERM-handling worker can flush
		// its log + exit cleanly before we yank the claim. monitorWorker's
		// FinishCardRun is gated on status='running' (already cancelled),
		// so it's a no-op now anyway.
		if signalled {
			time.Sleep(500 * time.Millisecond)
		}
		c, err := st.ReleaseCardClaim(ctx, *cardID, authorLabel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "card-cancel-run: release claim: %v\n", err)
			return exitInternal
		}
		card = c
	}

	if *format == "json" {
		out := map[string]any{
			"run_id":       res.RunID,
			"worker_label": res.WorkerLabel,
			"pid":          res.PID,
			"signalled":    signalled,
		}
		if signalErr != "" {
			out["signal_error"] = signalErr
		}
		if card != nil {
			out["card_status"] = card.Status
			out["claimed_by"] = card.ClaimedBy
		}
		return emitJSON(out)
	}
	if signalled {
		fmt.Printf("cancelled run #%d (pid %d SIGTERM'd)\n", res.RunID, res.PID)
	} else if res.PID > 0 {
		fmt.Printf("cancelled run #%d (pid %d not signalled%s)\n", res.RunID, res.PID,
			func() string {
				if signalErr != "" {
					return ": " + signalErr
				}
				return " — process already gone or remote host"
			}())
	} else {
		fmt.Printf("cancelled run #%d (no pid recorded — worker likely exited before pid update)\n", res.RunID)
	}
	if card != nil {
		fmt.Printf("card #%d → status=%s claimed_by=%q\n", *cardID, card.Status, card.ClaimedBy)
	}
	return exitOK
}
