//go:build darwin

package main

// The claim docs/launch-identity.md rests on is about a process's startup
// environment as any same-user process can read it. These tests read it the
// same way an attacker would — sysctl KERN_PROCARGS2 on the pid — rather than
// asking the service what it saw.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"golang.org/x/sys/unix"
)

type procArgs struct {
	raw  []byte
	exe  string
	argv []string
	env  map[string]string
}

func readProcArgs(t *testing.T, pid int) procArgs {
	t.Helper()
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		t.Fatalf("KERN_PROCARGS2 for pid %d: %v", pid, err)
	}
	if len(raw) < 4 {
		t.Fatalf("KERN_PROCARGS2 for pid %d: %d bytes", pid, len(raw))
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		t.Fatalf("KERN_PROCARGS2 for pid %d: no exec path terminator", pid)
	}
	out := procArgs{raw: raw, exe: string(rest[:end]), env: map[string]string{}}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	fields := bytes.Split(rest, []byte{0})
	if len(fields) < argc {
		t.Fatalf("KERN_PROCARGS2 for pid %d: argc %d, %d fields", pid, argc, len(fields))
	}
	for _, f := range fields[:argc] {
		out.argv = append(out.argv, string(f))
	}
	for _, f := range fields[argc:] {
		if len(f) == 0 {
			break
		}
		k, v, _ := strings.Cut(string(f), "=")
		out.env[k] = v
	}
	return out
}

var sixtyFourHex = regexp.MustCompile(`[0-9a-f]{64}`)

// assertNoRelaySecret fails if p's argv or environment holds a removed
// credential name or any 64-hex value this test process does not itself
// carry. Excluding the test's own environment keeps an unrelated hash in the
// developer's PATH from reading as a leak; the launch secret is never in it.
func assertNoRelaySecret(t *testing.T, what string, p procArgs) {
	t.Helper()
	for _, name := range bridge.RemovedCredentialEnv {
		if v, ok := p.env[name]; ok {
			t.Errorf("%s: real environment holds %s=%q", what, name, v)
		}
	}
	own := strings.Join(os.Environ(), "\n")
	for _, m := range sixtyFourHex.FindAll(p.raw, -1) {
		if !strings.Contains(own, string(m)) {
			t.Errorf("%s: real argv/environment holds a 64-hex value %q", what, m)
		}
	}
}

func writeWrapperScript(t *testing.T, target string) string {
	t.Helper()
	path := filepath.Join(mkShortTempDir(t, "wrap-"), "wrapper.sh")
	// Not exec: the wrapper stays the parent, so the process presenting the
	// secret is a grandchild of the shell relay started.
	script := fmt.Sprintf("#!/bin/bash\n%q \"$@\"\nexit $?\n", target)
	assertNoErr(t, os.WriteFile(path, []byte(script), 0o700), "write wrapper")
	return path
}

func TestServiceLaunch_RealEnvironmentHoldsNoRelayCredential(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	for _, name := range bridge.RemovedCredentialEnv {
		t.Setenv(name, "relay-inherited-this-"+name)
	}

	for _, tc := range []struct {
		name    string
		id      string
		command string
		wrapped bool
	}{
		{"direct", "svc-env-direct", binPath, false},
		{"wrapper script", "svc-env-wrapped", writeWrapperScript(t, binPath), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enhanced := NewEnhancedServiceRegistry(nil)
			router, reg := startSandboxBridge(t, enhanced)
			cfg := &config.ServiceConfig{
				ID: tc.id, DisplayName: tc.id, Command: tc.command,
				Args: []string{"--register"}, Capabilities: capsBridge,
			}
			assertNoErr(t, reg.Start(cfg), "Start")
			t.Cleanup(func() { reg.Stop(cfg.ID) })

			waitFor(t, 10*time.Second, "a manifest registered by launch identity", func() bool {
				return enhanced.Get(cfg.ID) != nil
			})
			id, ok := router.launches.Bound(cfg.ID)
			if !ok {
				t.Fatal("the manifest registered but no identity is bound")
			}

			bound := readProcArgs(t, int(id.Process.PID))
			if filepath.Base(bound.exe) != "testservice" {
				t.Fatalf("bound pid %d is %q, not the process that presented the secret", id.Process.PID, bound.exe)
			}
			launchedPID := reg.PIDsByServiceID()[cfg.ID]
			if tc.wrapped && int(id.Process.PID) == launchedPID {
				t.Fatalf("with a wrapper the identity must be the daemon, not the launched pid %d", launchedPID)
			}
			if got := bound.env[bridge.EnvLaunchFD]; got != "3" {
				t.Errorf("%s = %q, want 3", bridge.EnvLaunchFD, got)
			}
			if got := bound.env[bridge.EnvServiceID]; got != cfg.ID {
				t.Errorf("%s = %q, want %q", bridge.EnvServiceID, got, cfg.ID)
			}
			if _, ok := bound.env[bridge.EnvFrontendSocket]; ok {
				t.Errorf("a service without the frontend capability received %s", bridge.EnvFrontendSocket)
			}
			assertNoRelaySecret(t, "bound process", bound)
			if launchedPID != 0 && launchedPID != int(id.Process.PID) {
				assertNoRelaySecret(t, "launched process", readProcArgs(t, launchedPID))
			}
		})
	}
}

func TestServiceLaunch_AServiceWithoutManifestCannotRegisterOneAndItsIdentityEndsWithIt(t *testing.T) {
	binPath := buildTestServiceBinary(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)
	exited := make(chan struct{}, 1)
	reg.OnProcessExit = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}

	cfg := &config.ServiceConfig{ID: "svc-consumer-register", DisplayName: "consumer", Command: binPath, Args: []string{"--register"}, Capabilities: capsFrontend}
	assertNoErr(t, reg.Start(cfg), "Start")
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("a service refused RegisterManifest did not exit")
	}

	if enhanced.Get(cfg.ID) != nil {
		t.Fatal("a service without the manifest capability registered a manifest")
	}
	if _, ok := router.launches.Bound(cfg.ID); ok {
		t.Fatal("the identity outlived its launch")
	}
	if n := router.launches.Len(); n != 0 {
		t.Fatalf("%d launches still live after the process exited", n)
	}

	dir, err := serviceLogDir()
	assertNoErr(t, err, "serviceLogDir")
	logText, err := os.ReadFile(filepath.Join(dir, cfg.ID+".log"))
	assertNoErr(t, err, "read service log")
	if !strings.Contains(string(logText), "identity bound") {
		t.Fatalf("the consumer never completed Hello, so the refusal proves nothing:\n%s", logText)
	}
	if !strings.Contains(string(logText), "register manifest") {
		t.Fatalf("the consumer did not fail at RegisterManifest:\n%s", logText)
	}
}
