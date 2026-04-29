package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// storeOpen is a package-level indirection so tests can fake it; real
// code opens the shared SQLite store via sqlitestore.Open.
var storeOpen = func(ctx context.Context) (webStore, error) {
	return sqlitestore.Open(ctx)
}

// webStore is the subset of SQLiteLocal methods the data handlers
// need. Narrow interface + indirection = testable without wiring a real
// DB.
type webStore interface {
	FetchPairs(ctx context.Context, opts sqlitestore.FetchPairsOpts) ([]sqlitestore.WebPair, error)
	FetchRooms(ctx context.Context, pairKey string) ([]sqlitestore.WebRoom, error)
	FetchMessages(ctx context.Context, opts sqlitestore.FetchMessagesOpts) ([]sqlitestore.WebMessage, bool, error)
	AllPairKeys(ctx context.Context) ([]string, error)
	SenderCWD(ctx context.Context, scope sqlitestore.SenderScope, label string) (string, error)
	ClearChannelSocket(ctx context.Context, cwd, label string) error
	TerminateRoom(ctx context.Context, pairKey, by string) error
	SessionByToken(ctx context.Context, token string) (*sqlitestore.SessionAuth, error)
	Send(ctx context.Context, params sqlitestore.SendParams) (sqlitestore.SendResult, error)
	BroadcastLocal(ctx context.Context, params sqlitestore.SendParams) ([]sqlitestore.SendResult, error)
	RegisterOwner(ctx context.Context, params sqlitestore.RegisterOwnerParams) error
	ForwardLocalPush(ctx context.Context, label, pairKey string, payload map[string]any) (int, string, error)
	SweepStaleActive(ctx context.Context, cutoff time.Duration, now time.Time) (int64, error)
	ListCards(ctx context.Context, filter sqlitestore.CardListFilter) ([]*sqlitestore.Card, error)
	UpdateCardStatus(ctx context.Context, id int64, status, author string) (*sqlitestore.Card, error)
	UpdateCardFields(ctx context.Context, id int64, params sqlitestore.UpdateCardFieldsParams, author string) (*sqlitestore.Card, error)
	AppendCardEvent(ctx context.Context, params sqlitestore.AppendCardEventParams) (*sqlitestore.CardEvent, error)
	ListCardEvents(ctx context.Context, cardID int64, limit int) ([]*sqlitestore.CardEvent, error)
	CreateCard(ctx context.Context, params sqlitestore.CreateCardParams) (*sqlitestore.Card, error)
	CardBoardSummaries(ctx context.Context) ([]*sqlitestore.CardBoardSummary, error)
	GetCard(ctx context.Context, id int64) (*sqlitestore.Card, error)
	MessagesByIDs(ctx context.Context, ids []int64) ([]sqlitestore.WebMessage, error)
	ClaimCard(ctx context.Context, id int64, label string, force bool) (*sqlitestore.Card, error)
	// v3.11 Phase 1 — durable runs + per-board drainer settings.
	CreateCardRun(ctx context.Context, params sqlitestore.CreateCardRunParams) (*sqlitestore.CardRun, error)
	UpdateCardRunPID(ctx context.Context, id int64, pid int) error
	FinishCardRun(ctx context.Context, id int64, status string, exitCode int) error
	GetCardRun(ctx context.Context, id int64) (*sqlitestore.CardRun, error)
	ListCardRunsByCard(ctx context.Context, cardID int64, limit int) ([]*sqlitestore.CardRun, error)
	ListRunningCardRuns(ctx context.Context, host string) ([]*sqlitestore.CardRun, error)
	CountRunningForBoard(ctx context.Context, pairKey, host string) (int, error)
	HasRunningRunForCard(ctx context.Context, cardID int64) (bool, error)
	GetBoardSettings(ctx context.Context, pairKey string) (sqlitestore.BoardSettings, error)
	ListBoardSettingsAutoDrain(ctx context.Context) ([]sqlitestore.BoardSettings, error)
	UpsertBoardSettings(ctx context.Context, b sqlitestore.BoardSettings) error
	// v3.12.1 — agent registry CRUD.
	CreateAgent(ctx context.Context, p sqlitestore.CreateAgentParams) (*sqlitestore.Agent, error)
	GetAgent(ctx context.Context, id int64) (*sqlitestore.Agent, error)
	GetAgentByLabel(ctx context.Context, label string) (*sqlitestore.Agent, error)
	ListAgents(ctx context.Context, enabledOnly bool) ([]sqlitestore.Agent, error)
	UpdateAgent(ctx context.Context, id int64, p sqlitestore.UpdateAgentParams) error
	DeleteAgent(ctx context.Context, id int64) error
	// v3.12.2 — per-board pool composition.
	AddPoolMember(ctx context.Context, p sqlitestore.AddPoolMemberParams) (*sqlitestore.PoolMember, error)
	GetPoolMember(ctx context.Context, pairKey string, agentID int64) (*sqlitestore.PoolMember, error)
	ListPoolMembers(ctx context.Context, pairKey string) ([]sqlitestore.PoolMember, error)
	UpdatePoolMember(ctx context.Context, pairKey string, agentID int64, p sqlitestore.UpdatePoolMemberParams) error
	RemovePoolMember(ctx context.Context, pairKey string, agentID int64) error
	// v3.12.4 — pool-aware dispatch + designated assignment.
	PickAgentForCard(ctx context.Context, card *sqlitestore.Card) (*sqlitestore.Agent, error)
	AssignCardToAgent(ctx context.Context, cardID, agentID int64, author string) (*sqlitestore.Card, error)
	Close() error
}

