package relayfs

// Black-box tests driving a real p9.Client against a real p9.Server against
// a real fixture directory, exactly as the package's own design intends
// (see Root's doc comment in root.go) — no mocks, kernel not involved. The
// fixture is built by testdata/mkfixture.sh, the same script the design doc
// anticipates, run as a subprocess so its shape (nested dirs, varied
// content, the whole symlink hazard set, permission fixtures) stays in one
// place rather than being re-invented in Go.
//
// One deliberate exception: the four extended-attribute File methods
// (SetXattr/GetXattr/ListXattrs/RemoveXattr) cannot be driven through
// github.com/hugelgupf/p9's vendored client at v0.4.1 — clientFile's
// implementations of all four return linux.ENOSYS locally and never send a
// Txattrwalk/Txattrcreate request at all (see client_file.go in the module
// cache), so no real client can reach relayfs's xattr containment rules
// over the wire. Those cases are exercised by calling the unexported *file
// methods directly (TestRelayfs_Xattr_*, below) — still real files and real
// fd-based xattr syscalls, just without the 9P hop the client can't make.
// This is a limitation of the vendored library, not of relayfs; it is not
// worked around by adding any production-code seam.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"
)

// buildFixture runs testdata/mkfixture.sh against a fresh temp dir and
// returns the fixture root it printed (its own documented output contract:
// the last line of stdout is the root path). Each call gets its own temp
// dir, so tests that mutate the fixture (create/rename/unlink) never step
// on one another.
func buildFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	script, err := filepath.Abs(filepath.Join("testdata", "mkfixture.sh"))
	if err != nil {
		t.Fatalf("resolving mkfixture.sh path: %v", err)
	}
	cmd := exec.Command("bash", script, base)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mkfixture.sh failed: %v\nstderr:\n%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	root := lines[len(lines)-1]
	fi, statErr := os.Stat(root)
	if root == "" || statErr != nil || !fi.IsDir() {
		t.Fatalf("mkfixture.sh did not print a usable root path; got %q (stat err: %v)", root, statErr)
	}
	// guarded.txt carries a deny-delete ACE (see the script's own comment on
	// why it clears ACLs before its own next run). t.TempDir's automatic
	// removal knows nothing about that, so clear it ourselves — registered
	// after t.TempDir's own cleanup, so LIFO runs this first.
	t.Cleanup(func() { exec.Command("chmod", "-RN", base).Run() })
	return root
}

// harness wires a real relayfs.Root to a real p9.Client over a net.Pipe, the
// same shape the package's own commit message says it was verified against
// during development.
type harness struct {
	fixtureRoot string // e.g. <tmp>/root
	fixtureBase string // e.g. <tmp>, parent of both root/ and outside/
	root        *Root
	client      *p9.Client
	rootFile    p9.File
}

func newHarness(t *testing.T, write bool) *harness {
	t.Helper()
	fixtureRoot := buildFixture(t)

	root, err := Open(fixtureRoot, Policy{Write: write}, NopHooks{})
	if err != nil {
		t.Fatalf("relayfs.Open: %v", err)
	}
	t.Cleanup(func() { root.Close() })

	serverConn, clientConn := net.Pipe()
	srv := p9.NewServer(root)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		srv.Handle(serverConn, serverConn)
	}()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
		<-serverDone
	})

	client, err := p9.NewClient(clientConn)
	if err != nil {
		t.Fatalf("p9.NewClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	rootFile, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { rootFile.Close() })

	return &harness{
		fixtureRoot: fixtureRoot,
		fixtureBase: filepath.Dir(fixtureRoot),
		root:        root,
		client:      client,
		rootFile:    rootFile,
	}
}

// walk is a small helper: walk from the mount root to a slash-joined path,
// failing the test on any error. Every call gets a fresh fid — required
// whenever the caller might otherwise reuse one that Create has already
// repurposed (see the Create containment-table subtest for why).
func (h *harness) walk(t *testing.T, name string) p9.File {
	t.Helper()
	var names []string
	if name != "" {
		names = strings.Split(name, "/")
	}
	_, f, err := h.rootFile.Walk(names)
	if err != nil {
		t.Fatalf("Walk(%q): %v", name, err)
	}
	return f
}

func wantErrno(t *testing.T, err error, want linux.Errno, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got nil error, want %v", what, want)
	}
	var got linux.Errno
	if !errors.As(err, &got) {
		t.Fatalf("%s: err = %v (%T), want a linux.Errno %v", what, err, err, want)
	}
	if got != want {
		t.Fatalf("%s: errno = %v, want %v", what, got, want)
	}
}

