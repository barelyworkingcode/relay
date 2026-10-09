package projectfs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// failBound is only the upper bound for a wait; every wait names its signal.
const failBound = 60 * time.Second

type makeBackend func(root string) projectfs.Backend

// eachBackend runs body once for the console backend and once for the host
// backend. The host leg runs the embedded agent through a local shell, with
// real node, and skips only when node is absent.
func eachBackend(t *testing.T, body func(t *testing.T, kind string, mk makeBackend)) {
	t.Helper()
	t.Run("console", func(t *testing.T) {
		body(t, "console", func(root string) projectfs.Backend {
			b, err := projectfs.NewLocal(root)
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			return b
		})
	})
	t.Run("host", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Fatal("node is not installed")
		}
		body(t, "host", func(root string) projectfs.Backend {
			pool := newLocalPool(projectfs.HostPoolOptions{})
			t.Cleanup(func() { pool.Drop("h1") })
			return pool.Backend(localHost(node), root)
		})
	})
}

func localHost(node string) config.Host {
	return config.Host{ID: "h1", Name: "testbox", Target: "testbox", Probe: &config.HostProbe{NodePath: node}}
}

// newLocalPool builds a pool whose agent runs the launcher in a local shell.
func newLocalPool(o projectfs.HostPoolOptions) *projectfs.HostPool {
	if o.Argv == nil {
		o.Argv = func(_ config.Host, launcher string) ([]string, error) {
			return []string{"/bin/sh", "-c", launcher}, nil
		}
	}
	return projectfs.NewHostPool(o)
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var fe *projectfs.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want error code %s, got %v", code, err)
	}
	if fe.Code != code {
		t.Fatalf("want error code %s, got %s (%s)", code, fe.Code, fe.Msg)
	}
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func slurp(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// linkFixture lays out a root with real/inner.txt, link -> real (a directory
// link), flink -> real/inner.txt (a file link), and an outside directory that
// the links point at when escaping is the question.
type linkFixture struct{ root, outside string }

func newLinkFixture(t *testing.T) linkFixture {
	t.Helper()
	f := linkFixture{root: t.TempDir(), outside: t.TempDir()}
	put(t, filepath.Join(f.root, "real", "inner.txt"), "inner")
	put(t, filepath.Join(f.root, "plain.txt"), "plain")
	put(t, filepath.Join(f.outside, "secret.txt"), "secret")
	for name, target := range map[string]string{
		"link": "real", "flink": "real/inner.txt", "escape": f.outside,
	} {
		if err := os.Symlink(target, filepath.Join(f.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func entryMap(es []projectfs.Entry) map[string]projectfs.Entry {
	m := map[string]projectfs.Entry{}
	for _, e := range es {
		m[e.Name] = e
	}
	return m
}

func TestConformance_List(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, ".hidden"), "h")
		b := mk(f.root)
		ctx := ctxT(t)

		es, err := b.List(ctx, "", false)
		if err != nil {
			t.Fatal(err)
		}
		m := entryMap(es)
		if _, ok := m[".hidden"]; ok {
			t.Error("hidden entry listed with show_hidden=false")
		}
		if e := m["plain.txt"]; e.Type != projectfs.TypeFile || e.Size != 5 || e.MtimeMs <= 0 {
			t.Errorf("plain.txt = %+v", e)
		}
		if m["real"].Type != projectfs.TypeDirectory {
			t.Errorf("real = %+v", m["real"])
		}
		for _, n := range []string{"link", "flink", "escape"} {
			if m[n].Type != projectfs.TypeSymlink {
				t.Errorf("%s type = %q, want symlink", n, m[n].Type)
			}
		}

		es, err = b.List(ctx, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := entryMap(es)[".hidden"]; !ok {
			t.Error("hidden entry missing with show_hidden=true")
		}

		for _, link := range []string{"link", "escape", "link/"} {
			es, err = b.List(ctx, strings.TrimSuffix(link, "/"), false)
			for _, e := range es {
				if e.Name == "inner.txt" || e.Name == "secret.txt" {
					t.Errorf("list descended into %s: %v", link, es)
				}
			}
			_ = err
		}

		_, err = b.List(ctx, "missing", false)
		wantCode(t, err, projectfs.CodeENOENT)
		_, err = b.List(ctx, "plain.txt", false)
		wantCode(t, err, projectfs.CodeENOTDIR)
	})
}

func TestConformance_Stat(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		b := mk(f.root)
		ctx := ctxT(t)

		in, err := b.Stat(ctx, "plain.txt")
		if err != nil || in.Type != projectfs.TypeFile || in.Size != 5 || in.MtimeMs <= 0 {
			t.Errorf("file stat = %+v, %v", in, err)
		}
		in, err = b.Stat(ctx, "real")
		if err != nil || in.Type != projectfs.TypeDirectory {
			t.Errorf("dir stat = %+v, %v", in, err)
		}
		in, err = b.Stat(ctx, "")
		if err != nil || in.Type != projectfs.TypeDirectory {
			t.Errorf("root stat = %+v, %v", in, err)
		}
		_, err = b.Stat(ctx, "missing")
		wantCode(t, err, projectfs.CodeENOENT)
		for _, p := range []string{"flink", "link", "link/inner.txt", "escape/secret.txt"} {
			_, err = b.Stat(ctx, p)
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("stat %s: want SYMLINK, got %v", p, err)
			}
		}
	})
}

