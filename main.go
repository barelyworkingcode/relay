package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"relaygo/mcp"
)

func main() {
	logLevel := slog.LevelInfo
	if env := os.Getenv("RELAY_LOG_LEVEL"); env != "" {
		if err := logLevel.UnmarshalText([]byte(env)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid RELAY_LOG_LEVEL %q, using info\n", env)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	if len(os.Args) < 2 {
		runTrayApp()
		return
	}

	switch os.Args[1] {
	case "service":
		runServiceCommand(os.Args[2:])
	case "mcp":
		runMcpOrServer(os.Args[2:])
	case "mcpExec":
		runMcpExec(os.Args[2:])
	case "mcpList":
		exitError("mcpList has been removed. Use: relay mcpExec --token <TOKEN> --list")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: relay [service|mcp|mcpExec]\n", os.Args[1])
		os.Exit(1)
	}
}

// runMcpOrServer dispatches to MCP management subcommands or runs the stdio
// MCP server, depending on the first argument.
func runMcpOrServer(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "register", "unregister", "list":
			runMcpCommand(args)
			return
		}
	}

	// MCP stdio server mode.
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	token := fs.String("token", "", "auth token")
	fs.Parse(args)

	if *token == "" {
		*token = os.Getenv("RELAY_TOKEN")
	}
	if err := mcp.RunMCPServer(*token); err != nil {
		exitError("mcp server error: %v", err)
	}
}
