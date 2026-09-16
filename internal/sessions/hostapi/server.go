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
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// Config is what a running relay-sessions host needs to serve C5's internal
// API and C6's hook socket. RelayPID is fixed at construction: it comes from
// this host's own Hello to relay's bridge (kind "service", the existing,
// unmodified bridge.SendHello — see cmd/relaysessions/main.go), done once at
// startup. A relay restart mid-life is out of scope.
type Config struct {
	InternalSocket string // relay dials this; POST /launch, POST /terminate
	InternalBearer string // the bearer relay must present, per its own RegisterManifest
	RelayPID       int    // relay's pid, from this host's own Hello OK
	HookSocket     string // `relay-sessions hook` dials this; POST /permission
}

// Server is relay-sessions' internal API host: a thin dispatcher over
// Terminals/Sessions (types.go's package doc explains why it owns no
// spawning of its own anymore). Its session table now exists only to answer
// /permission's C3 membership walk for pty sessions and to recall a
// terminal session's root pid at exit time (session_table.go's doc
// comment).
type Server struct {
	cfg       Config
	table     *sessionTable
	terminals *terminal.Manager
	sessions  *session.Manager

	exitMu      sync.Mutex
	exitHandler func(id string, rootPID, exitCode int, reason string)
	terminating map[string]bool // session ids this host's own /terminate is closing; read/cleared by the exit callback to tell "closed" apart from an unrelated "exit"

	internalLn  net.Listener
	internalSrv *http.Server
	hookLn      net.Listener
	hookSrv     *http.Server
}

// New constructs a Server dispatching onto terminals (pty) and sessions
// (claude/pi/chat). Call ListenInternal/ListenHook then
// ServeInternal/ServeHook (typically each in its own goroutine).
func New(cfg Config, terminals *terminal.Manager, sessions *session.Manager) *Server {
	s := &Server{
		cfg:         cfg,
		table:       newSessionTable(),
		terminals:   terminals,
		sessions:    sessions,
		terminating: make(map[string]bool),
	}
	terminals.SetExitHandler(s.onTerminalExit)
	sessions.SetExitHandler(s.onSessionExit)
	return s
}

// SetExitHandler installs fn to be called, on its own goroutine, whenever a
// session this server dispatched to exits — the hook cmd/relaysessions uses
// to send C5's SessionExited bridge report. rootPID is 0 for a claude/pi/
// chat session (types.go's package doc: no provider.Provider pid is
// exposed to key one on); reason is "closed" when this host's own
// /terminate caused the exit, "exit" otherwise — C5 also names "idle" and
// "deleted", neither reachable here yet: idle-close is driven by
// NotifyViewerChange, which nothing calls without the eve-facing manifest
// surface this unit does not mount (types.go's package doc), and "deleted"
// is session.Manager.DeleteSession, a path /terminate never takes (it only
// ever calls EndSession, C5's own "SIGTERM ... SIGKILL" — not a data wipe).
func (s *Server) SetExitHandler(fn func(id string, rootPID, exitCode int, reason string)) {
	s.exitMu.Lock()
	s.exitHandler = fn
	s.exitMu.Unlock()
}

func (s *Server) reportExit(id string, rootPID, exitCode int, reason string) {
	s.exitMu.Lock()
	fn := s.exitHandler
	s.exitMu.Unlock()
	if fn != nil {
		fn(id, rootPID, exitCode, reason)
	}
}

func (s *Server) markTerminating(id string) {
	s.exitMu.Lock()
	s.terminating[id] = true
	s.exitMu.Unlock()
}

// markTerminatingIfAlive calls alive and, iff it reports true, marks id
// terminating — both under the same exitMu critical section consumeTerminating
// itself locks, so a concurrent exit report for id can never observe a state
// between "checked alive" and "marked" (handleTerminate's own doc comment on
// why that gap matters).
func (s *Server) markTerminatingIfAlive(id string, alive func() bool) {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	if alive() {
		s.terminating[id] = true
	}
}

