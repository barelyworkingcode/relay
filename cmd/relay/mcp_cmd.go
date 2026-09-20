package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
)

// register and unregister are brokered (ADR-017 decision 2): this process
// holds no sealer (§5.4), so it dials the running tray over admin_op and
// lets McpOps — the same core the MCP Servers tab and RegisterMcpRoutes
// share — do the discovery, the SSRF guard and the reconcile notify. `list`
// is a tray read too: the running tray is the only reader of the
// configuration.
func runMcpCommand(args []string) {
	runSubcommands("mcp", []cliSubcommand{
		{"register", mcpRegister},
		{"unregister", mcpUnregister},
		{"list", func(_ []string) { mcpList() }},
	}, args)
}

// mcpRegister covers both transports, matching McpOps.Add itself: an HTTP
// MCP that answers 401 during discovery still gets its record persisted
// (mcpView.AuthRequired says so), and completing OAuth for it is a desktop
// act with no CLI door (ADR-014 section 4 — StartOAuth needs a local
// callback listener and a real browser, and only the Settings window and
// the tray's own IPC ever reach it).
func mcpRegister(args []string) {
	fs := flag.NewFlagSet("mcp register", flag.ExitOnError)
	var opts registerOpts
	addRegisterFlags(fs, &opts)
	command := fs.String("command", "", "command to run (required for stdio transport)")
	transport := fs.String("transport", "stdio", "transport type (stdio or http)")
	mcpURL := fs.String("url", "", "MCP endpoint URL (required for http)")
	tccServices := fs.String("tcc-services", "", "comma-separated TCC services the MCP needs (e.g. calendar,contacts,reminders,microphone,appleevents)")
	fs.Parse(args)

	if opts.Name == "" {
		exitError("--name is required")
	}
	if *transport != "stdio" && *transport != "http" {
		exitError("--transport must be stdio or http")
	}

	env, err := parseEnvPairs(opts.EnvPairs)
	if err != nil {
		exitError("%v", err)
	}
	fields := mcpFields{
		ID:          opts.ID,
		DisplayName: opts.Name,
		Transport:   *transport,
		URL:         *mcpURL,
		Command:     *command,
		Args:        []string(opts.Args),
		Env:         env,
		TccServices: parseTccServices(*tccServices),
	}

	client := requireService("relay mcp register")
	body, err := json.Marshal(fields)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("mcp.register", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var view mcpView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("registered mcp %q (%s)\n", view.DisplayName, view.ID)
	if view.AuthRequired {
		fmt.Println("  this MCP requires authentication — finish it from the Relay Settings window's Authenticate button")
	}
}

// mcpUnregister sends the name as given: the tray resolves it against its
// own snapshot inside mcp.unregister, so no stale read precedes the
// mutation.
func mcpUnregister(args []string) {
	fs := flag.NewFlagSet("mcp unregister", flag.ExitOnError)
	id := fs.String("id", "", "MCP ID")
	name := fs.String("name", "", "MCP display name")
	fs.Parse(args)
	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	client := requireService("relay mcp unregister")
	body, err := json.Marshal(mcpUnregisterRequest{ID: *id, Name: *name})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("mcp.unregister", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var removed mcpUnregisterRequest
	if err := json.Unmarshal(raw, &removed); err != nil {
		exitError("parse response: %v", err)
	}
	fmt.Printf("unregistered mcp %q\n", removed.ID)
}

func mcpList() {
	mcps := adminRead[mcpListResult]("relay mcp list", "mcp.list", nil).Mcps

	if len(mcps) == 0 {
		fmt.Println("no mcp servers registered")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tENDPOINT")
	for _, m := range mcps {
		transport := m.Transport
		if transport == "" {
			transport = "stdio"
		}
		var endpoint string
		if m.HTTP {
			endpoint = m.URL
		} else {
			endpoint = m.Command
			if len(m.Args) > 0 {
				endpoint += " " + strings.Join(m.Args, " ")
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.ID, m.DisplayName, transport, endpoint)
	}
	w.Flush()
}
