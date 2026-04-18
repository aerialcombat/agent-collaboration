// Command daemon — the O4 orchestrator + W3 worker-shell that auto-
// replies to peer-inbox messages on behalf of a managed session label.
// Commit 7 of Topic 3 v0 per plans/v3.x-topic-3-implementation-scope.md.
//
// Shape: one long-running process per managed label (§2.2 blast-radius
// recommendation — config consolidation is a v1+ call). Registers the
// session up-front (externally — daemon does not call `session
// register`; config already has the label + session-key), then enters a
// main loop that atomically claims fresh unread rows, spawns a fresh
// CLI ("claude -p" / "codex exec" / "gemini -p") per batch with the
// envelope text piped as the user prompt, waits for a completion ack
// (mechanism 1 direct peer-inbox daemon-complete, or mechanism 2
// JSONL stdout marker), and marks the batch complete on ack — or
// abandons internally on ack-timeout so the sweeper requeues (§3.4
// (c)/(d) fail-loud semantics).
//
// §2.4 load-bearing: daemon owns envelope delivery; the spawned CLI's
// hook MUST no-op via the §3.4 (f) AGENT_COLLAB_DAEMON_SPAWN=1 env-flag
// short-circuit. The daemon exports both that flag + the CLI-specific
// session-key env var + the neutral AGENT_COLLAB_SESSION_KEY fallback
// into every spawn's environment so ResolveSelf on the child side
// resolves to the same (cwd, label) sessions row.
//
// Four-layer termination stack wire-up (§6):
//   - L1 content-stop sentinel detected in message bodies → complete
//     the batch, flip sessions.daemon_state='closed', go dormant.
//   - L2 envelope.state=closed → no sender-side support in v0; the
//     claim-time daemon_state preflight inside DaemonModeClaim covers
//     the "externally-written closed" case (§3.4 (e)).
//   - L3 quiescence — exp-backoff on rapid same-peer batches,
//     empty-response terminates (no respawn), pause-on-idle lowers
//     poll frequency.
//   - L4 heartbeat — OUT OF SCOPE. Depends on papercut 712121c
//     landing; documented as a TODO below.
//
// Ack mechanisms (§7.2, order of preference):
//  1. PRIMARY — the spawned CLI itself shells out to
//     `peer-inbox daemon-complete --cwd <cwd> --as <label>` as its
//     final tool call. After spawn exits, daemon re-polls the DB via
//     store.DaemonModeComplete; if the in-flight rows are already
//     marked completed, that call returns ErrStaleClaim (benign) and
//     the daemon moves on.
//  2. FALLBACK — daemon scans spawn stdout for a JSONL line matching
//     `{"peer_inbox_ack": true, ...}`. Uses an actual JSON parser per-
//     line (NOT substring match on "<peer-inbox-ack>" tag — that
//     would false-positive on agent prose discussing peer-inbox).
//  3. DEFERRED — MCP-tool ack is v1+ per §7.2 bullet 3.
//
// Fail-open on transient DB errors. Fail-loud on TTL invariant
// violation or receive-mode mismatch (exit 78 / non-zero respectively).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-collaboration/go/pkg/envelope"
	"agent-collaboration/go/pkg/obs"
	"agent-collaboration/go/pkg/store"
	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// ---------------------------------------------------------------------------
// Exit codes
// ---------------------------------------------------------------------------

const (
	exitOK       = 0
	exitInternal = 1
	exitUsage    = 64 // sysexits EX_USAGE
	// exitConfig: TTL ordering invariant violation or other
	// config-time fail-loud (§3.4 (c) "misconfiguration is a fail-
	// loud, not a silent race"). sysexits EX_CONFIG.
	exitConfig = 78
)

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

const (
	defaultAckTimeoutSecs  = 300  // 5 minutes (§3.3 ack-timeout default)
	defaultSweepTTLSecs    = 600  // 10 minutes (§3.3 sweeper_ttl default, 2× ack)
	defaultPollIntervalSec = 2
	defaultPauseOnIdleSec  = 1800 // 30 minutes (§6 Layer 3 pause-on-idle default)
	idlePollCapSec         = 60   // §6 L3: slow poll no lower than this

	// ackJSONLMarkerKey is the fence key the FALLBACK stdout-ack
	// parser looks for. Full shape: {"peer_inbox_ack": true, ...}
	// with an optional metadata payload. JSON structural fence per
	// §7.2 + §8.2 — resistant to agent prose that contains
	// "<peer-inbox>" strings or natural-language "ack" mentions.
	ackJSONLMarkerKey = "peer_inbox_ack"
)

// contentStopSentinel is the Layer-1 content-stop token. Intentionally
// NOT literalized as a single string constant because this very file
// lives inside a repo where agent prose quotes the sentinel; a literal
// string here would self-trigger on any grep-based scan of the source.
// Assembled at init from parts so the token value only appears in
// memory at runtime. Mirror of the constant referenced in the MCP
// server instructions / docs/PEER-INBOX-GUIDE.md — keep in sync.
var contentStopSentinel = "[" + "[" + "end" + "]" + "]"

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type cliKind string

