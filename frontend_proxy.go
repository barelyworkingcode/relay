package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// newRelayLLMProxy returns a reverse proxy that forwards HTTP traffic over
// the internal Unix socket to relayLLM. Streaming responses pass through
// unbuffered (FlushInterval=-1).
//
// Any inbound Authorization header is stripped before the upstream token is
// added — relay's own auth check has already run, so the frontend token must
// not cross the trust boundary.
func newRelayLLMProxy(internal Endpoint) http.Handler {
	target, _ := url.Parse("http://internal.relay.localsocket")
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", internal.Socket)
		},
	}
	rp.FlushInterval = -1

	originalDirector := rp.Director
	rp.Director = func(r *http.Request) {
		originalDirector(r)
		r.Header.Del("Authorization")
		if internal.Token != "" {
			r.Header.Set("Authorization", "Bearer "+internal.Token)
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("frontend proxy upstream error",
			"method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	return rp
}

// CheckOrigin is permissive: the frontend listener is a Unix socket, so only
// same-host processes can reach it, and bearer auth has already validated.
var frontendWSUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// newRelayLLMWSProxy upgrades the client connection and bidirectionally
// forwards messages to relayLLM's /ws endpoint over the internal socket.
func newRelayLLMWSProxy(internal Endpoint) http.Handler {
	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return net.Dial("unix", internal.Socket)
		},
		HandshakeTimeout: 10 * time.Second,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientConn, err := frontendWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Warn("frontend WS upgrade failed", "error", err)
			return
		}
		defer clientConn.Close()

		upstreamHeader := http.Header{}
		if internal.Token != "" {
			upstreamHeader.Set("Authorization", "Bearer "+internal.Token)
		}
		upstreamConn, _, err := dialer.Dial("ws://internal.relay.localsocket"+r.URL.RequestURI(), upstreamHeader)
		if err != nil {
			slog.Warn("frontend WS upstream dial failed", "error", err)
			_ = clientConn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream unreachable"),
				time.Now().Add(time.Second),
			)
			return
		}
		defer upstreamConn.Close()

		var once sync.Once
		closeBoth := func() {
			once.Do(func() {
				_ = clientConn.Close()
				_ = upstreamConn.Close()
			})
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go forwardWS(clientConn, upstreamConn, &wg, closeBoth)
		go forwardWS(upstreamConn, clientConn, &wg, closeBoth)
		wg.Wait()
	})
}

// forwardWS pumps messages from src to dst until either side closes. The
// 32KB buffer is reused across messages to avoid per-message allocation on
// streaming token traffic.
func forwardWS(src, dst *websocket.Conn, wg *sync.WaitGroup, closeBoth func()) {
	defer wg.Done()
	defer closeBoth()
	buf := make([]byte, 32*1024)
	for {
		msgType, reader, err := src.NextReader()
		if err != nil {
			return
		}
		writer, err := dst.NextWriter(msgType)
		if err != nil {
			return
		}
		if _, err := io.CopyBuffer(writer, reader, buf); err != nil {
			_ = writer.Close()
			return
		}
		if err := writer.Close(); err != nil {
			return
		}
	}
}
