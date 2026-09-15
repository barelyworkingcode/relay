package hostapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// Config is what a running relay-sessions host needs to serve C5's internal
// API and C6's hook socket. RelayPID is fixed at construction: it comes from
// this host's own Hello to relay's bridge (kind "service", the existing,
// unmodified bridge.SendHello — see cmd/relaysessions/main.go), done once at
// startup. A relay restart mid-life is out of scope for this skeleton.
type Config struct {
	InternalSocket string // relay dials this; POST /launch, POST /terminate
	InternalBearer string // the bearer relay must present, per its own RegisterManifest
	RelayPID       int    // relay's pid, from this host's own Hello OK
	HookSocket     string // `relay-sessions hook` dials this; POST /permission
	ShimBinary     string // absolute path to the relay-sessions binary (argv[0] re-exec for `exec`)
}

// Server is relay-sessions' internal API host. Its session table is a
// skeleton (session_table.go's doc comment): enough bookkeeping to make
// /launch, /terminate and /permission meaningful, no real terminal/Claude/pi
// hosting.
type Server struct {
	cfg   Config
	table *sessionTable

	internalLn  net.Listener
	internalSrv *http.Server
	hookLn      net.Listener
	hookSrv     *http.Server
}

// New constructs a Server. Call ListenInternal/ListenHook then
// ServeInternal/ServeHook (typically each in its own goroutine).
func New(cfg Config) *Server {
	return &Server{cfg: cfg, table: newSessionTable()}
}

// connInfo is what this package's ConnContext hook captures per accepted
// connection: the peer's kernel-attested audit token and the moment the
// connection was accepted, both of which membership.Resolve needs (C3) and
// neither of which a request handler can otherwise reach through
// net/http's Request.
type connInfo struct {
	peer       peertoken.Token
	acceptedAt time.Time
}

type connInfoKey struct{}

func captureConnInfo(ctx context.Context, c net.Conn) context.Context {
	info := connInfo{acceptedAt: time.Now()}
	if tok, err := peertoken.FromConn(c); err == nil {
		info.peer = tok
	}
	return context.WithValue(ctx, connInfoKey{}, info)
}

func connInfoFrom(ctx context.Context) (connInfo, bool) {
	info, ok := ctx.Value(connInfoKey{}).(connInfo)
	return info, ok
}

// listenSocket binds path as a Unix socket at mode 0600, removing any stale
// file left by a prior process — the same convention bridge.NewBridgeServer
// and model_endpoint.go's ListenSocket use.
func listenSocket(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// ListenInternal binds the relay<->host socket. ServeInternal blocks serving
// it.
func (s *Server) ListenInternal() error {
	ln, err := listenSocket(s.cfg.InternalSocket)
	if err != nil {
		return fmt.Errorf("listen internal socket: %w", err)
	}
	s.internalLn = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/launch", s.handleLaunch)
	mux.HandleFunc("/terminate", s.handleTerminate)
	s.internalSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext:       captureConnInfo,
	}
	return nil
}

