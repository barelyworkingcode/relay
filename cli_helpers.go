package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(val string) error {
	*s = append(*s, val)
	return nil
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

// registerOpts is the flag set `mcp register` and `service register` share.
// There is deliberately no --id override: McpOps.Add and ServiceOps.Create
// both derive the record's id from --name (slugify) unconditionally, the
// same as every other door (HTTP, IPC) always has, so a flag that could
// never take effect once brokered would be worse than no flag at all.
type registerOpts struct {
	Name     string
	Args     stringSlice
	EnvPairs stringSlice
}

func addRegisterFlags(fs *flag.FlagSet, opts *registerOpts) {
	fs.StringVar(&opts.Name, "name", "", "display name (required)")
	fs.Var(&opts.Args, "args", "command arguments (repeatable)")
	fs.Var(&opts.EnvPairs, "env", "environment KEY=VALUE (repeatable)")
}

func exitError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
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

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
