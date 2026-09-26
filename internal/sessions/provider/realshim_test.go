package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// writeEchoBackScript writes a POSIX sh script standing in for `claude
// --print --output-format stream-json ...` or `pi --mode rpc`: it echoes
// each stdin line back, verbatim, prefixed with a marker, until stdin
// closes, then exits with $TEST_EXIT_CODE (default 0). Used to pin pipe
// mode's wire contract end to end -- byte-identical framing, in order,
// whole lines, EOF on stdin close, exit code propagation -- through a real
// shim, not just through the argv/env builder functions.
func writeEchoBackScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "echoback.sh")
	script := "#!/bin/sh\n" +
		"while IFS= read -r line; do\n" +
		"  printf 'ECHO:%s\\n' \"$line\"\n" +
		"done\n" +
		"exit \"${TEST_EXIT_CODE:-0}\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write echoback script: %v", err)
	}
	return path
}

type rawResult struct {
	output   []byte
	exitCode int
}

// spawnAndCollect starts cmd (already fully configured but not yet
// Started), optionally waits for the real shim's own "started" status event
// first (statusR nil for a plain direct spawn), writes lines to its stdin,
// closes stdin, and fully drains stdout BEFORE calling Wait. Draining before
// Wait is deliberate, not incidental: os/exec's own docs warn that Wait
// closes the pipes it created as soon as it sees the child exit, so a
// caller racing its own drain against Wait can lose already-written output
// out from under a manual StdoutPipe reader -- a real, pre-existing
// property of this pattern, not particular to shim wrapping, and exactly
// the failure mode this test exists to rule out for pipe mode specifically.
// extraFiles (the shim's own secret/status pipe write ends) are closed
// immediately after Start, mirroring buildShimCmd's own documented
// contract.
func spawnAndCollect(t *testing.T, cmd *exec.Cmd, statusR *os.File, extraFiles []*os.File, lines []string) (rawResult, int) {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, f := range extraFiles {
		_ = f.Close()
	}

	targetPID := 0
	if statusR != nil {
		outcome, _ := readShimStatus(statusR)
		_ = statusR.Close()
		if !outcome.started {
			t.Fatalf("shim never reported started: %+v", outcome)
		}
		targetPID = outcome.targetPID
	}

	for _, l := range lines {
		if _, err := stdin.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	data, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}

	exitCode := 0
	if werr := cmd.Wait(); werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("wait: %v", werr)
		}
	}
	return rawResult{output: data, exitCode: exitCode}, targetPID
}

func truncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + fmt.Sprintf("...(%d more bytes)", len(b)-max)
}

