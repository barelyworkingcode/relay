package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Trust boundaries stay distinct end to end: a control-plane credential or a
// launch identity authenticates the caller to relay, and a separate internal token (injected per
// service, never the caller's) authenticates relay to the enhanced service.
type FrontendDispatcher struct {
	registry *EnhancedServiceRegistry
}

func NewFrontendDispatcher(registry *EnhancedServiceRegistry) *FrontendDispatcher {
	return &FrontendDispatcher{registry: registry}
}

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
	svc.proxy.ServeHTTP(w, r)
}

// Permissive by design: the frontend listener is a Unix socket, so only
// same-host processes can reach it, and bearer auth has already run by the
// time the dispatcher sees the request.
var dispatcherWSUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsPongWait/wsPingPeriod are atomics rather than plain vars so a test can
// shorten them to exercise the half-open-reaping path without racing the
// live proxy goroutines that read them on every pong/tick. Invariant:
// wsPingPeriod < wsPongWait, so a pong can arrive before the read deadline.
var (
	wsPongWaitNanos   atomic.Int64
	wsPingPeriodNanos atomic.Int64
)

func init() {
	wsPongWaitNanos.Store(int64(60 * time.Second))
	wsPingPeriodNanos.Store(int64(50 * time.Second))
}

func wsPongWait() time.Duration   { return time.Duration(wsPongWaitNanos.Load()) }
func wsPingPeriod() time.Duration { return time.Duration(wsPingPeriodNanos.Load()) }

const wsWriteWait = 10 * time.Second

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
	defer func() { _ = clientConn.Close() }()

	upstreamHeader := http.Header{}
	if svc.InternalToken != "" {
		upstreamHeader.Set("Authorization", "Bearer "+svc.InternalToken)
	}
	upstreamConn, resp, err := dialer.Dial("ws://internal.relay.localsocket"+r.URL.RequestURI(), upstreamHeader)
	if err != nil {
		// On handshake failure gorilla may still return a non-nil response
		// whose body the caller owns; close it before bailing out.
		if resp != nil {
			_ = resp.Body.Close()
		}
		slog.Warn("frontend dispatch: WS upstream dial failed",
			"service", svc.ServiceID, "error", err)
		_ = clientConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream unreachable"),
			time.Now().Add(time.Second),
		)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	done := make(chan struct{})
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			close(done)
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		})
	}

	// Pingers are not part of the WaitGroup below: the data pumps own
	// teardown, and pingers just exit when done closes.
	go pingDispatchedWS(clientConn, done, closeBoth)
	go pingDispatchedWS(upstreamConn, done, closeBoth)

	var wg sync.WaitGroup
	wg.Add(2)
	go forwardDispatchedWS(clientConn, upstreamConn, &wg, closeBoth)
	go forwardDispatchedWS(upstreamConn, clientConn, &wg, closeBoth)
	wg.Wait()
	closeBoth() // redundant once.Do call as a safety net so the pingers always exit
}

// The 32KB buffer is reused across messages to avoid per-message allocation
// on streaming token traffic. The read deadline on src is extended by every
// pong AND every data frame, so a half-open peer (no close frame, no
// traffic) becomes a read error instead of blocking NextReader forever.
func forwardDispatchedWS(src, dst *websocket.Conn, wg *sync.WaitGroup, closeBoth func()) {
	defer wg.Done()
	defer closeBoth()

	_ = src.SetReadDeadline(time.Now().Add(wsPongWait()))
	src.SetPongHandler(func(string) error {
		return src.SetReadDeadline(time.Now().Add(wsPongWait()))
	})

	buf := make([]byte, 32*1024)
	for {
		msgType, reader, err := src.NextReader()
		if err != nil {
			return
		}
		// Inbound data frames also prove liveness, not just pongs.
		_ = src.SetReadDeadline(time.Now().Add(wsPongWait()))
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

// WriteControl is safe to call concurrently with the single data writer
// (gorilla guarantees this), so no write lock is needed here.
func pingDispatchedWS(conn *websocket.Conn, done <-chan struct{}, closeBoth func()) {
	ticker := time.NewTicker(wsPingPeriod())
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