const (
	cliClaude cliKind = "claude"
	cliCodex  cliKind = "codex"
	cliGemini cliKind = "gemini"
)

// daemonConfig is the resolved runtime config for one daemon instance.
// Built from CLI flags + env-var overrides in parseFlags; passed by
// value into the main loop so the loop cannot accidentally mutate it.
type daemonConfig struct {
	Label       string
	CWD         string
	SessionKey  string
	CLI         cliKind
	AckTimeout  time.Duration
	SweepTTL    time.Duration
	Poll        time.Duration
	PauseOnIdle time.Duration
	// Claude-only: --settings path (mandatory per §4 bullet 5 + §8.2
	// fixture-pin). Resolved once at startup so the per-spawn helper
	// stays allocation-free.
	ClaudeSettingsPath string
	LogPath            string
}

// ---------------------------------------------------------------------------
// main / flag parsing
// ---------------------------------------------------------------------------

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, rc, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return rc
	}

	// Build the slog logger early so validation failures get logged
	// structurally.
	log, closeLog, err := buildLogger(cfg.LogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: open log: %v\n", err)
		return exitInternal
	}
	defer closeLog()

	// §3.4 (c) TTL ordering invariant (gamma #1 + alpha
	// complementary): sweeper_ttl MUST be strictly greater than
	// daemon_ack_timeout. Config-time fail-loud.
	if cfg.SweepTTL <= cfg.AckTimeout {
		log.Error("daemon.ttl_invariant_violation",
			"ack_timeout_sec", cfg.AckTimeout.Seconds(),
			"sweep_ttl_sec", cfg.SweepTTL.Seconds(),
			"msg", "sweep-ttl MUST be strictly greater than ack-timeout",
		)
		fmt.Fprintf(os.Stderr,
			"daemon: TTL invariant violated: --sweep-ttl (%v) must be strictly "+
				"greater than --ack-timeout (%v). §3.4 (c): misconfiguration is "+
				"a fail-loud, not a silent race.\n",
			cfg.SweepTTL, cfg.AckTimeout,
		)
		return exitConfig
	}

	log.Info("daemon.start",
		"label", cfg.Label,
		"cwd", cfg.CWD,
		"cli", string(cfg.CLI),
		"ack_timeout_sec", cfg.AckTimeout.Seconds(),
		"sweep_ttl_sec", cfg.SweepTTL.Seconds(),
		"poll_interval_sec", cfg.Poll.Seconds(),
		"pause_on_idle_sec", cfg.PauseOnIdle.Seconds(),
	)

	// L4 heartbeat dep — see papercut 712121c (docs/plans/post-
	// option-j-handoff.md). This daemon does NOT ship heartbeat in
	// v0. `peer list` shows the daemon label with only its last_seen_
	// at which the DaemonModeClaim path bumps — crash-between-claims
	// is not yet observable. When heartbeat lands, a ticker goroutine
	// here should emit a sessions.last_seen_at UPDATE on a fixed
	// cadence independent of claim activity.

	// Wire SIGINT/SIGTERM to a cancelable context so the main loop
	// drains in-flight work gracefully.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	d := &daemon{cfg: cfg, log: log}
	return d.mainLoop(ctx)
}

