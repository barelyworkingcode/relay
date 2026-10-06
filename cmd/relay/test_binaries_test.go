package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// relayBinPath is set by TestMain before m.Run; "" in a re-exec'd helper child.
var relayBinPath string

// buildTestBinaries builds every binary a test runs inside a timed budget
// into dir. This is deliberate: a build started inside a test's own deadline
// spends that deadline on the compiler, so these builds happen in TestMain,
// before any budget opens. repoRoot(t) needs a *testing.T, so the module
// root is found by walking up from the working directory.
func buildTestBinaries(dir string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return fmt.Errorf("no go.mod above the working directory")
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
