package main

import (
	"errors"
	"flag"
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
	fs := flag.NewFlagSet("mcp register", flag.ExitOnError)
	name := fs.String("name", "", "display name (required)")
	command := fs.String("command", "", "command to run")
	id := fs.String("id", "", "override generated ID")
	transport := fs.String("transport", "stdio", "transport type (stdio or http)")
	mcpURL := fs.String("url", "", "MCP endpoint URL (required for http)")
	var mcpArgs, envPairs stringSlice
	fs.Var(&mcpArgs, "args", "command arguments (repeatable)")
	fs.Var(&envPairs, "env", "environment KEY=VALUE (repeatable)")
	fs.Parse(args)

	if *name == "" {
		fmt.Fprintf(os.Stderr, "error: --name is required\n")
		os.Exit(1)
	}

	if *transport == "http" {
		mcpRegisterHTTP(*name, *id, *mcpURL)
		return
	}

	if *command == "" {
		fmt.Fprintf(os.Stderr, "error: --command is required for stdio transport\n")
		os.Exit(1)
	}

	resolvedID := *id
	if resolvedID == "" {
		resolvedID = slugify(*name)
	}
	if resolvedID == "" {
		fmt.Fprintf(os.Stderr, "error: could not derive ID from name %q\n", *name)
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
		ID:              resolvedID,
		DisplayName:     *name,
		Command:         *command,
		Args:            []string(mcpArgs),
		Env:             env,
		DiscoveredTools: []ToolInfo{},
	}

	var updated bool
	var adminSecret string
	WithSettings(func(s *Settings) {
		adminSecret = s.AdminSecret
		for _, m := range s.ExternalMcps {
			if m.ID == resolvedID {
				s.UpdateExternalMcp(cfg)
				updated = true
				return
			}
		}
		s.AddExternalMcp(cfg)
	})

	if updated {
		fmt.Printf("updated mcp %q (%s)\n", *name, resolvedID)
		_ = bridge.SendReloadMcp(resolvedID, adminSecret)
	} else {
		fmt.Printf("registered mcp %q (%s)\n", *name, resolvedID)
		_ = bridge.SendReconcile(adminSecret)
	}
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

	var updated bool
	var adminSecret string
	WithSettings(func(s *Settings) {
		adminSecret = s.AdminSecret
		for _, m := range s.ExternalMcps {
			if m.ID == id {
				s.UpdateExternalMcp(*result)
				updated = true
				return
			}
		}
		s.AddExternalMcp(*result)
	})

	if updated {
		fmt.Printf("updated mcp %q (%s) with %d tools\n", name, id, len(result.DiscoveredTools))
		_ = bridge.SendReloadMcp(id, adminSecret)
	} else {
		fmt.Printf("registered mcp %q (%s) with %d tools\n", name, id, len(result.DiscoveredTools))
		_ = bridge.SendReconcile(adminSecret)
	}
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
	fs := flag.NewFlagSet("mcp unregister", flag.ExitOnError)
	id := fs.String("id", "", "MCP ID")
	name := fs.String("name", "", "MCP display name")
	fs.Parse(args)

	if *id == "" && *name == "" {
		fmt.Fprintf(os.Stderr, "error: --id or --name is required\n")
		os.Exit(1)
	}

	var resolvedID string
	var adminSecret string
	WithSettings(func(s *Settings) {
		resolvedID = *id
		if resolvedID == "" {
			for _, m := range s.ExternalMcps {
				if m.DisplayName == *name {
					resolvedID = m.ID
					break
				}
			}
		}
		if resolvedID == "" {
			return
		}
		found := false
		for _, m := range s.ExternalMcps {
			if m.ID == resolvedID {
				found = true
				break
			}
		}
		if !found {
			resolvedID = ""
			return
		}
		s.RemoveExternalMcp(resolvedID)
		adminSecret = s.AdminSecret
	})

	if resolvedID == "" {
		if *id != "" {
			fmt.Fprintf(os.Stderr, "error: no mcp found with id %q\n", *id)
		} else {
			fmt.Fprintf(os.Stderr, "error: no mcp found with name %q\n", *name)
		}
		os.Exit(1)
	}

	fmt.Printf("unregistered mcp %q\n", resolvedID)
	_ = bridge.SendReconcile(adminSecret)
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
