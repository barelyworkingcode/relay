//go:build darwin

package main

// The hermetic tests beside this one declare a process tree; these spawn
// one. Every fact here comes from the kernel: the peer pid and pidversion
// relay reads off the accepted connection, the ppid and start-time chain
// proc_pidinfo reports for real processes, and a real kqueue exit watch on
// the session's root. Nothing is asserted from a caller's own account of
// itself, and no part of membership.Resolve is stubbed.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/service"
)

const (
	bridgeCallerEnv       = "GO_WANT_BRIDGE_CALLER"
	bridgeCallerSockEnv   = "BRIDGE_CALLER_SOCK"
	bridgeCallerOutEnv    = "BRIDGE_CALLER_OUT"
	bridgeCallerModeEnv   = "BRIDGE_CALLER_MODE"
	bridgeCallerDetachEnv = "BRIDGE_CALLER_WAIT_REPARENT"
)

// TestHelperBridgeCaller is not a real test — the env guard is what keeps an
// ordinary `go test ./...` from doing anything here, following
// cli_subprocess_test.go's established pattern. In the child it is a real
// process making a real tokenless bridge call, which is the only honest way
// to exercise an ancestry check: relay must be answering about a pid it read
// from the kernel, not one a test handed it.
func TestHelperBridgeCaller(t *testing.T) {
	if os.Getenv(bridgeCallerEnv) != "1" {
		return
	}
	switch os.Getenv(bridgeCallerModeEnv) {
	case "child":
		// One more generation, so the caller is a GRANDchild of the session
		// root and the walk has to climb twice.
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperBridgeCaller")
		cmd.Env = append(os.Environ(), bridgeCallerModeEnv+"=direct")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "doublefork":
		// Detach: the grandchild is setsid'd and this process exits at once,
		// so the kernel reparents it to launchd and the chain back to the
		// session root is severed (SP5's finding, and SH §4.3's "fail
		// closed" row).
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperBridgeCaller")
		cmd.Env = append(os.Environ(), bridgeCallerModeEnv+"=direct", bridgeCallerDetachEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	if os.Getenv(bridgeCallerDetachEnv) == "1" {
		deadline := time.Now().Add(10 * time.Second)
		for os.Getppid() != 1 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}

	line, err := bridgeCallerRequest(os.Getenv(bridgeCallerSockEnv))
	if err != nil {
		line = `{"type":"error","message":"helper: ` + err.Error() + `"}`
	}
	if err := os.WriteFile(os.Getenv(bridgeCallerOutEnv), []byte(line), 0o600); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// bridgeCallerRequest sends one tokenless ListTools — with a Cwd set, which
// must buy it nothing either way — and returns the raw response line.
func bridgeCallerRequest(sock string) (string, error) {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("unix", sock)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	cwd, _ := os.Getwd()
	payload, err := json.Marshal(bridge.BridgeRequest{Type: bridge.ReqListTools, Cwd: cwd})
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return "", err
	}
	sc := bridge.NewScanner(conn)
	if !sc.Scan() {
		return "", errors.New("no response")
	}
	return sc.Text(), nil
}

// sessionBridge is a real bridge server on a real socket, with one bound
// project_session whose root process is the test binary itself — so every
// process the test spawns is a genuine descendant of a genuine session root.
type sessionBridge struct {
	router   *appRouter
	launches *service.Launches
	sock     string
}

func startSessionBridge(t *testing.T) *sessionBridge {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{
			ID: "p-real", Name: "real", Path: t.TempDir(),
			AllowedMcpIDs: []string{"*"},
			Token:         config.NewSecret(testToken),
			TokenHash:     config.HashToken(testToken),
		})
	}), "seed project")

	launches := service.NewLaunches()
	router := &appRouter{
		store:    store,
		tools:    mcpbroker.NewManager(nil),
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
		launches: launches,
		onChange: func() {},
		// The real Source and the real launch table; only the host-pid stop
		// is moved aside. It has to be: relay's own pid IS this test binary,
		// and the same process cannot be both the walk's stop condition and
		// the session root whose descendants must reach it. R-M0's darwin
		// tests park it out of the way for the same reason.
		membership: &membershipAuth{launches: launches, src: membership.NewSource(), hostPID: 1 << 30},
	}
	srv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	b := &sessionBridge{router: router, launches: launches, sock: bridge.SocketPath()}
	_ = dialUnixWithTimeout(t, b.sock, 2*time.Second).Close()
	return b
}

