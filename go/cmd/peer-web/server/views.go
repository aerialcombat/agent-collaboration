package server

import (
	"html"
	"io"
	"net/http"
	"strings"
)

// handleRoot serves the multi-room index page at /. Reads index.html
// from the embedded static FS; it's a pure client-side SPA that fetches
// /api/index so no server-side templating is needed.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// Only serve the index at the literal root. Other unmatched paths
	// bubble up to 404 so users hitting typos don't land on the index.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.serveStatic(w, r, "index.html", "")
}

// handleView serves the Slack-shaped room detail SPA at /view. Expects
// ?scope=pair_key&key=K or ?scope=cwd&path=P; the SPA parses its own
// scope from the query string, so the server only has to stamp the
// page title + sidebar-header banner and let the client drive.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	var title, banner string
	switch scope {
	case "pair_key":
		title = "peer-inbox — pair " + q.Get("key")
		banner = title
	case "cwd":
		title = "peer-inbox — cwd " + q.Get("path")
		banner = title
	default:
		title = "peer-inbox — (no scope)"
		banner = "pass ?scope=pair_key&key=K or ?scope=cwd&path=P"
	}
	s.serveStatic(w, r, "view.html", titleVars(title, banner))
}

// serveStatic reads a file from the embedded FS and writes it with
// optional __PLACEHOLDER__ substitution. vars is pre-escaped HTML.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, name string, substitutions string) {
	f, err := s.static.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	out := string(body)
	// Python template uses __TITLE__ and __CWD__. Apply both even if
	// only one was populated — unknown placeholders fall through to
	// something sensible.
	if substitutions != "" {
		out = applySubs(out, substitutions)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, out)
}

// titleVars returns a substitution string encoding __TITLE__ and
// __CWD__ values. Format: "__TITLE__\x00<t>\x01__CWD__\x00<c>". Sole
// producer/consumer so the format is private.
func titleVars(title, banner string) string {
	return "__TITLE__\x00" + html.EscapeString(title) +
		"\x01__CWD__\x00" + html.EscapeString(banner)
}

func applySubs(in, substitutions string) string {
	parts := strings.Split(substitutions, "\x01")
	for _, p := range parts {
		kv := strings.SplitN(p, "\x00", 2)
		if len(kv) != 2 {
			continue
		}
		in = strings.ReplaceAll(in, kv[0], kv[1])
	}
	return in
}
