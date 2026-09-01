package mcpbroker

// Small duplicates of cmd/relay's own support_test.go helpers. A _test.go
// file's symbols do not cross a package boundary, and these are a handful of
// lines each, so internal/enrolment and internal/project carry their own
// copies too rather than have main export a test seam for them.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate support_test.go")
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root containing go.mod")
		}
	}
}

// mkShortTempDir creates a tempdir under /tmp (short paths) and registers
// cleanup. Use instead of t.TempDir() whenever the dir holds a Unix
// socket — macOS caps sun_path at 104 chars.
func mkShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkShortTempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mkEmptySandboxRelayHome redirects bridge.ConfigDir() away from the
// operator's real one, per cmd/relay/support_test.go's headline rule. Empty
// rather than fixture-populated: nothing here reads settings.json, only
// writes a sandbox profile beside it.
func mkEmptySandboxRelayHome(t *testing.T) string {
	t.Helper()
	dir := mkShortTempDir(t, "relay-home-empty-")
	bridge.SetConfigDirForTest(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })
	return dir
}

var (
	testmcpBinOnce sync.Once
	testmcpBinPath string
	testmcpBinErr  error
)

func buildTestMcpBinary(t *testing.T) string {
	t.Helper()
	testmcpBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "testmcp-bin-")
		if err != nil {
			testmcpBinErr = err
			return
		}
		path := filepath.Join(dir, "testmcp")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/testmcp")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			testmcpBinErr = err
			return
		}
		testmcpBinPath = path
	})
	if testmcpBinErr != nil {
		t.Fatalf("build cmd/testmcp: %v", testmcpBinErr)
	}
	return testmcpBinPath
}

func ptr[T any](v T) *T { return &v }