// parseFlags returns the resolved config OR an error + exit code.
func parseFlags(args []string) (daemonConfig, int, error) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		label          = fs.String("label", "", "managed session label (required)")
		cwd            = fs.String("cwd", "", "workspace cwd for the managed session (required)")
		sessionKey     = fs.String("session-key", "", "stable session-key value exported into every spawn (required)")
		cliFlag        = fs.String("cli", "", "which CLI to spawn per batch: claude|codex|gemini (required)")
		ackTimeoutSec  = fs.Int("ack-timeout", 0, "per-batch ack timeout in seconds (default: AGENT_COLLAB_ACK_TIMEOUT env or 300)")
		sweepTTLSec    = fs.Int("sweep-ttl", 0, "sweeper TTL in seconds — used here only for the startup TTL-ordering validator (default: AGENT_COLLAB_SWEEP_TTL env or 600)")
		pollSec        = fs.Int("poll-interval", defaultPollIntervalSec, "DB poll cadence between ticks in seconds")
		pauseIdleSec   = fs.Int("pause-on-idle", defaultPauseOnIdleSec, "seconds of no activity before slowing poll frequency")
		logPath        = fs.String("log-path", "", "optional slog JSON output file path")
		claudeSettings = fs.String("claude-settings", "", "path to Claude settings.json passed via --settings on every claude spawn (default: ~/.claude/settings.json)")
	)
	if err := fs.Parse(args); err != nil {
		return daemonConfig{}, exitUsage, fmt.Errorf("parse flags: %w", err)
	}

	if *label == "" {
		return daemonConfig{}, exitUsage, errors.New("--label is required")
	}
	if *cwd == "" {
		return daemonConfig{}, exitUsage, errors.New("--cwd is required")
	}
	if *sessionKey == "" {
		return daemonConfig{}, exitUsage, errors.New("--session-key is required")
	}
	if *cliFlag == "" {
		return daemonConfig{}, exitUsage, errors.New("--cli is required (claude|codex|gemini)")
	}

	kind, err := parseCLIKind(*cliFlag)
	if err != nil {
		return daemonConfig{}, exitUsage, err
	}

	// Resolve cwd (symlink-resolved to match session-register + Go
	// peer-inbox resolveCWD behavior) so (cwd, label) lookups in the
	// DB match.
	resolvedCWD, err := resolveCWD(*cwd)
	if err != nil {
		return daemonConfig{}, exitInternal, fmt.Errorf("resolve cwd: %w", err)
	}

	ack := resolveIntSecondsWithEnv(*ackTimeoutSec, "AGENT_COLLAB_ACK_TIMEOUT", defaultAckTimeoutSecs)
	sweep := resolveIntSecondsWithEnv(*sweepTTLSec, "AGENT_COLLAB_SWEEP_TTL", defaultSweepTTLSecs)

	if ack <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--ack-timeout must be positive")
	}
	if sweep <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--sweep-ttl must be positive")
	}
	if *pollSec <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--poll-interval must be positive")
	}
	if *pauseIdleSec <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--pause-on-idle must be positive")
	}

	// Claude settings: §4 bullet 5 + §8.2 fixture-pin MANDATORY.
	// Resolve to a concrete path now so the fixture-pin test can
	// inspect the daemon's spawn argv deterministically.
	claudeSettingsResolved := *claudeSettings
	if claudeSettingsResolved == "" {
		if home, err := os.UserHomeDir(); err == nil {
			claudeSettingsResolved = filepath.Join(home, ".claude", "settings.json")
		} else {
			claudeSettingsResolved = ".claude/settings.json"
		}
	}

	return daemonConfig{
		Label:              *label,
		CWD:                resolvedCWD,
		SessionKey:         *sessionKey,
		CLI:                kind,
		AckTimeout:         time.Duration(ack) * time.Second,
		SweepTTL:           time.Duration(sweep) * time.Second,
		Poll:               time.Duration(*pollSec) * time.Second,
		PauseOnIdle:        time.Duration(*pauseIdleSec) * time.Second,
		ClaudeSettingsPath: claudeSettingsResolved,
		LogPath:            *logPath,
	}, exitOK, nil
}

func parseCLIKind(v string) (cliKind, error) {
	switch v {
	case "claude":
		return cliClaude, nil
	case "codex":
		return cliCodex, nil
	case "gemini":
		return cliGemini, nil
	default:
		return "", fmt.Errorf("--cli must be one of: claude, codex, gemini (got %q)", v)
	}
}

// resolveIntSecondsWithEnv implements the precedence explicit-flag >
// env-var > default. Matches the Python/Go peer-inbox pattern
// (resolveSweepTTL in go/cmd/peer-inbox). The flag signals "unset"
// via 0 because flag's default int is 0 — relies on the caller to
// pre-check that 0 is not a valid positive value (done by the
// ack/sweep positive-check branch upstream).
func resolveIntSecondsWithEnv(explicitFlag int, envVar string, fallback int) int {
	if explicitFlag > 0 {
		return explicitFlag
	}
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func resolveCWD(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// buildLogger returns a slog.Logger writing to either the given path
// (JSON handler) or to stderr (via obs.Logger). The returned closer
// is always non-nil — no-op when logging to stderr.
func buildLogger(path string) (*slog.Logger, func(), error) {
	if path == "" {
		return obs.Logger(), func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}, err
	}
	lvl := slog.LevelInfo
	if os.Getenv("AGENT_COLLAB_LOG_LEVEL") == "debug" {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: lvl}))
	return logger, func() { _ = f.Close() }, nil
}

// ---------------------------------------------------------------------------
// Daemon state
// ---------------------------------------------------------------------------

// daemon holds loop-local mutable state kept off the stack so signal-
// handling + the main loop can coordinate via the struct rather than a
// rats-nest of closure captures.
type daemon struct {
	cfg daemonConfig
	log *slog.Logger

	// Layer 3 exp-backoff state: recent batch timestamps per peer
	// label, trimmed to a 60s window on each claim.
	recentBatchesByPeer map[string][]time.Time
	recentBatchesMu     sync.Mutex
	// currentPoll is the current poll interval; L3 exp-backoff
	// doubles this up to idlePollCapSec. Reset to cfg.Poll whenever
	// recent-batch window drops back to <=3.
	currentPoll time.Duration
	// lastActivity tracks the last time a non-empty batch was
	// claimed; used for the pause-on-idle timer.
	lastActivity time.Time

	// shutdown: set once Layer 1 content-stop triggers so the main
	// loop goes dormant without exiting (matches Layer 2 externally-
	// written daemon_state='closed' behavior — daemon stays alive for
	// external reopen, per §6 Layer 2 bullet 5). The main loop
	// periodically re-polls daemon_state so an externally-written
	// open flip wakes it up.
	contentStopped bool
}

