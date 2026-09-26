//go:build live

package sandbox

// Live tier (docs/testing-roadmap.md): these run a real
// /usr/bin/sandbox-exec against a profile this package rendered and assert
// what the kernel actually allowed, not what a tool reported about itself.
// Everything they touch — projects, sockets, listeners, the profile — is
// created under t.TempDir(); no part of this file reads or writes relay's
// real config directory, its sockets, or any live session's state.
//
// A machine without sandbox-exec sees a skip, never a failure.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sandboxProbe is the action a re-exec of this test binary performs inside
// the sandbox. The helper process exits 0 when the operation succeeded and 1
// when the kernel refused it, which is the only thing the parent asserts on.
const probeEnv = "RELAY_SANDBOX_PROBE"

// probeUnreached is the helper's exit code when a filesystem-shape probe
// failed for a reason other than the sandbox (a missing source, a directory
// that is not empty), so a refusal the kernel never made is not counted as one.
const probeUnreached = 3

func TestSandboxProbeHelper(t *testing.T) {
	action := os.Getenv(probeEnv)
	if action == "" {
		t.Skip("helper process only; driven by the live tests in this file")
	}
	verb, arg, _ := strings.Cut(action, " ")
	if fsShapeProbes[verb] {
		runFSShapeProbe(action, verb, arg)
	}
	var err error
	switch verb {
	case "write":
		err = os.WriteFile(arg, []byte("probe\n"), 0o600)
	case "read":
		_, err = os.ReadFile(arg)
	case "connect-unix":
		var c net.Conn
		c, err = net.Dial("unix", arg)
		if c != nil {
			_ = c.Close()
		}
	case "connect-tcp":
		var c net.Conn
		c, err = net.Dial("tcp", arg)
		if c != nil {
			_ = c.Close()
		}
	case "exec":
		err = exec.Command(arg, "-p", fmt.Sprint(os.Getpid())).Run()
	case "run":
		err = exec.Command(arg).Run()
	case "list":
		_, err = os.ReadDir(arg)
	default:
		fmt.Fprintf(os.Stderr, "unknown probe %q\n", action)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe %q: %v\n", action, err)
		os.Exit(1)
	}
	os.Exit(0)
}

var fsShapeProbes = map[string]bool{
	"rename": true, "swap": true, "clone": true, "fclone": true,
	"rmdir": true, "unlink": true, "mkdir": true, "chmod": true, "utimes": true,
}

// runFSShapeProbe performs one rename, clone or metadata probe and exits. Its
// arg is one path, or a source and a destination separated by a space.
//
// This is deliberate: unix.Rename, not os.Rename, which refuses to rename
// over a directory before the kernel is asked.
func runFSShapeProbe(action, verb, arg string) {
	from, to, _ := strings.Cut(arg, " ")
	var err error
	switch verb {
	case "rename":
		err = unix.Rename(from, to)
	case "swap":
		err = unix.RenamexNp(from, to, unix.RENAME_SWAP)
	case "clone":
		err = unix.Clonefile(from, to, 0)
	case "fclone":
		fd, openErr := unix.Open(from, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "probe %q could not open its source: %v\n", action, openErr)
			os.Exit(probeUnreached)
		}
		err = unix.Fclonefileat(fd, unix.AT_FDCWD, to, 0)
		_ = unix.Close(fd)
	case "rmdir":
		err = unix.Rmdir(arg)
	case "unlink":
		err = unix.Unlink(arg)
	case "mkdir":
		err = os.Mkdir(arg, 0o700)
	case "chmod":
		err = os.Chmod(arg, 0o700)
	case "utimes":
		now := time.Now()
		err = os.Chtimes(arg, now, now)
	}
	switch {
	case err == nil:
		os.Exit(0)
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		fmt.Fprintf(os.Stderr, "probe %q: %v\n", action, err)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "probe %q failed for a reason other than the sandbox: %v\n", action, err)
		os.Exit(probeUnreached)
	}
}

