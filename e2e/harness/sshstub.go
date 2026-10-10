package harness

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// fakeSSHState is the stub's per-destination state dir under the remote root
// (see e2e/fakes/fakessh).
func (i *Instance) fakeSSHState() string {
	return filepath.Join(i.RemoteRoot(), ".fakessh")
}

// installSSHStub links the fakessh binary into the instance, writes the config
// it reads beside itself, and writes the test-build seam that points relay at
// it. It runs before serve so the seam file exists at boot.
func (i *Instance) installSSHStub() {
	t := i.t
	t.Helper()
	root := i.RemoteRoot()
	for _, d := range []string{filepath.Join(root, "tmp"), filepath.Join(root, "tmux"), filepath.Join(i.Dir, "sshstub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	// The stub reads fakessh.json beside its own binary, so each instance gets
	// its own link rather than sharing the bundle's.
	stub := filepath.Join(i.Dir, "sshstub", "ssh")
	if err := os.Link(bundle.FakeSSH, stub); err != nil {
		data, rerr := os.ReadFile(bundle.FakeSSH)
		if rerr != nil {
			t.Fatalf("linking the ssh stub: %v; reading %s: %v", err, bundle.FakeSSH, rerr)
		}
		if werr := os.WriteFile(stub, data, 0o755); werr != nil {
			t.Fatalf("copying the ssh stub: %v", werr)
		}
	}
	cfg, _ := json.Marshal(map[string]string{
		"root": root, "path": os.Getenv("PATH"), "call_log": i.fakePath("fakessh.jsonl"),
	})
	writeFileAtomic(t, filepath.Join(i.Dir, "sshstub", "fakessh.json"), cfg)
	seam, _ := json.Marshal(map[string]string{"command": stub})
	writeFileAtomic(t, filepath.Join(i.ConfigDir, "test-ssh.json"), seam)
}

// sshStateName maps a destination to its state file name, as the stub does.
func sshStateName(dest string) string {
	b := []byte(dest)
	for k, ch := range b {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '@', ch == '.', ch == '-', ch == '_':
		default:
			b[k] = '_'
		}
	}
	if s := string(b); s != "." && s != ".." {
		return s
	}
	return "_"
}

func (i *Instance) requireSSHStub() {
	i.t.Helper()
	if i.fake || !i.opts.SSHStub {
		i.t.Fatalf("SSHDown and SSHUp need a real instance started with Options.SSHStub")
	}
}

// SSHDown makes target refuse every ssh call and drops the links in flight.
// It returns after the marker is written and each live stub for target has
// been sent SIGTERM; the caller waits on the frame that follows.
func (i *Instance) SSHDown(target string) {
	t := i.t
	t.Helper()
	i.requireSSHStub()
	name := sshStateName(target)
	marker := filepath.Join(i.fakeSSHState(), "down", name)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(marker), err)
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("writing %s: %v", marker, err)
	}
	_ = f.Close()
	// The stub's pid files carry no destination, so a pid is matched to target
	// by its command line.
	entries, _ := os.ReadDir(filepath.Join(i.fakeSSHState(), "pids"))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := exec.Command("ps", "-o", "command=", "-p", e.Name()).Output()
		if err != nil || !strings.Contains(string(cmdline), target) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("signalling ssh stub %d: %v", pid, err)
		}
	}
}

// SSHUp lets target accept ssh calls again.
func (i *Instance) SSHUp(target string) {
	t := i.t
	t.Helper()
	i.requireSSHStub()
	marker := filepath.Join(i.fakeSSHState(), "down", sshStateName(target))
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing %s: %v", marker, err)
	}
}
