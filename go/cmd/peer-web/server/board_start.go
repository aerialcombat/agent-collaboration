package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.11 Phase 1 — standing drainer. board_settings.auto_drain is the
// durable on/off. peer-web boot reads every row with auto_drain=1 and
// spawns a drainer goroutine per pair_key; settings updates take
// effect within one tick.
//
// Per-tick logic:
//
//	1. Refresh BoardSettings (cheap; one indexed read).
//	2. deficit = max_concurrent − count(card_runs WHERE running, host=self).
//	3. If deficit > 0: SELECT ready+unclaimed cards, dispatch up to
//	   deficit via dispatchWorkerForCard. Each dispatch writes a
//	   card_runs row + spawns a monitor goroutine.
//	4. If auto_promote: walk in_review cards with hasOpenBlockee
//	   downstream, promote to done.
//
// Lifecycle: drainer goroutines exit on auto_drain=0 (settings flip
// observed on next tick) or on Server context cancellation. The
// drainer itself never decides "we're done" — async assignment means
// new cards can arrive at any time. Only auto_drain=0 stops it.

const (
	defaultDrainerPollInterval = 5 * time.Second
)

// drainer holds the per-pair_key drainer goroutine state.
type drainer struct {
	pairKey  string
	cancel   context.CancelFunc
	doneCh   chan struct{}

	mu         sync.Mutex
	settings   sqlitestore.BoardSettings
	dispatched int
	failures   int
	promoted   int
	lastErr    string
	lastTickAt time.Time
	startedAt  time.Time
}

func (d *drainer) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]any{
		"pair_key":           d.pairKey,
		"running":            true,
		"started_at":         d.startedAt.UTC().Format(time.RFC3339),
		"dispatched":         d.dispatched,
		"failures":           d.failures,
		"promoted":           d.promoted,
		"max_concurrent":     d.settings.MaxConcurrent,
		"auto_promote":       d.settings.AutoPromote,
		"poll_interval_secs": d.settings.PollIntervalSecs,
	}
	if !d.lastTickAt.IsZero() {
		out["last_tick_at"] = d.lastTickAt.UTC().Format(time.RFC3339)
	}
	if d.lastErr != "" {
		out["last_error"] = d.lastErr
	}
	return out
}

// handleBoardSubpath demuxes /api/boards/{pair_key}/{verb}.
// Supported verbs: start, stop, status, settings.
func (s *Server) handleBoardSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/boards/")
	idx := strings.LastIndex(rest, "/")
	if idx <= 0 || idx == len(rest)-1 {
		writeJSONError(w, http.StatusNotFound, "expected /api/boards/{pair_key}/{verb}")
		return
	}
	verb := rest[idx+1:]
	switch verb {
	case "start":
		s.handleBoardStart(w, r)
	case "stop":
		s.handleBoardStop(w, r)
	case "status":
		s.handleBoardStatus(w, r)
	case "settings":
		s.handleBoardSettings(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown board verb: "+verb)
	}
}

