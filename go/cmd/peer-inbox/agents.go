package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// v3.12.1 — agent registry CLI verbs.
//
//   peer-inbox agent-create  --label L --runtime {claude|pi}
//                            [--role R] [--worker-cmd "..."] [--config '{...}']
//                            [--enabled on|off] [--format plain|json]
//   peer-inbox agent-list    [--enabled-only] [--format plain|json]
//   peer-inbox agent-get     --label L [--format plain|json]
//                            (or --id N)
//   peer-inbox agent-update  (--label L | --id N)
//                            [--runtime {claude|pi}] [--role R]
//                            [--worker-cmd "..."] [--config '{...}']
//                            [--enabled on|off] [--format plain|json]
//   peer-inbox agent-delete  (--label L | --id N)
//
// CLI mirrors the agents store layer. Used by the smoke harness, the
// MCP wrappers, and any operator flow not going through the web UI.

func runAgentCreate(args []string) int {
	fs := flag.NewFlagSet("agent-create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	label := fs.String("label", "", "agent label (required, unique)")
	runtime := fs.String("runtime", "", "runtime: claude|pi (required)")
	role := fs.String("role", "", "optional role (matches cards.needs_role)")
	workerCmd := fs.String("worker-cmd", "", "optional argv override")
	config := fs.String("config", "", "optional model_config JSON blob")
	enabled := fs.String("enabled", "on", "on|off (default on)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *label == "" || *runtime == "" {
		fmt.Fprintln(os.Stderr, "agent-create: --label and --runtime required")
		return exitUsage
	}
	enabledBool, err := parseOnOff(*enabled, "--enabled")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-create: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	a, err := st.CreateAgent(ctx, sqlitestore.CreateAgentParams{
		Label:       *label,
		Runtime:     *runtime,
		WorkerCmd:   *workerCmd,
		Role:        *role,
		ModelConfig: *config,
		Enabled:     &enabledBool,
	})
	if err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrAgentLabelTaken):
			fmt.Fprintf(os.Stderr, "agent-create: label %q already exists\n", *label)
			return exitUsage
		default:
			fmt.Fprintf(os.Stderr, "agent-create: %v\n", err)
			return exitInternal
		}
	}
	return emitAgent(a, *format)
}

func runAgentList(args []string) int {
	fs := flag.NewFlagSet("agent-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	enabledOnly := fs.Bool("enabled-only", false, "list only enabled agents")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-list: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	agents, err := st.ListAgents(ctx, *enabledOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-list: %v\n", err)
		return exitInternal
	}
	return emitAgents(agents, *format)
}

