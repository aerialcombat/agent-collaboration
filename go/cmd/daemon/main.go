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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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

// codexBannerSessionRE matches the session-id line emitted by
// `codex exec` on first-spawn startup. Fixture-pinned at
// tests/fixtures/cli-session/codex-banner.txt (real codex 0.121.0
// output). Topic 3 v0.1 Arch D §6.1 — capture strategy: scan the
// spawn's own stdout buffer for this line; on match, persist group 1
// (UUID) to sessions.daemon_cli_session_id for resume on next batch.
//
// Compiled once at package init for per-spawn reuse. Case-insensitive
// to tolerate codex-vendor minor casing drift without a fixture-pin
// break; fixture-pin on the test side still fails loud on format
// change (e.g., the "session id:" prefix disappearing entirely).
//
// Per §3.4 invariant 5: capture-failure (no match) is non-fatal —
// logged as a warning and the batch proceeds Arch B fresh-invocation.
var codexBannerSessionRE = regexp.MustCompile(`(?i)session id:\s*([0-9a-f-]{36})`)

// codexStaleSessionRE matches codex's "session not found" /
// "no such session" error patterns. Liberal match per Topic 3 v0.1
// Arch D §3.4 invariant 5 "if uncertain, log + clear" — better to
// drop a still-live UUID (next capture recovers) than wedge a stale
// one across respawns.
//
// Scanned against BOTH stdout AND stderr of the spawned codex (v0.1.2
// fix — earlier draft only scanned stdout, but real codex 0.121.0 writes
// the banner + most diagnostics to stderr; v0.1 acknowledged this risk
// in code comments but shipped stdout-only capture, and E5 probe
// confirmed the gap empirically). execSpawn now captures stderr into a
// separate buffer (still also inherited via io.MultiWriter to the
// daemon's stderr for real-time operator visibility), and
// postSpawnCodexResumeHandling scans the concatenation of both streams.
var codexStaleSessionRE = regexp.MustCompile(`(?i)(session not found|no such session|session .* not found|unknown session)`)

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
	// Topic 3 v0.1 (Architecture D) §7 — CLI-native session-resume
	// opt-in flag. When true AND CLI in {codex, gemini}, daemon
	// captures the CLI vendor session-ID on first spawn (§6) and
	// passes the cached identity to subsequent spawns. When CLI=claude,
	// the daemon emits a one-time warning at startup (§4.3) and
	// proceeds Arch B fresh-invocation regardless. Default false
	// preserves Topic 3 v0 behavior.
	CLISessionResume bool
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
		"cli_session_resume", cfg.CLISessionResume,
	)

	// Topic 3 v0.1 Arch D §4.3 + §3.4 invariant 4: claude + cli_session_resume
	// is non-fatal — emit the documented warning string and proceed in
	// fresh-invocation mode. Sub-task B/C/D will read cfg.CLISessionResume in
	// the per-CLI spawn helpers; for cliClaude the helper ignores the flag
	// (defensive assertion locked at the spawn-construction layer per gate
	// 6d). Warn at startup so operators see it once, not per-spawn.
	if cfg.CLI == cliClaude && cfg.CLISessionResume {
		log.Warn("daemon.cli_session_resume.claude_asymmetry",
			"label", cfg.Label,
			"msg", "Claude has no cross-process session-resume; --cli-session-resume is a no-op for this daemon (see Arch B asymmetry note in operator guide).",
		)
		fmt.Fprintln(os.Stderr,
			"Claude has no cross-process session-resume; --cli-session-resume is a no-op for this daemon (see Arch B asymmetry note in operator guide).",
		)
	}

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

