// Package bridge serves the bridge socket: newline-delimited JSON frames,
// Hello and RegisterManifest only.
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/launch"
	"github.com/barelyworkingcode/relay/fakerelay/internal/peer"
)

// Error codes of the Error frame.
const (
	CodeParse        = -32700
	CodeUnsupported  = -32601
	CodeInvalid      = -32602
	CodeRefused      = -32001
	CodeInternal     = -32603
	maxFrame         = 10 << 20
	msgCapability    = "RegisterManifest requires a launch identity holding that capability"
	msgToken         = "RegisterManifest requires the calling service's launch identity, not a token"
	reservedSessions = "is reserved to relay's internal session-host API"
)

// Manifest is the part of a service manifest the dispatcher uses. Status,
// actions and config are accepted and not interpreted.
type Manifest struct {
	Routes  []string        `json:"routes"`
	Status  json.RawMessage `json:"status,omitempty"`
	Actions json.RawMessage `json:"actions,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
}

// Validate applies the rules a manifest meets before the route table does.
func (m Manifest) Validate() error {
	if len(m.Routes) == 0 {
		return errors.New("manifest: routes is empty")
	}
	seen := map[string]bool{}
	for i, r := range m.Routes {
		if !strings.HasPrefix(r, "/") {
			return fmt.Errorf("manifest: routes[%d] %q must start with \"/\"", i, r)
		}
		if seen[r] {
			return fmt.Errorf("manifest: routes[%d] %q is duplicated", i, r)
		}
		seen[r] = true
		for _, reserved := range []string{"/launch", "/terminate", "/handoff", "/handback", "/send"} {
			if r == reserved || strings.HasPrefix(r, reserved+"/") {
				return fmt.Errorf("manifest: routes[%d] %q %s", i, r, reservedSessions)
			}
		}
		if r == "/relay" || strings.HasPrefix(r, "/relay/") {
			return fmt.Errorf("manifest registry: route %q is reserved to relay (/relay/)", r)
		}
	}
	return nil
}

// FaultResult is what a fault says to do with a frame.
type FaultResult struct {
	Down    bool
	Refuse  bool
	Message string
}

// Hooks connect the socket to the rest of the instance.
type Hooks struct {
	Events   *events.Log
	RelayPID int
	Launch   *launch.Manager
	// Register installs a manifest. The registration outlives the connection
	// that made it; only the service's process ending forgets it.
	Register func(serviceID string, m Manifest, socket, token string) error
	// Fault runs the BRIDGE <Type> faults for a frame; it may block while a
	// fault holds the frame.
	Fault func(typ string) FaultResult
}

type registerArgs struct {
	ServiceID      string          `json:"serviceId"`
	Manifest       json.RawMessage `json:"manifest"`
	InternalSocket string          `json:"internalSocket"`
	InternalToken  string          `json:"internalToken"`
}

type request struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Token     string          `json:"token"`
	Kind      string          `json:"kind"`
	Arguments json.RawMessage `json:"arguments"`
	TraceID   string          `json:"trace_id"`
}

// Serve accepts connections until l is closed, then waits for them to end.
func Serve(l net.Listener, h Hooks) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	for {
		c, err := l.Accept()
		if err != nil {
			break
		}
		mu.Lock()
		conns[c] = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(c, h)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
		}()
	}
	mu.Lock()
	for c := range conns {
		c.Close()
	}
	mu.Unlock()
	wg.Wait()
}

func serveConn(c net.Conn, h Hooks) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			reply(c, errFrame(CodeParse, "frame is not valid JSON"))
			continue
		}
		if f := h.Fault(req.Type); f.Down {
			return
		} else if f.Refuse {
			if req.Type == "Hello" {
				reply(c, errFrame(CodeRefused, "hello refused"))
			} else {
				reply(c, errFrame(CodeInternal, f.Message))
			}
			continue
		}
		ctx := events.WithTrace(context.Background(), req.TraceID)
		switch req.Type {
		case "Hello":
			reply(c, hello(ctx, c, req, h))
		case "RegisterManifest":
			reply(c, register(ctx, c, req, h))
		default:
			reply(c, errFrame(CodeUnsupported, "unknown request type: "+req.Type))
		}
	}
}

func hello(ctx context.Context, c net.Conn, req request, h Hooks) []byte {
	ev := h.Events.Begin(ctx, "bridge.hello").Set("service_id", req.Name).Set("kind", "service")
	pid, err := peer.PID(c)
	if err == nil && (req.Kind == "" || req.Kind == "service") && h.Launch.Hello(req.Name, req.Token, pid) {
		ev.End("ok", "", nil)
		b, _ := json.Marshal(map[string]any{"type": "OK", "data": map[string]any{"kind": "service", "service_id": req.Name, "relay_pid": h.RelayPID}})
		return b
	}
	ev.End("denied", "unauthorized", errors.New("hello refused"))
	return errFrame(CodeRefused, "hello refused")
}

func register(ctx context.Context, c net.Conn, req request, h Hooks) []byte {
	ev := h.Events.Begin(ctx, "service.manifest.register")
	refuse := func(status, reason string, code int, msg string) []byte {
		ev.End(status, reason, errors.New(msg))
		return errFrame(code, msg)
	}
	if req.Token != "" {
		return refuse("denied", "unauthorized", CodeRefused, msgToken)
	}
	pid, err := peer.PID(c)
	var ident launch.Identity
	ok := false
	if err == nil {
		ident, ok = h.Launch.IdentityByPID(pid)
	}
	if !ok || !ident.Has("manifest") {
		return refuse("denied", "not_granted", CodeRefused, msgCapability)
	}
	var a registerArgs
	if len(req.Arguments) == 0 || json.Unmarshal(req.Arguments, &a) != nil {
		return refuse("error", "invalid", CodeInvalid, "register_manifest: missing arguments")
	}
	ev.Set("service_id", a.ServiceID)
	if a.ServiceID != ident.ServiceID {
		return refuse("denied", "not_granted", CodeRefused, fmt.Sprintf("RegisterManifest: service %q may not register a manifest for %q", ident.ServiceID, a.ServiceID))
	}
	for _, f := range []struct{ name, v string }{{"serviceId", a.ServiceID}, {"internalSocket", a.InternalSocket}, {"internalToken", a.InternalToken}} {
		if f.v == "" {
			return refuse("error", "invalid", CodeInvalid, "register_manifest: "+f.name+" is empty")
		}
	}
	var m Manifest
	if err := json.Unmarshal(a.Manifest, &m); err != nil {
		return refuse("error", "invalid", CodeInvalid, "manifest: routes is empty")
	}
	if err := m.Validate(); err != nil {
		return refuse("error", "invalid", CodeInvalid, err.Error())
	}
	err = h.Register(a.ServiceID, m, a.InternalSocket, a.InternalToken)
	if err != nil {
		return refuse("error", "conflict", CodeInvalid, err.Error())
	}
	ev.End("ok", "", nil)
	return []byte(`{"type":"OK"}`)
}

func errFrame(code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "Error", "code": code, "message": msg})
	return b
}

func reply(c net.Conn, frame []byte) { _, _ = c.Write(append(frame, '\n')) }
