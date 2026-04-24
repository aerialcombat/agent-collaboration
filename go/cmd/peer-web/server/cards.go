package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// handleCards — GET /api/cards?pair_key=K
//
// Query params (all optional except pair_key):
//
//	pair_key      scope to one room (required; matches /api/rooms contract)
//	status        filter by status enum
//	needs_role    filter by needs_role
//	claimed_by    filter by claimed_by label
//	ready_only    boolean (1/true) — status=todo + no pending blockers
//	blocked_only  boolean — status=todo + ≥1 pending blocker
//	limit         max rows (integer, > 0)
//
// Response shape mirrors the Go CLI's card-list --format json:
//
//	{
//	  "pair_key": "fleet-quartz-842c",
//	  "cards": [ {id, title, status, ...} ],
//	  "by_status": {
//	    "todo": [1,2,...], "in_progress": [...], ...
//	  }
//	}
//
// The `by_status` bucket lets the SPA paint its kanban columns in one
// pass without re-grouping client-side.
func (s *Server) handleCards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	pairKey := q.Get("pair_key")
	if pairKey == "" {
		writeJSONError(w, http.StatusBadRequest,
			"pair_key query param is required")
		return
	}

	filter := sqlitestore.CardListFilter{
		PairKey:   pairKey,
		Status:    q.Get("status"),
		NeedsRole: q.Get("needs_role"),
		ClaimedBy: q.Get("claimed_by"),
	}
	if parseBool(q.Get("ready_only")) {
		filter.ReadyOnly = true
	}
	if parseBool(q.Get("blocked_only")) {
		filter.BlockedOnly = true
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest,
				"limit must be a positive integer")
			return
		}
		filter.Limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	cards, err := st.ListCards(ctx, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonCards := make([]map[string]any, 0, len(cards))
	byStatus := map[string][]int64{
		sqlitestore.CardStatusTodo:       {},
		sqlitestore.CardStatusInProgress: {},
		sqlitestore.CardStatusInReview:   {},
		sqlitestore.CardStatusDone:       {},
		sqlitestore.CardStatusCancelled:  {},
	}
	for _, c := range cards {
		jsonCards = append(jsonCards, cardToJSON(c))
		byStatus[c.Status] = append(byStatus[c.Status], c.ID)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"pair_key":  pairKey,
		"cards":     jsonCards,
		"by_status": byStatus,
	})
}

// cardToJSON mirrors cmd/peer-inbox/cards.go:cardAsMap so CLI JSON and
// API JSON are byte-equivalent. Kept local rather than exported from
// the store because the shape is presentation-layer, not domain.
func cardToJSON(c *sqlitestore.Card) map[string]any {
	m := map[string]any{
		"id":          c.ID,
		"pair_key":    c.PairKey,
		"home_host":   c.HomeHost,
		"title":       c.Title,
		"status":      c.Status,
		"created_by":  c.CreatedBy,
		"priority":    c.Priority,
		"created_at":  c.CreatedAt,
		"updated_at":  c.UpdatedAt,
		"blocker_ids": c.BlockerIDs,
		"blockee_ids": c.BlockeeIDs,
		"ready":       c.Ready,
	}
	if c.Body != "" {
		m["body"] = c.Body
	}
	if c.NeedsRole != "" {
		m["needs_role"] = c.NeedsRole
	}
	if c.ClaimedBy != "" {
		m["claimed_by"] = c.ClaimedBy
	}
	if c.Tags != "" {
		m["tags"] = json.RawMessage(c.Tags)
	}
	if c.ContextRefs != "" {
		m["context_refs"] = json.RawMessage(c.ContextRefs)
	}
	if c.ClaimedAt != "" {
		m["claimed_at"] = c.ClaimedAt
	}
	if c.CompletedAt != "" {
		m["completed_at"] = c.CompletedAt
	}
	return m
}

func parseBool(s string) bool {
	switch s {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
