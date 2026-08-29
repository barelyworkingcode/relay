package main

// Issue #49: an execute-class route is absent from the TCP mux (ADR-015
// decision 2), so the probe that decision exists to defeat is answered by
// http.ServeMux itself and used to leave no trace at all, while the same
// request on the socket wrote a denied control decision. These tests pin the
// trace: one line per unmatched path, none for a route that ran, and none
// reachable without a credential.

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// urCaptureLogs redirects the default logger into a buffer for the duration
// of one test.
func urCaptureLogs(t *testing.T) *lrSyncBuffer {
	t.Helper()
	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func urUnmatchedLines(logs *lrSyncBuffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, unmatchedRouteWarning) {
			out = append(out, line)
		}
	}
	return out
}

// POST /api/services is the near-miss ADR-015 decision 2 produces: GET holds
// the path, the execute-class POST was never registered on this listener,
// and nothing absorbs it because ClassProxy is socket-only. http.ServeMux
// answers 405 with no handler run, which is exactly the shape that used to
// be silent.
func TestTCPUnmatchedRoute_MethodNotAllowedIsLogged(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	resp, body := ts.doTCP(t, teRoute{method: "POST", path: "/api/services"})
	teAssertMuxRefused(t, resp, body)

	lines := urUnmatchedLines(logs)
	if len(lines) != 1 {
		t.Fatalf("want exactly one unmatched-route warning, got %d:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"method=POST", "path=/api/services", "transport=tcp"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("warning does not carry %s: %s", want, lines[0])
		}
	}
}

func TestTCPUnmatchedRoute_UnknownPathIsLogged(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	resp, body := ts.doTCP(t, teRoute{method: "GET", path: "/api/no-such-thing"})
	teAssertMuxRefused(t, resp, body)

	lines := urUnmatchedLines(logs)
	if len(lines) != 1 {
		t.Fatalf("want exactly one unmatched-route warning, got %d:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], "path=/api/no-such-thing") {
		t.Fatalf("warning names the wrong path: %s", lines[0])
	}
}

// A handler that ran and answered "no such project" is not an unmatched
// route, and a 404 status alone cannot tell the two apart.
func TestTCPUnmatchedRoute_AHandlersOwn404IsNotLogged(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	resp, body := ts.doTCP(t, teRoute{method: "GET", path: "/api/projects/no-such-project"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from the handler; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("content-type = %q, so this was not a relay handler answering; body=%s",
			resp.Header.Get("Content-Type"), body)
	}
	if lines := urUnmatchedLines(logs); len(lines) != 0 {
		t.Fatalf("a handler's own 404 was logged as an unmatched route:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTCPUnmatchedRoute_AMatchedRouteIsNotLogged(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	resp, body := ts.doTCP(t, teRoute{method: "GET", path: "/api/projects"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if lines := urUnmatchedLines(logs); len(lines) != 0 {
		t.Fatalf("a matched route was logged as unmatched:\n%s", strings.Join(lines, "\n"))
	}
}

// The line is reachable only past frontendCredentialAuth. Option C rests on
// that: a logger an unauthenticated caller can drive is a new
// unauthenticated-reachable code path on a listener ADR-015 left empty.
func TestTCPUnmatchedRoute_UnauthenticatedProbeLogsNothing(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	for _, token := range []string{"", "not-a-real-credential"} {
		resp, body := teDo(t, http.DefaultClient, "POST", ts.tcpBase+"/api/services", token, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401; body=%s", token, resp.StatusCode, body)
		}
	}
	if lines := urUnmatchedLines(logs); len(lines) != 0 {
		t.Fatalf("an unauthenticated probe reached the unmatched-route logger:\n%s", strings.Join(lines, "\n"))
	}
}

// The socket has the catch-all, so nothing there is unmatched: a miss is the
// dispatcher's "no service registered for this path", which is a different
// fact about a different listener and must stay unlogged here.
func TestSocketUnmatchedRoute_StaysSilent(t *testing.T) {
	logs := urCaptureLogs(t)
	ts := teNewServer(t, newCLISandboxStore(t), nil)

	resp, body := ts.doSocket(t, teRoute{method: "POST", path: "/api/no-such-thing"})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), teDispatcherNoService) {
		t.Fatalf("socket miss did not reach the dispatcher: status=%d body=%s", resp.StatusCode, body)
	}
	if lines := urUnmatchedLines(logs); len(lines) != 0 {
		t.Fatalf("the socket door logged an unmatched route:\n%s", strings.Join(lines, "\n"))
	}
}
