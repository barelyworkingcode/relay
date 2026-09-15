// Command relay-sessions is the session-hosting binary plan-broker-and-
// sessions.md's contracts C5 and C6 describe: relay launches one instance of
// it (mode "service"), which in turn spawns one `relay-sessions exec` shim
// (mode "exec") per terminal or provider session, and Claude Code's
// PreToolUse hook runs `relay-sessions hook` (mode "hook") once per tool
// call.
//
// This unit (R-S5, "sh/host-skeleton") builds all three modes' skeleton: a
// service that can accept the internal API connection and hold a session
// table, a fully working exec shim, and a fully working hook client. Real
// terminal, Claude and pi hosting land in later units on top of this.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/sessions/hook"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/shim"
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

// runService is the "service" mode's skeleton: it performs its own launch
// Hello (the existing, unmodified bridge.SendHello — this is a plain
// "service" kind Hello, not a project_session one, so it needs none of the
// "kind" wire addition R-S1 is adding this wave), then serves C5's internal
// API and C6's hook socket. No real terminal/session/provider hosting sits
// behind /launch yet; see internal/sessions/hostapi's package doc.
func runService(args []string) int {
	cfg, err := parseServiceArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-sessions service: %v\n", err)
		return 2
	}

	relayPID := cfg.relayPIDOverride
	secret, launched, err := bridge.ReadLaunchSecret()
	if err != nil {
		log.Fatalf("relay-sessions: launch fd: %v", err)
	}
	if launched {
		bridgeSock := cfg.bridgeSocket
		if bridgeSock == "" {
			bridgeSock = os.Getenv(bridge.EnvBridgeSocket)
		}
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
		ShimBinary:     shimBinary,
	})
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

type serviceConfig struct {
	internalSocket   string
	hookSocket       string
	bridgeSocket     string
	shimBinary       string
	serviceName      string
	relayPIDOverride int
}

func parseServiceArgs(args []string) (serviceConfig, error) {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	internalSocket := fs.String("internal-socket", "", "Unix socket path relay dials for /launch and /terminate")
	hookSocket := fs.String("hook-socket", "", "Unix socket path `relay-sessions hook` dials for /permission")
	bridgeSocket := fs.String("bridge-socket", "", "override RELAY_BRIDGE_SOCKET for this process's own Hello")
	shimBinary := fs.String("shim-binary", "", "override the exec-mode binary path (default: this binary's own path)")
	serviceName := fs.String("service-name", "relaysessions", "launch name for this process's own Hello")
	relayPID := fs.Int("relay-pid", 0, "dev/test only: relay's pid, when this process was not itself launched by relay")
	if err := fs.Parse(args); err != nil {
		return serviceConfig{}, err
	}
	if *internalSocket == "" || *hookSocket == "" {
		return serviceConfig{}, fmt.Errorf("-internal-socket and -hook-socket are required")
	}
	return serviceConfig{
		internalSocket:   *internalSocket,
		hookSocket:       *hookSocket,
		bridgeSocket:     *bridgeSocket,
		shimBinary:       *shimBinary,
		serviceName:      *serviceName,
		relayPIDOverride: *relayPID,
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
