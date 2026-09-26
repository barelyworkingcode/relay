//go:build live

package sandbox

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// socketDenyFixture is a read-write stand-in home H holding two socket deny
// dirs: D, with a listening socket, and E, empty. S and O are siblings a
// session may move freely.
type socketDenyFixture struct {
	profile           string
	home, app         string
	dir, sock, idle   string
	sibling, appOther string
}

func newSocketDenyFixture(t *testing.T) socketDenyFixture {
	t.Helper()
	if err := Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	// /tmp, not t.TempDir(): a socket under the per-test temp path overflows
	// sun_path's 104 bytes.
	tmp, err := os.MkdirTemp("/tmp", "relay-sock-deny-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	root := evalSymlinks(t, tmp)
	home := filepath.Join(root, "h")
	f := socketDenyFixture{
		home:     home,
		app:      filepath.Join(home, "app"),
		dir:      filepath.Join(home, "app", "svc"),
		idle:     filepath.Join(home, "app", "idle"),
		sibling:  filepath.Join(home, "sib"),
		appOther: filepath.Join(home, "app", "other"),
	}
	mkdirs(t, f.dir, f.idle, f.sibling, f.appOther)
	f.sock = listenUnix(t, filepath.Join(f.dir, "x.sock"))
	profile, err := Write(filepath.Join(root, "profiles"), "socket-deny-session", Spec{
		ReadWrite:           []string{home, "/dev"},
		UnixConnectDenyDirs: []string{f.dir, f.idle},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.profile = profile
	return f
}

func requireSocketAt(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode().Type() != fs.ModeSocket {
		t.Errorf("the socket is no longer at %s: %v", path, err)
	}
}

func TestLive_SocketDenyDirAndItsParentCannotBeRenamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(f socketDenyFixture) (from, to, sockAfter string)
	}{
		{"the deny dir", func(f socketDenyFixture) (string, string, string) {
			to := filepath.Join(f.app, "moved")
			return f.dir, to, filepath.Join(to, "x.sock")
		}},
		{"its parent", func(f socketDenyFixture) (string, string, string) {
			to := filepath.Join(f.home, "app2")
			return f.app, to, filepath.Join(to, "svc", "x.sock")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSocketDenyFixture(t)
			from, to, moved := tc.move(f)
			if runProbe(t, f.profile, "rename "+from+" "+to) {
				t.Errorf("renaming %s succeeded", from)
			}
			requireSocketAt(t, f.sock)
			if runProbe(t, f.profile, "connect-unix "+moved) {
				t.Errorf("connecting to the socket at its new path %s succeeded", moved)
			}
		})
	}
}

func TestLive_SocketDenyDirCannotBeSwappedRemovedOrCloned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(f socketDenyFixture) (action, dst string)
	}{
		{"swap the deny dir with a sibling", func(f socketDenyFixture) (string, string) { return "swap " + f.dir + " " + f.sibling, "" }},
		{"swap a sibling with the deny dir", func(f socketDenyFixture) (string, string) { return "swap " + f.sibling + " " + f.dir, "" }},
		{"rmdir the empty deny dir", func(f socketDenyFixture) (string, string) { return "rmdir " + f.idle, "" }},
		{"rename a sibling over the empty deny dir", func(f socketDenyFixture) (string, string) { return "rename " + f.sibling + " " + f.idle, "" }},
		{"clone the deny dir", func(f socketDenyFixture) (string, string) {
			dst := filepath.Join(f.home, "clone")
			return "clone " + f.dir + " " + dst, dst
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSocketDenyFixture(t)
			action, dst := tc.action(f)
			if runProbe(t, f.profile, action) {
				t.Errorf("%q succeeded", action)
			}
			requireSocketAt(t, f.sock)
			if dst == "" {
				return
			}
			if _, err := os.Lstat(dst); err == nil {
				t.Errorf("%s exists after the refused %q", dst, action)
			}
		})
	}
}

func TestLive_SocketInADenyDirIsUnreachableInsideAndReachableOutside(t *testing.T) {
	f := newSocketDenyFixture(t)
	c, err := net.Dial("unix", f.sock)
	if err != nil {
		t.Fatalf("setup: %s unreachable outside the sandbox: %v", f.sock, err)
	}
	_ = c.Close()
	if runProbe(t, f.profile, "connect-unix "+f.sock) {
		t.Error("the socket in the deny dir was reachable inside the sandbox")
	}
}

func TestLive_SocketFileCannotLeaveItsDenyDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		verb  func(f socketDenyFixture) string
		moved func(f socketDenyFixture) string
	}{
		{"rename out", func(f socketDenyFixture) string {
			return "rename " + f.sock + " " + filepath.Join(f.home, "y.sock")
		}, func(f socketDenyFixture) string { return filepath.Join(f.home, "y.sock") }},
		{"unlink", func(f socketDenyFixture) string { return "unlink " + f.sock }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSocketDenyFixture(t)
			action := tc.verb(f)
			if runProbe(t, f.profile, action) {
				t.Errorf("%q succeeded", action)
			}
			requireSocketAt(t, f.sock)
			if tc.moved == nil {
				return
			}
			if moved := tc.moved(f); runProbe(t, f.profile, "connect-unix "+moved) {
				t.Errorf("connecting to the socket at %s succeeded", moved)
			}
		})
	}
}

func TestLive_SocketFileCannotBeSwappedReplacedOrNested(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(f socketDenyFixture, file, sub string) string
	}{
		{"swap a file with the socket", func(f socketDenyFixture, file, _ string) string { return "swap " + file + " " + f.sock }},
		{"rename a file over the socket", func(f socketDenyFixture, file, _ string) string { return "rename " + file + " " + f.sock }},
		{"rename the socket into a subdirectory", func(f socketDenyFixture, _, sub string) string {
			return "rename " + f.sock + " " + filepath.Join(sub, "x.sock")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSocketDenyFixture(t)
			file, sub := filepath.Join(f.home, "f"), filepath.Join(f.dir, "sub")
			writeFile(t, file)
			mkdirs(t, sub)
			action := tc.action(f, file, sub)
			if runProbe(t, f.profile, action) {
				t.Errorf("%q succeeded", action)
			}
			requireSocketAt(t, f.sock)
		})
	}
}

// TestLive_SocketDenyDirLeavesNormalUseWorking runs its steps in order on one
// fixture: each step works on what the one before it left.
func TestLive_SocketDenyDirLeavesNormalUseWorking(t *testing.T) {
	f := newSocketDenyFixture(t)
	file, renamed := filepath.Join(f.dir, "f.txt"), filepath.Join(f.dir, "g.txt")
	sub := filepath.Join(f.dir, "sub")
	for _, action := range []string{
		"write " + file,
		"rename " + file + " " + renamed,
		"unlink " + renamed,
		"mkdir " + sub,
		"rmdir " + sub,
		"chmod " + f.dir,
		"utimes " + f.dir,
		"list " + f.dir,
		"rename " + f.appOther + " " + filepath.Join(f.app, "other2"),
	} {
		if !runProbe(t, f.profile, action) {
			t.Errorf("%q was refused", action)
		}
	}
}
