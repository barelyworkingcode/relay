package main

import (
	"flag"
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

// ---------------------------------------------------------------------------
// Shared register helpers — eliminates duplicate flag/validation/upsert logic
// between mcp_cmd.go and service_cmd.go.
// ---------------------------------------------------------------------------

// registerOpts holds flag values common to both mcp and service registration.
type registerOpts struct {
	Name     string
	ID       string
	Args     stringSlice
	EnvPairs stringSlice
}

// addRegisterFlags adds the common --name, --id, --args, --env flags to a flag set.
func addRegisterFlags(fs *flag.FlagSet, opts *registerOpts) {
	fs.StringVar(&opts.Name, "name", "", "display name (required)")
	fs.StringVar(&opts.ID, "id", "", "override generated ID")
	fs.Var(&opts.Args, "args", "command arguments (repeatable)")
	fs.Var(&opts.EnvPairs, "env", "environment KEY=VALUE (repeatable)")
}

// resolveIDAndEnv validates name, resolves the ID, and parses env pairs.
// Exits on validation failure.
func (opts *registerOpts) resolveIDAndEnv() (id string, env map[string]string) {
	if opts.Name == "" {
		exitError("--name is required")
	}
	id = resolveID(opts.ID, opts.Name)
	if id == "" {
		exitError("could not derive ID from name %q", opts.Name)
	}
	var err error
	env, err = parseEnvPairs(opts.EnvPairs)
	if err != nil {
		exitError("%v", err)
	}
	return
}

// upsertAndPrint atomically upserts an entity via store.With, extracts the
// admin secret, and prints the result. Returns whether it was an update and the secret.
// If toolCount >= 0, the tool count is included in the output message.
func upsertAndPrint(store SettingsStore, entity, name, id string, fn func(*Settings) bool, toolCount int) (updated bool, adminSecret string) {
	if err := store.With(func(s *Settings) {
		adminSecret = s.AdminSecret
		updated = fn(s)
	}); err != nil {
		exitError("failed to save settings: %v", err)
	}
	verb := "registered"
	if updated {
		verb = "updated"
	}
	if toolCount >= 0 {
		fmt.Printf("%s %s %q (%s) with %d tools\n", verb, entity, name, id, toolCount)
	} else {
		fmt.Printf("%s %s %q (%s)\n", verb, entity, name, id)
	}
	return
}

// exitError prints an error message to stderr and exits with code 1.
func exitError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// notifyMcpChange sends the appropriate bridge message after an MCP upsert.
// Updated MCPs get a targeted reload; new MCPs trigger a full reconcile.
func notifyMcpChange(updated bool, id, adminSecret string) {
	var err error
	if updated {
		err = bridge.SendReloadMcp(id, adminSecret)
	} else {
		err = bridge.SendReconcile(adminSecret)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: could not notify tray app: %v\n", err)
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

// resolveAndRemove resolves an entity by id/name, removes it via store.With, and
// prints the result. Returns the resolved ID and admin secret, or exits with an
// error if not found.
func resolveAndRemove(store SettingsStore, entity, id, name string, resolveFn func(*Settings, string, string) string, removeFn func(*Settings, string)) (string, string) {
	if id == "" && name == "" {
		exitError("--id or --name is required")
	}

	var resolvedID string
	var adminSecret string
	if err := store.With(func(s *Settings) {
		resolvedID = resolveFn(s, id, name)
		if resolvedID == "" {
			return
		}
		removeFn(s, resolvedID)
		adminSecret = s.AdminSecret
	}); err != nil {
		exitError("failed to save settings: %v", err)
	}

	if resolvedID == "" {
		if id != "" {
			exitError("no %s found with id %q", entity, id)
		} else {
			exitError("no %s found with name %q", entity, name)
		}
	}

	fmt.Printf("unregistered %s %q\n", entity, resolvedID)
	return resolvedID, adminSecret
}

// newTabWriter returns a tabwriter configured for CLI list output.
func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