func TestConformance_Read(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		b := mk(f.root)
		ctx := ctxT(t)

		content, size, err := b.Read(ctx, "plain.txt", projectfs.MaxReadBytes)
		if err != nil || content != "plain" || size != 5 {
			t.Errorf("read = %q, %d, %v", content, size, err)
		}
		_, _, err = b.Read(ctx, "plain.txt", 3)
		wantCode(t, err, projectfs.CodeTooLarge)
		var fe *projectfs.Error
		if errors.As(err, &fe) && fe.Size != 5 {
			t.Errorf("TOO_LARGE size = %d, want 5", fe.Size)
		}
		_, _, err = b.Read(ctx, "missing", 0)
		wantCode(t, err, projectfs.CodeENOENT)
		_, _, err = b.Read(ctx, "real", 0)
		wantCode(t, err, projectfs.CodeEISDIR)
		for _, p := range []string{"flink", "link/inner.txt", "escape/secret.txt"} {
			_, _, err = b.Read(ctx, p, 0)
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("read %s: want SYMLINK, got %v", p, err)
			}
		}
	})
}

func TestConformance_Open(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, "range.txt"), "0123456789")
		b := mk(f.root)
		ctx := ctxT(t)

		rc, in, err := b.Open(ctx, "range.txt")
		if err != nil {
			t.Fatal(err)
		}
		if in.Size != 10 || in.Type != projectfs.TypeFile {
			t.Errorf("open info = %+v", in)
		}
		if kind == "console" {
			// Range on the console is served from the file itself.
			rs, ok := rc.(io.ReadSeeker)
			if !ok {
				t.Fatalf("console Open returned %T, want an io.ReadSeeker", rc)
			}
			if _, err := rs.Seek(4, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(io.LimitReader(rs, 3))
			if string(got) != "456" {
				t.Errorf("ranged read = %q, want 456", got)
			}
		} else {
			got, err := io.ReadAll(rc)
			if err != nil || string(got) != "0123456789" {
				t.Errorf("stream = %q, %v", got, err)
			}
		}
		rc.Close()

		for p, code := range map[string]string{
			"missing": projectfs.CodeENOENT, "real": projectfs.CodeEISDIR,
			"flink": projectfs.CodeSymlink, "link/inner.txt": projectfs.CodeSymlink,
			"escape/secret.txt": projectfs.CodeSymlink,
		} {
			if rc, _, err := b.Open(ctx, p); err == nil {
				rc.Close()
				t.Errorf("open %s succeeded, want %s", p, code)
			} else if projectfs.CodeOf(err) != code {
				t.Errorf("open %s: want %s, got %v", p, code, err)
			}
		}
	})
}

