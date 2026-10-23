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
	// The stub host's tmux server keeps its socket under <root>/tmux and ends
	// with the test, so no session outlives it.
	tmuxDir := filepath.Join(root, "tmux")
	// The explicit -S is deliberate: with TMUX_TMPDIR alone, tmux falls back to
	// /tmp when the dir is gone, and kill-server would end the machine's own
	// server. A missing socket fails here and touches no other server.
	t.Cleanup(func() {
		sock := filepath.Join(tmuxDir, "tmux-"+strconv.Itoa(os.Getuid()), "default")
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
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
	psDir := filepath.Join(i.fakeSSHState(), "bin")
	if err := os.MkdirAll(psDir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", psDir, err)
	}
	if err := os.WriteFile(filepath.Join(psDir, "ps"), []byte(psWrapper), 0o700); err != nil {
		t.Fatalf("writing the ps wrapper: %v", err)
	}
	cfg, _ := json.Marshal(map[string]string{
		"root": root, "path": psDir + string(os.PathListSeparator) + os.Getenv("PATH"), "call_log": i.fakePath("fakessh.jsonl"),
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

// SSHCommandLineCap makes target refuse any remote command longer than max
// characters, as cmd.exe does past 8,191. Needs Options.SSHStub.
func (i *Instance) SSHCommandLineCap(target string, max int) {
	t := i.t
	t.Helper()
	i.requireSSHStub()
	path := filepath.Join(i.fakeSSHState(), "cmdmax", sshStateName(target))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(max)), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// psWrapper stands first on the stub host's PATH. It runs the machine's ps and,
// for `ps -o lstart= -p <pid>` on a pid named in <root>/.fakessh/lie/, shifts
// the year of the answer, as a host would report if the pid had been reused by
// a process that started at another time. Every other call passes through.
const psWrapper = `#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
pid=
ls=
prev=
for a in "$@"; do
  case "$prev" in
    -p) pid=$a ;;
    -o) case "$a" in lstart=) ls=1 ;; esac ;;
  esac
  prev=$a
done
if [ -n "$ls" ] && [ -n "$pid" ] && [ -e "$d/../lie/$pid" ]; then
  /bin/ps "$@" | sed -e 's/[0-9][0-9][0-9][0-9][[:space:]]*$/1999/'
  exit 0
fi
exec /bin/ps "$@"
`

// SSHLieStart makes the stub host report a different start time for pid from
// now on: the host's ps answers `lstart` with a shifted year. Needs
// Options.SSHStub. The pid's real start time and the registry are untouched, so
// the process reads as one that was replaced after relay listed it.
func (i *Instance) SSHLieStart(pid int) {
	t := i.t
	t.Helper()
	i.requireSSHStub()
	path := filepath.Join(i.fakeSSHState(), "lie", strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// SSHNoClaude removes from the stub host's PATH every directory that holds an
// executable named claude, so a probe of the host finds none. The stub reads
// its config on every call, so the change applies to the next ssh call. Needs
// Options.SSHStub.
func (i *Instance) SSHNoClaude() {
	t := i.t
	t.Helper()
	i.requireSSHStub()
	path := filepath.Join(i.Dir, "sshstub", "fakessh.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg map[string]string
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	var keep []string
	for _, d := range strings.Split(cfg["path"], string(os.PathListSeparator)) {
		if fi, err := os.Stat(filepath.Join(d, "claude")); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			continue
		}
		keep = append(keep, d)
	}
	cfg["path"] = strings.Join(keep, string(os.PathListSeparator))
	out, _ := json.Marshal(cfg)
	writeFileAtomic(t, path, out)
}
