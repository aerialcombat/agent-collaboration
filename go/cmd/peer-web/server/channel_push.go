package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// handleChannelPush is the v3.5 cross-host channel-push gateway. Other
// hosts' `pushChannel` dispatcher POSTs here when a session's channel
// URI is http(s):// and points at this host's peer-web. We look up the
// local session by (label, pair_key) and forward the payload to its
// local unix socket, so the receiving Claude context sees the push
// identically to same-host pushes.
//
// Request:
//
//	POST /api/channel-push?label=<label>&pair_key=<key>
//	Content-Type: application/json
//	Body: {"from": "<sender-label>", "body": "<text>", "meta": {...}}
//
// PoC scope: no auth. Deployments are Tailscale/LAN/VPN-scoped. v3.5.1
// adds bearer-token auth reciprocal to the existing sessions.auth_token.
//
// Responses:
//
//	200  {"ok": true}              forwarded successfully
//	400  {"error": "..."}          missing params / bad json
//	404  {"error": "..."}          no local session for label+pair_key
//	410  {"error": "..."}          session has no forwardable channel
//	502  {"error": "..."}          unix socket returned non-200 or dial failed
//
// The 502 case is distinct from 404/410: the session is registered and
// has a unix socket URI, but the socket isn't accepting pushes
// (MCP server crashed, file went away since session-register). Operators
// debugging "push didn't arrive" can tell "no such session" from
// "session exists but channel dead" from the response codes.
func (s *Server) handleChannelPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	label := r.URL.Query().Get("label")
	if label == "" {
		writeJSONError(w, http.StatusBadRequest, "label query param required")
		return
	}
	pairKey := r.URL.Query().Get("pair_key") // empty allowed for cwd-mode

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	// Normalize: require `body` field. `from` and `meta` are pass-through.
	if _, ok := payload["body"]; !ok {
		writeJSONError(w, http.StatusBadRequest, "payload.body required")
		return
	}

	// v3.6 multi-room: propagate pair_key from the query string into
	// payload.meta so the Python channel MCP can stamp the correct
	// <channel pair_key=...> header and route replies back into the
	// right room. Legacy senders (pre-v3.6 binaries) don't include
	// meta.pair_key; payload values from v3.6 senders win.
	if pairKey != "" {
		meta, ok := payload["meta"].(map[string]any)
		if !ok {
			meta = map[string]any{}
		}
		if _, has := meta["pair_key"]; !has {
			meta["pair_key"] = pairKey
		}
		payload["meta"] = meta
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	code, body, err := st.ForwardLocalPush(ctx, label, pairKey, payload)
	if err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrNoLocalSession):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, sqlitestore.ErrNoLocalChannel):
			writeJSONError(w, http.StatusGone, err.Error())
		default:
			writeJSONError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	if code != http.StatusOK {
		writeJSONError(w, http.StatusBadGateway,
			"local unix channel returned "+intToDec(code)+": "+body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// intToDec is a tiny stdlib-free decimal formatter for a status code in
// an error message. Avoids an strconv import for this single call site.
func intToDec(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
