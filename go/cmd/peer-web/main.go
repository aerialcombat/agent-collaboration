// Command peer-web — Go port of the Python peer-inbox web dashboard
// (scripts/peer-inbox-db.py cmd_peer_web). Serves a Slack-shaped live
// view of peer-inbox traffic backed by the shared
// ~/.agent-collab/sessions.db.
//
// Scoping differs from Python: the Go server is multi-room by default.
// --pair-key K is a deep-link shortcut; --only-pair-key K locks the
// server to one room (safety mitigation for operators wanting narrow
// localhost exposure per the v3.2-frontend-go-rewrite scoping doc).
//
// Status: skeleton. /api/scope implemented; /api/pairs, /api/rooms,
// /api/messages, POST /api/send land in later commits.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-collaboration/go/cmd/peer-web/server"
)

const (
	exitOK       = 0
	exitUsage    = 64
	exitInternal = 1
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("peer-web", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usageText)
	}

	var (
		port        = fs.Int("port", 8787, "HTTP port to bind (default 8787)")
		bind        = fs.String("bind", "127.0.0.1", "bind address; use 0.0.0.0 or a specific iface IP for cross-machine federation")
		pairKey     = fs.String("pair-key", "", "deep-link to a specific room on first visit; server stays multi-room")
		onlyPairKey = fs.String("only-pair-key", "", "lock server to a single pair_key; reject requests for other scopes")
		cwdFlag     = fs.String("cwd", "", "cwd-mode deep-link (optional)")
		asLabel     = fs.String("as", "", "viewer label hint (optional)")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *pairKey != "" && *onlyPairKey != "" {
		fmt.Fprintln(os.Stderr, "peer-web: --pair-key and --only-pair-key are mutually exclusive")
		return exitUsage
	}

	cfg := server.Config{
		Port:        *port,
		Bind:        *bind,
		PairKey:     *pairKey,
		OnlyPairKey: *onlyPairKey,
		CWD:         *cwdFlag,
		AsLabel:     *asLabel,
	}

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-web: new server: %v\n", err)
		return exitInternal
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr,
		"peer-web (go): http://%s:%d  (Ctrl-C to stop)\n", *bind, *port)
	if *bind == "0.0.0.0" || (*bind != "" && *bind != "127.0.0.1" && *bind != "localhost") {
		fmt.Fprintln(os.Stderr,
			"peer-web: WARNING — bound to a non-loopback address. "+
				"Traffic is plain HTTP; only expose over a trusted network "+
				"(LAN, SSH tunnel, VPN, or Tailscale) until TLS lands.")
	}

	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "peer-web: serve: %v\n", err)
		return exitInternal
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "peer-web: shutdown: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "(stopped)")
	return exitOK
}

const usageText = `usage: peer-web [flags]

  --port N              bind port (default 8787)
  --bind ADDR           bind address (default 127.0.0.1; use 0.0.0.0 or an
                        interface IP to accept federation traffic from peers
                        on other machines — requires a trusted network
                        since the server speaks plain HTTP only)
  --pair-key KEY        deep-link to a pair-key room on first visit
  --only-pair-key KEY   lock server to one pair-key room (rejects other scopes)
  --cwd PATH            cwd-mode deep-link
  --as LABEL            viewer label hint

Serves http://<bind>:PORT with a multi-room index + per-room detail.
Go port of the Python cmd_peer_web; reads the shared ~/.agent-collab/
sessions.db. Python version stays as fallback; dispatch via
AGENT_COLLAB_WEB_IMPL=python forces the Python path.
`