// ---------------------------------------------------------------------------
// Reads: walk, open, content
// ---------------------------------------------------------------------------

func TestRelayfs_WalkOpenReadAt_ContentMatchesFixture(t *testing.T) {
	h := newHarness(t, true)

	want, err := os.ReadFile(filepath.Join(h.fixtureRoot, "notes", "config.txt"))
	if err != nil {
		t.Fatalf("reading fixture file directly: %v", err)
	}

	f := h.walk(t, "notes/config.txt")
	defer f.Close()
	if _, _, err := f.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, len(want))
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("content mismatch: got %q, want %q", buf[:n], want)
	}
}

func TestRelayfs_WalkOpenReadAt_LargeBinaryContentMatchesFixture(t *testing.T) {
	h := newHarness(t, true)

	want, err := os.ReadFile(filepath.Join(h.fixtureRoot, "src", "big.bin"))
	if err != nil {
		t.Fatalf("reading fixture file directly: %v", err)
	}

	f := h.walk(t, "src/big.bin")
	defer f.Close()
	if _, _, err := f.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, len(want))
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(want) || !bytes.Equal(buf[:n], want) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes, equal=%v", n, len(want), bytes.Equal(buf[:n], want))
	}
}

// ---------------------------------------------------------------------------
// Walk refusals — enforced by the vendored p9 library itself
// ---------------------------------------------------------------------------

func TestRelayfs_Walk_UnsafeComponentsRefused(t *testing.T) {
	h := newHarness(t, true)
	cases := []struct {
		name string
		bad  string
	}{
		{"dot", "."},
		{"dotdot", ".."},
		{"embedded-slash", "notes/config.txt"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := h.rootFile.Walk([]string{c.bad})
			if err == nil {
				t.Fatalf("Walk([%q]) succeeded, want refused", c.bad)
			}
			wantErrno(t, err, linux.EINVAL, "Walk of an unsafe single component")
		})
	}
}

// ---------------------------------------------------------------------------
// Symlinks: open refused, Readlink verbatim, create allowed
// ---------------------------------------------------------------------------

// SUSPECTED PRODUCTION BUG (see the final test-run report, not fixed here):
// file.go's Open refuses a symlink with linux.ELOOP and reports it through
// hooks.Refused(..., "symlink-on-open") — but the vendored p9 server's own
// Tlopen handler (handlers.go: CanOpen, called from tlopen.handle before
// ref.file.Open is ever reached) already refuses opening a symlink fid,
// with linux.EINVAL, for every File implementation including this one.
// relayfs's ELOOP branch and its audit hook call are therefore dead code
// through any real 9P client on this library version: the real,
// client-observable errno is EINVAL, and the "symlink-on-open" Refused
// event this package's own design promises never fires. This test asserts
// the real, observed behavior; TestRelayfs_Open_OnSymlink_DirectCall_ELOOP
// below confirms file.Open's own logic is correct in isolation — it is the
// framework layer above it that never lets a real caller reach it.
func TestRelayfs_Open_OnSymlink_EINVAL_FrameworkRefusesBeforeFileOpen(t *testing.T) {
	h := newHarness(t, true)
	for _, link := range []string{"link-out-file", "link-out-dangling", "rel-link-in", "cycle-a"} {
		t.Run(link, func(t *testing.T) {
			f := h.walk(t, link)
			defer f.Close()
			_, _, err := f.Open(p9.ReadOnly)
			wantErrno(t, err, linux.EINVAL, "Open on a symlink ("+link+") through the real client")
		})
	}
}

