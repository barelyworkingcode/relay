// Command fakeservice is a relay-enhanced service for end-to-end tests. It
// follows docs/launch-identity.md and docs/service-manifest.md: it reads the
// launch secret, says Hello, binds an internal socket behind a bearer,
// registers a manifest and serves the declared routes. Every inbound request
// and every bridge outcome is appended to the call log.
//
//	fakeservice --call-log FILE [--config FILE] [--claim-id ID] [--close-bridge-after-register]
//
// A refused Hello or RegisterManifest is logged and the process stays up, so a
// test reads the refusal from the call log rather than from a restart loop.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"relaye2e/fakes/calllog"
)

var secretRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Entry is one call-log line. Kind is "hello" or "register" for a bridge
// outcome (Outcome "ok" or "error", with Code and Message on an error) and
// "request" for an inbound HTTP request.
type Entry struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	Auth    string `json:"auth,omitempty"`
	Body    string `json:"body,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeservice:", err)
		os.Exit(1)
	}
}

func run() error {
	logPath := flag.String("call-log", "", "call log file (required)")
	configPath := flag.String("config", "", "declare this file as the editable config")
	claimID := flag.String("claim-id", "", "register the manifest under this service id")
	closeBridge := flag.Bool("close-bridge-after-register", false, "close the bridge connection after a successful RegisterManifest and keep running")
	flag.Parse()
	if *logPath == "" {
		return errors.New("--call-log is required")
	}
	sockPath := os.Getenv("RELAY_BRIDGE_SOCKET")
	id := os.Getenv("RELAY_SERVICE_ID")
	if sockPath == "" || id == "" {
		return errors.New("RELAY_BRIDGE_SOCKET and RELAY_SERVICE_ID must be set")
	}
	secret, err := readLaunchSecret()
	if err != nil {
		return err
	}
	log, err := calllog.Open(*logPath)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()

	if *configPath != "" {
		if _, err := os.Stat(*configPath); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(*configPath, []byte("{}\n"), 0o600); err != nil {
				return fmt.Errorf("seed config %q: %w", *configPath, err)
			}
		}
	}

	dir, err := os.MkdirTemp("", "fsvc")
	if err != nil {
		return fmt.Errorf("internal socket dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	internalSock := filepath.Join(dir, "s.sock")
	bearer, err := randomHex(32)
	if err != nil {
		return err
	}
	ln, err := net.Listen("unix", internalSock)
	if err != nil {
		return fmt.Errorf("listen %q: %w", internalSock, err)
	}
	if err := os.Chmod(internalSock, 0o600); err != nil {
		return fmt.Errorf("chmod internal socket: %w", err)
	}
	srv := &http.Server{Handler: handler(log, bearer), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// By default the connection stays open for the process lifetime.
	// --close-bridge-after-register closes it once the manifest is registered,
	// as a service that registers on a one-shot connection does.
	conn, err := net.DialTimeout("unix", sockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	if reply, err := exchange(conn, br, map[string]any{"type": "Hello", "name": id, "token": secret}); err != nil {
		return err
	} else {
		logOutcome(log, "hello", reply)
		if reply.Type == "OK" {
			regID := id
			if *claimID != "" {
				regID = *claimID
			}
			reg := map[string]any{
				"type": "RegisterManifest",
				"arguments": map[string]any{
					"serviceId":      regID,
					"manifest":       manifest(*configPath),
					"internalSocket": internalSock,
					"internalToken":  bearer,
				},
			}
			rr, err := exchange(conn, br, reg)
			if err != nil {
				return err
			}
			logOutcome(log, "register", rr)
			if *closeBridge && rr.Type == "OK" {
				_ = conn.Close()
			}
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return nil
}

// readLaunchSecret reads the secret from the descriptor named by
// RELAY_LAUNCH_FD to EOF and refuses anything but 64 lowercase hex characters.
func readLaunchSecret() (string, error) {
	var fd int
	if _, err := fmt.Sscanf(os.Getenv("RELAY_LAUNCH_FD"), "%d", &fd); err != nil || fd < 3 {
		return "", errors.New("RELAY_LAUNCH_FD is not a descriptor number")
	}
	f := os.NewFile(uintptr(fd), "launch-fd")
	if f == nil {
		return "", errors.New("RELAY_LAUNCH_FD is not open")
	}
	b, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("read launch fd: %w", err)
	}
	if !secretRE.Match(b) {
		return "", errors.New("launch secret is not 64 lowercase hex characters")
	}
	return string(b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

type reply struct {
	Type    string `json:"type"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func exchange(conn net.Conn, br *bufio.Reader, frame map[string]any) (reply, error) {
	line, err := json.Marshal(frame)
	if err != nil {
		return reply{}, fmt.Errorf("encode %v frame: %w", frame["type"], err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return reply{}, fmt.Errorf("write %v: %w", frame["type"], err)
	}
	raw, err := br.ReadBytes('\n')
	if err != nil {
		return reply{}, fmt.Errorf("read %v reply: %w", frame["type"], err)
	}
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return reply{}, fmt.Errorf("decode %v reply: %w", frame["type"], err)
	}
	return r, nil
}

func logOutcome(log *calllog.Writer, kind string, r reply) {
	e := Entry{TS: now(), Kind: kind, Outcome: "ok"}
	if r.Type != "OK" {
		e.Outcome, e.Code, e.Message = "error", r.Code, r.Message
	}
	_ = log.Append(e)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func manifest(configPath string) map[string]any {
	m := map[string]any{
		"routes": []string{"/fakesvc/"},
		"status": map[string]any{"path": "/fakesvc/status"},
		"actions": []map[string]any{
			{"id": "ping", "label": "Ping", "method": "POST", "pathTemplate": "/fakesvc/ping"},
		},
	}
	if configPath != "" {
		m["config"] = map[string]any{
			"path":   configPath,
			"label":  "config.json",
			"format": "json",
			"schema": []map[string]any{{"id": "greeting", "label": "Greeting", "type": "text"}},
		}
	}
	return m
}

// handler logs the request before it replies, so a reader that has seen the
// reply needs no wait to see the line.
func handler(log *calllog.Writer, bearer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		auth := calllog.AuthLabel(r.Header.Get("Authorization"))
		_ = log.Append(Entry{TS: now(), Kind: "request", Method: r.Method, Path: r.URL.Path, Auth: auth, Body: string(body)})
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/fakesvc/status":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/fakesvc/ping":
			_, _ = w.Write([]byte(`{"ok":true,"pong":true}`))
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	})
}
