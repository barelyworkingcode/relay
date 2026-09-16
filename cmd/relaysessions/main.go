// Command relay-sessions is the session-hosting binary plan-broker-and-
// sessions.md's contracts C5 and C6 describe: relay launches one instance of
// it (mode "service"), which in turn spawns one `relay-sessions exec` shim
// (mode "exec") per pty session and one direct claude/pi/chat process per
// provider session, and Claude Code's PreToolUse hook runs
// `relay-sessions hook` (mode "hook") once per tool call.
//
// "service" mode wires hostapi.Server as a thin dispatcher over a real
// terminal.Manager and session.Manager — see internal/sessions/hostapi's
// package doc for why the dispatcher itself no longer spawns anything.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/sessions/hook"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/migrate"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/shim"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode, rest := os.Args[1], os.Args[2:]
	switch mode {
	case "exec":
		os.Exit(shim.Run(rest))
	case "hook":
		os.Exit(hook.Run(hook.Deps{}))
	case "service":
		os.Exit(runService(rest))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relay-sessions <service|exec|hook> [flags]")
}

// runService performs this host's own launch Hello (the existing,
// unmodified bridge.SendHello — a plain "service" kind Hello, not a
// project_session one), runs the one-time relayLLM data migration, builds
// the real terminal.Manager/session.Manager, and serves C5's internal API
// and C6's hook socket on top of them.
func runService(args []string) int {
	cfg, err := parseServiceArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-sessions service: %v\n", err)
		return 2
	}

	bridgeSock := cfg.bridgeSocket
	if bridgeSock == "" {
		bridgeSock = os.Getenv(bridge.EnvBridgeSocket)
	}

	relayPID := cfg.relayPIDOverride
	secret, launched, err := bridge.ReadLaunchSecret()
	if err != nil {
		log.Fatalf("relay-sessions: launch fd: %v", err)
	}
	if launched {
		if bridgeSock == "" {
			log.Fatal("relay-sessions: launched by relay with no RELAY_BRIDGE_SOCKET")
		}
		hello, err := bridge.SendHello(bridgeSock, cfg.serviceName, secret)
		if err != nil {
			log.Fatalf("relay-sessions: hello: %v", err)
		}
		relayPID = hello.RelayPID
		log.Printf("relay-sessions: identity bound (relay pid %d)", relayPID)
	} else if relayPID == 0 {
		log.Println("relay-sessions: not launched by relay and no -relay-pid override; the internal API will refuse every caller")
	}

	shimBinary := cfg.shimBinary
	if shimBinary == "" {
		self, err := os.Executable()
		if err != nil {
			log.Fatalf("relay-sessions: resolve own executable path: %v", err)
		}
		shimBinary = self
	}

	if err := migrate.Run(cfg.relayLLMDataDir(), cfg.dataDir); err != nil {
		// Best-effort, not fatal: a migration failure leaves relayLLM's old
		// data where it was (copyTree never deletes a source), so the worst
		// outcome is a fresh host that can't see pre-existing sessions this
		// run, not data loss. Fatal-ing here would turn "relayLLM's old
		// sessions dir has a permissions problem" into "the host never
		// starts at all".
		slog.Warn("relay-sessions: relayLLM data migration failed", "error", err)
	}

	terminals := terminal.NewManager(terminal.Config{
		ShimBinary:   shimBinary,
		LogDir:       filepath.Join(cfg.dataDir, "terminal_logs"),
		BridgeSocket: bridgeSock,
		ModelSocket:  cfg.modelSocket,
	})
	store := session.NewStore(filepath.Join(cfg.dataDir, "sessions"))
	sessions := session.NewManager(session.Config{
		Claude: provider.ClaudeConfig{
			HookSocket:      cfg.hookSocket,
			HookCommandPath: shimBinary,
			BridgeSocket:    bridgeSock,
			ModelSocket:     cfg.modelSocket,
		},
		Pi: provider.PiConfig{
			DataDir:      cfg.dataDir,
			BridgeSocket: bridgeSock,
			ModelSocket:  cfg.modelSocket,
		},
		Chat: provider.ChatConfig{
			ModelSocket:  cfg.modelSocket,
			ShimBinary:   shimBinary,
			BridgeSocket: bridgeSock,
		},
	}, store, permission.NewPermissionManager())

	// F1 (security review): the internal bearer must never be an argv
	// value — any same-uid process, including a sandboxed session target,
	// can read another process's argv via ps(1). It is generated here, in
	// memory, and handed to hostapi.Config directly; it never touches a
	// flag, an environment variable, or a log line. Handing this bearer to
	// relay belongs in a RegisterManifest call's InternalToken field (the
	// same mechanism cmd/testservice's own sendRegisterManifest uses) —
	// deferred here because bridge.Manifest.Validate rejects an empty
	// Routes list, and this unit has no real manifest route to declare yet
	// (only /launch and /terminate, which relay's manifest validation
	// refuses to accept as routes at all, C5). Whichever unit adds
	// relay-sessions' first real route (R-S9/R-S4b) should thread this
	// generated bearer through as InternalToken instead of ever adding it
	// back as a flag.
	internalBearer, err := generateBearer()
	if err != nil {
		log.Fatalf("relay-sessions: generate internal bearer: %v", err)
	}

	srv := hostapi.New(hostapi.Config{
		InternalSocket: cfg.internalSocket,
		InternalBearer: internalBearer,
		RelayPID:       relayPID,
		HookSocket:     cfg.hookSocket,
	}, terminals, sessions)
	srv.SetExitHandler(reportSessionExited)
	if err := srv.ListenInternal(); err != nil {
		log.Fatalf("relay-sessions: %v", err)
	}
	if err := srv.ListenHook(); err != nil {
		log.Fatalf("relay-sessions: %v", err)
	}
	go func() {
		if err := srv.ServeInternal(); err != nil {
			log.Printf("relay-sessions: internal API server: %v", err)
		}
	}()
	go func() {
		if err := srv.ServeHook(); err != nil {
			log.Printf("relay-sessions: hook server: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	srv.Close()
	return 0
}

// reportSessionExited is hostapi.Server's exit hook: send C5's advisory,
// tokenless SessionExited report over relay's bridge (bridge.Client's own
// doc comment on NewClient("") — a tokenless caller relies entirely on C3
// membership over the connection's own peer credentials). A fresh Client
// per call, not a shared one: bridge.Client is a thin, stateless value
// (sockPath + token) built for exactly this one-shot-call usage everywhere
// else in this codebase calls it.
func reportSessionExited(id string, rootPID, exitCode int, reason string) {
	err := bridge.NewClient("").SessionExited(bridge.SessionExitedRequest{
		SessionID:  id,
		RootPID:    rootPID,
		ExitStatus: exitCode,
		Reason:     reason,
	})
	if err != nil {
		slog.Warn("relay-sessions: SessionExited report failed", "session", id, "error", err)
	}
}

type serviceConfig struct {
	internalSocket   string
	hookSocket       string
	bridgeSocket     string
	shimBinary       string
	serviceName      string
	relayPIDOverride int
	dataDir          string
	modelSocket      string
}

// relayLLMDataDir is relayLLM's own data directory, the migration's copy
// source (internal/sessions/migrate's doc comment) — resolved the same way
// relayLLM/internal/app/app.go resolves its own default *dataDir, since
// that is the layout on disk this is migrating from. relayLLM is a separate
// module, not importable from here, so this is a small, deliberate
// re-derivation of that one path, not a port of its config loading.
func (c serviceConfig) relayLLMDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, "relayLLM")
}

