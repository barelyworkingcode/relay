package main

import (
	"bytes"
	"github.com/barelyworkingcode/relay/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lcNewStore mirrors newCLISandboxStore but also hands back the directory,
// so a test can read settings.json off disk the way an operator's shell
// session would never do, but a test proving the plaintext never lands
// there needs to.
func lcNewStore(t *testing.T) (config.SettingsStore, string) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return store, dir
}

func lcCapture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}

// TestLoginEnrol_CLIPrintsCodeButStoresOnlyItsHash is the enrol case: the
// plaintext must appear on stdout (it is unrecoverable, so this is the only
// chance) and must never appear in settings.json, which stores nothing but
// the SHA-256 (ADR-016 decision 2). `login enrol` is brokered (ADR-017
// decision 2), so this needs a real bridge server behind a wired LoginOps
// over the same store the plaintext-absence check reads back from.
func TestLoginEnrol_CLIPrintsCodeButStoresOnlyItsHash(t *testing.T) {
	store, dir := lcNewStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	out := lcCapture(t, func() { loginEnrol() })

	const prefix = "login code: "
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("output did not contain a login code line: %q", out)
	}
	line := out[idx+len(prefix):]
	plaintext := strings.TrimSpace(strings.SplitN(line, "\n", 2)[0])
	if plaintext == "" {
		t.Fatalf("printed code was empty: %q", out)
	}
	if !strings.Contains(out, "NOT a password") {
		t.Fatalf("output did not plainly say this is not a password: %q", out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if bytes.Contains(raw, []byte(plaintext)) {
		t.Fatalf("the plaintext login code is present in settings.json")
	}
	if !bytes.Contains(raw, []byte("login_bootstrap")) {
		t.Fatalf("settings.json has no login_bootstrap record after enrol: %s", raw)
	}
}

func TestLoginRevoke_UnknownIDErrors(t *testing.T) {
	store, _ := lcNewStore(t)

	if _, err := revokePasskey(store, "no-such-passkey"); err == nil {
		t.Fatalf("revokePasskey on an unknown id returned nil error")
	} else if !strings.Contains(err.Error(), "no-such-passkey") {
		t.Fatalf("error does not name the unknown id: %v", err)
	}
}

// TestLoginList_CLINeverPrintsKeyMaterial registers a passkey directly
// through the store (the ceremony that would normally populate one lives in
// the concurrent webauthn work, not here) and checks that `login list`'s
// output carries none of its public key coordinates.
func TestLoginList_CLINeverPrintsKeyMaterial(t *testing.T) {
	store, _ := lcNewStore(t)

	const xMarker = "AAAASECRETXCOORDAAAA"
	const yMarker = "BBBBSECRETYCOORDBBBB"
	err := store.With(func(s *config.Settings) {
		s.Passkeys = append(s.Passkeys, config.Passkey{
			ID:      "cred-id-0123456789abcdef",
			Name:    "test-passkey",
			X:       []byte(xMarker),
			Y:       []byte(yMarker),
			Created: "2026-08-28T00:00:00Z",
		})
	})
	if err != nil {
		t.Fatalf("store.With: %v", err)
	}

	serveBroker(t, newBrokerRouter(t, store, nil))

	out := lcCapture(t, func() { loginList() })

	if !strings.Contains(out, "test-passkey") {
		t.Fatalf("listing did not show the passkey's name: %q", out)
	}
	if strings.Contains(out, xMarker) || strings.Contains(out, yMarker) {
		t.Fatalf("listing printed public key material: %q", out)
	}
	// A base64 or hex encoding of the raw marker bytes is a second way the
	// same leak could sneak back in without the literal ASCII marker
	// appearing.
	if strings.Contains(out, "QUFBQVNFQ1JFVFhDT09SREFBQUE") {
		t.Fatalf("listing printed a base64-encoded X coordinate: %q", out)
	}
	if strings.Contains(out, "4141414153454352455458434f4f524441414141") {
		t.Fatalf("listing printed a hex-encoded X coordinate: %q", out)
	}
}