// Confirms file.Open's own ELOOP logic is correct when called directly,
// bypassing the p9 server framework's CanOpen gate that shadows it for
// every real caller (see the comment above).
func TestRelayfs_Open_OnSymlink_DirectCall_ELOOP(t *testing.T) {
	root, _ := newDirectRoot(t, true)
	f := &file{r: root, rel: "link-out-file"}
	_, _, err := f.Open(p9.ReadOnly)
	wantErrno(t, err, linux.ELOOP, "file.Open called directly on a symlink")
}

func TestRelayfs_Readlink_AbsoluteOutOfRoot_ReturnsRawTargetVerbatim(t *testing.T) {
	h := newHarness(t, true)
	f := h.walk(t, "link-out-file")
	defer f.Close()

	target, err := f.Readlink()
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	want := filepath.Join(h.fixtureBase, "outside", "secret.txt")
	if target != want {
		t.Fatalf("Readlink returned %q, want the raw fixture target %q verbatim", target, want)
	}
}

func TestRelayfs_Readlink_RelativeTarget_ReturnsVerbatim(t *testing.T) {
	h := newHarness(t, true)
	f := h.walk(t, "rel-link-in")
	defer f.Close()

	target, err := f.Readlink()
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "notes" {
		t.Fatalf("Readlink returned %q, want the raw relative target %q verbatim", target, "notes")
	}
}

func TestRelayfs_Symlink_Create_AnyTargetSucceeds(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	defer dir.Close()

	const absoluteTarget = "/etc/hosts" // arbitrary, deliberately outside the root and not required to resolve
	qid, err := dir.Symlink(absoluteTarget, "new-symlink.sym", p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if qid.Type != p9.TypeSymlink {
		t.Fatalf("Symlink QID type = %v, want TypeSymlink", qid.Type)
	}

	fi, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "new-symlink.sym"))
	if err != nil {
		t.Fatalf("Lstat on disk: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("created node is not a symlink on disk: mode=%v", fi.Mode())
	}
	gotTarget, err := os.Readlink(filepath.Join(h.fixtureRoot, "src", "new-symlink.sym"))
	if err != nil {
		t.Fatalf("os.Readlink: %v", err)
	}
	if gotTarget != absoluteTarget {
		t.Fatalf("on-disk symlink target = %q, want %q (unvalidated, unmodified)", gotTarget, absoluteTarget)
	}
}

// ---------------------------------------------------------------------------
// Hardlinks and device nodes
// ---------------------------------------------------------------------------

func TestRelayfs_Link_Hardlink_EPERM(t *testing.T) {
	h := newHarness(t, true)
	target := h.walk(t, "notes/config.txt")
	defer target.Close()
	dir := h.walk(t, "notes")
	defer dir.Close()

	err := dir.Link(target, "hardlink-attempt.txt")
	wantErrno(t, err, linux.EPERM, "Link (hardlink)")

	if _, err := os.Lstat(filepath.Join(h.fixtureRoot, "notes", "hardlink-attempt.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused hardlink left a node on disk (stat err: %v)", err)
	}
}

func TestRelayfs_Mknod_DeviceNodesRefused_RegularCreates(t *testing.T) {
	h := newHarness(t, true)

	cases := []struct {
		name string
		mode p9.FileMode
	}{
		{"a-fifo", p9.ModeNamedPipe | 0o644},
		{"a-socket", p9.ModeSocket | 0o644},
		{"a-chardev", p9.ModeCharacterDevice | 0o644},
		{"a-blockdev", p9.ModeBlockDevice | 0o644},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := h.walk(t, "src")
			defer dir.Close()
			_, err := dir.Mknod(c.name, c.mode, 0, 0, p9.NoUID, p9.NoGID)
			wantErrno(t, err, linux.EPERM, "Mknod of a non-regular node ("+c.name+")")
			if _, statErr := os.Lstat(filepath.Join(h.fixtureRoot, "src", c.name)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused mknod left a node on disk (stat err: %v)", statErr)
			}
		})
	}

	t.Run("S_IFREG creates a regular file", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		qid, err := dir.Mknod("mknod-regular.txt", p9.ModeRegular|0o644, 0, 0, p9.NoUID, p9.NoGID)
		if err != nil {
			t.Fatalf("Mknod(S_IFREG): %v", err)
		}
		if qid.Type != p9.TypeRegular {
			t.Fatalf("QID type = %v, want TypeRegular", qid.Type)
		}
		fi, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "mknod-regular.txt"))
		if err != nil {
			t.Fatalf("Lstat on disk: %v", err)
		}
		if !fi.Mode().IsRegular() {
			t.Fatalf("mode = %v, want a regular file", fi.Mode())
		}
	})
}

