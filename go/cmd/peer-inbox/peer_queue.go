package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// outboundQueueTTLSecs mirrors Python's OUTBOUND_QUEUE_TTL_SECS.
const outboundQueueTTLSecs = 24 * 60 * 60 // 24 hours

// runPeerQueue ports cmd_peer_queue (scripts/peer-inbox-db.py line 4555).
// --show lists rows with age/host/room/attempts/last_error; --flush
// attempts to replay every row for the given host. Default (neither
// flag) implies --show. Byte-parity with Python is required.
func runPeerQueue(args []string) int {
	fs := flag.NewFlagSet("peer-queue", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		show     = fs.Bool("show", false, "list queued outbound sends")
		flush    = fs.Bool("flush", false, "attempt to replay every queued send")
		homeHost = fs.String("home-host", "",
			"scope --show or --flush to a single remote host label")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !*show && !*flush {
		*show = true
	}
	if *homeHost != "" {
		if err := sqlitestore.ValidateHostLabel(*homeHost); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 3 // EXIT_VALIDATION
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-queue: open store: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	if *flush {
		stats, err := flushOutboundQueue(ctx, st, *homeHost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-queue: flush: %v\n", err)
			return exitInternal
		}
		// Python: f"flush: flushed={...} dropped_ttl={...} still_failing={...}"
		fmt.Printf("flush: flushed=%d dropped_ttl=%d still_failing=%d\n",
			stats.Flushed, stats.DroppedTTL, stats.StillFailing)
	}

	if *show {
		rows, err := st.ListPendingOutbound(ctx, *homeHost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer-queue: %v\n", err)
			return exitInternal
		}
		if len(rows) == 0 {
			fmt.Println("(no queued sends)")
			return exitOK
		}
		// Header: Python emits
		//   f"{'ID':>4} {'AGE':>6} {'HOST':<12} {'ROOM':<24} "
		//   f"{'FROM→TO':<24} {'TRIES':>5}  LAST_ERROR"
		fmt.Printf("%4s %6s %-12s %-24s %-24s %5s  LAST_ERROR\n",
			"ID", "AGE", "HOST", "ROOM", "FROM→TO", "TRIES")
		now := time.Now().UTC()
		for _, r := range rows {
			age := int(secondsSince(now, r.CreatedAt))
			ft := fmt.Sprintf("%s→%s", r.FromLabel, r.ToLabel)
			lastErr := r.LastError
			if len(lastErr) > 40 {
				lastErr = lastErr[:40]
			}
			// Python: f"{r['id']:>4} {age:>5}s {r['home_host']:<12} ..."
			// Note the ">5" for age then literal "s" (so total width 6 via "%5ds").
			fmt.Printf("%4d %5ds %-12s %-24s %-24s %5d  %s\n",
				r.ID, age, r.HomeHost, r.PairKey, ft, r.Attempts, lastErr)
		}
	}
	return exitOK
}

// secondsSince returns wallclock seconds between two RFC3339-ish UTC
// timestamps. Tolerates Python's "YYYY-MM-DDTHH:MM:SSZ" without a zone
// offset the way `parse_iso` does. Returns 0 on parse failure — better
// than crashing a status-display path on a malformed column.
func secondsSince(now time.Time, ts string) float64 {
	t, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0
		}
	}
	return now.Sub(t).Seconds()
}

// flushStats carries the counters _flush_outbound_queue returns.
type flushStats struct {
	Flushed      int
	DroppedTTL   int
	StillFailing int
}

