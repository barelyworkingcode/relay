package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/presence"
)

// A relay route whose handler outlives frontendRouteReadDeadline must not
// cancel the context of the next request on the same keep-alive connection.
func TestRelayRouteReadDeadline_SlowHandlerDoesNotPoisonConnection(t *testing.T) {
	origDeadline := frontendRouteReadDeadline
	frontendRouteReadDeadline = 100 * time.Millisecond
	t.Cleanup(func() { frontendRouteReadDeadline = origDeadline })
	handlerRun := 3 * frontendRouteReadDeadline

	cases := []struct {
		name   string
		method string
		body   string
	}{
		{name: "no-body GET", method: http.MethodGet},
		{name: "PUT with JSON body through the presence gate", method: http.MethodPut, body: `{"name":"acme"}`},
	}
	gate, err := presence.NewGate(approvingPresenceProvider{})
	if err != nil {
		t.Fatalf("presence.NewGate: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/slow", func(w http.ResponseWriter, r *http.Request) {
				if tc.body != "" {
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != tc.body {
						http.Error(w, "body read: "+string(got), http.StatusBadRequest)
						return
					}
					if _, err := requireGate(gate, r.Context(), "credential.mint", presence.Digest{}, "test", presenceAttempt{}); err != nil {
						http.Error(w, "presence gate: "+err.Error(), http.StatusInternalServerError)
						return
					}
				}
				time.Sleep(handlerRun)
				w.WriteHeader(http.StatusOK)
			})
			// The "/" catch-all stands in for the proxied dispatcher, which
			// fails fast on a cancelled context.
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, errString(r.Context().Err()))
			})

			sock := filepath.Join(mkShortTempDir(t, "rd"), "f.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			srv := &http.Server{
				Handler:           withRelayRouteReadDeadline(mux),
				ConnContext:       withFrontendPeerFromConn,
				ReadHeaderTimeout: 30 * time.Second,
				IdleTimeout:       5 * time.Minute,
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			transport := &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
				MaxConnsPerHost: 1,
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}

			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			}
			slowReq, err := http.NewRequest(tc.method, "http://unix/api/slow", bodyReader)
			if err != nil {
				t.Fatalf("NewRequest slow: %v", err)
			}
			if tc.body != "" {
				slowReq.Header.Set("Content-Type", "application/json")
			}
			start := time.Now()
			slowStatus, slowBody := doAndDrain(t, client, slowReq)
			if slowStatus != http.StatusOK {
				t.Fatalf("slow route: status %d body=%q", slowStatus, slowBody)
			}
			if elapsed := time.Since(start); elapsed < handlerRun {
				t.Fatalf("slow route returned after %s, before its %s run; the deadline was never outlived", elapsed, handlerRun)
			}

			var reused bool
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
			nextReq, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "http://unix/api/models", nil)
			if err != nil {
				t.Fatalf("NewRequest next: %v", err)
			}
			nextStatus, ctxErr := doAndDrain(t, client, nextReq)
			if !reused {
				t.Fatalf("second request did not reuse the first request's connection; the test proves nothing")
			}
			if nextStatus != http.StatusOK || ctxErr != errString(nil) {
				t.Fatalf("next request on the same connection: status %d, context err %q; want 200 and %q", nextStatus, ctxErr, errString(nil))
			}
		})
	}
}

type approvingPresenceProvider struct{}

func (approvingPresenceProvider) Evaluate(context.Context, string) error { return nil }

func doAndDrain(t *testing.T, client *http.Client, req *http.Request) (int, string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, string(body)
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
