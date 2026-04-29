package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.2 — pool composition CLI verbs.
//
//   peer-inbox pool-add    --pair-key K --agent L
//                          [--count N] [--priority P] [--as A]
//                          [--format plain|json]
//   peer-inbox pool-remove --pair-key K --agent L
//                          [--format plain|json]
//   peer-inbox pool-list   --pair-key K [--format plain|json]
//   peer-inbox pool-update --pair-key K --agent L
//                          [--count N] [--priority P]
//                          [--format plain|json]
//
// Each verb is a thin wrapper over the pool store layer. Agent
// references are by label (operator-friendly); the verb resolves to
// agent_id via GetAgentByLabel before hitting pool_members.

func runPoolAdd(args []string) int {
	fs := flag.NewFlagSet("pool-add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "board pair_key (required)")
	agentLabel := fs.String("agent", "", "agent label (required)")
	count := fs.Int("count", 1, "parallel slots for this agent (>=1)")
	priority := fs.Int("priority", 0, "auto-select tiebreak (higher first)")
	addedBy := fs.String("as", "", "audit label (defaults to 'owner')")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" || *agentLabel == "" {
		fmt.Fprintln(os.Stderr, "pool-add: --pair-key and --agent required")
		return exitUsage
	}
	if *count < 1 {
		fmt.Fprintln(os.Stderr, "pool-add: --count must be >= 1")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-add: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, *agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintf(os.Stderr, "pool-add: agent %q not found\n", *agentLabel)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "pool-add: %v\n", err)
		return exitInternal
	}
	m, err := st.AddPoolMember(ctx, sqlitestore.AddPoolMemberParams{
		PairKey: *pairKey, AgentID: a.ID,
		Count: *count, Priority: *priority, AddedBy: *addedBy,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-add: %v\n", err)
		return exitUsage
	}
	return emitPoolMember(m, *format)
}

func runPoolRemove(args []string) int {
	fs := flag.NewFlagSet("pool-remove", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "board pair_key (required)")
	agentLabel := fs.String("agent", "", "agent label (required)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" || *agentLabel == "" {
		fmt.Fprintln(os.Stderr, "pool-remove: --pair-key and --agent required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-remove: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, *agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintf(os.Stderr, "pool-remove: agent %q not found\n", *agentLabel)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "pool-remove: %v\n", err)
		return exitInternal
	}
	if err := st.RemovePoolMember(ctx, *pairKey, a.ID); err != nil {
		if errors.Is(err, sqlitestore.ErrPoolMemberNotFound) {
			fmt.Fprintf(os.Stderr, "pool-remove: not in pool\n")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "pool-remove: %v\n", err)
		return exitInternal
	}
	if *format == "json" {
		return emitJSON(map[string]any{
			"removed": true, "pair_key": *pairKey, "agent": *agentLabel,
		})
	}
	fmt.Printf("removed agent=%s from pool=%s\n", *agentLabel, *pairKey)
	return exitOK
}

func runPoolList(args []string) int {
	fs := flag.NewFlagSet("pool-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "board pair_key (required)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" {
		fmt.Fprintln(os.Stderr, "pool-list: --pair-key required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-list: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	members, err := st.ListPoolMembers(ctx, *pairKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-list: %v\n", err)
		return exitInternal
	}
	return emitPoolMembers(members, *format)
}

func runPoolUpdate(args []string) int {
	fs := flag.NewFlagSet("pool-update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "board pair_key (required)")
	agentLabel := fs.String("agent", "", "agent label (required)")
	count := fs.Int("count", 0, "new parallel slot count (>=1)")
	priority := fs.Int("priority", 0, "new priority")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" || *agentLabel == "" {
		fmt.Fprintln(os.Stderr, "pool-update: --pair-key and --agent required")
		return exitUsage
	}

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	var p sqlitestore.UpdatePoolMemberParams
	if visited["count"] {
		if *count < 1 {
			fmt.Fprintln(os.Stderr, "pool-update: --count must be >= 1")
			return exitUsage
		}
		p.Count = count
	}
	if visited["priority"] {
		p.Priority = priority
	}
	if p.Count == nil && p.Priority == nil {
		fmt.Fprintln(os.Stderr, "pool-update: pass --count and/or --priority")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-update: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	a, err := st.GetAgentByLabel(ctx, *agentLabel)
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintf(os.Stderr, "pool-update: agent %q not found\n", *agentLabel)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "pool-update: %v\n", err)
		return exitInternal
	}
	if err := st.UpdatePoolMember(ctx, *pairKey, a.ID, p); err != nil {
		if errors.Is(err, sqlitestore.ErrPoolMemberNotFound) {
			fmt.Fprintln(os.Stderr, "pool-update: not in pool")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "pool-update: %v\n", err)
		return exitInternal
	}
	saved, err := st.GetPoolMember(ctx, *pairKey, a.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-update: %v\n", err)
		return exitInternal
	}
	return emitPoolMember(saved, *format)
}

// --- emitters ---------------------------------------------------------

func emitPoolMember(m *sqlitestore.PoolMember, format string) int {
	if format == "json" {
		return emitJSON(poolMemberAsMap(m))
	}
	fmt.Println(renderPoolMemberPlain(m))
	return exitOK
}

func emitPoolMembers(members []sqlitestore.PoolMember, format string) int {
	if format == "json" {
		arr := make([]map[string]any, 0, len(members))
		for i := range members {
			arr = append(arr, poolMemberAsMap(&members[i]))
		}
		return emitJSON(arr)
	}
	if len(members) == 0 {
		fmt.Println("(empty pool)")
		return exitOK
	}
	for i := range members {
		fmt.Println(renderPoolMemberPlain(&members[i]))
	}
	return exitOK
}

func poolMemberAsMap(m *sqlitestore.PoolMember) map[string]any {
	out := map[string]any{
		"pair_key":      m.PairKey,
		"agent_id":      m.AgentID,
		"agent_label":   m.AgentLabel,
		"agent_runtime": m.AgentRuntime,
		"agent_enabled": m.AgentEnabled,
		"count":         m.Count,
		"priority":      m.Priority,
		"added_at":      m.AddedAt,
	}
	if m.AgentRole != "" {
		out["agent_role"] = m.AgentRole
	}
	if m.AddedBy != "" {
		out["added_by"] = m.AddedBy
	}
	return out
}

func renderPoolMemberPlain(m *sqlitestore.PoolMember) string {
	state := "enabled"
	if !m.AgentEnabled {
		state = "disabled"
	}
	line := m.PairKey + " · " + m.AgentLabel +
		" (" + m.AgentRuntime + ", " + state + ")"
	if m.AgentRole != "" {
		line += " role=" + m.AgentRole
	}
	line += " priority=" + strconv.Itoa(m.Priority) +
		" count=" + strconv.Itoa(m.Count)
	return line
}