// bindSelfAsSessionRoot makes THIS process a live session's root, through
// the real Bind path: relay reads its start time with proc_pidinfo and
// registers a real kqueue NOTE_EXIT watch on it.
func (b *sessionBridge) bindSelfAsSessionRoot(t *testing.T, sessionID, projectID string) *service.Launch {
	t.Helper()
	secret, launch, err := b.launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: sessionID, ProjectID: projectID,
		ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin")
	id, err := b.launches.Bind(sessionID, secret, selfPeerToken(t))
	assertNoErr(t, err, "Bind")
	if int(id.Process.PID) != os.Getpid() || id.RootStartSec == 0 {
		t.Fatalf("bound identity %+v does not describe this process as the kernel sees it", id)
	}
	return launch
}

// callFrom runs the helper in mode and returns relay's raw response line.
func (b *sessionBridge) callFrom(t *testing.T, mode string) bridge.BridgeResponse {
	t.Helper()
	out := filepath.Join(t.TempDir(), "response.json")
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperBridgeCaller")
	cmd.Env = append(os.Environ(),
		bridgeCallerEnv+"=1",
		bridgeCallerSockEnv+"="+b.sock,
		bridgeCallerOutEnv+"="+out,
		bridgeCallerModeEnv+"="+mode,
	)
	cmd.Stderr = os.Stderr
	assertNoErr(t, cmd.Start(), "start helper (%s)", mode)
	t.Cleanup(func() { _ = cmd.Wait() })

	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(out)
		if err == nil && len(raw) > 0 {
			var resp bridge.BridgeResponse
			assertNoErr(t, json.Unmarshal(raw, &resp), "decode helper response %q", raw)
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper (%s) never wrote a response", mode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A real child and a real grandchild of a live session's root authenticate
// with no token, no secret and nothing in their environment — by being
// where the kernel says they are.
func TestDarwinMembership_RealDescendantsAreMembers(t *testing.T) {
	b := startSessionBridge(t)
	b.bindSelfAsSessionRoot(t, "sess-real", "p-real")

	for _, mode := range []string{"direct", "child"} {
		resp := b.callFrom(t, mode)
		if resp.Type != bridge.RespTools {
			t.Fatalf("%s of the session root: got %+v, want a tools response", mode, resp)
		}
	}
}

// SP5's finding, end to end: a process that detached itself (double fork
// plus setsid) is reparented to launchd, so no walk from it ever reaches the
// session root. It is refused even though it was started from inside the
// session and its own working directory is the project's.
func TestDarwinMembership_ADetachedProcessIsNotAMember(t *testing.T) {
	b := startSessionBridge(t)
	b.bindSelfAsSessionRoot(t, "sess-real", "p-real")

	resp := b.callFrom(t, "doublefork")
	if resp.Type != bridge.RespError || resp.Code != jsonrpc.CodeUnauthorized {
		t.Fatalf("detached process: got %+v, want an unauthorized error", resp)
	}
	if strings.Contains(strings.ToLower(resp.Message), "working directory") {
		t.Fatalf("the refusal reasoned about the caller's asserted cwd: %q", resp.Message)
	}
}

// With no session bound at all, the very same real child is refused —
// which is what proves the acceptance above came from the session binding
// and not from something ambient about being a subprocess of the test.
func TestDarwinMembership_NoSessionMeansNoMember(t *testing.T) {
	b := startSessionBridge(t)

	resp := b.callFrom(t, "direct")
	if resp.Type != bridge.RespError || resp.Code != jsonrpc.CodeUnauthorized {
		t.Fatalf("child with no session bound: got %+v, want an unauthorized error", resp)
	}
}

// A real kqueue NOTE_EXIT on the root ends the session; this asserts the
// other half — that while the root is alive the watch has NOT fired, and
// that ending the launch refuses the next real call. (The exit itself is
// R-M0's darwin coverage; what is new here is that relay's auth answer
// follows it.)
func TestDarwinMembership_EndingTheSessionRefusesTheNextRealCall(t *testing.T) {
	b := startSessionBridge(t)
	launch := b.bindSelfAsSessionRoot(t, "sess-real", "p-real")

	if resp := b.callFrom(t, "direct"); resp.Type != bridge.RespTools {
		t.Fatalf("while live: got %+v", resp)
	}
	launch.End()
	if resp := b.callFrom(t, "direct"); resp.Type != bridge.RespError || resp.Code != jsonrpc.CodeUnauthorized {
		t.Fatalf("after the session ended: got %+v, want an unauthorized error", resp)
	}
}
