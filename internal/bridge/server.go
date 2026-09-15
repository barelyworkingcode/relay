package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/presence"
)

type BridgeServer struct {
	router   ToolRouter
	listener net.Listener
	sockPath string
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc

	// closed gates wg.Add against a concurrent wg.Wait. Adding to a WaitGroup
	// whose counter has reached zero while Wait is running is a race, and
	// closing the listener does not prevent it: Accept can return a connection
	// just before StopAccepting runs.
	mu     sync.Mutex
	closed bool

	// callerSession resolves a connection's presence.CallerSession (§6.6).
	// nil in every struct literal that does not set it explicitly — a
	// handful of this package's own tests build a BridgeServer by hand
	// rather than through NewBridgeServer — so handleConn falls back to
	// PeerCallerSession itself rather than requiring every call site to
	// know about this field. Production code never overrides it.
	callerSession func(net.Conn) presence.CallerSession

	// membership answers C3 for a tokenless peer holding no launch identity
	// (plan-broker-and-sessions.md §2 C3). nil means no caller can ever be a
	// member: every tokenless request that reaches step 3 is unauthorized.
	membership MembershipResolver
}

// SetCallerSessionResolverForTest overrides how THIS server resolves a
// connection's presence.CallerSession. It exists because a real peer's
// kernel audit session is whatever the process running the test happens to
// have — there is no way to force AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS off
// from inside the test binary itself — and a test proving §6.6's refusal
// end to end needs that fact pinned, not ambient. This does not touch
// package presence or weaken Gate.Request in any way: it only supplies the
// one input Gate.Request already reads off the context before deciding
// whether to prompt at all. Production never calls this; NewBridgeServer's
// default (PeerCallerSession) is the only resolver a shipped relay uses.
func (s *BridgeServer) SetCallerSessionResolverForTest(fn func(net.Conn) presence.CallerSession) {
	s.callerSession = fn
}

func NewBridgeServer(ctx context.Context, router ToolRouter) (*BridgeServer, error) {
	sockPath := SocketPath()

	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	// Enforce owner-only access regardless of umask.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	s := &BridgeServer{
		router:        router,
		listener:      listener,
		sockPath:      sockPath,
		ctx:           ctx,
		cancel:        cancel,
		callerSession: PeerCallerSession,
	}
	// This is deliberate: the membership resolver is taken from the router
	// by optional interface rather than passed in, so there is no wiring
	// step a call site can forget. Forgetting one would not fail loudly —
	// it would silently refuse every legitimate session member, which is
	// safe but indistinguishable from the feature being broken. A router
	// that does not implement it (every fake ToolRouter in this repo's
	// tests) leaves membership nil and refuses, which is the same
	// fail-closed answer those routers give today. cmd/relay asserts at
	// compile time that its real router implements it.
	if mr, ok := router.(MembershipResolver); ok {
		s.membership = mr
	}
	return s, nil
}

// SetMembershipResolverForTest overrides how THIS server answers C3, for a
// test that needs a controlled session table without building a real
// appRouter. Production never calls this: NewBridgeServer's own lookup is
// the only resolver a shipped relay uses.
func (s *BridgeServer) SetMembershipResolverForTest(mr MembershipResolver) {
	s.membership = mr
}

func (s *BridgeServer) Serve() error {
	for {
		conn, err := s.listener.Accept()
		// This is subtle: taken before the error check, before trackConn,
		// before anything. acceptedAt bounds the connect→accept pid-reuse
		// window in C3's walk (the peer must have started before it) — every
		// microsecond spent between Accept returning and this line is a
		// microsecond a just-freed peer pid could be reused inside, so
		// nothing goes above it.
		acceptedAt := time.Now()
		if err != nil {
			return err
		}
		if !s.trackConn() {
			_ = conn.Close()
			return net.ErrClosed
		}
		go s.handleConn(conn, acceptedAt)
	}
}

