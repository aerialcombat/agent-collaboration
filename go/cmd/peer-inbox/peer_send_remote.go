package main

import (
	"context"
	"fmt"
	"os"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// remoteSendArgs carries the inputs to runPeerSendRemote. Pulled into
// a struct so peer_send.go's call site stays readable.
type remoteSendArgs struct {
	pairKey   string
	homeHost  string
	messageID string
	toLabel   string
	body      string
	selfLabel string
	jsonOut   bool
}

// runPeerSendRemote routes a peer-send to a remote host's peer-web
// /api/send. Mirrors Python _peer_send_remote (peer-inbox-db.py:1039).
//
// Flush-before-send: attempts to replay any previously-queued rows
// for this host first, preserving FIFO ordering. On success, the
// current message is POSTed. On network failure, the message joins
// the queue (idempotent on message_id) and exit is 0 so callers
// don't hang — matches Python's "queued for host ..." behavior.
//
// Misconfigured host (not in remotes.json) is EXIT_CONFIG_ERROR —
// operator needs to add the entry. This path never auto-queues
// because a missing host config means the send has no target URL.
func runPeerSendRemote(ctx context.Context, st *sqlitestore.SQLiteLocal, a remoteSendArgs) int {
	remotes, err := loadRemotes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load remotes: %v\n", err)
		return exitConfigError
	}
	cfg, ok := remotes[a.homeHost]
	if !ok {
		fmt.Fprintf(os.Stderr,
			"error: room %s is home=%q but no remote config found. Add an "+
				"entry to ~/.agent-collab/remotes.json:\n"+
				"  {%q: {\"base_url\": \"https://...\", \"auth_token_env\": \"AGENT_COLLAB_TOKEN_%s\"}}\n",
			a.pairKey, a.homeHost, a.homeHost, envHostTag(a.homeHost))
		return exitConfigError
	}

	flushed, err := flushOutboundQueue(ctx, st, a.homeHost)
	if err != nil {
		// Queue flush is best-effort — don't fail the send if the queue
		// probe errored. Python prints a warning and continues.
		fmt.Fprintf(os.Stderr, "warning: queue flush failed: %v\n", err)
	}

	reachable, parsed, errStr := remoteSendHTTP(
		cfg, a.pairKey, a.selfLabel, a.toLabel, a.body, a.messageID,
	)

	if !reachable {
		enqueueErr := errStr
		if enqueueErr == "" {
			enqueueErr = "unknown network error"
		}
		if qerr := st.EnqueueOutbound(ctx, a.messageID, a.homeHost, a.pairKey,
			a.selfLabel, a.toLabel, a.body, enqueueErr); qerr != nil {
			fmt.Fprintf(os.Stderr, "error: enqueue: %v\n", qerr)
			return exitInternal
		}
		depth, _ := st.ListPendingOutbound(ctx, a.homeHost)
		queueDepth := len(depth)

		if a.jsonOut {
			payload := map[string]any{
				"ok":          true,
				"to":          a.toLabel,
				"message_id":  a.messageID,
				"server_seq":  0,
				"push_status": "queued",
				"dedup_hit":   false,
				"queued":      true,
				"queue_depth": queueDepth,
				"routed_via":  a.homeHost,
				"error":       enqueueErr,
			}
			if err := writePyCompatJSON(payload); err != nil {
				fmt.Fprintf(os.Stderr, "error: encode json: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr,
				"queued for %s (depth=%d): %s. Retries on next send; "+
					"manual flush: agent-collab peer queue --flush\n",
				a.homeHost, queueDepth, enqueueErr)
		}
		return exitOK
	}

	status, _ := parsed["_http_status"].(float64)
	okVal, okPresent := parsed["ok"]
	httpOK := int(status) == 200
	if okPresent {
		if b, _ := okVal.(bool); !b {
			httpOK = false
		}
	}

	// Pull server-side fields with safe defaults so a malformed response
	// still returns a usable shape.
	serverSeq, _ := parsed["server_seq"].(float64)
	pushStatus, _ := parsed["push_status"].(string)
	if pushStatus == "" {
		pushStatus = "remote"
	}
	dedupHit, _ := parsed["dedup_hit"].(bool)
	parsedMsgID, _ := parsed["message_id"].(string)
	if parsedMsgID == "" {
		parsedMsgID = a.messageID
	}

	if a.jsonOut {
		payload := map[string]any{
			"ok":          httpOK,
			"to":          a.toLabel,
			"message_id":  parsedMsgID,
			"server_seq":  int64(serverSeq),
			"push_status": pushStatus,
			"dedup_hit":   dedupHit,
			"terminates":  false,
			"mentions":    []string{},
			"routed_via":  a.homeHost,
		}
		if flushed.Flushed > 0 {
			payload["queue_flushed"] = flushed.Flushed
		}
		if flushed.DroppedTTL > 0 {
			payload["queue_dropped_ttl"] = flushed.DroppedTTL
		}
		if err := writePyCompatJSON(payload); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode json: %v\n", err)
		}
	} else {
		seqStr := "?"
		if serverSeq > 0 {
			seqStr = fmt.Sprintf("%d", int64(serverSeq))
		}
		dedup := ""
		if dedupHit {
			dedup = " [deduped]"
		}
		flushNote := ""
		if flushed.Flushed > 0 {
			flushNote = fmt.Sprintf(" [queue flushed %d]", flushed.Flushed)
		}
		fmt.Printf("sent to %s via %s (seq=%s)%s%s\n",
			a.toLabel, a.homeHost, seqStr, dedup, flushNote)
	}
	if httpOK {
		return exitOK
	}
	return exitPeerOffline
}

// envHostTag uppercases a host label for the AGENT_COLLAB_TOKEN_<HOST>
// hint printed in the missing-remote-config error. Uppercases in-place
// (ASCII only); '-' stays as-is so hyphenated hostnames format cleanly.
func envHostTag(h string) string {
	b := []byte(h)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

