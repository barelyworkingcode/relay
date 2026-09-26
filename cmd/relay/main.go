package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

// buildVersion is set by build.sh via -ldflags "-X main.buildVersion=...";
// a plain `go build` (a developer checkout, or this repo's own tests) keeps
// "dev" rather than failing or lying about a release it isn't.
var buildVersion = "dev"

// HelperCDHash and HelperTeam are set by build.sh via -ldflags -X. build.sh
// must build and sign Contents/Helpers/relay-sessions and read its CDHash
// before it builds relay, since relay's own binary embeds that hash to
// verify the helper at launch. HelperTeam is the
// Developer ID team OU string; empty on an ad-hoc build, where SP3's
// ad-hoc note applies (gate on cdhash alone, skip the team requirement
// entirely -- it cannot be satisfied without a certificate). A plain
// `go build` (this repo's own tests, a developer checkout) leaves
// HelperCDHash empty, which is why runTrayApp treats an empty value as
// "no helper verifier available" rather than trying to construct one.
var (
	HelperCDHash = ""
	HelperTeam   = ""
)

func main() {
	logLevel := slog.LevelInfo
	if env := os.Getenv("RELAY_LOG_LEVEL"); env != "" {
		if err := logLevel.UnmarshalText([]byte(env)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid RELAY_LOG_LEVEL %q, using info\n", env)
		}
	}

	args := os.Args[1:]
	args, configDirExplicit := applyConfigDirFlag(args)

	// LaunchServices sends a GUI app's stderr to /dev/null, which would
	// otherwise lose every slog line; tee to <config-dir>/logs/relay.log too.
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
		runMcpOrServer(args[1:], configDirExplicit)
	case "mcpExec":
		runMcpExec(args[1:])
	case "audit":
		runAuditCommand(args[1:])
	case "enrol":
		runEnrolCommand(args[1:])
	case "credential":
		runCredentialCommand(args[1:])
	case "login":
		runLoginCommand(args[1:])
	case "eve":
		runEveCommand(args[1:])
	case "grant":
		runGrantCommand(args[1:])
	case "sandbox":
		runSandboxCommand(args[1:])
	case "mcpList":
		exitError("mcpList has been removed. Use: relay mcpExec --token <TOKEN> --list")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: relay [--config-dir DIR] [service|mcp|mcpExec|audit|enrol|credential|login|eve|grant|sandbox]\n", args[0])
		os.Exit(1)
	}
}

// applyConfigDirFlag must run before any subcommand's flag.Parse: each
// subcommand owns its own flag.FlagSet, so this is the only chance to apply
// --config-dir before anything reads ConfigDir. explicit is false for an
// empty value, which leaves ConfigDir at its default.
func applyConfigDirFlag(args []string) (rest []string, explicit bool) {
	if len(args) == 0 {
		return args, false
	}
	const eqPrefix = "--config-dir="
	switch {
	case args[0] == "--config-dir":
		if len(args) < 2 {
			exitError("--config-dir requires a path argument")
		}
		bridge.SetConfigDir(args[1])
		return args[2:], args[1] != ""
	case strings.HasPrefix(args[0], eqPrefix):
		dir := args[0][len(eqPrefix):]
		bridge.SetConfigDir(dir)
		return args[1:], dir != ""
	}
	return args, false
}

const (
	mcpSocketSourceConfigDir = "--config-dir"
	mcpSocketSourceEnv       = bridge.EnvBridgeSocket
	mcpSocketSourceDefault   = "default"

	mcpBridgeProbeTimeout = 2 * time.Second
)

// resolveMcpBridgeSocket picks the socket the stdio server dials. An
// explicit --config-dir outranks RELAY_BRIDGE_SOCKET, which outranks the
// default; see docs/cli.md. A set env var is never second-guessed by a
// fallback to the default socket.
func resolveMcpBridgeSocket(configDirExplicit bool, getenv func(string) string) (path, source string, err error) {
	if configDirExplicit {
		return bridge.SocketPath(), mcpSocketSourceConfigDir, nil
	}
	if env := getenv(bridge.EnvBridgeSocket); env != "" {
		if !filepath.IsAbs(env) {
			return "", "", fmt.Errorf("%s must be an absolute path, got %q", bridge.EnvBridgeSocket, env)
		}
		return env, mcpSocketSourceEnv, nil
	}
	return bridge.SocketPath(), mcpSocketSourceDefault, nil
}

func probeBridgeSocket(path string, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runMcpOrServer's subcommands keep bridge.NewClient: they carry secrets the
// operator typed, so they must not follow an inherited RELAY_BRIDGE_SOCKET.
func runMcpOrServer(args []string, configDirExplicit bool) {
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

	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	token := fs.String("token", "", "auth token")
	fs.Parse(args)

	sockPath, source, err := resolveMcpBridgeSocket(configDirExplicit, os.Getenv)
	if err != nil {
		exitError("relay mcp: %v", err)
	}
	// Probed before stdin is read so a wrong or dead socket fails the launch
	// visibly instead of surfacing as a per-call error inside the client.
	if err := probeBridgeSocket(sockPath, mcpBridgeProbeTimeout); err != nil {
		exitError("relay mcp: bridge socket %s (from %s) is unreachable: %v", sockPath, source, err)
	}

	*token = resolveMcpToken(*token)
	// An empty token is not fatal: a tokenless caller may still be a C3
	// member of a live project_session (plan-broker-and-sessions.md §2 C3),
	// which the bridge resolves on its own. Failing here would deny that
	// path before relay can decide.
	if err := mcp.RunMCPServerAt(sockPath, *token); err != nil {
		exitError("mcp server error: %v", err)
	}
}
