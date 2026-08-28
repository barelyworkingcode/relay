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
	// An empty token is not fatal: the bridge falls back to directory auth for
	// projects that opted in. The CLI can't know which projects opted in, so
	// it defers to the bridge and reports whatever it says (tokenlessHint
	// expands the denial).
	if !*list && *tool == "" {
		fmt.Fprintln(os.Stderr, "error: must specify --list or --tool")
		os.Exit(1)
	}

	client := bridge.NewClient(*token)
	tokenless := *token == ""

	if *list {
		raw, err := client.ListTools()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			tokenlessHint(tokenless)
			os.Exit(1)
		}

		var tools []mcp.Tool
		if err := json.Unmarshal(raw, &tools); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing tools: %v\n", err)
			os.Exit(1)
		}

		if *schema {
			// Skill generators consume this full-schema form to render parameter docs.
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
		tokenlessHint(tokenless)
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

// tokenlessHint only fires after a failure on a run that supplied no token at
// all — a bad-token failure is a different problem and shouldn't be answered
// with directory-auth advice.
func tokenlessHint(tokenless bool) {
	if !tokenless {
		return
	}
	fmt.Fprintln(os.Stderr, "\nNo token was supplied. Either:")
	fmt.Fprintln(os.Stderr, "  export RELAY_PROJECT_TOKEN=<project-token>   # Settings UI → Projects → Bearer Token")
	fmt.Fprintln(os.Stderr, "  or enable Settings UI → Projects → Directory Auth for the project containing this directory")
}

// resolveToolArgs: the --args-file/stdin form exists because it's the
// shell-quoting-safe path generated SKILL.md files rely on for prompts
// containing quotes, apostrophes ("Van Gogh's"), or parentheses.
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