// mainLoop is the daemon's heart. Claim → process → complete → repeat
// with ack-timeout + sweeper-requeue recovery. Returns an exit code.
func (d *daemon) mainLoop(ctx context.Context) int {
	d.recentBatchesByPeer = make(map[string][]time.Time)
	d.currentPoll = d.cfg.Poll
	d.lastActivity = time.Now()

	// Exponential backoff state for transient DB errors (distinct
	// from L3 exp-backoff which is peer-rate-based).
	dbBackoff := d.cfg.Poll
	const dbBackoffCap = 30 * time.Second

	for {
		if ctx.Err() != nil {
			d.log.Info("daemon.shutdown", "reason", "context_canceled")
			return exitOK
		}

		// Pause-on-idle: if no activity for cfg.PauseOnIdle,
		// slow poll to min(currentPoll, idlePollCap). L3
		// quiescence primitive.
		if time.Since(d.lastActivity) > d.cfg.PauseOnIdle {
			if d.currentPoll < time.Duration(idlePollCapSec)*time.Second {
				newPoll := time.Duration(idlePollCapSec) * time.Second
				if newPoll != d.currentPoll {
					d.log.Warn("daemon.pause_on_idle",
						"idle_sec", time.Since(d.lastActivity).Seconds(),
						"old_poll_sec", d.currentPoll.Seconds(),
						"new_poll_sec", newPoll.Seconds(),
					)
					d.currentPoll = newPoll
				}
			}
		}

		d.log.Debug("daemon.tick", "poll_interval_sec", d.currentPoll.Seconds())

		// Open per-tick. SQLiteLocal holds a single connection, and
		// long-held handles across signal + sweep activity have
		// historically shown lock-contention with the Python path.
		// Open/close per tick is cheap on SQLite + WAL.
		rows, rc, err := d.claimTick(ctx)
		if err != nil {
			// Differentiate fail-loud (receive-mode mismatch) from
			// fail-open (transient). Receive-mode mismatch = exit
			// because the daemon is running on a label that isn't
			// configured for it (§3.4 (b)).
			if rc == exitConfig {
				return rc
			}
			d.log.Warn("daemon.claim_tick_failed",
				"err", err.Error(),
				"backoff_sec", dbBackoff.Seconds(),
			)
			// Fail-open sleep with cap.
			if !sleepCtx(ctx, dbBackoff) {
				d.log.Info("daemon.shutdown", "reason", "context_canceled_during_backoff")
				return exitOK
			}
			if dbBackoff < dbBackoffCap {
				dbBackoff *= 2
				if dbBackoff > dbBackoffCap {
					dbBackoff = dbBackoffCap
				}
			}
			continue
		}
		dbBackoff = d.cfg.Poll // reset on success

		if len(rows) == 0 {
			// Empty batch OR daemon_state='closed' — either way,
			// sleep.
			if !sleepCtx(ctx, d.currentPoll) {
				d.log.Info("daemon.shutdown", "reason", "context_canceled_idle")
				return exitOK
			}
			continue
		}

		// Non-empty batch: process.
		d.lastActivity = time.Now()

		// L3 exp-backoff: track peers. If >3 batches in 60s from a
		// single peer, double poll interval.
		d.recordBatchPeers(rows)
		d.applyExpBackoff()

		// Reset poll interval on activity if we're above baseline
		// AND not in exp-backoff territory.
		// (applyExpBackoff handles the upward direction; the
		// downward direction here catches "was idle, resumed".)
		if !d.isPeerHot() && d.currentPoll > d.cfg.Poll {
			d.log.Info("daemon.poll_reset",
				"old_poll_sec", d.currentPoll.Seconds(),
				"new_poll_sec", d.cfg.Poll.Seconds(),
			)
			d.currentPoll = d.cfg.Poll
		}

		// L1 content-stop detection: scan bodies BEFORE spawning.
		// We still process the batch (agent gets to respond to the
		// final message) — the "drain then dormant" wording in §6
		// Layer 1 — but after processing we flip daemon_state and
		// the next claim returns empty per §3.4 (e).
		l1Triggered := containsContentStop(rows)

		// Process: spawn CLI + wait for ack OR timeout.
		d.processBatch(ctx, rows)

		if l1Triggered {
			d.log.Warn("daemon.layer1_content_stop_triggered",
				"label", d.cfg.Label,
				"batch_size", len(rows),
			)
			if err := d.transitionClosed(ctx); err != nil {
				d.log.Warn("daemon.layer1_transition_failed", "err", err.Error())
			}
			d.contentStopped = true
			// Stay in the loop; subsequent claims return empty by
			// DaemonModeClaim's §3.4 (e) preflight until daemon_
			// state is externally flipped back to 'open'.
		}
	}
}

