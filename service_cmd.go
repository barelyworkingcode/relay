package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"relaygo/bridge"
)

func runServiceCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("service", []cliSubcommand{
		{"register", func(a []string) { serviceRegister(store, a) }},
		{"unregister", func(a []string) { serviceUnregister(store, a) }},
		{"list", func(_ []string) { serviceList(store) }},
	}, args)
}

func serviceRegister(store *SettingsStore, args []string) {
	fs := flag.NewFlagSet("service register", flag.ExitOnError)
	name := fs.String("name", "", "display name (required)")
	command := fs.String("command", "", "command to run (required)")
	id := fs.String("id", "", "override generated ID")
	workdir := fs.String("workdir", "", "working directory")
	url := fs.String("url", "", "service URL")
	autostart := fs.Bool("autostart", false, "start automatically")
	var svcArgs, envPairs stringSlice
	fs.Var(&svcArgs, "args", "command arguments (repeatable)")
	fs.Var(&envPairs, "env", "environment KEY=VALUE (repeatable)")
	fs.Parse(args)

	if *name == "" {
		exitError("--name is required")
	}
	if *command == "" {
		exitError("--command is required")
	}

	resolvedID := resolveID(*id, *name)
	if resolvedID == "" {
		exitError("could not derive ID from name %q", *name)
	}

	resolvedWorkdir := *workdir
	if resolvedWorkdir != "" {
		abs, err := filepath.Abs(resolvedWorkdir)
		if err != nil {
			exitError("could not resolve workdir: %v", err)
		}
		resolvedWorkdir = abs
	}

	env, err := parseEnvPairs(envPairs)
	if err != nil {
		exitError("%v", err)
	}

	config := ServiceConfig{
		ID:          resolvedID,
		DisplayName: *name,
		Command:     *command,
		Args:        []string(svcArgs),
		Env:         env,
		WorkingDir:  resolvedWorkdir,
		Autostart:   *autostart,
		URL:         *url,
	}

	var updated bool
	var adminSecret string
	store.With(func(s *Settings) {
		adminSecret = s.AdminSecret
		s.MergeServiceDefaults(&config)
		updated = s.UpsertService(config)
	})

	printUpsertResult("service", *name, resolvedID, updated)

	if err := bridge.SendReloadService(resolvedID, adminSecret); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not notify tray app: %v\n", err)
	}
}

func serviceUnregister(store *SettingsStore, args []string) {
	fs := flag.NewFlagSet("service unregister", flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	name := fs.String("name", "", "service display name")
	fs.Parse(args)

	_, _ = resolveAndRemove(store, "service", id, name,
		(*Settings).ResolveServiceID, (*Settings).RemoveService)
}

func serviceList(store *SettingsStore) {
	s := store.Get()

	if len(s.Services) == 0 {
		fmt.Println("no services registered")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tCOMMAND\tURL\tAUTOSTART")
	for _, svc := range s.Services {
		cmd := svc.Command
		if len(svc.Args) > 0 {
			cmd += " " + strings.Join(svc.Args, " ")
		}
		auto := "no"
		if svc.Autostart {
			auto = "yes"
		}
		urlStr := svc.URL
		if urlStr == "" {
			urlStr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", svc.ID, svc.DisplayName, cmd, urlStr, auto)
	}
	w.Flush()
}