// handleTerminateInactive ends every room whose activity is "stale"
// (no member seen in the last STALE_THRESHOLD_SECS = 30min). Marks
// peer_rooms.terminated_at; reversible via `agent-collab peer reset
// --pair-key K`. Rejected when the server is locked via
// --only-pair-key (the lock pins scope; bulk operations across scopes
// would violate that pin).
func (s *Server) handleTerminateInactive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OnlyPairKey != "" {
		writeJSONError(w, http.StatusForbidden,
			"server is locked to --only-pair-key; cross-scope bulk ops disabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	pairKeys, err := st.AllPairKeys(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	terminated := []string{}
	skipped := []string{}
	for _, pk := range pairKeys {
		rooms, err := st.FetchRooms(ctx, pk)
		if err != nil || len(rooms) == 0 {
			skipped = append(skipped, pk)
			continue
		}
		rm := rooms[0]
		// Only stale rooms count as "inactive" — active/idle rooms are
		// too hot to bulk-terminate. Already-terminated rooms skip.
		if rm.Activity != "stale" {
			continue
		}
		if err := st.TerminateRoom(ctx, pk, "peer-web"); err != nil {
			skipped = append(skipped, pk)
			continue
		}
		terminated = append(terminated, pk)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"terminated":       terminated,
		"terminated_count": len(terminated),
		"skipped":          skipped,
		"skipped_count":    len(skipped),
	})
}

// handleIndex aggregates every pair_key room on the machine into one
// summary list so the user can jump between rooms without restarting
// the server. Multi-room mode only; if the server is locked via
// --only-pair-key, the handler returns just that one room.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
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

	var pairKeys []string
	if s.cfg.OnlyPairKey != "" {
		pairKeys = []string{s.cfg.OnlyPairKey}
	} else {
		pairKeys, err = st.AllPairKeys(ctx)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	rooms := make([]map[string]any, 0, len(pairKeys))
	for _, pk := range pairKeys {
		rs, err := st.FetchRooms(ctx, pk)
		if err != nil {
			// Don't fail the whole index on one bad pair_key; surface per-room.
			rooms = append(rooms, map[string]any{
				"pair_key": pk,
				"key":      pk,
				"error":    err.Error(),
			})
			continue
		}
		if len(rs) == 0 {
			continue
		}
		rooms = append(rooms, roomsToJSON(rs)...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rooms":    rooms,
		"locked":   s.cfg.OnlyPairKey != "",
		"pair_key": orNull(s.cfg.OnlyPairKey),
	})
}

