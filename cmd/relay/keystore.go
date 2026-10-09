//go:build !relaytest

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// newKeyring is the one place the login-keychain Keyring is constructed
// (§5.3.3): a second call site would let a CLI process satisfy the ACL that
// design depends on the CLI never asking.
func newKeyring(configDir string) (sealed.Keyring, error) {
	account, err := keychainAccount(configDir)
	if err != nil {
		return nil, err
	}
	return sealed.NewKeychainKeyring(resolveRelayBin(), account), nil
}

// keychainAccount names the login-keychain item for configDir. The default
// dir keeps the tray's item; any other dir gets its own, so a reset on one
// instance never destroys another's key. A dir that cannot be resolved
// refuses rather than falling back to the default account, which would
// share the tray's key.
func keychainAccount(configDir string) (string, error) {
	dir, err := resolveDirForKeychain(configDir)
	if err != nil {
		return "", err
	}
	def, err := resolveDirForKeychain(bridge.DefaultConfigDir())
	if err != nil {
		return "", err
	}
	if dir == def {
		return sealed.DefaultKeychainAccount, nil
	}
	sum := sha256.Sum256([]byte(dir))
	return sealed.DefaultKeychainAccount + "." + hex.EncodeToString(sum[:])[:16], nil
}

// resolveDirForKeychain symlink-resolves dir. A directory that does not exist
// yet resolves through its nearest existing parent, so the name is the same
// before and after the directory is created.
func resolveDirForKeychain(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("config dir %q cannot be resolved for the sealing key: %w", dir, err)
	}
	rest := ""
	cur := abs
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("config dir %q cannot be resolved for the sealing key: %w", dir, err)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
