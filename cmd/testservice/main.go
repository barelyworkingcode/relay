// Command testservice is a real spawnable binary (not a mock) that the
// hermetic test suite uses to exercise relay's real service spawn path —
// env-var injection, the launch fd and Hello, pidfile, log file, reaper —
// without mocking exec.Command.
//
// Built on demand by TestMain in service_registry_test.go.
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
	"strings"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func main() {
	register := flag.Bool("register", false, "send RegisterManifest before serving")
	statusAfter := flag.Duration("status-after", 0, "exit after this duration (0 = block on signal)")
	dumpEnv := flag.String("dump-env", "", "write os.Environ() (one VAR=value per line) to this file, then continue")
	flag.Parse()

	// Written before any work so a test can poll for the file regardless of
	// --register/--status-after.
	if *dumpEnv != "" {
		if err := os.WriteFile(*dumpEnv, []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
			log.Fatalf("dump-env: %v", err)
		}
	}

	serviceID := os.Getenv(bridge.EnvServiceID)
	bridgeSock := os.Getenv(bridge.EnvBridgeSocket)

	if serviceID == "" {
		log.Fatal("testservice: RELAY_SERVICE_ID not set")
	}
	log.Printf("testservice %s starting (bridge=%s)", serviceID, bridgeSock)

	// Before anything else that could spawn: a relay launch that cannot
	// complete its Hello exits non-zero rather than running without an
	// identity relay believes it has.
	secret, launched, err := bridge.ReadLaunchSecret()
	if err != nil {
		log.Fatalf("testservice: launch fd: %v", err)
	}
	if launched {
		if bridgeSock == "" {
			log.Fatal("testservice: launched by relay with no RELAY_BRIDGE_SOCKET")
		}
		hello, err := bridge.SendHello(bridgeSock, serviceID, secret)
		if err != nil {
			log.Fatalf("testservice: hello: %v", err)
		}
		log.Printf("testservice %s identity bound (relay pid %d)", hello.ServiceID, hello.RelayPID)
	}

	// Per-pid tempdir so concurrent test runs don't collide.
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
	defer func() { _ = ln.Close() }()
	defer func() { _ = os.RemoveAll(internalDir) }()

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
		if !launched {
			log.Fatal("testservice: --register requires a relay launch identity")
		}
		// Dial bridgeSock directly rather than going through bridge.Client:
		// bridge.SocketPath() derives from ConfigDir, but our parent might be
		// running a non-default one, so it wouldn't find the same socket.
		if err := sendRegisterManifest(bridgeSock, bridge.RegisterManifestRequest{
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

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	log.Printf("testservice %s caught signal, exiting", serviceID)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// sendRegisterManifest sends no token: relay authenticates the request by
// this process's audit token, bound at Hello.
func sendRegisterManifest(sockPath string, req bridge.RegisterManifestRequest) error {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	args, err := json.Marshal(req)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(bridge.BridgeRequest{
		Type:      bridge.ReqRegisterManifest,
		Arguments: args,
	})
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	sc := bridge.NewScanner(conn)
	if !sc.Scan() {
		return fmt.Errorf("read: %w", sc.Err())
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