func TestConformance_Write(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		b := mk(f.root)
		ctx := ctxT(t)

		if err := b.Write(ctx, "new.txt", []byte("héllo"), projectfs.WriteOpts{Encoding: "utf8"}); err != nil {
			t.Fatal(err)
		}
		if got := slurp(t, filepath.Join(f.root, "new.txt")); got != "héllo" {
			t.Errorf("new.txt = %q", got)
		}
		if err := b.Write(ctx, "new.txt", []byte("v2"), projectfs.WriteOpts{}); err != nil {
			t.Fatal(err)
		}
		if got := slurp(t, filepath.Join(f.root, "new.txt")); got != "v2" {
			t.Errorf("overwrite left %q", got)
		}
		bin := []byte{0xff, 0xfe, 0x00, 0x80}
		if err := b.Write(ctx, "bin.dat", bin, projectfs.WriteOpts{Encoding: "base64"}); err != nil {
			t.Fatal(err)
		}
		if got := slurp(t, filepath.Join(f.root, "bin.dat")); got != string(bin) {
			t.Errorf("binary write = %q", got)
		}

		err := b.Write(ctx, "new.txt", []byte("x"), projectfs.WriteOpts{CreateOnly: true})
		wantCode(t, err, projectfs.CodeEEXIST)
		if got := slurp(t, filepath.Join(f.root, "new.txt")); got != "v2" {
			t.Errorf("create_only clobbered the file: %q", got)
		}
		if err := b.Write(ctx, "fresh.txt", []byte("x"), projectfs.WriteOpts{CreateOnly: true}); err != nil {
			t.Errorf("create_only on a new name: %v", err)
		}

		wantCode(t, b.Write(ctx, "nodir/a.txt", []byte("x"), projectfs.WriteOpts{}), projectfs.CodeENOENT)
		if exists(filepath.Join(f.root, "nodir")) {
			t.Error("write created a missing parent")
		}
		wantCode(t, b.Write(ctx, "real", []byte("x"), projectfs.WriteOpts{}), projectfs.CodeEISDIR)

		for _, p := range []string{"flink", "link/new.txt", "escape/new.txt", "escape/secret.txt"} {
			if got := projectfs.CodeOf(b.Write(ctx, p, []byte("pwn"), projectfs.WriteOpts{})); got != projectfs.CodeSymlink {
				t.Errorf("write %s: want SYMLINK, got %s", p, got)
			}
		}
		if slurp(t, filepath.Join(f.root, "real", "inner.txt")) != "inner" ||
			slurp(t, filepath.Join(f.outside, "secret.txt")) != "secret" || exists(filepath.Join(f.outside, "new.txt")) {
			t.Error("a write went through a symbolic link")
		}

		big := make([]byte, projectfs.MaxWriteBytes+1)
		err = b.Write(ctx, "big.bin", big, projectfs.WriteOpts{Encoding: "base64"})
		wantCode(t, err, projectfs.CodeTooLarge)
	})
}

