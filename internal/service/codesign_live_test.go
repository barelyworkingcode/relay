//go:build live

package service

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// parseCodesignOutput reads CDHash= and TeamIdentifier= out of `codesign
// -dvvvv`'s stderr, the same two fields build.sh's own helper step embeds
// into relay via -ldflags -X (see build.sh's HELPER_CDHASH/HELPER_TEAM).
func parseCodesignOutput(out string) (cdhash, team string) {
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "CDHash="):
			cdhash = strings.TrimPrefix(line, "CDHash=")
		case strings.HasPrefix(line, "TeamIdentifier="):
			team = strings.TrimPrefix(line, "TeamIdentifier=")
			if team == "not set" {
				team = ""
			}
		}
	}
	return cdhash, team
}

// TestDarwinHelperVerifier_Live exercises the real Security.framework
// static check (SP3) against the actual, already-built and signed
// Contents/Helpers/relay-sessions -- it needs the binary build.sh produces,
// not a running tray (docs/testing-roadmap.md's live-tier convention: a
// developer without a build sees a skip, never a failure).
func TestDarwinHelperVerifier_Live(t *testing.T) {
	helperPath := "/Applications/Relay.app/Contents/Helpers/relay-sessions"
	if _, err := os.Stat(helperPath); err != nil {
		t.Skipf("no built relay-sessions helper at %s (run build.sh first): %v", helperPath, err)
	}
	out, err := exec.Command("codesign", "-dvvvv", helperPath).CombinedOutput()
	if err != nil {
		t.Skipf("codesign -dvvvv failed against %s: %v\n%s", helperPath, err, out)
	}
	cdhash, team := parseCodesignOutput(string(out))
	if cdhash == "" {
		t.Skip("built helper carries no CDHash (unsigned?) -- nothing to verify")
	}

	v, err := NewDarwinHelperVerifier(team, cdhash)
	if err != nil {
		t.Fatalf("NewDarwinHelperVerifier: %v", err)
	}
	if err := v.VerifyStatic(helperPath); err != nil {
		t.Fatalf("the real, correctly-signed helper failed its own cdhash/team check: %v", err)
	}

	// SP3's swap test: presenting a DIFFERENT binary (relay itself) under
	// the helper's own cdhash requirement must be refused -- the whole
	// point of pinning cdhash rather than team alone.
	relayPath := "/Applications/Relay.app/Contents/MacOS/relay"
	if _, err := os.Stat(relayPath); err == nil {
		if err := v.VerifyStatic(relayPath); err == nil {
			t.Fatal("a different binary (relay itself) passed the helper's own cdhash requirement")
		}
	}

	// A tampered CDHash must be refused too -- confirms this is checking
	// the value given, not merely "is this binary signed by anyone".
	wrong, err := NewDarwinHelperVerifier(team, strings.Repeat("0", len(cdhash)))
	if err != nil {
		t.Fatalf("NewDarwinHelperVerifier(wrong cdhash): %v", err)
	}
	if err := wrong.VerifyStatic(helperPath); err == nil {
		t.Fatal("a deliberately wrong cdhash requirement was satisfied by the real helper")
	}
}
