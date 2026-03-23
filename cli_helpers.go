package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"relaygo/bridge"
)

// stringSlice implements flag.Value for repeated string flags.
type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(val string) error {
	*s = append(*s, val)
	return nil
}

// resolveID returns id if non-empty, otherwise slugifies name.
func resolveID(id, name string) string {
	if id != "" {
		return id
	}
	return slugify(name)
}

// parseEnvPairs parses KEY=VALUE pairs into a map.
func parseEnvPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --env format %q (expected KEY=VALUE)", pair)
		}
		env[k] = v
	}
	return env, nil
}

// exitError prints an error message to stderr and exits with code 1.
func exitError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// notifyMcpChange sends the appropriate bridge message after an MCP upsert.
// Updated MCPs get a targeted reload; new MCPs trigger a full reconcile.
func notifyMcpChange(updated bool, id, adminSecret string) {
	if updated {
		_ = bridge.SendReloadMcp(id, adminSecret)
	} else {
		_ = bridge.SendReconcile(adminSecret)
	}
}

// ---------------------------------------------------------------------------
// Shared CLI subcommand framework
// ---------------------------------------------------------------------------

// cliSubcommand describes a named subcommand for a CLI verb (mcp, service).
type cliSubcommand struct {
	Name string
	Run  func(args []string)
}

// runSubcommands dispatches to the matching subcommand or prints usage and exits.
func runSubcommands(verb string, commands []cliSubcommand, args []string) {
	if len(args) == 0 {
		printSubcommandUsage(verb, commands)
		os.Exit(1)
	}
	for _, cmd := range commands {
		if args[0] == cmd.Name {
			cmd.Run(args[1:])
			return
		}
	}
	fmt.Fprintf(os.Stderr, "unknown %s command: %s\n", verb, args[0])
	printSubcommandUsage(verb, commands)
	os.Exit(1)
}

func printSubcommandUsage(verb string, commands []cliSubcommand) {
	fmt.Fprintf(os.Stderr, "Usage: relay %s <command>\n\nCommands:\n", verb)
	for _, cmd := range commands {
		fmt.Fprintf(os.Stderr, "  %s\n", cmd.Name)
	}
}

// printUpsertResult prints a consistent "registered" or "updated" message.
// If toolCount is provided, appends "with N tools".
func printUpsertResult(entity, name, id string, updated bool, toolCount ...int) {
	verb := "registered"
	if updated {
		verb = "updated"
	}
	if len(toolCount) > 0 {
		fmt.Printf("%s %s %q (%s) with %d tools\n", verb, entity, name, id, toolCount[0])
	} else {
		fmt.Printf("%s %s %q (%s)\n", verb, entity, name, id)
	}
}

// resolveAndRemove resolves an entity by id/name, removes it via store.With, and
// prints the result. Returns the resolved ID and admin secret, or exits with an
// error if not found.
func resolveAndRemove(store *SettingsStore, entity string, id, name *string, resolveFn func(*Settings, string, string) string, removeFn func(*Settings, string)) (string, string) {
	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	var resolvedID string
	var adminSecret string
	store.With(func(s *Settings) {
		resolvedID = resolveFn(s, *id, *name)
		if resolvedID == "" {
			return
		}
		removeFn(s, resolvedID)
		adminSecret = s.AdminSecret
	})

	if resolvedID == "" {
		if *id != "" {
			exitError("no %s found with id %q", entity, *id)
		} else {
			exitError("no %s found with name %q", entity, *name)
		}
	}

	fmt.Printf("unregistered %s %q\n", entity, resolvedID)
	return resolvedID, adminSecret
}

// newTabWriter returns a tabwriter configured for CLI list output.
func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
