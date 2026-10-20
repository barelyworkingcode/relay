package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
)

// Phase is what LocalStatus reports about a generation.
type Phase string

const (
	PhaseNone   Phase = "none"   // no generation exists
	PhaseAlive  Phase = "alive"  // the runner is running
	PhaseExited Phase = "exited" // the agent exited; Status.Code is its code
	PhaseDead   Phase = "dead"   // the runner is gone and wrote no exit file
	PhaseLegacy Phase = "legacy" // a live runner from before runner.json (hosts only)
)

// State is runner.json.
type State struct {
	V          int    `json:"v"`
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	Started    string `json:"started"`
	Session    string `json:"session"`
	Generation string `json:"generation"`
	Delivered  string `json:"delivered"`
}

// Status is one generation's phase and, while it is known, its runner state.
type Status struct {
	Phase      Phase
	Generation string
	Code       int
	State      State
}

// LocalSpec describes the agent a local runner starts.
type LocalSpec struct {
	Argv       []string   // the shim argv, or claude's when unshimmed
	Env        []string   // the agent's environment; the runner gets the same
	Dir        string     // the agent's working folder
	ExtraFiles []*os.File // fds 3, 4, 5 as the shim expects; the caller closes its copies
	Patterns   Patterns
}

// LocalRun names one generation's run directory on this machine.
type LocalRun struct{ Base, Session, Generation string }

var (
	ErrNotRunning = errors.New("runner: the agent is not running")
	ErrNoReceipt  = errors.New("runner: no receipt within the wait; the message stays queued")
)

const (
	// messageEvent is a relay-written event request; it reaches no agent.
	messageEvent MessageKind = "evt"

	receiptWait      = 25 * time.Second
	receiptPoll      = 20 * time.Millisecond
	startWait        = 10 * time.Second
	stopStep         = 100 * time.Millisecond
	stopWait         = 3 * time.Second
	statusEveryPolls = 50
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// processUmask is read once, before any goroutine runs, because reading it
// means setting it.
var processUmask = func() int {
	m := syscall.Umask(0o77)
	syscall.Umask(m)
	return m
}()

func checkID(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("runner: invalid %s id %q", kind, id)
	}
	return nil
}

func latestGeneration(sessionDir string) string {
	entries, _ := os.ReadDir(sessionDir)
	best := 0
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "g"))
		if e.IsDir() && strings.HasPrefix(e.Name(), "g") && err == nil && n > best {
			best = n
		}
	}
	if best == 0 {
		return ""
	}
	return "g" + strconv.Itoa(best)
}

// Dir is the generation's run directory.
func (r LocalRun) Dir() string { return filepath.Join(r.Base, r.Session, r.Generation) }

// AllocLocal creates the session's next generation directory (mode 0700)
// without starting anything, so a caller can put files in it first.
func AllocLocal(base, session string) (LocalRun, error) {
	if err := checkID("session", session); err != nil {
		return LocalRun{}, err
	}
	sessionDir := filepath.Join(base, session)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return LocalRun{}, fmt.Errorf("runner: create %s: %w", sessionDir, err)
	}
	for range 20 {
		n := 0
		if cur := latestGeneration(sessionDir); cur != "" {
			n, _ = strconv.Atoi(cur[1:])
		}
		gen := "g" + strconv.Itoa(n+1)
		err := os.Mkdir(filepath.Join(sessionDir, gen), 0o700)
		if err == nil {
			return LocalRun{Base: base, Session: session, Generation: gen}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return LocalRun{}, fmt.Errorf("runner: create run directory: %w", err)
		}
	}
	return LocalRun{}, fmt.Errorf("runner: could not allocate a generation for session %s", session)
}

