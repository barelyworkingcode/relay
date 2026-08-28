package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"relaygo/bridge"
)

type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(val string) error {
	*s = append(*s, val)
	return nil
}

func resolveID(id, name string) string {
	if id != "" {
		return id
	}
	return slugify(name)
}

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

type registerOpts struct {
	Name     string
	ID       string
	Args     stringSlice
	EnvPairs stringSlice
}

func addRegisterFlags(fs *flag.FlagSet, opts *registerOpts) {
	fs.StringVar(&opts.Name, "name", "", "display name (required)")
	fs.StringVar(&opts.ID, "id", "", "override generated ID")
	fs.Var(&opts.Args, "args", "command arguments (repeatable)")
	fs.Var(&opts.EnvPairs, "env", "environment KEY=VALUE (repeatable)")
}

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

// upsertAndPrint prints a tool count in the output message when toolCount
// >= 0; a negative value means "omit" rather than "zero".
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

func exitError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func warnNotifyFailure(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: could not notify tray app: %v\n", err)
	}
}

func notifyMcpChange(updated bool, id, adminSecret string) {
	if updated {
		warnNotifyFailure(bridge.SendReloadMcp(id, adminSecret))
	} else {
		warnNotifyFailure(bridge.SendReconcile(adminSecret))
	}
}

type cliSubcommand struct {
	Name string
	Run  func(args []string)
}

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

// resolveAndRemove resolves and removes inside the same store.With() call,
// deliberately, to avoid a TOCTOU race between a separate Get() and With().
func resolveAndRemove(store SettingsStore, entity, id, name string, resolveFn func(*Settings, string, string) string, removeFn func(*Settings, string)) (string, string) {
	if id == "" && name == "" {
		exitError("--id or --name is required")
	}

	var resolvedID string
	var adminSecret string
	if err := store.With(func(s *Settings) {
		resolvedID = resolveFn(s, id, name)
		if resolvedID == "" {
			return // no-op write; entity not found
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

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