func runAgentGet(args []string) int {
	fs := flag.NewFlagSet("agent-get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	label := fs.String("label", "", "agent label")
	id := fs.Int64("id", 0, "agent id")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *label == "" && *id == 0 {
		fmt.Fprintln(os.Stderr, "agent-get: --label or --id required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-get: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	var a *sqlitestore.Agent
	if *id != 0 {
		a, err = st.GetAgent(ctx, *id)
	} else {
		a, err = st.GetAgentByLabel(ctx, *label)
	}
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintln(os.Stderr, "agent-get: not found")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "agent-get: %v\n", err)
		return exitInternal
	}
	return emitAgent(a, *format)
}

func runAgentUpdate(args []string) int {
	fs := flag.NewFlagSet("agent-update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	label := fs.String("label", "", "agent label")
	id := fs.Int64("id", 0, "agent id")
	runtime := fs.String("runtime", "", "claude|pi")
	role := fs.String("role", "", "role (empty string clears it)")
	workerCmd := fs.String("worker-cmd", "", "argv override (empty clears)")
	config := fs.String("config", "", "model_config JSON (empty clears)")
	enabled := fs.String("enabled", "", "on|off")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *label == "" && *id == 0 {
		fmt.Fprintln(os.Stderr, "agent-update: --label or --id required")
		return exitUsage
	}

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	var p sqlitestore.UpdateAgentParams
	if visited["runtime"] {
		p.Runtime = runtime
	}
	if visited["role"] {
		p.Role = role
	}
	if visited["worker-cmd"] {
		p.WorkerCmd = workerCmd
	}
	if visited["config"] {
		p.ModelConfig = config
	}
	if visited["enabled"] {
		v, err := parseOnOff(*enabled, "--enabled")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		p.Enabled = &v
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-update: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	target := *id
	if target == 0 {
		a, err := st.GetAgentByLabel(ctx, *label)
		if err != nil {
			if errors.Is(err, sqlitestore.ErrAgentNotFound) {
				fmt.Fprintln(os.Stderr, "agent-update: not found")
				return exitUsage
			}
			fmt.Fprintf(os.Stderr, "agent-update: %v\n", err)
			return exitInternal
		}
		target = a.ID
	}

	if err := st.UpdateAgent(ctx, target, p); err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintln(os.Stderr, "agent-update: not found")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "agent-update: %v\n", err)
		return exitInternal
	}
	saved, err := st.GetAgent(ctx, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-update: %v\n", err)
		return exitInternal
	}
	return emitAgent(saved, *format)
}

func runAgentDelete(args []string) int {
	fs := flag.NewFlagSet("agent-delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	label := fs.String("label", "", "agent label")
	id := fs.Int64("id", 0, "agent id")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *label == "" && *id == 0 {
		fmt.Fprintln(os.Stderr, "agent-delete: --label or --id required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-delete: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	target := *id
	resolvedLabel := *label
	if target == 0 {
		a, err := st.GetAgentByLabel(ctx, *label)
		if err != nil {
			if errors.Is(err, sqlitestore.ErrAgentNotFound) {
				fmt.Fprintln(os.Stderr, "agent-delete: not found")
				return exitUsage
			}
			fmt.Fprintf(os.Stderr, "agent-delete: %v\n", err)
			return exitInternal
		}
		target = a.ID
		resolvedLabel = a.Label
	}
	if err := st.DeleteAgent(ctx, target); err != nil {
		if errors.Is(err, sqlitestore.ErrAgentNotFound) {
			fmt.Fprintln(os.Stderr, "agent-delete: not found")
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "agent-delete: %v\n", err)
		return exitInternal
	}
	if *format == "json" {
		return emitJSON(map[string]any{"deleted": true, "id": target, "label": resolvedLabel})
	}
	fmt.Printf("deleted agent id=%d label=%s\n", target, resolvedLabel)
	return exitOK
}

// --- emitters ----------------------------------------------------------

func emitAgent(a *sqlitestore.Agent, format string) int {
	if format == "json" {
		return emitJSON(agentAsMap(a))
	}
	fmt.Println(renderAgentPlain(a))
	return exitOK
}

func emitAgents(agents []sqlitestore.Agent, format string) int {
	if format == "json" {
		arr := make([]map[string]any, 0, len(agents))
		for i := range agents {
			arr = append(arr, agentAsMap(&agents[i]))
		}
		return emitJSON(arr)
	}
	if len(agents) == 0 {
		fmt.Println("(no agents)")
		return exitOK
	}
	for i := range agents {
		fmt.Println(renderAgentPlain(&agents[i]))
	}
	return exitOK
}

func agentAsMap(a *sqlitestore.Agent) map[string]any {
	m := map[string]any{
		"id":         a.ID,
		"label":      a.Label,
		"runtime":    a.Runtime,
		"enabled":    a.Enabled,
		"created_at": a.CreatedAt,
		"updated_at": a.UpdatedAt,
	}
	if a.WorkerCmd != "" {
		m["worker_cmd"] = a.WorkerCmd
	}
	if a.Role != "" {
		m["role"] = a.Role
	}
	if a.ModelConfig != "" {
		m["model_config"] = a.ModelConfig
	}
	return m
}

func renderAgentPlain(a *sqlitestore.Agent) string {
	state := "enabled"
	if !a.Enabled {
		state = "disabled"
	}
	line := "agent#" + strconv.FormatInt(a.ID, 10) +
		"  " + a.Label +
		"  runtime=" + a.Runtime +
		"  " + state
	if a.Role != "" {
		line += "  role=" + a.Role
	}
	if a.WorkerCmd != "" {
		line += "  worker_cmd=" + a.WorkerCmd
	}
	return line
}
