//go:build !darwin

package presence

import "errors"

// PeerGraphicAccess reports "not determinable" on every platform other than
// darwin: the kernel audit-session mechanism it depends on
// (LOCAL_PEERTOKEN, auditon(A_GETSINFO_ADDR)) is macOS-only, and relay's
// tray host is macOS-only regardless. Kept so the package still builds and
// tests elsewhere.
func PeerGraphicAccess(fd int) (bool, error) {
	return false, errors.New("presence: peer session graphic access is not determinable on this platform")
}
