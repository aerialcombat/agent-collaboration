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

// v3.12.1 — agent registry HTTP surface backing the /agents UI and any
// remote operator that needs to manage agents over HTTP. The CLI
// (peer-inbox agent-*) talks to the local DB directly; this endpoint
// covers the same operations through peer-web.

// handleAgentsRoot dispatches /api/agents:
//
//	GET  → list (with ?enabled_only=1 filter)
//	POST → create
func (s *Server) handleAgentsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAgentsList(w, r)
	case http.MethodPost:
		s.handleAgentsCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAgentSubpath dispatches /api/agents/{id-or-label}:
//
//	GET    → fetch
//	PATCH  → update mutable fields
//	DELETE → remove
func (s *Server) handleAgentSubpath(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if suffix == "" {
		writeJSONError(w, http.StatusBadRequest, "agent id or label required in path")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAgentGet(w, r, suffix)
	case http.MethodPatch:
		s.handleAgentPatch(w, r, suffix)
	case http.MethodDelete:
		s.handleAgentDelete(w, r, suffix)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	enabledOnly := r.URL.Query().Get("enabled_only") == "1" ||
		r.URL.Query().Get("enabled_only") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	rows, err := st.ListAgents(ctx, enabledOnly)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, agentToJSON(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

type agentCreateBody struct {
	Label       string  `json:"label"`
	Runtime     string  `json:"runtime"`
	Role        *string `json:"role"`
	WorkerCmd   *string `json:"worker_cmd"`
	ModelConfig *string `json:"model_config"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Server) handleAgentsCreate(w http.ResponseWriter, r *http.Request) {
	var body agentCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Label == "" {
		writeJSONError(w, http.StatusBadRequest, "label required")
		return
	}
	if body.Runtime != sqlitestore.AgentRuntimeClaude && body.Runtime != sqlitestore.AgentRuntimePi {
		writeJSONError(w, http.StatusBadRequest, "runtime must be 'claude' or 'pi'")
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

	p := sqlitestore.CreateAgentParams{
		Label:   body.Label,
		Runtime: body.Runtime,
		Enabled: body.Enabled,
	}
	if body.Role != nil {
		p.Role = *body.Role
	}
	if body.WorkerCmd != nil {
		p.WorkerCmd = *body.WorkerCmd
	}
	if body.ModelConfig != nil {
		p.ModelConfig = *body.ModelConfig
	}
	a, err := st.CreateAgent(ctx, p)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentLabelTaken) {
			writeJSONError(w, http.StatusConflict, "label already exists")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, agentToJSON(a))
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request, key string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	a, err := lookupAgent(ctx, st, key)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agentToJSON(a))
}

type agentPatchBody struct {
	Runtime     *string `json:"runtime"`
	Role        *string `json:"role"`
	WorkerCmd   *string `json:"worker_cmd"`
	ModelConfig *string `json:"model_config"`
	Enabled     *bool   `json:"enabled"`
}

func (s *Server) handleAgentPatch(w http.ResponseWriter, r *http.Request, key string) {
	var body agentPatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
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

	a, err := lookupAgent(ctx, st, key)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := sqlitestore.UpdateAgentParams{
		Runtime:     body.Runtime,
		Role:        body.Role,
		WorkerCmd:   body.WorkerCmd,
		ModelConfig: body.ModelConfig,
		Enabled:     body.Enabled,
	}
	if err := st.UpdateAgent(ctx, a.ID, p); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := st.GetAgent(ctx, a.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agentToJSON(saved))
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request, key string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer st.Close()

	a, err := lookupAgent(ctx, st, key)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := st.DeleteAgent(ctx, a.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true, "id": a.ID, "label": a.Label,
	})
}

// lookupAgent resolves the path segment as either a numeric id or a
// label, returning the matching agent.
func lookupAgent(ctx context.Context, st webStore, key string) (*sqlitestore.Agent, error) {
	if id, err := strconv.ParseInt(key, 10, 64); err == nil && id > 0 {
		return st.GetAgent(ctx, id)
	}
	return st.GetAgentByLabel(ctx, key)
}

func agentToJSON(a *sqlitestore.Agent) map[string]any {
	m := map[string]any{
		"id":         a.ID,
		"label":      a.Label,
		"runtime":    a.Runtime,
		"enabled":    a.Enabled,
		"created_at": a.CreatedAt,
		"updated_at": a.UpdatedAt,
	}
	if a.Role != "" {
		m["role"] = a.Role
	}
	if a.WorkerCmd != "" {
		m["worker_cmd"] = a.WorkerCmd
	}
	if a.ModelConfig != "" {
		m["model_config"] = a.ModelConfig
	}
	return m
}
