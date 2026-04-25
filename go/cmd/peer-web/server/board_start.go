package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// Shape B v1 — global "Start board" button. Clicking Start kicks off a
// background drainer goroutine for that pair_key. The drainer:
//
//   1. Polls every 5s for ready+unclaimed todo cards on the board.
//   2. Dispatches each via runWorkerForCard (parallel — they're
//      independent ready cards by definition).
//   3. Optionally auto-promotes in_review → done so chained cards
//      unblock without human review (opt-in via auto_promote=1).
//   4. Exits when no work remains (no ready, no in_progress, no
//      auto-promote candidates) — or when the user clicks Stop.
//
// State lives in-memory (Server.drainers). Survives concurrent
// Start-then-Stop-then-Start cycles via a per-pair_key mutex.
// peer-web restart loses drainer state — workers already spawned
// keep running (orphaned but functional); the user just doesn't see
// the "draining" badge anymore. Acceptable trade-off for v0.

const (
	drainerPollInterval = 5 * time.Second
	drainerMaxRuntime   = 30 * time.Minute
)

type drainer struct {
	pairKey     string
	autoPromote bool
	startedAt   time.Time
	stopCh      chan struct{}
	doneCh      chan struct{}

	mu          sync.Mutex
	dispatched  int
	promoted    int
	failures    int
	lastErr     string
	finishedAt  time.Time // zero if still running
}

func (d *drainer) snapshot() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]any{
		"pair_key":     d.pairKey,
		"auto_promote": d.autoPromote,
		"started_at":   d.startedAt.UTC().Format(time.RFC3339),
		"dispatched":   d.dispatched,
		"promoted":     d.promoted,
		"failures":     d.failures,
		"running":      d.finishedAt.IsZero(),
	}
	if d.lastErr != "" {
		out["last_error"] = d.lastErr
	}
	if !d.finishedAt.IsZero() {
		out["finished_at"] = d.finishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// handleBoardSubpath demuxes /api/boards/{pair_key}/{verb}.
// Supported verbs: start (POST), stop (POST), status (GET).
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
	default:
		writeJSONError(w, http.StatusNotFound, "unknown board verb: "+verb)
	}
}

// handleBoardStart — POST /api/boards/{pair_key}/start[?auto_promote=1]
func (s *Server) handleBoardStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/boards/"), "/start")
	if pairKey == "" || strings.Contains(pairKey, "/") {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	autoPromote := parseBool(r.URL.Query().Get("auto_promote"))

	s.drainersMu.Lock()
	if s.drainers == nil {
		s.drainers = make(map[string]*drainer)
	}
	if d, ok := s.drainers[pairKey]; ok && d.finishedAt.IsZero() {
		s.drainersMu.Unlock()
		writeJSON(w, http.StatusConflict, d.snapshot())
		return
	}
	d := &drainer{
		pairKey:     pairKey,
		autoPromote: autoPromote,
		startedAt:   time.Now(),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	s.drainers[pairKey] = d
	s.drainersMu.Unlock()

	go s.runDrainer(d)

	writeJSON(w, http.StatusAccepted, d.snapshot())
}

// handleBoardStop — POST /api/boards/{pair_key}/stop
func (s *Server) handleBoardStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/boards/"), "/stop")
	if pairKey == "" || strings.Contains(pairKey, "/") {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	s.drainersMu.Lock()
	d, ok := s.drainers[pairKey]
	s.drainersMu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no drainer running for this pair_key")
		return
	}
	select {
	case <-d.stopCh:
		// already closed
	default:
		close(d.stopCh)
	}
	writeJSON(w, http.StatusOK, d.snapshot())
}

// handleBoardStatus — GET /api/boards/{pair_key}/status
func (s *Server) handleBoardStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pairKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/boards/"), "/status")
	if pairKey == "" || strings.Contains(pairKey, "/") {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	s.drainersMu.Lock()
	d, ok := s.drainers[pairKey]
	s.drainersMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"pair_key": pairKey, "running": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, d.snapshot())
}

