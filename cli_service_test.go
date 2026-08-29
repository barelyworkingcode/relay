package main

import (
	"net"
	"strings"
	"testing"

	"relaygo/bridge"
)

// TestServiceRequiredMessage_NamesTheCommand is AC-11's textual requirement
// on the exact function requireService calls to build its refusal: it must
// contain the command's own name and the words "requires the service", not
// a generic bridge or settings error.
func TestServiceRequiredMessage_NamesTheCommand(t *testing.T) {
	msg := serviceRequiredMessage("relay credential mint")

	if !strings.Contains(msg, "relay credential mint") {
		t.Errorf("refusal does not name the command: %q", msg)
	}
	if !strings.Contains(msg, "requires the service") {
		t.Errorf("refusal does not say it requires the service: %q", msg)
	}
	if !strings.Contains(msg, "relay credential list") || !strings.Contains(msg, "relay audit") {
		t.Errorf("refusal does not point at the read commands that still work: %q", msg)
	}
}

// TestServiceReachable_NoListener asserts the probe reports false against a
// socket path nothing is listening on.
func TestServiceReachable_NoListener(t *testing.T) {
	dir := mkShortTempDir(t, "relay-nosvc-")
	applyOverride(t, dir)

	if serviceReachable() {
		t.Fatal("serviceReachable reported true with no listener on the socket")
	}
}

// TestServiceReachable_WithListener is the other half: a real listener on
// relay's own socket path must be detected as reachable, so requireService's
// refusal fires only when relay genuinely is not there to answer.
func TestServiceReachable_WithListener(t *testing.T) {
	dir := mkShortTempDir(t, "relay-withsvc-")
	applyOverride(t, dir)

	ln, err := net.Listen("unix", bridge.SocketPath())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	if !serviceReachable() {
		t.Fatal("serviceReachable reported false with a real listener on the socket")
	}
}
