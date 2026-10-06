package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// relayBinPath is set by TestMain before m.Run; "" in a re-exec'd helper child.
var relayBinPath string

// buildTestBinaries builds every binary a test runs inside a timed budget
// into dir. This is deliberate: a build started inside a test's own deadline
// spends that deadline on the compiler, so these builds happen in TestMain,
// before any budget opens.
func buildTestBinaries(dir string) error {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("runtime.Caller failed: cannot locate test_binaries_test.go")
	}
	root := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return fmt.Errorf("no go.mod above %s", file)
		}
		root = parent
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "relay"), "./cmd/relay")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build ./cmd/relay: %w", err)
	}
	return nil
}

// relayBinary returns the relay binary TestMain built.
func relayBinary(t testing.TB) string {
	t.Helper()
	if relayBinPath == "" {
		t.Fatal("relay binary not built: TestMain builds it only in the top-level test process")
	}
	return relayBinPath
}
