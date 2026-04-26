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
	if !card.Ready {
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
	label := fmt.Sprintf("runner-%d-%s", card.ID, suffix)

	// Insert the run row first so the reaper has something to find if we
	// crash mid-dispatch. log_path is filled later (we don't know it
	// yet — depends on filesystem writability).
	run, err := st.CreateCardRun(ctx, sqlitestore.CreateCardRunParams{
		CardID:      card.ID,
		PairKey:     card.PairKey,
		WorkerLabel: label,
		Trigger:     trigger,
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
	prompt := buildWorkerPrompt(card, label, resolved)

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

	cmd, err := spawnWorker(prompt, logFile)
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
	dispatchMeta, _ := json.Marshal(map[string]any{
		"run_id":       run.ID,
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
		"trigger":      trigger,
	})
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

	return map[string]any{
		"card_id":      card.ID,
		"run_id":       run.ID,
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
	}, nil
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
}

// spawnWorker launches `claude -p` non-blocking, redirecting stdout +
// stderr to logFile. Returns the *exec.Cmd so the caller can attach a
// monitor goroutine. Children inherit our process group, so they receive
// SIGTERM when peer-web shuts down (the "die with parent" rule from the
// Phase 1 plan §12).
func spawnWorker(prompt string, logFile *os.File) (*exec.Cmd, error) {
	cmd := exec.Command("claude",
		"-p",
		"--permission-mode=bypassPermissions",
		"--max-turns=40",
	)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// buildWorkerPrompt composes the prompt headless `claude -p` receives
// via stdin. The structure matches the manual subagent dispatch — same
// workflow rules, same output contract.
func buildWorkerPrompt(card *sqlitestore.Card, label string, resolved map[string]any) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are a headless kanban worker spawned to drain a single card.\n")
	fmt.Fprintf(&b, "You are running via `claude -p` — there is NO HUMAN to ask clarifying questions.\n\n")

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
