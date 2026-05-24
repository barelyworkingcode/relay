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
// owns the relay-frontend Unix socket, registers project endpoints directly
// (projects are a relay-internal concern), and dispatches everything else
// to whichever enhanced service registered for the route prefix.
//
// The frontend bearer token is validated on every request and WS upgrade.
// The dispatcher then injects each service's own internal token before
// dialing it — the two trust boundaries stay distinct.
type FrontendServer struct {
	socketPath string
	server     *http.Server
	listener   net.Listener
}

// NewFrontendServer wires the mux and binds the frontend Unix socket at 0600.
// skillLister supplies the live tool list used for out-of-band SKILL.md
// regeneration on project mutations; pass nil to disable.
//
// tools enumerates the live MCP tool list for the project-picker UI; nil
// makes the GET /api/mcps/{id}/tools endpoint return 503.
//
// onProjectsChanged fires after every successful project mutation so the
// tray Settings webview can rebuild its state; nil suppresses fan-out.
//
// The dispatcher is the single handler for every route not claimed by
// relay-internal endpoints (project routes). It reads from the enhanced-
// services registry to pick a target service per request — no hardcoded
// per-service handlers live here.
func NewFrontendServer(store SettingsStore, mcps ContextSchemasProvider, tools MCPToolsProvider, frontend Endpoint, enhanced *EnhancedServiceRegistry, skillLister SkillLister, onProjectsChanged ProjectsChangedFn) (*FrontendServer, error) {
	if frontend.Socket == "" {
		return nil, errors.New("frontend socket path is empty")
	}
	if enhanced == nil {
		return nil, errors.New("enhanced-services registry is nil")
	}

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, mcps, tools, skillLister, onProjectsChanged)

	// Catch-all dispatcher: any path not matched by a more specific handler
	// (project routes above) is resolved against the manifest registry and
	// reverse-proxied to the matching enhanced service. WS upgrades are
	// handled by the same dispatcher (it detects them from the request).
	dispatcher := NewFrontendDispatcher(enhanced)
	mux.Handle("/", dispatcher)

	handler := frontendBearerAuth(frontend.Token, frontendRecover(mux))

	srv := &http.Server{
		Handler: handler,
		// Streaming sessions run for many minutes; only header/idle timeouts
		// apply, never write timeout (it would kill in-progress generations).
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}

	if err := os.MkdirAll(filepath.Dir(frontend.Socket), 0o700); err != nil {
		return nil, fmt.Errorf("create frontend socket dir: %w", err)
	}
	_ = os.Remove(frontend.Socket)
	ln, err := net.Listen("unix", frontend.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on frontend socket: %w", err)
	}
	if err := os.Chmod(frontend.Socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod frontend socket: %w", err)
	}

	slog.Info("frontend server bound",
		"socket", frontend.Socket, "auth", frontend.Token != "")
	return &FrontendServer{
		socketPath: frontend.Socket,
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
