package provider

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// modelProxy turns RELAY_MODEL_SOCKET (a Unix socket path, C5's env table)
// into a loopback-TCP http(s) URL pi's overlay can use as a baseUrl.
//
// Judgment call: pi is a Node process using the stock `openai` SDK against a
// caller-supplied baseUrl (SP9) — it has no mechanism to dial a Unix socket
// itself, unlike this Go process (see relayHarness's internal/llm/openai.go
// for the client-side half of this same pattern: a custom
// http.Transport.DialContext that ignores the network/address net/http
// hands it and dials the socket directly). There is no existing "unix
// socket behind a TCP URL" bridge anywhere else in this codebase as of this
// port — relay's model_endpoint TCP listener (docs/model-endpoint.md) is a
// separate, globally-configured, off-by-default listener, not a per-session
// one this package could just point pi at. So this file is that bridge:
// a loopback-only, ephemeral-port reverse proxy, started per pi session and
// torn down with it, that forwards every request (headers included, so
// pi's own `Authorization: Bearer <rmk_key>` reaches the broker unmodified)
// straight through to the socket. The broker's own auth order checks the
// bearer first regardless of transport, so this proxy adds no privilege of
// its own — a caller without the session's key gets exactly the 401 it
// would get dialing the socket directly.
type modelProxy struct {
	listener net.Listener
	server   *http.Server
}

// startModelProxy binds 127.0.0.1:0 and starts forwarding to socketPath.
// Returns the resulting base URL (e.g. "http://127.0.0.1:54321") and the
// proxy; the caller must call Close when the session ends. Returns
// ("", nil, nil) when socketPath is empty — callers should treat that as
// "no model broker available for this session" rather than an error.
func startModelProxy(socketPath string) (string, *modelProxy, error) {
	if socketPath == "" {
		return "", nil, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}

	dialer := net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	target := &url.URL{Scheme: "http", Host: "unix"}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
		},
		Transport: transport,
	}

	srv := &http.Server{Handler: rp}
	go func() { _ = srv.Serve(ln) }()

	return "http://" + ln.Addr().String(), &modelProxy{listener: ln, server: srv}, nil
}

func (m *modelProxy) Close() {
	if m == nil || m.server == nil {
		return
	}
	_ = m.server.Close()
}
