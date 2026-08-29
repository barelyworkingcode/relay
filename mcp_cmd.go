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
// is unaffected: it reads settings.json directly and keeps working with
// the tray stopped.
func runMcpCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("mcp", []cliSubcommand{
		{"register", func(a []string) { mcpRegister(store, a) }},
		{"unregister", func(a []string) { mcpUnregister(store, a) }},
		{"list", func(_ []string) { mcpList(store) }},
	}, args)
}

// mcpRegister covers both transports, matching McpOps.Add itself: an HTTP
// MCP that answers 401 during discovery still gets its record persisted
// (mcpView.AuthRequired says so), and completing OAuth for it is a desktop
// act with no CLI door (ADR-014 section 4 — StartOAuth needs a local
// callback listener and a real browser, and only the Settings window and
// the tray's own IPC ever reach it).
func mcpRegister(store SettingsStore, args []string) {
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

func mcpUnregister(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("mcp unregister", flag.ExitOnError)
	id := fs.String("id", "", "MCP ID")
	name := fs.String("name", "", "MCP display name")
	fs.Parse(args)
	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	client := requireService("relay mcp unregister")
	resolvedID := store.Get().ResolveMcpID(*id, *name)
	if resolvedID == "" {
		if *id != "" {
			exitError("no mcp found with id %q", *id)
		}
		exitError("no mcp found with name %q", *name)
	}

	body, err := json.Marshal(mcpUnregisterRequest{ID: resolvedID})
	if err != nil {
		exitError("%v", err)
	}
	if _, err := client.AdminOp("mcp.unregister", body); err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	fmt.Printf("unregistered mcp %q\n", resolvedID)
}

func mcpList(store SettingsStore) {
	s := store.Get()

	if len(s.ExternalMcps) == 0 {
		fmt.Println("no mcp servers registered")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tENDPOINT")
	for _, m := range s.ExternalMcps {
		transport := m.Transport
		if transport == "" {
			transport = "stdio"
		}
		var endpoint string
		if m.IsHTTP() {
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