// consumeTerminating reports and clears whether id's exit was caused by this
// host's own /terminate, so a later exit of a same-named session (impossible
// in practice — ids are relay-minted UUIDs — but not this function's job to
// assume) never reads a stale entry.
func (s *Server) consumeTerminating(id string) bool {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	closed := s.terminating[id]
	delete(s.terminating, id)
	return closed
}

// onTerminalExit is terminal.Manager's exit hook: read this session's shim
// pid from the table before marking it ended (session_table.go's markEnded
// removes the byRootPID entry, not the byID one, so table.get still answers
// after this — but reading first, not after, is what keeps this correct
// even if a later change ever made markEnded remove byID too).
func (s *Server) onTerminalExit(id string, exitCode int) {
	rootPID := 0
	if e, ok := s.table.get(id); ok {
		rootPID = e.shimPID
	}
	s.table.markEnded(id)
	reason := "exit"
	if s.consumeTerminating(id) {
		reason = "closed"
	}
	s.reportExit(id, rootPID, exitCode, reason)
}

// onSessionExit is session.Manager's exit hook. No table entry exists for a
// provider-hosted session (New's own doc comment), so there is nothing to
// look up beyond the id and exit code the callback already carries.
func (s *Server) onSessionExit(id string, exitCode int) {
	reason := "exit"
	if s.consumeTerminating(id) {
		reason = "closed"
	}
	s.reportExit(id, 0, exitCode, reason)
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

// Close shuts down both sockets and every live session this server
// dispatched to. Safe to call even if only one socket was listened on.
// Sockets close first: a relay or hook peer mid-request against a socket
// that's about to disappear underneath it should see the connection drop,
// not a session teardown it raced against being interrupted by that drop.
func (s *Server) Close() {
	if s.internalSrv != nil {
		_ = s.internalSrv.Close()
	}
	if s.hookSrv != nil {
		_ = s.hookSrv.Close()
	}
	s.terminals.StopAll()
	s.sessions.StopAll()
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

// handleLaunch implements C5's POST /launch: peer-check, decode, route by
// kind to terminal.Manager or session.Manager, translate whichever Session
// comes back into C5's 201 body. Neither manager is spawned by this method —
// each owns its own shim-spawn (or direct-spawn) and Hello-wait mechanics
// end to end (types.go's package doc), so this handler's only remaining job
// once the spec is built is to call Create and map the result.
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

	switch {
	case req.Kind == kindPTY:
		s.launchTerminal(w, req)
	case isProviderKind(req.Kind):
		s.launchSession(w, req)
	default:
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, fmt.Sprintf("kind %q is not one of pty, claude, pi, chat", req.Kind))
	}
}

func (s *Server) launchTerminal(w http.ResponseWriter, req LaunchRequest) {
	spec, err := buildTerminalSpec(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, err.Error())
		return
	}
	sess, err := s.terminals.Create(spec)
	if err != nil {
		status, code := terminalLaunchStatus(err)
		writeErr(w, status, code, err.Error())
		return
	}

	rootPID := sess.RootPID()
	// SH §4.2: the shim is this session's root, matching what relay's own
	// launch identity binds to (whoever said Hello). Registered as already
	// stateLive in one step (put, not put-then-setLive): terminal.Manager's
	// own Create has already blocked until the session is fully live or
	// returned an error, so there is no "launching" window left for this
	// table to observe by the time it ever sees this id.
	//
	// A failed Info read here (the shim exited in the narrow window between
	// Create returning and this line) is not papered over with a zero-value
	// rootStart: a half-set root is exactly what a hostile racing /permission
	// call could match by accident of a zero start time, so this session is
	// torn down and reported failed instead of
	// published with a membership root nothing can actually verify.
	rootInfo, ok := membership.NewSource().Info(rootPID)
	if !ok {
		s.terminals.Close(req.SessionID)
		writeErr(w, http.StatusInternalServerError, ErrSpawnFailed, "shim exited before its start time could be read")
		return
	}
	s.table.put(&sessionEntry{id: req.SessionID, state: stateLive, shimPID: rootPID, rootStart: rootInfo})
	// The shim can exit in the gap between Create returning and the line
	// above — Info succeeding is not a guarantee it is still alive by the
	// time this table entry is published. Left uncorrected, that entry
	// would sit at stateLive forever: nothing else ever re-checks it, and
	// onTerminalExit only runs on a future exit event this already-past one
	// will never produce.
	if !sess.Alive() {
		s.table.markEnded(req.SessionID)
	}

	body, _ := json.Marshal(sess.CreatedBody())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(LaunchResponse{SessionID: req.SessionID, RootPID: rootPID, Body: body})
}