// TestClaudeProvider_RealShimSpawn_StreamJSONFramingIsByteIdentical spawns a
// fake CLI (writeEchoBackScript) once directly and once through a real shim
// (a freshly built relay-sessions binary, with a real identity Hello against
// a fake bridge), and asserts the wire framing claude's stream-json
// depends on -- whole lines, in order, a large single line intact,
// stdin-close propagating to child EOF, and exit code propagation -- is
// byte-identical between the two. buildShimCmd is exactly what
// ClaudeProvider.Start calls for its shim-wrapped branch (shimspawn.go), so
// this pins the same contract Start relies on without fighting the
// higher-level provider's own event-translation goroutines, which have
// nothing to do with what this test is checking.
func TestClaudeProvider_RealShimSpawn_StreamJSONFramingIsByteIdentical(t *testing.T) {
	relaySessionsBin := buildRelaySessionsBinary(t)
	bridgeSock, received := startFakeBridge(t)
	scratch := shortTempDir(t)
	script := writeEchoBackScript(t, scratch)
	t.Setenv("TEST_EXIT_CODE", "7")

	lines := []string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		strings.Repeat("x", 1024*1024), // a 1MiB single line must survive whole
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"bye"}]}}`,
	}

	directRes, _ := spawnAndCollect(t, exec.Command(script), nil, nil, lines)

	spec := shimSpec{
		Binary:       relaySessionsBin,
		SessionID:    "claude-realshim-1",
		BridgeSocket: bridgeSock,
		Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("1", 64)},
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, script, nil)
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	cmd.Env = append(os.Environ(), "RELAY_BRIDGE_SOCKET="+bridgeSock, "RELAY_SESSION_ID="+spec.SessionID)

	shimRes, targetPID := spawnAndCollect(t, cmd, statusR, extraFiles, lines)

	select {
	case hello := <-received:
		if hello.Type != "Hello" || hello.Kind != "project_session" || hello.Name != spec.SessionID {
			t.Fatalf("hello = %+v, unexpected shape", hello)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake bridge never received a Hello -- the shim never ran")
	}
	if targetPID == 0 {
		t.Fatal("targetPID never populated -- the shim never reported \"started\"")
	}

	if string(shimRes.output) != string(directRes.output) {
		t.Fatalf("shim-wrapped transcript differs from a direct spawn's\nshim:   %s\ndirect: %s", truncate(shimRes.output), truncate(directRes.output))
	}
	if shimRes.exitCode != 7 || directRes.exitCode != 7 {
		t.Fatalf("exit codes = shim:%d direct:%d, want both 7", shimRes.exitCode, directRes.exitCode)
	}
}

// TestPiProvider_RealShimSpawn_JSONLFramingIsByteIdentical is the claude
// framing test's exact mirror against `pi --mode rpc`'s own JSONL shape --
// same buildShimCmd, same real shim binary, same real identity Hello.
func TestPiProvider_RealShimSpawn_JSONLFramingIsByteIdentical(t *testing.T) {
	relaySessionsBin := buildRelaySessionsBinary(t)
	bridgeSock, received := startFakeBridge(t)
	scratch := shortTempDir(t)
	script := writeEchoBackScript(t, scratch)
	t.Setenv("TEST_EXIT_CODE", "9")

	lines := []string{
		`{"id":"a1","type":"prompt","message":"hello"}`,
		strings.Repeat("y", 1024*1024),
		`{"id":"a2","type":"prompt","message":"bye"}`,
	}

	directRes, _ := spawnAndCollect(t, exec.Command(script), nil, nil, lines)

	spec := shimSpec{
		Binary:       relaySessionsBin,
		SessionID:    "pi-realshim-1",
		BridgeSocket: bridgeSock,
		Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("2", 64)},
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, script, nil)
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	cmd.Env = append(os.Environ(), "RELAY_BRIDGE_SOCKET="+bridgeSock, "RELAY_SESSION_ID="+spec.SessionID)

	shimRes, targetPID := spawnAndCollect(t, cmd, statusR, extraFiles, lines)

	select {
	case hello := <-received:
		if hello.Type != "Hello" || hello.Kind != "project_session" || hello.Name != spec.SessionID {
			t.Fatalf("hello = %+v, unexpected shape", hello)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake bridge never received a Hello -- the shim never ran")
	}
	if targetPID == 0 {
		t.Fatal("targetPID never populated -- the shim never reported \"started\"")
	}

	if string(shimRes.output) != string(directRes.output) {
		t.Fatalf("shim-wrapped transcript differs from a direct spawn's\nshim:   %s\ndirect: %s", truncate(shimRes.output), truncate(directRes.output))
	}
	if shimRes.exitCode != 9 || directRes.exitCode != 9 {
		t.Fatalf("exit codes = shim:%d direct:%d, want both 9", shimRes.exitCode, directRes.exitCode)
	}
}

// TestClaudeProvider_RealShimSpawn_KillReapsSandboxedGrandchild spawns a
// target (through the real shim, this time via ClaudeProvider.Start/Kill
// themselves, since this test is about their behavior specifically) that
// forks a long-sleeping grandchild sharing its process group, then calls
// Kill and asserts both the target and the grandchild are gone -- proving
// Kill's -targetPID SIGKILL fallback (and the shim's own SIGINT forwarding)
// reach the whole group, not just the shim's direct child.
func TestClaudeProvider_RealShimSpawn_KillReapsSandboxedGrandchild(t *testing.T) {
	relaySessionsBin := buildRelaySessionsBinary(t)
	bridgeSock, received := startFakeBridge(t)
	scratch := shortTempDir(t)

	grandchildPIDFile := filepath.Join(scratch, "grandchild.pid")
	script := filepath.Join(scratch, "forksleep.sh")
	// trap '' INT: SIG_IGN survives exec, so both the backgrounded `sleep 300`
	// (POSIX already ignores SIGINT/SIGQUIT in an asynchronous list under a
	// non-interactive shell) and the foregrounded one (exec'd in the
	// script's own place, inheriting the ignored disposition the trap set)
	// survive the shim's first, best-effort SIGINT forward -- forcing Kill's
	// 3s-grace hard-kill fallback (SIGKILL -targetPID) to be what actually
	// reaps them, which is the exact code path this test exists to
	// exercise. Neither leg reads stdin at all, deliberately: this is a
	// signal-delivery test, not a pipe-EOF one. The pid goes to a sibling
	// temp file and is renamed, deliberately: waitForFile returns once the
	// file exists, and `>` creates it before echo writes.
	body := "#!/bin/sh\n" +
		"trap '' INT\n" +
		"sleep 300 &\n" +
		"echo $! > \"" + grandchildPIDFile + ".tmp\" && mv \"" + grandchildPIDFile + ".tmp\" \"" + grandchildPIDFile + "\"\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write forksleep script: %v", err)
	}

	session := &sessionstypes.Session{ID: "claude-killreap-1", Model: "sonnet", Directory: scratch}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		Binary:       script,
		ShimBinary:   relaySessionsBin,
		BridgeSocket: bridgeSock,
		Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("3", 64)},
	}, nil)

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("fake bridge never received a Hello")
	}
	if p.targetPID == 0 {
		t.Fatal("targetPID never populated")
	}
	targetPID := p.targetPID

	waitForFile(t, grandchildPIDFile)
	grandchildPID := readPID(t, grandchildPIDFile)

	if !pidAlive(targetPID) {
		t.Fatalf("target pid %d already gone before Kill", targetPID)
	}
	if !pidAlive(grandchildPID) {
		t.Fatalf("grandchild pid %d already gone before Kill", grandchildPID)
	}

	p.Kill()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(targetPID) && !pidAlive(grandchildPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("after Kill: target alive=%v (pid %d), grandchild alive=%v (pid %d)",
		pidAlive(targetPID), targetPID, pidAlive(grandchildPID), grandchildPID)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file %s: %v", path, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		t.Fatalf("parse pid file %s: %v", path, err)
	}
	return pid
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
