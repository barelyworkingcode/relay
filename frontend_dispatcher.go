package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// FrontendDispatcher routes inbound front-door HTTP and WebSocket requests
// to the appropriate enhanced service, using longest-prefix-match against
// every registered manifest's routes. The single handler covers both
// protocols — WS upgrades are detected from the request headers.
//
// Per request, it reverse-proxies to the resolved service's internal Unix
// socket, stripping any inbound Authorization header and injecting the
// service-declared internal token. Trust boundaries remain distinct:
//   - frontend token authenticates Eve/Scheduler → relay
//   - internal token authenticates relay → enhanced service
type FrontendDispatcher struct {
	registry *EnhancedServiceRegistry
}

// NewFrontendDispatcher returns a dispatcher reading from the given registry.
// The registry is the only mutable state; the dispatcher itself is
// immutable + reentrant.
func NewFrontendDispatcher(registry *EnhancedServiceRegistry) *FrontendDispatcher {
	return &FrontendDispatcher{registry: registry}
}

// ServeHTTP routes one request. 404 if no manifest claims the path.
func (d *FrontendDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	svc := d.registry.LookupByPath(r.URL.Path)
	if svc == nil {
		slog.Debug("frontend dispatch: no service for path", "path", r.URL.Path)
		http.Error(w, "no service registered for this path", http.StatusNotFound)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		d.proxyWS(svc, w, r)
		return
	}
	// proxy is built once at register time; it owns a connection-pooling
	// transport, so the per-request cost here is one map lookup + ServeHTTP.
	svc.proxy.ServeHTTP(w, r)
}

// dispatcherWSUpgrader is permissive: the frontend listener is a Unix
// socket, so only same-host processes can reach it, and bearer auth has
// already validated by the time the dispatcher runs.
var dispatcherWSUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WebSocket keepalive parameters. The proxy pings each peer every wsPingPeriod
// and requires a pong (or any frame) within wsPongWait, otherwise the read
// deadline trips and both conns are torn down. Without this, a peer that dies
// without sending a close frame (network partition, hung process) would never
// unblock the read pump, leaking both conns and their goroutines.
const (
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 50 * time.Second // < wsPongWait so a pong can arrive before the deadline
	wsWriteWait  = 10 * time.Second
)

// proxyWS upgrades the client connection and bidirectionally forwards
// messages to the resolved service's WebSocket endpoint over its internal
// socket.
func (d *FrontendDispatcher) proxyWS(svc *EnhancedService, w http.ResponseWriter, r *http.Request) {
	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return net.Dial("unix", svc.InternalSocket)
		},
		HandshakeTimeout: 10 * time.Second,
	}

	clientConn, err := dispatcherWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("frontend dispatch: WS upgrade failed",
			"service", svc.ServiceID, "error", err)
		return
	}
	defer clientConn.Close()

	upstreamHeader := http.Header{}
	if svc.InternalToken != "" {
		upstreamHeader.Set("Authorization", "Bearer "+svc.InternalToken)
	}
	upstreamConn, _, err := dialer.Dial("ws://internal.relay.localsocket"+r.URL.RequestURI(), upstreamHeader)
	if err != nil {
		slog.Warn("frontend dispatch: WS upstream dial failed",
			"service", svc.ServiceID, "error", err)
		_ = clientConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream unreachable"),
			time.Now().Add(time.Second),
		)
		return
	}
	defer upstreamConn.Close()

	done := make(chan struct{})
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			close(done)
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		})
	}

	// Pingers keep each half-connection observably alive; they exit when done
	// closes. Not part of the WaitGroup — the data pumps own teardown.
	go pingDispatchedWS(clientConn, done, closeBoth)
	go pingDispatchedWS(upstreamConn, done, closeBoth)

	var wg sync.WaitGroup
	wg.Add(2)
	go forwardDispatchedWS(clientConn, upstreamConn, &wg, closeBoth)
	go forwardDispatchedWS(upstreamConn, clientConn, &wg, closeBoth)
	wg.Wait()
	closeBoth() // ensure done is closed so the pingers exit even on a clean close
}

// forwardDispatchedWS pumps messages from src to dst until either side
// closes. The 32KB buffer is reused across messages to avoid per-message
// allocation on streaming token traffic.
//
// An idle read deadline on src, extended by every pong and every data frame,
// turns a half-open peer (no close frame, no traffic) into a read error so the
// pump can tear down instead of blocking forever in NextReader.
func forwardDispatchedWS(src, dst *websocket.Conn, wg *sync.WaitGroup, closeBoth func()) {
	defer wg.Done()
	defer closeBoth()

	_ = src.SetReadDeadline(time.Now().Add(wsPongWait))
	src.SetPongHandler(func(string) error {
		return src.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	buf := make([]byte, 32*1024)
	for {
		msgType, reader, err := src.NextReader()
		if err != nil {
			return
		}
		// Inbound traffic also proves liveness — extend the deadline.
		_ = src.SetReadDeadline(time.Now().Add(wsPongWait))
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

// pingDispatchedWS sends a periodic ping so a silent-but-alive peer keeps the
// read deadline fresh, and so a dead peer is detected promptly when the ping
// write fails. WriteControl is safe to call concurrently with the single data
// writer (gorilla guarantees this), so no write lock is needed.
func pingDispatchedWS(conn *websocket.Conn, done <-chan struct{}, closeBoth func()) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				closeBoth()
				return
			}
		}
	}
}