// runProbe runs one probe inside the sandbox and reports whether the
// operation succeeded.
func runProbe(t *testing.T, profile, action string) bool {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profile, "--",
		os.Args[0], "-test.run=^TestSandboxProbeHelper$")
	cmd.Env = append(os.Environ(), probeEnv+"="+action)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("probe %q did not run: %v\n%s", action, err, out)
	}
	if exit.ExitCode() == probeUnreached {
		t.Fatalf("probe %q never reached the sandbox: %s", action, out)
	}
	t.Logf("probe %q refused: %s", action, out)
	return false
}

// liveFixture is one session's worth of C7: a project the session owns, a
// read-only directory, another project and a stand-in home directory it was
// never granted, a stand-in relay directory holding two sockets, and two
// loopback listeners.
type liveFixture struct {
	profile      string
	projA, projB string
	readOnly     string
	roFile       string
	homeSecret   string
	secret       string
	allowSock    string
	denySock     string
	allowAddr    string
	denyAddr     string
}

func newLiveFixture(t *testing.T) liveFixture {
	t.Helper()
	if err := Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	// /tmp, not t.TempDir(): macOS's per-test temp path is long enough that
	// a socket under it overflows sun_path's 104 bytes, the same reason
	// internal/sshhost's own tests root themselves here.
	root, err := os.MkdirTemp("/tmp", "relay-sandbox-live-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := liveFixture{
		projA: filepath.Join(root, "projA"),
		projB: filepath.Join(root, "projB"),
	}
	relayDir := filepath.Join(root, "relay")
	f.readOnly = filepath.Join(root, "readonly")
	homeSSH := filepath.Join(root, "home", ".ssh")
	for _, dir := range []string{f.projA, f.projB, relayDir, f.readOnly, homeSSH} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	f.secret = filepath.Join(f.projB, "secret.txt")
	if err := os.WriteFile(f.secret, []byte("other project's secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	f.roFile = filepath.Join(f.readOnly, "tool.conf")
	if err := os.WriteFile(f.roFile, []byte("read-only config\n"), 0o600); err != nil {
		t.Fatalf("write read-only file: %v", err)
	}
	f.homeSecret = filepath.Join(homeSSH, "id_ed25519")
	if err := os.WriteFile(f.homeSecret, []byte("a private key\n"), 0o600); err != nil {
		t.Fatalf("write home secret: %v", err)
	}

	f.allowSock = listenUnix(t, filepath.Join(relayDir, "relay.sock"))
	f.denySock = listenUnix(t, filepath.Join(relayDir, "relay-frontend.sock"))
	f.allowAddr = listenTCP(t)
	f.denyAddr = listenTCP(t)

	spec := Spec{
		ReadWrite:            []string{f.projA, "/dev"},
		Read:                 []string{f.readOnly},
		UnixConnectDenyDirs:  []string{relayDir},
		UnixConnectAllow:     []string{f.allowSock},
		TCPLoopbackDeny:      []int{port(t, f.denyAddr)},
		TCPLoopbackAllow:     []int{port(t, f.allowAddr)},
		DenySetIDExec:        true,
		ReadWriteFiles:       []string{filepath.Join(f.projA, "state.json")},
		UnixConnectDenyPaths: []string{f.denySock},
	}
	profile, err := Write(filepath.Join(root, "profiles"), "live-session", spec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.profile = profile
	return f
}

func listenUnix(t *testing.T, path string) string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAll(ln)
	return path
}

func listenTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAll(ln)
	return ln.Addr().String()
}

func acceptAll(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

func port(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
		t.Fatalf("parse port %q: %v", p, err)
	}
	return n
}

func TestLive_WriteOutsideTheProjectIsDenied(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "write "+filepath.Join(f.projA, "allowed.txt")) {
		t.Error("a write inside the project was denied")
	}
	outside := filepath.Join(filepath.Dir(f.projA), "escaped.txt")
	if runProbe(t, f.profile, "write "+outside) {
		t.Error("a write outside the project succeeded")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("the denied write left a file on disk")
	}
}

