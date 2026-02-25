package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"relaygo/bridge"
)

func runServiceCommand(args []string) {
	if len(args) == 0 {
		serviceUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "register":
		serviceRegister(args[1:])
	case "unregister":
		serviceUnregister(args[1:])
	case "list":
		serviceList()
	default:
		fmt.Fprintf(os.Stderr, "unknown service command: %s\n", args[0])
		serviceUsage()
		os.Exit(1)
	}
}

func serviceUsage() {
	fmt.Fprintf(os.Stderr, "Usage: relay service <command>\n\nCommands:\n  register     Register or update a service\n  unregister   Remove a service\n  list         List registered services\n")
}

func serviceRegister(args []string) {
	var (
		name      string
		command   string
		id        string
		workdir   string
		url       string
		autostart bool
		svcArgs   []string
		envPairs  []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "--command":
			i++
			if i < len(args) {
				command = args[i]
			}
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--args":
			i++
			if i < len(args) {
				svcArgs = append(svcArgs, args[i])
			}
		case "--workdir":
			i++
			if i < len(args) {
				workdir = args[i]
			}
		case "--url":
			i++
			if i < len(args) {
				url = args[i]
			}
		case "--env":
			i++
			if i < len(args) {
				envPairs = append(envPairs, args[i])
			}
		case "--autostart":
			autostart = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if name == "" {
		fmt.Fprintf(os.Stderr, "error: --name is required\n")
		os.Exit(1)
	}
	if command == "" {
		fmt.Fprintf(os.Stderr, "error: --command is required\n")
		os.Exit(1)
	}

	if id == "" {
		id = slugify(name)
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "error: could not derive ID from name %q\n", name)
		os.Exit(1)
	}

	if workdir != "" {
		abs, err := filepath.Abs(workdir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not resolve workdir: %v\n", err)
			os.Exit(1)
		}
		workdir = abs
	}

	var env map[string]string
	if len(envPairs) > 0 {
		env = make(map[string]string, len(envPairs))
		for _, pair := range envPairs {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				fmt.Fprintf(os.Stderr, "error: invalid --env format %q (expected KEY=VALUE)\n", pair)
				os.Exit(1)
			}
			env[k] = v
		}
	}

	config := ServiceConfig{
		ID:          id,
		DisplayName: name,
		Command:     command,
		Args:        svcArgs,
		Env:         env,
		WorkingDir:  workdir,
		Autostart:   autostart,
		URL:         url,
	}

	s := LoadSettings()

	// Check for existing service with same ID (idempotent update).
	for _, svc := range s.Services {
		if svc.ID == id {
			s.UpdateService(config)
			fmt.Printf("updated service %q (%s)\n", name, id)
			// Tell the tray app to reload (restarts if running).
			if err := bridge.SendReloadService(id); err != nil {
				fmt.Fprintf(os.Stderr, "note: could not notify tray app: %v\n", err)
			}
			return
		}
	}

	s.AddService(config)
	fmt.Printf("registered service %q (%s)\n", name, id)
}

func serviceUnregister(args []string) {
	var id, name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	if id == "" && name == "" {
		fmt.Fprintf(os.Stderr, "error: --id or --name is required\n")
		os.Exit(1)
	}

	s := LoadSettings()

	// Resolve name to ID if needed.
	if id == "" {
		for _, svc := range s.Services {
			if svc.DisplayName == name {
				id = svc.ID
				break
			}
		}
		if id == "" {
			fmt.Fprintf(os.Stderr, "error: no service found with name %q\n", name)
			os.Exit(1)
		}
	}

	// Verify service exists.
	found := false
	for _, svc := range s.Services {
		if svc.ID == id {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "error: no service found with id %q\n", id)
		os.Exit(1)
	}

	s.RemoveService(id)
	fmt.Printf("unregistered service %q\n", id)
}

func serviceList() {
	s := LoadSettings()

	if len(s.Services) == 0 {
		fmt.Println("no services registered")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
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