func TestConformance_Mkdir(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		b := mk(f.root)
		ctx := ctxT(t)

		p, err := b.Mkdir(ctx, "real", "lib")
		if err != nil || p != "real/lib" {
			t.Fatalf("mkdir = %q, %v", p, err)
		}
		if fi, err := os.Stat(filepath.Join(f.root, "real", "lib")); err != nil || !fi.IsDir() {
			t.Error("directory not created")
		}
		p, err = b.Mkdir(ctx, "", "top")
		if err != nil || p != "top" {
			t.Errorf("mkdir at root = %q, %v", p, err)
		}
		_, err = b.Mkdir(ctx, "real", "lib")
		wantCode(t, err, projectfs.CodeEEXIST)
		_, err = b.Mkdir(ctx, "nodir", "x")
		wantCode(t, err, projectfs.CodeENOENT)
		for _, n := range []string{"", ".", "..", "a/b"} {
			_, err = b.Mkdir(ctx, "", n)
			if projectfs.CodeOf(err) != projectfs.CodeInvalid {
				t.Errorf("mkdir name %q: want INVALID, got %v", n, err)
			}
		}
		for _, parent := range []string{"link", "escape"} {
			_, err = b.Mkdir(ctx, parent, "x")
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("mkdir under %s: want SYMLINK, got %v", parent, err)
			}
		}
		if exists(filepath.Join(f.outside, "x")) || exists(filepath.Join(f.root, "real", "x")) {
			t.Error("mkdir went through a symbolic link")
		}
	})
}

func TestConformance_Rename(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, "d", "a.txt"), "A")
		put(t, filepath.Join(f.root, "d", "taken.txt"), "T")
		b := mk(f.root)
		ctx := ctxT(t)

		p, err := b.Rename(ctx, "d/a.txt", "b.txt")
		if err != nil || p != "d/b.txt" {
			t.Fatalf("rename = %q, %v", p, err)
		}
		if exists(filepath.Join(f.root, "d", "a.txt")) || slurp(t, filepath.Join(f.root, "d", "b.txt")) != "A" {
			t.Error("rename did not move the entry")
		}

		_, err = b.Rename(ctx, "d/b.txt", "taken.txt")
		wantCode(t, err, projectfs.CodeEEXIST)
		if slurp(t, filepath.Join(f.root, "d", "taken.txt")) != "T" || !exists(filepath.Join(f.root, "d", "b.txt")) {
			t.Error("refused rename changed the tree")
		}

		p, err = b.Rename(ctx, "d/b.txt", "B.txt")
		if err != nil || p != "d/B.txt" {
			t.Fatalf("case-only rename = %q, %v", p, err)
		}
		ents, err := os.ReadDir(filepath.Join(f.root, "d"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if !slices.Contains(names, "B.txt") || slices.Contains(names, "b.txt") {
			t.Errorf("case-only rename left %v", names)
		}

		_, err = b.Rename(ctx, "d/missing", "x")
		wantCode(t, err, projectfs.CodeENOENT)
		for _, n := range []string{"", ".", "..", "x/y"} {
			_, err = b.Rename(ctx, "plain.txt", n)
			if projectfs.CodeOf(err) != projectfs.CodeInvalid {
				t.Errorf("rename to %q: want INVALID, got %v", n, err)
			}
		}
		for _, src := range []string{"flink", "link", "link/inner.txt", "escape/secret.txt"} {
			_, err = b.Rename(ctx, src, "moved")
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("rename %s: want SYMLINK, got %v", src, err)
			}
		}
		if !exists(filepath.Join(f.root, "flink")) || !exists(filepath.Join(f.outside, "secret.txt")) ||
			exists(filepath.Join(f.root, "moved")) {
			t.Error("a refused rename changed the tree")
		}
	})
}