// scopeFromRequest resolves the query-param scope shared across the
// data endpoints. Returns a decoded scope struct or an error describing
// the validation failure.
type reqScope struct {
	pairKey string
	cwd     string
}

func (s *Server) scopeFromRequest(r *http.Request) (reqScope, error) {
	// --only-pair-key K locks the server to one scope; any request for
	// anything else is rejected (403). Deep-link flags (--pair-key,
	// --cwd) do NOT lock — they're first-visit hints only.
	pk := r.URL.Query().Get("pair_key")
	cwd := r.URL.Query().Get("cwd")

	if s.cfg.OnlyPairKey != "" {
		if pk != "" && pk != s.cfg.OnlyPairKey {
			return reqScope{}, fmt.Errorf("server is locked to --only-pair-key; requested %q rejected", pk)
		}
		if cwd != "" {
			return reqScope{}, fmt.Errorf("server is locked to --only-pair-key; cwd scope rejected")
		}
		return reqScope{pairKey: s.cfg.OnlyPairKey}, nil
	}

	if pk == "" && cwd == "" {
		return reqScope{}, fmt.Errorf("scope required: pass ?pair_key=K or ?cwd=PATH")
	}
	if pk != "" && cwd != "" {
		return reqScope{}, fmt.Errorf("scope ambiguous: pass one of pair_key or cwd, not both")
	}
	return reqScope{pairKey: pk, cwd: cwd}, nil
}

