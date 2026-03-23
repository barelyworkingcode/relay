package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"relaygo/bridge"
)

func runMcpCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("mcp", []cliSubcommand{
		{"register", func(a []string) { mcpRegister(store, a) }},
		{"unregister", func(a []string) { mcpUnregister(store, a) }},
		{"list", func(_ []string) { mcpList(store) }},
	}, args)
}

func mcpRegister(store *SettingsStore, args []string) {
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
		exitError("--name is required")
	}

	if *transport == "http" {
		mcpRegisterHTTP(store, *name, *id, *mcpURL)
		return
	}

	if *command == "" {
		exitError("--command is required for stdio transport")
	}

	resolvedID := resolveID(*id, *name)
	if resolvedID == "" {
		exitError("could not derive ID from name %q", *name)
	}

	env, err := parseEnvPairs(envPairs)
	if err != nil {
		exitError("%v", err)
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
	store.With(func(s *Settings) {
		adminSecret = s.AdminSecret
		updated = s.UpsertExternalMcp(cfg)
	})

	printUpsertResult("mcp", *name, resolvedID, updated)
	notifyMcpChange(updated, resolvedID, adminSecret)
}

func mcpRegisterHTTP(store *SettingsStore, name, id, mcpURL string) {
	if mcpURL == "" {
		exitError("--url is required for HTTP transport")
	}
	if err := validateMcpURL(mcpURL); err != nil {
		exitError("%v", err)
	}

	id = resolveID(id, name)
	if id == "" {
		exitError("could not derive ID from name %q", name)
	}

	fmt.Printf("discovering HTTP MCP %q at %s...\n", name, mcpURL)

	result, err := DiscoverHTTPMcp(context.Background(), name, id, mcpURL, nil)
	if err != nil && !errors.Is(err, ErrAuthRequired) {
		exitError("%v", err)
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
			result, err = DiscoverHTTPMcp(context.Background(), name, id, mcpURL, oauth)
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
	store.With(func(s *Settings) {
		adminSecret = s.AdminSecret
		updated = s.UpsertExternalMcp(*result)
	})

	printUpsertResult("mcp", name, id, updated, len(result.DiscoveredTools))
	notifyMcpChange(updated, id, adminSecret)
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

func mcpUnregister(store *SettingsStore, args []string) {
	fs := flag.NewFlagSet("mcp unregister", flag.ExitOnError)
	id := fs.String("id", "", "MCP ID")
	name := fs.String("name", "", "MCP display name")
	fs.Parse(args)

	_, adminSecret := resolveAndRemove(store, "mcp", id, name,
		(*Settings).ResolveMcpID, (*Settings).RemoveExternalMcp)
	_ = bridge.SendReconcile(adminSecret)
}

func mcpList(store *SettingsStore) {
	s := store.Get()

	if len(s.ExternalMcps) == 0 {
		fmt.Println("no mcp servers registered")
		return
	}

	w := newTabWriter()
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
