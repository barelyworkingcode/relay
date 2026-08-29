package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"relaygo/presence"
	"relaygo/sealed"
)

// sealedResetDigest binds a sealed.reset grant to the two key ids the
// operator is looking at when they approve it (§6.4's table entry for
// `sealed.reset`): the id settings.json currently names, and whatever the
// keychain currently holds (absent when the item is already gone, which is
// the ordinary case — this is usually run BECAUSE the key vanished).
// Neither value is a secret; both are 16 hex characters naming a key, never
// the key itself.
func sealedResetDigest(settingsKeyID string, keyring sealed.Keyring) presence.Digest {
	keychainKeyID, _, _ := keyring.Load()
	return presence.NewDigestBuilder("sealed.reset").
		StringField("settings_key_id", settingsKeyID != "", settingsKeyID).
		StringField("keychain_key_id", keychainKeyID != "", keychainKeyID).
		Build()
}

// sealedResetReason is the §6.5.2 localized reason: a lowercase verb phrase
// naming the actual act, read by the operator inside the OS password prompt
// itself. This IS the panel §5.6 clause 5 asks for, on the one surface that
// works regardless of what the Settings WebView can currently render: a
// degraded store's own recovery path must not depend on machinery that
// might itself be part of what is degraded.
func sealedResetReason(s *Settings) string {
	return fmt.Sprintf(
		"reset the sealed store, permanently deleting %d project token(s), %d control-plane credential(s), "+
			"%d enrolment(s) and the certificate authority that signed them, and %d passkey(s)",
		len(s.Projects), len(s.APICredentials), len(s.Enrolments), len(s.Passkeys))
}

// resetSealedStore is the whole of ADR-017's break-glass (§5.6 clause 5):
// the one recovery from a degraded sealed store, and — by construction —
// the only door into wiping one that exists at all. It is reachable from
// nowhere but the tray's own "Reset Sealed Store…" menu item: there is no
// CLI subcommand, no --force-reset flag and no RELAY_* env var (AC-25d),
// because any of those would be a second door into the sealed store this
// whole design spends its effort closing (§5.6 clause 6).
//
// gate.Require runs BEFORE anything is touched, so a refused or cancelled
// presence check leaves every file exactly as it was. Once it succeeds,
// deletion is unconditional and unrecoverable: settings.json, ca.key.sealed,
// ca.crt and the keychain item are all gone, in that order, and store is
// then handed a fresh key via Reresolve — the one call site in the whole
// program where minting a brand new key is correct, because the operator
// standing at the keyboard just proved it with their password, which is
// exactly what §5.5.1 reserves this act for and no other.
func resetSealedStore(ctx context.Context, dir string, store *FileSettingsStore, keyring sealed.Keyring, gate *presence.Gate) error {
	s := store.Get()
	digest := sealedResetDigest(s.SealedKeyID, keyring)
	if _, err := requireGate(gate, ctx, "sealed.reset", digest, sealedResetReason(s)); err != nil {
		return err
	}

	for _, name := range []string{"settings.json", caKeySealedFile, caCertFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sealed reset: removing %s: %w", name, err)
		}
	}
	if err := keyring.Destroy(); err != nil {
		return fmt.Errorf("sealed reset: removing the keychain item: %w", err)
	}
	if err := store.Reresolve(keyring); err != nil {
		return fmt.Errorf("sealed reset: resolving a fresh key: %w", err)
	}
	return store.EnsureInitialized()
}
