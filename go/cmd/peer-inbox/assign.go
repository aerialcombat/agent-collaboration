package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.4 D3 — designated assignment CLI.
//
//   peer-inbox card-assign   --card N --agent L [--as AUTHOR] [--format plain|json]
//   peer-inbox card-unassign --card N           [--as AUTHOR] [--format plain|json]
//
// Pins a specific agent to a card; PickAgentForCard honors this over
// pool auto-select. Unassign clears the designation.

func runCardAssign(args []string) int {
	fs := flag.NewFlagSet("card-assign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cardID := fs.Int64("card", 0, "card id (required)")
	agentLabel := fs.String("agent", "", "agent label (required)")
	author := fs.String("as", "", "audit label (defaults to 'system')")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID == 0 || *agentLabel == "" {
		fmt.Fprintln(os.Stderr, "card-assign: --card and --agent required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-assign: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, *agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintf(os.Stderr, "card-assign: agent %q not found\n", *agentLabel)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "card-assign: %v\n", err)
		return exitInternal
	}
	saved, err := st.AssignCardToAgent(ctx, *cardID, a.ID, *author)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			fmt.Fprintln(os.Stderr, "card-assign: card not found")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "card-assign: %v\n", err)
		return exitInternal
	}
	if *format == "json" {
		return emitJSON(map[string]any{
			"card_id":              saved.ID,
			"assigned_to_agent_id": saved.AssignedToAgentID,
			"agent_label":          a.Label,
		})
	}
	fmt.Printf("card #%d assigned to agent %s\n", saved.ID, a.Label)
	return exitOK
}

func runCardUnassign(args []string) int {
	fs := flag.NewFlagSet("card-unassign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cardID := fs.Int64("card", 0, "card id (required)")
	author := fs.String("as", "", "audit label (defaults to 'system')")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID == 0 {
		fmt.Fprintln(os.Stderr, "card-unassign: --card required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-unassign: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	saved, err := st.AssignCardToAgent(ctx, *cardID, 0, *author)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			fmt.Fprintln(os.Stderr, "card-unassign: card not found")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "card-unassign: %v\n", err)
		return exitInternal
	}
	if *format == "json" {
		return emitJSON(map[string]any{
			"card_id":              saved.ID,
			"assigned_to_agent_id": nil,
		})
	}
	fmt.Printf("card #%d unassigned\n", saved.ID)
	return exitOK
}