func (s *Server) launchSession(w http.ResponseWriter, req LaunchRequest) {
	spec, err := buildSessionSpec(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, err.Error())
		return
	}
	sess, err := s.sessions.Create(spec)
	if err != nil {
		status, code := sessionLaunchStatus(err)
		writeErr(w, status, code, err.Error())
		return
	}

	// No membership table entry: a provider-hosted session's PreToolUse hook
	// authenticates with a hook token, not process ancestry (types.go's
	// package doc), and provider.Provider exposes no pid this handler could
	// register as a root even if it wanted to — root_pid is 0 here, same as
	// SessionExited's for this kind (server.go's SetExitHandler doc comment).
	body, err := json.Marshal(sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, ErrSpawnFailed, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(LaunchResponse{SessionID: req.SessionID, RootPID: 0, Body: body})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.SessionID != "" {
		// Which manager owns req.SessionID is resolved by asking each in
		// turn, not by a third table recording it redundantly: terminal.
		// Manager and session.Manager already are the live set each owns,
		// and an id landing in neither (unknown, or still in its own
		// Create's launching window) is the same no-op /terminate always
		// was for an unrecognized id.
		if term, ok := s.terminals.Get(req.SessionID); ok {
			// The aliveness check and the mark it gates run under one exitMu
			// critical section (markTerminatingIfAlive), the same lock
			// consumeTerminating uses: mark only when a live process
			// actually exists to be killed (if the terminal already exited
			// naturally, Close is a no-op — waitForExit already ran once and
			// will not run again, so onTerminalExit will never fire to
			// consume the mark, and an unconsumed mark is a permanent, if
			// tiny, leak in a long-lived host process), with no gap between
			// the check and the write for a racing onTerminalExit to land in.
			s.markTerminatingIfAlive(req.SessionID, term.Alive)
			s.terminals.Close(req.SessionID)
		} else if sess, ok := s.sessions.Get(req.SessionID); ok {
			s.markTerminatingIfAlive(req.SessionID, func() bool {
				p := sess.Provider()
				return p != nil && p.Alive()
			})
			s.sessions.EndSession(req.SessionID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxPermissionBodyBytes bounds a /permission body. A real tool_input is
// small (a shell command, an edit's arguments); this is generous headroom,
// not a tuned limit — a later unit can tighten it once real tool payloads
// are observed.
const maxPermissionBodyBytes = 1 << 20 // 1 MiB

// handlePermission implements C6's `relay-sessions hook` subsection, host
// side: admit the call iff the peer is a C3 member of the named session's
// root, no bearer. The membership seam is real, not a placeholder: it is
// internal/membership.Resolve (C3, already landed on main as of this unit —
// see this package's doc comment on sessionTable) walked against this
// host's own sessionTable via rootsAdapter, exactly as the plan's C3 section
// names ("relay-sessions hook socket (against the host's own session
// table)"). What is NOT real yet is the policy decision itself: no
// PermissionManager is wired in, so an admitted call gets a fixed
// placeholder decision.
//
// Membership is resolved before the body is ever read: the peer pid and
// accept time needed for Resolve both come from the connection itself
// (connInfo), not the request body, so a caller that fails the membership
// check is refused without this handler decoding a single byte it sent —
// hook.sock is 0600 but reachable by any same-uid process, including the
// sandboxed session target, so an unauthenticated caller must not be able
// to drive unbounded JSON-decode allocation here the way it could if the
// body were decoded first.
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

	resolvedID, memberOK := membership.Resolve(membership.NewSource(), rootsAdapter{s.table}, int(info.peer.PID()), info.acceptedAt)
	if !memberOK {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPermissionBodyBytes)
	var body permissionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if resolvedID != body.SessionID {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(permissionResponseBody{
		Decision: "deny",
		Reason:   "session host: no policy engine wired yet",
	})
}
