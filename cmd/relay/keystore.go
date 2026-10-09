//go:build !relaytest

package main

import "github.com/barelyworkingcode/relay/internal/sealed"

// newKeyring is the one place the login-keychain Keyring is constructed
// (§5.3.3): a second call site would let a CLI process satisfy the ACL that
// design depends on the CLI never asking. configDir is unused in a release
// build.
func newKeyring(configDir string) (sealed.Keyring, error) {
	return sealed.NewKeychainKeyring(resolveRelayBin()), nil
}
