// Package api holds the WebSocket hub other session-hosting units register
// their message handlers on. It carries no knowledge of sessions, terminals,
// or permissions — those message types and their handlers land later
// (R-S6/R-S7b/R-S7c), on top of the registry this file defines.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	clk "github.com/barelyworkingcode/relay/internal/sessions/clock"
)

// CheckOrigin is blanket-true deliberately, not an oversight: this hub is
// reachable only via hostapi's /ws, which gates every request through
// checkInternalPeer (kernel-attested peer pid + bearer) before Upgrade is
// ever called. A same-origin check here would add nothing — the peer check
// already rejects every caller that isn't relay's own front-door dispatcher
// dialing over a 0600 Unix socket — so do not "fix" this without removing
// that gate too.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Conn wraps one live WebSocket connection. ID/RemoteAddr/ConnectedAt are
// immutable after newConn; activity is tracked so a later status feature can
// report per-connection idle state without the hub needing to know what a
// session or terminal is.
type Conn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	ID          uint64
	RemoteAddr  string
	ConnectedAt time.Time
	clock       clk.Clock

	lastActivityNano atomic.Int64
}

var connSeq atomic.Uint64

func newConn(conn *websocket.Conn, remoteAddr string, clock clk.Clock) *Conn {
	if clock == nil {
		clock = clk.DefaultClock
	}
	now := clock.Now()
	c := &Conn{
		conn:        conn,
		ID:          connSeq.Add(1),
		RemoteAddr:  remoteAddr,
		ConnectedAt: now,
		clock:       clock,
	}
	c.lastActivityNano.Store(now.UnixNano())
	return c
}

// Write sends a raw text frame to this connection. The single funnel every
// outbound message goes through, so activity tracking can never drift from
// what actually went out on the wire.
func (c *Conn) Write(data []byte) error {
	c.mu.Lock()
	err := c.conn.WriteMessage(websocket.TextMessage, data)
	c.mu.Unlock()
	if err == nil {
		c.lastActivityNano.Store(c.clock.Now().UnixNano())
	}
	return err
}

func (c *Conn) noteRead() {
	c.lastActivityNano.Store(c.clock.Now().UnixNano())
}

// LastActivity is the time of this connection's most recent read or write.
func (c *Conn) LastActivity() time.Time {
	return time.Unix(0, c.lastActivityNano.Load())
}

// Handler processes one inbound message of the type it was registered for.
// raw is the full, still-encoded message, so a handler decodes its own
// request shape instead of the hub guessing it.
type Handler func(c *Conn, raw []byte)

// DisconnectHook is called with a connection's ID after it has been removed
// from the hub. Whatever package owns per-connection state keyed by
// connection ID (a session or terminal's viewer set) registers one of these
// to clean up, without the hub needing to know what that state is.
type DisconnectHook func(connID uint64)

// Hub tracks live WebSocket connections and dispatches each inbound
// message, by its "type" field, to a handler another package registered.
// It never grows session/terminal/permission-specific knowledge itself —
// that belongs entirely to the handlers registered on top of it.
type Hub struct {
	mu    sync.RWMutex
	conns map[*Conn]bool

	handlers        map[string]Handler
	disconnectHooks []DisconnectHook

	clock clk.Clock
}

// NewHub returns an empty hub with no connections and no registered
// handlers.
func NewHub() *Hub {
	return &Hub{
		conns:    make(map[*Conn]bool),
		handlers: make(map[string]Handler),
		clock:    clk.DefaultClock,
	}
}

// SetClock installs the clock used for connection activity timestamps.
func (h *Hub) SetClock(c clk.Clock) {
	if c == nil {
		c = clk.DefaultClock
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = c
}

// RegisterHandler wires fn to run for every inbound message whose "type"
// field equals msgType. Intended to be called at startup, once per message
// type; a later registration for the same type replaces the earlier one.
func (h *Hub) RegisterHandler(msgType string, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[msgType] = fn
}

// OnDisconnect registers fn to run, with a connection's ID, once that
// connection has been removed from the hub.
func (h *Hub) OnDisconnect(fn DisconnectHook) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.disconnectHooks = append(h.disconnectHooks, fn)
}

// Broadcast sends data to every currently connected client.
func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		c.Write(data)
	}
}

// Conns returns a snapshot of the currently live connections.
func (h *Hub) Conns() []*Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

// HandleUpgrade upgrades r to a WebSocket, registers the connection with the
// hub, and runs its read loop until the client disconnects, dispatching each
// inbound message by its "type" field to a registered handler. A message
// whose type has no registered handler is dropped silently.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}
	slog.Info("websocket connected", "remote", r.RemoteAddr)

	h.mu.RLock()
	clock := h.clock
	h.mu.RUnlock()
	c := newConn(conn, r.RemoteAddr, clock)

	h.mu.Lock()
	h.conns[c] = true
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		hooks := append([]DisconnectHook(nil), h.disconnectHooks...)
		h.mu.Unlock()
		for _, hook := range hooks {
			hook(c.ID)
		}
		conn.Close()
		slog.Info("websocket disconnected", "remote", r.RemoteAddr)
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("websocket read error", "error", err)
			}
			return
		}
		c.noteRead()

		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		h.mu.RLock()
		fn := h.handlers[msg.Type]
		h.mu.RUnlock()
		if fn != nil {
			fn(c, msgBytes)
		}
	}
}
