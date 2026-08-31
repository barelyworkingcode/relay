//go:build !darwin

package sealed

import "fmt"

var errKeychainUnavailable = fmt.Errorf("%w: sealed config requires macOS", ErrKeyMissing)

// keychainKeyring is a no-op outside darwin: relay's tray host is
// macOS-only, and this stub exists so the module still builds and tests on
// other platforms rather than gating the whole build behind darwin.
type keychainKeyring struct{}

// NewKeychainKeyring returns a Keyring that refuses every call outside
// darwin. See keychain_darwin.go for the real implementation.
func NewKeychainKeyring(trustedAppPath string) Keyring { return keychainKeyring{} }

func (keychainKeyring) Load() (string, []byte, error)   { return "", nil, errKeychainUnavailable }
func (keychainKeyring) Create() (string, []byte, error) { return "", nil, errKeychainUnavailable }
func (keychainKeyring) Destroy() error                  { return errKeychainUnavailable }
