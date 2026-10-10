package harness

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// SSHHost is a user-mode sshd on loopback, for the host rows. It has its own
// host key and client key under the run root and accepts only that client key.
type SSHHost struct {
	Target       string // "<user>@127.0.0.1"
	Port         int
	IdentityFile string // client private key, 0600

	hostKeyPub string
}

const (
	sshdPath         = "/usr/sbin/sshd"
	sshdReadyMarker  = "Server listening on"
	sshdStartTries   = 3
	sshdStartTimeout = 30 * time.Second
	sshdStopTimeout  = 15 * time.Second
)

// StartSSHHost starts sshd and returns after its "Server listening on" stderr
// line. A lost port race (sshd exits before that line) is retried. Cleanup
// stops sshd, which ends any ssh control master relay left behind.
func StartSSHHost(t *testing.T) *SSHHost {
	t.Helper()
	if runRoot == "" {
		t.Fatalf("harness.Main did not run: use it from TestMain")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatalf("looking up the current user: %v", err)
	}
	dir, err := os.MkdirTemp(runRoot, "sshd-")
	if err != nil {
		t.Fatalf("creating the sshd directory: %v", err)
	}
	h := &SSHHost{Target: u.Username + "@127.0.0.1", IdentityFile: filepath.Join(dir, "client_key")}
	hostKey := filepath.Join(dir, "host_key")
	keygen(t, hostKey)
	keygen(t, h.IdentityFile)
	pub := readFile(t, hostKey+".pub")
	fields := strings.Fields(pub)
	if len(fields) < 2 {
		t.Fatalf("host public key %s.pub is not '<type> <key>': %q", hostKey, pub)
	}
	h.hostKeyPub = fields[0] + " " + fields[1]
	writeFile(t, filepath.Join(dir, "authorized_keys"), []byte(readFile(t, h.IdentityFile+".pub")))

	var lastErr string
	for try := 0; try < sshdStartTries; try++ {
		port := freeLoopbackPort(t)
		cfg := filepath.Join(dir, "sshd_config")
		writeFile(t, cfg, []byte(sshdConfig(port, hostKey, filepath.Join(dir, "authorized_keys"), filepath.Join(dir, "sshd.pid"))))
		stop, ok, why := startSSHD(t, cfg)
		if ok {
			h.Port = port
			t.Cleanup(stop)
			return h
		}
		lastErr = why
	}
	t.Fatalf("sshd did not report listening after %d tries: %s", sshdStartTries, lastErr)
	return nil
}

func sshdConfig(port int, hostKey, authorizedKeys, pidFile string) string {
	return strings.Join([]string{
		"ListenAddress 127.0.0.1",
		fmt.Sprintf("Port %d", port),
		"HostKey " + hostKey,
		"AuthorizedKeysFile " + authorizedKeys,
		"PidFile " + pidFile,
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PubkeyAuthentication yes",
		"UsePAM no",
		"StrictModes no",
		"LogLevel INFO",
		"",
	}, "\n")
}

// startSSHD runs sshd and waits for its listening line. ok is false when sshd
// exited first or the line never came; why carries its stderr.
func startSSHD(t *testing.T, cfg string) (stop func(), ok bool, why string) {
	t.Helper()
	cmd := exec.Command(sshdPath, "-D", "-e", "-f", cfg)
	// A non-file writer makes Wait finish the copy before it returns, so the
	// stderr of an sshd that died early is complete when it is read.
	log := &listenWatch{listening: make(chan struct{})}
	cmd.Stderr = log
	// sshd forks one process per connection and they inherit stderr. Own process
	// group lets stop end them with the listener, and WaitDelay keeps Wait from
	// blocking on a pipe a straggler still holds.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", sshdPath, err)
	}
	listening := log.listening
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop = func() {
		pgid := cmd.Process.Pid
		// Each connection's monitor process leads its own process group, so the
		// group signal alone misses it; its ssh control master would then live on.
		// The tree is read before the listener dies, while the parent links hold.
		conns := descendants(pgid)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		for _, pid := range conns {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		select {
		case <-exited:
		case <-time.After(sshdStopTimeout):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-exited
		}

	}
	select {
	case <-listening:
		return stop, true, ""
	case <-exited:
		return nil, false, log.text()
	case <-time.After(sshdStartTimeout):
		stop()
		return nil, false, "no listening line within " + sshdStartTimeout.String()
	}
}

// listenWatch collects sshd's stderr and signals its listening line.
type listenWatch struct {
	mu        sync.Mutex
	buf       []byte
	once      sync.Once
	listening chan struct{}
}

func (w *listenWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) < 8192 {
		w.buf = append(w.buf, p...)
	}
	if strings.Contains(string(w.buf), sshdReadyMarker) {
		w.once.Do(func() { close(w.listening) })
	}
	return len(p), nil
}

func (w *listenWatch) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// descendants lists every process below root, read from ps.
func descendants(root int) []int {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		var pid, ppid int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d %d", &pid, &ppid); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var all []int
	queue := []int{root}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			all = append(all, c)
			queue = append(queue, c)
		}
	}
	return all
}

func keygen(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen %s: %v\n%s", path, err, out)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TrustSSHHost lets this instance's ssh accept h's host key. relay runs ssh with
// BatchMode=yes and no StrictHostKeyChecking override, so an unknown key would
// fail the connection.
//
// ssh reads known_hosts from the passwd entry's home, not from $HOME, so the
// instance's HOME alone would not reach it. A one-line ssh wrapper first on the
// instance PATH points ssh at the instance's own file instead, which keeps the
// real ~/.ssh untouched.
func (i *Instance) TrustSSHHost(h *SSHHost) {
	i.t.Helper()
	sshDir := filepath.Join(i.Home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		i.t.Fatalf("creating %s: %v", sshDir, err)
	}
	known := filepath.Join(sshDir, "known_hosts")
	f, err := os.OpenFile(known, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		i.t.Fatalf("opening %s: %v", known, err)
	}
	_, werr := fmt.Fprintf(f, "[127.0.0.1]:%d %s\n", h.Port, h.hostKeyPub)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		i.t.Fatalf("writing %s: %v", known, werr)
	}
	wrapper := filepath.Join(i.Home, ".local", "bin", "ssh")
	script := fmt.Sprintf("#!/bin/sh\nexec /usr/bin/ssh -o UserKnownHostsFile=%s -o GlobalKnownHostsFile=/dev/null \"$@\"\n", shellQuote(known))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		i.t.Fatalf("writing %s: %v", wrapper, err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