// runDrainer is the goroutine body. Detached from any HTTP request
// context — uses context.Background() with the drainer's stopCh as
// the cancellation signal, plus a hard 30-minute runtime ceiling so
// a stuck drainer can't live forever.
func (s *Server) runDrainer(d *drainer) {
	defer close(d.doneCh)
	defer func() {
		d.mu.Lock()
		d.finishedAt = time.Now()
		d.mu.Unlock()
	}()

	deadline := time.NewTimer(drainerMaxRuntime)
	defer deadline.Stop()

	tick := time.NewTicker(drainerPollInterval)
	defer tick.Stop()

	// First tick immediately so the user sees activity within a couple
	// hundred ms of clicking Start, not after a 5s wait.
	for first := true; ; {
		select {
		case <-d.stopCh:
			return
		case <-deadline.C:
			d.mu.Lock()
			d.lastErr = "max runtime exceeded"
			d.mu.Unlock()
			return
		default:
		}
		if !first {
			select {
			case <-d.stopCh:
				return
			case <-deadline.C:
				d.mu.Lock()
				d.lastErr = "max runtime exceeded"
				d.mu.Unlock()
				return
			case <-tick.C:
			}
		}
		first = false

		more := s.drainerOnePass(d)
		if !more {
			return
		}
	}
}

// drainerOnePass does a single sweep. Returns true if the loop should
// keep going (something might still be in flight or might unblock),
// false if the board is fully drained.
func (s *Server) drainerOnePass(d *drainer) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := storeOpen(ctx)
	if err != nil {
		d.mu.Lock()
		d.lastErr = "open store: " + err.Error()
		d.mu.Unlock()
		return true // transient — try again next tick
	}
	defer st.Close()

	cards, err := st.ListCards(ctx, sqlitestore.CardListFilter{
		PairKey: d.pairKey,
	})
	if err != nil {
		d.mu.Lock()
		d.lastErr = "list: " + err.Error()
		d.mu.Unlock()
		return true
	}

	// Dispatch every ready+unclaimed todo card.
	dispatched := 0
	for _, c := range cards {
		if c.Status != sqlitestore.CardStatusTodo || !c.Ready || c.ClaimedBy != "" {
			continue
		}
		if _, err := runWorkerForCard(ctx, st, c); err != nil {
			d.mu.Lock()
			d.failures++
			d.lastErr = fmt.Sprintf("dispatch #%d: %v", c.ID, err)
			d.mu.Unlock()
			continue
		}
		dispatched++
		// Tiny stagger to reduce SQLITE_BUSY pressure on the next claim
		// (busy_timeout handles it but no need to invite contention).
		time.Sleep(50 * time.Millisecond)
	}

	// Auto-promote in_review → done if opted in. Only promotes cards
	// that are blockers for some other card (i.e. promoting them
	// actually unblocks downstream work). Standalone in_review cards
	// stay there for a real human review.
	promoted := 0
	if d.autoPromote {
		for _, c := range cards {
			if c.Status != sqlitestore.CardStatusInReview {
				continue
			}
			// Only promote if this card has at least one downstream
			// blockee that is still todo (otherwise the promote is
			// meaningless and clobbers a legitimate review queue).
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
	d.mu.Unlock()

	// Decide whether to keep running. We stop when:
	//   - no ready+unclaimed todos remain (nothing to dispatch this pass)
	//   - AND no in_progress cards remain (nothing to wait on)
	//   - AND (no auto-promote, OR no chain-blocker in_review cards remain)
	hasReadyTodo := false
	hasInProgress := false
	hasChainReview := false
	for _, c := range cards {
		if c.Status == sqlitestore.CardStatusTodo && c.Ready && c.ClaimedBy == "" {
			hasReadyTodo = true
		}
		if c.Status == sqlitestore.CardStatusInProgress {
			hasInProgress = true
		}
		if c.Status == sqlitestore.CardStatusInReview && hasOpenBlockee(c, cards) {
			hasChainReview = true
		}
	}
	if hasReadyTodo || hasInProgress {
		return true
	}
	if d.autoPromote && hasChainReview {
		return true
	}
	return false
}

// hasOpenBlockee is true when this card is a blocker for at least one
// other card that's still in a non-terminal state. Used by
// auto-promote to avoid clobbering legitimate review queues.
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

