package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// GOPROXY=off makes any module the build would fetch a hard failure, so a
// pass means the build found every dependency without the network even
// though HOME points at an empty directory.
func TestGoBuildUnderEmptySandboxHomeNeedsNoNetwork(t *testing.T) {
	mkEmptySandboxRelayHome(t)

	out := filepath.Join(t.TempDir(), "relaysessions")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/relaysessions")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/relaysessions under an empty sandbox HOME with GOPROXY=off: %v\n%s", err, output)
	}
}
