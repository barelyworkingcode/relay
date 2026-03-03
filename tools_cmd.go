package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"relaygo/bridge"
	"relaygo/mcp"
)

func runMcpList(args []string) {
	var token string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--token":
			i++
			if i < len(args) {
				token = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if token == "" {
		fmt.Fprintf(os.Stderr, "Usage: relay mcpList --token <TOKEN>\n")
		os.Exit(1)
	}

	client := bridge.NewClient(token)
	raw, err := client.ListTools()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing tools: %v\n", err)
		os.Exit(1)
	}

	if len(tools) == 0 {
		fmt.Println("no tools available for this token")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tDESCRIPTION")
	for _, t := range tools {
		desc := t.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\n", t.Name, desc)
	}
	w.Flush()

	fmt.Printf("\n%d tools available\n", len(tools))
}