// sleepCtx sleeps up to d or until ctx is canceled. Returns false if
// the context was canceled (caller should exit the loop).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// claimTick opens the store, claims one batch, closes. Returns the
// claimed rows + an exit code (only meaningful when err != nil; for
// happy paths rc is exitOK and rows is the claim). Wraps the receive-
// mode mismatch → exitConfig mapping.
func (d *daemon) claimTick(ctx context.Context) ([]store.InboxMessage, int, error) {
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		return nil, exitInternal, fmt.Errorf("store open: %w", err)
	}
	defer st.Close()

	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	rows, err := st.DaemonModeClaim(openCtx, self)
	if err != nil {
		if errors.Is(err, store.ErrReceiveModeMismatch) {
			// §3.4 (b) fail-loud: daemon is running on a label
			// whose sessions.receive_mode isn't 'daemon'. Exit.
			return nil, exitConfig, fmt.Errorf(
				"receive-mode mismatch: session (%s, %s) is not registered with "+
					"receive_mode='daemon'; register via "+
					"`agent-collab session register --receive-mode daemon --as %s` "+
					"before starting this daemon (§3.4 (b) verb-entry gate)",
				d.cfg.CWD, d.cfg.Label, d.cfg.Label,
			)
		}
		return nil, exitInternal, err
	}
	d.log.Info("daemon.claim",
		"label", d.cfg.Label,
		"batch_size", len(rows),
	)
	return rows, exitOK, nil
}

// recordBatchPeers tracks per-peer batch timestamps inside a 60s
// window for the L3 exp-backoff decision. Cheap; runs on every non-
// empty batch.
func (d *daemon) recordBatchPeers(rows []store.InboxMessage) {
	d.recentBatchesMu.Lock()
	defer d.recentBatchesMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	// Record the peer the final message came from (not every message;
	// a single batch is one "turn").
	peer := rows[len(rows)-1].FromLabel
	ts := append(d.recentBatchesByPeer[peer], now)
	// Trim past the 60s window.
	for i, t := range ts {
		if t.After(cutoff) {
			ts = ts[i:]
			break
		}
	}
	d.recentBatchesByPeer[peer] = ts
}

// applyExpBackoff doubles the current poll interval (up to the idle
// cap) if any tracked peer has emitted >3 batches in the 60s window.
// §6 Layer 3 bullet 1.
func (d *daemon) applyExpBackoff() {
	if !d.isPeerHot() {
		return
	}
	cap := time.Duration(idlePollCapSec) * time.Second
	if d.currentPoll >= cap {
		return
	}
	newPoll := d.currentPoll * 2
	if newPoll > cap {
		newPoll = cap
	}
	d.log.Warn("daemon.layer3_exp_backoff",
		"old_poll_sec", d.currentPoll.Seconds(),
		"new_poll_sec", newPoll.Seconds(),
	)
	d.currentPoll = newPoll
}

func (d *daemon) isPeerHot() bool {
	d.recentBatchesMu.Lock()
	defer d.recentBatchesMu.Unlock()
	for _, ts := range d.recentBatchesByPeer {
		if len(ts) > 3 {
			return true
		}
	}
	return false
}

// containsContentStop scans batch bodies for the Layer-1 content-stop
// sentinel. Returns true if ANY message in the batch contains the
// token (the final message is the likely location but we scan all
// defensively).
func containsContentStop(rows []store.InboxMessage) bool {
	for _, r := range rows {
		if strings.Contains(r.Body, contentStopSentinel) {
			return true
		}
	}
	return false
}

