package main

import (
	"flag"
	"log/slog"
	"os"

	"relaygo/mcp"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

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
		runTrayApp()
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
