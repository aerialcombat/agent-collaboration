package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.2 — per-board pool composition HTTP surface.
//
//	GET    /api/boards/{pk}/pool                → list joined members
//	POST   /api/boards/{pk}/pool                → add by agent label
//	PATCH  /api/boards/{pk}/pool/{agent_label}  → update count/priority
//	DELETE /api/boards/{pk}/pool/{agent_label}  → remove
//
// Member-targeted routes use the agent label in the path (operator-
// friendly). The handler resolves to agent_id via GetAgentByLabel
// before touching pool_members.

func (s *Server) handleBoardPool(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/boards/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] != "pool" {
		writeJSONError(w, http.StatusNotFound, "expected /api/boards/{pair_key}/pool[/{agent}]")
		return
	}
	pairKey := parts[0]
	memberLabel := ""
	if len(parts) == 3 {
		memberLabel = parts[2]
	}

	switch r.Method {
	case http.MethodGet:
		if memberLabel != "" {
			writeJSONError(w, http.StatusMethodNotAllowed, "GET on /pool/{agent} not supported; GET /pool returns the full list")
			return
		}
		s.handlePoolList(w, r, pairKey)
	case http.MethodPost:
		if memberLabel != "" {
			writeJSONError(w, http.StatusMethodNotAllowed, "POST on /pool/{agent} not supported; POST /pool adds a member")
			return
		}
		s.handlePoolAdd(w, r, pairKey)
	case http.MethodPatch:
		if memberLabel == "" {
			writeJSONError(w, http.StatusBadRequest, "PATCH requires /pool/{agent_label} target")
			return
		}
		s.handlePoolUpdate(w, r, pairKey, memberLabel)
	case http.MethodDelete:
		if memberLabel == "" {
			writeJSONError(w, http.StatusBadRequest, "DELETE requires /pool/{agent_label} target")
			return
		}
		s.handlePoolRemove(w, r, pairKey, memberLabel)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePoolList(w http.ResponseWriter, r *http.Request, pairKey string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	members, err := st.ListPoolMembers(ctx, pairKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(members))
	for i := range members {
		out = append(out, poolMemberToJSON(&members[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pair_key": pairKey,
		"members":  out,
	})
}

type poolAddBody struct {
	Agent    string  `json:"agent"`
	Count    *int    `json:"count"`
	Priority *int    `json:"priority"`
	AddedBy  *string `json:"added_by"`
}

func (s *Server) handlePoolAdd(w http.ResponseWriter, r *http.Request, pairKey string) {
	var body poolAddBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Agent == "" {
		writeJSONError(w, http.StatusBadRequest, "agent label required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
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
	p := sqlitestore.AddPoolMemberParams{PairKey: pairKey, AgentID: a.ID}
	if body.Count != nil {
		p.Count = *body.Count
	}
	if body.Priority != nil {
		p.Priority = *body.Priority
	}
	if body.AddedBy != nil {
		p.AddedBy = *body.AddedBy
	}
	m, err := st.AddPoolMember(ctx, p)
	if err != nil {
		// Duplicate (pair_key, agent_id) is a 409 — operator should
		// PATCH instead of POST.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeJSONError(w, http.StatusConflict, "agent already in pool — use PATCH to update")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, poolMemberToJSON(m))
}

type poolUpdateBody struct {
	Count    *int `json:"count"`
	Priority *int `json:"priority"`
}

func (s *Server) handlePoolUpdate(w http.ResponseWriter, r *http.Request, pairKey, agentLabel string) {
	var body poolUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Count == nil && body.Priority == nil {
		writeJSONError(w, http.StatusBadRequest, "pass count and/or priority")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := sqlitestore.UpdatePoolMemberParams{Count: body.Count, Priority: body.Priority}
	if err := st.UpdatePoolMember(ctx, pairKey, a.ID, p); err != nil {
		if errors.Is(err, sqlitestore.ErrPoolMemberNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not in pool")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := st.GetPoolMember(ctx, pairKey, a.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, poolMemberToJSON(saved))
}

func (s *Server) handlePoolRemove(w http.ResponseWriter, r *http.Request, pairKey, agentLabel string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := st.RemovePoolMember(ctx, pairKey, a.ID); err != nil {
		if errors.Is(err, sqlitestore.ErrPoolMemberNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not in pool")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed":  true,
		"pair_key": pairKey,
		"agent":    agentLabel,
	})
}

func poolMemberToJSON(m *sqlitestore.PoolMember) map[string]any {
	out := map[string]any{
		"pair_key":      m.PairKey,
		"agent_id":      m.AgentID,
		"agent_label":   m.AgentLabel,
		"agent_runtime": m.AgentRuntime,
		"agent_enabled": m.AgentEnabled,
		"count":         m.Count,
		"priority":      m.Priority,
		"added_at":      m.AddedAt,
	}
	if m.AgentRole != "" {
		out["agent_role"] = m.AgentRole
	}
	if m.AddedBy != "" {
		out["added_by"] = m.AddedBy
	}
	return out
}
