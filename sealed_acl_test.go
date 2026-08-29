package main

// AC-24 itself — "tampering with /Applications/Relay.app breaks the
// signature, the ACL stops matching, and the silent unlock degrades to a
// prompt" — needs a real, installed, code-signed .app bundle: the ACL binds
// to a code identity SecTrustedApplicationCreateFromPath reads off a file
// on disk (§5.3.1), and `go test` produces neither a signed bundle nor a
// stable identity to tamper with. It is explicitly a manual criterion in
// the spec ("manual, -tags=live") and stays one here; this file covers the
// two things that ARE provable from source without one:
//
//   - AC-24b: no code path weakens the ACL for an unsigned/ad-hoc build.
//   - AC-23's structural half: presence.Gate.Require cannot reach anything
//     keychain-related, because package presence never imports package
//     sealed at all — provable by the import graph, not by review.
//
// The manual procedure for AC-24 itself, against the installed bundle,
// per the measured facts already on record for this ADR:
//   1. codesign --force --sign - /Applications/Relay.app  (ad-hoc re-sign)
//   2. Restart relay; the keychain read must fail -25308 where no session
//      can prompt, or raise the keychain's OWN consent dialog (not the
//      presence gate) where one can — never a silent unlock.
//   3. security find-generic-password -s com.barelyworkingcode.relay
//      -a config-seal-key   confirms the item itself is untouched either way.
// Not run here: this repo's instructions forbid running build.sh or the
// installed app, and SIP is disabled on this VM (§ AC-24's own caveat),
// so even a manual run here would measure something weaker than production.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPresence_GateCannotReachTheKeychain is AC-23's structural half: the
// presence package — which Gate.Require lives in — has no import of
// package sealed anywhere in its non-test sources, so there is no call
// graph from a presence check into anything keychain-related for a source
// review to miss. The darwin provider (localauth_darwin.go) only ever
// calls into LocalAuthentication; sealing and the keychain are a
// completely separate package tree.
func TestPresence_GateCannotReachTheKeychain(t *testing.T) {
	root := gsModuleRoot(t)
	presenceDir := filepath.Join(root, "presence")
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", presenceDir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(presenceDir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "relaygo/sealed" {
				t.Errorf("%s imports relaygo/sealed — presence.Gate.Require would have a path to the keychain", name)
			}
		}
	}
}

// TestSeal_NoDevModeACLRelaxation is AC-24b: a grep of every non-test .go
// file and build.sh for a branch that would weaken the keychain ACL for an
// unsigned or ad-hoc build. §5.3.2 measured that the ACL binds to code
// IDENTITY (team + bundle id), not to a cdhash, so it already survives a
// rebuild by the same signing identity with no maintenance — the "a local
// rebuild re-prompts" problem ADR-017's Context worried about does not
// exist on a Developer-ID-signed install (§9.2), and there is nothing here
// for a relaxation to fix. A relaxation added anyway would be exactly the
// weakening the ADR warns survives past the test that motivated it.
//
// This cannot prove the ACL behaves correctly against a real tampered
// bundle — that needs a signed .app and is AC-24's own manual procedure,
// documented above. What this proves is narrower and just as real: nobody
// wrote an escape hatch for it.
func TestSeal_NoDevModeACLRelaxation(t *testing.T) {
	root := gsModuleRoot(t)

	scan := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lower := strings.ToLower(string(data))
		for _, phrase := range []string{
			"skip the acl", "skip acl", "relax the acl", "disable the acl",
			"acl bypass", "bypass the acl", "insecure mode", "dev mode acl",
			"relay_skip_acl", "relay_no_acl", "relay_insecure",
		} {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s contains %q — a dev-mode ACL relaxation", path, phrase)
			}
		}
	}

	scan(filepath.Join(root, "build.sh"))

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scan(filepath.Join(root, name))
	}
	for _, sub := range []string{"sealed"} {
		subEntries, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			t.Fatalf("read dir %s: %v", sub, err)
		}
		for _, e := range subEntries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			scan(filepath.Join(root, sub, name))
		}
	}
}