func (s *Server) ServeInternal() error {
	if s.internalLn == nil {
		return errors.New("hostapi: ListenInternal was not called")
	}
	if err := s.internalSrv.Serve(s.internalLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ListenHook binds the hook socket `relay-sessions hook` dials. ServeHook
// blocks serving it.
func (s *Server) ListenHook() error {
	ln, err := listenSocket(s.cfg.HookSocket)
	if err != nil {
		return fmt.Errorf("listen hook socket: %w", err)
	}
	s.hookLn = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/permission", s.handlePermission)
	s.hookSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext:       captureConnInfo,
	}
	return nil
}

func (s *Server) ServeHook() error {
	if s.hookLn == nil {
		return errors.New("hostapi: ListenHook was not called")
	}
	if err := s.hookSrv.Serve(s.hookLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// RegisterSessionForTest inserts a live session root directly, bypassing
// POST /launch entirely. Test-only seam (production code only ever
// populates the table from handleLaunch/watchRoot): it exists so a
// /permission test can exercise the real C3 membership.Resolve walk against
// a known root without needing a full shim+target process tree for every
// case, the same way the shim's own tests build fake process trees rather
// than driving relay's entire launch path.
func (s *Server) RegisterSessionForTest(id string, rootPID int) {
	info, ok := membership.NewSource().Info(rootPID)
	if !ok {
		info = membership.ProcInfo{}
	}
	s.table.put(&sessionEntry{id: id, state: stateLive, rootPID: rootPID, rootStart: info})
}

// Close shuts down both sockets. Safe to call even if only one was listened
// on.
func (s *Server) Close() {
	if s.internalSrv != nil {
		_ = s.internalSrv.Close()
	}
	if s.hookSrv != nil {
		_ = s.hookSrv.Close()
	}
}

// checkInternalPeer implements C5's "Host side" mutual check: the accepted
// connection's peer pid must equal relay_pid from this host's own Hello OK,
// and the bearer must match. Both are required; neither alone is enough.
func (s *Server) checkInternalPeer(r *http.Request) bool {
	info, ok := connInfoFrom(r.Context())
	if !ok || !info.peer.Valid() || int(info.peer.PID()) != s.cfg.RelayPID {
		return false
	}
	got := []byte(r.Header.Get("Authorization"))
	want := []byte("Bearer " + s.cfg.InternalBearer)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func writeErr(w http.ResponseWriter, code int, errCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: errCode, Message: message})
}

// handleLaunch implements C5's POST /launch. This unit's own scope stops at
// the API surface, the shim invocation and this bookkeeping layer: it has no
// provider-specific argv builder (R-S7b), no template resolution (R-S3), no
// sandbox profile generation (R-S8) and no real PTY host (R-S6) — so it
// requires the caller to supply a ready-to-run argv, and refuses pty and
// resume requests outright rather than pretending to support them.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// C5: "Otherwise 403 with an empty body." — checked before touching the
	// body at all, so a caller that fails this check learns nothing else.
	if !s.checkInternalPeer(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var req LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, err.Error())
		return
	}
	if req.V != 1 || req.SessionID == "" || req.Kind == "" {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, "v, session_id and kind are required")
		return
	}
	if req.PTY != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, "pty sessions need a real terminal host (R-S6), not implemented by this skeleton")
		return
	}
	if len(req.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, "argv is required: this skeleton has no provider argv builder (R-S7b)")
		return
	}
	if s.table.exists(req.SessionID) {
		writeErr(w, http.StatusConflict, ErrSessionExists, "session_id already launched")
		return
	}

	s.table.put(&sessionEntry{id: req.SessionID, state: stateLaunching})

	cmd, outcome, err := s.spawnShim(req.SessionID, req)
	if err != nil {
		// The shim process itself never started: nothing for reapShim to
		// wait on, so this is the one failure path that must mark the
		// session ended itself.
		s.table.markEnded(req.SessionID)
		writeErr(w, http.StatusInternalServerError, ErrSpawnFailed, err.Error())
		return
	}
	shimPID := cmd.Process.Pid
	go s.reapShim(req.SessionID, cmd)

	if outcome.identityRefused {
		_ = cmd.Process.Kill() // C5: "502 identity_refused (the host kills the shim)"
		writeErr(w, http.StatusBadGateway, ErrIdentityRefused, "the shim's Hello was refused")
		return
	}
	if !outcome.started || outcome.spawnFailed {
		writeErr(w, http.StatusInternalServerError, ErrSpawnFailed, fmt.Sprintf("target spawn failed (errno=%d)", outcome.spawnErrno))
		return
	}

	s.table.mu.Lock()
	if e, ok := s.table.byID[req.SessionID]; ok {
		e.state = stateLive
		e.shimPID = shimPID
		e.rootPID = outcome.rootPID
	}
	s.table.mu.Unlock()
	go s.watchRoot(req.SessionID, outcome.rootPID)

	body, _ := json.Marshal(map[string]any{"session_id": req.SessionID, "kind": req.Kind})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(LaunchResponse{
		SessionID: req.SessionID,
		RootPID:   outcome.rootPID,
		Body:      body,
	})
}

// handleTerminate implements C5's POST /terminate.
func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.checkInternalPeer(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var req TerminateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		if e, ok := s.table.get(req.SessionID); ok && e.shimPID != 0 {
			terminateShim(e.shimPID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePermission implements C6's `relay-sessions hook` subsection, host
// side: admit the call iff the peer is a C3 member of the named session's
// root, no bearer. The membership seam is real, not a placeholder: it is
// internal/membership.Resolve (C3, already landed on main as of this unit —
// see this package's doc comment on sessionTable) walked against this
// host's own sessionTable via rootsAdapter, exactly as the plan's C3 section
// names ("relay-sessions hook socket (against the host's own session
// table)"). What is NOT real yet is the policy decision itself: no
// PermissionManager is wired in (that needs a live provider session,
// R-S7b/R-S7c), so an admitted call gets a fixed placeholder decision.
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	info, ok := connInfoFrom(r.Context())
	if !ok || !info.peer.Valid() {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var body permissionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	resolvedID, memberOK := membership.Resolve(membership.NewSource(), rootsAdapter{s.table}, int(info.peer.PID()), info.acceptedAt)
	if !memberOK || resolvedID != body.SessionID {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(permissionResponseBody{
		Decision: "deny",
		Reason:   "session host skeleton: no policy engine wired yet (R-S7b/R-S7c)",
	})
}
