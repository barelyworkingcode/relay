//go:build !windows

package main

// fsMCP v3 integration, R5: spawning a --root'd stdio MCP under seatbelt.
// The fail-closed tests are the point — a caller of prepareStdioLaunch that
// gets a non-nil error must never fall back to an unsandboxed spawn.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStdioRootFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		ok   bool
	}{
		{"space form", []string{"--root", "/a/b"}, "/a/b", true},
		{"equals form", []string{"--root=/a/b"}, "/a/b", true},
		{"absent", []string{"--read-only"}, "", false},
		{"trailing with no value", []string{"--root"}, "", false},
		{"among other flags", []string{"--read-only", "--root", "/a/b", "--max-response-bytes=9"}, "/a/b", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stdioRootFlag(c.args)
			if got != c.want || ok != c.ok {
				t.Errorf("stdioRootFlag(%v) = %q, %v; want %q, %v", c.args, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestStdioReadOnlyFlag(t *testing.T) {
	if stdioReadOnlyFlag([]string{"--root", "/x"}) {
		t.Error("no --read-only present but reported true")
	}
	if !stdioReadOnlyFlag([]string{"--root", "/x", "--read-only"}) {
		t.Error("--read-only present but reported false")
	}
	if !stdioReadOnlyFlag([]string{"--read-only=true"}) {
		t.Error("--read-only=true not recognized")
	}
}

func TestEnsureSandboxProfile_ReadWriteVsReadOnly(t *testing.T) {
	mkSandboxRelayHome(t)

	rwPath, err := ensureSandboxProfile(false)
	if err != nil {
		t.Fatalf("ensureSandboxProfile(false): %v", err)
	}
	roPath, err := ensureSandboxProfile(true)
	if err != nil {
		t.Fatalf("ensureSandboxProfile(true): %v", err)
	}
	if rwPath == roPath {
		t.Fatal("read-write and read-only profiles must not share a path")
	}

	rw, err := os.ReadFile(rwPath)
	if err != nil {
		t.Fatalf("read rw profile: %v", err)
	}
	ro, err := os.ReadFile(roPath)
	if err != nil {
		t.Fatalf("read ro profile: %v", err)
	}
	if !strings.Contains(string(rw), sandboxGrantReadWrite) {
		t.Errorf("read-write profile missing the write grant clause:\n%s", rw)
	}
	if strings.Contains(string(ro), sandboxGrantReadWrite) {
		t.Errorf("read-only profile must drop the write grant clause entirely:\n%s", ro)
	}
	if !strings.Contains(string(ro), sandboxGrantReadOnly) {
		t.Errorf("read-only profile missing the read grant clause:\n%s", ro)
	}
	// Neither profile ever names an actual directory — the grant travels as
	// -D GRANT=, never interpolated into the file (R5).
	for _, body := range [][]byte{rw, ro} {
		if strings.Contains(string(body), "/Users") {
			t.Errorf("profile interpolated a concrete path:\n%s", body)
		}
	}

	// Idempotent: writing again must not error or change the content.
	again, err := ensureSandboxProfile(false)
	if err != nil || again != rwPath {
		t.Fatalf("second ensureSandboxProfile(false) = %q, %v", again, err)
	}
}

func TestPrepareStdioLaunch_NoRootPassesThrough(t *testing.T) {
	cfg := &ExternalMcp{ID: "plain", Command: "/bin/echo", Args: []string{"hi"}}
	command, args, err := prepareStdioLaunch(cfg)
	if err != nil {
		t.Fatalf("prepareStdioLaunch: %v", err)
	}
	if command != cfg.Command || len(args) != 1 || args[0] != "hi" {
		t.Errorf("an MCP with no --root was rewritten: command=%q args=%v", command, args)
	}
	if cfg.ResolvedRoot != "" {
		t.Errorf("ResolvedRoot set with no --root argument: %q", cfg.ResolvedRoot)
	}
}

func TestPrepareStdioLaunch_WrapsWithSeatbelt(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()

	cfg := &ExternalMcp{ID: "fsmcp3", Command: "/usr/local/bin/fsmcp3",
		Args: []string{"--root", root}}
	command, args, err := prepareStdioLaunch(cfg)
	if err != nil {
		t.Fatalf("prepareStdioLaunch: %v", err)
	}
	if command != sandboxExecPath {
		t.Errorf("command = %q, want %q", command, sandboxExecPath)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	if cfg.ResolvedRoot != resolvedRoot {
		t.Errorf("cfg.ResolvedRoot = %q, want %q", cfg.ResolvedRoot, resolvedRoot)
	}
	want := []string{"-f", args[1] /* profile path, asserted below */, "-D", "GRANT=" + resolvedRoot,
		"/usr/local/bin/fsmcp3", "--root", root}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want shape %v", args, want)
	}
	if args[0] != "-f" || args[2] != "-D" || args[3] != "GRANT="+resolvedRoot ||
		args[4] != "/usr/local/bin/fsmcp3" || args[5] != "--root" || args[6] != root {
		t.Errorf("args = %v", args)
	}
	if !strings.HasSuffix(args[1], "sandbox-rw.sb") {
		t.Errorf("read-write MCP did not get the rw profile: %q", args[1])
	}
	if _, err := os.Stat(args[1]); err != nil {
		t.Errorf("profile file was not actually written: %v", err)
	}
}

func TestPrepareStdioLaunch_ReadOnlyPicksTheReadOnlyProfile(t *testing.T) {
	mkSandboxRelayHome(t)
	root := t.TempDir()
	cfg := &ExternalMcp{ID: "fsmcp3ro", Command: "/usr/local/bin/fsmcp3",
		Args: []string{"--root", root, "--read-only"}}
	_, args, err := prepareStdioLaunch(cfg)
	if err != nil {
		t.Fatalf("prepareStdioLaunch: %v", err)
	}
	if !strings.HasSuffix(args[1], "sandbox-ro.sb") {
		t.Errorf("read-only MCP did not get the ro profile: %q", args[1])
	}
}

// TestPrepareStdioLaunch_FailClosed_SandboxExecMissing is the fail-closed
// requirement's core claim: no sandbox-exec, no spawn at all — never a
// silent fallback to running the MCP unsandboxed.
func TestPrepareStdioLaunch_FailClosed_SandboxExecMissing(t *testing.T) {
	mkSandboxRelayHome(t)
	old := sandboxExecPath
	sandboxExecPath = filepath.Join(t.TempDir(), "no-such-sandbox-exec")
	t.Cleanup(func() { sandboxExecPath = old })

	cfg := &ExternalMcp{ID: "fsmcp3", Command: "/usr/local/bin/fsmcp3",
		Args: []string{"--root", t.TempDir()}}
	command, args, err := prepareStdioLaunch(cfg)
	if err == nil {
		t.Fatalf("expected an error with sandbox-exec missing, got command=%q args=%v", command, args)
	}
	if cfg.ResolvedRoot != "" {
		t.Errorf("ResolvedRoot must not be set on a failed, unsandboxed launch: %q", cfg.ResolvedRoot)
	}
}

// TestPrepareStdioLaunch_FailClosed_RootDoesNotResolve covers a --root that
// cannot be resolved through symlinks (does not exist): seatbelt matches real
// paths, so relay must refuse rather than sandbox against an unresolved one.
func TestPrepareStdioLaunch_FailClosed_RootDoesNotResolve(t *testing.T) {
	mkSandboxRelayHome(t)
	cfg := &ExternalMcp{ID: "fsmcp3", Command: "/usr/local/bin/fsmcp3",
		Args: []string{"--root", filepath.Join(t.TempDir(), "does-not-exist")}}
	if _, _, err := prepareStdioLaunch(cfg); err == nil {
		t.Fatal("expected an error for a --root that does not resolve")
	}
	if cfg.ResolvedRoot != "" {
		t.Errorf("ResolvedRoot must not be set when resolution failed: %q", cfg.ResolvedRoot)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: a real stdio child spawned through connectStdio, under a real
// seatbelt, on this machine.
// ---------------------------------------------------------------------------

// TestExternalMcpManager_StdioRoot_SpawnsUnderSeatbeltAndRecordsRoot proves
// the whole path relay actually runs: register an MCP with --root, start it
// through the manager exactly as StartAll/Reconcile do, and check both that
// the handshake still succeeds under seatbelt and that McpSurfaceFor reports
// the resolved root (what router.go stamps onto the audit log, R2).
func TestExternalMcpManager_StdioRoot_SpawnsUnderSeatbeltAndRecordsRoot(t *testing.T) {
	if _, err := os.Stat(sandboxExecPath); err != nil {
		t.Skipf("sandbox-exec not available on this machine: %v", err)
	}
	mkSandboxRelayHome(t)
	bin := buildTestMcpBinary(t)
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}

	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	cfg := ExternalMcp{ID: "fsmcp3-sandboxed", DisplayName: "fsmcp3-sandboxed",
		Transport: "stdio", Command: bin, Args: []string{"--root", root}}
	if err := m.startOne(context.Background(), &cfg); err != nil {
		t.Fatalf("startOne under seatbelt: %v", err)
	}
	if !m.IsConnected("fsmcp3-sandboxed") {
		t.Fatal("MCP did not connect when spawned under seatbelt")
	}
	surface := m.McpSurfaceFor("fsmcp3-sandboxed")
	if surface.Root != resolvedRoot {
		t.Errorf("surface.Root = %q, want %q", surface.Root, resolvedRoot)
	}
}

// TestExternalMcpManager_StdioRoot_FailsClosedWhenSandboxExecMissing proves
// the manager-level path never spawns unsandboxed either: with sandbox-exec
// unavailable, startOne must return an error and the MCP must never connect.
func TestExternalMcpManager_StdioRoot_FailsClosedWhenSandboxExecMissing(t *testing.T) {
	mkSandboxRelayHome(t)
	old := sandboxExecPath
	sandboxExecPath = filepath.Join(t.TempDir(), "no-such-sandbox-exec")
	t.Cleanup(func() { sandboxExecPath = old })

	bin := buildTestMcpBinary(t)
	m := NewExternalMcpManager(nil)
	t.Cleanup(m.StopAll)
	cfg := ExternalMcp{ID: "fsmcp3-unsandboxable", DisplayName: "fsmcp3-unsandboxable",
		Transport: "stdio", Command: bin, Args: []string{"--root", t.TempDir()}}
	if err := m.startOne(context.Background(), &cfg); err == nil {
		t.Fatal("startOne succeeded with sandbox-exec unavailable — must fail closed")
	}
	if m.IsConnected("fsmcp3-unsandboxable") {
		t.Fatal("MCP connected despite sandbox-exec being unavailable")
	}
}