// StartLocal allocates the next generation, starts the runner in its own
// session, reaps it in a goroutine, and returns once runner.json exists.
func StartLocal(base, session string, spec LocalSpec) (LocalRun, State, error) {
	run, err := AllocLocal(base, session)
	if err != nil {
		return LocalRun{}, State{}, err
	}
	st, err := run.Start(spec)
	if err != nil {
		_ = os.RemoveAll(run.Dir())
		return LocalRun{}, State{}, err
	}
	return run, st, nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Start writes the runner and the agent's launch script into the directory
// AllocLocal made and starts the runner.
func (r LocalRun) Start(spec LocalSpec) (State, error) {
	if len(spec.Argv) == 0 {
		return State{}, errors.New("runner: no agent command")
	}
	dir := r.Dir()
	for _, d := range []string{InboxDir, SentDir} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			return State{}, fmt.Errorf("runner: create %s: %w", d, err)
		}
	}
	for _, f := range []string{OutFile, EventsFile, StderrFile} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return State{}, fmt.Errorf("runner: create %s: %w", f, err)
		}
	}
	var agent strings.Builder
	if spec.Dir != "" {
		agent.WriteString("cd " + shQuote(spec.Dir) + " || exit 126\n")
	}
	agent.WriteString("exec")
	for _, a := range spec.Argv {
		agent.WriteString(" " + shQuote(a))
	}
	agent.WriteString("\n")
	if err := os.WriteFile(filepath.Join(dir, "agent.sh"), []byte(agent.String()), 0o600); err != nil {
		return State{}, fmt.Errorf("runner: write agent script: %w", err)
	}
	script := filepath.Join(dir, "runner.sh")
	if err := os.WriteFile(script, []byte(Script), 0o600); err != nil {
		return State{}, fmt.Errorf("runner: write runner: %w", err)
	}

	cmd := exec.Command("/bin/sh", script, dir, fmt.Sprintf("%04o", processUmask), r.Session, r.Generation, spec.Patterns.TurnEnded, spec.Patterns.Asking)
	cmd.Env = spec.Env
	cmd.ExtraFiles = spec.ExtraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return State{}, fmt.Errorf("runner: start: %w", err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()

	// Deliberate: runner.json is the only completion signal the runner gives,
	// so this polls for it; a runner that dies first is seen through reaped.
	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if st, ok := readState(dir); ok {
			// The runner is the process cmd.Start returned: it is not forked
			// from, so runner.json's pid and this one are the same.
			st.PID = cmd.Process.Pid
			return st, nil
		}
		select {
		case <-reaped:
			if st, ok := readState(dir); ok {
				return st, nil
			}
			return State{}, fmt.Errorf("runner: the runner exited before it wrote %s", StateFile)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return State{}, fmt.Errorf("runner: the runner wrote no %s within %s", StateFile, startWait)
		case <-tick.C:
		}
	}
}

func readState(dir string) (State, bool) {
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	var st State
	if err != nil || json.Unmarshal(data, &st) != nil || st.PID <= 0 {
		return State{}, false
	}
	return st, true
}

// startTime is ps's start time of pid in the form the runner records.
func startTime(pid int) string {
	// Same form as the runner's `ps -o lstart=` with runs of blanks squeezed,
	// read without forking: lstart prints the kernel's start second in the
	// local zone.
	if info, ok := membership.NewSource().Info(pid); ok {
		return time.Unix(info.StartSec, 0).Format("Mon Jan 2 15:04:05 2006")
	}
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func runnerAlive(st State) bool {
	if st.PID <= 0 || st.Started == "" || syscall.Kill(st.PID, 0) == syscall.ESRCH {
		return false
	}
	return startTime(st.PID) == st.Started
}

// LocalStatus reports one generation of session; generation "" is the latest.
func LocalStatus(base, session, generation string) (Status, error) {
	if err := checkID("session", session); err != nil {
		return Status{}, err
	}
	if generation != "" {
		if err := checkID("generation", generation); err != nil {
			return Status{}, err
		}
	}
	sessionDir := filepath.Join(base, session)
	if generation == "" {
		generation = latestGeneration(sessionDir)
	}
	if generation == "" {
		return Status{Phase: PhaseNone}, nil
	}
	dir := filepath.Join(sessionDir, generation)
	if _, err := os.Stat(dir); err != nil {
		return Status{Phase: PhaseNone}, nil
	}
	st := Status{Generation: generation}
	st.State, _ = readState(dir)
	if data, err := os.ReadFile(filepath.Join(dir, ExitFile)); err == nil {
		var exit struct {
			Code int `json:"code"`
		}
		code := 1
		if json.Unmarshal(data, &exit) == nil {
			code = exit.Code
		}
		st.Phase, st.Code = PhaseExited, code
		return st, nil
	}
	if runnerAlive(st.State) {
		st.Phase = PhaseAlive
	} else {
		st.Phase = PhaseDead
	}
	return st, nil
}

func (r LocalRun) deliver(ctx context.Context, kind MessageKind, body []byte) error {
	dir := r.Dir()
	if st, err := LocalStatus(r.Base, r.Session, r.Generation); err != nil || st.Phase != PhaseAlive {
		return ErrNotRunning
	}
	id := NewMessageID(time.Now())
	name := id + "." + string(kind)
	tmp := filepath.Join(dir, InboxDir, "."+id+".tmp")
	line := append([]byte(strings.TrimRight(string(body), "\n")), '\n')
	if err := os.WriteFile(tmp, line, 0o600); err != nil {
		return fmt.Errorf("runner: queue message: %w", err)
	}
	queued := filepath.Join(dir, InboxDir, name)
	if err := os.Rename(tmp, queued); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runner: queue message: %w", err)
	}
	received := func() bool { _, err := os.Stat(filepath.Join(dir, SentDir, name)); return err == nil }

	deadline := time.NewTimer(receiptWait)
	defer deadline.Stop()
	tick := time.NewTicker(receiptPoll)
	defer tick.Stop()
	for polls := 1; ; polls++ {
		select {
		case <-ctx.Done():
			if received() {
				return nil
			}
			return ErrNoReceipt
		case <-deadline.C:
			if received() {
				return nil
			}
			return ErrNoReceipt
		case <-tick.C:
		}
		if received() {
			return nil
		}
		_, exited := os.Stat(filepath.Join(dir, ExitFile))
		if exited == nil || polls%statusEveryPolls == 0 {
			if st, err := LocalStatus(r.Base, r.Session, r.Generation); exited == nil || (err == nil && st.Phase != PhaseAlive) {
				if received() {
					return nil
				}
				_ = os.Remove(queued)
				return ErrNotRunning
			}
		}
	}
}

