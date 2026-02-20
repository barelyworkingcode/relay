package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"relaygo/bridge"
)

func runMcpCommand(args []string) {
	if len(args) == 0 {
		mcpUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "register":
		mcpRegister(args[1:])
	case "unregister":
		mcpUnregister(args[1:])
	case "list":
		mcpList()
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp command: %s\n", args[0])
		mcpUsage()
		os.Exit(1)
	}
}

func mcpUsage() {
	fmt.Fprintf(os.Stderr, "Usage: relay mcp <command>\n\nCommands:\n  register     Register or update an external MCP server\n  unregister   Remove an external MCP server\n  list         List registered MCP servers\n")
}

func mcpRegister(args []string) {
	var (
		name     string
		command  string
		id       string
		mcpArgs  []string
		envPairs []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "--command":
			i++
			if i < len(args) {
				command = args[i]
			}
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--args":
			i++
			if i < len(args) {
				mcpArgs = append(mcpArgs, args[i])
			}
		case "--env":
			i++
			if i < len(args) {
				envPairs = append(envPairs, args[i])
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if name == "" {
		fmt.Fprintf(os.Stderr, "error: --name is required\n")
		os.Exit(1)
	}
	if command == "" {
		fmt.Fprintf(os.Stderr, "error: --command is required\n")
		os.Exit(1)
	}

	if id == "" {
		id = slugify(name)
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "error: could not derive ID from name %q\n", name)
		os.Exit(1)
	}

	var env map[string]string
	if len(envPairs) > 0 {
		env = make(map[string]string, len(envPairs))
		for _, pair := range envPairs {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				fmt.Fprintf(os.Stderr, "error: invalid --env format %q (expected KEY=VALUE)\n", pair)
				os.Exit(1)
			}
			env[k] = v
		}
	}

	cfg := ExternalMcp{
		ID:              id,
		DisplayName:     name,
		Command:         command,
		Args:            mcpArgs,
		Env:             env,
		DiscoveredTools: []ToolInfo{},
	}

	s := LoadSettings()

	// Check for existing MCP with same ID (idempotent update).
	for _, m := range s.ExternalMcps {
		if m.ID == id {
			s.UpdateExternalMcp(cfg)
			fmt.Printf("updated mcp %q (%s)\n", name, id)
			_ = bridge.SendReconcile()
			return
		}
	}

	s.AddExternalMcp(cfg)
	fmt.Printf("registered mcp %q (%s)\n", name, id)
	_ = bridge.SendReconcile()
}

func mcpUnregister(args []string) {
	var id, name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if id == "" && name == "" {
		fmt.Fprintf(os.Stderr, "error: --id or --name is required\n")
		os.Exit(1)
	}

	s := LoadSettings()

	// Resolve name to ID if needed.
	if id == "" {
		for _, m := range s.ExternalMcps {
			if m.DisplayName == name {
				id = m.ID
				break
			}
		}
		if id == "" {
			fmt.Fprintf(os.Stderr, "error: no mcp found with name %q\n", name)
			os.Exit(1)
		}
	}

	// Verify MCP exists.
	found := false
	for _, m := range s.ExternalMcps {
		if m.ID == id {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "error: no mcp found with id %q\n", id)
		os.Exit(1)
	}

	s.RemoveExternalMcp(id)
	fmt.Printf("unregistered mcp %q\n", id)
	_ = bridge.SendReconcile()
}

func mcpList() {
	s := LoadSettings()

	if len(s.ExternalMcps) == 0 {
		fmt.Println("no mcp servers registered")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCOMMAND\tTOOLS")
	for _, m := range s.ExternalMcps {
		cmd := m.Command
		if len(m.Args) > 0 {
			cmd += " " + strings.Join(m.Args, " ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", m.ID, m.DisplayName, cmd, len(m.DiscoveredTools))
	}
	w.Flush()
}
