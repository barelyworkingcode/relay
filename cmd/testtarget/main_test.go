package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMarker_NeverObservedPartial(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "testtarget")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build testtarget: %v", err)
	}

	const iterations = 400
	for i := 0; i < iterations; i++ {
		marker := filepath.Join(dir, fmt.Sprintf("marker-%d.json", i))
		cmd := exec.Command(bin, "-marker", marker, "-sleep", "100ms")
		if err := cmd.Start(); err != nil {
			t.Fatalf("iteration %d: start testtarget: %v", i, err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })

		// Deliberately no sleep between reads: the window being probed is
		// the gap between the file appearing and its content landing.
		deadline := time.Now().Add(5 * time.Second)
		for {
			b, err := os.ReadFile(marker)
			if errors.Is(err, fs.ErrNotExist) {
				if time.Now().After(deadline) {
					t.Fatalf("iteration %d: marker never appeared", i)
				}
				continue
			}
			if err != nil {
				t.Fatalf("iteration %d: read marker: %v", i, err)
			}
			var m struct {
				PID int `json:"pid"`
			}
			if err := json.Unmarshal(b, &m); err != nil || m.PID == 0 {
				t.Fatalf("iteration %d: marker read as %q, want JSON with a pid (decode err: %v)", i, b, err)
			}
			break
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func TestWriteFileAtomic(t *testing.T) {
	const perm fs.FileMode = 0o640
	want := []byte(`{"pid":42}`)

	for _, tc := range []struct {
		name string
		seed func(t *testing.T, path string)
	}{
		{"creates", func(*testing.T, string) {}},
		{"replaces existing", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("old content, longer than the new"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out.json")
			tc.seed(t, path)

			if err := writeFileAtomic(path, want, perm); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("content = %q, want %q", got, want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != perm {
				t.Errorf("perm = %v, want %v", info.Mode().Perm(), perm)
			}
			assertOnlyEntry(t, dir, "out.json")
		})
	}

	t.Run("failed rename leaves no temp file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "out.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(path, want, perm); err == nil {
			t.Fatal("writeFileAtomic over a directory: want error, got nil")
		}
		assertOnlyEntry(t, dir, "out.json")
	})
}

func assertOnlyEntry(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %q, want only %q", names, name)
	}
}
