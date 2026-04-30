package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.4.6 H1 — card-handoff CLI verb.
//
//   peer-inbox card-handoff --card N
//                           [--body STRUCTURED_BODY | --body-file PATH]
//                           [--as AUTHOR] [--format plain|json]
//
// Records a kind=handoff event with the given body. Body shape is
// caller-defined — typically markdown / JSON containing the structured
// fields the handoff prompt asks for (summary, decisions, open
// questions, next steps, files touched, context to preserve). The
// store enforces the 64 KB cap (Q11.D).
//
// One of --body or --body-file is required (not both).
func runCardHandoff(args []string) int {
	fs := flag.NewFlagSet("card-handoff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cardID := fs.Int64("card", 0, "card id (required)")
	body := fs.String("body", "", "handoff body (or use --body-file)")
	bodyFile := fs.String("body-file", "", "read body from this path (use '-' for stdin)")
	author := fs.String("as", "", "author label for the audit row (defaults to 'agent')")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID == 0 {
		fmt.Fprintln(os.Stderr, "card-handoff: --card required")
		return exitUsage
	}
	if (*body == "" && *bodyFile == "") || (*body != "" && *bodyFile != "") {
		fmt.Fprintln(os.Stderr, "card-handoff: pass exactly one of --body or --body-file")
		return exitUsage
	}

	finalBody := *body
	if *bodyFile != "" {
		var data []byte
		var err error
		if *bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*bodyFile)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "card-handoff: read body: %v\n", err)
			return exitInternal
		}
		finalBody = string(data)
	}
	if finalBody == "" {
		fmt.Fprintln(os.Stderr, "card-handoff: body is empty")
		return exitUsage
	}

	authorLabel := *author
	if authorLabel == "" {
		authorLabel = "agent"
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-handoff: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	ev, err := st.RecordCardHandoff(ctx, *cardID, finalBody, authorLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrHandoffTooLarge) {
			fmt.Fprintf(os.Stderr, "card-handoff: %v — compress or split before retry\n", err)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "card-handoff: %v\n", err)
		return exitInternal
	}

	if *format == "json" {
		return emitJSON(map[string]any{
			"id":         ev.ID,
			"card_id":    ev.CardID,
			"kind":       ev.Kind,
			"author":     ev.Author,
			"body_bytes": len(ev.Body),
			"created_at": ev.CreatedAt,
		})
	}
	fmt.Printf("handoff #%d on card #%d by %s (%d bytes)\n",
		ev.ID, ev.CardID, ev.Author, len(ev.Body))
	return exitOK
}
