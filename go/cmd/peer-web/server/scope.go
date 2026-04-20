package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
)

// Default from AGENT_COLLAB_MAX_PAIR_TURNS in the Python CLI. Kept in
// parity so turn-cap warnings land at the same threshold the sender
// hits at cap-time.
const defaultMaxPairTurns = 100

// ScopeResponse mirrors the Python /scope.json payload with additions
// for multi-room mode. The Python version always returned exactly one
// of mode="cwd" or mode="pair_key"; the Go version adds mode="multi"
// for the unscoped index view. JS client sniffs the new field with a
// back-compat fallback.
type ScopeResponse struct {
	Mode         string `json:"mode"`                    // "multi" | "pair_key" | "cwd"
	PairKey      string `json:"pair_key,omitempty"`      // --pair-key deep-link value (if set)
	OnlyPairKey  string `json:"only_pair_key,omitempty"` // --only-pair-key lock (if set)
	CWD          string `json:"cwd,omitempty"`           // --cwd deep-link / cwd-mode
	AsLabel      string `json:"as_label,omitempty"`      // --as viewer label hint
	MultiRoom    bool   `json:"multi_room"`              // true when server is not locked to one scope
	Implementation string `json:"implementation"`        // "go" — lets the JS detect which server it's talking to
	MaxPairTurns int    `json:"max_pair_turns"`          // room turn cap (AGENT_COLLAB_MAX_PAIR_TURNS env); client uses for ≥80% warning
	StaleThresholdSecs int `json:"stale_threshold_secs"` // client-side stale filter threshold (10 min default)
}

func maxPairTurns() int {
	if v := os.Getenv("AGENT_COLLAB_MAX_PAIR_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxPairTurns
}

func (s *Server) handleScope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Mode resolution:
	//   --only-pair-key locked → "pair_key" (strictly scoped, multi_room=false)
	//   --pair-key deep-link   → "multi" but default_scope hints pair_key
	//   --cwd deep-link        → "multi" but default_scope hints cwd
	//   nothing                → "multi"
	// Python's single-mode semantics live at --only-pair-key only;
	// everything else is multi-room with optional first-visit deep-links.
	mode := "multi"
	multi := true
	if s.cfg.OnlyPairKey != "" {
		mode = "pair_key"
		multi = false
	}

	resp := ScopeResponse{
		Mode:               mode,
		PairKey:            s.cfg.PairKey,
		OnlyPairKey:        s.cfg.OnlyPairKey,
		CWD:                s.cfg.CWD,
		AsLabel:            s.cfg.AsLabel,
		MultiRoom:          multi,
		Implementation:     "go",
		MaxPairTurns:       maxPairTurns(),
		StaleThresholdSecs: 10 * 60,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// client has already received headers; nothing to do but log
		return
	}
}
