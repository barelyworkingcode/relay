// Command testservice is a minimal relay-enhanced service used by the
// hermetic test suite to exercise the real service spawn path
// (env-var injection, pidfile, log file, reaper) without mocking
// exec.Command.
//
// On start it reads RELAY_BRIDGE_SOCKET and RELAY_SERVICE_ID, optionally
// dials the bridge to register a manifest (if --register is set), then
// serves a stub HTTP listener on an internal Unix socket until SIGTERM.
//
// Built on demand by TestMain in service_registry_test.go.
//
// Usage (intended for tests only, but the flags are documented for
// debugging):
//
//	testservice                    # block on SIGTERM, no manifest registration
//	testservice --register         # register a stub manifest first
//	testservice --status-after Ns  # exit after N seconds (life-cycle tests)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"relaygo/bridge"
)

func main() {
	register := flag.Bool("register", false, "send RegisterManifest before serving")
	statusAfter := flag.Duration("status-after", 0, "exit after this duration (0 = block on signal)")
	flag.Parse()

	serviceID := os.Getenv(bridge.EnvServiceID)
	bridgeSock := os.Getenv(bridge.EnvBridgeSocket)
	mcpToken := os.Getenv(bridge.EnvServiceToken) // service-grade token, injected by service_registry
	if mcpToken == "" {
		mcpToken = os.Getenv(bridge.EnvServiceTokenLegacy) // transition fallback
	}

	if serviceID == "" {
		log.Fatal("testservice: RELAY_SERVICE_ID not set")
	}
	log.Printf("testservice %s starting (bridge=%s)", serviceID, bridgeSock)

	// Bind our own internal socket in a per-pid tempdir so concurrent test
	// runs don't collide.
	internalDir, err := os.MkdirTemp("", "testservice-")
	if err != nil {
		log.Fatalf("mkdtemp: %v", err)
	}
	internalSock := filepath.Join(internalDir, "internal.sock")
	internalToken := "testservice-internal-token-" + serviceID

	ln, err := net.Listen("unix", internalSock)
	if err != nil {
		log.Fatalf("listen %s: %v", internalSock, err)
	}
	defer ln.Close()
	defer os.RemoveAll(internalDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"service": serviceID,
			"path":    r.URL.Path,
		})
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": serviceID,
			"healthy": true,
			"uptime":  "test",
		})
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	if *register {
		if bridgeSock == "" {
			log.Fatal("testservice: --register requires RELAY_BRIDGE_SOCKET")
		}
		if mcpToken == "" {
			log.Fatal("testservice: --register requires RELAY_SERVICE_TOKEN")
		}
		// The shared bridge.Client uses bridge.SocketPath() (derived from
		// ConfigDir) but services receive RELAY_BRIDGE_SOCKET explicitly —
		// our parent might be running a non-default ConfigDir. Dial directly.
		if err := sendRegisterManifest(bridgeSock, mcpToken, bridge.RegisterManifestRequest{
			ServiceID: serviceID,
			Manifest: bridge.Manifest{
				Routes: []string{"/api/" + serviceID},
				Status: &bridge.StatusDecl{Path: "/api/status"},
			},
			InternalSocket: internalSock,
			InternalToken:  internalToken,
		}); err != nil {
			log.Fatalf("register manifest: %v", err)
		}
		log.Printf("testservice %s registered", serviceID)
	}

	if *statusAfter > 0 {
		time.Sleep(*statusAfter)
		log.Printf("testservice %s exiting after %s", serviceID, *statusAfter)
		return
	}

	// Block on SIGTERM / SIGINT.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	log.Printf("testservice %s caught signal, exiting", serviceID)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// sendRegisterManifest writes a RegisterManifest request directly to the
// bridge socket. Mirrors what bridge.Client.RegisterManifest does, but
// against an arbitrary socket path passed at runtime (via env var) instead
// of bridge.SocketPath().
func sendRegisterManifest(sockPath, token string, req bridge.RegisterManifestRequest) error {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	args, err := json.Marshal(req)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(bridge.BridgeRequest{
		Type:      bridge.ReqRegisterManifest,
		Arguments: args,
		Token:     token,
	})
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	sc := bridge.NewScanner(conn)
	if !sc.Scan() {
		return fmt.Errorf("read: %v", sc.Err())
	}
	var resp bridge.BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if resp.Type == bridge.RespError {
		return fmt.Errorf("bridge error %d: %s", resp.Code, resp.Message)
	}
	return nil
}
