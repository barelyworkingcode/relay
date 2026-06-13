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

	argsJSON, err := resolveToolArgs(*argsFile, *toolArgs, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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

// resolveToolArgs determines the tool-arguments JSON from the mutually-exclusive
// --args-file (a path, or "-" for stdin) and inline --args flags. The file/stdin
// form is the shell-quoting-safe path that generated SKILL.md files rely on for
// prompts containing quotes, apostrophes ("Van Gogh's"), or parentheses.
// Returns (nil, nil) when no arguments are supplied; an error on conflicting
// flags, an unreadable source, or invalid JSON.
func resolveToolArgs(argsFile, toolArgs string, stdin io.Reader) (json.RawMessage, error) {
	if argsFile != "" && toolArgs != "" {
		return nil, fmt.Errorf("pass either --args or --args-file, not both")
	}
	var rawArgs string
	switch {
	case argsFile == "-":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("reading --args-file from stdin: %w", err)
		}
		rawArgs = string(data)
	case argsFile != "":
		data, err := os.ReadFile(argsFile)
		if err != nil {
			return nil, fmt.Errorf("reading --args-file: %w", err)
		}
		rawArgs = string(data)
	default:
		rawArgs = toolArgs
	}

	if strings.TrimSpace(rawArgs) == "" {
		return nil, nil
	}
	if !json.Valid([]byte(rawArgs)) {
		return nil, fmt.Errorf("invalid args JSON")
	}
	return json.RawMessage(rawArgs), nil
}
