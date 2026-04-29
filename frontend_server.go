package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FrontendServer hosts the HTTP API that Eve and relayScheduler consume. It
// owns the relay-frontend Unix socket, registers project endpoints directly,
// and reverse-proxies everything else to relayLLM.
//
// The frontend bearer token is validated on every request and WS upgrade.
// relay then injects the *internal* token before dialing relayLLM — the two
// trust boundaries stay distinct.
type FrontendServer struct {
	socketPath string
	server     *http.Server
	listener   net.Listener
}

// NewFrontendServer wires the mux and binds the frontend Unix socket at 0600.
func NewFrontendServer(store SettingsStore, creds LLMChannelCreds) (*FrontendServer, error) {
	if creds.Frontend.Socket == "" {
		return nil, errors.New("frontend socket path is empty")
	}
	if creds.Internal.Socket == "" {
		return nil, errors.New("internal socket path is empty")
	}

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store)

	// Catch-all proxy: any /api/* not matched by a more specific handler
	// (project routes above) falls through to relayLLM. New relayLLM
	// endpoints work without a relay-side allowlist update.
	httpProxy := newRelayLLMProxy(creds.Internal)
	mux.Handle("/api/", httpProxy)
	mux.Handle("/ws", newRelayLLMWSProxy(creds.Internal))

	handler := frontendBearerAuth(creds.Frontend.Token, frontendRecover(mux))

	srv := &http.Server{
		Handler: handler,
		// Streaming sessions run for many minutes; only header/idle timeouts
		// apply, never write timeout (it would kill in-progress generations).
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}

	if err := os.MkdirAll(filepath.Dir(creds.Frontend.Socket), 0o700); err != nil {
		return nil, fmt.Errorf("create frontend socket dir: %w", err)
	}
	_ = os.Remove(creds.Frontend.Socket)
	ln, err := net.Listen("unix", creds.Frontend.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on frontend socket: %w", err)
	}
	if err := os.Chmod(creds.Frontend.Socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod frontend socket: %w", err)
	}

	slog.Info("frontend server bound",
		"socket", creds.Frontend.Socket, "auth", creds.Frontend.Token != "")
	return &FrontendServer{
		socketPath: creds.Frontend.Socket,
		server:     srv,
		listener:   ln,
	}, nil
}

// Serve blocks accepting connections until Shutdown is called.
func (s *FrontendServer) Serve() error {
	if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown drains in-flight requests and unlinks the socket file.
func (s *FrontendServer) Shutdown(ctx context.Context) {
	if s == nil || s.server == nil {
		return
	}
	_ = s.server.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
}

// frontendBearerAuth validates the frontend bearer token. Empty token = dev
// mode (no auth). Constant-time comparison runs before any handler so
// unauthenticated WS upgrades never allocate a session.
func frontendBearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(strings.TrimSpace(header[len(prefix):]))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			slog.Warn("frontend: bad bearer token",
				"method", r.Method, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func frontendRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("frontend: panic in handler",
					"error", err, "method", r.Method, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
