package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"relaygo/bridge"
	"relaygo/mcp"
)

func runMcpExec(args []string) {
	var (
		token    string
		list     bool
		tool     string
		toolArgs string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--token":
			i++
			if i < len(args) {
				token = args[i]
			}
		case "--list":
			list = true
		case "--tool":
			i++
			if i < len(args) {
				tool = args[i]
			}
		case "--args":
			i++
			if i < len(args) {
				toolArgs = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if token == "" {
		fmt.Fprintf(os.Stderr, "Usage: relay mcpExec --token <TOKEN> [--list | --tool <name> [--args '<json>']]\n")
		os.Exit(1)
	}
	if !list && tool == "" {
		fmt.Fprintf(os.Stderr, "error: must specify --list or --tool\n")
		os.Exit(1)
	}

	client := bridge.NewClient(token)

	if list {
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
		return
	}

	// Call tool.
	var argsJSON json.RawMessage
	if toolArgs != "" {
		if !json.Valid([]byte(toolArgs)) {
			fmt.Fprintf(os.Stderr, "error: invalid --args JSON\n")
			os.Exit(1)
		}
		argsJSON = json.RawMessage(toolArgs)
	}

	raw, err := client.CallTool(tool, argsJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var result mcp.CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing result: %v\n", err)
		os.Exit(1)
	}

	for _, c := range result.Content {
		if c.Text != "" {
			fmt.Println(c.Text)
		}
	}

	if result.IsError {
		os.Exit(1)
	}
}
