package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"relaygo/bridge"
	"relaygo/mcp"
)

func runMcpExec(args []string) {
	fs := flag.NewFlagSet("mcpExec", flag.ExitOnError)
	token := fs.String("token", "", "auth token")
	list := fs.Bool("list", false, "list available tools")
	tool := fs.String("tool", "", "tool name to call")
	toolArgs := fs.String("args", "", "tool arguments as JSON")
	fs.Parse(args)

	if *token == "" {
		*token = os.Getenv("RELAY_TOKEN")
	}
	if *token == "" {
		fmt.Fprintf(os.Stderr, "Usage: relay mcpExec --token <TOKEN> [--list | --tool <name> [--args '<json>']]\n")
		os.Exit(1)
	}
	if !*list && *tool == "" {
		fmt.Fprintf(os.Stderr, "error: must specify --list or --tool\n")
		os.Exit(1)
	}

	client := bridge.NewClient(*token)

	if *list {
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

		w := newTabWriter()
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
	if *toolArgs != "" {
		if !json.Valid([]byte(*toolArgs)) {
			fmt.Fprintf(os.Stderr, "error: invalid --args JSON\n")
			os.Exit(1)
		}
		argsJSON = json.RawMessage(*toolArgs)
	}

	raw, err := client.CallTool(*tool, argsJSON)
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
