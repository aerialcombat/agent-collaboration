package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.11 Phase 1 — board + run CLI verbs.
//
//   peer-inbox board-set --pair-key K [--auto-drain on|off] [--max-concurrent N]
//                        [--auto-promote on|off] [--poll-interval-secs N]
//                        [--as LABEL] [--format plain|json]
//
//   peer-inbox card-runs --card N [--limit N] [--format plain|json]
//
// CLI mirrors the /api/boards/{pair_key}/settings POST body and the
// /api/cards/{id}/runs response. Used by tests and any operator flows
// that don't go through the web UI.

// runBoardSet — board-set --pair-key K [knobs...]
//
// Reads the current row, applies any of the four mutable fields the
// caller passes, and upserts. Absent flags leave the field unchanged.
// auto-drain / auto-promote accept the strings "on", "true", "1" /
// "off", "false", "0" (anything else is an error).
func runBoardSet(args []string) int {
	fs := flag.NewFlagSet("board-set", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "pair_key (required)")
	autoDrain := fs.String("auto-drain", "", "on|off — flips drainer membership")
	maxConcurrent := fs.Int("max-concurrent", 0, "max concurrent runs per board (>=1)")
	autoPromote := fs.String("auto-promote", "", "on|off — auto-promote in_review→done when downstream blockees")
	pollSecs := fs.Int("poll-interval-secs", 0, "drainer tick interval (>=1)")
	as := fs.String("as", "", "author label written to board_settings.updated_by")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" {
		fmt.Fprintln(os.Stderr, "board-set: --pair-key required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-set: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	cur, err := st.GetBoardSettings(ctx, *pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-set: %v\n", err)
		return exitInternal
	}
	cur.PairKey = *pairKey

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if visited["auto-drain"] {
		v, err := parseOnOff(*autoDrain, "--auto-drain")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		cur.AutoDrain = v
	}
	if visited["max-concurrent"] {
		if *maxConcurrent < 1 {
			fmt.Fprintln(os.Stderr, "board-set: --max-concurrent must be >= 1")
			return exitUsage
		}
		cur.MaxConcurrent = *maxConcurrent
	}
	if visited["auto-promote"] {
		v, err := parseOnOff(*autoPromote, "--auto-promote")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		cur.AutoPromote = v
	}
	if visited["poll-interval-secs"] {
		if *pollSecs < 1 {
			fmt.Fprintln(os.Stderr, "board-set: --poll-interval-secs must be >= 1")
			return exitUsage
		}
		cur.PollIntervalSecs = *pollSecs
	}
	if *as != "" {
		cur.UpdatedBy = *as
	}

	if err := st.UpsertBoardSettings(ctx, cur); err != nil {
		fmt.Fprintf(os.Stderr, "board-set: %v\n", err)
		return exitInternal
	}
	saved, err := st.GetBoardSettings(ctx, *pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "board-set: %v\n", err)
		return exitInternal
	}
	return emitBoardSettings(saved, *format)
}

// runCardRuns — card-runs --card N [--limit N]
func runCardRuns(args []string) int {
	fs := flag.NewFlagSet("card-runs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	limit := fs.Int("limit", 0, "max rows to return (0 = no cap)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 {
		fmt.Fprintln(os.Stderr, "card-runs: --card required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-runs: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()
	runs, err := st.ListCardRunsByCard(ctx, *id, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-runs: %v\n", err)
		return exitInternal
	}
	return emitCardRuns(runs, *format)
}

func emitBoardSettings(b sqlitestore.BoardSettings, format string) int {
	if format == "json" {
		return emitJSON(boardSettingsAsMap(b))
	}
	fmt.Printf("%s  auto_drain=%v  max=%d  auto_promote=%v  poll=%ds  updated_by=%s\n",
		b.PairKey, b.AutoDrain, b.MaxConcurrent, b.AutoPromote,
		b.PollIntervalSecs, b.UpdatedBy)
	return exitOK
}

func emitCardRuns(runs []*sqlitestore.CardRun, format string) int {
	if format == "json" {
		arr := make([]map[string]any, 0, len(runs))
		for _, r := range runs {
			arr = append(arr, cardRunAsMap(r))
		}
		return emitJSON(arr)
	}
	if len(runs) == 0 {
		fmt.Println("(no runs)")
		return exitOK
	}
	for _, r := range runs {
		fmt.Println(renderCardRunPlain(r))
	}
	return exitOK
}

func boardSettingsAsMap(b sqlitestore.BoardSettings) map[string]any {
	return map[string]any{
		"pair_key":           b.PairKey,
		"auto_drain":         b.AutoDrain,
		"max_concurrent":     b.MaxConcurrent,
		"auto_promote":       b.AutoPromote,
		"poll_interval_secs": b.PollIntervalSecs,
		"updated_at":         b.UpdatedAt,
		"updated_by":         b.UpdatedBy,
	}
}

func cardRunAsMap(r *sqlitestore.CardRun) map[string]any {
	m := map[string]any{
		"id":           r.ID,
		"card_id":      r.CardID,
		"pair_key":     r.PairKey,
		"host":         r.Host,
		"worker_label": r.WorkerLabel,
		"started_at":   r.StartedAt,
		"status":       r.Status,
		"trigger":      r.Trigger,
	}
	if r.PID != 0 {
		m["pid"] = r.PID
	}
	if r.EndedAt != "" {
		m["ended_at"] = r.EndedAt
		m["exit_code"] = r.ExitCode
	}
	if r.LogPath != "" {
		m["log_path"] = r.LogPath
	}
	return m
}

func renderCardRunPlain(r *sqlitestore.CardRun) string {
	line := "run#" + strconv.FormatInt(r.ID, 10) +
		"  card=" + strconv.FormatInt(r.CardID, 10) +
		"  [" + r.Status + "]"
	if r.PID != 0 {
		line += "  pid=" + strconv.Itoa(r.PID)
	}
	line += "  worker=" + r.WorkerLabel
	line += "  trigger=" + r.Trigger
	line += "  started=" + r.StartedAt
	if r.EndedAt != "" {
		line += "  ended=" + r.EndedAt + "  rc=" + strconv.Itoa(r.ExitCode)
	}
	return line
}

func parseOnOff(s, flagName string) (bool, error) {
	switch s {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("%s: expected on|off|true|false|1|0, got %q", flagName, s)
}

// emitJSON is shared with cards.go; redeclaration would conflict, so
// boards.go reuses the existing helper. Reference here so go vet is
// happy if cards.go is ever pulled out.
var _ = json.RawMessage("")