// configFile is the on-disk JSON shape for daemon config. Field names match
// the CLI flag names exactly so operators can translate between the two
// without a lookup. All fields optional except when no corresponding CLI
// flag is also passed.
//
// TOML support is explicitly v1+ (§10 Q#6 open for owner; JSON keeps the
// v0 dep tree minimal and the schema easily convertible later).
type configFile struct {
	Label          string `json:"label,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	SessionKey     string `json:"session_key,omitempty"`
	CLI            string `json:"cli,omitempty"`
	AckTimeoutSec  int    `json:"ack_timeout,omitempty"`
	SweepTTLSec    int    `json:"sweep_ttl,omitempty"`
	PollSec        int    `json:"poll_interval,omitempty"`
	PauseIdleSec   int    `json:"pause_on_idle,omitempty"`
	LogPath        string `json:"log_path,omitempty"`
	ClaudeSettings string `json:"claude_settings,omitempty"`
	// Topic 3 v0.1 Arch D §7.1 opt-in flag. JSON boolean only in v0.1.
	// §7.3 reserves the per-CLI object form (cli_session_resume:
	// {codex: true, gemini: false}) for v1+; if operator passes the
	// object form, json.Unmarshal returns a clear "cannot unmarshal
	// object into Go struct field" error pointing at the line — that's
	// sufficient for v0.1 since the v1+ shape is documented in §7.3.
	// Pointer to distinguish "field absent" from "field set to false."
	CLISessionResume *bool `json:"cli_session_resume,omitempty"`
}

// resolveConfigPath expands --config arguments:
//   - absolute path or one containing a slash/dot  → used verbatim
//   - bare name (e.g. "reviewer-codex")           → ~/.agent-collab/daemons/<name>.json
//
// Operator ergonomics: `agent-collab-daemon --config reviewer-codex` works
// without typing the full path (matches §2.2 "one file per daemon" pattern).
func resolveConfigPath(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	// Treat any argument with a path separator or `.` as a literal path.
	if strings.ContainsAny(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasSuffix(arg, ".json") {
		return arg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for --config shorthand: %w", err)
	}
	return filepath.Join(home, ".agent-collab", "daemons", arg+".json"), nil
}

// loadConfigFile reads + parses the JSON at path. Returns a zero-value
// configFile + nil error when path is empty (caller passed no --config).
func loadConfigFile(path string) (configFile, error) {
	var cf configFile
	if path == "" {
		return cf, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cf, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &cf); err != nil {
		return cf, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cf, nil
}

// parseFlags returns the resolved config OR an error + exit code.
//
// Flag-vs-file precedence (§10 Q#6 operator contract):
//   - CLI flag explicitly passed → wins.
//   - CLI flag absent + config file field present → file value used.
//   - CLI flag absent + config file field absent → default (or env for TTLs).
//
// The daemon-side TTL ordering invariant (§3.4 (c)) is checked post-resolve
// and applies regardless of which source supplied each TTL.
func parseFlags(args []string) (daemonConfig, int, error) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configArg      = fs.String("config", "", "path to JSON config file (or bare name resolved to ~/.agent-collab/daemons/<name>.json). Flags override file values.")
		label          = fs.String("label", "", "managed session label (required if not in --config)")
		cwd            = fs.String("cwd", "", "workspace cwd for the managed session (required if not in --config)")
		sessionKey     = fs.String("session-key", "", "stable session-key value exported into every spawn (required if not in --config)")
		cliFlag        = fs.String("cli", "", "which CLI to spawn per batch: claude|codex|gemini (required if not in --config)")
		ackTimeoutSec  = fs.Int("ack-timeout", 0, "per-batch ack timeout in seconds (default: config file, AGENT_COLLAB_ACK_TIMEOUT env, or 300)")
		sweepTTLSec    = fs.Int("sweep-ttl", 0, "sweeper TTL in seconds — used here only for the startup TTL-ordering validator (default: config file, AGENT_COLLAB_SWEEP_TTL env, or 600)")
		pollSec        = fs.Int("poll-interval", 0, "DB poll cadence between ticks in seconds (default: config file or 2)")
		pauseIdleSec   = fs.Int("pause-on-idle", 0, "seconds of no activity before slowing poll frequency (default: config file or 1800)")
		logPath          = fs.String("log-path", "", "optional slog JSON output file path")
		claudeSettings   = fs.String("claude-settings", "", "path to Claude settings.json passed via --settings on every claude spawn (default: config file, or ~/.claude/settings.json)")
		cliSessionResume = fs.Bool("cli-session-resume", false, "Topic 3 v0.1 Arch D opt-in: pass the CLI vendor's native session-ID into subsequent spawns (codex/gemini only; no-op + warn for claude). Default false preserves Topic 3 v0 fresh-invocation behavior.")
	)
	if err := fs.Parse(args); err != nil {
		return daemonConfig{}, exitUsage, fmt.Errorf("parse flags: %w", err)
	}

	// Track which flags were explicitly passed so file values don't
	// clobber them. `flag.Visit` iterates over flags that Parse saw on
	// the command line, so we can distinguish "flag set to zero value"
	// from "flag not passed."
	explicitlySet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitlySet[f.Name] = true })

	// Load config file if --config was passed.
	configPath, err := resolveConfigPath(*configArg)
	if err != nil {
		return daemonConfig{}, exitUsage, err
	}
	cf, err := loadConfigFile(configPath)
	if err != nil {
		// Config-file read/parse failures are operator-recoverable with
		// a clearer file, so return exitConfig (78, sysexits EX_CONFIG)
		// rather than exitUsage so the exit code reflects "fix your
		// config" not "fix your command line."
		return daemonConfig{}, exitConfig, err
	}

	// Resolve each config value with the flag-file-env-default precedence.
	resolvedLabel := *label
	if !explicitlySet["label"] && cf.Label != "" {
		resolvedLabel = cf.Label
	}
	resolvedCWD := *cwd
	if !explicitlySet["cwd"] && cf.CWD != "" {
		resolvedCWD = cf.CWD
	}
	resolvedSessionKey := *sessionKey
	if !explicitlySet["session-key"] && cf.SessionKey != "" {
		resolvedSessionKey = cf.SessionKey
	}
	resolvedCLIString := *cliFlag
	if !explicitlySet["cli"] && cf.CLI != "" {
		resolvedCLIString = cf.CLI
	}
	resolvedLogPath := *logPath
	if !explicitlySet["log-path"] && cf.LogPath != "" {
		resolvedLogPath = cf.LogPath
	}
	resolvedClaudeSettings := *claudeSettings
	if !explicitlySet["claude-settings"] && cf.ClaudeSettings != "" {
		resolvedClaudeSettings = cf.ClaudeSettings
	}

	// Topic 3 v0.1 Arch D §7.2 — opt-in flag precedence: CLI flag > config
	// file > env > default. Default is false (preserves Topic 3 v0
	// fresh-invocation behavior).
	resolvedCLISessionResume := *cliSessionResume
	if !explicitlySet["cli-session-resume"] {
		if cf.CLISessionResume != nil {
			resolvedCLISessionResume = *cf.CLISessionResume
		} else if os.Getenv("AGENT_COLLAB_CLI_SESSION_RESUME") == "1" {
			resolvedCLISessionResume = true
		}
	}

	if resolvedLabel == "" {
		return daemonConfig{}, exitUsage, errors.New("--label is required (or set `label` in --config file)")
	}
	if resolvedCWD == "" {
		return daemonConfig{}, exitUsage, errors.New("--cwd is required (or set `cwd` in --config file)")
	}
	if resolvedSessionKey == "" {
		return daemonConfig{}, exitUsage, errors.New("--session-key is required (or set `session_key` in --config file)")
	}
	if resolvedCLIString == "" {
		return daemonConfig{}, exitUsage, errors.New("--cli is required (or set `cli` in --config file): claude|codex|gemini")
	}

	kind, err := parseCLIKind(resolvedCLIString)
	if err != nil {
		return daemonConfig{}, exitUsage, err
	}

	// Resolve cwd (symlink-resolved to match session-register + Go
	// peer-inbox resolveCWD behavior) so (cwd, label) lookups in the
	// DB match.
	resolvedCWDFinal, err := resolveCWD(resolvedCWD)
	if err != nil {
		return daemonConfig{}, exitInternal, fmt.Errorf("resolve cwd: %w", err)
	}

	// TTL resolution: flag → config file → env → default. `explicitlySet`
	// distinguishes "--ack-timeout 0" (unusual; 0 is invalid downstream)
	// from "flag not passed".
	ackSec := *ackTimeoutSec
	if !explicitlySet["ack-timeout"] && cf.AckTimeoutSec > 0 {
		ackSec = cf.AckTimeoutSec
	}
	sweepSec := *sweepTTLSec
	if !explicitlySet["sweep-ttl"] && cf.SweepTTLSec > 0 {
		sweepSec = cf.SweepTTLSec
	}
	ack := resolveIntSecondsWithEnv(ackSec, "AGENT_COLLAB_ACK_TIMEOUT", defaultAckTimeoutSecs)
	sweep := resolveIntSecondsWithEnv(sweepSec, "AGENT_COLLAB_SWEEP_TTL", defaultSweepTTLSecs)

	pollResolved := *pollSec
	if !explicitlySet["poll-interval"] && cf.PollSec > 0 {
		pollResolved = cf.PollSec
	}
	if pollResolved == 0 {
		pollResolved = defaultPollIntervalSec
	}
	pauseResolved := *pauseIdleSec
	if !explicitlySet["pause-on-idle"] && cf.PauseIdleSec > 0 {
		pauseResolved = cf.PauseIdleSec
	}
	if pauseResolved == 0 {
		pauseResolved = defaultPauseOnIdleSec
	}

	if ack <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--ack-timeout must be positive")
	}
	if sweep <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--sweep-ttl must be positive")
	}
	if pollResolved <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--poll-interval must be positive")
	}
	if pauseResolved <= 0 {
		return daemonConfig{}, exitUsage, errors.New("--pause-on-idle must be positive")
	}

	// Claude settings: §4 bullet 5 + §8.2 fixture-pin MANDATORY.
	// Resolve to a concrete path now so the fixture-pin test can
	// inspect the daemon's spawn argv deterministically.
	if resolvedClaudeSettings == "" {
		if home, err := os.UserHomeDir(); err == nil {
			resolvedClaudeSettings = filepath.Join(home, ".claude", "settings.json")
		} else {
			resolvedClaudeSettings = ".claude/settings.json"
		}
	}

	return daemonConfig{
		Label:              resolvedLabel,
		CWD:                resolvedCWDFinal,
		SessionKey:         resolvedSessionKey,
		CLI:                kind,
		AckTimeout:         time.Duration(ack) * time.Second,
		SweepTTL:           time.Duration(sweep) * time.Second,
		Poll:               time.Duration(pollResolved) * time.Second,
		PauseOnIdle:        time.Duration(pauseResolved) * time.Second,
		ClaudeSettingsPath: resolvedClaudeSettings,
		LogPath:            resolvedLogPath,
		CLISessionResume:   resolvedCLISessionResume,
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
//
// Topic 3 v0.1 Arch D §8.2 SECONDARY auto-GC: after SetDaemonState
// succeeds, this helper additionally NULLs sessions.daemon_cli_session_id
// for the same (cwd, label). This is the *only* daemon-side trigger for
// CLI-session-ID reset in v0/v0.1 — L2 (external SQL flip of
// daemon_state='closed' with no L1 sentinel) does NOT auto-GC the CLI
// session-ID; the operator must run `peer-inbox daemon-reset-session`
// separately. L3 quiescence is "pause poll cadence" (not "drop daemon
// state") and deliberately preserves the resume invariant. A timer-based
// auto-GC is deferred to v1+ per §10 Q4.
//
// Both UPDATEs ride the same SQLiteLocal handle (one Open, two updates,
// then Close) — efficient and keeps the transition atomic from the
// daemon's point of view. ClearDaemonCLISessionID is idempotent per
// §3.4 invariant 3: if the column is already NULL, the call still
// returns nil (no error, safe-to-spam).
func (d *daemon) transitionClosed(ctx context.Context) error {
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		return fmt.Errorf("store open: %w", err)
	}
	defer st.Close()
	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	if err := st.SetDaemonState(openCtx, self, "closed"); err != nil {
		return fmt.Errorf("set daemon_state closed: %w", err)
	}
	// §8.2 auto-GC: NULL the captured CLI session-ID so reopening this
	// daemon (operator flipping daemon_state back to 'open') gets a
	// fresh CLI session by construction. Non-fatal — if ClearDaemon
	// CLISessionID fails (e.g. mid-transition DB contention), we log and
	// continue: daemon_state='closed' is the load-bearing state change;
	// a stale CLI session-ID in a closed daemon is harmless because no
	// spawn will read it until reopening, at which point the operator
	// can re-assert via `peer-inbox daemon-reset-session` per §3.3
	// PRIMARY. Idempotent on already-NULL columns per §3.4 invariant 3.
	if err := st.ClearDaemonCLISessionID(openCtx, self); err != nil {
		d.log.Warn("daemon.cli_session_auto_gc.l1_content_stop_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
	} else {
		d.log.Info("daemon.cli_session_auto_gc.l1_content_stop",
			"label", d.cfg.Label,
		)
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
	stdout, _, err := d.execSpawn(ctx, "claude", args, map[string]string{
		// CLI-specific env-key for Claude.
		"CLAUDE_SESSION_ID": d.cfg.SessionKey,
	})
	return stdout, err
}

// spawnCodex runs `codex exec --skip-git-repo-check <prompt>` with
// stdin=/dev/null. §4 bullets 2 + 3: stdin-close prevents codex exec's
// known hang-on-open-stdin bug; --skip-git-repo-check lets the daemon
// operate in non-git workspaces (e.g. scratch dirs).
//
// Topic 3 v0.1 Arch D §4.1 + §6.1: when d.cfg.CLISessionResume is true,
// this helper additionally wires in CLI-native session-resume:
//  1. Before argv construction: query sessions.daemon_cli_session_id via
//     GetDaemonCLISessionID. If non-empty, argv becomes
//     `codex exec resume --skip-git-repo-check <UUID> <prompt>` per §4.1.
//     If empty, argv stays as today (first-batch / post-reset fresh spawn).
//  2. After spawn returns (regardless of exit code — capture is best-effort
//     per §3.4 invariant 5): scan the captured stdout buffer for
//     `(?i)session id:\s*([0-9a-f-]{36})`. If matched AND no UUID was
//     previously cached, persist via SetDaemonCLISessionID for next batch.
//  3. Stale-UUID fallback: if the spawn exited non-zero AND we passed a
//     cached UUID AND the output indicates "session not found" (liberal
//     match per §3.4 invariant 5 "if uncertain, log + clear"), clear the
//     cached UUID so the next batch re-captures. Non-fatal; daemon's
//     normal stale-claim handling (§3.4 (d)) kicks in via the existing
//     spawn-error flow in processBatch.
//
// When d.cfg.CLISessionResume is false, spawnCodex behavior is
// byte-identical to Topic 3 v0 (preserved per §9.3 gate 4c defensive
// assertion: flag=false + column non-NULL must still produce
// resume-free argv). §3.4 invariant 1 — envelope payload remains
// load-bearing per batch: the full prompt is always passed, regardless
// of whether resume is in use. Resume only augments argv, never splits
// or elides the prompt content.
func (d *daemon) spawnCodex(ctx context.Context, prompt string) (string, error) {
	session := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}

	// Opt-in gate: Topic 3 v0 byte-identical behavior when flag is false.
	// §3.4 gate-4c + asymmetry defense: the flag is authoritative; a
	// non-NULL column value alone must not change argv shape.
	var cachedSessionID string
	if d.cfg.CLISessionResume {
		cachedSessionID = d.lookupCodexSessionID(ctx, session)
	}

	args := []string{
		"exec",
		"--skip-git-repo-check",
		prompt,
	}
	if cachedSessionID != "" {
		// §4.1 resume-with-cached-UUID argv form:
		//   codex exec resume --skip-git-repo-check <UUID> <prompt>
		args = []string{
			"exec",
			"resume",
			"--skip-git-repo-check",
			cachedSessionID,
			prompt,
		}
	}

	stdout, stderr, runErr := d.execSpawn(ctx, "codex", args, map[string]string{
		"CODEX_SESSION_ID": d.cfg.SessionKey,
	})

	// Best-effort capture / stale-detection. §3.4 invariant 5: any
	// failure path here is a warning, never a fatal — the batch result
	// (stdout, runErr) is returned unchanged to the caller.
	//
	// v0.1.2 fix: pass stderr alongside stdout — real codex 0.121.0
	// emits the banner (and most diagnostics) to stderr; v0.1 only
	// scanned stdout, missing the capture entirely on real CLI runs
	// (E5 probe surfaced the gap).
	if d.cfg.CLISessionResume {
		d.postSpawnCodexResumeHandling(ctx, session, cachedSessionID, stdout, stderr, runErr)
	}

	return stdout, runErr
}

// lookupCodexSessionID reads sessions.daemon_cli_session_id for the
// daemon's (cwd, label). Returns empty string on any SQL error (§3.4
// invariant 5 capture-failure-non-fatal — falling through to fresh
// invocation is always safe). Uses a 5-second timeout so a wedged DB
// can't stall the spawn indefinitely.
func (d *daemon) lookupCodexSessionID(ctx context.Context, session store.Session) string {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(lookupCtx)
	if err != nil {
		d.log.Warn("daemon.cli_session_capture.codex_lookup_open_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
			"msg", "store open failed; proceeding fresh-invocation Arch B (non-fatal per §3.4 invariant 5)",
		)
		return ""
	}
	defer st.Close()
	id, err := st.GetDaemonCLISessionID(lookupCtx, session)
	if err != nil {
		d.log.Warn("daemon.cli_session_capture.codex_lookup_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
			"msg", "lookup failed; proceeding fresh-invocation Arch B (non-fatal per §3.4 invariant 5)",
		)
		return ""
	}
	return id
}

// postSpawnCodexResumeHandling implements the post-spawn side of Arch D
// §6.1 capture + §3.4 invariant 5 stale-UUID fallback for codex. Called
// only when d.cfg.CLISessionResume is true.
//
//   - If cachedSessionID was empty AND the stdout banner yielded a new
//     UUID: persist via SetDaemonCLISessionID for the next batch.
//   - If cachedSessionID was non-empty AND the spawn exited non-zero
//     AND output indicates a stale session (liberal match): clear via
//     ClearDaemonCLISessionID so the next batch re-captures fresh.
//   - All failures (no regex match, SQL error) are warnings — never
//     propagated up; the batch result is untouched.
//
// Note on stderr vs stdout (v0.1.2 fix): real codex 0.121.0 emits the
// session-id banner to STDERR, not stdout. v0.1 scanned stdout only,
// missing the entire capture path on real CLI runs (E5 probe surfaced
// the gap). v0.1.2 execSpawn now captures BOTH streams (stderr also
// teed to operator's stderr via io.MultiWriter so real-time visibility
// is preserved). postSpawnCodexResumeHandling concatenates both and
// scans the combined buffer for the banner regex AND the stale-UUID
// regex — covers any future codex version that splits its output
// across streams differently.
func (d *daemon) postSpawnCodexResumeHandling(ctx context.Context, session store.Session, cachedSessionID, stdout, stderr string, runErr error) {
	// v0.1.2: scan the concatenation of stdout + stderr (newline-joined
	// for clean line boundaries). Real codex 0.121.0 emits banner +
	// most diagnostics to stderr; the JSONL ack marker (when the agent
	// uses fallback mechanism 2) lives in stdout. Concatenation covers
	// both streams without depending on per-codex-version stream choice.
	combined := stdout + "\n" + stderr

	// Stale-UUID-fallback path: we passed a cached UUID, the spawn
	// errored, and the combined output suggests the CLI-side session is
	// gone. Liberal match per task spec: "if uncertain, log + clear".
	if cachedSessionID != "" && runErr != nil && codexStaleSessionRE.MatchString(combined) {
		d.log.Warn("daemon.cli_session_capture.codex_stale_uuid",
			"label", d.cfg.Label,
			"cached_uuid", cachedSessionID,
			"msg", "codex reported stale/unknown session; clearing daemon_cli_session_id (next batch will re-capture per §3.4 invariant 5)",
		)
		clearCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		st, err := sqlitestore.Open(clearCtx)
		if err != nil {
			d.log.Warn("daemon.cli_session_capture.codex_clear_open_failed",
				"label", d.cfg.Label,
				"err", err.Error(),
			)
			return
		}
		defer st.Close()
		if err := st.ClearDaemonCLISessionID(clearCtx, session); err != nil {
			d.log.Warn("daemon.cli_session_capture.codex_clear_failed",
				"label", d.cfg.Label,
				"err", err.Error(),
			)
		}
		return
	}

	// First-capture path: previously-uncached label + this spawn's
	// stdout banner yielded a UUID → persist. No-op if we already had
	// a cached UUID (the already-cached identity is the stable one;
	// resumed spawns may still emit a banner line with the same or a
	// different UUID depending on codex-vendor behavior — do not
	// clobber the persisted value on a re-spawn).
	if cachedSessionID != "" {
		return
	}

	match := codexBannerSessionRE.FindStringSubmatch(combined)
	if match == nil {
		d.log.Warn("daemon.cli_session_capture.codex_no_match",
			"label", d.cfg.Label,
			"stdout_bytes", len(stdout),
			"stderr_bytes", len(stderr),
			"msg", "codex banner regex did not match in stdout+stderr; leaving daemon_cli_session_id NULL (fixture drift or banner-format change — next batch will re-attempt capture)",
		)
		return
	}
	captured := match[1]

	setCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(setCtx)
	if err != nil {
		d.log.Warn("daemon.cli_session_capture.codex_persist_open_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return
	}
	defer st.Close()
	if err := st.SetDaemonCLISessionID(setCtx, session, captured); err != nil {
		d.log.Warn("daemon.cli_session_capture.codex_persist_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return
	}
	d.log.Info("daemon.cli_session_capture.codex_captured",
		"label", d.cfg.Label,
		"uuid", captured,
	)
}

// spawnGemini runs `gemini -p <prompt>` with stdin=/dev/null. §4
// bullet 4 gemini timeout-units is already handled by install-script
// on the receive side; daemon just needs to export the env var.
//
// Topic 3 v0.1 Arch D (§4.2 + §6.2): when d.cfg.CLISessionResume is
// true, the spawn helper captures the gemini vendor's session-UUID on
// first batch via a --list-sessions delta-snapshot (BEFORE/AFTER the
// no-resume spawn) and persists it to sessions.daemon_cli_session_id.
// On subsequent batches, re-queries --list-sessions to translate the
// cached UUID into its CURRENT 1-based index (indices shift as new
// sessions push older ones up the list per gemini 0.38.2 --help:
// "Use 'latest' for most recent or index number (e.g. --resume 5)"),
// then invokes `gemini --resume <N> -p <prompt>`. UUID stays the
// stable persisted identity; the index is computed at resume time.
//
// v3 amendment (Checkpoint 1 finding): gemini 0.38.2's --resume is
// documented as accepting "latest" or <index> only; direct-UUID-resume
// works empirically but is undocumented. v0.1 builds on the documented
// index-addressing API. v1+ may collapse the translation step if a
// future gemini release documents direct-UUID-resume.
//
// §3.4 invariants honored here:
//   - Invariant 1 (envelope load-bearing per batch): the full prompt
//     is ALWAYS passed via -p regardless of resume state; --resume
//     only prepends argv, never replaces the prompt content.
//   - Invariant 2 (no session-store introspection): the daemon only
//     calls `gemini --list-sessions`; it never reads ~/.gemini/*
//     files or parses gemini-internal session history.
//   - Invariant 5 (capture-failure non-fatal): zero new UUIDs, >1 new
//     UUIDs (concurrent-gemini race per §6.2 + §10 Q2), or
//     --list-sessions exec failure — all log a warning and leave the
//     column NULL; the spawn still proceeds Arch B fresh-invocation.
//
// When d.cfg.CLISessionResume is false the helper is byte-identical
// to the Topic 3 v0 form (no --list-sessions calls, no DB reads).
// Gate 5d defensive-assertion verifies that flag=false + column
// non-NULL still produces a resume-free argv.
func (d *daemon) spawnGemini(ctx context.Context, prompt string) (string, error) {
	if !d.cfg.CLISessionResume {
		// Topic 3 v0 byte-identical path. Gate 5d asserts this.
		args := []string{
			"-p",
			prompt,
		}
		stdout, _, err := d.execSpawn(ctx, "gemini", args, map[string]string{
			"GEMINI_SESSION_ID": d.cfg.SessionKey,
		})
		return stdout, err
	}

	// Arch D enabled. Consult the DB for a cached UUID — fresh store
	// handle per invocation matches the completeBatch / claimTick
	// pattern; short timeout bounds any wedged-DB stall.
	cachedUUID := d.readCachedGeminiSessionID(ctx)

	if cachedUUID != "" {
		// Resume path: re-query --list-sessions, translate UUID →
		// current index. If the UUID is no longer present (CLI-side
		// eviction / vendor GC / operator deletion), NULL the column
		// and fall through to Arch B fresh-invocation for this batch
		// (next batch will re-capture via the capture path below).
		idx, found, err := d.lookupGeminiSessionIndex(ctx, cachedUUID)
		switch {
		case err != nil:
			// §3.4 invariant 5: --list-sessions failure is non-fatal.
			// DO NOT clear the cached UUID — the UUID may still be
			// valid; this is a transient exec error, not evidence of
			// eviction. Proceed Arch B for this batch.
			d.log.Warn("daemon.cli_session_resume.gemini_list_sessions_failed",
				"label", d.cfg.Label,
				"err", err.Error(),
				"msg", "--list-sessions re-query failed; proceeding Arch B for this batch",
			)
		case found:
			// Happy path: pass INDEX (not UUID) to --resume per v3
			// amendment / §4.2.
			args := []string{
				"--resume", strconv.Itoa(idx),
				"-p",
				prompt,
			}
			stdout, _, err := d.execSpawn(ctx, "gemini", args, map[string]string{
				"GEMINI_SESSION_ID": d.cfg.SessionKey,
			})
			return stdout, err
		default:
			// UUID-not-found (stale) per §6.2 Resume subsection. Clear
			// the column + fall through to capture path below.
			d.log.Warn("daemon.cli_session_resume.gemini_stale_uuid",
				"label", d.cfg.Label,
				"cached_uuid", cachedUUID,
				"msg", "cached UUID not in current --list-sessions (CLI-side eviction / GC / operator deletion); NULL-ing column + proceeding Arch B; next batch will re-capture",
			)
			d.clearCachedGeminiSessionID(ctx)
			cachedUUID = "" // Trigger capture path below.
		}
	}

	// Capture path: no cached UUID (first batch OR stale UUID just
	// cleared). Snapshot BEFORE, spawn without --resume, snapshot
	// AFTER, persist the new UUID via set-difference.
	var before map[string]int
	if cachedUUID == "" {
		// Best-effort BEFORE snapshot. Failure → log + skip capture
		// this batch per §3.4 invariant 5.
		snap, err := d.runGeminiListSessions(ctx)
		if err != nil {
			d.log.Warn("daemon.cli_session_capture.gemini_before_failed",
				"label", d.cfg.Label,
				"err", err.Error(),
				"msg", "--list-sessions BEFORE snapshot failed; skipping capture for this batch",
			)
			// Leave before==nil so we skip the AFTER snapshot too.
		} else {
			before = snap
		}
	}

	args := []string{
		"-p",
		prompt,
	}
	out, _, spawnErr := d.execSpawn(ctx, "gemini", args, map[string]string{
		"GEMINI_SESSION_ID": d.cfg.SessionKey,
	})

	// AFTER snapshot + delta. Only attempt if BEFORE succeeded.
	// Capture-failure is always non-fatal: (out, spawnErr) flow to
	// the caller untouched.
	if before != nil {
		after, err := d.runGeminiListSessions(ctx)
		if err != nil {
			d.log.Warn("daemon.cli_session_capture.gemini_after_failed",
				"label", d.cfg.Label,
				"err", err.Error(),
				"msg", "--list-sessions AFTER snapshot failed; leaving column NULL",
			)
		} else {
			newUUIDs := make([]string, 0, 1)
			for uuid := range after {
				if _, inBefore := before[uuid]; !inBefore {
					newUUIDs = append(newUUIDs, uuid)
				}
			}
			switch len(newUUIDs) {
			case 1:
				d.persistCapturedGeminiSessionID(ctx, newUUIDs[0])
			case 0:
				// §6.2: "gemini may have suppressed session creation
				// on this invocation." Leave NULL per §3.4 invariant
				// 5; next batch re-attempts capture.
				d.log.Warn("daemon.cli_session_capture.gemini_no_new_uuid",
					"label", d.cfg.Label,
					"before_count", len(before),
					"after_count", len(after),
					"msg", "no new UUID in --list-sessions delta; gemini may have suppressed session creation on this invocation; leaving column NULL",
				)
			default:
				// CONSERVATIVE per §6.2 + §10 Q2: concurrent-gemini
				// race. Daemon does NOT pick a winner (mtime or
				// otherwise) because silent pick risks grabbing the
				// operator's interactive gemini session as the
				// daemon's resume target (contaminating both).
				// Warn-and-NULL is loud-recoverable: operator sees
				// the log, can quiesce other gemini invocations OR
				// set per-daemon GEMINI_CONFIG_DIR per §6.2
				// operator-setup paragraph.
				d.log.Warn("daemon.cli_session_capture.gemini_race_detected",
					"label", d.cfg.Label,
					"new_uuid_count", len(newUUIDs),
					"new_uuids", newUUIDs,
					"msg", "more than one new UUID between BEFORE/AFTER --list-sessions snapshots; concurrent-gemini race — leaving column NULL; consider GEMINI_CONFIG_DIR per operator guide",
				)
			}
		}
	}

	return out, spawnErr
}

// geminiSessionLineRE matches one row of `gemini --list-sessions`
// output. Gemini 0.38.2 format (fixture: tests/fixtures/cli-session/
// gemini-list-sessions.txt):
//
//	  1. PROMPT_PREVIEW (1 day ago) [UUID]
//
// Leading whitespace, 1-based index, a dot, space, arbitrary prompt
// preview (may include its own punctuation — we use a greedy match up
// to the FINAL `[UUID]` suffix), then the UUID in brackets at end-of-
// line. The header line "Available sessions for this project (N):"
// and blank lines do not match and are skipped.
var geminiSessionLineRE = regexp.MustCompile(`^\s*(\d+)\.\s+.*\[([0-9a-fA-F-]{36})\]\s*$`)

// parseGeminiListSessions parses raw `gemini --list-sessions` stdout
// into a map of UUID → 1-based index. Returns an empty map on no
// matches (benign: fresh install with empty session store, or header-
// only output). Duplicate UUIDs in the output (shouldn't happen per
// gemini's model) keep the first-seen index defensively.
func parseGeminiListSessions(data []byte) map[string]int {
	out := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	// Bump buffer so long prompt previews don't trip the default 64
	// KiB line cap.
	buf := make([]byte, 0, 1<<20)
	scanner.Buffer(buf, 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		m := geminiSessionLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx < 1 {
			continue
		}
		uuid := strings.ToLower(m[2])
		if _, exists := out[uuid]; !exists {
			out[uuid] = idx
		}
	}
	return out
}

// runGeminiListSessions execs `gemini --list-sessions` with the same
// env inheritance (including GEMINI_CONFIG_DIR if the operator set it
// per §6.2 for race-avoidance) and cwd as the upcoming spawn. Short
// 5-second timeout — this is a local enumeration query, never a model
// call. Returns the UUID → 1-based-index map or an exec/parse error.
//
// Per §3.4 invariant 2: this is the ONLY permitted introspection of
// gemini's session-store state. The daemon does NOT open or parse
// ~/.gemini/* files.
func (d *daemon) runGeminiListSessions(ctx context.Context) (map[string]int, error) {
	bin := "gemini"
	if override := os.Getenv("AGENT_COLLAB_DAEMON_GEMINI_BIN"); override != "" {
		bin = override
	}

	timeoutSec := resolveGeminiListTimeoutSec()
	listCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(listCtx, bin, "--list-sessions")
	cmd.Dir = d.cfg.CWD
	cmd.Stdin = nil
	// Inherit full env — GEMINI_CONFIG_DIR (operator opt-in per §6.2)
	// rides along unchanged. The --list-sessions enumeration does not
	// need the daemon-spawn flags (AGENT_COLLAB_DAEMON_SPAWN, session
	// key) because there's no prompt + no hook short-circuit concern.
	cmd.Env = os.Environ()
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if listCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("gemini --list-sessions timed out after %ds (override via AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT)", timeoutSec)
		}
		return nil, fmt.Errorf("gemini --list-sessions: %w", err)
	}
	return parseGeminiListSessions([]byte(out.String())), nil
}

// resolveGeminiListTimeoutSec returns the effective timeout (in seconds)
// for `gemini --list-sessions`. v0.1.2 fix: real gemini 0.38.2 takes
// ~5.3s to enumerate against an operator-sized config (E6 probe surfaced
// the gap; v0.1's hardcoded 5s deterministically missed). Default raised
// to 15s; AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT env var lets operators
// tune up further (very-large session stores) or down (CI / fixture
// runs that want fast-fail).
//
// Invalid values (non-int, ≤0) fall back to the 15s default with no
// error — capture-failure path per §3.4 invariant 5 catches downstream
// problems anyway, so a config typo here shouldn't block the daemon.
func resolveGeminiListTimeoutSec() int {
	const defaultSec = 15
	v := os.Getenv("AGENT_COLLAB_DAEMON_GEMINI_LIST_TIMEOUT")
	if v == "" {
		return defaultSec
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultSec
	}
	return n
}

// lookupGeminiSessionIndex re-queries `gemini --list-sessions` and
// returns the current 1-based index for the given UUID. found=false
// means the UUID is no longer in the list (CLI-side eviction / GC /
// operator deletion); caller clears the column and proceeds Arch B.
// A non-nil error means --list-sessions itself failed (exec err,
// timeout); caller logs + proceeds Arch B but DOES NOT clear the
// column on this path (transient exec errors shouldn't cost the
// cached identity).
func (d *daemon) lookupGeminiSessionIndex(ctx context.Context, uuid string) (idx int, found bool, err error) {
	snap, err := d.runGeminiListSessions(ctx)
	if err != nil {
		return 0, false, err
	}
	normalized := strings.ToLower(uuid)
	idx, found = snap[normalized]
	return idx, found, nil
}

// readCachedGeminiSessionID opens a fresh store handle, reads the
// cached UUID, closes. Returns "" on any error (treated as "no
// cache") — §3.4 invariant 5: DB-read failure is non-fatal; the
// batch just proceeds Arch B + re-attempts capture.
func (d *daemon) readCachedGeminiSessionID(ctx context.Context) string {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		d.log.Warn("daemon.cli_session_resume.gemini_store_open_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return ""
	}
	defer st.Close()
	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	id, err := st.GetDaemonCLISessionID(openCtx, self)
	if err != nil {
		d.log.Warn("daemon.cli_session_resume.gemini_get_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return ""
	}
	return id
}

// persistCapturedGeminiSessionID writes the captured UUID to the
// sessions row. Non-fatal on error per §3.4 invariant 5.
func (d *daemon) persistCapturedGeminiSessionID(ctx context.Context, uuid string) {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		d.log.Warn("daemon.cli_session_capture.gemini_store_open_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return
	}
	defer st.Close()
	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	if err := st.SetDaemonCLISessionID(openCtx, self, uuid); err != nil {
		d.log.Warn("daemon.cli_session_capture.gemini_set_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return
	}
	d.log.Info("daemon.cli_session_capture.gemini_captured",
		"label", d.cfg.Label,
		"uuid", uuid,
	)
}

// clearCachedGeminiSessionID NULLs the column on stale-UUID fallback.
// Non-fatal on error per §3.4 invariant 5 — if clear fails, the
// daemon proceeds Arch B for this batch; next batch will attempt a
// fresh resume and may re-detect the stale-UUID condition (loop is
// bounded by operator intervention via the SQL escape hatch per
// §3.3 TERTIARY or `peer-inbox daemon-reset-session`).
func (d *daemon) clearCachedGeminiSessionID(ctx context.Context) {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := sqlitestore.Open(openCtx)
	if err != nil {
		d.log.Warn("daemon.cli_session_resume.gemini_clear_open_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
		return
	}
	defer st.Close()
	self := store.Session{CWD: d.cfg.CWD, Label: d.cfg.Label}
	if err := st.ClearDaemonCLISessionID(openCtx, self); err != nil {
		d.log.Warn("daemon.cli_session_resume.gemini_clear_failed",
			"label", d.cfg.Label,
			"err", err.Error(),
		)
	}
}

// execSpawn is the shared spawn implementation. Stdin is always
// /dev/null (even for Claude — prevents the interactive-stdin hang
// trap across all three). Stdout is captured for the ack-marker
// scanner; stderr is captured for codex banner regex (v0.1.2 fix —
// real codex 0.121.0 writes banner + diagnostics to stderr, not stdout)
// AND inherited via io.MultiWriter to the daemon's own stderr so
// operators still see CLI errors in real time.
//
// Returns (stdout, stderr, err). Callers that only care about stdout
// (claude/gemini ack-marker scan, empty-response check) can _-discard
// the stderr return; callers that need both streams (codex Arch D
// post-spawn handling) use both.
//
// Per-CLI env vars are merged with the daemon-neutral vars:
//   - AGENT_COLLAB_SESSION_KEY=<session-key>    (hook SESSION_KEY_ENV_CANDIDATES fallback)
//   - AGENT_COLLAB_DAEMON_SPAWN=1               (§3.4 (f) hook short-circuit)
//   - PATH                                      (inherited from daemon process)
func (d *daemon) execSpawn(ctx context.Context, bin string, args []string, extraEnv map[string]string) (string, string, error) {
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

	// Capture stdout for ack-marker scanning. Capture stderr too (v0.1.2
	// fix — codex banner lives on stderr) AND tee to the daemon's own
	// stderr via io.MultiWriter so operators still see CLI errors in
	// real time.
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

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
			return outBuf.String(), errBuf.String(), context.DeadlineExceeded
		}
		return outBuf.String(), errBuf.String(), runErr
	}
	return outBuf.String(), errBuf.String(), nil
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

