package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// handleSend wires the composer send path. Matches the Python
// cmd_peer_web /send handler at scripts/peer-inbox-db.py byte-for-
// byte on happy paths:
//   1. Parse POST body {from, to, body}.
//   2. Resolve sender's cwd from the sessions table (by pair_key+label
//      in pair-key mode, cwd+label in cwd mode).
//   3. Auto-register "owner" on first send in pair-key mode (the human
//      in the loop). Other unregistered senders are rejected.
//   4. Shell out to `peer-inbox-db.py peer-send` / `peer-broadcast`.
//      Reusing the Python CLI gives us identical validation, room-cap,
//      termination, and push semantics for free — documented rationale
//      lives at plans/v3.2-frontend-go-rewrite-scoping.md §4 Item 6.
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
		From string `json:"from"`
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	body.From = trimLabel(body.From)
	body.To = trimLabel(body.To)
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

	sendCWD, err := s.resolveSenderCWD(ctx, st, scope, body.From)
	if err != nil {
		code := http.StatusNotFound
		if errors.Is(err, errSenderAutoRegisterFailed) {
			code = http.StatusInternalServerError
		}
		writeJSONError(w, code, err.Error())
		return
	}

	script := s.resolvePeerInboxScript()
	if script == "" {
		writeJSONError(w, http.StatusServiceUnavailable,
			"peer-inbox-db.py not found (checked $AGENT_COLLAB_PEER_INBOX_DB_SCRIPT, script-sibling, ~/.agent-collab/scripts/)")
		return
	}

	verb := "peer-send"
	args := []string{"--cwd", sendCWD, "--as", body.From, "--to", body.To, "--message-stdin"}
	if body.To == "" || body.To == "@room" {
		verb = "peer-broadcast"
		args = []string{"--cwd", sendCWD, "--as", body.From, "--message-stdin"}
	}

	cmd := exec.CommandContext(ctx, "python3", append([]string{script, verb}, args...)...)
	cmd.Stdin = strings.NewReader(body.Body)
	stdout, err := cmd.Output()
	ok := err == nil
	var stderr string
	var exitCode int
	if exitErr, ok2 := err.(*exec.ExitError); ok2 {
		stderr = string(exitErr.Stderr)
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		stderr = err.Error()
		exitCode = -1
	}

	code := http.StatusOK
	if !ok {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]any{
		"ok":     ok,
		"exit":   exitCode,
		"stdout": trimTrailingNewline(string(stdout)),
		"stderr": trimTrailingNewline(stderr),
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
	script := s.resolvePeerInboxScript()
	if script == "" {
		return "", errSenderAutoRegisterFailed
	}
	cmd := exec.CommandContext(ctx, "python3", script, "session-register",
		"--cwd", ownerCWD,
		"--label", "owner",
		"--agent", "human",
		"--role", "owner",
		"--pair-key", scope.pairKey,
		"--session-key", "owner-web-"+scope.pairKey,
		"--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %v: %s", errSenderAutoRegisterFailed, err, string(out))
	}
	// Scrub stale channel_socket — Python's register walks the process
	// tree and may bind an ancestor socket to "owner"; owner is human
	// and has no channel listener. Python does the same via sqlite3
	// directly; we replicate.
	_ = st.ClearChannelSocket(ctx, ownerCWD, "owner")
	return ownerCWD, nil
}

func (s *Server) resolvePeerInboxScript() string {
	if v := os.Getenv("AGENT_COLLAB_PEER_INBOX_DB_SCRIPT"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		// $repo/go/bin/peer-web → $repo/scripts/peer-inbox-db.py
		candidate := filepath.Join(dir, "..", "..", "scripts", "peer-inbox-db.py")
		if _, err := os.Stat(candidate); err == nil {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, ".agent-collab", "scripts", "peer-inbox-db.py")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func trimLabel(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