func TestLive_OtherProjectIsUnreadableAndUnchanged(t *testing.T) {
	f := newLiveFixture(t)
	before := fileDigest(t, f.secret)
	if runProbe(t, f.profile, "read "+f.secret) {
		t.Error("another project's file was readable")
	}
	if runProbe(t, f.profile, "write "+f.secret) {
		t.Error("another project's file was writable")
	}
	if after := fileDigest(t, f.secret); after != before {
		t.Error("another project's file changed on disk")
	}
}

// TestLive_UngrantedFilesAreUnreadable is the property the whole model
// exists for: nothing names the directories below, and none of them is
// reachable. A stand-in ~/.ssh is one of them.
func TestLive_UngrantedFilesAreUnreadable(t *testing.T) {
	f := newLiveFixture(t)
	for name, path := range map[string]string{
		"a stand-in ~/.ssh key": f.homeSecret,
		"another project":       f.secret,
	} {
		if runProbe(t, f.profile, "read "+path) {
			t.Errorf("%s was readable", name)
		}
		if runProbe(t, f.profile, "list "+filepath.Dir(path)) {
			t.Errorf("the directory holding %s could be listed", name)
		}
	}
}

// TestLive_ReadOnlyGrantReadsButDoesNotWrite pins the two directions apart.
func TestLive_ReadOnlyGrantReadsButDoesNotWrite(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "read "+f.roFile) {
		t.Error("a file under a read grant was unreadable")
	}
	if runProbe(t, f.profile, "write "+f.roFile) {
		t.Error("a file under a read grant was writable")
	}
	if runProbe(t, f.profile, "write "+filepath.Join(f.readOnly, "new.txt")) {
		t.Error("a file could be created under a read grant")
	}
}

// TestLive_ProjectIsReadableAndWritable pins that a read-write grant is both.
func TestLive_ProjectIsReadableAndWritable(t *testing.T) {
	f := newLiveFixture(t)
	file := filepath.Join(f.projA, "own.txt")
	if !runProbe(t, f.profile, "write "+file) {
		t.Fatal("a write inside the project was denied")
	}
	if !runProbe(t, f.profile, "read "+file) {
		t.Error("a file the session just wrote was unreadable")
	}
	if !runProbe(t, f.profile, "list "+f.projA) {
		t.Error("the project directory could not be listed")
	}
}

// TestLive_BaselineLetsAProcessStart pins that the system baseline is enough
// to run a system binary, which is what denying every file read would
// otherwise break first.
func TestLive_BaselineLetsAProcessStart(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "run /usr/bin/true") {
		t.Error("/usr/bin/true did not start under the baseline")
	}
}

