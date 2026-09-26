//go:build live

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A subtree grant on a regular file must reach that file and nothing beside
// it: the session launcher relies on this to grant a swapped-in file without
// its atomic-write siblings.
func TestLive_SubpathOnARegularFileGrantsNoSiblings(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root := realTempDir(t)
	home := filepath.Join(root, "home")
	mkdirs(t, home)
	f := filepath.Join(home, "acme")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := Write(filepath.Join(root, "profiles"), "rw-file-session", Spec{
		ReadWrite: []string{f, "/dev"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !runProbe(t, profile, "write "+f) {
		t.Fatal("a write to the granted file was denied; the profile does not load")
	}
	for _, sibling := range []string{f + ".lock", f + ".tmp.1", f + ".backup"} {
		if runProbe(t, profile, "write "+sibling) {
			t.Errorf("creating %s beside the granted file succeeded", filepath.Base(sibling))
		}
		if _, err := os.Stat(sibling); err == nil {
			t.Errorf("the refused write left %s on disk", filepath.Base(sibling))
		}
	}
}
