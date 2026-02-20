package main

import (
	"fmt"
	"os"

	"relaygo/mcp"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "service" {
		runServiceCommand(os.Args[2:])
		return
	}

	if len(os.Args) >= 2 && os.Args[1] == "mcp" {
		// Check for register/unregister/list subcommands.
		if len(os.Args) >= 3 {
			switch os.Args[2] {
			case "register", "unregister", "list":
				runMcpCommand(os.Args[2:])
				return
			}
		}

		// MCP stdio server mode
		var token string
		for i, arg := range os.Args {
			if arg == "--token" && i+1 < len(os.Args) {
				token = os.Args[i+1]
			}
		}
		if err := mcp.RunMCPServer(token); err != nil {
			fmt.Fprintf(os.Stderr, "mcp server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Tray app mode
	runTrayApp()
}
