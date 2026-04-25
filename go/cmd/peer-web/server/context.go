package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// Context resolution turns a card's context_refs into the actual blob
// of knowledge a worker (or human) needs to do the work. It's used in
// two places:
//
//   - GET /api/cards/{id}/context — drawer "Context" panel.
//   - The Shape B spawn orchestrator (later) — same resolved bundle is
//     handed to a fresh `claude -p` worker as initial prompt material.
//
// Both surfaces want the same result, so resolution lives in one place.

// Per-resource caps to keep resolved context bounded.
const (
	maxFileBytes    = 200 * 1024  // 200 KB per file
	maxURLBytes     = 200 * 1024  // 200 KB per URL
	totalContextCap = 2 * 1024 * 1024 // 2 MB total across the bundle
	urlFetchTimeout = 8 * time.Second
)

// contextRefsShape mirrors what callers stash in cards.context_refs as
// JSON. All fields optional — an empty refs object is valid.
type contextRefsShape struct {
	MsgIDs []int64  `json:"msg_ids"`
	Files  []string `json:"files"`
	URLs   []string `json:"urls"`
	Cards  []int64  `json:"cards"`
}

// resolvedFile / resolvedURL / resolvedMsg / resolvedCard are the
// per-ref output rows. `Error` is non-empty on failure (and Content is
// then empty); the bundle never silently drops a ref — failures are
// surfaced so the human/agent knows the context is incomplete.
type resolvedFile struct {
	Path      string `json:"path"`
	Content   string `json:"content,omitempty"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}
type resolvedURL struct {
	URL         string `json:"url"`
	Content     string `json:"content,omitempty"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"content_type,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Error       string `json:"error,omitempty"`
}
type resolvedMsg struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Error     string `json:"error,omitempty"`
}
type resolvedCard struct {
	ID        int64  `json:"id"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	Body      string `json:"body,omitempty"`
	NeedsRole string `json:"needs_role,omitempty"`
	Error     string `json:"error,omitempty"`
}

// resolveContext does the four kinds of resolution and returns the
// bundle. Soft-fails per ref (each row carries its own `error`) but
// returns hard errors only for store-open failures.
func resolveContext(
	ctx context.Context,
	st webStore,
	refs contextRefsShape,
) map[string]any {
	out := map[string]any{
		"files":   []any{},
		"urls":    []any{},
		"msg_ids": []any{},
		"cards":   []any{},
		"warnings": []string{},
	}
	totalBytes := 0
	cap := func(n int) int {
		if totalBytes+n > totalContextCap {
			return totalContextCap - totalBytes
		}
		return n
	}

	// Files — read from local FS, cap at maxFileBytes.
	files := []resolvedFile{}
	for _, raw := range refs.Files {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		// Reject paths that look like an attempt to escape into sensitive
		// system dirs. This is a single-user dev tool, not a hardened
		// service, but cheap guardrails are still worth it.
		clean := filepath.Clean(path)
		if !filepath.IsAbs(clean) {
			files = append(files, resolvedFile{Path: path,
				Error: "only absolute paths are supported"})
			continue
		}
		for _, banned := range []string{"/etc/", "/proc/", "/sys/", "/dev/"} {
			if strings.HasPrefix(clean+"/", banned) {
				files = append(files, resolvedFile{Path: path,
					Error: "path is in a restricted directory"})
				goto nextFile
			}
		}
		{
			info, err := os.Stat(clean)
			if err != nil {
				files = append(files, resolvedFile{Path: path,
					Error: "stat: " + err.Error()})
				goto nextFile
			}
			if info.IsDir() {
				files = append(files, resolvedFile{Path: path,
					Error: "path is a directory, not a file"})
				goto nextFile
			}
			f, err := os.Open(clean)
			if err != nil {
				files = append(files, resolvedFile{Path: path,
					Error: "open: " + err.Error()})
				goto nextFile
			}
			limit := cap(maxFileBytes)
			if limit <= 0 {
				files = append(files, resolvedFile{Path: path,
					Error: "context cap reached, ref skipped"})
				_ = f.Close()
				goto nextFile
			}
			buf, err := io.ReadAll(io.LimitReader(f, int64(limit)))
			_ = f.Close()
			if err != nil {
				files = append(files, resolvedFile{Path: path,
					Error: "read: " + err.Error()})
				goto nextFile
			}
			truncated := info.Size() > int64(len(buf))
			totalBytes += len(buf)
			files = append(files, resolvedFile{
				Path: path, Content: string(buf),
				Bytes: int(info.Size()), Truncated: truncated,
			})
		}
	nextFile:
	}
	out["files"] = files

	// URLs — basic GET, cap at maxURLBytes. Keep the timeout tight.
	urls := []resolvedURL{}
	client := &http.Client{Timeout: urlFetchTimeout}
	for _, u := range refs.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			urls = append(urls, resolvedURL{URL: u, Error: "build req: " + err.Error()})
			continue
		}
		req.Header.Set("User-Agent", "peer-inbox-context-resolver/1.0")
		resp, err := client.Do(req)
		if err != nil {
			urls = append(urls, resolvedURL{URL: u, Error: "fetch: " + err.Error()})
			continue
		}
		ct := resp.Header.Get("Content-Type")
		limit := cap(maxURLBytes)
		if limit <= 0 {
			urls = append(urls, resolvedURL{URL: u,
				ContentType: ct,
				Error:       "context cap reached, ref skipped"})
			_ = resp.Body.Close()
			continue
		}
		buf, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
		_ = resp.Body.Close()
		if err != nil {
			urls = append(urls, resolvedURL{URL: u, ContentType: ct,
				Error: "read: " + err.Error()})
			continue
		}
		var truncated bool
		if cl := resp.ContentLength; cl > 0 && cl > int64(len(buf)) {
			truncated = true
		}
		totalBytes += len(buf)
		status := resp.StatusCode
		var errMsg string
		if status >= 400 {
			errMsg = fmt.Sprintf("HTTP %d", status)
		}
		urls = append(urls, resolvedURL{
			URL: u, Content: string(buf), Bytes: len(buf),
			ContentType: ct, Truncated: truncated, Error: errMsg,
		})
	}
	out["urls"] = urls

	// Inbox messages — replay msg_ids as conversation history.
	msgs := []resolvedMsg{}
	if len(refs.MsgIDs) > 0 {
		rows, err := st.MessagesByIDs(ctx, refs.MsgIDs)
		if err != nil {
			out["warnings"] = append(out["warnings"].([]string),
				"MessagesByIDs: "+err.Error())
		} else {
			byID := make(map[int64]sqlitestore.WebMessage, len(rows))
			for _, m := range rows {
				byID[m.ID] = m
			}
			for _, id := range refs.MsgIDs {
				if m, ok := byID[id]; ok {
					msgs = append(msgs, resolvedMsg{
						ID: m.ID, From: m.From, To: m.To,
						Body: m.Body, CreatedAt: m.CreatedAt,
					})
				} else {
					msgs = append(msgs, resolvedMsg{
						ID: id, Error: "not found",
					})
				}
			}
		}
	}
	out["msg_ids"] = msgs

	// Predecessor cards — recursive in spirit but not literally; we only
	// surface direct refs (one hop). Going deeper invites context-bomb
	// scenarios; the human can chain refs explicitly if needed.
	cards := []resolvedCard{}
	for _, id := range refs.Cards {
		c, err := st.GetCard(ctx, id)
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			cards = append(cards, resolvedCard{ID: id, Error: "card not found"})
			continue
		}
		if err != nil {
			cards = append(cards, resolvedCard{ID: id, Error: err.Error()})
			continue
		}
		cards = append(cards, resolvedCard{
			ID: c.ID, Title: c.Title, Status: c.Status,
			Body: c.Body, NeedsRole: c.NeedsRole,
		})
	}
	out["cards"] = cards
	out["total_bytes"] = totalBytes
	return out
}

// handleCardContext — GET /api/cards/{id}/context
//
// Resolves the card's context_refs (files, urls, msg_ids, cards) into
// the actual content. Returns an empty bundle (with all four buckets
// as empty arrays) when the card has no refs — never errors on absence.
//
// Soft-fails per ref: each resolved row may carry its own `error`
// field. Hard errors (store unavailable, card not found) bubble as 4xx/5xx.
func (s *Server) handleCardContext(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	card, err := st.GetCard(ctx, id)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		writeJSONError(w, http.StatusNotFound, "card not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	refs := contextRefsShape{}
	if card.ContextRefs != "" {
		if err := json.Unmarshal([]byte(card.ContextRefs), &refs); err != nil {
			writeJSONError(w, http.StatusInternalServerError,
				"context_refs is not valid JSON: "+err.Error())
			return
		}
	}

	resolved := resolveContext(ctx, st, refs)
	resolved["card_id"] = id
	resolved["pair_key"] = card.PairKey
	writeJSON(w, http.StatusOK, resolved)
}
