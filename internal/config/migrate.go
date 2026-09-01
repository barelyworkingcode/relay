package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/barelyworkingcode/relay/internal/sealed"
)

// needsSealedMigration reports whether s holds any Secret that has never
// been sealed: a bare-string value from a settings.json written before
// ADR-017 (§4.7), or — indistinguishably, and deliberately so — a brand
// new Settings that has nothing on disk yet. Both cases want the same
// treatment: seal whatever plaintext is currently held and stamp
// sealed_key_id, which is exactly what migrateLocked does.
func needsSealedMigration(s *Settings) bool {
	found := false
	_ = forEachSecret(s, func(_ string, sec *Secret) error {
		if !sec.closed && sec.env == nil {
			found = true
		}
		return nil
	})
	return found
}

// caKeyAwaitsMigration reports whether a plaintext ca.key sits beside a
// config dir that has no ca.key.sealed yet (§5.7 clause 2).
func caKeyAwaitsMigration(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, CAKeySealedFile))
	return err != nil
}

// migrateLocked seals every plaintext Secret in s, folds a plaintext
// ca.key into ca.key.sealed if one is waiting, and names every stale
// plaintext copy it finds beside settings.json (§4.7). Caller must hold
// ss.mu and ss.sealer must already be resolved — EnsureInitialized only
// calls this once a working sealer exists; a keyring that cannot produce
// one at all is a degraded start (§5.6), never a reason to migrate with
// nothing to seal under.
func (ss *FileSettingsStore) migrateLocked(s *Settings) error {
	// §4.2: assert the invariant BEFORE writing anything. Every project's
	// token is still legacy plaintext at this point (migration runs before
	// anything has been sealed), so a mismatch here means the file itself
	// is not what relay thinks it is, not a sealing failure — refuse the
	// whole migration and write nothing.
	for i := range s.Projects {
		p := &s.Projects[i]
		pt, ok := p.Token.Reveal()
		if !ok {
			return fmt.Errorf("migration refused: project %s (%q) has no readable token", p.ID, p.Name)
		}
		if HashToken(pt) != p.TokenHash {
			return fmt.Errorf("migration refused: project %s (%q) token does not match its stored token_hash — this file is not what relay thinks it is", p.ID, p.Name)
		}
	}

	if err := ensureAdminSecret(s); err != nil {
		return err
	}
	s.SealedKeyID = ss.sealer.KeyID()

	if err := ss.save(s); err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	ss.cache = s
	if info, err := os.Stat(ss.path()); err == nil {
		ss.lastModTime = info.ModTime().UnixNano()
	}

	if err := MigrateCAKey(ss.dir, ss.sealer); err != nil {
		return fmt.Errorf("migration: %w", err)
	}

	warnStalePlaintextCopies(ss.dir)
	slog.Info("settings.json is sealed", "dir", ss.dir, "key_id", s.SealedKeyID)
	return nil
}

// CAAADPrefix namespaces ca.key.sealed's associated data from settings
// fields (sealAADPrefix): it is a different file with its own format, not
// an entry in the sealed set forEachSecret enumerates.
// MigrateCAKey folds a plaintext ca.key into ca.key.sealed and only then
// removes the plaintext (§4.7 step 4) — a crash between the two leaves
// both, which loadCA resolves in favour of the sealed one, and the reverse
// order can lose the CA and every enrolment it signed.
func MigrateCAKey(dir string, sealer sealed.Sealer) error {
	keyPath := filepath.Join(dir, CAKeyFile)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", keyPath, err)
	}
	if err := SealCAKeyFile(dir, sealer, keyPEM); err != nil {
		return err
	}
	if err := os.Remove(keyPath); err != nil {
		return fmt.Errorf("sealed ca.key written, but removing the plaintext copy failed: %w", err)
	}
	return nil
}

// SealCAKeyFile seals keyPEM and atomically writes ca.key.sealed. Shared by
// MigrateCAKey and enrolment.GenerateCA (internal/enrolment/ca.go), which never writes a
// plaintext ca.key at all.
func SealCAKeyFile(dir string, sealer sealed.Sealer, keyPEM []byte) error {
	env, err := sealer.Seal(keyPEM, []byte(CAAADPrefix+"ca.key"))
	if err != nil {
		return fmt.Errorf("seal ca.key: %w", err)
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sealed ca.key: %w", err)
	}
	return AtomicWriteFile(filepath.Join(dir, CAKeySealedFile), data, 0600)
}

// warnStalePlaintextCopies scans dir for settings.json* files other than
// settings.json itself that still hold plaintext at a sealed field path,
// and names each one relay finds. It deletes nothing: a copy the operator
// deliberately kept aside — settings.json.bak-fsreview,
// settings.json.pre-v3-backup on the box this spec was written against —
// is not relay's to remove, ADR-017's own recovery story depends on being
// able to keep one, and this matches docs/tokens.md's existing refusal to
// sweep *.tmp leftovers on a timer.
func warnStalePlaintextCopies(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("could not scan config dir for stale plaintext settings copies", "dir", dir, "error", err)
		return
	}
	var leftoverTmp int
	for _, e := range entries {
		name := e.Name()
		if name == "settings.json" || !strings.HasPrefix(name, "settings.json") {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			// Sealed since it was staged by THIS build's atomicWriteFile,
			// which always seals before writing — harmless, just counted.
			leftoverTmp++
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		n, err := countPlaintextSecrets(data)
		if err != nil || n == 0 {
			continue
		}
		slog.Warn("stale settings copy holds plaintext secrets; migration did not touch it",
			"file", path, "plaintext_values", n)
		fmt.Printf("warning: %s holds %d secret value(s) in PLAINTEXT. This migration removed "+
			"them from settings.json; it did not touch this file. Those values are still live. "+
			"Rotate them (Settings → Projects → Rotate) and delete the file, or delete the file "+
			"and rotate anyway.\n", path, n)
	}
	if leftoverTmp > 0 {
		slog.Info("found leftover settings.json.*.tmp file(s); they postdate sealing and hold no plaintext", "count", leftoverTmp)
	}
}

// countPlaintextSecrets parses data as a Settings document and counts
// Secret fields holding a nonempty legacy-plaintext value — reusing
// Secret's own JSON classification (settings_secret.go) rather than
// re-deriving "is this field sealed" from raw JSON shape.
func countPlaintextSecrets(data []byte) (int, error) {
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, err
	}
	n := 0
	_ = forEachSecret(&s, func(_ string, sec *Secret) error {
		if !sec.closed && sec.env == nil && sec.plain != "" {
			n++
		}
		return nil
	})
	return n, nil
}
