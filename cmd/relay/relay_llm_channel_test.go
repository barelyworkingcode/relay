package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestFrontendEnvIsServiceAgnostic(t *testing.T) {
	endpoint := Endpoint{Socket: "/tmp/fe.sock", Token: "fe-token"}
	got := endpoint.FrontendEnv()
	want := map[string]string{
		EnvFrontendSocket: "/tmp/fe.sock",
		EnvFrontendToken:  "fe-token",
	}
	if len(got) != len(want) {
		t.Fatalf("FrontendEnv: len=%d want=%d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("FrontendEnv[%q]=%q want=%q", k, got[k], v)
		}
	}
}

func TestFrontendChannelEnsureIsIdempotent(t *testing.T) {
	mkEmptySandboxRelayHome(t) // Ensure now scans ConfigDir (pruneStaleFrontendSockets)
	c := NewFrontendChannel()
	e1, err := c.Ensure()
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if e1.Token == "" || e1.Socket == "" {
		t.Fatalf("Ensure returned empty values: %+v", e1)
	}

	e2, err := c.Ensure()
	if err != nil {
		t.Fatalf("second Ensure failed: %v", err)
	}
	if e1 != e2 {
		t.Errorf("Ensure not idempotent")
	}
	c.Close()
	if _, err := os.Stat(e1.Socket); !os.IsNotExist(err) {
		t.Errorf("Close did not unlink socket file: %v (path=%s)", err, e1.Socket)
	}
}

func TestFrontendChannelTokenIsLongHex(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	c := NewFrontendChannel()
	endpoint, err := c.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 32 random bytes → 64 hex chars
	if len(endpoint.Token) != 64 {
		t.Errorf("expected 64-char token, got %d: %q", len(endpoint.Token), endpoint.Token)
	}
	for _, ch := range endpoint.Token {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Errorf("token contains non-hex char %q", ch)
			break
		}
	}
}

func TestFrontendChannelEnsureConcurrent(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	c := NewFrontendChannel()
	defer c.Close()

	const N = 64
	var wg sync.WaitGroup
	results := make([]Endpoint, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			endpoint, err := c.Ensure()
			if err != nil {
				t.Errorf("ensure failed: %v", err)
				return
			}
			results[idx] = endpoint
		}(i)
	}
	wg.Wait()

	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent Ensure produced divergent values at index %d", i)
		}
	}
}

// TestPruneStaleFrontendSockets_RemovesOnlyDeadPids is item 8's regression
// test: a relay-frontend-<pid>.sock left behind by a crashed or force-quit
// instance is removed, but a live one -- and anything that merely looks
// like one -- is left alone.
func TestPruneStaleFrontendSockets_RemovesOnlyDeadPids(t *testing.T) {
	dir := t.TempDir()

	// A pid that is guaranteed to no longer exist: run a real, short-lived
	// process to completion and reuse its now-dead pid. Far more reliable
	// than picking an arbitrary large number, which risks colliding with an
	// actually-running process on a busy box.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run `true`: %v", err)
	}
	deadPid := cmd.Process.Pid

	livePath := filepath.Join(dir, socketNameFor(os.Getpid()))
	deadPath := filepath.Join(dir, socketNameFor(deadPid))
	unrelatedPath := filepath.Join(dir, "not-a-frontend-socket.sock")
	// A directory whose name happens to match the naming pattern -- IsDir()
	// must be checked before the pid is ever parsed out of it.
	dirLookalike := filepath.Join(dir, "relay-frontend-424242.sock")

	for _, p := range []string{livePath, deadPath, unrelatedPath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	if err := os.Mkdir(dirLookalike, 0o700); err != nil {
		t.Fatalf("seed dir %s: %v", dirLookalike, err)
	}

	pruneStaleFrontendSockets(dir)

	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("expected the live pid's socket to survive, stat err=%v", err)
	}
	if _, err := os.Stat(unrelatedPath); err != nil {
		t.Errorf("expected the unrelated file to survive, stat err=%v", err)
	}
	if _, err := os.Stat(dirLookalike); err != nil {
		t.Errorf("expected the directory to survive, stat err=%v", err)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Errorf("expected the dead pid's socket to be removed, stat err=%v", err)
	}
}

func socketNameFor(pid int) string {
	return fmt.Sprintf("relay-frontend-%d.sock", pid)
}