// flushOutboundQueue ports _flush_outbound_queue. FIFO per
// (home_host, pair_key); rows older than outboundQueueTTLSecs drop
// with a stderr warning. Hosts missing from remotes.json are skipped
// (counted as still_failing, no attempts bump). On the first
// per-host failure we stop iterating that host to preserve FIFO.
func flushOutboundQueue(ctx context.Context, st *sqlitestore.SQLiteLocal, homeHost string) (flushStats, error) {
	stats := flushStats{}
	remotes, err := loadRemotes()
	if err != nil {
		return stats, err
	}
	rows, err := st.ListPendingOutbound(ctx, homeHost)
	if err != nil {
		return stats, err
	}

	now := time.Now().UTC()
	stalledHosts := map[string]bool{}
	for _, r := range rows {
		// Preserve FIFO within a host: once a host fails, skip its tail.
		if stalledHosts[r.HomeHost] {
			continue
		}
		age := secondsSince(now, r.CreatedAt)
		if age > outboundQueueTTLSecs {
			if err := st.DropPendingOutbound(ctx, r.ID); err != nil {
				return stats, err
			}
			stats.DroppedTTL++
			fmt.Fprintf(os.Stderr,
				"dropped queued send %s (age=%ds > %ds TTL, host=%s, room=%s)\n",
				r.MessageID, int(age), outboundQueueTTLSecs, r.HomeHost, r.PairKey)
			continue
		}

		cfg, ok := remotes[r.HomeHost]
		if !ok {
			stats.StillFailing++
			stalledHosts[r.HomeHost] = true
			continue
		}

		reachable, parsed, errStr := remoteSendHTTP(
			cfg, r.PairKey, r.FromLabel, r.ToLabel, r.Body, r.MessageID,
		)
		httpOK := false
		if reachable && parsed != nil {
			status, _ := parsed["_http_status"].(float64)
			if status == 200 {
				// Python: parsed.get("ok", True) — absent → True.
				okVal, present := parsed["ok"]
				if !present {
					httpOK = true
				} else if b, _ := okVal.(bool); b {
					httpOK = true
				}
			}
		}
		if httpOK {
			if err := st.DropPendingOutbound(ctx, r.ID); err != nil {
				return stats, err
			}
			stats.Flushed++
			continue
		}

		stats.StillFailing++
		bumpErr := errStr
		if bumpErr == "" && parsed != nil {
			if status, ok := parsed["_http_status"].(float64); ok {
				bumpErr = fmt.Sprintf("http %d", int(status))
			} else {
				bumpErr = "http ?"
			}
		}
		if bumpErr == "" {
			bumpErr = "http ?"
		}
		if err := st.BumpPendingOutboundAttempt(ctx, r.ID, bumpErr); err != nil {
			return stats, err
		}
		stalledHosts[r.HomeHost] = true
	}
	return stats, nil
}

// remoteCfg mirrors the Python load_remotes() output entry.
type remoteCfg struct {
	BaseURL      string `json:"base_url"`
	AuthTokenEnv string `json:"auth_token_env"`
	AuthToken    string `json:"auth_token"`
}

// loadRemotes mirrors Python's load_remotes. AGENT_COLLAB_REMOTES env
// wins over ~/.agent-collab/remotes.json. Missing file → empty map.
// Parse errors surface as errors so the caller can exit non-zero.
func loadRemotes() (map[string]remoteCfg, error) {
	path := os.Getenv("AGENT_COLLAB_REMOTES")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return map[string]remoteCfg{}, nil
		}
		path = filepath.Join(home, ".agent-collab", "remotes.json")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]remoteCfg{}, nil
		}
		return nil, fmt.Errorf("open remotes config: %w", err)
	}
	defer f.Close()
	var raw map[string]map[string]any
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse remotes config: %w", err)
	}
	out := make(map[string]remoteCfg, len(raw))
	for host, cfg := range raw {
		base := ""
		if v, ok := cfg["base_url"].(string); ok {
			base = strings.TrimRight(v, "/")
		}
		if base == "" {
			return nil, fmt.Errorf("remotes[%q] missing base_url", host)
		}
		env := ""
		if v, ok := cfg["auth_token_env"].(string); ok {
			env = v
		}
		tok := ""
		if v, ok := cfg["auth_token"].(string); ok {
			tok = v
		}
		out[host] = remoteCfg{BaseURL: base, AuthTokenEnv: env, AuthToken: tok}
	}
	return out, nil
}

// resolveRemoteAuthToken mirrors Python's resolve_remote_auth_token:
// env-var name wins over inline.
func resolveRemoteAuthToken(c remoteCfg) string {
	if c.AuthTokenEnv != "" {
		if v := os.Getenv(c.AuthTokenEnv); v != "" {
			return v
		}
	}
	return c.AuthToken
}

// remoteSendHTTP posts a federation /api/send. Matches Python's
// _remote_send_http: returns (reachable, parsed, error_string).
// reachable=true means we got any HTTP status back; false is a
// network-level failure that queues the row.
func remoteSendHTTP(
	cfg remoteCfg,
	pairKey, fromLabel, toLabel, body, messageID string,
) (bool, map[string]any, string) {
	url := cfg.BaseURL + "/api/send?pair_key=" + pairKey
	payload := map[string]string{
		"from":       fromLabel,
		"to":         toLabel,
		"body":       body,
		"message_id": messageID,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return false, nil, err.Error()
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(buf))
	if err != nil {
		return false, nil, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := resolveRemoteAuthToken(cfg); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil, err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, nil, err.Error()
	}
	parsed := map[string]any{}
	if len(raw) > 0 {
		if jerr := json.Unmarshal(raw, &parsed); jerr != nil {
			parsed = map[string]any{"raw": string(raw)}
		}
	}
	parsed["_http_status"] = float64(resp.StatusCode)
	return true, parsed, ""
}
