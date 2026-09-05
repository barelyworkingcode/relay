//go:build live

package sshhost

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// canSSHLocalhost is the acceptance test's own precondition check: loopback
// ssh must already work passwordless, or every assertion below is really a
// test of the box, not of this package. t.Skip, never t.Fatal, so a
// developer without a local sshd configured for key auth sees a skip.
func canSSHLocalhost(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "localhost", "true")
	return cmd.Run() == nil
}

// TestLive_ProbeAndCheckAndDisconnect_Localhost is the real-ssh half of
// docs/ssh-hosts.md's end-to-end acceptance criterion: a host registered as
// the devbox itself. It exercises the actual /usr/bin/ssh path this
// package's hermetic tests stub out.
func TestLive_ProbeAndCheckAndDisconnect_Localhost(t *testing.T) {
	if !canSSHLocalhost(t) {
		t.Skip("ssh -o BatchMode=yes localhost true failed; passwordless loopback ssh is not set up on this box")
	}

	h := config.Host{Name: "localhost-live-test", Target: "localhost"}

	probe, err := Probe(context.Background(), h)
	if err != nil {
		t.Fatalf("Probe returned a Go error (should always return a HostProbe): %v", err)
	}
	if !probe.OK {
		t.Fatalf("expected a successful probe against localhost, got: %+v", probe)
	}
	if probe.OS == "" {
		t.Fatal("expected an OS to be discovered")
	}
	if probe.NodePath == "" {
		t.Log("node not found on PATH for localhost's login shell -- probe still succeeded, which is the point being tested")
	}

	if !Check(h) {
		t.Fatal("expected Check to report a live ControlMaster right after a successful Probe")
	}

	if err := Disconnect(h); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if Check(h) {
		t.Fatal("expected Check to report false after Disconnect")
	}
}