// handleBoardStart — POST /api/boards/{pair_key}/start
//
// Backward-compat shim: flips board_settings.auto_drain=1 (creating the
// row if absent) and ensures the drainer goroutine is running. The
// optional ?auto_promote=1 query param continues to work — sets
// auto_promote in board_settings.
func (s *Server) handleBoardStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := pairKeyFromBoardPath(r.URL.Path, "/start")
	if pairKey == "" {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	autoPromote := parseBool(r.URL.Query().Get("auto_promote"))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	cur, err := st.GetBoardSettings(ctx, pairKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur.AutoDrain = true
	if autoPromote {
		cur.AutoPromote = true
	}
	if cur.UpdatedBy == "" {
		cur.UpdatedBy = "board-start"
	}
	if err := st.UpsertBoardSettings(ctx, cur); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	d := s.ensureDrainer(pairKey, cur)
	writeJSON(w, http.StatusAccepted, d.snapshot())
}

// handleBoardStop — POST /api/boards/{pair_key}/stop
//
// Flips board_settings.auto_drain=0 (durable). The drainer goroutine
// observes the change on its next tick and exits.
func (s *Server) handleBoardStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := pairKeyFromBoardPath(r.URL.Path, "/stop")
	if pairKey == "" {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	cur, err := st.GetBoardSettings(ctx, pairKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur.AutoDrain = false
	if cur.UpdatedBy == "" {
		cur.UpdatedBy = "board-stop"
	}
	if err := st.UpsertBoardSettings(ctx, cur); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Cancel the in-memory goroutine immediately so the snapshot the
	// caller sees reflects the new state. The goroutine would also
	// notice on next tick, but that adds up to 5s of dead-air.
	s.stopDrainer(pairKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"pair_key":   pairKey,
		"running":    false,
		"auto_drain": false,
	})
}

// handleBoardStatus — GET /api/boards/{pair_key}/status
func (s *Server) handleBoardStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := pairKeyFromBoardPath(r.URL.Path, "/status")
	if pairKey == "" {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	s.drainersMu.Lock()
	d, ok := s.drainers[pairKey]
	s.drainersMu.Unlock()
	if !ok {
		// Fall back to the durable settings — the drainer might have
		// been auto_drain=1 but peer-web hasn't restarted yet.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		st, err := storeOpen(ctx)
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		defer st.Close()
		cur, err := st.GetBoardSettings(ctx, pairKey)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"pair_key":           pairKey,
			"running":            false,
			"auto_drain":         cur.AutoDrain,
			"max_concurrent":     cur.MaxConcurrent,
			"auto_promote":       cur.AutoPromote,
			"poll_interval_secs": cur.PollIntervalSecs,
		})
		return
	}
	writeJSON(w, http.StatusOK, d.snapshot())
}

// ensureDrainer starts a goroutine for pair_key if one isn't already
// running. settings is the current row (provided so callers don't need
// a second DB roundtrip).
func (s *Server) ensureDrainer(pairKey string, settings sqlitestore.BoardSettings) *drainer {
	s.drainersMu.Lock()
	defer s.drainersMu.Unlock()
	if s.drainers == nil {
		s.drainers = make(map[string]*drainer)
	}
	if d, ok := s.drainers[pairKey]; ok {
		d.mu.Lock()
		d.settings = settings
		d.mu.Unlock()
		return d
	}
	ctx, cancel := context.WithCancel(s.drainerCtx())
	d := &drainer{
		pairKey:   pairKey,
		cancel:    cancel,
		doneCh:    make(chan struct{}),
		settings:  settings,
		startedAt: time.Now(),
	}
	s.drainers[pairKey] = d
	go s.runDrainer(ctx, d)
	return d
}

// stopDrainer cancels the goroutine for pair_key (if present) and
// removes it from the map. Idempotent.
func (s *Server) stopDrainer(pairKey string) {
	s.drainersMu.Lock()
	d, ok := s.drainers[pairKey]
	if ok {
		delete(s.drainers, pairKey)
	}
	s.drainersMu.Unlock()
	if !ok {
		return
	}
	d.cancel()
	select {
	case <-d.doneCh:
	case <-time.After(2 * time.Second):
		// Don't block API responses on a slow drainer exit.
	}
}

// drainerCtx returns the parent context for all drainer goroutines.
// Bound to s.serveCtx so peer-web shutdown cancels every drainer.
func (s *Server) drainerCtx() context.Context {
	if s.serveCtx == nil {
		return context.Background()
	}
	return s.serveCtx
}