// TestLive_WronglyCasedGrantStillGrants is the reason resolve asks the kernel
// for the on-disk spelling: without it a project path stored in the wrong
// case renders a rule that matches nothing, and the session cannot touch its
// own directory.
func TestLive_WronglyCasedGrantStillGrants(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "Relay-Sandbox-Case-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	proj := filepath.Join(root, "MixedCaseProject")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wrong := strings.ToLower(proj)
	if _, err := os.Stat(wrong); err != nil {
		t.Skip("this volume is case-sensitive; the case problem does not exist here")
	}
	profile, err := Write(filepath.Join(root, "profiles"), "case-session", Spec{ReadWrite: []string{wrong, "/dev"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !runProbe(t, profile, "write "+filepath.Join(proj, "ok.txt")) {
		t.Error("a grant spelled in the wrong case did not grant the real directory")
	}
}

func TestLive_OnlyTheAllowedSocketsAreReachable(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "connect-unix "+f.allowSock) {
		t.Error("relay.sock was not reachable")
	}
	if runProbe(t, f.profile, "connect-unix "+f.denySock) {
		t.Error("the frontend socket was reachable")
	}
	if !runProbe(t, f.profile, "connect-tcp "+f.allowAddr) {
		t.Error("the allowed loopback port was not reachable")
	}
	if runProbe(t, f.profile, "connect-tcp "+f.denyAddr) {
		t.Error("a denied loopback port was reachable")
	}
}

// TestLive_SetIDExecIsDenied uses /bin/ps, which is setgid on macOS and is
// the exact binary SP2 row 9 measured — an agent under this profile loses
// ps, deliberately.
func TestLive_SetIDExecIsDenied(t *testing.T) {
	f := newLiveFixture(t)
	if _, err := os.Stat("/bin/ps"); err != nil {
		t.Skipf("no /bin/ps: %v", err)
	}
	if runProbe(t, f.profile, "exec /bin/ps") {
		t.Error("a setuid/setgid binary was executable")
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// TestLive_DenyCarvesAHoleOutOfAGrant is the case Spec.Deny exists for: the
// grant covers the directory, the deny removes one child of it.
func TestLive_DenyCarvesAHoleOutOfAGrant(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(ssh, "id_test")
	other := filepath.Join(home, "notes.txt")
	for _, p := range []string{key, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := Write(filepath.Join(root, "profiles"), "deny-session", Spec{
		ReadWrite: []string{home, "/dev"},
		Deny:      []string{ssh},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !runProbe(t, profile, "read "+other) {
		t.Error("a file beside the denied directory was unreadable")
	}
	if runProbe(t, profile, "read "+key) {
		t.Error("a file under the denied directory was readable")
	}
	if runProbe(t, profile, "write "+filepath.Join(ssh, "new")) {
		t.Error("a file could be created under the denied directory")
	}
	if runProbe(t, profile, "list "+ssh) {
		t.Error("the denied directory could be listed")
	}
}

// TestLive_LinkSwappedInAfterTheWalkGrantsNothingNew is the race the one-pass
// walk closes: a granted directory replaced by a link between the walk and the
// profile being spelled must not hand the session the link's target.
func TestLive_LinkSwappedInAfterTheWalkGrantsNothingNew(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root := realTempDir(t)
	proj, secret, ok := filepath.Join(root, "proj"), filepath.Join(root, "secret"), filepath.Join(root, "ok")
	mkdirs(t, proj, secret, ok)
	fired := swapAfterWalk(t, proj, func() { replaceWithLink(t, proj, secret) })

	profile, err := Write(filepath.Join(root, "profiles"), "swap-session", Spec{ReadWrite: []string{proj, ok, "/dev"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !*fired {
		t.Fatal("beforeSpell never saw the read-write grant")
	}
	if !runProbe(t, profile, "write "+filepath.Join(ok, "allowed.txt")) {
		t.Fatal("a write inside a granted directory was denied; the profile does not load")
	}
	if runProbe(t, profile, "write "+filepath.Join(secret, "escaped.txt")) {
		t.Error("a write reached the target of a link swapped in after the walk")
	}
}

// denyFixture is a stand-in home and project, both read-write, a read-only
// shared directory, and a read-write directory standing in for the temp dir,
// all under one root so a clone between them stays on one volume. Each deny
// sits below a grant; the two under empty/ name paths that do not exist, so
// their parents are empty ancestors.
type denyFixture struct {
	profile                           string
	home, config, secret, sibling     string
	proj, shared, readme, tmp         string
	emptyForRmdir, emptyForRenameOver string
}

func newDenyFixture(t *testing.T) denyFixture {
	t.Helper()
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	f := denyFixture{
		home:               home,
		config:             filepath.Join(home, ".config"),
		sibling:            filepath.Join(home, "sibling"),
		proj:               filepath.Join(root, "work", "acme"),
		shared:             filepath.Join(root, "work", "shared"),
		tmp:                filepath.Join(root, "tmp"),
		emptyForRmdir:      filepath.Join(home, "empty", "a"),
		emptyForRenameOver: filepath.Join(home, "empty", "b"),
	}
	gh, keys := filepath.Join(f.config, "gh"), filepath.Join(f.shared, "keys")
	mkdirs(t, gh, filepath.Join(f.config, "other"), f.sibling, f.proj, keys, f.tmp, f.emptyForRmdir, f.emptyForRenameOver)
	f.secret = filepath.Join(gh, "hosts.yml")
	f.readme = filepath.Join(f.shared, "readme")
	for _, p := range []string{f.secret, f.readme, filepath.Join(keys, "id"), filepath.Join(f.proj, ".env"),
		filepath.Join(f.proj, "draft.txt"), filepath.Join(f.config, "other", "notes")} {
		writeFile(t, p)
	}
	profile, err := Write(filepath.Join(root, "profiles"), "deny-ancestor-session", Spec{
		ReadWrite: []string{f.home, f.proj, f.tmp, "/dev"},
		Read:      []string{f.shared},
		Deny: []string{gh, filepath.Join(f.proj, ".env"), keys,
			filepath.Join(f.emptyForRmdir, "x"), filepath.Join(f.emptyForRenameOver, "x")},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.profile = profile
	return f
}

func TestLive_RenamingAnAncestorOfADenyDoesNotExposeIt(t *testing.T) {
	f := newDenyFixture(t)
	moved := filepath.Join(f.home, ".c2")
	if runProbe(t, f.profile, "rename "+f.config+" "+moved) {
		t.Error("renaming an ancestor of a deny succeeded")
		if runProbe(t, f.profile, "read "+filepath.Join(moved, "gh", "hosts.yml")) {
			t.Error("the denied file was readable under the ancestor's new name")
		}
		return
	}
	if _, err := os.Stat(f.secret); err != nil {
		t.Errorf("the denied file left its path although the rename was refused: %v", err)
	}
}

func TestLive_AncestorOfADenyCannotBeMovedRemovedOrCloned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(f denyFixture) (action, dst string)
	}{
		{"swap an ancestor with a sibling", func(f denyFixture) (string, string) { return "swap " + f.config + " " + f.sibling, "" }},
		{"swap a sibling with an ancestor", func(f denyFixture) (string, string) { return "swap " + f.sibling + " " + f.config, "" }},
		{"rmdir an empty ancestor", func(f denyFixture) (string, string) { return "rmdir " + f.emptyForRmdir, "" }},
		{"rename a sibling over an empty ancestor", func(f denyFixture) (string, string) {
			return "rename " + f.sibling + " " + f.emptyForRenameOver, ""
		}},
		{"rename the grant root into the temp dir", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "acme")
			return "rename " + f.proj + " " + dst, dst
		}},
		{"clone the grant root into the temp dir", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "acme")
			return "clone " + f.proj + " " + dst, dst
		}},
		{"clonefile a nested ancestor", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "config")
			return "clone " + f.config + " " + dst, dst
		}},
		{"fclonefileat a nested ancestor", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "config")
			return "fclone " + f.config + " " + dst, dst
		}},
		{"clonefile the home grant", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "home")
			return "clone " + f.home + " " + dst, dst
		}},
		{"fclonefileat the home grant", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "home")
			return "fclone " + f.home + " " + dst, dst
		}},
		{"clone a read-granted ancestor", func(f denyFixture) (string, string) {
			dst := filepath.Join(f.tmp, "shared")
			return "clone " + f.shared + " " + dst, dst
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDenyFixture(t)
			action, dst := tc.action(f)
			if runProbe(t, f.profile, action) {
				t.Errorf("%q succeeded", action)
			}
			if dst == "" {
				return
			}
			if _, err := os.Lstat(dst); err == nil {
				t.Errorf("%s exists after the refused %q", dst, action)
			}
		})
	}
}