func TestConformance_Move(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, "src", "a.txt"), "A")
		put(t, filepath.Join(f.root, "src", "t.txt"), "src-T")
		put(t, filepath.Join(f.root, "lib", "t.txt"), "lib-T")
		b := mk(f.root)
		ctx := ctxT(t)

		p, err := b.Move(ctx, "src/a.txt", "lib")
		if err != nil || p != "lib/a.txt" {
			t.Fatalf("move = %q, %v", p, err)
		}
		if exists(filepath.Join(f.root, "src", "a.txt")) || slurp(t, filepath.Join(f.root, "lib", "a.txt")) != "A" {
			t.Error("move did not relocate the entry")
		}

		_, err = b.Move(ctx, "src/t.txt", "lib")
		wantCode(t, err, projectfs.CodeEEXIST)
		if slurp(t, filepath.Join(f.root, "lib", "t.txt")) != "lib-T" || !exists(filepath.Join(f.root, "src", "t.txt")) {
			t.Error("refused move changed the tree")
		}

		_, err = b.Move(ctx, "src/missing", "lib")
		wantCode(t, err, projectfs.CodeENOENT)
		_, err = b.Move(ctx, "plain.txt", "nodir")
		wantCode(t, err, projectfs.CodeENOENT)
		for _, src := range []string{"flink", "link", "link/inner.txt"} {
			_, err = b.Move(ctx, src, "lib")
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("move %s: want SYMLINK, got %v", src, err)
			}
		}
		for _, dst := range []string{"link", "escape"} {
			_, err = b.Move(ctx, "plain.txt", dst)
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("move into %s: want SYMLINK, got %v", dst, err)
			}
		}
		if !exists(filepath.Join(f.root, "plain.txt")) || exists(filepath.Join(f.outside, "plain.txt")) ||
			exists(filepath.Join(f.root, "real", "plain.txt")) {
			t.Error("a refused move changed the tree")
		}
	})
}

func TestConformance_Delete(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, "tree", "deep", "x.txt"), "x")
		// A unique name lets the console leg find and remove what it put in
		// the Trash, and nothing else.
		uniq := "relay-conformance-" + filepath.Base(f.root) + ".txt"
		put(t, filepath.Join(f.root, uniq), "trash-me")
		b := mk(f.root)
		ctx := ctxT(t)

		trashed, err := b.Delete(ctx, uniq)
		if err != nil {
			if kind == "console" && projectfs.CodeOf(err) == projectfs.CodeEACCES {
				t.Fatalf("Trash is not writable here: %v", err)
			}
			t.Fatal(err)
		}
		if kind == "console" {
			if home, herr := os.UserHomeDir(); herr == nil {
				leftover := filepath.Join(home, ".Trash", uniq)
				if fi, serr := os.Lstat(leftover); serr == nil && fi.Mode().IsRegular() && slurp(t, leftover) == "trash-me" {
					t.Cleanup(func() { _ = os.Remove(leftover) })
				}
			}
		}
		if wantTrashed := kind == "console"; trashed != wantTrashed {
			t.Errorf("trashed = %v, want %v", trashed, wantTrashed)
		}
		if exists(filepath.Join(f.root, uniq)) {
			t.Error("deleted entry is still in place")
		}

		if kind == "host" {
			trashed, err = b.Delete(ctx, "tree")
			if err != nil || trashed || exists(filepath.Join(f.root, "tree")) {
				t.Errorf("host delete of a directory = %v, %v", trashed, err)
			}
		}

		_, err = b.Delete(ctx, "missing")
		wantCode(t, err, projectfs.CodeENOENT)
		for _, p := range []string{"flink", "link", "link/inner.txt", "escape/secret.txt"} {
			_, err = b.Delete(ctx, p)
			if projectfs.CodeOf(err) != projectfs.CodeSymlink {
				t.Errorf("delete %s: want SYMLINK, got %v", p, err)
			}
		}
		for _, p := range []string{filepath.Join(f.root, "flink"), filepath.Join(f.root, "link"),
			filepath.Join(f.root, "real", "inner.txt"), filepath.Join(f.outside, "secret.txt")} {
			if !exists(p) {
				t.Errorf("a refused delete removed %s", filepath.Base(p))
			}
		}
	})
}

