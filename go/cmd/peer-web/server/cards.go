package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// handleCardsRoot dispatches /api/cards by method:
//   GET  → handleCards (list/filter for one board)
//   POST → handleCreateCard (create a new card)
func (s *Server) handleCardsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleCards(w, r)
	case http.MethodPost:
		s.handleCreateCard(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

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
	if c.AssignedToAgentID != 0 {
		m["assigned_to_agent_id"] = c.AssignedToAgentID
	}
	return m
}

// handleBoards — GET /api/boards
//
// Returns one summary row per pair_key that has at least one card.
// Backs the boards-index view at /cards (no pair_key).
//
// Response shape:
//
//	{
//	  "boards": [
//	    { "pair_key": "fleet-quartz-842c", "total": 6,
//	      "by_status": {"todo":1,"in_progress":0,"in_review":2,"done":2,"cancelled":1},
//	      "ready_todo": 1, "claimers": ["claude-session", "reviewer-claude"],
//	      "last_updated_at": "2026-04-25T..." },
//	    ...
//	  ]
//	}
func (s *Server) handleBoards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	boards, err := st.CardBoardSummaries(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]map[string]any, 0, len(boards))
	for _, b := range boards {
		claimers := b.Claimers
		if claimers == nil {
			claimers = []string{}
		}
		out = append(out, map[string]any{
			"pair_key": b.PairKey,
			"total":    b.Total,
			"by_status": map[string]int{
				"todo":        b.Todo,
				"in_progress": b.InProgress,
				"in_review":   b.InReview,
				"done":        b.Done,
				"cancelled":   b.Cancelled,
			},
			"ready_todo":      b.ReadyTodo,
			"distinct_roles":  b.DistinctRoles,
			"claimers":        claimers,
			"last_updated_at": b.LastUpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"boards": out})
}

