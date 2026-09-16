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
// straight through to the socket.
//
// This is subtle: the socket's own auth is peer-authenticated, and the peer
// on every request is always this proxy process, never the original TCP
// caller. Forwarding a tokenless request as-is would authenticate it as
// relay-sessions itself, which holds far broader grants than any individual
// session's key. requireAPIKeyHeader closes that gap in front of the dial,
// not behind it: a request with neither Authorization nor x-api-key gets a
// 401 straight from this proxy and the Unix socket is never touched.
type modelProxy struct {
	listener net.Listener
	server   *http.Server
}

// requireAPIKeyHeader rejects a request before rp ever forwards it (and
// therefore before the Unix socket is dialed) unless the request carries an
// Authorization or x-api-key header.
func requireAPIKeyHeader(rp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	})
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

			// This is subtle: httputil.ReverseProxy strips hop-by-hop
			// headers from r.Out before Rewrite ever runs, and per RFC
			// 7230 §6.1 a client names a header as hop-by-hop simply by
			// listing it in its own Connection header — so a request with
			// "Connection: Authorization" gets Authorization stripped
			// regardless of requireAPIKeyHeader having already seen it on
			// r.In. Re-assert both credential headers from r.In last, so
			// nothing later in this func can re-trigger the strip. Map
			// assignment, not .Set(): resolveBearerHeaders on the far side
			// treats a multi-value header as present-but-invalid and must
			// see every value the client actually sent, not one collapsed
			// by .Set().
			for _, h := range []string{"Authorization", "X-Api-Key"} {
				if v, ok := r.In.Header[h]; ok {
					r.Out.Header[h] = v
				} else {
					r.Out.Header.Del(h)
				}
			}
		},
		Transport: transport,
	}

	srv := &http.Server{Handler: requireAPIKeyHeader(rp)}
	go func() { _ = srv.Serve(ln) }()

	return "http://" + ln.Addr().String(), &modelProxy{listener: ln, server: srv}, nil
}

func (m *modelProxy) Close() {
	if m == nil || m.server == nil {
		return
	}
	_ = m.server.Close()
}
