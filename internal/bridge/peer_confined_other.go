//go:build !darwin

package bridge

import "github.com/barelyworkingcode/relay/internal/peertoken"

// PeerConfined has no implementation outside darwin: relay's tray host is
// macOS-only. ok is always false so the bridge package still builds and
// tests on other platforms without ever answering "not confined".
func PeerConfined(peertoken.Token) (confined bool, ok bool) { return false, false }