// runDrainer is the per-pair_key goroutine body. Long-running until ctx
// is cancelled or auto_drain flips to 0.
func (s *Server) runDrainer(ctx context.Context, d *drainer) {
	defer close(d.doneCh)

	tick := time.NewTimer(0) // fire immediately
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		shouldExit := s.drainerOnePass(ctx, d)
		if shouldExit {
			return
		}

		d.mu.Lock()
		interval := time.Duration(d.settings.PollIntervalSecs) * time.Second
		d.mu.Unlock()
		if interval < time.Second {
			interval = defaultDrainerPollInterval
		}
		tick.Reset(interval)
	}
}

// drainerOnePass runs one tick. Returns true when the goroutine should
// exit (auto_drain flipped to 0).
func (s *Server) drainerOnePass(parent context.Context, d *drainer) bool {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	st, err := storeOpen(ctx)
	if err != nil {
		d.recordTickErr("open store: " + err.Error())
		return false
	}
	defer st.Close()

	// Refresh settings every tick so UI updates take effect within one
	// poll interval.
	cur, err := st.GetBoardSettings(ctx, d.pairKey)
	if err != nil {
		d.recordTickErr("settings: " + err.Error())
		return false
	}
	d.mu.Lock()
	d.settings = cur
	d.mu.Unlock()

	if !cur.AutoDrain {
		// Owner flipped auto_drain off — exit cleanly.
		return true
	}

	// Capacity gate. Count running rows for this board on this host.
	running, err := st.CountRunningForBoard(ctx, d.pairKey, sqlitestore.SelfHost())
	if err != nil {
		d.recordTickErr("count running: " + err.Error())
		return false
	}
	deficit := cur.MaxConcurrent - running

	cards, err := st.ListCards(ctx, sqlitestore.CardListFilter{PairKey: d.pairKey})
	if err != nil {
		d.recordTickErr("list cards: " + err.Error())
		return false
	}

	dispatched := 0
	if deficit > 0 {
		for _, c := range cards {
			if deficit <= 0 {
				break
			}
			if c.Status != sqlitestore.CardStatusTodo || !c.Ready || c.ClaimedBy != "" {
				continue
			}
			// Skip cards already mid-flight (defensive — claimed_by check
			// above usually catches it, but a stale row could slip).
			busy, err := st.HasRunningRunForCard(ctx, c.ID)
			if err == nil && busy {
				continue
			}
			if _, err := dispatchWorkerForCard(ctx, st, c, sqlitestore.CardRunTriggerDrainer); err != nil {
				d.mu.Lock()
				d.failures++
				d.lastErr = fmt.Sprintf("dispatch #%d: %v", c.ID, err)
				d.mu.Unlock()
				continue
			}
			dispatched++
			deficit--
			// Tiny stagger reduces SQLite contention on burst dispatch.
			time.Sleep(50 * time.Millisecond)
		}
	}

	promoted := 0
	if cur.AutoPromote {
		for _, c := range cards {
			if c.Status != sqlitestore.CardStatusInReview {
				continue
			}
			if !hasOpenBlockee(c, cards) {
				continue
			}
			if _, err := st.UpdateCardStatus(ctx, c.ID, sqlitestore.CardStatusDone, "drainer-auto-promote"); err != nil {
				d.mu.Lock()
				d.failures++
				d.lastErr = fmt.Sprintf("auto-promote #%d: %v", c.ID, err)
				d.mu.Unlock()
				continue
			}
			promoted++
		}
	}

	d.mu.Lock()
	d.dispatched += dispatched
	d.promoted += promoted
	d.lastTickAt = time.Now()
	if dispatched > 0 || promoted > 0 {
		d.lastErr = ""
	}
	d.mu.Unlock()
	return false
}

func (d *drainer) recordTickErr(msg string) {
	d.mu.Lock()
	d.lastErr = msg
	d.lastTickAt = time.Now()
	d.failures++
	d.mu.Unlock()
}

