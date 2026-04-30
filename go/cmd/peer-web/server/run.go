package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// v3.11 Phase 1 — every dispatch produces a durable card_runs row that
// the drainer reads (capacity gate, dedup) and the reaper scans on
// peer-web boot. The wave-shaped drainer in board_start.go is now a
// long-running ticker; this file owns the per-card dispatch + monitor
// logic shared between manual run (handleCardRun) and the standing
// drainer.

const (
	runnerLogDir     = "/tmp/peer-inbox/runners"
	runnerCmdTimeout = 30 * time.Minute // safety ceiling on a single spawn
)

// handleCardRun — POST /api/cards/{id}/run
//
// Verifies the card is in todo + ready, claims it to a generated label,
// flips status to in_progress, then forks a `claude -p` subprocess.
// Non-blocking — returns once the worker has been started.
func (s *Server) handleCardRun(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "open store: "+err.Error())
		return
	}
	defer st.Close()

	card, err := st.GetCard(ctx, id)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		writeJSONError(w, http.StatusNotFound, "card not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Eligibility — manual run only fires on ready todos. The drainer's
	// own dispatch path bypasses this check (it pre-filters candidates).
	if card.Status != sqlitestore.CardStatusTodo {
		writeJSONError(w, http.StatusConflict,
			fmt.Sprintf("card status is %s, expected todo", card.Status))
		return
	}
	// v3.12.4.5 — epics dispatch the decomposer, not real work. They're
	// inception-level by design (children don't exist yet, so nothing to
	// block on). Skip the ready gate so an operator can fire a fresh
	// epic immediately even if its blocker graph is unusual.
	if !card.Ready && card.Kind != sqlitestore.CardKindEpic {
		writeJSONError(w, http.StatusConflict,
			"card has open blockers — run the blockers first")
		return
	}
	if card.ClaimedBy != "" {
		writeJSONError(w, http.StatusConflict,
			fmt.Sprintf("card already claimed by %s", card.ClaimedBy))
		return
	}

	disp, err := dispatchWorkerForCard(ctx, st, card, sqlitestore.CardRunTriggerManual)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCardAlreadyClaimed) {
			writeJSONError(w, http.StatusConflict, "card was claimed mid-flight")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, disp)
}

// dispatchResult mirrors the JSON body /api/cards/{id}/run returns.
type dispatchResult struct {
	CardID      int64  `json:"card_id"`
	RunID       int64  `json:"run_id"`
	WorkerLabel string `json:"worker_label"`
	PID         int    `json:"pid"`
	LogPath     string `json:"log_path"`
	PromptBytes int    `json:"prompt_bytes"`
}

