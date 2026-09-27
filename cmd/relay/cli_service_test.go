package main

import (
	"strings"
	"testing"
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
	if !strings.Contains(msg, "relay audit") {
		t.Errorf("refusal does not point at the commands that still work stopped: %q", msg)
	}
	if strings.Contains(msg, "Read commands still work") {
		t.Errorf("refusal claims configuration reads work with relay stopped: %q", msg)
	}
}