// hasOpenBlockee is true when this card is a blocker for at least one
// other card that's still in a non-terminal state. Used by auto-promote
// to avoid clobbering legitimate review queues.
func hasOpenBlockee(c *sqlitestore.Card, all []*sqlitestore.Card) bool {
	if len(c.BlockeeIDs) == 0 {
		return false
	}
	byID := make(map[int64]*sqlitestore.Card, len(all))
	for _, x := range all {
		byID[x.ID] = x
	}
	for _, id := range c.BlockeeIDs {
		other, ok := byID[id]
		if !ok {
			continue
		}
		if other.Status == sqlitestore.CardStatusTodo ||
			other.Status == sqlitestore.CardStatusInProgress {
			return true
		}
	}
	return false
}

// reapOrphanedRuns runs once at peer-web boot. Walks card_runs in
// status='running' for self_host and finalizes them based on pid
// liveness. The "die with parent" rule (Phase 1 plan §12 Q1) means
// children don't usually survive a peer-web restart on Linux when
// run under systemd's KillMode=mixed; on macOS they may survive but
// we no longer hold the cmd.Wait handle, so we treat them as lost.
//
// Logic per row:
//
//	pid==0 OR kill(pid,0) returns ESRCH → mark 'lost', exit_code=-1.
//	pid alive                            → also mark 'lost' (we lost
//	                                       the wait handle on restart).
//	                                       The orphan keeps running
//	                                       until self-exit; output goes
//	                                       to its log file unmonitored.
//
// This is the pragmatic answer over re-adopting via os.FindProcess +
// poll-loop: the orphan can no longer post run_complete events with an
// exit code, so claiming we "still know" what it's doing would be a
// lie. Reaping to 'lost' is honest.
func (s *Server) reapOrphanedRuns(ctx context.Context) (int, error) {
	st, err := storeOpen(ctx)
	if err != nil {
		return 0, err
	}
	defer st.Close()

	rows, err := st.ListRunningCardRuns(ctx, sqlitestore.SelfHost())
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, r := range rows {
		if r.PID > 0 && pidAlive(r.PID) {
			fmt.Fprintf(os.Stderr,
				"reaper: run=%d card=%d pid=%d still alive but unmonitored — marking lost\n",
				r.ID, r.CardID, r.PID)
		}
		if err := st.FinishCardRun(ctx, r.ID, sqlitestore.CardRunStatusLost, -1); err != nil {
			fmt.Fprintf(os.Stderr, "reaper: FinishCardRun run=%d: %v\n", r.ID, err)
			continue
		}
		// Append a run_complete event so the timeline reflects reality.
		_, _ = st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
			CardID: r.CardID, Kind: sqlitestore.CardEventRunComplete,
			Author: "system",
			Body:   fmt.Sprintf("run %d reaped on peer-web boot (was pid %d)", r.ID, r.PID),
			Meta:   `{"status":"lost","reason":"peer_web_restart"}`,
		})
		reaped++
	}
	return reaped, nil
}

// pidAlive returns true when kill(pid, 0) doesn't return ESRCH.
// Cross-platform; Go's syscall.Kill works on darwin + linux.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// reconstructAutoDrainBoards is the boot-time companion to the reaper.
// Reads board_settings WHERE auto_drain=1 and starts a drainer for each.
func (s *Server) reconstructAutoDrainBoards(ctx context.Context) (int, error) {
	st, err := storeOpen(ctx)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	rows, err := st.ListBoardSettingsAutoDrain(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range rows {
		s.ensureDrainer(b.PairKey, b)
	}
	return len(rows), nil
}

// pairKeyFromBoardPath extracts {pair_key} from /api/boards/{pair_key}{suffix}.
// Returns "" if the suffix isn't matched or pair_key is empty.
func pairKeyFromBoardPath(path, suffix string) string {
	rest := strings.TrimPrefix(path, "/api/boards/")
	pk := strings.TrimSuffix(rest, suffix)
	if pk == rest || pk == "" || strings.Contains(pk, "/") {
		return ""
	}
	return pk
}
