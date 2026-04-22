package server

import (
	"context"
	"log"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.8 phase 2: stale-active watchdog.
//
// Agents are supposed to flip state=active on turn-start (hook or pi
// message_start event) and state=idle on turn-end (Stop hook or pi
// turn_end event). If the agent crashes or is killed mid-turn, the
// idle-side write never fires and the session's state gets stuck on
// "active" forever — peer-web's UI would show a permanent green pulse
// for a dead agent. That violates the v3.8 invariant "if UI says busy,
// Claude must be busy."
//
// The watchdog fixes this by sweeping every minute: any session with
// state=active whose state_changed_at is older than staleActiveCutoff
// gets force-flipped to idle. The cutoff is 30 minutes, deliberately
// longer than any reasonable turn (Claude's typical multi-tool turns
// run 10s-5min). A session that's genuinely busy for >30min has
// bigger problems than a wrong UI dot, and the next UserPromptSubmit
// hook will flip it back to active within a second.
//
// The sweep is a single UPDATE — zero RPCs, zero reads, just a
// bounded WHERE clause. Cheap enough to run unconditionally.

const (
	staleActiveCutoff    = 30 * time.Minute
	staleActiveSweepTick = 1 * time.Minute
)

func (s *Server) runStaleActiveWatchdog(ctx context.Context) {
	// First tick immediately on boot — cleans up any state rows left
	// stranded from a prior crash before the first client render.
	if err := sweepStaleActive(ctx); err != nil {
		log.Printf("peer-web watchdog: first sweep failed: %v", err)
	}

	t := time.NewTicker(staleActiveSweepTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sweepStaleActive(ctx); err != nil {
				log.Printf("peer-web watchdog: sweep failed: %v", err)
			}
		}
	}
}

// sweepStaleActive flips any stuck-active sessions back to idle.
// Returns the count of rows it touched for observability.
func sweepStaleActive(ctx context.Context) error {
	st, err := storeOpen(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.SweepStaleActive(ctx, staleActiveCutoff, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("peer-web watchdog: flipped %d stuck-active session(s) to idle", n)
	}
	return nil
}

// Silence unused-import when the store type isn't referenced directly;
// the interface method is exercised via webStore above.
var _ = sqlitestore.ErrSessionNotFound
