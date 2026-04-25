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

// Shape B v0: per-card "Run" — server spawns a headless `claude -p`
// worker scoped to a single card. The worker reads the card body +
// pre-resolved context, does the work, then transitions the card to
// in_review (or back to todo with a BLOCKED annotation if the spec
// is too thin).
//
// This is deliberately the surgical version (Shape C's UI on Shape B's
// mechanism). A global "Start" button can layer on top later by
// looping this same handler over every ready card.

const (
	runnerLogDir   = "/tmp/peer-inbox/runners"
	runnerCmdTimeout = 30 * time.Minute // safety ceiling on a single spawn
)

// handleCardRun — POST /api/cards/{id}/run
//
// Verifies the card is in todo + ready (no open blockers), claims it
// to a generated label `runner-<id>-<short-uuid>`, flips status to
// in_progress, then forks a `claude -p` subprocess with the composed
// prompt. Non-blocking — returns once the worker has been started.
//
// Response:
//
//	{
//	  "card_id": 24, "worker_label": "runner-24-a3f2",
//	  "pid": 88421, "log_path": "/tmp/peer-inbox/runners/run-24-...log"
//	}
//
// The user can `tail -f log_path` to watch the worker think aloud.
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

	// Eligibility — spawn only on ready todos. Anything else is the
	// human's job to untangle (claim conflict, blocker still open, etc).
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

	// Resolve refs once, server-side, so the worker doesn't need to fetch.
	refs := contextRefsShape{}
	if card.ContextRefs != "" {
		if err := json.Unmarshal([]byte(card.ContextRefs), &refs); err != nil {
			writeJSONError(w, http.StatusInternalServerError,
				"context_refs is not valid JSON: "+err.Error())
			return
		}
	}
	resolved := resolveContext(ctx, st, refs)

	// Generate a runner label. Collision-resistant enough at typical scale.
	suffix, err := randHex(4)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random: "+err.Error())
		return
	}
	label := fmt.Sprintf("runner-%d-%s", id, suffix)

	// Optimistically claim → in_progress before forking. If the user
	// double-clicks Run, the second call will see ClaimedBy != "" and
	// 409 out, which is the desired behavior.
	if _, err := st.ClaimCard(ctx, id, label, false); err != nil {
		if errors.Is(err, sqlitestore.ErrCardAlreadyClaimed) {
			writeJSONError(w, http.StatusConflict, "card was claimed mid-flight")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "claim: "+err.Error())
		return
	}
	if _, err := st.UpdateCardStatus(ctx, id, sqlitestore.CardStatusInProgress, label); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "status: "+err.Error())
		return
	}

	prompt := buildWorkerPrompt(card, label, resolved)

	if err := os.MkdirAll(runnerLogDir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "log dir: "+err.Error())
		return
	}
	logPath := filepath.Join(runnerLogDir,
		fmt.Sprintf("run-%d-%s.log", id, time.Now().UTC().Format("20060102-150405")))
	logFile, err := os.Create(logPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "log file: "+err.Error())
		return
	}

	pid, err := spawnWorker(prompt, logFile)
	if err != nil {
		_ = logFile.Close()
		writeJSONError(w, http.StatusInternalServerError, "spawn: "+err.Error())
		return
	}

	// Record the dispatch as a timeline event so the drawer Activity
	// panel surfaces it next to the auto-emitted claim + status_change.
	dispatchMeta, _ := json.Marshal(map[string]any{
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
	})
	if _, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: id, Kind: sqlitestore.CardEventRunDispatch,
		Author: "system",
		Body:   fmt.Sprintf("worker %s dispatched (pid %d)", label, pid),
		Meta:   string(dispatchMeta),
	}); err != nil {
		// Non-fatal; the worker is already running.
		fmt.Fprintf(os.Stderr, "card_events: run_dispatch record failed: %v\n", err)
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"card_id":      id,
		"worker_label": label,
		"pid":          pid,
		"log_path":     logPath,
		"prompt_bytes": len(prompt),
	})
}

// spawnWorker launches `claude -p <prompt>` non-blocking, redirecting
// both stdout and stderr to logFile. Returns the child PID. The caller
// has already opened logFile; spawnWorker owns the close (after fork
// the parent doesn't need the FD held open, but we keep the logFile
// alive in the background goroutine until the child exits so the OS
// doesn't reclaim it prematurely).
func spawnWorker(prompt string, logFile *os.File) (int, error) {
	// Pipe the prompt via stdin rather than as an argument to dodge
	// max-arg-length limits and shell-quoting hazards.
	cmd := exec.Command("claude",
		"-p",
		// --permission-mode=bypassPermissions runs without per-tool prompts;
		// the worker has full Edit/Write/Bash. Sandbox via cwd if you need
		// stricter isolation. (Future flag work.)
		"--permission-mode=bypassPermissions",
		// Reasonable cap so a confused worker can't hang forever.
		"--max-turns=40",
	)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Inherit cwd from peer-web (typically the repo root).

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid

	// Wait + close in a background goroutine so we don't leak the
	// process table entry.
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()
	return pid, nil
}

// buildWorkerPrompt composes the system+user prompt the headless
// `claude -p` invocation receives via stdin. The structure matches
// what the manual subagent dispatch used for card #23 — same workflow
// rules, same output contract, just packaged for an unattended run.
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

	// Resolved context — only emit a section if there's anything to show.
	hasContext := false
	for _, k := range []string{"files", "urls", "msg_ids", "cards"} {
		if v, ok := resolved[k].([]any); ok && len(v) > 0 {
			hasContext = true
			break
		}
		// resolveContext returns []resolvedFile etc., not []any; type-switch.
	}
	// resolveContext returns concrete typed slices; reflect via JSON for
	// safety regardless of which slice flavor we got.
	ctxBytes, _ := json.MarshalIndent(map[string]any{
		"files":   resolved["files"],
		"urls":    resolved["urls"],
		"msg_ids": resolved["msg_ids"],
		"cards":   resolved["cards"],
	}, "", "  ")
	_ = hasContext
	if string(ctxBytes) != "" && string(ctxBytes) != "{}" {
		fmt.Fprintf(&b, "## Pre-resolved context\n")
		fmt.Fprintf(&b, "All files/urls/messages/predecessor cards listed in this card's "+
			"context_refs have been pre-resolved. The bundle is below in JSON. "+
			"DO NOT re-fetch these via Read/WebFetch — they're already here.\n\n")
		fmt.Fprintf(&b, "```json\n%s\n```\n\n", string(ctxBytes))
	}

	fmt.Fprintf(&b, "## Workflow\n")
	fmt.Fprintf(&b, "1. Read the spec + context above.\n")
	fmt.Fprintf(&b, "2. Do the work the body asks for. If files need to be written or edited, "+
		"do that. If it's a pure-spec card (e.g. \"add a CHANGELOG entry\"), do that.\n")
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

func randHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
