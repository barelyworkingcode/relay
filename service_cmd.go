package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"relaygo/bridge"
)

func runServiceCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("service", []cliSubcommand{
		{"register", func(a []string) { serviceRegister(store, a) }},
		{"unregister", func(a []string) { serviceUnregister(store, a) }},
		{"restart", func(a []string) { serviceRestart(store, a) }},
		{"list", func(_ []string) { serviceList(store) }},
	}, args)
}

func serviceRegister(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("service register", flag.ExitOnError)
	var opts registerOpts
	addRegisterFlags(fs, &opts)
	command := fs.String("command", "", "command to run (required)")
	workdir := fs.String("workdir", "", "working directory")
	url := fs.String("url", "", "service URL")
	autostart := fs.Bool("autostart", false, "start automatically")
	noFrontendCreds := fs.Bool("no-frontend-creds", false, "do not inject relay front-door creds (RELAY_FRONTEND_SOCKET/TOKEN); set for backends that never dial the front door, so the bearer can't leak into spawned shells")
	fs.Parse(args)

	if *command == "" {
		exitError("--command is required")
	}

	// nil (flag absent) leaves the setting untouched on re-register (see
	// MergeServiceDefaults); an explicit false opts the service out.
	var frontendConsumer *bool
	if *noFrontendCreds {
		f := false
		frontendConsumer = &f
	}

	id, env := opts.resolveIDAndEnv()

	resolvedWorkdir := *workdir
	if resolvedWorkdir != "" {
		abs, err := filepath.Abs(resolvedWorkdir)
		if err != nil {
			exitError("could not resolve workdir: %v", err)
		}
		resolvedWorkdir = abs
	}

	config := ServiceConfig{
		ID:               id,
		DisplayName:      opts.Name,
		Command:          *command,
		Args:             []string(opts.Args),
		Env:              env,
		WorkingDir:       resolvedWorkdir,
		Autostart:        *autostart,
		URL:              *url,
		FrontendConsumer: frontendConsumer,
	}

	_, secret := upsertAndPrint(store, "service", opts.Name, id, func(s *Settings) bool {
		s.MergeServiceDefaults(&config)
		return s.UpsertService(config)
	}, -1)

	warnNotifyFailure(bridge.SendReloadService(id, secret))
}

func serviceUnregister(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("service unregister", flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	name := fs.String("name", "", "service display name")
	fs.Parse(args)

	resolvedID, adminSecret := resolveAndRemove(store, "service", *id, *name,
		(*Settings).ResolveServiceID, (*Settings).RemoveService)
	warnNotifyFailure(bridge.SendReloadService(resolvedID, adminSecret))
}

// serviceRestart triggers an in-place Stop+Start of a service via the bridge.
// Sends the same ReloadService message that an upsert sends, which the tray
// already implements as Stop → Start. Without a running tray this is a no-op
// (the warning surfaces via warnNotifyFailure).
func serviceRestart(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("service restart", flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	name := fs.String("name", "", "service display name")
	fs.Parse(args)

	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	s := store.Get()
	resolvedID := s.ResolveServiceID(*id, *name)
	if resolvedID == "" {
		if *id != "" {
			exitError("no service found with id %q", *id)
		}
		exitError("no service found with name %q", *name)
	}

	fmt.Printf("restarting service %q\n", resolvedID)
	warnNotifyFailure(bridge.SendReloadService(resolvedID, s.AdminSecret))
}

func serviceList(store SettingsStore) {
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
