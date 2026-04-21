package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// handleSend is the composer send path. v3.4 Phase 1 ported this to a
// native Go call into the sqlite store — no more python3 shell-out.
// Contract mirrors the prior Python-backed version byte-for-byte on
// happy paths + error codes:
//
//  1. Parse POST body {from, to, body, message_id}.
//  2. Bearer-token auth (v3.3 Item 7): non-owner senders must present
//     a valid Authorization: Bearer <token>; wrong label = 403.
//  3. "owner" is the one tokenless path — auto-register on first send
//     in pair-key mode, same as before.
//  4. Dispatch to store.Send (unicast) or store.BroadcastLocal
//     (to=="" or "@room").
//  5. Map typed errors to HTTP codes (peer-not-found 404,
//     peer-offline/turn-cap/terminated 400).
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := s.scopeFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Body      string `json:"body"`
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	body.From = trimLabel(body.From)
	body.To = trimLabel(body.To)
	body.MessageID = trimLabel(body.MessageID)
	if body.From == "" || body.Body == "" {
		writeJSONError(w, http.StatusBadRequest, "from + body required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	bearer := extractBearer(r.Header.Get("Authorization"))
	var sendCWD string
	switch {
	case bearer != "":
		auth, lookupErr := st.SessionByToken(ctx, bearer)
		if lookupErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "auth lookup: "+lookupErr.Error())
			return
		}
		if auth == nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		if body.From != "" && body.From != auth.Label {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("token is bound to %q, not %q", auth.Label, body.From))
			return
		}
		body.From = auth.Label
		sendCWD = auth.CWD
	case body.From != "owner":
		writeJSONError(w, http.StatusUnauthorized,
			"bearer token required for non-owner senders "+
				"(register the session on this host and set Authorization: Bearer <token>)")
		return
	default:
		sendCWD, err = s.resolveSenderCWD(ctx, st, scope, body.From)
		if err != nil {
			code := http.StatusNotFound
			if errors.Is(err, errSenderAutoRegisterFailed) {
				code = http.StatusInternalServerError
			}
			writeJSONError(w, code, err.Error())
			return
		}
	}

	params := sqlitestore.SendParams{
		SenderCWD:   sendCWD,
		SenderLabel: body.From,
		ToLabel:     body.To,
		Body:        body.Body,
		MessageID:   body.MessageID,
		PairKey:     scope.pairKey,
	}

	// Broadcast when To is empty or the explicit "@room" sentinel.
	if body.To == "" || body.To == "@room" {
		params.ToLabel = ""
		results, err := st.BroadcastLocal(ctx, params)
		if err != nil {
			writeJSONError(w, mapSendError(err), err.Error())
			return
		}
		writeBroadcastJSON(w, results)
		return
	}

	result, err := st.Send(ctx, params)
	if err != nil {
		writeJSONError(w, mapSendError(err), err.Error())
		return
	}
	writeSendJSON(w, result)
}

// mapSendError mirrors Python cmd_peer_send's exit codes in HTTP shape.
// EXIT_NOT_FOUND → 404, EXIT_PEER_OFFLINE / EXIT_VALIDATION → 400.
func mapSendError(err error) int {
	switch {
	case errors.Is(err, sqlitestore.ErrPeerNotFound):
		return http.StatusNotFound
	case errors.Is(err, sqlitestore.ErrPeerOffline),
		errors.Is(err, sqlitestore.ErrRoomTerminated),
		errors.Is(err, sqlitestore.ErrTurnCapExceeded),
		errors.Is(err, sqlitestore.ErrBodyTooLarge),
		errors.Is(err, sqlitestore.ErrEmptyBody):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeSendJSON(w http.ResponseWriter, r sqlitestore.SendResult) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"to":          r.To,
		"message_id":  r.MessageID,
		"server_seq":  r.ServerSeq,
		"push_status": r.PushStatus,
		"dedup_hit":   r.DedupHit,
		"terminates":  r.Terminates,
		"mentions":    r.Mentions,
	})
}

func writeBroadcastJSON(w http.ResponseWriter, results []sqlitestore.SendResult) {
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]any{
			"to":          r.To,
			"message_id":  r.MessageID,
			"server_seq":  r.ServerSeq,
			"push_status": r.PushStatus,
			"dedup_hit":   r.DedupHit,
			"terminates":  r.Terminates,
			"mentions":    r.Mentions,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"broadcast":  true,
		"recipients": out,
	})
}

var errSenderAutoRegisterFailed = errors.New("auto-register owner failed")

func (s *Server) resolveSenderCWD(
	ctx context.Context, st webStore, scope reqScope, from string,
) (string, error) {
	ss := sqlitestore.SenderScope{PairKey: scope.pairKey, CWD: scope.cwd}
	cwd, err := st.SenderCWD(ctx, ss, from)
	if err == nil {
		return cwd, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// Not registered. Owner auto-registration is supported only in
	// pair-key mode (matches Python). Other labels must exist.
	if from != "owner" || scope.pairKey == "" {
		return "", fmt.Errorf("sender %q not registered in this scope", from)
	}

	ownerCWD := s.cfg.CWD
	if ownerCWD == "" {
		wd, _ := os.Getwd()
		ownerCWD = wd
	}
	if err := st.RegisterOwner(ctx, sqlitestore.RegisterOwnerParams{
		CWD:     ownerCWD,
		PairKey: scope.pairKey,
	}); err != nil {
		return "", fmt.Errorf("%w: %v", errSenderAutoRegisterFailed, err)
	}
	// Scrub stale channel_socket — owner is human and has no channel
	// listener. RegisterOwner already nulls it, but if a prior row came
	// from Python's register (which walks the process tree and may bind
	// an ancestor socket), ClearChannelSocket is a safety net.
	_ = st.ClearChannelSocket(ctx, ownerCWD, "owner")
	return ownerCWD, nil
}

// extractBearer parses `Authorization: Bearer <token>`. Case-insensitive
// on the scheme. Returns "" if absent or malformed.
func extractBearer(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) {
		return ""
	}
	if strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func trimLabel(s string) string {
	return strings.TrimSpace(s)
}