// transitionClosed flips sessions.daemon_state='closed' for the
// managed label. Called after a content-stop-triggered batch completes
// — on next poll, DaemonModeClaim's §3.4 (e) preflight returns empty
// and the daemon stays dormant until externally reopened.
func (d *daemon) transitionClosed(ctx context.Context) error {
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		return fmt.Errorf("store open: %w", err)
	}
	defer st.Close()
	if err := st.SetDaemonState(openCtx, store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}, "closed"); err != nil {
		return fmt.Errorf("set daemon_state closed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Batch processing — spawn + ack
// ---------------------------------------------------------------------------

// processBatch builds the envelope text, spawns the configured CLI,
// waits for completion (mechanism 1 or 2) OR ack-timeout, then
// attempts DaemonModeComplete. §7.2 + §3.4 (d) — stale-claim on
// DaemonModeComplete is a legitimate outcome (sweeper reaped while
// we were waiting) and is logged-and-continued.
func (d *daemon) processBatch(ctx context.Context, rows []store.InboxMessage) {
	if len(rows) == 0 {
		return
	}

	// §7.1 trusted-template: static prompt; peer content is ONLY
	// passed via the envelope payload. No {{var}} substitution of
	// peer content into the instruction.
	envText := d.renderBatchText(rows)
	promptText := d.buildStaticPrompt() + "\n\n" + envText

	d.log.Info("daemon.spawn.begin",
		"label", d.cfg.Label,
		"cli", string(d.cfg.CLI),
		"batch_size", len(rows),
		"prompt_bytes", len(promptText),
	)

	ackCtx, cancel := context.WithTimeout(ctx, d.cfg.AckTimeout)
	defer cancel()

	stdoutBytes, err := d.spawnCLI(ackCtx, promptText)
	spawnErr := err

	// Empty-response termination (§6 L3 bullet 2): if the spawn
	// returned clean but stdout is empty/whitespace-only, treat as
	// "agent has nothing to add" — mark completed so rows don't
	// re-trigger a respawn on next poll.
	if spawnErr == nil && len(strings.TrimSpace(stdoutBytes)) == 0 {
		d.log.Warn("daemon.layer3_empty_response",
			"label", d.cfg.Label,
			"msg", "spawn returned empty stdout; not respawning (L3)",
		)
		d.completeBatch(ctx, "empty-response")
		return
	}

	// Mechanism 2 FALLBACK: scan stdout for JSONL ack marker.
	ackViaMarker := false
	if spawnErr == nil {
		ackViaMarker = scanStdoutForAckMarker(stdoutBytes)
	}

	// Mechanism 1 PRIMARY: the spawned CLI may have already called
	// `peer-inbox daemon-complete`. DaemonModeComplete returns
	// ErrStaleClaim in that case because the UPDATE matches zero
	// rows (completed_at already set by the child). We treat
	// ErrStaleClaim here as "mechanism 1 already fired" — benign.
	//
	// The daemon issues a best-effort DaemonModeComplete even when
	// mechanism-2 fired, for idempotence — double-completion is a
	// no-op by construction (§8.2 mechanism-mixing gate).
	spawnedOK := spawnErr == nil
	switch {
	case !spawnedOK && errors.Is(spawnErr, context.DeadlineExceeded):
		// Ack-timeout: daemon abandons internally. Do NOT call
		// DaemonModeComplete. Sweeper requeues on next pass per
		// §3.4 (c)/(d).
		d.log.Warn("daemon.ack_timeout_abandoned",
			"label", d.cfg.Label,
			"ack_timeout_sec", d.cfg.AckTimeout.Seconds(),
			"msg", "abandoning batch; sweeper will requeue",
		)
		return
	case !spawnedOK:
		d.log.Warn("daemon.spawn_failed",
			"label", d.cfg.Label,
			"err", spawnErr.Error(),
		)
		// Spawn failure without timeout: do NOT complete. Sweeper
		// requeues eventually.
		return
	}

	reason := "mechanism1_direct_or_idempotent_followup"
	if ackViaMarker {
		reason = "mechanism2_jsonl_marker"
	}
	d.completeBatch(ctx, reason)
}

// completeBatch runs DaemonModeComplete + handles the ErrStaleClaim
// legitimate-outcome case. Separated so both the happy-ack path and
// the L3-empty-response path can reuse it.
func (d *daemon) completeBatch(ctx context.Context, reason string) {
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		d.log.Warn("daemon.complete_open_failed", "err", err.Error())
		return
	}
	defer st.Close()
	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	ids, err := st.DaemonModeComplete(openCtx, self)
	switch {
	case err == nil:
		d.log.Info("daemon.complete",
			"label", d.cfg.Label,
			"completed_ids", ids,
			"reason", reason,
		)
	case errors.Is(err, store.ErrStaleClaim):
		// Mechanism-1 completed via the child's own peer-inbox
		// daemon-complete call, OR the sweeper reaped between
		// claim-time and complete-time (§3.4 (d) fail-loud on
		// reap-race). Either way benign for the daemon.
		d.log.Info("daemon.complete_stale",
			"label", d.cfg.Label,
			"reason", reason,
			"msg", "stale claim — mechanism-1 likely already completed, or sweeper reaped",
		)
	default:
		d.log.Warn("daemon.complete_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
			"reason", reason,
		)
	}
}

// renderBatchText builds the <peer-inbox>...</peer-inbox> block via
// the SHARED envelope.RenderText serializer (§5.2 byte-parity gate).
func (d *daemon) renderBatchText(rows []store.InboxMessage) string {
	msgs := make([]envelope.Message, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, envelope.Message{
			ID:        r.ID,
			FromCWD:   r.FromCWD,
			FromLabel: r.FromLabel,
			Body:      r.Body,
			CreatedAt: r.CreatedAt,
			RoomKey:   r.RoomKey,
		})
	}
	env := envelope.Envelope{
		To: &envelope.Addressee{
			CWD:   d.cfg.CWD,
			Label: d.cfg.Label,
		},
		Messages: msgs,
	}
	// Use default budget (4 KiB) unless the hook budget env is set
	// — the daemon respects the same AGENT_COLLAB_HOOK_BLOCK_BUDGET
	// override for byte-parity with the hook path.
	budget := envelope.DefaultHookBlockBudgetBytes
	if v := os.Getenv("AGENT_COLLAB_HOOK_BLOCK_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			budget = n
		}
	}
	return envelope.RenderText(env, budget)
}

