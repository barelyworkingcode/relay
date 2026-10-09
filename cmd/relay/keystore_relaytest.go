//go:build relaytest

package main

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/sealed"
)

// newKeyring keeps the sealing key in a file under configDir unless
// configDir is the default one, where the login keychain is used as in a
// release build so an in-place swap onto the real config dir behaves as today.
func newKeyring(configDir string) (sealed.Keyring, error) {
	if !seamsActive(configDir) {
		return sealed.NewKeychainKeyring(resolveRelayBin()), nil
	}
	k, err := sealed.OpenFileKeyring(configDir)
	if err != nil {
		return nil, fmt.Errorf("test keychain in %s cannot be used: %w", configDir, err)
	}
	return k, nil
}
