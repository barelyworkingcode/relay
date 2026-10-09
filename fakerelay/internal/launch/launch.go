// Package launch starts services the way relay does: a single-use secret on
// fd 3, a Hello that binds it to the peer pid, and an identity that ends with
// the process.
package launch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// StopGrace is how long a stopped service has after SIGTERM before SIGKILL.
const StopGrace = 10 * time.Second

// Identity is what a bound launch is known as.
type Identity struct {
	ServiceID    string
	Capabilities []string
}

// Has reports whether the identity holds a capability.
func (i Identity) Has(c string) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Status is one row of service list.
type Status struct {
	ID, Name     string
	Capabilities []string
	State        string
}

// Config is what the manager needs to know about the instance.
type Config struct {
	Dir, BridgeSocket, FrontendSocket, Self string
	Events                                  *events.Log
}

// Manager owns the service records, their launches and the identity table.
type Manager struct {
	cfg    Config
	mu     sync.Mutex
	recs   map[string]*record
	order  []string
	idents map[int]Identity
	onEnd  func(serviceID string)
}

type record struct {
	rec      world.Service
	cur      *run
	failed   bool
	exitCode int
}

type run struct {
	hash     [32]byte
	spent    bool
	pid      int
	cmd      *exec.Cmd
	done     chan struct{}
	stopping bool
}

func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, recs: map[string]*record{}, idents: map[int]Identity{}}
}

// OnEnd registers fn to run, outside the lock, whenever a launch ends.
func (m *Manager) OnEnd(fn func(serviceID string)) { m.onEnd = fn }

// Add registers a service record.
func (m *Manager) Add(rec world.Service) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[rec.ID]; !ok {
		m.order = append(m.order, rec.ID)
	}
	m.recs[rec.ID] = &record{rec: rec}
}

// Find resolves a record by id, or by name when the id matches nothing.
func (m *Manager) Find(id, name string) (world.Service, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.order {
		r := m.recs[k].rec
		if (id != "" && r.ID == id) || (id == "" && name != "" && r.Name == name) {
			return r, true
		}
	}
	return world.Service{}, false
}

// List returns every record with its supervision state.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for _, k := range m.order {
		r := m.recs[k]
		st := "-"
		if r.cur != nil {
			st = "running"
		} else if r.failed {
			st = fmt.Sprintf("failed (exit %d)", r.exitCode)
		}
		out = append(out, Status{r.rec.ID, r.rec.Name, r.rec.Capabilities, st})
	}
	return out
}

// StartAutostart starts every record marked autostart.
func (m *Manager) StartAutostart() error {
	m.mu.Lock()
	var ids []string
	for _, k := range m.order {
		if m.recs[k].rec.Autostart {
			ids = append(ids, k)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.Start(id); err != nil {
			return err
		}
	}
	return nil
}

// Start begins a launch with a fresh secret. It ends any earlier launch of the
// same record first.
func (m *Manager) Start(id string) error {
	m.Stop(id)
	m.mu.Lock()
	r := m.recs[id]
	if r == nil {
		m.mu.Unlock()
		return fmt.Errorf("no service %q", id)
	}
	secret, hash, err := newSecret()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	cmd, closeFiles, err := m.command(r.rec, secret)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("service %s: %w", id, err)
	}
	if err := cmd.Start(); err != nil {
		closeFiles()
		m.mu.Unlock()
		return fmt.Errorf("service %s: start: %w", id, err)
	}
	closeFiles()
	cur := &run{hash: hash, cmd: cmd, done: make(chan struct{})}
	r.cur, r.failed = cur, false
	m.mu.Unlock()
	m.state(id, "running", 0, nil)
	go m.wait(id, cur)
	return nil
}

func (m *Manager) wait(id string, cur *run) {
	_ = cur.cmd.Wait()
	code := cur.cmd.ProcessState.ExitCode()
	m.mu.Lock()
	r := m.recs[id]
	if r.cur == cur {
		r.cur = nil
	}
	for pid, ident := range m.idents {
		if pid == cur.pid && ident.ServiceID == id {
			delete(m.idents, pid)
		}
	}
	unrequested := !cur.stopping
	if unrequested {
		r.failed, r.exitCode = true, code
	}
	m.mu.Unlock()
	if m.onEnd != nil {
		m.onEnd(id)
	}
	if unrequested {
		m.state(id, "failed", 1, &code)
	}
	close(cur.done)
}

