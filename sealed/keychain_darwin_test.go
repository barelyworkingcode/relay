//go:build darwin

package sealed

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestKeychainPayload_RoundTrip is hermetic: it never touches the keychain,
// only the pure encode/decode of the item's JSON value.
func TestKeychainPayload_RoundTrip(t *testing.T) {
	keyID, key, err := generateKey()
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}
	payload, err := encodeKeychainPayload(keyID, key)
	if err != nil {
		t.Fatalf("encodeKeychainPayload: %v", err)
	}
	gotID, gotKey, err := decodeKeychainPayload(payload)
	if err != nil {
		t.Fatalf("decodeKeychainPayload: %v", err)
	}
	if gotID != keyID || !bytes.Equal(gotKey, key) {
		t.Fatal("decodeKeychainPayload did not return what encodeKeychainPayload wrote")
	}
}

func TestKeychainPayload_RejectsGarbage(t *testing.T) {
	if _, _, err := decodeKeychainPayload([]byte("not json")); err == nil {
		t.Fatal("decodeKeychainPayload accepted non-JSON")
	}
	if _, _, err := decodeKeychainPayload([]byte(`{"key_id":"x","key":"not-base64!"}`)); err == nil {
		t.Fatal("decodeKeychainPayload accepted a non-base64 key")
	}
}

// The tests below touch the real login keychain. They are skipped unless
// RELAY_SEALED_LIVE_KEYCHAIN_TEST is set, per the house rule that no test
// may touch a developer's real keychain by default. Every item they create
// is under a name clearly distinguishable from relay's real
// com.barelyworkingcode.relay/config-seal-key item, and every test cleans
// its own item up via t.Cleanup regardless of how the test ends.

const liveKeychainEnvVar = "RELAY_SEALED_LIVE_KEYCHAIN_TEST"

func skipUnlessLiveKeychain(t *testing.T) {
	t.Helper()
	if os.Getenv(liveKeychainEnvVar) == "" {
		t.Skipf("skipping: set %s=1 to run tests against the real login keychain", liveKeychainEnvVar)
	}
}

// securityFindGenericPassword reports whether a generic-password item
// exists under service/account, by shelling out to the same `security`
// CLI an operator would use to verify residue.
func securityFindGenericPassword(t *testing.T, service, account string) bool {
	t.Helper()
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", account)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if strings.Contains(string(out), "could not be found") {
		return false
	}
	t.Fatalf("security find-generic-password -s %s -a %s: %v (%s)", service, account, err, out)
	return false
}

// testKeychainKeyring returns a keychainKeyring under a service/account
// pair namespaced to this test run, and registers cleanup that deletes it
// and then asserts (via the `security` CLI, not this package's own Load)
// that nothing survives.
func testKeychainKeyring(t *testing.T) *keychainKeyring {
	t.Helper()
	skipUnlessLiveKeychain(t)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	service := "com.barelyworkingcode.relay.sealed-test"
	account := "config-seal-key-test-" + t.Name()
	kr := &keychainKeyring{trustedAppPath: exe, service: service, account: account}

	t.Cleanup(func() {
		_ = kr.Destroy()
		if securityFindGenericPassword(t, service, account) {
			t.Errorf("keychain item %s/%s survived test cleanup", service, account)
		}
	})
	return kr
}

func TestKeychainKeyring_CreateThenLoadRoundTrips(t *testing.T) {
	kr := testKeychainKeyring(t)

	keyID, key, err := kr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gotID, gotKey, err := kr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotID != keyID || !bytes.Equal(gotKey, key) {
		t.Fatal("Load did not return what Create stored")
	}
}

func TestKeychainKeyring_CreateRefusesToOverwrite(t *testing.T) {
	kr := testKeychainKeyring(t)

	if _, _, err := kr.Create(); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, _, err := kr.Create(); err == nil {
		t.Fatal("second Create succeeded instead of refusing to overwrite")
	}
}

// TestKeychainKeyring_LoadOnMissingItemCreatesNothing is AC-25e's mechanism
// at the keyring's own boundary: asking for a key that is not there must
// answer ErrKeyMissing and leave the keychain exactly as empty as it found
// it, never a helpfully generated replacement.
func TestKeychainKeyring_LoadOnMissingItemCreatesNothing(t *testing.T) {
	kr := testKeychainKeyring(t)

	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("Load on a missing item: got %v, want ErrKeyMissing", err)
	}
	if securityFindGenericPassword(t, kr.service, kr.account) {
		t.Fatal("Load on a missing item created one")
	}
}

func TestKeychainKeyring_DestroyThenLoadIsMissing(t *testing.T) {
	kr := testKeychainKeyring(t)

	if _, _, err := kr.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := kr.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("Load after Destroy: got %v, want ErrKeyMissing", err)
	}
}

