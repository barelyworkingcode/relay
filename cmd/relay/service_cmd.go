package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
)

// register, unregister and restart are brokered (ADR-017 decision 2): this
// process holds no sealer (§5.4), so it dials the running tray over
// admin_op and lets ServiceOps — the same core the Services tab and
// RegisterServiceRoutes share — do the work. `list` is unaffected: it reads
// settings.json directly and keeps working with the tray stopped.
func runServiceCommand(args []string) {
	store := config.NewSettingsStore()
	runSubcommands("service", []cliSubcommand{
		{"register", serviceRegister},
		{"unregister", func(a []string) { serviceUnregister(store, a) }},
		{"restart", func(a []string) { serviceRestart(store, a) }},
		{"list", func(_ []string) { serviceList(store) }},
	}, args)
}

func serviceRegister(args []string) {
	fs := flag.NewFlagSet("service register", flag.ExitOnError)
	var opts registerOpts
	addRegisterFlags(fs, &opts)
	command := fs.String("command", "", "command to run (required)")
	workdir := fs.String("workdir", "", "working directory")
	url := fs.String("url", "", "service URL")
	autostart := fs.Bool("autostart", false, "start automatically")
	var capabilityFlags stringSlice
	fs.Var(&capabilityFlags, "capability", "grant this service's launch identity a capability, repeatable: frontend (the frontend socket as read+configure+proxy), manifest (RegisterManifest), projects (ResolvePtyEnv, ResolveProjectTemplate, ListProjects, GetProject, service ListTools/CallTool), models (model-endpoint calls, limited by --allowed-model), model_host (RegisterModelHost); none given means none held")
	var allowedModelFlags stringSlice
	fs.Var(&allowedModelFlags, "allowed-model", "grant the models capability access to this model id, repeatable; omitted or none given means no models; pass \"*\" for every model")
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

	// This is deliberate: capabilities are always sent, never left to
	// Update's absent-preserves-existing merge. A register names every
	// capability the service holds, so repeating a register without them
	// narrows to none rather than silently keeping a grant nobody restated.
	capabilities, err := parseServiceCapabilities(capabilityFlags)
	if err != nil {
		exitError("%v", err)
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

	// This is deliberate, same as capabilities: a register names every model
	// this service is allowed, always sent rather than left to Update's
	// absent-preserves-existing merge, so repeating a register without
	// --allowed-model narrows to none rather than silently keeping a grant
	// nobody restated.
	allowedModels := []string(allowedModelFlags)

	fields := serviceFields{
		ID:            opts.ID,
		DisplayName:   opts.Name,
		Command:       *command,
		Args:          []string(opts.Args),
		Env:           envValuesToWire(env),
		WorkingDir:    workingDir,
		Autostart:     autostartSet,
		URL:           serviceURL,
		Capabilities:  &capabilities,
		AllowedModels: &allowedModels,
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
	fmt.Printf("  capabilities: %s\n", capabilitiesColumn(view.Capabilities))
	if len(view.Capabilities) == 0 {
		fmt.Println("  note: no capabilities: this service can start and say Hello, and can do nothing else through relay; pass --capability frontend|manifest|projects to grant one")
	}
	if slices.Contains(view.Capabilities, config.ServiceCapabilityModels) {
		if len(view.AllowedModels) == 0 {
			fmt.Println("  allowed models: none (note: an empty list means NO models for a service, the opposite of a project's default; pass --allowed-model ID, repeatable, or --allowed-model '*' for every model)")
		} else {
			fmt.Printf("  allowed models: %s\n", strings.Join(view.AllowedModels, ","))
		}
	}
	if view.ProcessError != "" {
		fmt.Printf("  note: %s\n", view.ProcessError)
	}
}

// parseServiceCapabilities refuses a name relay does not know, before the
// request reaches the tray, and drops repeats while keeping order.
func parseServiceCapabilities(names []string) ([]config.ServiceCapability, error) {
	out := []config.ServiceCapability{}
	for _, n := range names {
		c := config.ServiceCapability(strings.TrimSpace(n))
		if !slices.Contains(config.ServiceCapabilities, c) {
			return nil, fmt.Errorf("unknown capability %q: use frontend, manifest or projects", n)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

func capabilitiesColumn(caps []config.ServiceCapability) string {
	if len(caps) == 0 {
		return "none"
	}
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return strings.Join(names, ",")
}

func serviceUnregister(store config.SettingsStore, args []string) {
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
func serviceRestart(store config.SettingsStore, args []string) {
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

func serviceList(store config.SettingsStore) {
	s := store.Get()

	if len(s.Services) == 0 {
		fmt.Println("no services registered")
		return
	}

	// Best-effort and read-only: list keeps working with the tray stopped
	// (unlike register/unregister/restart, which require it), so a missing
	// or unreachable tray just means no STATE column detail beyond "-".
	statuses := fetchServiceSupervisionStatuses()

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tCOMMAND\tURL\tAUTOSTART\tCAPABILITIES\tSTATE")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", svc.ID, svc.DisplayName, cmd, urlStr, auto, capabilitiesColumn(svc.Capabilities), serviceStateColumn(svc.ID, statuses))
	}
	w.Flush()
}

// fetchServiceSupervisionStatuses probes the running tray for restart-
// supervision state (running/restarting/failed), which lives only in its
// memory. Returns nil -- never exits, never prints -- when relay is not
// running or the call fails, so `relay service list` keeps its documented
// property of working with the tray stopped.
func fetchServiceSupervisionStatuses() map[string]service.SupervisionStatus {
	if !serviceReachable() {
		return nil
	}
	raw, err := bridge.NewClient("").AdminOp("service.status", nil)
	if err != nil {
		return nil
	}
	var statuses map[string]service.SupervisionStatus
	if err := json.Unmarshal(raw, &statuses); err != nil {
		return nil
	}
	return statuses
}

// serviceStateColumn renders one row's STATE cell. "-" covers both "not
// supervised" (never started this session, or the operator stopped it) and
// "the tray wasn't reachable to ask."
func serviceStateColumn(id string, statuses map[string]service.SupervisionStatus) string {
	st, ok := statuses[id]
	if !ok {
		return "-"
	}
	switch st.Phase {
	case service.SupervisionRestarting:
		wait := time.Until(st.NextAttempt).Round(time.Second)
		if wait < 0 {
			wait = 0
		}
		return fmt.Sprintf("restarting (attempt %d, next in %s)", st.Attempt, wait)
	case service.SupervisionFailed:
		return fmt.Sprintf("failed (exit %d)", st.LastExitCode)
	case service.SupervisionRunning:
		return "running"
	default:
		return "-"
	}
}