// dispatchWorkerForCard does the full dispatch — insert card_runs row,
// claim card, flip status, resolve context, spawn worker, attach
// monitor goroutine that finalizes the run on child exit.
//
// Caller owns eligibility checks (status=todo, ready, no claimer).
// trigger ∈ {manual, drainer, matchmaker}.
func dispatchWorkerForCard(
	ctx context.Context,
	st webStore,
	card *sqlitestore.Card,
	trigger string,
) (map[string]any, error) {
	suffix, err := randHex(4)
	if err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}

	// v3.12.4 — pool-aware dispatch. If a pool member matches (by
	// designation, role, capacity), use its worker_cmd + tag the run.
	// Empty pool / no match → label = "runner-{id}-{suffix}" + worker
	// argv comes from AGENT_COLLAB_WORKER_CMD (Phase 1 fallback).
	chosen, err := st.PickAgentForCard(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("pick agent: %w", err)
	}

	label := fmt.Sprintf("runner-%d-%s", card.ID, suffix)
	var agentID int64
	var agentArgv []string
	if chosen != nil {
		label = fmt.Sprintf("%s-%d-%s", chosen.Label, card.ID, suffix)
		agentID = chosen.ID
		if chosen.WorkerCmd != "" {
			agentArgv = strings.Fields(chosen.WorkerCmd)
		} else {
			agentArgv = defaultArgvForRuntime(chosen.Runtime)
		}
	}

	// Insert the run row first so the reaper has something to find if we
	// crash mid-dispatch. log_path is filled later (we don't know it
	// yet — depends on filesystem writability).
	run, err := st.CreateCardRun(ctx, sqlitestore.CreateCardRunParams{
		CardID:      card.ID,
		PairKey:     card.PairKey,
		WorkerLabel: label,
		Trigger:     trigger,
		AgentID:     agentID,
	})
	if err != nil {
		return nil, fmt.Errorf("create card_run: %w", err)
	}

	// Claim + status flip. Failures here mark the run as failed before
	// any process is spawned.
	if err := retryOnBusy(func() error {
		_, e := st.ClaimCard(ctx, card.ID, label, false)
		return e
	}); err != nil {
		_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
		return nil, fmt.Errorf("claim: %w", err)
	}
	if err := retryOnBusy(func() error {
		_, e := st.UpdateCardStatus(ctx, card.ID, sqlitestore.CardStatusInProgress, label)
		return e
	}); err != nil {
		_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
		return nil, fmt.Errorf("status: %w", err)
	}

	// Resolve context + build prompt.
	refs := contextRefsShape{}
	if card.ContextRefs != "" {
		if err := json.Unmarshal([]byte(card.ContextRefs), &refs); err != nil {
			_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
			return nil, fmt.Errorf("context_refs is not valid JSON: %w", err)
		}
	}
	resolved := resolveContext(ctx, st, refs)
	// v3.12.4.6: fetch the prior handoff so buildWorkerPrompt can prepend
	// it. Only meaningful when track_handoffs is true (build skips the
	// section otherwise) but we always try the lookup — the cost is one
	// indexed query and the function returns (nil, nil) cleanly when
	// there's nothing to prepend.
	var priorHandoff *sqlitestore.CardEvent
	if card.TrackHandoffs {
		ph, herr := st.LatestHandoffEvent(ctx, card.ID)
		if herr != nil {
			fmt.Fprintf(os.Stderr, "card_runs: latest handoff lookup failed card=%d: %v\n", card.ID, herr)
		}
		priorHandoff = ph
	}
	prompt := buildWorkerPrompt(card, label, resolved, priorHandoff)

	if err := os.MkdirAll(runnerLogDir, 0o755); err != nil {
		_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
		return nil, fmt.Errorf("log dir: %w", err)
	}
	logPath := filepath.Join(runnerLogDir,
		fmt.Sprintf("run-%d-%s.log", card.ID, time.Now().UTC().Format("20060102-150405")))
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
		return nil, fmt.Errorf("log file: %w", err)
	}

	// v3.12.4.6: when track_handoffs is set, rewrite --max-turns to the
	// handoff budget so the worker has explicit room to write a structured
	// handoff before the runtime cuts it off. Resolve the fallback chain
	// here (instead of inside spawnWorker) so the tuner sees the actual
	// argv that will be exec'd. spawnWorker keeps its own fallback as a
	// defensive belt-and-suspenders.
	if card.TrackHandoffs {
		if len(agentArgv) == 0 {
			if override := os.Getenv("AGENT_COLLAB_WORKER_CMD"); override != "" {
				agentArgv = strings.Fields(override)
			} else {
				agentArgv = append([]string(nil), defaultWorkerArgv...)
			}
		}
		agentArgv = tuneArgvForHandoffs(agentArgv)
	}
	// Track 1 #1: per-board project root → cmd.Dir so workers land in
	// the right project tree. Empty/unset → inherit peer-web cwd
	// (prior behavior). Look up settings once per dispatch (cheap;
	// indexed by pair_key).
	cwd := ""
	if bs, err := st.GetBoardSettings(ctx, card.PairKey); err == nil && bs.ProjectRoot != "" {
		cwd = bs.ProjectRoot
	}
	cmd, err := spawnWorker(prompt, logFile, agentArgv, cwd)
	if err != nil {
		_ = logFile.Close()
		_ = st.FinishCardRun(ctx, run.ID, sqlitestore.CardRunStatusFailed, -1)
		return nil, fmt.Errorf("spawn: %w", err)
	}
	pid := cmd.Process.Pid

	// Update the run row with pid + log_path so observers (drawer
	// run-history, future matchmaker) see them immediately.
	if err := st.UpdateCardRunPID(ctx, run.ID, pid); err != nil {
		// Non-fatal — worker is already running; we just lose pid in DB.
		fmt.Fprintf(os.Stderr, "card_runs: pid update failed run=%d pid=%d: %v\n", run.ID, pid, err)
	}
	// Refresh the row so we can return log_path. Cheap second query;
	// keeps the response shape honest.
	if err := setCardRunLogPath(ctx, st, run.ID, logPath); err != nil {
		fmt.Fprintf(os.Stderr, "card_runs: log_path update failed run=%d: %v\n", run.ID, err)
	}

	// Append run_dispatch event with the run_id so the timeline can
	// link to the runs tab without a JOIN.
	dispatchMetaMap := map[string]any{
		"run_id":       run.ID,
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
		"trigger":      trigger,
	}
	if chosen != nil {
		dispatchMetaMap["agent_id"] = chosen.ID
		dispatchMetaMap["agent_label"] = chosen.Label
		dispatchMetaMap["agent_runtime"] = chosen.Runtime
	}
	dispatchMeta, _ := json.Marshal(dispatchMetaMap)
	if _, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: card.ID, Kind: sqlitestore.CardEventRunDispatch,
		Author: "system",
		Body:   fmt.Sprintf("worker %s dispatched (pid %d, run %d)", label, pid, run.ID),
		Meta:   string(dispatchMeta),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "card_events: run_dispatch record failed: %v\n", err)
	}

	// Detached monitor — owns the cmd.Wait + log file lifecycle + run
	// finalization. Detached so the caller (HTTP handler or drainer
	// tick) can return immediately.
	go monitorWorker(run.ID, card.ID, label, cmd, logFile)

	resp := map[string]any{
		"card_id":      card.ID,
		"run_id":       run.ID,
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
	}
	if chosen != nil {
		resp["agent_id"] = chosen.ID
		resp["agent_label"] = chosen.Label
	}
	return resp, nil
}

