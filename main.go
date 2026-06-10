package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"relaygo/bridge"
	"relaygo/mcp"
)

func main() {
	logLevel := slog.LevelInfo
	if env := os.Getenv("RELAY_LOG_LEVEL"); env != "" {
		if err := logLevel.UnmarshalText([]byte(env)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid RELAY_LOG_LEVEL %q, using info\n", env)
		}
	}

	// --config-dir is a global flag that may precede any subcommand. It
	// reroutes settings, pidfiles, logs, and the bridge socket to the given
	// directory — enables multi-instance use, the demo harness
	// (scripts/demo.sh), and as a graceful production surface for the same
	// override the test suite uses. See bridge.SetConfigDir.
	args := os.Args[1:]
	args = applyConfigDirFlag(args)

	// Tee logs to <config-dir>/logs/relay.log so the tray process's logs survive
	// a GUI launch: LaunchServices sends an app's stderr to /dev/null, which
	// otherwise loses every slog line. Managed services already log to the same
	// dir as <id>.log (see serviceLogDir). Set up after applyConfigDirFlag so it
	// honors --config-dir; falls back to stderr-only if the file can't be opened.
	logOut := io.Writer(os.Stderr)
	if logDir, err := serviceLogDir(); err == nil {
		if rw, err := openRotatingLog(filepath.Join(logDir, "relay.log")); err == nil {
			logOut = io.MultiWriter(os.Stderr, rw)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: logLevel})))

	if len(args) == 0 {
		runTrayApp()
		return
	}

	switch args[0] {
	case "service":
		runServiceCommand(args[1:])
	case "mcp":
		runMcpOrServer(args[1:])
	case "mcpExec":
		runMcpExec(args[1:])
	case "mcpList":
		exitError("mcpList has been removed. Use: relay mcpExec --token <TOKEN> --list")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: relay [--config-dir DIR] [service|mcp|mcpExec]\n", args[0])
		os.Exit(1)
	}
}

// applyConfigDirFlag consumes a leading --config-dir <path> (or --config-dir=<path>)
// argument if present, calls bridge.SetConfigDir, and returns the remaining
// args. Kept pre-flag.Parse because each subcommand uses its own flag.FlagSet
// and we need the override applied before any subcommand initializes anything
// that reads ConfigDir.
func applyConfigDirFlag(args []string) []string {
	if len(args) == 0 {
		return args
	}
	const eqPrefix = "--config-dir="
	switch {
	case args[0] == "--config-dir":
		if len(args) < 2 {
			exitError("--config-dir requires a path argument")
		}
		bridge.SetConfigDir(args[1])
		return args[2:]
	case strings.HasPrefix(args[0], eqPrefix):
		bridge.SetConfigDir(args[0][len(eqPrefix):])
		return args[1:]
	}
	return args
}

// runMcpOrServer dispatches to MCP management subcommands or runs the stdio
// MCP server, depending on the first argument.
func runMcpOrServer(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "register", "unregister", "list":
			runMcpCommand(args)
			return
		case "call":
			runMcpExec(args[1:])
			return
		}
	}

	// MCP stdio server mode.
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	token := fs.String("token", "", "auth token")
	fs.Parse(args)

	if *token == "" {
		*token = os.Getenv(bridge.EnvProjectToken)
	}
	if *token == "" {
		// Transition: accept the legacy env name from an un-migrated spawner.
		*token = os.Getenv(bridge.EnvProjectTokenLegacy)
	}
	if err := mcp.RunMCPServer(*token); err != nil {
		exitError("mcp server error: %v", err)
	}
}
