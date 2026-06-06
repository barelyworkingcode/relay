package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"relaygo/bridge"
	"relaygo/mcp"
)

func runMcpExec(args []string) {
	fs := flag.NewFlagSet("mcpExec", flag.ExitOnError)
	token := fs.String("token", "", "auth token (prefer RELAY_PROJECT_TOKEN env)")
	list := fs.Bool("list", false, "list available tools")
	schema := fs.Bool("schema", false, "with --list, emit JSON including each tool's input schema")
	tool := fs.String("tool", "", "tool name to call")
	toolArgs := fs.String("args", "", "tool arguments as JSON")
	argsFile := fs.String("args-file", "", "read tool arguments JSON from a file, or '-' for stdin (avoids shell-quoting issues with quotes/apostrophes/parens)")
	fs.Parse(args)

	if *token == "" {
		*token = os.Getenv(bridge.EnvProjectToken)
	}
	if *token == "" {
		// Transition: accept the legacy env name from an un-migrated spawner.
		*token = os.Getenv(bridge.EnvProjectTokenLegacy)
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "error: RELAY_PROJECT_TOKEN not set (and --token not provided)")
		fmt.Fprintln(os.Stderr, "  export RELAY_PROJECT_TOKEN=<project-token>  # find it in Settings UI → Projects")
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
			// Truncate by rune, not byte: a byte slice can split a multi-byte
			// rune (em-dash, accent, emoji) and emit invalid UTF-8 to the
			// terminal — the same hazard fixed elsewhere in the codebase.
			if r := []rune(desc); len(r) > 80 {
				desc = string(r[:77]) + "..."
			}
			fmt.Fprintf(w, "%s\t%s\n", t.Name, desc)
		}
		w.Flush()
		fmt.Printf("\n%d tools available\n", len(tools))
		return
	}

	// Call tool. Arguments come from --args-file (a path, or "-" for stdin) or
	// inline --args. The file/stdin form avoids shell-quoting pitfalls for
	// prompts containing quotes, apostrophes ("Van Gogh's"), or parentheses.
	if *argsFile != "" && *toolArgs != "" {
		fmt.Fprintln(os.Stderr, "error: pass either --args or --args-file, not both")
		os.Exit(1)
	}
	var rawArgs string
	switch {
	case *argsFile == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading --args-file from stdin: %v\n", err)
			os.Exit(1)
		}
		rawArgs = string(data)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading --args-file: %v\n", err)
			os.Exit(1)
		}
		rawArgs = string(data)
	default:
		rawArgs = *toolArgs
	}

	var argsJSON json.RawMessage
	if strings.TrimSpace(rawArgs) != "" {
		if !json.Valid([]byte(rawArgs)) {
			fmt.Fprintf(os.Stderr, "error: invalid args JSON\n")
			os.Exit(1)
		}
		argsJSON = json.RawMessage(rawArgs)
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