// setCardRunLogPath patches log_path on a running row. Separate helper
// because the public store interface intentionally doesn't expose a
// general "update card_run" verb (each field has a defined moment).
func setCardRunLogPath(ctx context.Context, st webStore, runID int64, logPath string) error {
	// Cheating slightly — the webStore interface doesn't have a general
	// updater. We re-open a writable connection through the same
	// concrete store. Phase 1 keeps the surface area minimal; if more
	// of these accumulate, add a proper UpdateCardRunFields helper.
	type logPathUpdater interface {
		UpdateCardRunLogPath(ctx context.Context, id int64, logPath string) error
	}
	if u, ok := st.(logPathUpdater); ok {
		return u.UpdateCardRunLogPath(ctx, runID, logPath)
	}
	return nil
}

// monitorWorker waits for the spawned process to exit and finalizes the
// card_runs row. Owns the log file close. Runs detached.
//
// Exit-status mapping:
//
//	cmd.Wait() returned nil → 'completed' (exit_code=0)
//	cmd.Wait() returned ExitError → 'failed' (exit_code=actual)
//	cmd.Wait() returned other error → 'failed' (exit_code=-1)
//
// The 'cancelled' and 'lost' statuses are written elsewhere (Phase 2
// cancel verb; reaper boot scan) — monitor never writes them itself.
func monitorWorker(runID, cardID int64, label string, cmd *exec.Cmd, logFile *os.File) {
	defer func() { _ = logFile.Close() }()

	waitErr := cmd.Wait()
	status := sqlitestore.CardRunStatusCompleted
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
		status = sqlitestore.CardRunStatusFailed
	}

	// New context — the originating request is long gone.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := storeOpen(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitorWorker: open store run=%d: %v\n", runID, err)
		return
	}
	defer st.Close()

	if err := st.FinishCardRun(ctx, runID, status, exitCode); err != nil {
		fmt.Fprintf(os.Stderr, "monitorWorker: FinishCardRun run=%d: %v\n", runID, err)
	}
	completeMeta, _ := json.Marshal(map[string]any{
		"run_id":    runID,
		"status":    status,
		"exit_code": exitCode,
	})
	if _, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: cardID, Kind: sqlitestore.CardEventRunComplete,
		Author: "system",
		Body:   fmt.Sprintf("worker %s exited %s (code %d)", label, status, exitCode),
		Meta:   string(completeMeta),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "monitorWorker: run_complete event run=%d: %v\n", runID, err)
	}

	finalizeDecomposerExit(ctx, st, cardID, label, status, exitCode)
	finalizeHandoffSelfPromotion(ctx, st, cardID, label, status)
}

// finalizeHandoffSelfPromotion sets track_handoffs=true on a card whose
// worker exited cleanly without reaching a terminal-or-todo state — the
// signal that the worker likely hit its turn budget mid-task. The next
// dispatch then teaches the handoff discipline so the worker has a
// structured way to preserve continuity.
//
// Skipped for epics (decomposer flow has its own finalizer) and for
// cards already track_handoffs=true (no point re-stamping).
//
// Plan §8 H3 / Q11.F.
func finalizeHandoffSelfPromotion(ctx context.Context, st webStore, cardID int64, label, runStatus string) {
	if runStatus != sqlitestore.CardRunStatusCompleted {
		return // worker crashed — separate failure mode, don't promote
	}
	card, err := st.GetCard(ctx, cardID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "finalizeHandoffSelfPromotion: GetCard %d: %v\n", cardID, err)
		return
	}
	if card.Kind == sqlitestore.CardKindEpic {
		return // decomposer path owns its own promotion logic
	}
	if card.TrackHandoffs {
		return // already promoted; nothing to do
	}
	if card.Status != sqlitestore.CardStatusInProgress {
		return // worker reached a terminal-or-todo state — no need
	}
	on := true
	if _, err := st.UpdateCardFields(ctx, cardID,
		sqlitestore.UpdateCardFieldsParams{TrackHandoffs: &on},
		"drainer-self-promote",
	); err != nil {
		fmt.Fprintf(os.Stderr, "finalizeHandoffSelfPromotion: promote card=%d: %v\n", cardID, err)
		return
	}
	// Comment so an operator can see why track_handoffs flipped on without
	// them touching it. body_update event is also recorded by
	// UpdateCardFields, but it's terse — this comment names the cause.
	if _, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: cardID, Kind: sqlitestore.CardEventComment, Author: "system",
		Body: fmt.Sprintf("track_handoffs auto-enabled — worker %s exited cleanly with card still in_progress (likely hit turn budget); next dispatch will include handoff discipline", label),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "finalizeHandoffSelfPromotion: comment card=%d: %v\n", cardID, err)
	}
}