func TestConformance_Search(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		f := newLinkFixture(t)
		put(t, filepath.Join(f.root, "src", "a.js"), "one\n// TODO first\n")
		put(t, filepath.Join(f.root, "bin.dat"), "TODO\x00binary")
		put(t, filepath.Join(f.root, "real", "inner.txt"), "TODO inner")
		put(t, filepath.Join(f.outside, "secret.txt"), "TODO secret")
		b := mk(f.root)
		ctx := ctxT(t)

		ms, trunc, err := b.Search(ctx, projectfs.SearchOpts{Query: "TODO"})
		if err != nil || trunc {
			t.Fatalf("search = %v, truncated %v", err, trunc)
		}
		got := paths(ms)
		if strings.Join(got, ",") != "real/inner.txt,src/a.js" {
			t.Errorf("matched files = %v; symlinks and binary files must be skipped", got)
		}

		ms, trunc, err = b.Search(ctx, projectfs.SearchOpts{Query: "TODO", MaxMatches: 1})
		if err != nil || !trunc || len(ms) != 1 {
			t.Errorf("max_matches=1: %d matches, truncated %v, %v", len(ms), trunc, err)
		}

		put(t, filepath.Join(f.root, "multi.txt"), "ab ab ab\n")
		ms, _, err = b.Search(ctx, projectfs.SearchOpts{Query: "ab", Globs: []string{"multi.txt"}})
		if err != nil || len(ms) != 3 || ms[0].Col != 1 || ms[1].Col != 4 || ms[2].Col != 7 {
			t.Errorf("every match on a line must be returned: %v, %v", ms, err)
		}

		for name, o := range map[string]projectfs.SearchOpts{
			"empty query": {Query: ""},
			"long query":  {Query: strings.Repeat("a", projectfs.MaxQueryLen+1)},
			"bad regex":   {Query: "(", Regex: true},
			"six globs":   {Query: "a", Globs: []string{"a", "b", "c", "d", "e", "f"}},
		} {
			if _, _, err := b.Search(ctx, o); projectfs.CodeOf(err) != projectfs.CodeInvalid {
				t.Errorf("%s: want INVALID, got %v", name, err)
			}
		}
	})
}

func paths(ms []projectfs.Match) []string {
	seen := map[string]bool{}
	for _, m := range ms {
		seen[m.Path] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is not installed")
	}
}

func runIn(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func TestConformance_SearchIgnoreRules(t *testing.T) {
	requireGit(t)
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		ctx := ctxT(t)

		t.Run("inside a git repo", func(t *testing.T) {
			root := t.TempDir()
			runIn(t, root, "git", "init", "-q")
			put(t, filepath.Join(root, ".gitignore"), "ignored.txt\nbuild/\n")
			put(t, filepath.Join(root, "kept.txt"), "NEEDLE kept")
			put(t, filepath.Join(root, "untracked", "u.txt"), "NEEDLE untracked")
			put(t, filepath.Join(root, ".dotfile"), "NEEDLE dot")
			put(t, filepath.Join(root, "ignored.txt"), "NEEDLE ignored")
			put(t, filepath.Join(root, "build", "out.txt"), "NEEDLE built")
			put(t, filepath.Join(root, "node_modules", "m.txt"), "NEEDLE module")
			ms, _, err := mk(root).Search(ctx, projectfs.SearchOpts{Query: "NEEDLE"})
			if err != nil {
				t.Fatal(err)
			}
			// git's own rules decide: ignored files are out, hidden and
			// node_modules are not special, .git is never listed.
			got := strings.Join(paths(ms), ",")
			for _, p := range []string{".dotfile", "kept.txt", "untracked/u.txt"} {
				if !strings.Contains(got, p) {
					t.Errorf("git repo search missed %s: %s", p, got)
				}
			}
			for _, p := range []string{"ignored.txt", "build/out.txt", ".git/"} {
				if strings.Contains(got, p) {
					t.Errorf("git repo search returned ignored %s: %s", p, got)
				}
			}
		})

		t.Run("outside a git repo", func(t *testing.T) {
			root := t.TempDir()
			put(t, filepath.Join(root, "kept.txt"), "NEEDLE kept")
			put(t, filepath.Join(root, "sub", "s.txt"), "NEEDLE sub")
			put(t, filepath.Join(root, ".hidden.txt"), "NEEDLE hidden")
			put(t, filepath.Join(root, ".hiddendir", "h.txt"), "NEEDLE hidden dir")
			put(t, filepath.Join(root, ".git", "config"), "NEEDLE git")
			put(t, filepath.Join(root, "node_modules", "m.txt"), "NEEDLE module")
			put(t, filepath.Join(root, "sub", "node_modules", "m.txt"), "NEEDLE nested module")
			ms, _, err := mk(root).Search(ctx, projectfs.SearchOpts{Query: "NEEDLE"})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(paths(ms), ","); got != "kept.txt,sub/s.txt" {
				t.Errorf("walk search = %s, want kept.txt,sub/s.txt", got)
			}
		})
	})
}