func (m *Manager) state(id, phase string, attempt int, code *int) {
	e := m.cfg.Events.Begin(context.Background(), "service.state").Set("service_id", id).Set("phase", phase).Set("attempt", attempt)
	if code != nil {
		e.Set("exit_code", *code)
	}
	e.End("ok", "", nil)
}

// Stop ends a running launch: SIGTERM, then SIGKILL after StopGrace. It
// returns once the process is gone.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	r := m.recs[id]
	var cur *run
	if r != nil {
		cur = r.cur
	}
	if cur != nil {
		cur.stopping = true
	}
	m.mu.Unlock()
	if cur == nil {
		return
	}
	_ = cur.cmd.Process.Signal(syscall.SIGTERM)
	t := time.NewTimer(StopGrace)
	defer t.Stop()
	select {
	case <-cur.done:
	case <-t.C:
		_ = cur.cmd.Process.Kill()
		<-cur.done
	}
}

// StopAll stops every launch at once.
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); m.Stop(id) }()
	}
	wg.Wait()
}

// Hello binds the live launch named name to pid when secret is its secret.
// Every refusal looks the same to the caller; a wrong secret spends nothing.
func (m *Manager) Hello(name, secret string, pid int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.recs[name]
	if pid <= 0 || r == nil || r.cur == nil || r.cur.spent {
		return false
	}
	if _, held := m.idents[pid]; held {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(sum[:], r.cur.hash[:]) != 1 {
		return false
	}
	r.cur.spent, r.cur.pid = true, pid
	m.idents[pid] = Identity{ServiceID: name, Capabilities: append([]string(nil), r.rec.Capabilities...)}
	return true
}

// IdentityByPID returns the identity bound to pid. The bind is to the pid
// alone: a recycled pid is only possible after the launch ended, and the
// identity is deleted then.
func (m *Manager) IdentityByPID(pid int) (Identity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.idents[pid]
	return i, ok
}

func newSecret() (string, [32]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", [32]byte{}, err
	}
	s := hex.EncodeToString(b)
	return s, sha256.Sum256([]byte(s)), nil
}

var removedEnv = map[string]bool{"RELAY_SERVICE_TOKEN": true, "RELAY_MCP_TOKEN": true, "RELAY_FRONTEND_TOKEN": true, "RELAY_LAUNCH_FD": true, "RELAY_FRONTEND_SOCKET": true}

// command builds the exec.Cmd with the launch fd, the environment and the log.
func (m *Manager) command(rec world.Service, secret string) (*exec.Cmd, func(), error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	if _, err := pw.WriteString(secret); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	pw.Close()
	logf, err := os.OpenFile(filepath.Join(m.cfg.Dir, "logs", rec.ID+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		pr.Close()
		return nil, nil, err
	}
	cmd := exec.Command(rec.Command, rec.Args...)
	cmd.Dir = rec.WorkingDir
	cmd.Env = m.env(rec)
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Stdout, cmd.Stderr = logf, logf
	setProcAttr(cmd)
	return cmd, func() { pr.Close(); logf.Close() }, nil
}

func (m *Manager) env(rec world.Service) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				if !removedEnv[kv[:i]] {
					env[kv[:i]] = kv[i+1:]
				}
				break
			}
		}
	}
	for k, v := range rec.Env {
		env[k] = v
	}
	env["RELAY_BRIDGE_SOCKET"] = m.cfg.BridgeSocket
	env["RELAY_SERVICE_ID"] = rec.ID
	env["RELAY_LAUNCH_FD"] = "3"
	env["RELAY_CONFIG_DIR"] = m.cfg.Dir
	env["RELAY_MCP_COMMAND"] = m.cfg.Self
	delete(env, "RELAY_FRONTEND_SOCKET")
	for _, c := range rec.Capabilities {
		if c == "frontend" {
			env["RELAY_FRONTEND_SOCKET"] = m.cfg.FrontendSocket
		}
	}
	for _, k := range []string{"RELAY_SERVICE_TOKEN", "RELAY_MCP_TOKEN", "RELAY_FRONTEND_TOKEN"} {
		delete(env, k)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
