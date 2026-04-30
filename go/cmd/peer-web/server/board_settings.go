package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.11 Phase 1 — board settings endpoint.
//
//	GET  /api/boards/{pair_key}/settings  → BoardSettings JSON
//	POST /api/boards/{pair_key}/settings  → upsert; mutates auto_drain
//	                                         membership in the in-memory
//	                                         drainers map immediately.
//
// The endpoint is the only public knob for auto_drain / max_concurrent /
// auto_promote / poll_interval_secs after boot. Toggle auto_drain on
// here and the next handler call ensures a drainer; toggle off and the
// drainer is cancelled. Backwards-compat /start and /stop continue to
// work; they just route through the same upsert.

func (s *Server) handleBoardSettings(w http.ResponseWriter, r *http.Request) {
	pairKey := pairKeyFromBoardPath(r.URL.Path, "/settings")
	if pairKey == "" {
		writeJSONError(w, http.StatusBadRequest, "pair_key required in path")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleBoardSettingsGet(w, r, pairKey)
	case http.MethodPost:
		s.handleBoardSettingsPost(w, r, pairKey)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleBoardSettingsGet(w http.ResponseWriter, r *http.Request, pairKey string) {
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
	writeJSON(w, http.StatusOK, boardSettingsToJSON(cur))
}

type boardSettingsBody struct {
	AutoDrain        *bool   `json:"auto_drain"`
	MaxConcurrent    *int    `json:"max_concurrent"`
	AutoPromote      *bool   `json:"auto_promote"`
	PollIntervalSecs *int    `json:"poll_interval_secs"`
	UpdatedBy        *string `json:"updated_by"`
	ProjectRoot      *string `json:"project_root"` // Track 1 #1 — empty clears
}

func (s *Server) handleBoardSettingsPost(w http.ResponseWriter, r *http.Request, pairKey string) {
	var body boardSettingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
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
	cur.PairKey = pairKey
	if body.AutoDrain != nil {
		cur.AutoDrain = *body.AutoDrain
	}
	if body.MaxConcurrent != nil {
		cur.MaxConcurrent = *body.MaxConcurrent
	}
	if body.AutoPromote != nil {
		cur.AutoPromote = *body.AutoPromote
	}
	if body.PollIntervalSecs != nil {
		cur.PollIntervalSecs = *body.PollIntervalSecs
	}
	if body.ProjectRoot != nil {
		v := *body.ProjectRoot
		if v != "" && !strings.HasPrefix(v, "/") {
			writeJSONError(w, http.StatusBadRequest,
				"project_root must be an absolute path or empty")
			return
		}
		cur.ProjectRoot = v
	}
	if body.UpdatedBy != nil && *body.UpdatedBy != "" {
		cur.UpdatedBy = *body.UpdatedBy
	} else if cur.UpdatedBy == "" {
		cur.UpdatedBy = "owner"
	}
	if err := st.UpsertBoardSettings(ctx, cur); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Re-read so updated_at reflects the row in the DB rather than the
	// pre-upsert value the caller passed in.
	saved, err := st.GetBoardSettings(ctx, pairKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Sync the in-memory drainers map with the new auto_drain state so
	// the change takes effect immediately, not on the next tick.
	if saved.AutoDrain {
		s.ensureDrainer(pairKey, saved)
	} else {
		s.stopDrainer(pairKey)
	}

	writeJSON(w, http.StatusOK, boardSettingsToJSON(saved))
}

func boardSettingsToJSON(b sqlitestore.BoardSettings) map[string]any {
	m := map[string]any{
		"pair_key":           b.PairKey,
		"auto_drain":         b.AutoDrain,
		"max_concurrent":     b.MaxConcurrent,
		"auto_promote":       b.AutoPromote,
		"poll_interval_secs": b.PollIntervalSecs,
		"updated_at":         orNull(b.UpdatedAt),
		"updated_by":         orNull(b.UpdatedBy),
	}
	if b.ProjectRoot != "" {
		m["project_root"] = b.ProjectRoot
	}
	return m
}