func (s *Server) handlePairs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := s.scopeFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
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

	pairs, err := st.FetchPairs(ctx, sqlitestore.FetchPairsOpts{
		PairKey: scope.pairKey,
		CWD:     scope.cwd,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Python /pairs.json includes only cwd + pairs (pair_key lives in
	// /rooms.json, cwd on the pairs response is the process cwd).
	writeJSON(w, http.StatusOK, map[string]any{
		"cwd":   s.cfg.CWD,
		"pairs": pairsToJSON(pairs),
	})
	_ = scope
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := s.scopeFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if scope.pairKey == "" {
		writeJSONError(w, http.StatusBadRequest,
			"rooms only available in pair_key mode")
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

	rooms, err := st.FetchRooms(ctx, scope.pairKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pair_key": scope.pairKey,
		"rooms":    roomsToJSON(rooms),
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := s.scopeFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	opts := sqlitestore.FetchMessagesOpts{
		PairKey: scope.pairKey,
		CWD:     scope.cwd,
		After:   after,
		Before:  before,
		Limit:   limit,
		A:       q.Get("a"),
		B:       q.Get("b"),
		AsLabel: q.Get("as_label"),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	msgs, hasMore, err := st.FetchMessages(ctx, opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pair := ""
	if opts.A != "" && opts.B != "" {
		if opts.A < opts.B {
			pair = opts.A + "+" + opts.B
		} else {
			pair = opts.B + "+" + opts.A
		}
	}
	// oldest_id is the cursor the client feeds back as `before` on the
	// next scroll-up page. Zero when the result set is empty.
	var oldestID int64
	if len(msgs) > 0 {
		oldestID = msgs[0].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cwd":       s.cfg.CWD,
		"pair_key":  orNull(scope.pairKey),
		"pair":      orNull(pair),
		"messages":  messagesToJSON(msgs),
		"has_more":  hasMore,
		"oldest_id": oldestID,
	})
}

// --- JSON shape adapters -------------------------------------------------
//
// Map the typed store values to JSON shapes that match the Python
// /pairs.json + /rooms.json + /messages.json output byte-for-byte
// (modulo key order; JSON objects are unordered). Python uses "from" /
// "to" as inbox keys (reserved words in Go), so explicit shaping is the
// only way to hold parity.

func pairsToJSON(pairs []sqlitestore.WebPair) []map[string]any {
	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		peers := make(map[string]any, len(p.Peers))
		for lbl, m := range p.Peers {
			peers[lbl] = memberToJSON(m, false /* includeLabel */)
		}
		out = append(out, map[string]any{
			"a":             p.A,
			"b":             p.B,
			"key":           p.Key,
			"activity":      p.Activity,
			"total":         p.Total,
			"last_id":       p.LastID,
			"last_at":       orNull(p.LastAt),
			"turn_count":    p.TurnCount,
			"terminated_at": orNull(p.TerminatedAt),
			"terminated_by": orNull(p.TerminatedBy),
			"peers":         peers,
		})
	}
	return out
}

func roomsToJSON(rooms []sqlitestore.WebRoom) []map[string]any {
	out := make([]map[string]any, 0, len(rooms))
	for _, r := range rooms {
		members := make([]map[string]any, 0, len(r.Members))
		for _, m := range r.Members {
			members = append(members, memberToJSON(m, true))
		}
		homeHost := r.HomeHost
		roomID := r.PairKey
		if homeHost != "" {
			roomID = homeHost + ":" + r.PairKey
		}
		out = append(out, map[string]any{
			"pair_key":      r.PairKey,
			"home_host":     orNull(homeHost),
			"room_id":       roomID,
			"key":           r.Key,
			"activity":      r.Activity,
			"total":         r.Total,
			"last_id":       r.LastID,
			"last_at":       orNull(r.LastAt),
			"turn_count":    r.TurnCount,
			"terminated_at": orNull(r.TerminatedAt),
			"terminated_by": orNull(r.TerminatedBy),
			"members":       members,
		})
	}
	return out
}

func messagesToJSON(msgs []sqlitestore.WebMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id":         m.ID,
			"from":       m.From,
			"to":         m.To,
			"body":       m.Body,
			"created_at": m.CreatedAt,
			"read":       m.Read,
		})
	}
	return out
}

func memberToJSON(m sqlitestore.WebMember, includeLabel bool) map[string]any {
	out := map[string]any{
		"agent":        orNull(m.Agent),
		"role":         orNull(m.Role),
		"activity":     m.Activity,
		"last_seen_at": orNull(m.LastSeenAt),
		"channel":      m.Channel,
		// v3.8 activity monitoring: "active" | "idle" | null. Client
		// renders a pulse dot when state == "active"; state_changed_at
		// feeds the "busy for Xs" tooltip.
		"state":            orNull(m.State),
		"state_changed_at": orNull(m.StateChangedAt),
		// v3.8 phase 2: derived state for UI display. Accounts for
		// sessions whose agent process is unreachable (no channel
		// socket + stale last_seen) even though their state column
		// still reads "active" or "idle". Keeps the raw `state`
		// field as the truth from the hook/extension writes, while
		// `state_display` is what the UI should render.
		"state_display": deriveStateDisplay(m),
	}
	if includeLabel {
		out["label"] = m.Label
		out["cwd"] = orNull(m.CWD)
	}
	return out
}

// deriveStateDisplay composes a UI-ready state label from the raw
// state column plus reachability heuristics. Three-color palette:
//
//	"active"       — green. turn in progress (raw state=active).
//	"waiting"      — yellow. agent is present (live channel + recent
//	                  last_seen) and not generating. This includes
//	                  sessions with state=idle AND sessions whose
//	                  state column is still NULL (pre-v3.8 session or
//	                  one that hasn't fired its first hook yet). A
//	                  reachable agent that isn't busy IS waiting, by
//	                  definition — we don't need a hook to confirm it.
//	"disconnected" — red. actually unreachable: no channel socket, or
//	                  stale last_seen. The process can't be signalled.
//	"human"        — hidden. agent=human (owner in peer-web browser).
//
// The function is pure — no DB reads — so it's cheap to call on every
// member render.
func deriveStateDisplay(m sqlitestore.WebMember) string {
	if m.Agent == "human" {
		return "human"
	}
	// Reachability: a present agent has a live channel socket AND a
	// non-stale last_seen_at (activity bucket is "active" or "idle").
	// Anything else is disconnected regardless of what state says.
	reachable := m.Channel && (m.Activity == "active" || m.Activity == "idle")
	if !reachable {
		return "disconnected"
	}
	if m.State == "active" {
		return "active"
	}
	// Reachable + (state=idle OR state=NULL) → waiting. Null state on
	// a reachable agent just means the hook hasn't fired yet; the agent
	// is alive and not mid-turn, so "waiting" is the honest display.
	return "waiting"
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
