// Package server wires the peer-web HTTP handlers. Split from main so
// tests can construct a Server directly without shelling through
// main.run. API handlers live in sibling files (scope.go, ...); data
// handlers follow in later commits.
package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Config is the flag-derived server configuration. Port is the bind
// port; the rest are scope hints (see main.go usage). Zero values mean
// "no hint" — server is multi-room.
type Config struct {
	Port        int
	Bind        string // bind address; "" → "127.0.0.1"
	PairKey     string
	OnlyPairKey string
	CWD         string
	AsLabel     string
}

// Server owns the http.Server + routing. New does not bind; Serve does.
type Server struct {
	cfg    Config
	mux    *http.ServeMux
	http   *http.Server
	static fs.FS

	// Per-pair_key standing drainer goroutines (Phase 1). drainersMu
	// guards drainers; the drainer struct itself has its own mutex for
	// snapshot-vs-mutation. serveCtx is the parent context drainers bind
	// to, set when Serve starts; cancelled on shutdown.
	drainersMu sync.Mutex
	drainers   map[string]*drainer
	serveCtx   context.Context
}

//go:embed static/*
var staticFS embed.FS

// New builds a configured but unbound Server. Returns an error if the
// embedded static assets cannot be prepared (should not happen at
// runtime — catches build-time embed typos early).
func New(cfg Config) (*Server, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("prepare static fs: %w", err)
	}
	// Default cfg.CWD to process cwd so the data-endpoint response field
	// matches Python (which always echoes str(resolve_cwd(args.cwd))).
	if cfg.CWD == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.CWD = wd
		}
	}
	s := &Server{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		static: sub,
	}
	s.registerRoutes()
	bind := cfg.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	s.http = &http.Server{
		Addr:              net.JoinHostPort(bind, itoa(cfg.Port)),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Serve blocks until ctx is cancelled or ListenAndServe errors. Returns
// nil on clean shutdown (ctx.Done), or the underlying error otherwise.
func (s *Server) Serve(ctx context.Context) error {
	s.serveCtx = ctx
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()

	// v3.8: start the stale-active watchdog. Runs for the server's
	// lifetime; cancels cleanly when ctx is cancelled.
	go s.runStaleActiveWatchdog(ctx)

	// v3.11: reap orphaned card_runs from the previous peer-web run,
	// then reconstruct standing drainers for boards with auto_drain=1.
	// Best-effort — failures log but don't prevent server start.
	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	if n, err := s.reapOrphanedRuns(bootCtx); err != nil {
		fmt.Fprintf(os.Stderr, "peer-web boot: reap orphaned runs: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "peer-web boot: reaped %d orphaned card_runs\n", n)
	}
	if n, err := s.reconstructAutoDrainBoards(bootCtx); err != nil {
		fmt.Fprintf(os.Stderr, "peer-web boot: reconstruct auto-drain boards: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "peer-web boot: started %d auto-drain board drainers\n", n)
	}
	bootCancel()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Shutdown stops the HTTP server gracefully within the ctx deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/api/scope", s.handleScope)
	s.mux.HandleFunc("/api/pairs", s.handlePairs)
	s.mux.HandleFunc("/api/rooms", s.handleRooms)
	s.mux.HandleFunc("/api/messages", s.handleMessages)
	s.mux.HandleFunc("/api/index", s.handleIndex)
	s.mux.HandleFunc("/api/send", s.handleSend)
	s.mux.HandleFunc("/api/channel-push", s.handleChannelPush)
	s.mux.HandleFunc("/api/rooms/terminate-inactive", s.handleTerminateInactive)
	s.mux.HandleFunc("/api/cards", s.handleCardsRoot)
	s.mux.HandleFunc("/api/cards/", s.handleCardSubpath)
	s.mux.HandleFunc("/api/boards", s.handleBoards)
	s.mux.HandleFunc("/api/boards/", s.handleBoardSubpath)
	s.mux.HandleFunc("/view", s.handleView)
	s.mux.HandleFunc("/cards", s.handleCardsView)
	// Root serves the multi-room index. Anything else falls through to
	// the embedded static FS (/ → index.html, /favicon.ico if added,
	// etc). We intercept / explicitly so the index bypasses the file-
	// server's default-index behavior and we can control caching.
	s.mux.HandleFunc("/", s.handleRoot)
}

func itoa(n int) string {
	// small helper to avoid importing strconv in a one-line use
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
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