// ---------------------------------------------------------------------------
// Create / Mkdir / UnlinkAt on a write-policy root
// ---------------------------------------------------------------------------

func TestRelayfs_Create_WritesAndOpensTheFile(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	// N.B. per p9.File.Create's own doc comment, the client-side fid this
	// call is made on becomes the created file — "dir" is not the
	// directory any more after this line, hence not reused below.
	created, qid, _, err := dir.Create("created.txt", p9.ReadWrite, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer created.Close()
	if qid.Type != p9.TypeRegular {
		t.Fatalf("QID type = %v, want TypeRegular", qid.Type)
	}
	if _, err := created.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatalf("WriteAt on the freshly created file: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(h.fixtureRoot, "src", "created.txt"))
	if err != nil {
		t.Fatalf("reading created file directly: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("on-disk content = %q, want %q", got, "hello")
	}
}

func TestRelayfs_Mkdir_CreatesADirectory(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	defer dir.Close()
	qid, err := dir.Mkdir("new-subdir", p9.FileMode(0o755), p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if qid.Type != p9.TypeDir {
		t.Fatalf("QID type = %v, want TypeDir", qid.Type)
	}
	fi, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "new-subdir"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("Lstat on disk: fi=%v err=%v", fi, err)
	}
}

func TestRelayfs_UnlinkAt_FileAndEmptyDirSucceed_NonEmptyDirRefused(t *testing.T) {
	h := newHarness(t, true)

	t.Run("file", func(t *testing.T) {
		dir := h.walk(t, "src")
		created, _, _, err := dir.Create("to-delete.txt", p9.WriteOnly, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		created.Close()

		srcDir := h.walk(t, "src")
		defer srcDir.Close()
		if err := srcDir.UnlinkAt("to-delete.txt", 0); err != nil {
			t.Fatalf("UnlinkAt(file): %v", err)
		}
		if _, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "to-delete.txt")); !os.IsNotExist(err) {
			t.Fatalf("file still exists on disk after UnlinkAt (stat err: %v)", err)
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		if err := h.rootFile.UnlinkAt("empty", 0); err != nil {
			t.Fatalf("UnlinkAt(empty dir): %v", err)
		}
		if _, err := os.Lstat(filepath.Join(h.fixtureRoot, "empty")); !os.IsNotExist(err) {
			t.Fatalf("empty dir still exists on disk after UnlinkAt (stat err: %v)", err)
		}
	})

	t.Run("non-empty directory", func(t *testing.T) {
		err := h.rootFile.UnlinkAt("notes", 0)
		wantErrno(t, err, linux.ENOTEMPTY, "UnlinkAt(non-empty dir)")
		if fi, statErr := os.Lstat(filepath.Join(h.fixtureRoot, "notes")); statErr != nil || !fi.IsDir() {
			t.Fatalf("notes/ was removed despite being non-empty: fi=%v err=%v", fi, statErr)
		}
	})
}

// ---------------------------------------------------------------------------
// RenameAt
// ---------------------------------------------------------------------------

func TestRelayfs_RenameAt_WithinRoot_Succeeds(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	created, _, _, err := dir.Create("rename-src.txt", p9.WriteOnly, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.Close()

	srcDir := h.walk(t, "src")
	defer srcDir.Close()
	if err := srcDir.RenameAt("rename-src.txt", srcDir, "rename-dst.txt"); err != nil {
		t.Fatalf("RenameAt: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "rename-src.txt")); !os.IsNotExist(err) {
		t.Fatalf("old name still exists after rename (stat err: %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "rename-dst.txt")); err != nil {
		t.Fatalf("new name does not exist after rename: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SetAttr: chmod strips setuid, chown is a same-owner no-op only
// ---------------------------------------------------------------------------

func TestRelayfs_SetAttr_Chmod_StripsSetuidBitOnDisk(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	created, _, _, err := dir.Create("chmod-target.txt", p9.WriteOnly, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.Close()

	f := h.walk(t, "src/chmod-target.txt")
	defer f.Close()

	const setuid = p9.FileMode(0o4000)
	err = f.SetAttr(p9.SetAttrMask{Permissions: true}, p9.SetAttr{Permissions: 0o755 | setuid})
	if err != nil {
		t.Fatalf("SetAttr(chmod with setuid): %v", err)
	}

	fi, err := os.Lstat(filepath.Join(h.fixtureRoot, "src", "chmod-target.txt"))
	if err != nil {
		t.Fatalf("Lstat on disk: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if st.Mode&syscall.S_ISUID != 0 {
		t.Fatalf("setuid bit survived on disk: mode=%#o", st.Mode)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("permission bits = %#o, want %#o", fi.Mode().Perm(), 0o755)
	}
}

func TestRelayfs_SetAttr_Chown_SelfNoOpSucceeds_OtherUIDRefused(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	created, _, _, err := dir.Create("chown-target.txt", p9.WriteOnly, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.Close()

	selfUID := p9.UID(os.Getuid())

	t.Run("no-op to current owner succeeds", func(t *testing.T) {
		f := h.walk(t, "src/chown-target.txt")
		defer f.Close()
		if err := f.SetAttr(p9.SetAttrMask{UID: true}, p9.SetAttr{UID: selfUID}); err != nil {
			t.Fatalf("SetAttr(chown to self): %v", err)
		}
	})

	t.Run("chown to another uid is refused", func(t *testing.T) {
		f := h.walk(t, "src/chown-target.txt")
		defer f.Close()
		other := selfUID + 12345
		err := f.SetAttr(p9.SetAttrMask{UID: true}, p9.SetAttr{UID: other})
		wantErrno(t, err, linux.EPERM, "SetAttr(chown to another uid)")
	})
}

// ---------------------------------------------------------------------------
// Read-only mount: every mutating call is EROFS, reads still work
// ---------------------------------------------------------------------------

func TestRelayfs_ReadOnlyMount_MutationsRefusedReadsWork(t *testing.T) {
	h := newHarness(t, false)

	t.Run("Create", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		_, _, _, err := dir.Create("nope.txt", p9.WriteOnly, p9.FileMode(0o644), p9.NoUID, p9.NoGID)
		wantErrno(t, err, linux.EROFS, "Create on a read-only mount")
	})

	t.Run("Mkdir", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		_, err := dir.Mkdir("nope-dir", p9.FileMode(0o755), p9.NoUID, p9.NoGID)
		wantErrno(t, err, linux.EROFS, "Mkdir on a read-only mount")
	})

	t.Run("Symlink", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		_, err := dir.Symlink("target", "nope.sym", p9.NoUID, p9.NoGID)
		wantErrno(t, err, linux.EROFS, "Symlink on a read-only mount")
	})

	t.Run("Mknod S_IFREG", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		_, err := dir.Mknod("nope-node.txt", p9.ModeRegular|0o644, 0, 0, p9.NoUID, p9.NoGID)
		wantErrno(t, err, linux.EROFS, "Mknod(S_IFREG) on a read-only mount")
	})

	t.Run("UnlinkAt", func(t *testing.T) {
		err := h.rootFile.UnlinkAt("empty", 0)
		wantErrno(t, err, linux.EROFS, "UnlinkAt on a read-only mount")
	})

	t.Run("RenameAt", func(t *testing.T) {
		dir := h.walk(t, "notes")
		defer dir.Close()
		err := dir.RenameAt("config.txt", dir, "renamed.txt")
		wantErrno(t, err, linux.EROFS, "RenameAt on a read-only mount")
	})

	t.Run("SetAttr chmod", func(t *testing.T) {
		f := h.walk(t, "notes/config.txt")
		defer f.Close()
		err := f.SetAttr(p9.SetAttrMask{Permissions: true}, p9.SetAttr{Permissions: 0o600})
		wantErrno(t, err, linux.EROFS, "SetAttr(chmod) on a read-only mount")
	})

	t.Run("Open write-mode is refused before symlink/mutation checks even apply", func(t *testing.T) {
		f := h.walk(t, "notes/config.txt")
		defer f.Close()
		_, _, err := f.Open(p9.WriteOnly)
		wantErrno(t, err, linux.EROFS, "Open(WriteOnly) on a read-only mount")
	})

	t.Run("reads still work: ReadAt", func(t *testing.T) {
		want, err := os.ReadFile(filepath.Join(h.fixtureRoot, "notes", "config.txt"))
		if err != nil {
			t.Fatalf("reading fixture file directly: %v", err)
		}
		f := h.walk(t, "notes/config.txt")
		defer f.Close()
		if _, _, err := f.Open(p9.ReadOnly); err != nil {
			t.Fatalf("Open(ReadOnly): %v", err)
		}
		buf := make([]byte, len(want))
		n, err := f.ReadAt(buf, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt: %v", err)
		}
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("content mismatch on a read-only mount: got %q, want %q", buf[:n], want)
		}
	})

	t.Run("reads still work: Readdir", func(t *testing.T) {
		dir := h.walk(t, "src")
		defer dir.Close()
		if _, _, err := dir.Open(p9.ReadOnly); err != nil {
			t.Fatalf("Open(ReadOnly) on a directory: %v", err)
		}
		ents, err := dir.Readdir(0, 1000)
		if err != nil {
			t.Fatalf("Readdir: %v", err)
		}
		if len(ents) == 0 {
			t.Fatalf("Readdir returned no entries on a read-only mount")
		}
	})

	t.Run("reads still work: Readlink", func(t *testing.T) {
		f := h.walk(t, "link-out-file")
		defer f.Close()
		if _, err := f.Readlink(); err != nil {
			t.Fatalf("Readlink on a read-only mount: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Readdir: count is an entry-count bound, not just a wire-truncation
// backstop — page through with a deliberately small count and confirm the
// full, correct, gap-free, duplicate-free entry set comes back. This is the
// regression the P2 commit message calls out by name (a wrong wire
// assumption that Treaddir's count field was a byte budget, not an entry
// count).
// ---------------------------------------------------------------------------

func TestRelayfs_Readdir_PagesWithSmallCount_FullSetNoDuplicatesNoGaps(t *testing.T) {
	h := newHarness(t, true)
	dir := h.walk(t, "src")
	defer dir.Close()
	if _, _, err := dir.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open(ReadOnly): %v", err)
	}

	want := []string{"attrs.txt", "big.bin", "guarded.txt", "longline.txt", "mode600.txt", "mode644.txt", "mode755.txt", "pixel.png"}
	sort.Strings(want)

	// Treaddir's Count is a wire BYTE budget (the vendored library's own
	// rreaddir.encode truncates the entry list to fit within it) — the
	// exact confusion the P2 commit message names as a real bug it found
	// during development. relayfs's own Readdir(offset, count) doc comment
	// treats the same numeric value as an ENTRY-count bound on server-side
	// work instead (a client asking for more entries than exist does no
	// extra Lstat work, but that isn't observable from here). 80 bytes is
	// small enough to carry only two or three of this directory's ~24-36
	// byte-encoded Dirents per round trip, which is what actually forces
	// this test through multiple Readdir calls.
	const wireByteBudget = 80

	var got []string
	var offset uint64
	for i := 0; i < 100; i++ {
		ents, err := dir.Readdir(offset, wireByteBudget)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Readdir(offset=%d): %v", offset, err)
		}
		if len(ents) == 0 {
			break
		}
		for _, e := range ents {
			got = append(got, e.Name)
			offset = e.Offset
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if i == 99 {
			t.Fatalf("Readdir did not terminate after 100 pages")
		}
	}

	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d entries %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry set mismatch at index %d: got %v, want %v (dup or gap)", i, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Close must not leak: open-read-close a few hundred times.
// ---------------------------------------------------------------------------

func TestRelayfs_Close_ManyOpensDoNotLeak(t *testing.T) {
	h := newHarness(t, true)
	const iterations = 300
	for i := 0; i < iterations; i++ {
		f := h.walk(t, "notes/config.txt")
		if _, _, err := f.Open(p9.ReadOnly); err != nil {
			t.Fatalf("Open at iteration %d: %v", i, err)
		}
		buf := make([]byte, 4)
		if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt at iteration %d: %v", i, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close at iteration %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Xattrs — driven directly against the unexported *file type, not through
// the client (see the package doc comment at the top of this file for why).
// ---------------------------------------------------------------------------

func newDirectRoot(t *testing.T, write bool) (*Root, string) {
	t.Helper()
	fixtureRoot := buildFixture(t)
	root, err := Open(fixtureRoot, Policy{Write: write}, NopHooks{})
	if err != nil {
		t.Fatalf("relayfs.Open: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root, fixtureRoot
}

func TestRelayfs_Xattr_AppleNamespaceRefusedBothDirections(t *testing.T) {
	root, _ := newDirectRoot(t, true)
	f := &file{r: root, rel: "src/attrs.txt"}

	wantErrno(t, f.SetXattr("com.apple.quarantine", []byte("0081;00000000;Chrome;"), 0), linux.EPERM, "SetXattr(com.apple.quarantine)")
	_, getErr := f.GetXattr("com.apple.quarantine")
	wantErrno(t, getErr, linux.EPERM, "GetXattr(com.apple.quarantine)")
	wantErrno(t, f.RemoveXattr("com.apple.quarantine"), linux.EPERM, "RemoveXattr(com.apple.quarantine)")
}

func TestRelayfs_Xattr_OrdinaryUserAttrRoundTrips(t *testing.T) {
	root, _ := newDirectRoot(t, true)
	f := &file{r: root, rel: "src/attrs.txt"}

	const name, value = "user.relayfs-test-marker", "payload-value"
	if err := f.SetXattr(name, []byte(value), 0); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	got, err := f.GetXattr(name)
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if string(got) != value {
		t.Fatalf("GetXattr = %q, want %q", got, value)
	}

	names, err := f.ListXattrs()
	if err != nil {
		t.Fatalf("ListXattrs: %v", err)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
		if isAppleXattr(n) {
			t.Fatalf("ListXattrs returned an apple-namespace name: %q", n)
		}
	}
	if !found {
		t.Fatalf("ListXattrs = %v, missing %q", names, name)
	}

	if err := f.RemoveXattr(name); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
	_, err = f.GetXattr(name)
	wantErrno(t, err, linux.ENODATA, "GetXattr after RemoveXattr")
}

func TestRelayfs_Xattr_ReadOnlyMountRefusesMutation(t *testing.T) {
	root, _ := newDirectRoot(t, false)
	f := &file{r: root, rel: "src/attrs.txt"}

	wantErrno(t, f.SetXattr("user.nope", []byte("x"), 0), linux.EROFS, "SetXattr on a read-only mount")
	wantErrno(t, f.RemoveXattr("com.example.marker"), linux.EROFS, "RemoveXattr on a read-only mount")

	// Reads are unaffected by the read-only policy — GetXattr/ListXattrs
	// have no refuseReadOnly check at all, matching the read-only mount
	// contract of "reads still work".
	if _, err := f.GetXattr("com.example.marker"); err != nil {
		t.Fatalf("GetXattr on a read-only mount for a real attribute: %v", err)
	}
	if _, err := f.ListXattrs(); err != nil {
		t.Fatalf("ListXattrs on a read-only mount: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Not reachable through a real client, noted rather than forced:
//
// "rmdir of the mount root itself" has no corresponding client operation.
// UnlinkAt always names a child of the fid it is called on (see
// p9.File.UnlinkAt's own doc: "name must be a file relative to this
// directory"); there is no fid representing the root's own parent for a
// client to call UnlinkAt through, on this attach or any other, so this
// containment-table row does not correspond to anything a real 9P client
// can do. Root.Close (the host-side teardown when a mount session ends) is
// exercised implicitly by every test's harness cleanup instead.
// ---------------------------------------------------------------------------
