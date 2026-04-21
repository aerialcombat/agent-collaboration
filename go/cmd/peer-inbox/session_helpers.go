package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// recentSeenSessions mirrors peer-inbox-db.py:recent_seen_sessions.
// Reads ~/.agent-collab/claude-sessions-seen/<cwd-hash>.log (newest
// entries at EOF, tab-separated "<timestamp>\t<session_id>") and
// returns up to `limit` distinct session_ids, newest first.
//
// Used as a fallback by session-register / session-close when no
// explicit session-key was passed and env discovery returned empty.
// Best-effort: missing log or parse errors return [].
func recentSeenSessions(cwd string, limit int) []string {
	if limit <= 0 {
		limit = 20
	}
	path := cwdSeenLogPath(cwd)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	// Log lines are short (ISO ts + UUID), but raise the default buffer
	// cap defensively in case an operator appends longer session ids.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil
	}

	seen := map[string]bool{}
	out := make([]string, 0, limit)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		sid := strings.TrimSpace(parts[1])
		if sid == "" || seen[sid] {
			continue
		}
		seen[sid] = true
		out = append(out, sid)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// cwdSeenLogPath mirrors Python's cwd_log_path — sha256(cwd)[:16]
// filename under the claude-sessions-seen dir.
func cwdSeenLogPath(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cwd))
	name := hex.EncodeToString(sum[:])[:16] + ".log"
	return filepath.Join(home, ".agent-collab", "claude-sessions-seen", name)
}

// sweepStaleMarkersForLabel mirrors
// peer-inbox-db.py:sweep_stale_markers_for_label. After a successful
// register, removes other marker files in <cwd>/.agent-collab/sessions
// that claim the same label (keyed on a different session_key hash).
// Prevents ResolveSelf's single-marker convenience from seeing ghosts.
//
// Best-effort: unreadable/unparseable markers are skipped, never
// deleted. Removal failures are ignored (next register retries).
func sweepStaleMarkersForLabel(cwd, label, keepSessionKey string) {
	dir := filepath.Join(cwd, ".agent-collab", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keepSum := sha256.Sum256([]byte(keepSessionKey))
	keepName := hex.EncodeToString(keepSum[:])[:16] + ".json"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Name() == keepName {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data struct {
			Label string `json:"label"`
		}
		if jerr := json.Unmarshal(raw, &data); jerr != nil {
			continue
		}
		if data.Label == label {
			_ = os.Remove(path)
		}
	}
}