func (s *BridgeServer) trackConn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// StopAccepting is the first phase of a two-phase shutdown: it stops taking
// new connections and cancels in-flight request contexts, but existing
// handlers keep running until they return or notice cancellation. Call Close
// after backends are torn down to drain the rest.
func (s *BridgeServer) StopAccepting() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	_ = s.listener.Close()
}

// Close waits for all connection handlers to finish and removes the socket
// file. Safe to call without a prior StopAccepting.
func (s *BridgeServer) Close() {
	s.StopAccepting()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

func bridgeError(code int, msg string) BridgeResponse {
	return ErrorResponse(code, msg)
}

func (s *BridgeServer) handleConn(conn net.Conn, acceptedAt time.Time) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("bridge handler panic (recovered)", "panic", r)
		}
	}()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	// Resolved once per connection, not per request: it can't change for the
	// life of the socket, and the getsockopt is pure overhead on every
	// subsequent frame. Audit attribution only.
	ctx = WithCallerPID(ctx, PeerPID(conn))
	var peer peertoken.Token
	if tok, err := peertoken.FromConn(conn); err == nil {
		peer = tok
		ctx = WithCallerPeer(ctx, tok)
	}

	// Resolved once per connection like the two above, but LAZILY: the
	// object is built here, and the ancestry walk behind it runs only if a
	// request actually reaches C3's step 3 (no token, no launch identity).
	// The peer captured above and acceptedAt are the walk's two inputs, and
	// both are fixed at accept — nothing a later request carries can change
	// which process this connection belongs to.
	ctx = WithConnMembership(ctx, NewConnMembership(s.membership, peer, acceptedAt))

	// Resolved once per connection too, beside PeerPID, and for the same
	// reason: it can't change for the socket's lifetime. Unlike PeerPID this
	// is a presence-gate INPUT, not audit-only — a caller whose session
	// cannot show a prompt must refuse before the gate ever asks
	// LocalAuthentication, which does not itself refuse a remote caller and
	// would otherwise raise the login-password prompt on the physical
	// console for whoever is sitting there (§6.6).
	resolveSession := s.callerSession
	if resolveSession == nil {
		resolveSession = PeerCallerSession
	}
	ctx = presence.WithCallerSession(ctx, resolveSession(conn))

	// Close the connection when this context is cancelled so a handler
	// blocked in scanner.Scan() unblocks promptly at shutdown. Without this,
	// StopAccepting() only cancels the context and closes the *listener* — an
	// in-flight Scan() keeps waiting for the peer to disconnect, and Close()'s
	// wg.Wait() can deadlock against a managed service holding a persistent
	// bridge connection that isn't killed until later in teardown.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// No deadlines: this is a 0600 Unix socket, so the peer is same-user and a
	// stalled client is a bug rather than an attack. RemoteServer, whose peer
	// is across a network, passes a non-zero idle timeout instead.
	NewFrameConn(conn, "bridge", 0).Serve(ctx, s.handleRequest)
}

type bridgeHandler struct {
	requireAdmin bool
	handle       func(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse
}

var bridgeHandlers = map[string]bridgeHandler{
	ReqListTools:             {handle: handleListTools},
	ReqCallTool:              {handle: handleCallTool},
	ReqReconcileExternalMcps: {requireAdmin: true, handle: handleReconcile},
	ReqReloadExternalMcp:     {requireAdmin: true, handle: handleReloadMcp},
	ReqReloadService:         {requireAdmin: true, handle: handleReloadService},
	ReqDescribeProject:       {handle: handleDescribeProject},
	ReqRegisterManifest:      {handle: handleRegisterManifest},
	ReqRegisterModelHost:     {handle: handleRegisterModelHost},
	ReqHello:                 {handle: handleHello},

	// This is deliberate: unlike every requireAdmin entry above, admin_op
	// carries no bearer. ADR-015 and ADR-016 both refuse to spend the 0600
	// socket's ambient trust twice, and admin_secret is a sealed value the
	// caller can no longer read to present here anyway. The gate that
	// matters lives inside the operation core this dispatches to, not on
	// this transport.
	ReqAdminOp: {handle: handleAdminOp},
}

func (s *BridgeServer) handleRequest(ctx context.Context, line string) BridgeResponse {
	var req BridgeRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "parse error: "+err.Error())
	}

	h, ok := bridgeHandlers[req.Type]
	if !ok {
		slog.Warn("bridge: unknown request type", "type", req.Type)
		return bridgeError(jsonrpc.CodeMethodNotFound, "unknown request type: "+req.Type)
	}

	if h.requireAdmin {
		if err := s.router.ValidateAdmin(req.Token); err != nil {
			return bridgeError(jsonrpc.CodeUnauthorized, "admin auth: "+err.Error())
		}
	}

	// This is deliberate: BridgeRequest.Cwd is decoded and then dropped on
	// the floor. Directory auth is retired (plan-broker-and-sessions.md §2
	// C3): a working directory is something a caller ASSERTS about itself,
	// and relay now identifies a tokenless caller only by what the kernel
	// says about it — its audit token, and its ancestry from there. The
	// field stays on the wire type so an older client's request still parses
	// rather than erroring, and it reaches no authorization or audit path at
	// all. Nothing here may put it on the context: the value being
	// unreachable is the guarantee.

	return h.handle(ctx, &req, s.router)
}

