package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

// register, unregister and restart are brokered (ADR-017 decision 2): this
// process holds no sealer (§5.4), so it dials the running tray over
// admin_op and lets ServiceOps — the same core the Services tab and
// RegisterServiceRoutes share — do the work. `list` is unaffected: it reads
// settings.json directly and keeps working with the tray stopped.
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

	if opts.Name == "" {
		exitError("--name is required")
	}
	if *command == "" {
		exitError("--command is required")
	}

	// visited is which flags actually appeared on the command line, per
	// flag.FlagSet.Visit — the CLI's only way to tell "the operator left
	// this out" from "the operator set it to the zero value" for --workdir,
	// --url and --autostart. A flag not in visited leaves the field nil, so
	// ServiceOps.Update carries the stored value forward unchanged instead
	// of clearing it (§6.4); one in visited is applied even at its zero
	// value, which is what lets --autostart=false turn autostart off.
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	// nil (flag absent) leaves the setting untouched on re-register (see
	// ServiceOps.Update's own merge for FrontendConsumer); an explicit
	// false opts the service out. There is deliberately no way to turn it
	// back on from the CLI (no --frontend-creds counterpart): the opt-out
	// is the only control on the frontend socket credential and re-arming
	// it is not a `register` concern.
	var frontendConsumer *bool
	if *noFrontendCreds {
		f := false
		frontendConsumer = &f
	}

	env, err := parseEnvPairs(opts.EnvPairs)
	if err != nil {
		exitError("%v", err)
	}

	var workingDir *string
	if visited["workdir"] {
		resolved := *workdir
		if resolved != "" {
			abs, err := filepath.Abs(resolved)
			if err != nil {
				exitError("could not resolve workdir: %v", err)
			}
			resolved = abs
		}
		workingDir = &resolved
	}

	var serviceURL *string
	if visited["url"] {
		serviceURL = url
	}

	var autostartSet *bool
	if visited["autostart"] {
		autostartSet = autostart
	}

	fields := serviceFields{
		ID:               opts.ID,
		DisplayName:      opts.Name,
		Command:          *command,
		Args:             []string(opts.Args),
		Env:              env,
		WorkingDir:       workingDir,
		Autostart:        autostartSet,
		URL:              serviceURL,
		FrontendConsumer: frontendConsumer,
	}

	client := requireService("relay service register")
	body, err := json.Marshal(fields)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("service.register", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var view serviceView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("registered service %q (%s)\n", view.DisplayName, view.ID)
	if view.ProcessError != "" {
		fmt.Printf("  note: %s\n", view.ProcessError)
	}
}

func serviceUnregister(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("service unregister", flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	name := fs.String("name", "", "service display name")
	fs.Parse(args)
	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	client := requireService("relay service unregister")
	resolvedID := store.Get().ResolveServiceID(*id, *name)
	if resolvedID == "" {
		if *id != "" {
			exitError("no service found with id %q", *id)
		}
		exitError("no service found with name %q", *name)
	}

	body, err := json.Marshal(serviceUnregisterRequest{ID: resolvedID})
	if err != nil {
		exitError("%v", err)
	}
	if _, err := client.AdminOp("service.unregister", body); err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	fmt.Printf("unregistered service %q\n", resolvedID)
}

// serviceRestart is not gated (§6.4): it changes no settings, it restarts
// what is already configured, and the edit that configured it was gated
// already. It is still brokered — this process cannot reach the registry
// that owns the running process, only the tray can — but no presence
// prompt is expected here.
func serviceRestart(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("service restart", flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	name := fs.String("name", "", "service display name")
	fs.Parse(args)
	if *id == "" && *name == "" {
		exitError("--id or --name is required")
	}

	client := requireService("relay service restart")
	resolvedID := store.Get().ResolveServiceID(*id, *name)
	if resolvedID == "" {
		if *id != "" {
			exitError("no service found with id %q", *id)
		}
		exitError("no service found with name %q", *name)
	}

	fmt.Printf("restarting service %q\n", resolvedID)
	body, err := json.Marshal(serviceRestartRequest{ID: resolvedID})
	if err != nil {
		exitError("%v", err)
	}
	if _, err := client.AdminOp("service.restart", body); err != nil {
		exitError("%s", adminOpErrorText(err))
	}
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