// defaultDataDir is C5's own named host data dir,
// "~/Library/Application Support/relay/sessions" — bridge.ConfigDir()'s
// value plus the one "sessions" segment C5 names, not a second
// path-construction scheme: relay-sessions' two sockets already live
// directly in bridge.ConfigDir() (internal/service/builtin_sessions.go),
// so this reuses that same resolution rather than inventing another.
func defaultDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, "relay", "sessions")
}

func parseServiceArgs(args []string) (serviceConfig, error) {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	internalSocket := fs.String("internal-socket", "", "Unix socket path relay dials for /launch and /terminate")
	hookSocket := fs.String("hook-socket", "", "Unix socket path `relay-sessions hook` dials for /permission")
	bridgeSocket := fs.String("bridge-socket", "", "override RELAY_BRIDGE_SOCKET for this process's own Hello")
	shimBinary := fs.String("shim-binary", "", "override the exec-mode binary path (default: this binary's own path)")
	serviceName := fs.String("service-name", "relaysessions", "launch name for this process's own Hello")
	relayPID := fs.Int("relay-pid", 0, "dev/test only: relay's pid, when this process was not itself launched by relay")
	dataDir := fs.String("data-dir", "", "override this host's own data dir (default: ~/Library/Application Support/relay/sessions)")
	modelSocket := fs.String("model-socket", "", "override RELAY_MODEL_SOCKET for every spawned session (default: env, then relay's own model.sock)")
	if err := fs.Parse(args); err != nil {
		return serviceConfig{}, err
	}
	if *internalSocket == "" || *hookSocket == "" {
		return serviceConfig{}, fmt.Errorf("-internal-socket and -hook-socket are required")
	}
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	model := *modelSocket
	if model == "" {
		model = os.Getenv("RELAY_MODEL_SOCKET")
	}
	if model == "" {
		model = bridge.ModelSocketPath()
	}
	return serviceConfig{
		internalSocket:   *internalSocket,
		hookSocket:       *hookSocket,
		bridgeSocket:     *bridgeSocket,
		shimBinary:       *shimBinary,
		serviceName:      *serviceName,
		relayPIDOverride: *relayPID,
		dataDir:          dir,
		modelSocket:      model,
	}, nil
}

// generateBearer mints a fresh 64-lowercase-hex bearer, matching this
// codebase's launch-secret format (internal/bridge's isLaunchSecret shape)
// even though nothing currently validates it against that exact grammar —
// consistency with the rest of the system's secrets, not a requirement.
func generateBearer() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
