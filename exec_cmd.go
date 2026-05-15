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
	token := fs.String("token", "", "auth token (prefer RELAY_TOKEN env)")
	list := fs.Bool("list", false, "list available tools")
	schema := fs.Bool("schema", false, "with --list, emit JSON including each tool's input schema")
	tool := fs.String("tool", "", "tool name to call")
	toolArgs := fs.String("args", "", "tool arguments as JSON")
	fs.Parse(args)

	if *token == "" {
		*token = os.Getenv("RELAY_TOKEN")
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "error: RELAY_TOKEN not set (and --token not provided)")
		fmt.Fprintln(os.Stderr, "  export RELAY_TOKEN=<project-token>  # find it in Settings UI → Projects")
		fmt.Fprintln(os.Stderr, "Usage: relay mcp call [--list [--schema] | --tool <name> [--args '<json>']]")
		os.Exit(1)
	}
	if !*list && *tool == "" {
		fmt.Fprintln(os.Stderr, "error: must specify --list or --tool")
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

		if *schema {
			// Emit full tool definitions (including input schemas) as pretty JSON.
			// Skill generators consume this to render parameter docs.
			out, err := json.MarshalIndent(tools, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error encoding tools: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(out))
			return
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