func gitInit(t *testing.T, root string) {
	t.Helper()
	runIn(t, root, "git", "init", "-q", "-b", "main")
	runIn(t, root, "git", "config", "user.email", "acme@example.invalid")
	runIn(t, root, "git", "config", "user.name", "Acme")
}

func TestConformance_Git(t *testing.T) {
	requireGit(t)
	repo, plain := t.TempDir(), t.TempDir()
	gitInit(t, repo)
	put(t, filepath.Join(repo, "tracked.txt"), "t")
	put(t, filepath.Join(plain, "x.txt"), "x")
	if err := os.Symlink(repo, filepath.Join(plain, "repolink")); err != nil {
		t.Fatal(err)
	}
	// An inherited GIT_DIR would point every git call at the wrong place.
	t.Setenv("GIT_DIR", filepath.Join(plain, "no-such-gitdir"))
	t.Setenv("GIT_WORK_TREE", plain)

	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		ctx := ctxT(t)
		rb, pb := mk(repo), mk(plain)

		r, err := rb.Git(ctx, "", []string{"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all"}, 0)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("git status = %+v, %v", r, err)
		}
		if !strings.Contains(string(r.Stdout), "# branch.head main") || !strings.Contains(string(r.Stdout), "tracked.txt") {
			t.Errorf("status output = %q", r.Stdout)
		}

		// A non-zero exit is a result, not an error.
		r, err = pb.Git(ctx, "", []string{"rev-parse", "--show-toplevel"}, 0)
		if err != nil {
			t.Fatalf("non-repo rev-parse returned an error: %v", err)
		}
		if r.ExitCode == 0 || !strings.Contains(r.Stderr, "not a git repository") {
			t.Errorf("non-repo rev-parse = %+v", r)
		}

		_, err = rb.Git(ctx, "", []string{"commit", "-m", "x"}, 0)
		wantCode(t, err, projectfs.CodeInvalid)
		_, err = rb.Git(ctx, "", []string{"diff", "--output=" + filepath.Join(repo, "pwn")}, 0)
		wantCode(t, err, projectfs.CodeInvalid)
		if exists(filepath.Join(repo, "pwn")) {
			t.Error("a refused git argv ran")
		}
		_, err = pb.Git(ctx, "missing", []string{"status"}, 0)
		wantCode(t, err, projectfs.CodeENOENT)
		_, err = pb.Git(ctx, "repolink", []string{"status"}, 0)
		wantCode(t, err, projectfs.CodeSymlink)
	})
}

func TestConformance_Watch(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		b := mk(root)
		ctx := ctxT(t)
		events := make(chan projectfs.Event, 64)
		stop, err := b.Watch(ctx, func(e projectfs.Event) {
			select {
			case events <- e:
			default:
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()

		// Watch has returned, so the watcher is live: this write must be seen.
		put(t, filepath.Join(root, "sub", "w.txt"), "w")
		deadline := time.After(failBound)
		for {
			select {
			case e := <-events:
				if e.Path != "sub/w.txt" {
					continue
				}
				if e.Kind != projectfs.KindChange && e.Kind != projectfs.KindRename {
					t.Fatalf("kind = %q", e.Kind)
				}
				return
			case <-deadline:
				t.Fatal("no event for sub/w.txt")
			}
		}
	})
}
