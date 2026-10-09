package projectfs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// awaitEvent returns the first event for path; the context bound is only the
// failure case.
func awaitEvent(t *testing.T, events <-chan projectfs.Event, path string) projectfs.Event {
	t.Helper()
	ctx := ctxT(t)
	for {
		select {
		case e := <-events:
			if e.Path == path {
				return e
			}
		case <-ctx.Done():
			t.Fatalf("no event for %s", path)
		}
	}
}

func watchRoot(t *testing.T, root string) <-chan projectfs.Event {
	t.Helper()
	b, err := projectfs.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan projectfs.Event, 256)
	stop, err := b.Watch(ctxT(t), func(e projectfs.Event) {
		select {
		case events <- e:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return events
}

// The test root is under /var, a link to /private/var: event paths must still
// come back root-relative, and create and remove are rename events.
func TestFSEventsPathsAreRootRelative(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "sub", "existing.txt"), "v1")
	events := watchRoot(t, root)

	put(t, filepath.Join(root, "sub", "created.txt"), "new")
	if e := awaitEvent(t, events, "sub/created.txt"); e.Kind != projectfs.KindRename {
		t.Errorf("create kind = %q, want rename", e.Kind)
	}

	// FSEvents keeps a created flag on a path for some seconds, so a fresh
	// file's modification can arrive as rename; only the path is pinned here.
	f, err := os.OpenFile(filepath.Join(root, "sub", "existing.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("more")
	f.Close()
	if e := awaitEvent(t, events, "sub/existing.txt"); e.Kind != projectfs.KindChange && e.Kind != projectfs.KindRename {
		t.Errorf("modify kind = %q", e.Kind)
	}

	if err := os.Remove(filepath.Join(root, "sub", "created.txt")); err != nil {
		t.Fatal(err)
	}
	for {
		e := awaitEvent(t, events, "sub/created.txt")
		if e.Kind == projectfs.KindRename {
			break
		}
	}
}

// .git and node_modules are forwarded, unfiltered: eve decides what to ignore.
func TestFSEventsForwardsGitAndNodeModules(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".git", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	events := watchRoot(t, root)
	put(t, filepath.Join(root, ".git", "HEAD"), "ref")
	put(t, filepath.Join(root, "node_modules", "m.js"), "x")
	awaitEvent(t, events, ".git/HEAD")
	awaitEvent(t, events, "node_modules/m.js")
}