// buildStaticPrompt returns the static-by-design instruction prefix
// per §7.1 item 3. NO peer content is substituted into this string;
// peer content flows exclusively through the envelope payload
// appended after it.
func (d *daemon) buildStaticPrompt() string {
	return strings.TrimSpace(`
You have received peer-inbox messages from a teammate session. The messages
appear below inside a <peer-inbox>...</peer-inbox> block. Respond to them
as you normally would in a chat thread.

When you are done responding, do ONE of the following so the daemon that
delivered this batch knows you are finished:

  (A) Run this command as your final tool call (preferred):

      peer-inbox daemon-complete --cwd <YOUR_CWD> --as <YOUR_LABEL>

  (B) Emit a single JSON line as your final stdout output:

      {"peer_inbox_ack": true}

If you produce no output at all, the daemon will treat it as "nothing to
add" and close out the batch without re-prompting you.
`)
}

// ---------------------------------------------------------------------------
// Ack-marker parser (FALLBACK mechanism 2)
// ---------------------------------------------------------------------------

// scanStdoutForAckMarker scans the given stdout text for a structural
// JSONL ack marker. A marker is a JSON-object line (after trim) that
// parses and has a truthy `peer_inbox_ack` key. §7.2 + §8.2 false-
// positive resistance: substring matching on "<peer-inbox-ack>" or
// natural-language "ack" would trigger on agent prose discussing the
// protocol; we require actual JSON parseability as the fence.
func scanStdoutForAckMarker(s string) bool {
	scanner := bufio.NewScanner(strings.NewReader(s))
	// Bump buffer so large single-line JSON blobs don't trip the
	// default 64 KiB cap.
	buf := make([]byte, 0, 1<<20)
	scanner.Buffer(buf, 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		v, ok := obj[ackJSONLMarkerKey]
		if !ok {
			continue
		}
		// Truthy check: bool true, or non-empty anything. Python
		// side (if it ever emits) uses `true`; Go daemons scan
		// defensively.
		if b, isBool := v.(bool); isBool && b {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CLI spawn helpers
// ---------------------------------------------------------------------------

// spawnCLI dispatches to the configured CLI kind. Returns captured
// stdout + any error (including context.DeadlineExceeded on ack-
// timeout).
func (d *daemon) spawnCLI(ctx context.Context, prompt string) (string, error) {
	switch d.cfg.CLI {
	case cliClaude:
		return d.spawnClaude(ctx, prompt)
	case cliCodex:
		return d.spawnCodex(ctx, prompt)
	case cliGemini:
		return d.spawnGemini(ctx, prompt)
	default:
		return "", fmt.Errorf("unknown cli kind: %q", d.cfg.CLI)
	}
}

// claudeArgv / codexArgv / geminiArgv are kept as small helpers so the
// §8.2 fixture-pin tests can invoke them directly if ever refactored
// into a testable form. For now they're inlined in the spawn helpers;
// the test harness shells in via PATH-indirection against a fake CLI
// and dumps argv there.

// spawnClaude runs `claude -p --settings <path> <prompt>` with the
// MANDATORY §4 bullet 5 --settings flag fixture-pinned in place. The
// flag is required TODAY for future-proofing against the `--bare`
// default roll-out: without --settings, --bare would skip hook + MCP
// + CLAUDE.md and the daemon-spawn would silently receive nothing.
// Pins the invocation shape NOW so tests/daemon-harness.sh fails
// loud if a future refactor drops it.
func (d *daemon) spawnClaude(ctx context.Context, prompt string) (string, error) {
	args := []string{
		"-p",
		"--settings", d.cfg.ClaudeSettingsPath,
		prompt,
	}
	return d.execSpawn(ctx, "claude", args, map[string]string{
		// CLI-specific env-key for Claude.
		"CLAUDE_SESSION_ID": d.cfg.SessionKey,
	})
}

// spawnCodex runs `codex exec --skip-git-repo-check <prompt>` with
// stdin=/dev/null. §4 bullets 2 + 3: stdin-close prevents codex exec's
// known hang-on-open-stdin bug; --skip-git-repo-check lets the daemon
// operate in non-git workspaces (e.g. scratch dirs).
func (d *daemon) spawnCodex(ctx context.Context, prompt string) (string, error) {
	args := []string{
		"exec",
		"--skip-git-repo-check",
		prompt,
	}
	return d.execSpawn(ctx, "codex", args, map[string]string{
		"CODEX_SESSION_ID": d.cfg.SessionKey,
	})
}

// spawnGemini runs `gemini -p <prompt>` with stdin=/dev/null. §4
// bullet 4 gemini timeout-units is already handled by install-script
// on the receive side; daemon just needs to export the env var.
func (d *daemon) spawnGemini(ctx context.Context, prompt string) (string, error) {
	args := []string{
		"-p",
		prompt,
	}
	return d.execSpawn(ctx, "gemini", args, map[string]string{
		"GEMINI_SESSION_ID": d.cfg.SessionKey,
	})
}

// execSpawn is the shared spawn implementation. Stdin is always
// /dev/null (even for Claude — prevents the interactive-stdin hang
// trap across all three). Stdout is captured for the ack-marker
// scanner; stderr is written to the daemon's log stream.
//
// Per-CLI env vars are merged with the daemon-neutral vars:
//   - AGENT_COLLAB_SESSION_KEY=<session-key>    (hook SESSION_KEY_ENV_CANDIDATES fallback)
//   - AGENT_COLLAB_DAEMON_SPAWN=1               (§3.4 (f) hook short-circuit)
//   - PATH                                      (inherited from daemon process)
func (d *daemon) execSpawn(ctx context.Context, bin string, args []string, extraEnv map[string]string) (string, error) {
	// Allow tests / operator overrides: if AGENT_COLLAB_DAEMON_<bin>_BIN
	// is set, use that path instead of PATH-resolving "claude"/etc.
	// This is how tests/daemon-*.sh inject fake CLIs without relying
	// on mv-ing $PATH under the daemon's feet mid-test.
	binKey := strings.ToUpper("AGENT_COLLAB_DAEMON_" + bin + "_BIN")
	if override := os.Getenv(binKey); override != "" {
		bin = override
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = d.cfg.CWD
	cmd.Stdin = nil // os/exec maps nil to /dev/null under the hood

	// Put the child in its own process group so we can SIGTERM the
	// whole subtree on ack-timeout. Without this, a bash wrapper
	// (real-world: wrapped CLIs) that forks a long-lived child may
	// keep the stdout pipe open after the parent is killed, causing
	// cmd.Wait to block until the orphan exits naturally.
	setProcessGroup(cmd)

	// WaitDelay bounds the time exec.Wait spends waiting on
	// orphaned children / unclosed pipes after context cancellation.
	// After cancellation, Go sends SIGKILL to the direct child; if
	// its stdout pipe is still open (held by a grandchild like
	// `sleep 30` in a bash wrapper), Wait would block forever.
	// WaitDelay forces Wait to return after 500ms regardless.
	cmd.WaitDelay = 500 * time.Millisecond
	// Also set a custom Cancel that tries a group SIGTERM first;
	// os/exec's default would only signal the direct child.
	cmd.Cancel = func() error {
		// Best-effort: SIGTERM the whole process group. If the OS
		// doesn't support -PID group signaling, fall back to the
		// direct child.
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		return nil
	}

	// Env: inherit + merge daemon-neutral + merge per-CLI.
	env := os.Environ()
	env = setOrAppendEnv(env, "AGENT_COLLAB_SESSION_KEY", d.cfg.SessionKey)
	env = setOrAppendEnv(env, "AGENT_COLLAB_DAEMON_SPAWN", "1")
	for k, v := range extraEnv {
		env = setOrAppendEnv(env, k, v)
	}
	cmd.Env = env

	// Capture stdout for ack-marker scanning; inherit stderr so agent
	// errors surface to the daemon's stderr (operator can see them).
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr

	d.log.Info("daemon.spawn.exec",
		"bin", bin,
		"argv", redactArgv(args),
	)

	runErr := cmd.Run()

	// Context-cancel → treat as DeadlineExceeded so the caller
	// detects ack-timeout. os/exec returns a non-nil error with
	// ctx.Err()==DeadlineExceeded; normalize.
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return outBuf.String(), context.DeadlineExceeded
		}
		return outBuf.String(), runErr
	}
	return outBuf.String(), nil
}

// setOrAppendEnv replaces or adds KEY=VAL in an env slice.
func setOrAppendEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// redactArgv returns a loggable form of argv that truncates the
// final prompt arg (peer content is in it, so logging the full
// content at Info level would bloat logs). Keeps the first few args
// verbatim so the fixture-pin test for claude --settings can still
// observe the flag in the slog output if needed.
func redactArgv(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		// Heuristic: the last arg to every CLI in spawnClaude/
		// Codex/Gemini is the prompt. Redact args longer than 256
		// chars OR the last arg specifically.
		if i == len(args)-1 || len(a) > 256 {
			out = append(out, fmt.Sprintf("<prompt: %d bytes>", len(a)))
			continue
		}
		out = append(out, a)
	}
	return out
}

