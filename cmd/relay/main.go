package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/logging"
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

// relayServiceID is the service id on every line the tray app and CLI write.
const relayServiceID = "relay"

func main() {
	// Deliberate: an inherited value must not reach a child; only a spawn that
	// has a request's trace ID sets it.
	os.Unsetenv(logging.EnvTraceID)

	args, configDir, source, err := selectConfigDir(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	bridge.SetConfigDir(configDir)
	args, traceID, err := selectTraceFlag(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	bridge.SetClientTraceID(traceID)

	// Only the tray and the server create the config dir. A client names an
	// instance, never makes one, so its log goes to the dir only when the dir
	// already exists.
	if len(args) > 0 && args[0] == "serve" && len(args) > 1 {
		fmt.Fprintf(os.Stderr, "relay serve: unexpected argument %q\n%s\n", args[1], serveUsage)
		os.Exit(2)
	}
	if len(args) == 0 || args[0] == "serve" {
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create config dir %s: %v\n", configDir, err)
			os.Exit(1)
		}
	}
	// LaunchServices sends a GUI app's stderr to /dev/null, which would
	// otherwise lose every slog line; tee to <config-dir>/logs/relay.log too.
	logOut := io.Writer(os.Stderr)
	logDir, haveLogDir := existingServiceLogDir()
	if len(args) == 0 || args[0] == "serve" {
		var err error
		logDir, err = serviceLogDir()
		haveLogDir = err == nil
	}
	// Deliberate: reading the log must not create or rotate it.
	if haveLogDir && (len(args) == 0 || args[0] != "logs") {
		if rw, err := openRotatingLog(filepath.Join(logDir, "relay.log")); err == nil {
			logOut = io.MultiWriter(os.Stderr, rw)
		}
	}
	logging.Install(logOut, logging.Options{DefaultService: relayServiceID})

	if len(args) == 0 {
		runTrayApp()
		return
	}

	switch args[0] {
	case "serve":
		runServeCommand(args[1:])
	case "service":
		runServiceCommand(args[1:])
	case "mcp":
		runMcpOrServer(args[1:], source)
	case "mcpExec":
		runMcpExec("relay mcpExec", args[1:])
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
	case "project":
		runProjectCommand(args[1:])
	case "sandbox":
		runSandboxCommand(args[1:])
	case "drop-in":
		runDropInCommand(args[1:])
	case "logs":
		runLogsCommand(args[1:], traceID)
	case "mcpList":
		exitError("mcpList has been removed. Use: relay mcpExec --token <TOKEN> --list")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: relay [--config-dir DIR] [--trace ID] [serve|service|mcp|mcpExec|audit|enrol|credential|login|eve|grant|project|sandbox|drop-in|logs]\n", args[0])
		os.Exit(1)
	}
}

const mcpBridgeProbeTimeout = 2 * time.Second

// resolveMcpBridgeSocket picks the socket the stdio server dials, and the
// label of the rule that chose it: --config-dir, then RELAY_BRIDGE_SOCKET,
// then RELAY_CONFIG_DIR, then the default; see docs/cli.md. A set env var is
// never second-guessed by a fallback to the default socket.
func resolveMcpBridgeSocket(source configDirSource, getenv func(string) string) (path, label string, err error) {
	if source == configDirFromFlag {
		return bridge.SocketPath(), string(configDirFromFlag), nil
	}
	if env := getenv(bridge.EnvBridgeSocket); env != "" {
		if !filepath.IsAbs(env) {
			return "", "", fmt.Errorf("%s must be an absolute path, got %q", bridge.EnvBridgeSocket, env)
		}
		return env, bridge.EnvBridgeSocket, nil
	}
	return bridge.SocketPath(), string(source), nil
}

func probeBridgeSocket(path string, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runMcpOrServer's subcommands keep bridge.NewClient and ignore
// RELAY_BRIDGE_SOCKET: only the stdio server is spawned by relay's own
// children with that variable naming the relay that launched them.
func runMcpOrServer(args []string, source configDirSource) {
	if len(args) > 0 {
		switch args[0] {
		case "register", "unregister", "list":
			runMcpCommand(args)
			return
		case "call":
			runMcpExec("relay mcp call", args[1:])
			return
		}
	}

	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	token := fs.String("token", "", "auth token")
	fs.Parse(args)

	sockPath, label, err := resolveMcpBridgeSocket(source, os.Getenv)
	if err != nil {
		exitError("relay mcp: %v", err)
	}
	// Probed before stdin is read so a wrong or dead socket fails the launch
	// visibly instead of surfacing as a per-call error inside the client.
	if err := probeBridgeSocket(sockPath, mcpBridgeProbeTimeout); err != nil {
		exitError("relay mcp: bridge socket %s (from %s) is unreachable: %v", sockPath, label, err)
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
