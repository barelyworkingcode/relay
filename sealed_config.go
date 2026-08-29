package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tidwall/jsonc"

	"relaygo/sealed"
)

// ResolveSealedStore opens dir's settings.json under ADR-017's sealing
// scheme, resolving keyring against whatever sealed_key_id the file names
// (§5.5), and returns a store ready for full use.
//
// It is not called from runTrayApp yet — wiring it in belongs beside the
// presence gate's own construction, at the same call site, which is a
// later step. Every branch here is exercised directly, with a memory
// keyring, by sealed_config_test.go.
//
// A non-nil error is never a reason to treat the returned store as unusable:
// on every degraded branch below, the store is already fully able to serve
// its read half (§5.6 clause 1 — relay starts, it does not exit). Only a
// key-creation failure on a genuine first run returns a nil store, because
// there is nothing yet to read a clear field out of.
func ResolveSealedStore(dir string, keyring sealed.Keyring) (*FileSettingsStore, error) {
	declaredKeyID, hasSealedFields := peekSealedKeyID(dir)
	keyID, key, keyErr := keyring.Load()

	switch {
	case declaredKeyID == "" && hasSealedFields:
		// §5.5: "key present, sealed_key_id absent, sealed fields present"
		// is a corrupt combination regardless of what the keyring holds —
		// this should never arise from relay's own writes, and adopting
		// either interpretation of it silently would be a guess dressed up
		// as a recovery.
		return NewSettingsStoreDegraded(dir, errors.New(
			"settings.json holds sealed fields but names no sealed_key_id — this combination should not exist")), nil

	case declaredKeyID == "":
		// First run, or a settings.json written before sealing existed
		// with nothing sealed yet — the only two conditions under which
		// relay may create a key (§5.5).
		if keyErr != nil {
			if !errors.Is(keyErr, sealed.ErrKeyMissing) {
				return nil, fmt.Errorf("reading the sealing key: %w", keyErr)
			}
			var createErr error
			keyID, key, createErr = keyring.Create()
			if createErr != nil {
				return nil, fmt.Errorf("no sealing key exists and one could not be created: %w", createErr)
			}
		}
		sealer, err := sealed.NewAESSealer(keyID, key)
		if err != nil {
			return nil, err
		}
		return NewSettingsStoreSealed(dir, sealer), nil

	default:
		if keyErr != nil {
			return NewSettingsStoreDegraded(dir, fmt.Errorf(
				"the sealed store expects key %s, but no such key is in the login keychain; "+
					"settings.json cannot be unsealed on this machine. relay will not create a "+
					"replacement — a new key would re-seal your secrets under a key you did not choose",
				declaredKeyID)), nil
		}
		if keyID != declaredKeyID {
			return NewSettingsStoreDegraded(dir, fmt.Errorf(
				"the sealed store is bound to key %s, settings.json expects key %s", keyID, declaredKeyID)), nil
		}
		sealer, err := sealed.NewAESSealer(keyID, key)
		if err != nil {
			return nil, err
		}
		return NewSettingsStoreSealed(dir, sealer), nil
	}
}

// peekSealedKeyID reads whatever settings.json currently holds without a
// sealer — sealed_key_id is clear (§4.1) and Secret.UnmarshalJSON never
// needs a key to classify an envelope as closed, only to open one — so
// this can decide which of §5.5's rows applies before any Sealer exists to
// construct a real store with.
//
// Every failure (no file yet, unreadable, unparsable) reads as "nothing
// declared": FileSettingsStore.load's own unreadableErrLocked check is
// what actually refuses a write over a genuinely corrupt file, so
// misclassifying one here costs nothing beyond a redundant check.
func peekSealedKeyID(dir string) (keyID string, hasSealedFields bool) {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return "", false
	}
	var s Settings
	if err := json.Unmarshal(jsonc.ToJSON(data), &s); err != nil {
		return "", false
	}
	_ = forEachSecret(&s, func(_ string, sec *Secret) error {
		if sec.closed {
			hasSealedFields = true
		}
		return nil
	})
	return s.SealedKeyID, hasSealedFields
}