// TestLive_DenyAncestorRuleLeavesNormalUseWorking runs its steps in order on
// one fixture: each step works on what the one before it left.
func TestLive_DenyAncestorRuleLeavesNormalUseWorking(t *testing.T) {
	f := newDenyFixture(t)
	file, renamed := filepath.Join(f.config, "new.txt"), filepath.Join(f.config, "renamed.txt")
	dir := filepath.Join(f.config, "newdir")
	for _, action := range []string{
		"write " + file,
		"write " + file,
		"mkdir " + dir,
		"rename " + file + " " + renamed,
		"unlink " + renamed,
		"rmdir " + dir,
		"rename " + filepath.Join(f.proj, "draft.txt") + " " + filepath.Join(f.tmp, "draft.txt"),
		"chmod " + f.config,
		"utimes " + f.config,
		"list " + f.config,
		"clone " + filepath.Join(f.config, "other") + " " + filepath.Join(f.tmp, "other"),
		"read " + f.readme,
	} {
		if !runProbe(t, f.profile, action) {
			t.Errorf("%q was refused", action)
		}
	}
}

// useBaseline points the fixed baseline at a stand-in tree for one test. It
// changes package state, so a test that calls it must not run in parallel.
func useBaseline(t *testing.T, read, deny, reopened []string) {
	t.Helper()
	r, d, o := baselineReadDirs, baselineDenyDirs, baselineReopenedFiles
	t.Cleanup(func() { baselineReadDirs, baselineDenyDirs, baselineReopenedFiles = r, d, o })
	baselineReadDirs, baselineDenyDirs, baselineReopenedFiles = read, deny, reopened
}