// Send queues one agent line and returns after the runner moved it to sent/.
func (r LocalRun) Send(ctx context.Context, kind MessageKind, line []byte) error {
	if kind != MessageTurn && kind != MessageControl {
		return fmt.Errorf("runner: invalid message kind %q", kind)
	}
	return r.deliver(ctx, kind, line)
}

// SendEvent asks the runner to write an event line; it writes nothing to the
// agent. Only "asking" is defined.
func (r LocalRun) SendEvent(ctx context.Context, typ string) error {
	if typ != "asking" {
		return fmt.Errorf("runner: invalid event type %q", typ)
	}
	return r.deliver(ctx, messageEvent, []byte(`{"type":"asking"}`))
}

// Open opens out.jsonl positioned at offset, a line start.
func (r LocalRun) Open(offset int64) (*os.File, error) {
	if err := checkID("generation", r.Generation); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(r.Dir(), OutFile))
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// sessionPIDs lists the processes whose session id is sid.
func sessionPIDs(sid int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		if s, err := syscall.Getsid(pid); err == nil && s == sid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func signalSession(sid int, sig syscall.Signal) bool {
	pids := sessionPIDs(sid)
	for _, pid := range pids {
		_ = syscall.Kill(pid, sig)
	}
	return len(pids) > 0
}

func waitSessionGone(ctx context.Context, sid int) bool {
	deadline := time.NewTimer(stopWait)
	defer deadline.Stop()
	tick := time.NewTicker(stopStep)
	defer tick.Stop()
	for len(sessionPIDs(sid)) > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return len(sessionPIDs(sid)) == 0
		case <-tick.C:
		}
	}
	return true
}

// Stop ends every process in the runner's session (its id is the runner's
// pid): TERM, then KILL. Deliberate: the shim puts the agent in its own
// process group, so a group signal would miss it. It signals only while the
// runner is alive with the start time runner.json recorded.
func (r LocalRun) Stop(ctx context.Context) error {
	st, err := LocalStatus(r.Base, r.Session, r.Generation)
	if err != nil || st.Phase != PhaseAlive {
		return err
	}
	sid := st.State.PID
	if !signalSession(sid, syscall.SIGTERM) || waitSessionGone(ctx, sid) {
		return nil
	}
	signalSession(sid, syscall.SIGKILL)
	if !waitSessionGone(ctx, sid) {
		return fmt.Errorf("runner: the agent processes of session %s did not end", r.Session)
	}
	return nil
}

// RemoveLocal deletes every generation of session.
func RemoveLocal(base, session string) error {
	if err := checkID("session", session); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(base, session))
}