// finalizeDecomposerExit handles epic post-run housekeeping. When an
// epic's decomposer exits cleanly with at least one child created
// (BlockeeIDs non-empty), promote the epic to done. If the worker
// exited cleanly without creating any children, leave the epic in
// in_progress and emit an error event so an operator can review.
//
// Non-epic cards are no-ops.
//
// Errors during finalization are logged but never bubble — monitor's
// run_complete write is the must-succeed action; this is icing.
func finalizeDecomposerExit(ctx context.Context, st webStore, cardID int64, label, runStatus string, exitCode int) {
	card, err := st.GetCard(ctx, cardID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "finalizeDecomposerExit: GetCard %d: %v\n", cardID, err)
		return
	}
	if card.Kind != sqlitestore.CardKindEpic {
		return
	}
	if runStatus != sqlitestore.CardRunStatusCompleted {
		// Decomposer crashed — leave the epic alone. Operator can re-run.
		return
	}
	if len(card.BlockeeIDs) > 0 {
		// At least one child was created (children depend on the epic
		// via add-dependency, so they show up as blockees). Promote.
		if _, err := st.UpdateCardStatus(ctx, cardID, sqlitestore.CardStatusDone, "decomposer-auto-promote"); err != nil {
			fmt.Fprintf(os.Stderr, "finalizeDecomposerExit: promote epic %d: %v\n", cardID, err)
		}
		return
	}
	// Clean exit but no children — decomposer punted. Record an error
	// event so the operator can read the rationale (worker should have
	// posted a comment) and either edit the body or cancel.
	errMeta, _ := json.Marshal(map[string]any{
		"reason":    "no_children_emitted",
		"exit_code": exitCode,
		"worker":    label,
	})
	if _, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: cardID, Kind: sqlitestore.CardEventComment,
		Author: "system",
		Body:   fmt.Sprintf("decomposer exited cleanly without creating children — epic needs human review (worker %s)", label),
		Meta:   string(errMeta),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "finalizeDecomposerExit: error event %d: %v\n", cardID, err)
	}
}