func TestLive_BaselineCarveOutAncestorsCannotBeCloned(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root := realTempDir(t)
	usr := filepath.Join(root, "usr")
	local := filepath.Join(usr, "local")
	etc, bin, tmp := filepath.Join(local, "etc"), filepath.Join(local, "bin"), filepath.Join(root, "tmp")
	openssl := filepath.Join(etc, "openssl@3")
	mkdirs(t, openssl, filepath.Join(etc, "ca-certificates"), filepath.Join(local, "var"), bin, tmp)
	reopened := []string{
		filepath.Join(openssl, "cert.pem"),
		filepath.Join(etc, "ca-certificates", "cert.pem"),
		filepath.Join(etc, "gitconfig"),
	}
	unopened := filepath.Join(etc, "service.conf")
	for _, p := range append([]string{unopened, filepath.Join(bin, "tool")}, reopened...) {
		writeFile(t, p)
	}
	// The real system baseline stays: the probe is this test binary, and it
	// cannot start without the real /usr.
	useBaseline(t, append(slices.Clone(baselineReadDirs), usr), []string{etc, filepath.Join(local, "var")}, reopened)
	profile, err := Write(filepath.Join(root, "profiles"), "baseline-ancestor-session", Spec{ReadWrite: []string{tmp, "/dev"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, src := range []string{usr, local, etc, openssl} {
		dst := filepath.Join(tmp, filepath.Base(src))
		if runProbe(t, profile, "clone "+src+" "+dst) {
			t.Errorf("cloning %s succeeded", src)
		}
		if _, err := os.Lstat(dst); err == nil {
			t.Errorf("%s exists after the refused clone of %s", dst, src)
		}
	}
	for _, p := range reopened {
		if !runProbe(t, profile, "read "+p) {
			t.Errorf("reopened %s was unreadable", p)
		}
	}
	if runProbe(t, profile, "read "+unopened) {
		t.Errorf("%s under the carve-out was readable", unopened)
	}
	if !runProbe(t, profile, "clone "+bin+" "+filepath.Join(tmp, "bin")) {
		t.Error("cloning a directory beside the carve-out was refused")
	}
}