// handleCreateCard — POST /api/cards
//
// Body: {"pair_key": "...", "title": "...", "body": "...",
//        "needs_role": "...", "priority": 0, "created_by": "...",
//        "tags": ["..."], "context_refs": {...}}
//
// pair_key + title are required. created_by defaults to "owner" so a
// human can create from the web UI without an explicit identity (mirrors
// the auto-register-owner pattern in /api/send).
//
// Returns the new card row on 201.
func (s *Server) handleCreateCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		PairKey     string          `json:"pair_key"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		NeedsRole   string          `json:"needs_role"`
		CreatedBy   string          `json:"created_by"`
		Priority    int             `json:"priority"`
		Tags        json.RawMessage `json:"tags"`
		ContextRefs json.RawMessage `json:"context_refs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.PairKey == "" || body.Title == "" {
		writeJSONError(w, http.StatusBadRequest, "pair_key and title are required")
		return
	}
	if body.CreatedBy == "" {
		body.CreatedBy = "owner"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	card, err := st.CreateCard(ctx, sqlitestore.CreateCardParams{
		PairKey:     body.PairKey,
		HomeHost:    sqlitestore.SelfHost(),
		Title:       body.Title,
		Body:        body.Body,
		NeedsRole:   body.NeedsRole,
		CreatedBy:   body.CreatedBy,
		Priority:    body.Priority,
		Tags:        string(body.Tags),
		ContextRefs: string(body.ContextRefs),
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, cardToJSON(card))
}

// handleUpdateCard — POST /api/cards/{id}/update
//
// Body: {"title"?, "body"?, "needs_role"?, "priority"?,
//        "tags"?, "context_refs"?}
//
// Partial update — only fields present in the body are mutated. Status,
// claim ownership, and dependency edges have dedicated endpoints.
//
// Returns the updated card row.
func (s *Server) handleUpdateCard(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Title       *string          `json:"title"`
		Body        *string          `json:"body"`
		NeedsRole   *string          `json:"needs_role"`
		Priority    *int             `json:"priority"`
		Tags        *json.RawMessage `json:"tags"`
		ContextRefs *json.RawMessage `json:"context_refs"`
		Author      string           `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	params := sqlitestore.UpdateCardFieldsParams{
		Title: body.Title, Body: body.Body,
		NeedsRole: body.NeedsRole, Priority: body.Priority,
	}
	if body.Tags != nil {
		s := string(*body.Tags)
		params.Tags = &s
	}
	if body.ContextRefs != nil {
		s := string(*body.ContextRefs)
		params.ContextRefs = &s
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	author := body.Author
	if author == "" {
		author = "owner"
	}
	card, err := st.UpdateCardFields(ctx, id, params, author)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		writeJSONError(w, http.StatusNotFound, "card not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cardToJSON(card))
}

// handleCardSubpath demultiplexes /api/cards/{id}/{verb} requests.
// Currently routes:
//   - /api/cards/{id}/status → handleCardStatus
//   - /api/cards/{id}/update → handleUpdateCard
func (s *Server) handleCardSubpath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/cards/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "card id must be a positive integer")
		return
	}
	switch parts[1] {
	case "status":
		s.handleCardStatus(w, r, id)
	case "update":
		s.handleUpdateCard(w, r, id)
	case "context":
		s.handleCardContext(w, r, id)
	case "run":
		s.handleCardRun(w, r, id)
	case "events":
		s.handleCardEvents(w, r, id)
	case "comment":
		s.handleCardComment(w, r, id)
	case "assign":
		s.handleCardAssign(w, r, id)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown card verb")
	}
}

// handleCardAssign — POST/DELETE /api/cards/{id}/assign
//
//	POST   body: {"agent": "<label>"} → set assigned_to_agent_id
//	DELETE                            → clear assigned_to_agent_id
//
// Logged on the card's activity timeline as an "assigned"/"unassigned"
// event so the audit trail is queryable from the drawer.
func (s *Server) handleCardAssign(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodPost:
		s.handleCardAssignSet(w, r, id)
	case http.MethodDelete:
		s.handleCardAssignClear(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type cardAssignBody struct {
	Agent  string  `json:"agent"`
	Author *string `json:"author"`
}

func (s *Server) handleCardAssignSet(w http.ResponseWriter, r *http.Request, id int64) {
	var body cardAssignBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Agent == "" {
		writeJSONError(w, http.StatusBadRequest, "agent label required")
		return
	}
	author := "system"
	if body.Author != nil && *body.Author != "" {
		author = *body.Author
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, body.Agent)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	saved, err := st.AssignCardToAgent(ctx, id, a.ID, author)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			writeJSONError(w, http.StatusNotFound, "card not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"card_id":              saved.ID,
		"assigned_to_agent_id": saved.AssignedToAgentID,
		"agent_label":          a.Label,
	})
}

func (s *Server) handleCardAssignClear(w http.ResponseWriter, r *http.Request, id int64) {
	author := r.URL.Query().Get("author")
	if author == "" {
		author = "system"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	saved, err := st.AssignCardToAgent(ctx, id, 0, author)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			writeJSONError(w, http.StatusNotFound, "card not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"card_id":              saved.ID,
		"assigned_to_agent_id": nil,
	})
}

// handleCardEvents — GET /api/cards/{id}/events
//
// Returns the card's timeline (comments + auto-recorded events),
// oldest-first. ?limit=N caps the response.
func (s *Server) handleCardEvents(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	events, err := st.ListCardEvents(ctx, id, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		row := map[string]any{
			"id": e.ID, "card_id": e.CardID, "kind": e.Kind,
			"author": e.Author, "body": e.Body,
			"created_at": e.CreatedAt,
		}
		if e.Meta != "" && e.Meta != "{}" {
			row["meta"] = json.RawMessage(e.Meta)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"card_id": id, "events": out,
	})
}

// handleCardComment — POST /api/cards/{id}/comment
//
// Body: {"body": "...", "author": "..."}
//
// Posts a free-form comment to the card timeline. Author defaults to
// "owner" so a human can post from the drawer composer without an
// explicit identity (mirrors /api/cards create).
func (s *Server) handleCardComment(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeJSONError(w, http.StatusBadRequest, "body is required")
		return
	}
	if body.Author == "" {
		body.Author = "owner"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	ev, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: id, Kind: sqlitestore.CardEventComment,
		Author: body.Author, Body: body.Body,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": ev.ID, "card_id": ev.CardID, "kind": ev.Kind,
		"author": ev.Author, "body": ev.Body,
		"created_at": ev.CreatedAt,
	})
}

// handleCardStatus — POST /api/cards/{id}/status
//
// Body: {"status": "todo|in_progress|in_review|done|cancelled"}
//
// Used by the kanban SPA when a card is dragged between columns.
// Returns the updated card on success, mirroring /api/cards row shape.
func (s *Server) handleCardStatus(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Status string `json:"status"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Status == "" {
		writeJSONError(w, http.StatusBadRequest, "status is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	author := body.Author
	if author == "" {
		author = "owner"
	}
	card, err := st.UpdateCardStatus(ctx, id, body.Status, author)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		writeJSONError(w, http.StatusNotFound, "card not found")
		return
	}
	if errors.Is(err, sqlitestore.ErrCardInvalidStatus) {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, cardToJSON(card))
}

func parseBool(s string) bool {
	switch s {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
