//go:build !darwin

package bridge

import "net"

// PeerPID is a no-op outside darwin: relay's tray host is macOS-only, and the
// audit log treats a zero pid as "unknown caller" rather than an error. Kept so
// the bridge package still builds and tests on other platforms.
func PeerPID(net.Conn) int { return 0 }
