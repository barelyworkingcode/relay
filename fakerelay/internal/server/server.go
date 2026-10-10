package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/bridge"
	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/launch"
	"github.com/barelyworkingcode/relay/fakerelay/internal/peer"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const maxSocketPath = 103

// Server is one fakerelay instance.
type Server struct {
	dir  string
	out  io.Writer
	lock *os.File

	world  *world.World
	state  *state.Store
	log    *events.Log
	audit  *events.Audit
	clock  *SimClock
	gate   *presenceGate
	faults *faultSet
	launch *launch.Manager
	disp   *dispatcher

	sockMux, tcpMux, ctlMux *http.ServeMux
	bridgePath, frontPath   string
	ctlPath, readyPath      string

	mu         sync.Mutex
	patterns   map[string]bool
	relayPaths []string
	verbs      map[string]VerbHandler
	done       chan struct{}
}

// New takes the instance lock, loads the world and builds the instance. It
// binds nothing. Errors name the config dir, or the world file.
func New(dir string, out io.Writer) (*Server, error) {
	s := &Server{
		dir: dir, out: out, done: make(chan struct{}),
		patterns: map[string]bool{}, verbs: map[string]VerbHandler{}, disp: newDispatcher(),
		sockMux: http.NewServeMux(), tcpMux: http.NewServeMux(), ctlMux: http.NewServeMux(),
		bridgePath: filepath.Join(dir, "relay.sock"),
		frontPath:  filepath.Join(dir, fmt.Sprintf("relay-frontend-%d.sock", os.Getpid())),
		ctlPath:    filepath.Join(dir, "fakerelay-control.sock"),
		readyPath:  filepath.Join(dir, "ready.json"),
	}
	for _, p := range []string{s.bridgePath, s.frontPath, s.ctlPath} {
		if len(p) > maxSocketPath {
			return nil, fmt.Errorf("config dir %s is too long: socket %s is %d bytes and macOS allows %d; choose a shorter directory", dir, p, len(p), maxSocketPath)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w (config dir %s)", err, dir)
	}
	if err := s.acquire(); err != nil {
		return nil, err
	}
	if err := s.build(); err != nil {
		s.Close()
		var we *worldError
		if errors.As(err, &we) {
			return nil, we.err
		}
		return nil, fmt.Errorf("%w (config dir %s)", err, dir)
	}
	return s, nil
}

type worldError struct{ err error }

func (e *worldError) Error() string { return e.err.Error() }

func (s *Server) acquire() error {
	f, err := os.OpenFile(filepath.Join(s.dir, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("%w (config dir %s)", err, s.dir)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another relay server already owns this configuration directory (%s)", s.dir)
		}
		return fmt.Errorf("%w (config dir %s)", err, s.dir)
	}
	s.lock = f
	return nil
}

func (s *Server) build() error {
	_ = os.Remove(s.readyPath)
	w, err := world.Load(s.dir)
	if err != nil {
		return &worldError{err}
	}
	s.world = w
	if s.log, err = events.NewLog(s.dir); err != nil {
		return err
	}
	if s.audit, err = events.NewAudit(s.dir); err != nil {
		return err
	}
	if s.state, err = state.New(w, s.dir); err != nil {
		return err
	}
	self, _ := os.Executable()
	s.clock = NewClock()
	s.gate = newPresenceGate(w.Presence, s.log, s.audit)
	s.faults = newFaultSet(s.log, s.knownRoute)
	s.launch = launch.New(launch.Config{Dir: s.dir, BridgeSocket: s.bridgePath, FrontendSocket: s.frontPath, Self: self, Events: s.log})
	s.launch.OnEnd(func(id string) { s.disp.forget(id, 0) })
	for _, svc := range w.Services {
		s.launch.Add(svc)
	}
	s.Route(ClassProxy, "/", s.dispatch)
	s.controlRoutes()
	return nil
}

// Registrar is the door installer for domain packages.
func (s *Server) Registrar() Registrar { return s }

// Deps is what domain packages receive.
func (s *Server) Deps() Deps {
	return Deps{Dir: s.dir, World: s.world, State: s.state, Events: s.log, Audit: s.audit, Presence: s.gate, Clock: s.clock}
}

// Launcher is the service manager, for the service verbs.
func (s *Server) Launcher() *launch.Manager { return s.launch }

// Close releases the instance lock and the log files.
func (s *Server) Close() {
	if s.log != nil {
		s.log.Close()
	}
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
}

type readyFile struct {
	Schema    int               `json:"schema"`
	PID       int               `json:"pid"`
	Version   string            `json:"version"`
	ConfigDir string            `json:"config_dir"`
	Sockets   map[string]string `json:"sockets"`
	Listeners map[string]string `json:"listeners"`
}

// Run binds every socket and listener, starts the autostart services, writes
// ready.json, prints its path and serves until ctx ends.
func (s *Server) Run(ctx context.Context) (err error) {
	defer s.Close()
	var closers []func()
	var wg sync.WaitGroup
	stop := func() {
		_ = os.Remove(s.readyPath)
		if s.launch != nil {
			s.launch.StopAll()
		}
		close(s.done)
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		wg.Wait()
	}
	defer func() {
		if err != nil {
			stop()
			var we *worldError
			if errors.As(err, &we) {
				err = we.err
			} else {
				err = fmt.Errorf("%w (config dir %s)", err, s.dir)
			}
		}
	}()

	bl, err := listenUnix(s.bridgePath)
	if err != nil {
		return err
	}
	// Every bound listener is registered at once so a later bind failure
	// closes the earlier ones.
	closers = append(closers, func() { os.Remove(s.bridgePath); os.Remove(s.frontPath); os.Remove(s.ctlPath) })
	closers = append(closers, func() { bl.Close() })
	fl, err := listenUnix(s.frontPath)
	if err != nil {
		return err
	}
	closers = append(closers, func() { fl.Close() })
	cl, err := listenUnix(s.ctlPath)
	if err != nil {
		return err
	}
	closers = append(closers, func() { cl.Close() })

	ready := readyFile{Schema: 1, PID: os.Getpid(), Version: "fakerelay", ConfigDir: s.dir,
		Sockets:   map[string]string{"bridge": s.bridgePath, "frontend": s.frontPath, "control": s.ctlPath},
		Listeners: map[string]string{}}
	frontend := &http.Server{Handler: s.sockMux, ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			pid, _ := peer.PID(c)
			return context.WithValue(ctx, pidKey{}, pid)
		}}
	servers := []struct {
		srv *http.Server
		l   net.Listener
	}{{frontend, fl}, {&http.Server{Handler: s.ctlMux, ReadHeaderTimeout: 10 * time.Second}, cl}}
	for _, t := range []struct{ key, addr string }{{"api", s.world.Listeners.API}, {"model", s.world.Listeners.Model}} {
		if t.addr == "" {
			continue
		}
		l, err := net.Listen("tcp", t.addr)
		if err != nil {
			return fmt.Errorf("listener %s: %w", t.key, err)
		}
		closers = append(closers, func() { l.Close() })
		h := http.Handler(s.tcpMux)
		if t.key == "model" {
			h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				WriteError(w, http.StatusNotFound, "the model endpoint is not faked")
			})
		}
		servers = append(servers, struct {
			srv *http.Server
			l   net.Listener
		}{&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}, l})
		ready.Listeners[t.key] = l.Addr().String()
	}
	for _, t := range servers {
		closers = append(closers, func() { t.srv.Close() })
		wg.Add(1)
		go func() { defer wg.Done(); _ = t.srv.Serve(t.l) }()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		bridge.Serve(bl, bridge.Hooks{Events: s.log, RelayPID: os.Getpid(), Launch: s.launch,
			Fault: s.faults.bridgeFault,
			Register: func(id string, m bridge.Manifest, sock, tok string) (func(), error) {
				s.mu.Lock()
				served := append([]string(nil), s.relayPaths...)
				s.mu.Unlock()
				return s.disp.register(id, m, sock, tok, served)
			}})
	}()

	for i, f := range s.world.Faults {
		if _, err := s.faults.Add(f); err != nil {
			return &worldError{fmt.Errorf("world.json: faults[%d]: %w", i, err)}
		}
	}
	for _, row := range s.world.Audit {
		if err := s.audit.Append(row); err != nil {
			return err
		}
	}
	if err := s.launch.StartAutostart(); err != nil {
		return err
	}
	if err := writeReady(s.readyPath, ready); err != nil {
		return err
	}
	fmt.Fprintln(s.out, s.readyPath)
	s.log.Begin(context.Background(), "server.ready").Set("config_dir", s.dir).Set("ready_file", s.readyPath).Set("pid", os.Getpid()).End("ok", "", nil)

	<-ctx.Done()
	stop()
	return nil
}

func listenUnix(path string) (net.Listener, error) {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func writeReady(path string, r readyFile) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