// spawnWorker launches the worker process non-blocking, redirecting
// stdout + stderr to logFile. Returns the *exec.Cmd so the caller can
// attach a monitor goroutine. Children inherit our process group, so
// they receive SIGTERM when peer-web shuts down (the "die with parent"
// rule from the Phase 1 plan §12).
//
// Argv resolution priority (v3.12.4):
//
//  1. perAgentArgv if non-nil — chosen pool member's worker_cmd or
//     defaultArgvForRuntime(agent.runtime). Pool routing wins.
//  2. AGENT_COLLAB_WORKER_CMD env (whitespace-split, no shell expansion)
//     — test harnesses, ad-hoc overrides.
//  3. Phase 1 default: claude -p --permission-mode=bypassPermissions
//     --max-turns=40.
//
// The prompt is piped on stdin in every case.
//
// cwd: when non-empty, sets cmd.Dir so the worker lands in that directory
// instead of inheriting peer-web's cwd. Track 1 #1 per-board
// project_root binding. Empty preserves prior behavior.
func spawnWorker(prompt string, logFile *os.File, perAgentArgv []string, cwd string) (*exec.Cmd, error) {
	argv := perAgentArgv
	if len(argv) == 0 {
		if override := os.Getenv("AGENT_COLLAB_WORKER_CMD"); override != "" {
			argv = strings.Fields(override)
		}
	}
	if len(argv) == 0 {
		argv = defaultWorkerArgv
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("spawnWorker: empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if cwd != "" {
		cmd.Dir = cwd
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

var defaultWorkerArgv = []string{
	"claude",
	"-p",
	"--permission-mode=bypassPermissions",
	"--max-turns=40",
}

// tuneArgvForHandoffs rewrites --max-turns=N → --max-turns=HandoffTurnBudget
// so a track_handoffs worker has explicit room to write a structured
// handoff. Pass-through for argv that doesn't carry --max-turns (e.g. pi
// runtime) — the worker prompt addendum still teaches the budget.
//
// Returns a fresh slice; never mutates the input.
func tuneArgvForHandoffs(argv []string) []string {
	out := make([]string, len(argv))
	target := fmt.Sprintf("--max-turns=%d", HandoffTurnBudget)
	for i, a := range argv {
		if strings.HasPrefix(a, "--max-turns=") {
			out[i] = target
		} else {
			out[i] = a
		}
	}
	return out
}

// defaultArgvForRuntime returns the runtime-specific default argv for
// a pool-routed dispatch when the agent's worker_cmd field is empty.
// Pool members can opt-in to runtime defaults by leaving worker_cmd
// blank; explicit worker_cmd always wins.
func defaultArgvForRuntime(runtime string) []string {
	switch runtime {
	case sqlitestore.AgentRuntimeClaude:
		return defaultWorkerArgv
	case sqlitestore.AgentRuntimePi:
		// pi headless one-shot — reads prompt on stdin, exits when done.
		// The exact flag inventory varies across pi versions; keep this
		// minimal and let operators override via worker_cmd when their
		// pi build needs different switches.
		return []string{"pi", "--once"}
	}
	return defaultWorkerArgv
}

// buildWorkerPrompt composes the prompt headless `claude -p` receives
// via stdin. The structure matches the manual subagent dispatch — same
// workflow rules, same output contract.
//
// v3.12.4.5: when card.Kind == "epic", returns the decomposer prompt
// instead of a regular worker prompt. The decomposer's job is to split
// the epic into N child cards via card_create + card_add_dependency MCP
// calls, then exit.
//
// v3.12.4.5: when card.Splittable is true, appends the mid-session split
// addendum so the worker knows it MAY split if it discovers hidden scope.
//
// v3.12.4.6: when card.TrackHandoffs is true, appends the handoff
// discipline addendum so the worker knows to call card_handoff near
// its turn budget. priorHandoff (when non-nil) is rendered as a
// "## Previous handoff" section prepended before the body so the next
// worker resumes from where the previous session left off.
func buildWorkerPrompt(
	card *sqlitestore.Card,
	label string,
	resolved map[string]any,
	priorHandoff *sqlitestore.CardEvent,
) string {
	if card.Kind == sqlitestore.CardKindEpic {
		return buildDecomposerPrompt(card, label)
	}
	var b strings.Builder

	fmt.Fprintf(&b, "You are a headless kanban worker spawned to drain a single card.\n")
	fmt.Fprintf(&b, "You are running via `claude -p` — there is NO HUMAN to ask clarifying questions.\n\n")

	// Continuity context goes BEFORE the spec so the worker reads it
	// first and plans the rest of the session against the prior state.
	if priorHandoff != nil && card.TrackHandoffs {
		fmt.Fprintf(&b, "## Previous handoff (from session %s, by %s)\n\n",
			priorHandoff.CreatedAt, priorHandoff.Author)
		fmt.Fprintf(&b, "%s\n\n", priorHandoff.Body)
		fmt.Fprintf(&b, "---\n\n")
		fmt.Fprintf(&b, "(The original card body follows. Continue from where the previous session left off.)\n\n")
	}

	fmt.Fprintf(&b, "## Identity\n")
	fmt.Fprintf(&b, "Card id: %d\n", card.ID)
	fmt.Fprintf(&b, "Pair key: %s\n", card.PairKey)
	fmt.Fprintf(&b, "Your label for this run: %s\n\n", label)

	fmt.Fprintf(&b, "## Card spec\n")
	fmt.Fprintf(&b, "Title: %s\n", card.Title)
	if card.NeedsRole != "" {
		fmt.Fprintf(&b, "Needs role: %s\n", card.NeedsRole)
	}
	if card.Priority != 0 {
		fmt.Fprintf(&b, "Priority: %d\n", card.Priority)
	}
	fmt.Fprintf(&b, "\nBody:\n%s\n\n", card.Body)

	ctxBytes, _ := json.MarshalIndent(map[string]any{
		"files":   resolved["files"],
		"urls":    resolved["urls"],
		"msg_ids": resolved["msg_ids"],
		"cards":   resolved["cards"],
	}, "", "  ")
	if string(ctxBytes) != "" && string(ctxBytes) != "{}" {
		fmt.Fprintf(&b, "## Pre-resolved context\n")
		fmt.Fprintf(&b, "All files/urls/messages/predecessor cards listed in this card's "+
			"context_refs have been pre-resolved. The bundle is below in JSON. "+
			"DO NOT re-fetch these via Read/WebFetch — they're already here.\n\n")
		fmt.Fprintf(&b, "```json\n%s\n```\n\n", string(ctxBytes))
	}

	fmt.Fprintf(&b, "## Workflow\n")
	fmt.Fprintf(&b, "1. Read the spec + context above.\n")
	fmt.Fprintf(&b, "2. Do the work the body asks for.\n")
	fmt.Fprintf(&b, "   - As you go, post short progress comments via\n")
	fmt.Fprintf(&b, "     `~/.local/bin/peer-inbox card-comment --card %d --body \"<one line>\" --as %s`\n",
		card.ID, label)
	fmt.Fprintf(&b, "     so the human watching /cards sees what you're doing in real time.\n")
	fmt.Fprintf(&b, "3. On SUCCESS:\n")
	fmt.Fprintf(&b, "   ```\n")
	fmt.Fprintf(&b, "   ~/.local/bin/peer-inbox card-update --card %d \\\n", card.ID)
	fmt.Fprintf(&b, "     --body \"DONE: <one-line summary + where any artifacts live>\" \\\n")
	fmt.Fprintf(&b, "     --as %s\n", label)
	fmt.Fprintf(&b, "   ~/.local/bin/peer-inbox card-update-status --card %d \\\n", card.ID)
	fmt.Fprintf(&b, "     --status in_review --as %s\n", label)
	fmt.Fprintf(&b, "   ```\n")
	fmt.Fprintf(&b, "   Mark in_review (NOT done) — the human reviews + closes.\n\n")
	fmt.Fprintf(&b, "4. If the spec is TOO THIN to act on confidently, DO NOT GUESS:\n")
	fmt.Fprintf(&b, "   ```\n")
	fmt.Fprintf(&b, "   ~/.local/bin/peer-inbox card-update --card %d \\\n", card.ID)
	fmt.Fprintf(&b, "     --body \"BLOCKED: needs <X>, <Y>, <Z>\" --as %s\n", label)
	fmt.Fprintf(&b, "   ~/.local/bin/peer-inbox card-update-status --card %d \\\n", card.ID)
	fmt.Fprintf(&b, "     --status todo --as %s\n", label)
	fmt.Fprintf(&b, "   ```\n")
	fmt.Fprintf(&b, "   Be explicit about what's missing — the human will edit the body and re-run.\n\n")

	fmt.Fprintf(&b, "## Constraints\n")
	fmt.Fprintf(&b, "- No clarifying questions. Either do it, or mark BLOCKED with what's missing.\n")
	fmt.Fprintf(&b, "- You have full Edit/Write/Bash. Be a good citizen — don't touch files outside "+
		"what the card asks for.\n")
	fmt.Fprintf(&b, "- Don't `git commit` or push. The human reviews artifacts before merging.\n")

	if card.Splittable {
		writeSplitAddendum(&b, card, label)
	}
	if card.TrackHandoffs {
		writeHandoffDiscipline(&b, card, label)
	}

	return b.String()
}

// HandoffTurnBudget is the soft turn budget the handoff discipline
// references in the prompt (and the cap we apply via --max-turns when
// dispatching a track_handoffs card). Lower than the v3.12.4 default
// (40) so a track-handoffs worker has explicit room to write a
// structured handoff before the runtime cuts it off.
const HandoffTurnBudget = 30

// writeHandoffDiscipline appends the handoff-discipline addendum so a
// worker on a long-lived card knows to call card_handoff near its turn
// budget instead of crashing into the runtime ceiling. Only emitted
// when card.TrackHandoffs is true.
func writeHandoffDiscipline(b *strings.Builder, card *sqlitestore.Card, label string) {
	fmt.Fprintf(b, "\n## Hand off if you can't finish\n\n")
	fmt.Fprintf(b, "You have approximately %d turns. If by turn %d you have not finished,\n",
		HandoffTurnBudget, HandoffTurnBudget-5)
	fmt.Fprintf(b, "you MUST write a structured handoff and exit cleanly:\n\n")
	fmt.Fprintf(b, "  1. Call the `card_handoff` MCP tool with `id`=%d and `body` containing:\n", card.ID)
	fmt.Fprintf(b, "       summary:             one-paragraph status\n")
	fmt.Fprintf(b, "       decisions:           list of choices made and why\n")
	fmt.Fprintf(b, "       open_questions:      list of unresolved items\n")
	fmt.Fprintf(b, "       next_steps:          ordered list for the next session\n")
	fmt.Fprintf(b, "       files_touched:       list of paths\n")
	fmt.Fprintf(b, "       context_to_preserve: any constraint or detail the next worker\n")
	fmt.Fprintf(b, "                            absolutely needs to know\n\n")
	fmt.Fprintf(b, "     The body is capped at 64 KB — compress if larger.\n\n")
	fmt.Fprintf(b, "  2. Exit cleanly. Your claim will be released. The next dispatch will\n")
	fmt.Fprintf(b, "     read your handoff and resume from where you left off.\n\n")
	fmt.Fprintf(b, "## CLI fallback\n")
	fmt.Fprintf(b, "If MCP tools aren't exposed in your session, the same operation is\n")
	fmt.Fprintf(b, "available via the CLI:\n")
	fmt.Fprintf(b, "  ~/.local/bin/peer-inbox card-handoff --card %d \\\n", card.ID)
	fmt.Fprintf(b, "    --body-file - --as %s --format json\n", label)
	fmt.Fprintf(b, "  (then write the structured body to stdin and EOF)\n\n")
	fmt.Fprintf(b, "If a previous handoff exists, it has been prepended above as\n")
	fmt.Fprintf(b, "\"Previous handoff\". Read it FIRST and continue from where the\n")
	fmt.Fprintf(b, "previous session left off — don't redo work the prior session\n")
	fmt.Fprintf(b, "already covered.\n")
}

// MaxChildrenPerSplit caps the number of children a single mid-session
// split may emit. Beyond this, the worker is required to write a handoff
// describing the proposed restructuring so a human can approve. See plan
// §3 Q-E and §11 Q11.E.
const MaxChildrenPerSplit = 5

// writeSplitAddendum appends the mid-session split discipline to the
// regular worker prompt. Only emitted when card.Splittable is true.
func writeSplitAddendum(b *strings.Builder, card *sqlitestore.Card, label string) {
	fmt.Fprintf(b, "\n## You may split this card mid-session\n\n")
	fmt.Fprintf(b, "If during work you discover this card's scope is wrong (e.g. it actually\n")
	fmt.Fprintf(b, "requires multiple sub-pieces, or has a hidden dependency), you may split:\n\n")
	fmt.Fprintf(b, "  1. Create child cards via the `card_create` MCP tool (max %d children).\n", MaxChildrenPerSplit)
	fmt.Fprintf(b, "  2. Wire dependencies via `card_add_dependency` so this card waits on them.\n")
	fmt.Fprintf(b, "  3. Call `card_comment` with a clear rationale: what you learned that\n")
	fmt.Fprintf(b, "     made splitting necessary.\n")
	fmt.Fprintf(b, "  4. Call `card_update_status --status todo` to release your claim:\n")
	fmt.Fprintf(b, "     ```\n")
	fmt.Fprintf(b, "     ~/.local/bin/peer-inbox card-update-status --card %d \\\n", card.ID)
	fmt.Fprintf(b, "       --status todo --as %s\n", label)
	fmt.Fprintf(b, "     ```\n")
	fmt.Fprintf(b, "  5. Exit cleanly. The drainer will pick up the children and re-dispatch\n")
	fmt.Fprintf(b, "     this card once they finish.\n\n")
	fmt.Fprintf(b, "You MUST declare what changed about your understanding (the comment in\n")
	fmt.Fprintf(b, "step 3). Splitting to avoid hard work is not allowed — the audit trail\n")
	fmt.Fprintf(b, "is visible to operators.\n\n")
	fmt.Fprintf(b, "If the proposed restructuring requires more than %d children, do NOT\n", MaxChildrenPerSplit)
	fmt.Fprintf(b, "split — instead, mark BLOCKED with a clear description so a human can\n")
	fmt.Fprintf(b, "approve the larger reorganization.\n")
}

// buildDecomposerPrompt returns the prompt for an epic card's
// decomposer dispatch. The decomposer reads the epic body and emits
// N child cards via card_create + card_add_dependency. When the worker
// exits with at least one child created, monitorWorker promotes the
// epic to done. (Plan §5.1)
func buildDecomposerPrompt(card *sqlitestore.Card, label string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are a job decomposer for the agent-collaboration kanban.\n")
	fmt.Fprintf(&b, "You are running via `claude -p` — there is NO HUMAN to ask clarifying questions.\n\n")

	fmt.Fprintf(&b, "## Identity\n")
	fmt.Fprintf(&b, "Epic card id: %d\n", card.ID)
	fmt.Fprintf(&b, "Pair key:     %s\n", card.PairKey)
	fmt.Fprintf(&b, "Your label:   %s\n\n", label)

	fmt.Fprintf(&b, "## Job to decompose\n")
	fmt.Fprintf(&b, "Title: %s\n", card.Title)
	if card.NeedsRole != "" {
		fmt.Fprintf(&b, "Hint role: %s\n", card.NeedsRole)
	}
	fmt.Fprintf(&b, "\nBody:\n%s\n\n", card.Body)

	fmt.Fprintf(&b, "## Your task\n")
	fmt.Fprintf(&b, "Read the job above and produce N child cards (1 ≤ N ≤ 12). Each child is\n")
	fmt.Fprintf(&b, "30-90 minutes of focused work for one agent, with one clear acceptance\n")
	fmt.Fprintf(&b, "criterion. If two cards share more than 50%% of their context, merge.\n")
	fmt.Fprintf(&b, "Wire dependencies — don't fan everything out flat unless the children\n")
	fmt.Fprintf(&b, "truly are independent.\n\n")

	fmt.Fprintf(&b, "For each child, you MUST call the `card_create` MCP tool with:\n")
	fmt.Fprintf(&b, "  pair_key:    %q\n", card.PairKey)
	fmt.Fprintf(&b, "  title:       short imperative (e.g. \"Add JWT verification middleware\")\n")
	fmt.Fprintf(&b, "  body:        markdown spec with acceptance criterion\n")
	fmt.Fprintf(&b, "  needs_role:  one of {impl, review, qa, design, docs} — pick the closest\n")
	fmt.Fprintf(&b, "  priority:    -1 | 0 | 1\n")
	fmt.Fprintf(&b, "  splittable:  true   (decomposer's children default splittable=true so\n")
	fmt.Fprintf(&b, "                       a worker that finds hidden scope can split again)\n\n")

	fmt.Fprintf(&b, "After EACH child is created, you MUST also call `card_add_dependency`\n")
	fmt.Fprintf(&b, "with blocker=%d (this epic's id) and blockee={child id}. This wires the\n", card.ID)
	fmt.Fprintf(&b, "child to wait until this epic transitions to done — the drainer uses\n")
	fmt.Fprintf(&b, "the resulting blockee count to detect that decomposition succeeded.\n\n")

	fmt.Fprintf(&b, "Then, for each inter-child blocker → blockee dependency you identified,\n")
	fmt.Fprintf(&b, "call `card_add_dependency` with the child ids.\n\n")

	fmt.Fprintf(&b, "When done, exit. The drainer will mark this epic done automatically once\n")
	fmt.Fprintf(&b, "you exit with at least one child created. If you cannot decompose the\n")
	fmt.Fprintf(&b, "job (too vague, too small, contradictory), exit without creating children\n")
	fmt.Fprintf(&b, "and the epic will be flagged for human review.\n\n")

	fmt.Fprintf(&b, "## CLI fallback\n")
	fmt.Fprintf(&b, "If the MCP tools are not exposed in your session, the same operations\n")
	fmt.Fprintf(&b, "are available via the CLI:\n")
	fmt.Fprintf(&b, "  ~/.local/bin/peer-inbox card-create --pair-key %s --title T \\\n", card.PairKey)
	fmt.Fprintf(&b, "    --body B --needs-role R --priority P --splittable on \\\n")
	fmt.Fprintf(&b, "    --as %s --format json\n", label)
	fmt.Fprintf(&b, "  ~/.local/bin/peer-inbox card-add-dep --blocker %d --blockee CHILD_ID \\\n", card.ID)
	fmt.Fprintf(&b, "    --as %s\n\n", label)

	fmt.Fprintf(&b, "## Constraints\n")
	fmt.Fprintf(&b, "- Don't do the actual work yourself — your job is decomposition only.\n")
	fmt.Fprintf(&b, "- Don't `git commit` or push.\n")
	fmt.Fprintf(&b, "- No clarifying questions; either decompose, or exit empty.\n")

	return b.String()
}

// retryOnBusy retries fn up to 4 times with exponential backoff
// (50ms → 200ms → 800ms → 1500ms) on SQLITE_BUSY-shaped errors.
func retryOnBusy(fn func() error) error {
	delays := []time.Duration{
		50 * time.Millisecond,
		200 * time.Millisecond,
		800 * time.Millisecond,
		1500 * time.Millisecond,
	}
	var lastErr error
	for i := 0; i <= len(delays); i++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		msg := lastErr.Error()
		if !strings.Contains(msg, "database is locked") && !strings.Contains(msg, "BUSY") {
			return lastErr
		}
		if i < len(delays) {
			time.Sleep(delays[i])
		}
	}
	return lastErr
}

func randHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
