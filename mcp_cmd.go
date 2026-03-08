package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
		name      string
		command   string
		id        string
		transport string
		mcpURL    string
		mcpArgs   []string
		envPairs  []string
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
		case "--transport":
			i++
			if i < len(args) {
				transport = args[i]
			}
		case "--url":
			i++
			if i < len(args) {
				mcpURL = args[i]
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

	if transport == "" {
		transport = "stdio"
	}

	if transport == "http" {
		mcpRegisterHTTP(name, id, mcpURL)
		return
	}

	if command == "" {
		fmt.Fprintf(os.Stderr, "error: --command is required for stdio transport\n")
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
			_ = bridge.SendReloadMcp(id)
			return
		}
	}

	s.AddExternalMcp(cfg)
	fmt.Printf("registered mcp %q (%s)\n", name, id)
	_ = bridge.SendReconcile()
}

func mcpRegisterHTTP(name, id, mcpURL string) {
	if mcpURL == "" {
		fmt.Fprintf(os.Stderr, "error: --url is required for HTTP transport\n")
		os.Exit(1)
	}
	if err := validateMcpURL(mcpURL); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if id == "" {
		id = slugify(name)
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "error: could not derive ID from name %q\n", name)
		os.Exit(1)
	}

	fmt.Printf("discovering HTTP MCP %q at %s...\n", name, mcpURL)

	result, err := DiscoverHTTPMcp(name, id, mcpURL, nil)
	if err != nil && !errors.Is(err, ErrAuthRequired) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if errors.Is(err, ErrAuthRequired) {
		fmt.Println("server requires authentication, starting OAuth flow...")
		oauth, oauthErr := startOAuthFlow(mcpURL, openBrowserCmd)
		if oauthErr != nil {
			fmt.Fprintf(os.Stderr, "OAuth failed: %v\n", oauthErr)
			fmt.Println("registering without authentication -- authenticate later via settings UI")
			result.OAuthState = nil
		} else {
			fmt.Println("authentication successful, retrying discovery...")
			result, err = DiscoverHTTPMcp(name, id, mcpURL, oauth)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error after auth: %v\n", err)
				// Still register with the OAuth state.
				result = &ExternalMcp{
					ID:              id,
					DisplayName:     name,
					Transport:       "http",
					URL:             mcpURL,
					OAuthState:      oauth,
					DiscoveredTools: []ToolInfo{},
				}
			} else {
				result.OAuthState = oauth
			}
		}
	}

	s := LoadSettings()

	for _, m := range s.ExternalMcps {
		if m.ID == id {
			s.UpdateExternalMcp(*result)
			fmt.Printf("updated mcp %q (%s) with %d tools\n", name, id, len(result.DiscoveredTools))
			_ = bridge.SendReloadMcp(id)
			return
		}
	}

	s.AddExternalMcp(*result)
	fmt.Printf("registered mcp %q (%s) with %d tools\n", name, id, len(result.DiscoveredTools))
	_ = bridge.SendReconcile()
}

// openBrowserCmd opens a URL in the default browser.
func openBrowserCmd(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
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
	fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tENDPOINT\tTOOLS")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", m.ID, m.DisplayName, transport, endpoint, len(m.DiscoveredTools))
	}
	w.Flush()
}