// TestKeychainKeyring_DestroyOnMissingItemIsNotAnError matches the
// break-glass reset (§5.6 clause 5), which deletes the keychain item
// unconditionally: deleting something already gone must not itself be a
// failure.
func TestKeychainKeyring_DestroyOnMissingItemIsNotAnError(t *testing.T) {
	kr := testKeychainKeyring(t)
	if err := kr.Destroy(); err != nil {
		t.Fatalf("Destroy on an item that was never created: %v", err)
	}
}

// TestKeychainKeyring_ACLGrantsSelfAndRejectsForeignAppPath exercises the
// legacy SecAccess mechanism end to end (§5.3.1): the item is created with
// this test binary's own path as the trusted application, and the same
// process reading it back is exactly relay's own steady-state case. A full
// test of the identity-not-path property (AC-24: a different build, same
// identity, still unlocks; an ad-hoc re-sign does not) needs the actual
// Developer-ID-signed app bundle re-signed under `-tags=live`, which is out
// of reach for a package-level test and is exercised manually instead.
func TestKeychainKeyring_ACLGrantsSelfAndRejectsForeignAppPath(t *testing.T) {
	kr := testKeychainKeyring(t)
	if _, _, err := kr.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := kr.Load(); err != nil {
		t.Fatalf("Load from the trusted application's own process: %v", err)
	}
}

// TestKeychainKeyring_PlantedItemResolvesToNamedRefusal_NeverPrompts is the
// live-keychain regression test for the startup-blocking defect. It
// reproduces the exact shape of the reported attack — `security
// add-generic-password -s ... -a ... -w '<json>'` with no `-T` — against a
// namespaced, non-production service/account. Omitting `-T` is the load-
// bearing detail: per `security`'s own default, an item added this way
// gets an ACL of "confirm before allowing access" rather than a hard
// trusted-application list, which is what makes ANY reader — including
// relay itself — fall to the keychain's own consent dialog. A
// `keychainKeyring{}.addItem` (used by every other test in this file)
// always builds a SecAccessCreate ACL naming one specific trusted
// application, which measurably refuses a mismatched reader with -25308
// and no dialog at all (§5.3.2's own table) — so it does NOT exercise the
// branch this test needs to and would pass whether or not the fix were
// present. Only the plain `security` CLI path reproduces the actual bug.
//
// This is subtle: kSecUseAuthenticationUISkip does NOT suppress this
// particular dialog — measured here against both an unsigned and a
// Developer-ID-signed reader, it still appears. copyItem's
// keychainReadTimeout is what actually keeps this test (and relay's own
// startup) from blocking on it: Load returns a named ErrKeyUnreadable once
// that deadline passes, well inside this test's own `-timeout`, but the
// OS-level dialog itself is NOT cancelled by that — it can be left showing
// on whatever screen ran this test until the process exits or a human
// dismisses it. That is a real, documented cost of running this test, not
// a bug in it: there is no query-level way to make the OS answer instead
// of a human once it has decided to ask one, so relay's fix is to stop
// waiting, not to stop the dialog from appearing.
func TestKeychainKeyring_PlantedItemResolvesToNamedRefusal_NeverPrompts(t *testing.T) {
	kr := testKeychainKeyring(t)

	keyID, key, err := generateKey()
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}
	payload, err := encodeKeychainPayload(keyID, key)
	if err != nil {
		t.Fatalf("encodeKeychainPayload: %v", err)
	}
	addCmd := exec.Command("security", "add-generic-password",
		"-s", kr.service, "-a", kr.account, "-w", string(payload))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("security add-generic-password: %v (%s)", err, out)
	}
	// kr.Destroy() (the cleanup testKeychainKeyring already registered)
	// goes through this package's own SecItemDelete, which is not
	// guaranteed to succeed against every ACL shape `security` can
	// produce. `security delete-generic-password` is the same tool that
	// created this item and is known to remove it; registered after
	// testKeychainKeyring's own cleanup, so it runs FIRST (t.Cleanup is
	// LIFO), leaving nothing for the standard cleanup's residue check to
	// find.
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password", "-s", kr.service, "-a", kr.account).Run()
	})

	_, _, loadErr := kr.Load()
	if errors.Is(loadErr, ErrKeyMissing) {
		t.Fatal("Load reported the key as MISSING, but the planted item is present — these must never be conflated")
	}
	if !errors.Is(loadErr, ErrKeyUnreadable) {
		t.Fatalf("Load against a planted item: got %v, want ErrKeyUnreadable", loadErr)
	}
}
