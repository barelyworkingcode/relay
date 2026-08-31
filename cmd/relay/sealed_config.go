package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tidwall/jsonc"

	"github.com/barelyworkingcode/relay/internal/sealed"
)

// ResolveSealedStore opens dir's settings.json under ADR-017's sealing
// scheme, resolving keyring against whatever sealed_key_id the file names
// (§5.5), and returns a store ready for full use.
//
// Called from runTrayApp, at the same call site the presence gate's own
// provider is constructed. Every branch here is exercised directly, with a
// memory keyring, by settings_store_sealed_test.go.
//
// A non-nil error is never a reason to treat the returned store as unusable:
// on every degraded branch below, the store is already fully able to serve
// its read half (§5.6 clause 1 — relay starts, it does not exit). Only a
// key-creation failure on a genuine first run returns a nil store, because
// there is nothing yet to read a clear field out of.
func ResolveSealedStore(dir string, keyring sealed.Keyring) (*FileSettingsStore, error) {
	sealer, degradedReason, err := resolveSealer(dir, keyring)
	if err != nil {
		return nil, err
	}
	if degradedReason != nil {
		return NewSettingsStoreDegraded(dir, degradedReason), nil
	}
	return NewSettingsStoreSealed(dir, sealer), nil
}

// resolveSealer is §5.5's table, factored out of ResolveSealedStore so the
// break-glass reset (sealed_reset.go) can re-run it against the SAME
// FileSettingsStore instance after deleting settings.json and the keychain
// item — every subsystem the running tray wired at startup holds that one
// store, and constructing a fresh *FileSettingsStore for it would leave all
// of them pointed at settings nothing refers to any more.
//
// Exactly one of (sealer, degradedReason) is non-nil on a nil err; err
// non-nil means resolution itself failed (a first run whose key could not
// even be created) and the other two returns are meaningless.
func resolveSealer(dir string, keyring sealed.Keyring) (sealer sealed.Sealer, degradedReason error, err error) {
	declaredKeyID, hasSealedFields := peekSealedKeyID(dir)
	keyID, key, keyErr := keyring.Load()

	switch {
	case declaredKeyID == "" && hasSealedFields:
		// §5.5: "key present, sealed_key_id absent, sealed fields present"
		// is a corrupt combination regardless of what the keyring holds —
		// this should never arise from relay's own writes, and adopting
		// either interpretation of it silently would be a guess dressed up
		// as a recovery.
		return nil, errors.New(
			"settings.json holds sealed fields but names no sealed_key_id — this combination should not exist"), nil

	case declaredKeyID == "":
		// First run, or a settings.json written before sealing existed
		// with nothing sealed yet — the only two conditions under which
		// relay may create a key (§5.5).
		if keyErr != nil {
			switch {
			case errors.Is(keyErr, sealed.ErrKeyMissing):
				var createErr error
				keyID, key, createErr = keyring.Create()
				if createErr != nil {
					return nil, nil, fmt.Errorf("no sealing key exists and one could not be created: %w", createErr)
				}
			case errors.Is(keyErr, sealed.ErrKeyUnreadable):
				// An item is already sitting under relay's own
				// service/account and relay cannot read it — §5.5.1's rule
				// applies even though nothing has been sealed yet: this is
				// never a reason to create a replacement over an item
				// relay does not control. Degrade rather than exit
				// (returned via err would be fatal — see the doc comment
				// on resolveSealer); Create() would refuse anyway once its
				// own Load saw the same item, but failing here, named,
				// keeps this a degraded start rather than os.Exit(1) in
				// runTrayApp.
				return nil, fmt.Errorf(
					"a login keychain item already exists under relay's own service/account, but relay "+
						"was refused permission to read it (%v); nothing has been sealed on this machine "+
						"yet, but relay will not create a replacement key over an item it does not control",
					keyErr), nil
			default:
				return nil, nil, fmt.Errorf("reading the sealing key: %w", keyErr)
			}
		}
		s, err := sealed.NewAESSealer(keyID, key)
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil

	default:
		if keyErr != nil {
			if errors.Is(keyErr, sealed.ErrKeyUnreadable) {
				// Distinct from ErrKeyMissing on purpose: the operator's
				// next move differs. An absent key means nothing is there
				// to investigate; an unreadable one means something else
				// now holds relay's keychain slot.
				return nil, fmt.Errorf(
					"the sealed store expects key %s, but the login keychain item under relay's own "+
						"service/account exists and relay was refused permission to read it (%v) — this "+
						"is not the same as the key being missing. settings.json cannot be unsealed on "+
						"this machine. relay will not create a replacement — a new key would re-seal "+
						"your secrets under a key you did not choose",
					declaredKeyID, keyErr), nil
			}
			return nil, fmt.Errorf(
				"the sealed store expects key %s, but no such key is in the login keychain; "+
					"settings.json cannot be unsealed on this machine. relay will not create a "+
					"replacement — a new key would re-seal your secrets under a key you did not choose",
				declaredKeyID), nil
		}
		if keyID != declaredKeyID {
			return nil, fmt.Errorf(
				"the sealed store is bound to key %s, settings.json expects key %s", keyID, declaredKeyID), nil
		}
		s, err := sealed.NewAESSealer(keyID, key)
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil
	}
}

// Reresolve recomputes ss's sealer against its own dir and keyring's
// CURRENT state — the only legitimate way ss's sealer ever changes after
// construction. Used solely by the break-glass reset (§5.6 clause 5), after
// settings.json and the keychain item have just been deleted: keyring.Load
// now fails with ErrKeyMissing and peekSealedKeyID sees no sealed_key_id, so
// this resolves exactly like a first run and mints a fresh key — which is
// correct here, and ONLY here, because the operator just passed a presence
// check demanding precisely that.
func (ss *FileSettingsStore) Reresolve(keyring sealed.Keyring) error {
	sealer, degradedReason, err := resolveSealer(ss.dir, keyring)
	if err != nil {
		return err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.sealer = sealer
	ss.sealUnavailable = degradedReason
	// Every cached fact about the file this store used to know is now
	// stale — the reset deleted it out from under this exact instance —
	// so the next Get()/EnsureInitialized() must re-stat and re-read
	// rather than answer from a cache describing a file that is gone.
	ss.cache = nil
	ss.fileSeen = false
	ss.readErr = nil
	ss.sealErrors = nil
	ss.lastModTime = 0
	return nil
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