func handleListTools(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	tools, err := router.ListTools(ctx, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespTools, Tools: tools}
}

func handleCallTool(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.CallTool(ctx, req.Name, req.Arguments, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespResult, Result: result}
}

func classifyErrorCode(err error) int {
	return ErrorCode(err)
}

func handleReconcile(ctx context.Context, _ *BridgeRequest, router ToolRouter) BridgeResponse {
	router.ReconcileExternalMcps(ctx)
	return BridgeResponse{Type: RespOK}
}

func handleReloadMcp(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if err := router.ReloadExternalMcp(ctx, req.Name); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}

func handleReloadService(_ context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if err := router.ReloadService(req.Name); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}

// handleHello answers every refusal with one fixed message: the router's
// error names the reason for relay's log, and a caller must learn neither
// that reason nor, ever, the secret it sent.
func handleHello(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.Hello(ctx, req.Name, req.Token, req.Kind)
	if err != nil {
		slog.Warn("bridge: hello refused", "name", req.Name, "peer_pid", CallerPIDFromContext(ctx), "reason", err)
		return bridgeError(jsonrpc.CodeUnauthorized, "hello refused")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, "hello: encode result")
	}
	return BridgeResponse{Type: RespOK, Data: data}
}

func handleDescribeProject(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	desc, err := router.DescribeProject(ctx, req.Token)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return bridgeError(jsonrpc.CodeInternalError, err.Error())
	}
	return BridgeResponse{Type: RespProjectDescription, Data: data}
}

func handleAdminOp(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	result, err := router.AdminOp(ctx, req.Name, req.Arguments)
	if err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespResult, Result: result}
}

func handleRegisterModelHost(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "register_model_host: missing arguments")
	}
	var r RegisterModelHostRequest
	if err := json.Unmarshal(req.Arguments, &r); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "register_model_host: "+err.Error())
	}
	if err := r.Validate(); err != nil {
		return bridgeError(jsonrpc.CodeInvalidParams, err.Error())
	}
	if err := router.RegisterModelHost(ctx, r, req.Token); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}

func handleRegisterManifest(ctx context.Context, req *BridgeRequest, router ToolRouter) BridgeResponse {
	if len(req.Arguments) == 0 {
		return bridgeError(jsonrpc.CodeInvalidParams, "register_manifest: missing arguments")
	}
	var r RegisterManifestRequest
	if err := json.Unmarshal(req.Arguments, &r); err != nil {
		return bridgeError(jsonrpc.CodeParseError, "register_manifest: "+err.Error())
	}
	if err := r.Validate(); err != nil {
		return bridgeError(jsonrpc.CodeInvalidParams, err.Error())
	}
	if err := router.RegisterManifest(ctx, r, req.Token); err != nil {
		return bridgeError(classifyErrorCode(err), err.Error())
	}
	return BridgeResponse{Type: RespOK}
}
