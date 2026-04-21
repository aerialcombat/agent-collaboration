package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// findPendingChannelSocket walks this process's parent chain looking
// for a pending-channel registration file written by the channel MCP
// server. Claude Code spawns the channel server at session start and
// it writes ~/.agent-collab/pending-channels/<claude-pid>.json. This
// process (invoked via Claude's Bash tool) is a descendant, so walking
// up our PID chain crosses Claude's PID.
//
// Returns the resolved socket_path when a candidate file exists AND
// the referenced socket exists on disk; otherwise "".
//
// Best-effort: any fs, json, or ps error collapses to "". Mirrors
// peer-inbox-db.py:find_pending_channel_for_self.
func findPendingChannelSocket() string {
	dir := pendingChannelsDir()
	if dir == "" {
		return ""
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	pid := os.Getpid()
	for i := 0; i < 12; i++ {
		if pid <= 1 {
			break
		}
		path := filepath.Join(dir, strconv.Itoa(pid)+".json")
		if b, err := os.ReadFile(path); err == nil {
			var data struct {
				SocketPath string `json:"socket_path"`
			}
			if jerr := json.Unmarshal(b, &data); jerr != nil {
				return ""
			}
			if data.SocketPath == "" {
				return ""
			}
			if _, serr := os.Stat(data.SocketPath); serr != nil {
				return ""
			}
			return data.SocketPath
		}
		pid = ppidOf(pid)
	}
	return ""
}

func pendingChannelsDir() string {
	if v := os.Getenv("PEER_INBOX_PENDING_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agent-collab", "pending-channels")
}

// ppidOf shells out to `ps -o ppid= -p <pid>`. Sandbox blocks (e.g.
// codex exec's Seatbelt) or missing `ps` return 0, which ends the walk
// cleanly. Channel pairing is best-effort — never crash register.
func ppidOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
